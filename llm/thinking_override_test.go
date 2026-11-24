package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureReq runs one arbitrary request and returns the raw + parsed request body.
func captureReq(t *testing.T, cfg Config, req CompletionRequest, stub string) (string, map[string]any) {
	t.Helper()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.Write([]byte(stub))
	}))
	defer srv.Close()
	cfg.BaseURL, cfg.APIKey, cfg.Model = srv.URL, "k", "m"
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	collect(t, p, req)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	return string(raw), m
}

func thinkingType(m map[string]any) string {
	if th, ok := m["thinking"].(map[string]any); ok {
		s, _ := th["type"].(string)
		return s
	}
	return ""
}

// A per-request Thinking="disabled" override must win over an enabled config AND
// strip replayed thinking blocks (Anthropic rejects them when thinking is off).
func TestAnthropicPerRequestThinkingDisabledStrips(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{
		Thinking: "disabled",
		Messages: []Message{
			UserText("hi"),
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockThinking, Thinking: "SECRET_THOUGHT"},
				TextBlock("answer"),
			}},
		},
	}
	raw, m := captureReq(t, Config{Format: FormatAnthropic, ThinkingType: "enabled"}, req, stub)
	if thinkingType(m) != "disabled" {
		t.Fatalf("override should force disabled, got %q", thinkingType(m))
	}
	if strings.Contains(raw, "SECRET_THOUGHT") {
		t.Fatal("thinking block must be stripped when thinking is disabled")
	}
	if !strings.Contains(raw, "answer") {
		t.Fatal("sibling text block should survive")
	}
}

func TestAnthropicPerRequestThinkingEnabledKeeps(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{
		Thinking: "enabled",
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "KEPT_THOUGHT", Signature: "s"}, TextBlock("a")}},
		},
	}
	raw, m := captureReq(t, Config{Format: FormatAnthropic}, req, stub) // cfg thinking unset
	if thinkingType(m) != "enabled" {
		t.Fatalf("override should force enabled, got %q", thinkingType(m))
	}
	if !strings.Contains(raw, "KEPT_THOUGHT") {
		t.Fatal("thinking block should be replayed when enabled")
	}
}

func TestAnthropicThinkingInheritsConfigWhenUnset(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{Messages: []Message{UserText("hi")}} // Thinking == ""
	_, m := captureReq(t, Config{Format: FormatAnthropic, ThinkingType: "enabled"}, req, stub)
	if thinkingType(m) != "enabled" {
		t.Fatalf("empty override should inherit config, got %q", thinkingType(m))
	}
	// And with config also unset, thinking is omitted entirely.
	_, m2 := captureReq(t, Config{Format: FormatAnthropic}, req, stub)
	if _, ok := m2["thinking"]; ok {
		t.Fatalf("thinking should be omitted, got %v", m2["thinking"])
	}
}

func TestOpenAIPerRequestThinkingOverride(t *testing.T) {
	// A non-empty stub: an empty completion would trip the empty-response retry
	// (this test only inspects the request body, so give it minimal content).
	stub := sse(`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`, ``, `data: [DONE]`, ``)
	req := CompletionRequest{
		Thinking: "disabled",
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockToolUse, ID: "t1", Name: "Bash", Input: []byte(`{}`)}}},
			{Role: RoleUser, Content: []ContentBlock{ToolResultText("t1", "ok", false)}},
		},
	}
	raw, m := captureReq(t, Config{Format: FormatOpenAI, ThinkingType: "enabled", ReasoningEffort: "high"}, req, stub)
	if thinkingType(m) != "disabled" {
		t.Fatalf("override should force disabled, got %q", thinkingType(m))
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("override disabled must suppress reasoning_effort, got %v", m["reasoning_effort"])
	}
	if strings.Contains(raw, reasoningElidedPlaceholder) {
		t.Fatal("disabled thinking must not backfill reasoning_content placeholder")
	}
	// When profile ThinkingType is "" ("do not send thinking field"), per-request
	// Thinking="disabled" must NOT force-inject "thinking":{"type":"disabled"} while
	// still suppressing reasoning_effort and placeholder injection.
	rawNoInject, mNoInject := captureReq(t, Config{Format: FormatOpenAI, ReasoningEffort: "high"}, req, stub)
	if _, ok := mNoInject["thinking"]; ok {
		t.Fatalf("unset profile thinking_type must not inject thinking field: %v", mNoInject["thinking"])
	}
	if _, ok := mNoInject["reasoning_effort"]; ok {
		t.Fatalf("override disabled must suppress reasoning_effort: %v", mNoInject["reasoning_effort"])
	}
	if strings.Contains(rawNoInject, reasoningElidedPlaceholder) {
		t.Fatal("disabled request must not backfill reasoning_content placeholder")
	}
	// Empty override inherits config.
	req2 := CompletionRequest{Messages: []Message{UserText("hi")}}
	_, m2 := captureReq(t, Config{Format: FormatOpenAI, ThinkingType: "enabled"}, req2, stub)
	if thinkingType(m2) != "enabled" {
		t.Fatalf("empty override should inherit config, got %q", thinkingType(m2))
	}
}

// When Anthropic profile has ThinkingType="" ("do not send"), per-request
// Thinking="disabled" must omit "thinking", strip replayed thinking blocks, and
// suppress output_config.effort. Also check BudgetTokens auto-ceiling when MaxTokens==0.
func TestAnthropicPerRequestDisabledNoForceInjectAndBudget(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{
		Thinking: "disabled",
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "HIDDEN"}, TextBlock("ok")}},
		},
	}
	raw, m := captureReq(t, Config{Format: FormatAnthropic, ReasoningEffort: "max", PreserveThinkingBlocks: true}, req, stub)
	if _, ok := m["thinking"]; ok {
		t.Fatalf("unset profile thinking_type must not inject thinking field: %v", m["thinking"])
	}
	if _, ok := m["output_config"]; ok {
		t.Fatalf("disabled request must suppress output_config: %v", m["output_config"])
	}
	if strings.Contains(raw, "HIDDEN") {
		t.Fatal("replayed thinking block must still be stripped when req.Thinking=disabled")
	}
	// BudgetTokens >= 8192 with unset MaxTokens (0) auto-raises max_tokens above budget.
	_, mBud := captureReq(t, Config{Format: FormatAnthropic, ThinkingType: "enabled", ThinkingBudgetTokens: 16384}, CompletionRequest{Messages: []Message{UserText("hi")}}, stub)
	th, _ := mBud["thinking"].(map[string]any)
	maxTok, _ := mBud["max_tokens"].(float64)
	if th["budget_tokens"] != float64(16384) || maxTok <= 16384 {
		t.Fatalf("expected budget=16384 and max_tokens>16384, got thinking=%v max_tokens=%v", th, maxTok)
	}
}

func TestOpenAIResponsesThinkingTypeAndEffort(t *testing.T) {
	stub := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	temp := 0.0
	// profile ThinkingType="disabled" suppresses ReasoningEffort
	_, m := captureReq(t, Config{Format: FormatOpenAIResponses, ThinkingType: "disabled", ReasoningEffort: "high"}, CompletionRequest{Messages: []Message{UserText("hi")}, Temperature: &temp}, stub)
	if _, ok := m["reasoning"]; ok {
		t.Fatalf("disabled ThinkingType must suppress reasoning: %v", m["reasoning"])
	}
	if _, ok := m["temperature"]; ok {
		t.Fatalf("temperature must be omitted for responses config with reasoning_effort: %v", m["temperature"])
	}
	// enabled/unset + effort sends reasoning.effort; req.Thinking="disabled" suppresses it
	_, m2 := captureReq(t, Config{Format: FormatOpenAIResponses, ThinkingType: "enabled", ReasoningEffort: "low"}, CompletionRequest{Messages: []Message{UserText("hi")}}, stub)
	if r, _ := m2["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Fatalf("expected reasoning.effort=low, got %v", m2["reasoning"])
	}
	_, m3 := captureReq(t, Config{Format: FormatOpenAIResponses, ReasoningEffort: "low"}, CompletionRequest{Thinking: "disabled", Messages: []Message{UserText("hi")}}, stub)
	if _, ok := m3["reasoning"]; ok {
		t.Fatalf("req.Thinking=disabled must suppress reasoning: %v", m3["reasoning"])
	}
}

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIPromptCacheKeyAndDisable(t *testing.T) {
	chatStub := sse(`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`, ``, `data: [DONE]`, ``)
	for _, tc := range []struct {
		name    string
		format  Format
		stub    string
		disable bool
		wantKey string
	}{
		{name: "chat_enabled", format: FormatOpenAI, stub: chatStub, wantKey: "sess-1"},
		{name: "chat_disabled", format: FormatOpenAI, stub: chatStub, disable: true, wantKey: ""},
		{
			name:   "responses_enabled",
			format: FormatOpenAIResponses,
			stub: sse(
				`event: response.output_text.delta`, `data: {"delta":"ok"}`, ``,
				`event: response.completed`, `data: {"response":{"status":"completed"}}`, ``,
			),
			wantKey: "sess-1",
		},
		{
			name:   "responses_disabled",
			format: FormatOpenAIResponses,
			stub: sse(
				`event: response.output_text.delta`, `data: {"delta":"ok"}`, ``,
				`event: response.completed`, `data: {"response":{"status":"completed"}}`, ``,
			),
			disable: true,
			wantKey: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw = make([]byte, r.ContentLength)
				_, _ = r.Body.Read(raw)
				w.Write([]byte(tc.stub))
			}))
			defer srv.Close()
			p, err := NewProvider(Config{
				Format:                tc.format,
				BaseURL:               srv.URL,
				APIKey:                "k",
				Model:                 "m",
				DisablePromptCacheKey: tc.disable,
			})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			collect(t, p, CompletionRequest{
				Messages:       []Message{UserText("hi")},
				PromptCacheKey: "sess-1",
			})
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, raw)
			}
			got, _ := m["prompt_cache_key"].(string)
			if got != tc.wantKey {
				t.Fatalf("prompt_cache_key=%q, want %q", got, tc.wantKey)
			}
		})
	}
}

func TestAnthropicInputTokensIncludesCache(t *testing.T) {
	streamBody := sse(
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_read_input_tokens":900,"output_tokens":1}}}`, ``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, ``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1000,"cache_read_input_tokens":900,"output_tokens":12}}`, ``,
		`data: {"type":"message_stop"}`, ``,
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		dec := json.NewDecoder(r.Body)
		_ = dec.Decode(&body)
		if body.Stream {
			w.Write([]byte(streamBody))
			return
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":800,"cache_read_input_tokens":900,"output_tokens":12}}`))
	}))
	defer srv.Close()

	// Default (InputTokensIncludesCache=false): adds input_tokens + cache_read.
	pDefault, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "m"})
	acc := NewAccumulator()
	for ev, err := range pDefault.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
	}
	if acc.Usage.InputTokens != 1900 || acc.Usage.CacheReadTokens != 900 || acc.Usage.OutputTokens != 12 {
		t.Fatalf("default stream usage=%+v, want in=1900 read=900 out=12", acc.Usage)
	}

	// Relay mode (InputTokensIncludesCache=true): does not double count cache_read.
	pRelay, _ := NewProvider(Config{
		Format:                   FormatAnthropic,
		BaseURL:                  srv.URL,
		APIKey:                   "k",
		Model:                    "m",
		InputTokensIncludesCache: true,
	})
	acc2 := NewAccumulator()
	for ev, err := range pRelay.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
		acc2.Add(ev)
	}
	if acc2.Usage.InputTokens != 1000 || acc2.Usage.CacheReadTokens != 900 || acc2.Usage.OutputTokens != 12 {
		t.Fatalf("relay stream usage=%+v, want in=1000 read=900 out=12", acc2.Usage)
	}

	// Complete with gateway reporting cache_read (900) > input_tokens (800): clamps to 900.
	_, _, gotComplete, err := pRelay.Complete(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if gotComplete.InputTokens != 900 || gotComplete.CacheReadTokens != 900 || gotComplete.OutputTokens != 12 {
		t.Fatalf("relay complete usage=%+v, want in=900 read=900 out=12", gotComplete)
	}
}

func TestAnthropicThinkingReplayGuard(t *testing.T) {
	msgs := []Message{
		UserText("hi"),
		{Role: RoleAssistant, Content: []ContentBlock{
			{Type: BlockThinking, Thinking: "plan", Signature: "sig"},
			TextBlock("ok"),
		}},
		UserText("next"),
	}
	checkHasThinking := func(cfg Config, reqThinking string, wantThinking bool) {
		t.Helper()
		p := &anthropicProvider{cfg: cfg}
		raw, err := p.buildBody(CompletionRequest{Messages: msgs, Thinking: reqThinking}, false)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		got := false
		for _, c := range body.Messages[1].Content {
			if c.Type == "thinking" {
				got = true
			}
		}
		if got != wantThinking {
			t.Fatalf("cfg=%+v reqThinking=%q: hasThinking=%v, want %v", cfg, reqThinking, got, wantThinking)
		}
	}

	checkHasThinking(Config{ThinkingType: "disabled"}, "", false)
	checkHasThinking(Config{ThinkingType: "enabled"}, "", true)
	checkHasThinking(Config{ThinkingType: "enabled"}, "disabled", false)
	checkHasThinking(Config{PreserveThinkingBlocks: true}, "", true)
	checkHasThinking(Config{PreserveThinkingBlocks: true}, "disabled", false)
}

func TestAnthropicRollingCacheBreakpoints(t *testing.T) {
	msgs := []Message{
		UserText("u0"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "t0", Signature: "s0"}, TextBlock("a0")}},
		UserText("u1"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockToolUse, ID: "id1", Name: "Bash", Input: []byte(`{"command":"ls"}`)}}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("id1", "out", false)}},
	}
	req := CompletionRequest{
		System:          []string{"static", "dynamic"},
		DynamicBoundary: 1,
		Tools: []ToolSchema{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
			{Name: "Bash", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: msgs,
	}

	// Default preserves single system breakpoint only.
	pLegacy := &anthropicProvider{cfg: Config{Model: "m"}}
	rawLegacy, err := pLegacy.buildBody(req, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := countCacheControl(t, rawLegacy); n != 1 {
		t.Fatalf("legacy cache_control count=%d, want 1", n)
	}

	// Rolling adds tools[-1], system[0], messages[len-3], messages[len-1] == 4 total.
	pRolling := &anthropicProvider{cfg: Config{Model: "m", ThinkingType: "enabled", CacheBreakpoints: "rolling"}}
	rawRolling, err := pRolling.buildBody(req, true)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		System   []map[string]any `json:"system"`
		Tools    []map[string]any `json:"tools"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rawRolling, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.System[0]["cache_control"] == nil || parsed.System[1]["cache_control"] != nil {
		t.Fatalf("unexpected system cache_control: %+v", parsed.System)
	}
	if parsed.Tools[0]["cache_control"] != nil || parsed.Tools[1]["cache_control"] == nil {
		t.Fatalf("expected cache_control on last tool only: %+v", parsed.Tools)
	}
	if parsed.Messages[2].Content[0]["cache_control"] == nil {
		t.Fatalf("expected cache_control on rolling anchor messages[len-3]: %+v", parsed.Messages[2])
	}
	if parsed.Messages[4].Content[0]["cache_control"] == nil {
		t.Fatalf("expected cache_control on last message messages[len-1]: %+v", parsed.Messages[4])
	}
	if n := countCacheControl(t, rawRolling); n != 4 {
		t.Fatalf("total rolling breakpoints=%d, want <= 4 (exact 4)", n)
	}
}

func TestAnthropicRollingSkipsEphemeralTailBlocks(t *testing.T) {
	p := &anthropicProvider{cfg: Config{Model: "m", CacheBreakpoints: "rolling"}}
	turn1Req := CompletionRequest{
		System:          []string{"static"},
		DynamicBoundary: 1,
		Tools:           []ToolSchema{{Name: "Bash", InputSchema: map[string]any{"type": "object"}}},
		Messages:        []Message{UserText("u0"), EphemeralUserText("ephemeral_ctx_1")},
	}
	raw1, err := p.buildBody(turn1Req, true)
	if err != nil {
		t.Fatal(err)
	}
	var p1 struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw1, &p1); err != nil {
		t.Fatal(err)
	}
	if len(p1.Messages) != 1 || len(p1.Messages[0].Content) != 2 {
		t.Fatalf("expected merged turn1 user message with 2 blocks, got %+v", p1.Messages)
	}
	if p1.Messages[0].Content[0]["cache_control"] == nil || p1.Messages[0].Content[1]["cache_control"] != nil {
		t.Fatalf("breakpoint must land on stable block[0], not ephemeral tail block[1]: %+v", p1.Messages[0].Content)
	}

	turn2Req := turn1Req
	turn2Req.Messages = []Message{
		UserText("u0"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockToolUse, ID: "c1", Name: "Bash", Input: []byte(`{}`)}}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("c1", "ok", false)}},
		EphemeralUserText("ephemeral_ctx_2"),
	}
	raw2, err := p.buildBody(turn2Req, true)
	if err != nil {
		t.Fatal(err)
	}
	var p2 struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw2, &p2); err != nil {
		t.Fatal(err)
	}
	if len(p2.Messages) != 3 {
		t.Fatalf("expected 3 messages on turn2, got %+v", p2.Messages)
	}
	// Turn 2's messages[0] must keep breakpoint on Content[0] (matching Turn 1's cached prefix!)
	if p2.Messages[0].Content[0]["cache_control"] == nil {
		t.Fatalf("turn2 messages[0] did not anchor at Turn1 prefix block: %+v", p2.Messages[0])
	}
	// Turn 2's tail must land on ToolResult block, not ephemeral_ctx_2
	if p2.Messages[2].Content[0]["cache_control"] == nil || p2.Messages[2].Content[1]["cache_control"] != nil {
		t.Fatalf("turn2 tail breakpoint landed on ephemeral block: %+v", p2.Messages[2].Content)
	}

	// Standalone trailing EphemeralUserText after Assistant turn must fall back to Assistant block
	standReq := turn1Req
	standReq.Messages = []Message{
		UserText("u0"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("a0")}},
		EphemeralUserText("ephemeral_only"),
	}
	rawS, err := p.buildBody(standReq, true)
	if err != nil {
		t.Fatal(err)
	}
	var ps struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rawS, &ps); err != nil {
		t.Fatal(err)
	}
	if ps.Messages[0].Content[0]["cache_control"] == nil || ps.Messages[1].Content[0]["cache_control"] == nil || ps.Messages[2].Content[0]["cache_control"] != nil {
		t.Fatalf("standalone ephemeral tail shifted anchors wrongly: %+v", ps.Messages)
	}
}

func TestAnthropicCacheTTL1hOnStaticPrefixOnly(t *testing.T) {
	p := &anthropicProvider{cfg: Config{Model: "m", CacheBreakpoints: "rolling", CacheTTL: "1h"}}
	raw, err := p.buildBody(CompletionRequest{
		System:          []string{"static", "dyn"},
		DynamicBoundary: 1,
		Tools:           []ToolSchema{{Name: "Bash", InputSchema: map[string]any{"type": "object"}}},
		Messages:        []Message{UserText("u0")},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Tools    []map[string]any `json:"tools"`
		System   []map[string]any `json:"system"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	toolCC, _ := parsed.Tools[0]["cache_control"].(map[string]any)
	sysCC, _ := parsed.System[0]["cache_control"].(map[string]any)
	msgCC, _ := parsed.Messages[0].Content[0]["cache_control"].(map[string]any)
	if toolCC["ttl"] != "1h" || sysCC["ttl"] != "1h" {
		t.Fatalf("expected 1h TTL on static tools/system, got tool=%v sys=%v", toolCC, sysCC)
	}
	if _, hasTTL := msgCC["ttl"]; hasTTL || msgCC["type"] != "ephemeral" {
		t.Fatalf("expected default 5m ephemeral without ttl on rolling message breakpoint, got %v", msgCC)
	}
}

func TestOpenAIPromptCacheRetention(t *testing.T) {
	p := &openaiProvider{cfg: Config{Format: FormatOpenAI, Model: "m", PromptCacheRetention: "24h"}}
	raw, err := p.buildBody(CompletionRequest{Messages: []Message{UserText("hi")}, PromptCacheKey: "k1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["prompt_cache_key"] != "k1" || m["prompt_cache_retention"] != "24h" {
		t.Fatalf("got key=%v retention=%v", m["prompt_cache_key"], m["prompt_cache_retention"])
	}

	pDis := &openaiProvider{cfg: Config{Format: FormatOpenAI, Model: "m", DisablePromptCacheKey: true, PromptCacheRetention: "24h"}}
	rawDis, err := pDis.buildBody(CompletionRequest{Messages: []Message{UserText("hi")}, PromptCacheKey: "k1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var mDis map[string]any
	_ = json.Unmarshal(rawDis, &mDis)
	if mDis["prompt_cache_retention"] != nil || mDis["prompt_cache_key"] != nil {
		t.Fatalf("DisablePromptCacheKey must suppress both key and retention: %v", mDis)
	}
}

func countCacheControl(t *testing.T, raw []byte) int {
	t.Helper()
	var anyJSON any
	if err := json.Unmarshal(raw, &anyJSON); err != nil {
		t.Fatal(err)
	}
	var walk func(v any) int
	walk = func(v any) int {
		switch x := v.(type) {
		case map[string]any:
			n := 0
			if x["cache_control"] != nil {
				n++
			}
			for k, vv := range x {
				if k != "cache_control" {
					n += walk(vv)
				}
			}
			return n
		case []any:
			n := 0
			for _, vv := range x {
				n += walk(vv)
			}
			return n
		default:
			return 0
		}
	}
	return walk(anyJSON)
}

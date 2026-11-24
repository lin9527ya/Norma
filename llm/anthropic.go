package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
)

type anthropicProvider struct{ cfg Config }

type anthropicSystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type anthropicReq struct {
	Model         string                 `json:"model"`
	System        []anthropicSystemBlock `json:"system,omitempty"`
	Messages      []anthropicMessage     `json:"messages"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float64               `json:"temperature,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Thinking      *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutputConfig `json:"output_config,omitempty"`
	Stream        bool                   `json:"stream"`
}

type anthropicMessage struct {
	Role    Role                    `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	ContentBlock
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

// anthropicThinking is the "thinking":{"type":"enabled"|"disabled"} switch.
type anthropicThinking struct {
	Type string `json:"type"`
}

// anthropicOutputConfig carries the reasoning strength via
// "output_config":{"effort":"low"|"medium"|"high"|"max"}.
type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

func (p *anthropicProvider) PreservesThinking() bool {
	return (p.cfg.ThinkingType == "enabled" || p.cfg.ThinkingType == "adaptive" ||
		(p.cfg.ThinkingType != "disabled" && p.cfg.PreserveThinkingBlocks)) && p.cfg.ThinkingType != "disabled"
}

func ephemeralCacheControl(ttl string) map[string]any {
	if ttl == "1h" {
		return map[string]any{"type": "ephemeral", "ttl": "1h"}
	}
	return map[string]any{"type": "ephemeral"}
}

func (p *anthropicProvider) buildBody(req CompletionRequest, stream bool) ([]byte, error) {
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 8192
	}
	// Effective thinking mode: a non-empty per-request override (req.Thinking)
	// wins over Config.ThinkingType. Thinking-type content blocks may be replayed
	// when thinking is actively enabled/adaptive or PreserveThinkingBlocks is configured
	// for gateways that enable thinking server-side; explicit "disabled" always strips them.
	thinkingType := p.cfg.ThinkingType
	if req.Thinking != "" {
		thinkingType = req.Thinking
	}
	thinkingEnabled := (thinkingType == "enabled" || thinkingType == "adaptive" ||
		(thinkingType != "disabled" && p.cfg.PreserveThinkingBlocks)) && req.Thinking != "disabled"

	staticTTL := p.cfg.CacheTTL
	if req.CacheTTL != "" {
		staticTTL = req.CacheTTL
	}

	sysBlocks := buildAnthropicSystemWithTTL(req, staticTTL)
	sysBPs := 0
	for _, sb := range sysBlocks {
		if sb.CacheControl != nil {
			sysBPs++
		}
	}

	tools := make([]anthropicTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	toolBPs := 0
	if p.cfg.CacheBreakpoints == "rolling" && len(tools) > 0 && sysBPs < maxAnthropicCacheBreakpoints {
		tools[len(tools)-1].CacheControl = ephemeralCacheControl(staticTTL)
		toolBPs = 1
	}

	cleanMsgs := mergeAdjacentSameRole(filterThinkingBlocks(req.Messages, thinkingEnabled))
	msgBudget := 0
	if p.cfg.CacheBreakpoints == "rolling" {
		msgBudget = maxAnthropicCacheBreakpoints - sysBPs - toolBPs
	}
	body := anthropicReq{
		Model:         p.cfg.Model,
		Messages:      buildAnthropicMessages(cleanMsgs, msgBudget),
		Tools:         tools,
		MaxTokens:     maxTok,
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
		System:        sysBlocks,
	}
	if thinkingType != "" {
		body.Thinking = &anthropicThinking{Type: thinkingType}
	}
	if p.cfg.ReasoningEffort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: p.cfg.ReasoningEffort}
	}
	return json.Marshal(body)
}

const maxAnthropicCacheBreakpoints = 4

func buildAnthropicMessages(msgs []Message, bpBudget int) []anthropicMessage {
	out := make([]anthropicMessage, len(msgs))
	for i, m := range msgs {
		blocks := make([]anthropicContentBlock, len(m.Content))
		for j, b := range m.Content {
			blocks[j] = anthropicContentBlock{ContentBlock: b}
		}
		out[i] = anthropicMessage{Role: m.Role, Content: blocks}
	}
	if bpBudget <= 0 || len(out) == 0 {
		return out
	}
	// Exclude trailing pure-CacheSkip messages (e.g. standalone ephemeral tail
	// indices) when choosing anchor & tail positions so transient blocks never
	// consume cache breakpoints or shift Turn-1 -> Turn-2 prefix alignment.
	stableEnd := len(out) - 1
	for stableEnd >= 0 && !hasCacheableBlock(&out[stableEnd]) {
		stableEnd--
	}
	if stableEnd < 0 {
		return out
	}
	used := 0
	// Place a rolling cache read breakpoint two turns back (where the previous
	// request terminated) when enough stable history exists, plus a write breakpoint
	// on the latest stable block — staying within Anthropic's 4-breakpoint limit.
	if bpBudget >= 2 && stableEnd >= 1 {
		anchor := stableEnd - 2
		if anchor < 0 {
			anchor = 0
		}
		for idx := anchor; idx >= 0; idx-- {
			if markLastCacheableBlock(&out[idx]) {
				used++
				break
			}
		}
	}
	if used < bpBudget {
		for idx := stableEnd; idx >= 0; idx-- {
			if markLastCacheableBlock(&out[idx]) {
				break
			}
		}
	}
	return out
}

func hasCacheableBlock(m *anthropicMessage) bool {
	for j := len(m.Content) - 1; j >= 0; j-- {
		b := m.Content[j]
		if !b.CacheSkip && b.Type != BlockThinking && !(b.Type == BlockText && b.Text == "") {
			return true
		}
	}
	return false
}

func markLastCacheableBlock(m *anthropicMessage) bool {
	for j := len(m.Content) - 1; j >= 0; j-- {
		b := m.Content[j]
		if b.CacheSkip || b.Type == BlockThinking || (b.Type == BlockText && b.Text == "") {
			continue
		}
		if b.CacheControl != nil {
			return false
		}
		m.Content[j].CacheControl = map[string]any{"type": "ephemeral"}
		return true
	}
	return false
}

// buildAnthropicSystem splits the system prompt at DynamicBoundary, placing a
// cache breakpoint on the static prefix (FR-02.2).
func buildAnthropicSystem(req CompletionRequest) []anthropicSystemBlock {
	return buildAnthropicSystemWithTTL(req, req.CacheTTL)
}

func buildAnthropicSystemWithTTL(req CompletionRequest, staticTTL string) []anthropicSystemBlock {
	if len(req.System) == 0 {
		return nil
	}
	b := req.DynamicBoundary
	if b <= 0 {
		// No boundary set → no cache breakpoint.
		return []anthropicSystemBlock{{Type: "text", Text: joinSystem(req.System)}}
	}
	if b >= len(req.System) {
		// Boundary at/after the end → the whole system prompt is static; cache it
		// all in one block (an all-static prompt should still be cacheable).
		return []anthropicSystemBlock{{Type: "text", Text: joinSystem(req.System), CacheControl: ephemeralCacheControl(staticTTL)}}
	}
	return []anthropicSystemBlock{
		{Type: "text", Text: joinSystem(req.System[:b]), CacheControl: ephemeralCacheControl(staticTTL)},
		{Type: "text", Text: joinSystem(req.System[b:])},
	}
}

func (p *anthropicProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := p.buildBody(req, true)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
			r.Header.Set("content-type", "application/json")
			r.Header.Set("x-api-key", p.cfg.APIKey)
			r.Header.Set("anthropic-version", p.cfg.APIVersion)
			r.Header.Set("accept", "text/event-stream")
		}, "anthropic", true)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		scan := newSSEScanner(resp.Body)
		// 逐字段合并当前请求的累计 usage，兼容迟报缓存和重复快照。
		usage := anthropicUsageState{inputTokensIncludesCache: p.cfg.InputTokensIncludesCache}
		for {
			_, data, serr := scan.next()
			if serr == io.EOF {
				return
			}
			if serr != nil {
				yield(StreamEvent{}, serr)
				return
			}
			if data == "" {
				continue
			}
			ev, ok, perr := parseAnthropicFrame(data, &usage)
			if perr != nil {
				yield(StreamEvent{}, perr)
				return
			}
			if !ok {
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Complete performs a real non-streaming completion: it sends stream:false and
// parses the single JSON response from /v1/messages into a full assistant
// Message, returning it with the stop reason and normalized usage.
func (p *anthropicProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	body, err := p.buildBody(req, false)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
		r.Header.Set("content-type", "application/json")
		r.Header.Set("x-api-key", p.cfg.APIKey)
		r.Header.Set("anthropic-version", p.cfg.APIVersion)
		r.Header.Set("accept", "application/json")
	}, "anthropic", false)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	return parseAnthropicResponseWithOptions(raw, p.cfg.InputTokensIncludesCache)
}

// parseAnthropicResponse decodes a non-streaming Messages API response into the
// neutral Message model. Anthropic already speaks the content-block shape, so
// text/thinking/tool_use blocks map across directly.
func parseAnthropicResponse(raw []byte) (Message, string, Usage, error) {
	return parseAnthropicResponseWithOptions(raw, false)
}

func parseAnthropicResponseWithOptions(raw []byte, inputTokensIncludesCache bool) (Message, string, Usage, error) {
	var r struct {
		Type    string `json:"type"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
		Error      *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: decode response: %w (body: %s)", err, truncate(string(raw), 500))
	}
	if r.Error != nil || r.Type == "error" {
		msg := "error response"
		if r.Error != nil {
			msg = r.Error.Message
		}
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: %s", msg)
	}
	var usage Usage
	if r.Usage != nil {
		usage = r.Usage.normWithOptions(inputTokensIncludesCache)
	}
	var blocks []ContentBlock
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				blocks = append(blocks, TextBlock(b.Text))
			}
		case "thinking":
			blocks = append(blocks, ContentBlock{Type: BlockThinking, Thinking: b.Thinking, Signature: b.Signature})
		case "tool_use":
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			blocks = append(blocks, ContentBlock{Type: BlockToolUse, ID: b.ID, Name: b.Name, Input: in})
		}
	}
	return Message{Role: RoleAssistant, Content: blocks}, r.StopReason, usage, nil
}

func parseAnthropicFrame(data string, usage *anthropicUsageState) (StreamEvent, bool, error) {
	var f struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage   *anthropicUsage `json:"usage"`
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return StreamEvent{}, false, nil // tolerate non-JSON keepalives
	}
	switch f.Type {
	case "message_start":
		if f.Message.Usage != nil {
			// 起始 output 通常是占位值，不参与累计；输入/缓存按字段跟踪。
			return StreamEvent{Type: SEMessageStart, Usage: usage.delta(*f.Message.Usage, false)}, true, nil
		}
	case "content_block_start":
		if f.ContentBlock.Type == "tool_use" {
			return StreamEvent{Type: SEToolUseStart, ToolID: f.ContentBlock.ID, ToolName: f.ContentBlock.Name}, true, nil
		}
	case "content_block_delta":
		switch f.Delta.Type {
		case "text_delta":
			return StreamEvent{Type: SETextDelta, Text: f.Delta.Text}, true, nil
		case "thinking_delta":
			return StreamEvent{Type: SEThinkingDelta, Text: f.Delta.Thinking}, true, nil
		case "signature_delta":
			return StreamEvent{Type: SEThinkingSignature, Text: f.Delta.Signature}, true, nil
		case "input_json_delta":
			return StreamEvent{Type: SEToolInputJSON, Text: f.Delta.PartialJSON}, true, nil
		}
	case "message_delta":
		ev := StreamEvent{Type: SEMessageDelta, StopReason: f.Delta.StopReason}
		if f.Usage != nil {
			ev.Usage = usage.delta(*f.Usage, true)
		}
		return ev, true, nil
	case "message_stop":
		return StreamEvent{Type: SEMessageStop}, true, nil
	case "error":
		msg := "stream error"
		if f.Error != nil {
			msg = f.Error.Message
		}
		return StreamEvent{}, false, fmt.Errorf("anthropic: %s", msg)
	}
	return StreamEvent{}, false, nil
}

// Anthropic usage 是当前请求的累计快照，官方和兼容网关可在不同帧补报字段。
// 用字段级高水位只发增量：start 有 input 不等于它已经给出了最终 cache，重复 delta
// 的 output 也不能再次相加。每个 HTTP 请求各持有一份状态，不跨会话共享。
type anthropicUsageState struct {
	input, output, cacheRead, cacheWrite, totalIn int
	inputTokensIncludesCache                      bool
}

func tokenSnapshotDelta(value int, previous *int) int {
	if value <= *previous {
		return 0
	}
	delta := value - *previous
	*previous = value
	return delta
}

func (s *anthropicUsageState) delta(u anthropicUsage, includeOutput bool) Usage {
	rawIn := tokenSnapshotDelta(u.InputTokens, &s.input)
	read := tokenSnapshotDelta(u.cacheRead(), &s.cacheRead)
	write := tokenSnapshotDelta(u.CacheCreationInputTokens, &s.cacheWrite)
	in := rawIn + read + write
	if s.inputTokensIncludesCache {
		in = tokenSnapshotDelta(max(s.input, s.cacheRead+s.cacheWrite), &s.totalIn)
	}
	out := Usage{InputTokens: in, CacheReadTokens: read, CacheWriteTokens: write}
	if includeOutput {
		out.OutputTokens = tokenSnapshotDelta(u.OutputTokens, &s.output)
	}
	return out
}

type anthropicUsage struct {
	InputTokens              int             `json:"input_tokens"`
	OutputTokens             int             `json:"output_tokens"`
	CacheReadInputTokens     *int            `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens"`
	Opencode                 json.RawMessage `json:"opencode"`
}

func (u anthropicUsage) cacheRead() int {
	return firstTokenField(u.CacheReadInputTokens, gatewayCacheRead(u.Opencode))
}

// norm maps Anthropic usage to the canonical Usage. InputTokens is the TOTAL input
// INCLUDING cached tokens (OpenAI-style: prompt_tokens semantics) — Anthropic's raw
// input_tokens counts only the non-cached portion, so cache read+creation are folded
// in. CacheReadTokens/CacheWriteTokens are the cached SUBSET (⊆ InputTokens).
func (u anthropicUsage) norm() Usage {
	return u.normWithOptions(false)
}

func (u anthropicUsage) normWithOptions(inputTokensIncludesCache bool) Usage {
	read := u.cacheRead()
	write := u.CacheCreationInputTokens
	totalIn := u.InputTokens + read + write
	if inputTokensIncludesCache {
		totalIn = max(u.InputTokens, read+write)
	}
	return Usage{
		InputTokens:      totalIn,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  read,
		CacheWriteTokens: write,
	}
}

// filterThinkingBlocks normalizes message history before it is sent to the
// Anthropic Messages API. It strips thinking-type content blocks when thinking
// is disabled (Anthropic rejects replayed thinking blocks without the thinking
// parameter enabled), and drops any message left with no content blocks —
// Anthropic rejects an empty content array. An empty message arrives here two
// ways: a thinking-only turn that collapses to empty once thinking is stripped,
// or an empty/truncated assistant turn (only reasoning, or nothing) that was
// recorded with no text or tool_use. (A thinking-only turn WITH thinking
// enabled keeps its thinking block — that is valid content and must be
// replayed.)
func filterThinkingBlocks(msgs []Message, thinkingEnabled bool) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if !thinkingEnabled {
			filtered := make([]ContentBlock, 0, len(content))
			for _, b := range content {
				if b.Type != BlockThinking {
					filtered = append(filtered, b)
				}
			}
			content = filtered
		}
		if len(content) == 0 {
			continue // empty content array is rejected by Anthropic — drop it
		}
		if len(content) == len(m.Content) {
			out = append(out, m)
		} else {
			out = append(out, Message{Role: m.Role, Content: content})
		}
	}
	return out
}

// mergeAdjacentSameRole coalesces consecutive same-role messages into one by
// concatenating their content blocks. Anthropic requires messages to alternate
// user/assistant; dropping an empty turn in filterThinkingBlocks can leave two
// adjacent user messages (e.g. a tool result followed by a resume nudge, once
// the empty assistant between them is gone), which the API would reject with
// "roles must alternate". Merging preserves alternation without losing content.
// A fresh content slice is built on merge so the caller's message backing
// arrays are never mutated.
func mergeAdjacentSameRole(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			merged := make([]ContentBlock, 0, len(out[n-1].Content)+len(m.Content))
			merged = append(merged, out[n-1].Content...)
			merged = append(merged, m.Content...)
			out[n-1].Content = merged
			continue
		}
		out = append(out, m)
	}
	return out
}

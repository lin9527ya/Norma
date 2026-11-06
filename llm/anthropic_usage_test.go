package llm

import "testing"

// 官方 Anthropic：输入侧在 message_start；delta 回显同一份输入，不能再计一次。
func TestAnthropicUsageInputFromStart(t *testing.T) {
	gotInput := false
	start := `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":2}}}`
	ev, ok, err := parseAnthropicFrame(start, &gotInput)
	if err != nil || !ok {
		t.Fatalf("start: ok=%v err=%v", ok, err)
	}
	if ev.Usage.InputTokens != 12 || ev.Usage.CacheReadTokens != 2 || ev.Usage.OutputTokens != 0 {
		t.Fatalf("start usage = %+v，want input=12 cache=2 output=0", ev.Usage)
	}
	if !gotInput {
		t.Fatal("start 已报输入，应标记 gotInput")
	}
	delta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":8,"cache_read_input_tokens":2}}`
	ev, ok, err = parseAnthropicFrame(delta, &gotInput)
	if err != nil || !ok {
		t.Fatalf("delta: ok=%v err=%v", ok, err)
	}
	if ev.Usage.InputTokens != 0 || ev.Usage.CacheReadTokens != 0 || ev.Usage.OutputTokens != 8 {
		t.Fatalf("delta 不应重复计入输入：%+v", ev.Usage)
	}
}

// 兼容网关（opencode 等）：message_start 全 0，真实输入只在 message_delta。
func TestAnthropicUsageInputFromDelta(t *testing.T) {
	gotInput := false
	start := `{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	ev, ok, err := parseAnthropicFrame(start, &gotInput)
	if err != nil || !ok {
		t.Fatalf("start: ok=%v err=%v", ok, err)
	}
	if ev.Usage.InputTokens != 0 || gotInput {
		t.Fatalf("start 全 0 时不应标记 gotInput：%+v", ev.Usage)
	}
	delta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":31,"output_tokens":8,"cache_read_input_tokens":7}}`
	ev, ok, err = parseAnthropicFrame(delta, &gotInput)
	if err != nil || !ok {
		t.Fatalf("delta: ok=%v err=%v", ok, err)
	}
	if ev.Usage.InputTokens != 38 || ev.Usage.CacheReadTokens != 7 || ev.Usage.OutputTokens != 8 {
		t.Fatalf("delta 应补上输入侧：%+v", ev.Usage)
	}
	if !gotInput {
		t.Fatal("delta 补过输入后应标记 gotInput")
	}
	// 再来一个 delta（部分网关会重复发）：不能二次补。
	ev, _, _ = parseAnthropicFrame(delta, &gotInput)
	if ev.Usage.InputTokens != 0 || ev.Usage.OutputTokens != 8 {
		t.Fatalf("后续 delta 不应重复补输入：%+v", ev.Usage)
	}
}

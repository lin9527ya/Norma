package llm

import "testing"

func TestAnthropicUsageSnapshots(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   Usage
	}{
		{"official start input plus echoed delta", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":8,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}`,
		}, Usage{InputTokens: 15, OutputTokens: 8, CacheReadTokens: 2, CacheWriteTokens: 3}},
		{"zero start then full delta", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}}`,
			`{"type":"message_delta","usage":{"input_tokens":31,"output_tokens":8,"cache_read_input_tokens":7}}`,
		}, Usage{InputTokens: 38, OutputTokens: 8, CacheReadTokens: 7}},
		{"cache reported after start input", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":20,"cache_read_input_tokens":0}}}`,
			`{"type":"message_delta","usage":{"input_tokens":20,"output_tokens":8,"cache_read_input_tokens":80}}`,
		}, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"partial fields and repeated cumulative output", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":20}}}`,
			`{"type":"message_delta","usage":{"output_tokens":3}}`,
			`{"type":"message_delta","usage":{"output_tokens":8,"cache_read_input_tokens":80}}`,
			`{"type":"message_delta","usage":{"output_tokens":8,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}}`,
			`{"type":"message_delta","usage":{"input_tokens":20,"output_tokens":8,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}}`,
		}, Usage{InputTokens: 110, OutputTokens: 8, CacheReadTokens: 80, CacheWriteTokens: 10}},
		{"delta without start", []string{
			`{"type":"message_delta","usage":{"input_tokens":20,"output_tokens":8,"cache_read_input_tokens":80}}`,
		}, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := anthropicUsageState{}
			acc := NewAccumulator()
			for _, frame := range c.frames {
				ev, ok, err := parseAnthropicFrame(frame, &state)
				if err != nil || !ok {
					t.Fatalf("frame: ok=%v err=%v", ok, err)
				}
				acc.Add(ev)
			}
			if acc.Usage != c.want {
				t.Fatalf("usage=%+v want %+v", acc.Usage, c.want)
			}
		})
	}
}

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCacheReadFieldAliases(t *testing.T) {
	cases := []struct {
		name, raw string
		want      Usage
	}{
		{"openai canonical", `{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":80}}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"deepseek native", `{"prompt_tokens":100,"completion_tokens":8,"prompt_cache_hit_tokens":80}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"openai gateway", `{"prompt_tokens":100,"completion_tokens":8,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"canonical wins", `{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":60},"prompt_cache_hit_tokens":70,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 60}},
		{"canonical zero is present", `{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":80}`, Usage{InputTokens: 100, OutputTokens: 8}},
		{"empty detail falls back", `{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{},"prompt_cache_hit_tokens":80}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"native wins over gateway", `{"prompt_tokens":100,"completion_tokens":8,"prompt_cache_hit_tokens":60,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 60}},
		{"missing cache", `{"prompt_tokens":100,"completion_tokens":8}`, Usage{InputTokens: 100, OutputTokens: 8}},
		{"malformed alias cannot break canonical", `{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":80},"prompt_cache_hit_tokens":"invalid","opencode":"invalid"}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"malformed gateway cache ignored", `{"prompt_tokens":100,"completion_tokens":8,"opencode":{"tokens":{"cache_read":"invalid"}}}`, Usage{InputTokens: 100, OutputTokens: 8}},
		{"null alias allows gateway fallback", `{"prompt_tokens":100,"completion_tokens":8,"prompt_cache_hit_tokens":null,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, got, err := parseOpenAIResponse([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + c.raw + `}`))
			if err != nil || got != c.want {
				t.Fatalf("complete=%+v want=%+v err=%v", got, c.want, err)
			}
			_, _, got, has := parseOpenAIFrame(`{"choices":[],"usage":`+c.raw+`}`, map[int]bool{})
			if !has || got != c.want {
				t.Fatalf("frame=%+v want=%+v", got, c.want)
			}
		})
	}
}

func TestOpenAIUsageSnapshotsPreserveFields(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   Usage
	}{
		{"later cache omitted", []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":80}}}`,
			`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":8}}`,
		}, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"late alias and repeated cumulative output", []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":100}}`,
			`{"choices":[],"usage":{"completion_tokens":3}}`,
			`{"choices":[],"usage":{"completion_tokens":8,"prompt_cache_hit_tokens":80}}`,
			`{"choices":[],"usage":{"completion_tokens":8}}`,
		}, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"nonmonotonic partial snapshot", []string{
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":8,"prompt_cache_hit_tokens":80}}`,
			`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":3,"prompt_cache_hit_tokens":0}}`,
		}, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range c.frames {
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()
			p, err := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fake"})
			if err != nil {
				t.Fatal(err)
			}
			acc := NewAccumulator()
			for ev, err := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("fixture")}}) {
				if err != nil {
					t.Fatal(err)
				}
				acc.Add(ev)
			}
			if acc.Usage != c.want {
				t.Fatalf("usage=%+v want=%+v", acc.Usage, c.want)
			}
		})
	}
}

func TestOpenAIUsageSnapshotsAreRequestLocal(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i, read := 100, 80
		if calls.Add(1) > 1 {
			i, read = 50, 0
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":8,\"prompt_cache_hit_tokens\":%d}}\n\ndata: [DONE]\n\n", i, read)
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fake"})
	for _, want := range []Usage{{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}, {InputTokens: 50, OutputTokens: 8}} {
		acc := NewAccumulator()
		for ev, err := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("fixture")}}) {
			if err != nil {
				t.Fatal(err)
			}
			acc.Add(ev)
		}
		if acc.Usage != want {
			t.Fatalf("usage=%+v want=%+v", acc.Usage, want)
		}
	}
}

func TestAnthropicGatewayCacheReadAlias(t *testing.T) {
	cases := []struct {
		name, raw string
		want      Usage
	}{
		{"gateway only", `{"input_tokens":20,"output_tokens":8,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}},
		{"canonical wins", `{"input_tokens":20,"output_tokens":8,"cache_read_input_tokens":60,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 80, OutputTokens: 8, CacheReadTokens: 60}},
		{"canonical zero is present", `{"input_tokens":20,"output_tokens":8,"cache_read_input_tokens":0,"opencode":{"tokens":{"cache_read":80}}}`, Usage{InputTokens: 20, OutputTokens: 8}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := anthropicUsageState{}
			acc := NewAccumulator()
			for _, frame := range []string{`{"type":"message_start","message":{"usage":{"input_tokens":20}}}`, `{"type":"message_delta","usage":` + c.raw + `}`, `{"type":"message_delta","usage":` + c.raw + `}`} {
				ev, ok, err := parseAnthropicFrame(frame, &state)
				if err != nil || !ok {
					t.Fatal(err)
				}
				acc.Add(ev)
			}
			if acc.Usage != c.want {
				t.Fatalf("stream=%+v want=%+v", acc.Usage, c.want)
			}
			_, _, got, err := parseAnthropicResponse([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":` + c.raw + `}`))
			if err != nil || got != c.want {
				t.Fatalf("complete=%+v want=%+v err=%v", got, c.want, err)
			}
			raw, _ := json.Marshal(acc.Usage)
			if len(raw) == 0 {
				t.Fatal("usage serialization failed")
			}
		})
	}
}

func TestOpenAIUsageSnapshotsResetOnEmptyRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":900,\"prompt_cache_hit_tokens\":800}}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":8}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fake", EmptyResponseInterval: 1})
	acc := NewAccumulator()
	for ev, err := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("fixture")}}) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
	}
	if acc.Usage != (Usage{InputTokens: 50, OutputTokens: 8}) || calls.Load() != 2 {
		t.Fatalf("retry usage=%+v calls=%d", acc.Usage, calls.Load())
	}
}

func TestOpenAIUsageSnapshotsConcurrentRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad fixture", 400)
			return
		}
		input, read := 100, 80
		if request.Messages[0].Content == "second" {
			input, read = 50, 0
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":8,\"prompt_cache_hit_tokens\":%d}}\n\ndata: [DONE]\n\n", input, read)
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "fake"})
	type result struct {
		usage Usage
		err   error
	}
	ch := make(chan result, 2)
	for _, label := range []string{"first", "second"} {
		go func() {
			acc := NewAccumulator()
			for ev, err := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText(label)}}) {
				if err != nil {
					ch <- result{err: err}
					return
				}
				acc.Add(ev)
			}
			ch <- result{usage: acc.Usage}
		}()
	}
	counts := map[Usage]int{}
	for range 2 {
		r := <-ch
		if r.err != nil {
			t.Fatal(r.err)
		}
		counts[r.usage]++
	}
	if counts[Usage{InputTokens: 100, OutputTokens: 8, CacheReadTokens: 80}] != 1 || counts[Usage{InputTokens: 50, OutputTokens: 8}] != 1 {
		t.Fatalf("shared request state: %+v", counts)
	}
}

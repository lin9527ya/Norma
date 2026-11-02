package agentcore

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lin9527ya/Norma/llm"
)

// AppendMessages 是宿主改写上下文（跨轮复用会话）的唯一入口：它必须让消息进入
// 后续请求，且不触碰 system prompt / 工具 schema 这两块缓存前缀。
func TestAppendMessagesReachNextRequest(t *testing.T) {
	var bodies []string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Write([]byte(sse(
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`, ``,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`, ``,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, ``,
				`data: {"type":"message_stop"}`, ``,
			)))
			return
		}
		w.Write([]byte(sse(
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`, ``,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`, ``,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, ``,
			`data: {"type":"message_stop"}`, ``,
		)))
	}))
	defer srv.Close()

	prov, _ := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "m"})
	s := NewSession(Options{Provider: prov, SystemPrompt: []string{"stable system"}})
	for range s.Prompt(context.Background(), "第一轮输入") {
	}
	s.AppendMessages(
		llm.UserText("上一轮问：拿下后台"),
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("结论：登录口有注入")}},
	)
	for range s.Prompt(context.Background(), "下一轮输入") {
	}

	if len(bodies) < 2 {
		t.Fatalf("应有两次请求，got %d", len(bodies))
	}
	second := bodies[1]
	for _, want := range []string{"上一轮问：拿下后台", "登录口有注入", "下一轮输入"} {
		if !strings.Contains(second, want) {
			t.Fatalf("第二次请求缺少 %q：\n%s", want, second)
		}
	}
	// 缓存前缀（system）必须与第一轮逐字一致。
	if !strings.Contains(second, "stable system") {
		t.Fatal("system prompt 丢失")
	}
}

// 空调用是无操作（宿主可以无条件调用，不必先判空）。
func TestAppendMessagesEmptyIsNoop(t *testing.T) {
	s := NewSession(Options{})
	s.AppendMessages()
	if len(s.Messages()) != 0 {
		t.Fatal("空 AppendMessages 不应产生消息")
	}
}

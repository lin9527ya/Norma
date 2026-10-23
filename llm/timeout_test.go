package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDoStreamConnectTimeout：服务端一直不发响应头时，连接阶段的 deadline 必须
// 主动取消请求——被休眠/代理弄死的连接可能卡在"请求写不出去"，TCP 不会给 FIN/RST，
// 光靠 ResponseHeaderTimeout（请求写完后才计时）兜不住。
func TestDoStreamConnectTimeout(t *testing.T) {
	old := llmConnectTimeout
	llmConnectTimeout = 120 * time.Millisecond
	defer func() { llmConnectTimeout = old }()

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // hold the headers until the client gives up (or the test ends)
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(func() { close(done); srv.CloseClientConnections(); srv.Close() })

	cfg := Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: -1}
	start := time.Now()
	_, err := doStream(context.Background(), cfg, srv.URL, []byte(`{}`), func(*http.Request) {}, "test", true)
	if !errors.Is(err, errLLMConnectTimeout) {
		t.Fatalf("err=%v, want connect timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("connect timeout took %s", elapsed)
	}
}

// TestDoStreamStreamIdleTimeout：响应头已到、流里来过一段数据后彻底安静——空闲看门狗
// 必须取消请求，并让 Read 返回可识别的错误（否则调用方会一直挂到 run 预算耗尽）。
func TestDoStreamStreamIdleTimeout(t *testing.T) {
	oldIdle, oldConn := llmStreamIdleTimeout, llmConnectTimeout
	llmStreamIdleTimeout = 120 * time.Millisecond
	llmConnectTimeout = 2 * time.Second
	defer func() { llmStreamIdleTimeout, llmConnectTimeout = oldIdle, oldConn }()

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"x\":1}\n\n"))
		w.(http.Flusher).Flush()
		select { // then go silent
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(func() { close(done); srv.CloseClientConnections(); srv.Close() })

	cfg := Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: -1}
	resp, err := doStream(context.Background(), cfg, srv.URL, []byte(`{}`), func(*http.Request) {}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 1024)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	start := time.Now()
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			if !errors.Is(err, errLLMStreamIdle) {
				t.Fatalf("err=%v, want stream idle", err)
			}
			break
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("idle timeout did not fire")
		}
	}
}

// TestDoStreamNonStreamNotIdleTimed：非流式响应（一次性 JSON）不能被流空闲看门狗
// 杀掉——它在完整生成前本来就没有字节可读。
func TestDoStreamNonStreamNotIdleTimed(t *testing.T) {
	oldIdle := llmStreamIdleTimeout
	llmStreamIdleTimeout = 50 * time.Millisecond
	defer func() { llmStreamIdleTimeout = oldIdle }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond) // longer than the idle timeout
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })

	cfg := Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: -1}
	resp, err := doStream(context.Background(), cfg, srv.URL, []byte(`{}`), func(*http.Request) {}, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if n == 0 {
		t.Fatalf("non-stream body read: n=%d err=%v", n, err)
	}
}

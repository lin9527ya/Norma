package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// LLM 调用的两端超时。**不能用 http.Client.Timeout**：它按整段响应计时，会把正常的长流
// 截断。这里拆成两段：
//
//   - 连接阶段（请求写完 + 收到响应头）：一条被中间设备/休眠唤醒弄死的连接，可能卡在
//     "请求体写不出去"上——此时 TCP 没有 FIN/RST，net/http 的 ResponseHeaderTimeout 也
//     不会启动（它在请求写完后才计时）。只有给 client.Do 一个独立的 deadline 才兜得住。
//   - 流空闲：收到响应头后，SSE 每个事件都会重置计时；连续 N 秒没有任何字节就取消请求。
//     长思考模型在首 token 前可能安静一阵，所以这个值要比连接超时宽松。
//
// 两个值用 var（不是 const）只为测试能缩短；生产值按"最多卡几分钟"取，而不是按通常延迟取。
var (
	llmConnectTimeout    = 2 * time.Minute
	llmStreamIdleTimeout = 3 * time.Minute
)

var (
	errLLMConnectTimeout = errors.New("llm: 连接/响应头超时")
	errLLMStreamIdle     = errors.New("llm: 流空闲超时")
)

// idleReadCloser 给流式响应体加空闲看门狗：连续 llmStreamIdleTimeout 没有新字节就取消
// 请求（net/http 会因此关闭连接、让阻塞中的 Read 返回）。收到任何字节都会重置计时。
type idleReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	timer  *time.Timer
	fired  atomic.Bool
}

func (b *idleReadCloser) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(llmStreamIdleTimeout)
	}
	if err != nil && b.fired.Load() {
		return n, fmt.Errorf("%w（%s 无数据）: %v", errLLMStreamIdle, llmStreamIdleTimeout, err)
	}
	return n, err
}

func (b *idleReadCloser) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

// closeWithCancel 让非流式响应在 Close 时释放 attempt context（否则每次调用都会漏一个
// cancel func 与它挂着的 timer）。
type closeWithCancel struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *closeWithCancel) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// doStream sends the streaming request with retries on transient failures and
// returns a 200 response (caller reads + closes the body). Only the request
// establishment phase is retried — connection reset / timeout / 429 / 5xx before
// any stream data is consumed — so retrying is safe (it can't duplicate
// partially-streamed assistant output). A mid-stream drop is the caller's to
// surface. Context cancellation is never retried and interrupts the backoff.
// Count comes from cfg.retries(), the wait between attempts from
// cfg.retryDelay() (exponential by default, fixed when Config.RetryInterval is set).
func doStream(ctx context.Context, cfg Config, url string, body []byte, setHeaders func(*http.Request), errPrefix string, stream bool) (*http.Response, error) {
	// Rate limit once per logical request (outside the retry loop, so retries do
	// not consume additional tokens). Shared across all callers of this provider.
	if err := cfg.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	// A Config assembled by hand (an embedder, a test) may carry no client; a nil
	// interface dereference would panic mid-request. Fall back to the same default
	// NewProvider installs, so the request either works or fails like any other
	// network error.
	client := cfg.HTTPClient
	if client == nil {
		if c, err := defaultHTTPClient(cfg.Proxy); err == nil {
			client = c
		} else {
			client = http.DefaultClient
		}
	}
	retries := cfg.retries()
	for attempt := 0; ; attempt++ {
		// 每次尝试一个独立的 attempt context：连接阶段用它兜住"请求写不出去/等不到
		// 响应头"；成功后流式响应把它交给空闲看门狗，非流式交给 Close。
		attemptCtx, cancel := context.WithCancel(ctx)
		connectTimer := time.AfterFunc(llmConnectTimeout, cancel)

		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			connectTimer.Stop()
			cancel()
			return nil, err
		}
		setHeaders(req)

		resp, err := client.Do(req)
		connectTimer.Stop()
		if err == nil && resp.StatusCode == http.StatusOK {
			if !stream {
				resp.Body = &closeWithCancel{ReadCloser: resp.Body, cancel: cancel}
				return resp, nil
			}
			ib := &idleReadCloser{rc: resp.Body, cancel: cancel}
			ib.timer = time.AfterFunc(llmStreamIdleTimeout, func() {
				ib.fired.Store(true)
				cancel()
			})
			resp.Body = ib
			return resp, nil
		}
		// 我们自己掐断的连接（连接超时）不是真正的 context 取消：还原成可读的错误，
		// 并保留重试资格（父 ctx 仍然活着）。
		if err != nil && attemptCtx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("%w（%s）: %v", errLLMConnectTimeout, llmConnectTimeout, err)
		}
		cancel()

		retryable := false
		switch {
		case err != nil:
			retryable = ctx.Err() == nil // transient network error (not cancellation)
		case isRetryableStatus(resp.StatusCode):
			retryable = true
		}
		if attempt >= retries || !retryable {
			if err != nil {
				return nil, err
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("%s: status %d: %s", errPrefix, resp.StatusCode, strings.TrimSpace(string(data)))
		}
		if resp != nil {
			resp.Body.Close()
		}
		if !backoffSleep(ctx, cfg.retryDelay(attempt)) {
			return nil, ctx.Err() // context cancelled during backoff
		}
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// expBackoff is the default retry ladder: 0.5s, 1s, 2s, … capped at 8s. Used
// whenever the caller did not pin a fixed interval on the Config.
func expBackoff(attempt int) time.Duration {
	d := 500 * time.Millisecond * (1 << attempt)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// backoffSleep waits d, interruptible by ctx. Returns false if the context was
// cancelled (or d is non-positive, which never happens for a real wait).
func backoffSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

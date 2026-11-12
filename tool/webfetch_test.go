package tool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WebFetch 发给目标站点的默认 UA 必须是普通浏览器，不能带平台/客户端标识。
func TestWebFetchNeutralUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>ok</body></html>"))
	}))
	defer srv.Close()

	tl := NewWebFetch(WebFetchConfig{})
	if _, err := tl.Call(context.Background(), []byte(fmt.Sprintf(`{"url":%q}`, srv.URL)), &ToolContext{}); err != nil {
		t.Fatal(err)
	}
	if got != defaultWebFetchUserAgent || strings.Contains(strings.ToLower(got), "norma") {
		t.Fatalf("default UA = %q", got)
	}

	tl = NewWebFetch(WebFetchConfig{UserAgent: "CustomUA/1.0"})
	if _, err := tl.Call(context.Background(), []byte(fmt.Sprintf(`{"url":%q}`, srv.URL)), &ToolContext{}); err != nil {
		t.Fatal(err)
	}
	if got != "CustomUA/1.0" {
		t.Fatalf("configured UA = %q", got)
	}
}

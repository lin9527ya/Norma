package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PathGuard 要挡住 Read，且**软链不能绕过**（判定用的是解析后的真实路径）。
func TestPathGuardRejectsReadAndFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetPathGuard(nil) })
	SetPathGuard(func(abs string) error {
		if filepath.Base(abs) == "secret.txt" {
			return fmt.Errorf("拒绝访问（测试）")
		}
		return nil
	})

	call := func(tool CoreTool, in map[string]any) Result {
		t.Helper()
		raw, _ := json.Marshal(in)
		res, err := tool.Call(context.Background(), raw, &ToolContext{WorkingDir: dir})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		return res
	}

	if res := call(NewRead(), map[string]any{"file_path": "secret.txt"}); !res.IsError || strings.Contains(res.Flatten(), "TOP-SECRET") {
		t.Fatalf("直接读被拒文件没拦住：%q", res.Flatten())
	}
	if res := call(NewRead(), map[string]any{"file_path": "link.txt"}); !res.IsError || strings.Contains(res.Flatten(), "TOP-SECRET") {
		t.Fatalf("软链绕过了 guard：%q", res.Flatten())
	}
	if res := call(NewRead(), map[string]any{"file_path": "ok.txt"}); res.IsError || !strings.Contains(res.Flatten(), "hello") {
		t.Fatalf("正常文件被误伤：%q", res.Flatten())
	}
	// 写新文件（目标还不存在）也要能过 guard：按最深的已存在祖先判定。
	if res := call(NewWrite(), map[string]any{"file_path": "new/thing.txt", "content": "x"}); res.IsError {
		t.Fatalf("允许路径下的新文件被误伤：%q", res.Flatten())
	}
	if res := call(NewWrite(), map[string]any{"file_path": "secret.txt", "content": "x"}); !res.IsError {
		t.Fatal("对被拒文件的写入没拦住")
	}
}

// Grep：装了 guard 时必须逐文件判定（并绕过 rg 子进程），被拒文件的内容不能出现在结果里。
func TestPathGuardBlocksGrepContent(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "secret.txt"), []byte("NEEDLE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ok.txt"), []byte("NEEDLE-OK"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetPathGuard(nil) })
	SetPathGuard(func(abs string) error {
		if filepath.Base(abs) == "secret.txt" {
			return fmt.Errorf("拒绝访问（测试）")
		}
		return nil
	})

	raw, _ := json.Marshal(map[string]any{"pattern": "NEEDLE", "path": ".", "output_mode": "content"})
	res, err := NewGrep().Call(context.Background(), raw, &ToolContext{WorkingDir: dir})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := res.Flatten()
	if strings.Contains(out, "NEEDLE-SECRET") {
		t.Fatalf("被 guard 拒绝的文件内容被打出来了：%q", out)
	}
	if !strings.Contains(out, "NEEDLE-OK") {
		t.Fatalf("正常文件没被搜到：%q", out)
	}
}

// resolveRealPath：目标不存在时用最深的已存在祖先解析，软链按真实路径算。
func TestResolveRealPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveRealPath(link); err != nil || got != real {
		t.Fatalf("软链应解析为 %q，得到 %q err=%v", real, got, err)
	}
	if got, err := resolveRealPath(filepath.Join(dir, "not-yet.txt")); err != nil || got != filepath.Join(dir, "not-yet.txt") {
		t.Fatalf("不存在的目标应保留在已存在祖先之下，得到 %q err=%v", got, err)
	}
}

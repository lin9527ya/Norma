package tool

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// ExecHook 必须作用到 Bash 的子进程上，并且能改它的 Env——宿主靠这个做降权 + 环境清洗。
func TestExecHookAppliesToBash(t *testing.T) {
	t.Setenv("AGENT_CORE_DISABLE_BACKGROUND_TASKS", "1") // 走同步路径，便于断言
	t.Cleanup(func() { SetExecHook(nil) })

	var calls int
	SetExecHook(func(cmd *exec.Cmd) {
		calls++
		cmd.Env = append(cmd.Env, "HOOK_MARK=on")
	})

	raw, _ := json.Marshal(map[string]any{"command": "echo mark=$HOOK_MARK"})
	res, err := NewBash().Call(context.Background(), raw, &ToolContext{})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	out := res.Flatten()
	if !strings.Contains(out, "mark=on") {
		t.Fatalf("钩子没有改到子进程 Env：%q", out)
	}
	if calls == 0 {
		t.Fatal("钩子没被调用")
	}
}

// 清除钩子后行为回到原样（默认关闭时对其他使用方零影响）。
func TestExecHookCanBeCleared(t *testing.T) {
	t.Setenv("AGENT_CORE_DISABLE_BACKGROUND_TASKS", "1")
	SetExecHook(func(cmd *exec.Cmd) { cmd.Env = append(cmd.Env, "HOOK_MARK=on") })
	SetExecHook(nil)

	raw, _ := json.Marshal(map[string]any{"command": "echo mark=$HOOK_MARK"})
	res, err := NewBash().Call(context.Background(), raw, &ToolContext{})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if strings.Contains(res.Flatten(), "mark=on") {
		t.Fatalf("清掉钩子后仍在生效：%q", res.Flatten())
	}
}

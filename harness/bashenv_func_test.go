package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lin9527ya/Norma/llm"
	"github.com/lin9527ya/Norma/permission"
	"github.com/lin9527ya/Norma/tool"
)

// BashEnvFunc 必须在**每次工具调用**时求值，而不是建 loop 时快照一次：
// 宿主的出口可能在 run 中途变化（代理池故障转移/启动探测完成），一次性快照
// 会让整轮 Bash 卡在旧值上（BLADE 任务 26 实测：重启窗口里冻结了黑洞代理）。
func TestBashEnvFuncEvaluatedPerCall(t *testing.T) {
	envs := [][]string{
		{"HTTP_PROXY=http://127.0.0.1:9"},
		{"HTTP_PROXY=http://192.168.10.137:7898"},
	}
	calls := 0
	var seen []string
	spec := tool.Build(tool.Spec{
		Name:   "EnvProbe",
		Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Decision{Behavior: permission.Allow}
		},
		Run: func(_ context.Context, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			seen = append(seen, tc.Env...)
			return tool.Result{Content: []llm.ContentBlock{llm.TextBlock("ok")}}, nil
		},
	})
	l := &loop{ctx: context.Background(), in: QueryInput{
		Tools:          tool.NewRegistry(spec),
		PermissionMode: permission.ModeBypass,
		// 静态 BashEnv 同时存在时也必须被 Func 覆盖。
		BashEnv: []string{"HTTP_PROXY=static-must-not-be-used"},
		BashEnvFunc: func() []string {
			env := envs[calls]
			calls++
			return env
		},
	}}
	for i := 0; i < 2; i++ {
		l.execOne(llm.ContentBlock{Type: llm.BlockToolUse, ID: "t1", Name: "EnvProbe", Input: json.RawMessage(`{}`)}, nil)
	}
	if len(seen) != 2 || seen[0] != "HTTP_PROXY=http://127.0.0.1:9" || seen[1] != "HTTP_PROXY=http://192.168.10.137:7898" {
		t.Fatalf("每次调用都应重新求值并以 Func 为准，实际 %v", seen)
	}
}

// 没设 Func 时行为不变：静态 BashEnv 原样下发。
func TestBashEnvStaticStillWorks(t *testing.T) {
	var seen []string
	spec := tool.Build(tool.Spec{
		Name:   "EnvProbe",
		Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Decision{Behavior: permission.Allow}
		},
		Run: func(_ context.Context, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			seen = tc.Env
			return tool.Result{Content: []llm.ContentBlock{llm.TextBlock("ok")}}, nil
		},
	})
	l := &loop{ctx: context.Background(), in: QueryInput{
		Tools:          tool.NewRegistry(spec),
		PermissionMode: permission.ModeBypass,
		BashEnv:        []string{"A=1", "B=2"},
	}}
	l.execOne(llm.ContentBlock{Type: llm.BlockToolUse, ID: "t1", Name: "EnvProbe", Input: json.RawMessage(`{}`)}, nil)
	if len(seen) != 2 || seen[0] != "A=1" || seen[1] != "B=2" {
		t.Fatalf("静态 BashEnv 应原样下发，实际 %v", seen)
	}
}

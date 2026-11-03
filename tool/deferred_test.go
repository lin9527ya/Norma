package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// echoTool is a trivial deferred tool that echoes its "msg" param.
func echoTool() CoreTool {
	return Build(Spec{
		Name:        "echo",
		Description: "Echoes the msg parameter back.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
			"required":   []any{"msg"},
		},
		Run: func(_ context.Context, in json.RawMessage, _ *ToolContext) (Result, error) {
			var a struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(in, &a)
			return Text("echo:" + a.Msg), nil
		},
	})
}

func TestUnlockSet(t *testing.T) {
	s := NewUnlockSet("a", "b")
	if !s.Has("a") || !s.Has("b") || s.Has("c") {
		t.Fatalf("seed wrong: %v", s.List())
	}
	s.Add("c")
	if !s.Has("c") {
		t.Fatal("Add failed")
	}
	if got := s.List(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("List not sorted/complete: %v", got)
	}
}

func TestRenderDeferredToolsBlock(t *testing.T) {
	if RenderDeferredToolsBlock(nil) != "" {
		t.Fatal("empty names should render empty")
	}
	out := RenderDeferredToolsBlock([]string{"mcp__b__y", "mcp__a__x"})
	if !strings.Contains(out, "<available-deferred-tools>") ||
		!strings.Contains(out, "mcp__a__x") || !strings.Contains(out, "mcp__b__y") ||
		!strings.Contains(out, ExecuteExtraToolName) {
		t.Fatalf("block missing parts:\n%s", out)
	}
	// sorted: a before b
	if strings.Index(out, "mcp__a__x") > strings.Index(out, "mcp__b__y") {
		t.Fatal("names not sorted")
	}
}

func TestSearchExtraTools(t *testing.T) {
	reg := NewRegistry(echoTool())
	search := NewSearchExtraTools(reg, []string{"echo"})

	// exact select
	r, _ := search.Call(context.Background(), json.RawMessage(`{"query":"select:echo"}`), nil)
	txt := r.Content[0].Text
	if !strings.Contains(txt, "## echo") || !strings.Contains(txt, "params schema") {
		t.Fatalf("select:echo missing schema:\n%s", txt)
	}
	// keyword
	r, _ = search.Call(context.Background(), json.RawMessage(`{"query":"echoes"}`), nil)
	if !strings.Contains(r.Content[0].Text, "## echo") {
		t.Fatalf("keyword search failed:\n%s", r.Content[0].Text)
	}
	// non-deferred name → not found (registry has it but it is not in deferred list)
	reg2 := NewRegistry(echoTool())
	search2 := NewSearchExtraTools(reg2, []string{"other"})
	r, _ = search2.Call(context.Background(), json.RawMessage(`{"query":"select:echo"}`), nil)
	if !strings.Contains(r.Content[0].Text, "No matching") {
		t.Fatalf("non-deferred should not surface:\n%s", r.Content[0].Text)
	}
}

func TestExecuteExtraTool(t *testing.T) {
	reg := NewRegistry(echoTool())
	unlock := NewUnlockSet("echo")
	exec := NewExecuteExtraTool(reg, unlock)

	// unlocked → runs
	r, _ := exec.Call(context.Background(), json.RawMessage(`{"tool_name":"echo","params":{"msg":"hi"}}`), nil)
	if r.IsError || r.Content[0].Text != "echo:hi" {
		t.Fatalf("expected echo:hi, got %+v", r)
	}

	// locked → rejected
	locked := NewExecuteExtraTool(reg, NewUnlockSet())
	r, _ = locked.Call(context.Background(), json.RawMessage(`{"tool_name":"echo","params":{"msg":"hi"}}`), nil)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "not unlocked") {
		t.Fatalf("locked tool should be rejected, got %+v", r)
	}

	// unknown tool
	r, _ = exec.Call(context.Background(), json.RawMessage(`{"tool_name":"nope","params":{}}`), nil)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "unknown tool") {
		t.Fatalf("unknown tool should error, got %+v", r)
	}

	// invalid params (missing required msg) → schema validation error
	r, _ = exec.Call(context.Background(), json.RawMessage(`{"tool_name":"echo","params":{}}`), nil)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "invalid params") {
		t.Fatalf("invalid params should error, got %+v", r)
	}
}

// 模型常一次问多个工具名（"chrome-devtools navigate_page new_page list_pages"）。
// 严格 AND 在现实中没有任何单个工具同时命中全部 token，整条查询零命中，模型据此
// 判定"这些工具不可用"（BLADE 任务 22 实测）。零命中时必须退化成按命中数排序的
// 近似匹配，而不是一句 No matching。
func TestSearchExtraToolsFallsBackToRankedMatch(t *testing.T) {
	names := []string{
		"mcp__chrome-devtools__navigate_page",
		"mcp__chrome-devtools__new_page",
		"mcp__chrome-devtools__list_pages",
		"mcp__playwright__browser_navigate",
	}
	var tools []CoreTool
	for _, n := range names {
		tools = append(tools, Build(Spec{Name: n, Description: "browser page navigation tool", Schema: map[string]any{"type": "object"}}))
	}
	search := NewSearchExtraTools(NewRegistry(tools...), names)
	in, _ := json.Marshal(map[string]any{"query": "chrome-devtools navigate_page new_page list_pages"})
	res, err := search.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Flatten()
	if strings.Contains(got, "No matching") {
		t.Fatalf("多工具提问不应零命中：%s", got)
	}
	// 三个 chrome-devtools 工具都应被列出（playwright 那个不含 chrome-devtools，排在后面或缺失都行）。
	for _, want := range []string{"navigate_page", "new_page", "list_pages"} {
		if !strings.Contains(got, want) {
			t.Fatalf("回退结果缺少 %s：%s", want, got)
		}
	}

	// 完全无关的查询仍然如实零命中——回退不能变成"问什么都给一堆"。
	in2, _ := json.Marshal(map[string]any{"query": "zzz-nonexistent-token"})
	res2, err := search.Call(context.Background(), in2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res2.Flatten(), "No matching") {
		t.Fatalf("无关查询应零命中：%s", res2.Flatten())
	}
}

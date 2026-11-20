package harness

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/lin9527ya/Norma/llm"
	"github.com/lin9527ya/Norma/permission"
	"github.com/lin9527ya/Norma/tool"
)

// On hitting MaxTurns with a Settlement configured, the loop injects the wrap-up
// prompt, hides DisabledTools during wrap-up, and finishes with the ORIGINAL
// budget reason (not ReasonCompleted).
func TestSettlementOnBudgetHit(t *testing.T) {
	var toolsSeen [][]string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`), // turn 1 → hits MaxTurns=1
		textTurn("已写回事实并总结"),                          // settlement turn → natural stop
	}
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(toolsSeen)
		toolsSeen = append(toolsSeen, names)
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW 请写回事实", DisabledTools: []string{"Bash"}},
	}
	term := drain(t, in, QueryDeps{CallModel: call})

	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns（收尾后仍报原预算 reason）", term.Reason)
	}
	found := false
	for _, m := range term.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Text, "SETTLE_NOW") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("收尾提示未被注入到对话")
	}
	if len(toolsSeen) < 2 {
		t.Fatalf("应至少 2 次模型调用(主+收尾)，得 %d", len(toolsSeen))
	}
	if !contains(toolsSeen[0], "Bash") {
		t.Fatalf("主阶段应能看到 Bash: %v", toolsSeen[0])
	}
	if contains(toolsSeen[1], "Bash") {
		t.Fatalf("收尾阶段应屏蔽 Bash: %v", toolsSeen[1])
	}
}

func TestSettlementKeepToolSchemasPreservesToolsAndBlocksExecution(t *testing.T) {
	var toolsSeen [][]string
	var blockedMsg string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`), // turn 1 -> hits MaxTurns=1
		toolTurn("b", "Bash", `{"command":"echo 2"}`), // settlement turn 1 tries Bash -> blocked at execution
		textTurn("done"),                              // settlement turn 2 stops
	}
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(toolsSeen)
		toolsSeen = append(toolsSeen, names)
		if i == 2 {
			// Check that the previous turn returned the disabled tool error result.
			last := req.Messages[len(req.Messages)-1]
			for _, b := range last.Content {
				if b.Type == llm.BlockToolResult && b.ToolUseID == "b" {
					for _, inner := range b.Content {
						blockedMsg += inner.Text
					}
				}
			}
		}
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Settlement: &Settlement{
			Prompt:          "SETTLE_NOW",
			MaxTurns:        2,
			DisabledTools:   []string{"Bash"},
			KeepToolSchemas: true,
		},
	}
	term := drain(t, in, QueryDeps{CallModel: call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns", term.Reason)
	}
	if len(toolsSeen) < 3 {
		t.Fatalf("calls=%d, want 3", len(toolsSeen))
	}
	if !contains(toolsSeen[1], "Bash") || !contains(toolsSeen[2], "Bash") {
		t.Fatalf("KeepToolSchemas must preserve Bash schema across settlement: %v", toolsSeen)
	}
	if !strings.Contains(blockedMsg, `tool "Bash" is disabled during wrap-up`) {
		t.Fatalf("expected execution block message, got %q", blockedMsg)
	}
}

// With no Settlement, hitting the budget finishes immediately (unchanged behavior).
func TestNoSettlementFinishesImmediately(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"echo 2"}`),
	}}
	in := QueryInput{
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v", term.Reason)
	}
	if m.calls != 1 {
		t.Fatalf("无收尾应只调用 1 次模型，得 %d", m.calls)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

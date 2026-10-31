package mcp

import (
	"context"
	"os/exec"
	"testing"

	"github.com/lin9527ya/Norma/tool"
)

// TestStdioClientAppliesExecHook 钉住一条安全边界：宿主安装的进程钩子（降权 /
// 环境清洗）必须同样作用到 MCP stdio 子进程。
//
// 为什么值得单独测：钩子只装在 tool 包，而 stdio 子进程由本包 exec。漏调用的后果
// 不是"少了一个优化"，而是「宿主以为 agent 已被关进低权限用户」在这里破口——
// 一个能配置 MCP 的 agent 等于能以服务端身份执行任意命令。
func TestStdioClientAppliesExecHook(t *testing.T) {
	var seen *exec.Cmd
	tool.SetExecHook(func(cmd *exec.Cmd) { seen = cmd })
	t.Cleanup(func() { tool.SetExecHook(nil) })

	// 用一个必然启动失败的命令：钩子在 Start 之前调用，所以即使握手失败，
	// 我们也应该已经看到它被调用过。
	_, _ = NewStdioClient(context.Background(), "hook-probe", "/nonexistent-mcp-binary-for-hook-test", nil)
	if seen == nil {
		t.Fatal("MCP stdio 子进程没有经过宿主执行钩子（降权/环境清洗被绕过）")
	}
}

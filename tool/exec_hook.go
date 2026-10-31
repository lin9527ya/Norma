package tool

import (
	"os/exec"
	"sync/atomic"
)

// ExecHook is invoked on every child process this package spawns — Bash (sync and
// background tasks) and interactive shell sessions — right after Dir/Env are set and
// immediately before Start/Run. It may rewrite SysProcAttr, Env and Dir.
//
// Hosts use it to (a) run agent commands as a less-privileged user
// (`SysProcAttr.Credential`) and (b) scrub secrets the daemon inherited out of the
// child environment. Unset (the default) means behaviour is unchanged.
//
// 它是进程级的一次性配置：一个进程 = 一个信任级别，不按工具/会话区分。
type ExecHook func(*exec.Cmd)

var execHook atomic.Pointer[ExecHook]

// SetExecHook installs fn as the process-wide exec hook. Pass nil to remove it.
func SetExecHook(fn ExecHook) {
	if fn == nil {
		execHook.Store(nil)
		return
	}
	execHook.Store(&fn)
}

// ApplyExecHook 是本包内部的 applyExecHook 的导出版本，供**同进程内其它会 spawn
// 子进程的包**复用同一套宿主策略（目前是 mcp 的 stdio 传输）。
//
// 导出它的理由：宿主只安装一次钩子（tool.SetExecHook），如果别处 spawn 时不调用，
// 那条路径就静默地落在策略之外——不是"没配"，而是"配了但这里没生效"，更危险。
// 调用点应当在 cmd 的 Dir/Env 都已设好、Start 之前调用。
func ApplyExecHook(cmd *exec.Cmd) { applyExecHook(cmd) }

// applyExecHook runs the installed hook, if any. Call it as the last step before
// starting the command so the hook sees the final Env/Dir.
func applyExecHook(cmd *exec.Cmd) {
	if h := execHook.Load(); h != nil {
		(*h)(cmd)
	}
}

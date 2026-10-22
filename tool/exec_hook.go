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

// applyExecHook runs the installed hook, if any. Call it as the last step before
// starting the command so the hook sees the final Env/Dir.
func applyExecHook(cmd *exec.Cmd) {
	if h := execHook.Load(); h != nil {
		(*h)(cmd)
	}
}

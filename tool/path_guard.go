package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// PathGuard vets every filesystem path the built-in file tools are about to touch
// (Read/Write/Edit/MultiEdit/LS/Glob/Grep). Returning an error denies the access and
// the message is surfaced to the model as the tool's failure reason.
//
// 它收到的是**解析过软链的真实绝对路径**：否则一个指向禁止路径的软链就能绕过判定。
//
// 这是**进程内策略**，不是 OS 边界——它的用途是挡住"服务端进程读得到、但 agent 不该碰"
// 的路径（例如安装目录下的 config.json / jwt.key）。对"命令去读"的那条路，靠的是
// ExecHook 的降权，两者互补。
type PathGuard func(abs string) error

var pathGuard atomic.Pointer[PathGuard]

// SetPathGuard installs fn as the process-wide path guard. Pass nil to remove it.
func SetPathGuard(fn PathGuard) {
	if fn == nil {
		pathGuard.Store(nil)
		return
	}
	pathGuard.Store(&fn)
}

// pathGuardSet reports whether a guard is installed. Grep uses it to skip the ripgrep
// fast path: rg is a subprocess that reads files itself, so the guard would not see
// which paths it touched.
func pathGuardSet() bool { return pathGuard.Load() != nil }

// guardPath hands the resolved real path to the installed guard.
func guardPath(abs string) error {
	h := pathGuard.Load()
	if h == nil {
		return nil
	}
	real, err := resolveRealPath(abs)
	if err != nil {
		// fail closed：解析不出真实路径就不放行。
		return fmt.Errorf("拒绝访问 %s：无法解析其真实路径（%v）", abs, err)
	}
	return (*h)(real)
}

// resolveRealPath resolves abs to its real path. The target may not exist yet (writing
// a new file), so it resolves the **deepest existing ancestor** with EvalSymlinks and
// re-appends the missing tail. Only "does not exist" keeps walking up; any other error
// fails closed.
func resolveRealPath(abs string) (string, error) {
	tail := ""
	for cur := abs; ; {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if tail == "" {
				return real, nil
			}
			return filepath.Join(real, tail), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

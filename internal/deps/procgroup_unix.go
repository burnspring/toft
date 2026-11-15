//go:build unix

package deps

import (
	"os/exec"
	"syscall"
)

// ownGroup puts a resolver in a process group of its own and makes the
// timeout kill that whole group, not only the direct child: `npm install
// --package-lock-only` spawns git for a git+https dependency, and an orphaned
// git holding the output pipe open kept cmd.Wait — and the build worker —
// blocked long after npm itself was killed.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

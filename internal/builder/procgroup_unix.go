//go:build unix

package builder

import (
	"os/exec"
	"syscall"
)

// ownGroup puts a build step in a process group of its own and makes the
// timeout kill that whole group, not only the direct child (the umask stage
// execs, so the step itself leads the group). A step's children — git under npm, a
// compiler server, a package manager's cache daemon — die with it.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// endGroup kills what a finished step left behind in its group. Nothing a
// build step starts is meant to outlive it: a daemon kept running as the app's
// account would hold the release open and run beside the app it built. ESRCH —
// the group is already empty — is the ordinary case.
func endGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// asAccount makes the clone itself the account's: uid, primary gid and whole
// group list set between fork and exec, so the first program that sees the
// build's environment already runs as the account (runtime.SpawnCommand).
// nil leaves the command as the platform's. Call after ownGroup, which
// replaces SysProcAttr.
func asAccount(cmd *exec.Cmd, c *accountCreds) {
	if c == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	groups := make([]uint32, len(c.groups))
	for i, g := range c.groups {
		groups[i] = uint32(g)
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(c.uid), Gid: uint32(c.gid), Groups: groups}
}

package runner

import (
	"os"

	"golang.org/x/sys/unix"
)

// becomeSubreaper makes orphans of the runner's tree its own to reap. PID 1
// already receives them, and asking there is harmless but meaningless.
func becomeSubreaper() {
	if os.Getpid() == 1 {
		return
	}
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// waitExited blocks until pid has exited, without reaping it: WNOWAIT leaves
// the zombie, and so the exit status, to cmd.Wait. It reports false if it
// could not wait, and the caller falls back to waiting on the app's output.
func waitExited(pid int) bool {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		return err == nil
	}
}

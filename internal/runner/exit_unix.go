//go:build unix

package runner

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ExitAs ends the runner as its app ended, so whatever reaps the runner reads
// what the app did: its exit code, or the signal that killed it (RUN.36). The
// agent reports a signal death as the signal and no code, which it can only do
// if the runner dies of it too.
//
// A Go program cannot die of a signal it sends itself — its runtime catches
// SIGSEGV and the rest and exits 2 — so the runner becomes a shell, in the
// same process, that does. Not as PID 1, where a signal with no handler is
// ignored and the shell would exit 0: there, as in a pod, the 128+n code is
// the answer.
func ExitAs(e AppExit) {
	if e.Signal != 0 && os.Getpid() != 1 {
		name := strings.TrimPrefix(unix.SignalName(e.Signal), "SIG")
		if name != "" {
			_ = syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -" + name + " $$"}, nil)
		}
	}
	os.Exit(e.Code)
}

// signalName is a signal as an operator writes it: "SIGKILL", not "killed".
func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}
	return fmt.Sprintf("signal %d", int(sig))
}

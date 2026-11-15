//go:build linux

package agent

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

// The agent is replaced by re-exec, and only for a protocol change (ADR §134).
//
// Compatibility is ProtocolVersion, never the build: a platform rebuilt with
// the same protocol keeps the agent it finds, so a rebuild restarts nothing. On
// a mismatch the platform asks for POST /reexec, and the agent writes its
// state and execve's its own binary — the one on disk now, which is the new
// one. The pid is kept, so every workload is still its child: the new image
// reads the state, opens each process's pidfd by pid and its FIFOs by path, and
// reaps an exit that happened during the exec with its real status. Nothing
// crosses the exec but the state file, which is why no descriptor needs to.
//
// The route answers whatever protocol the caller speaks — a caller that can no
// longer talk to this agent is exactly the one that needs to replace it.

func (a *Agent) handleReexec(w http.ResponseWriter, r *http.Request) {
	a.saveState()
	w.WriteHeader(http.StatusAccepted)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go a.reexec()
}

func (a *Agent) reexec() {
	// The 202 is flushed; a moment for it to reach the caller before the
	// image it came from is gone.
	time.Sleep(50 * time.Millisecond)
	a.reexecing.Store(true)
	logf("re-executing %s for a new protocol; %d workload(s) carry over", a.cfg.Exe, len(a.list().Workloads))
	if a.ln != nil {
		a.ln.Close()
	}
	args := []string{a.cfg.Exe, Command, "--dir", a.cfg.Dir, "--resume"}
	args = append(args, a.cfg.flags()...)
	err := syscall.Exec(a.cfg.Exe, args, os.Environ())
	// Reached only when the exec failed. The old image is still whole, so it
	// keeps the estate and answers again (Main serves once more); the
	// platform's next call finds a protocol it cannot speak and says so.
	logf("re-executing failed, carrying on as before: %v", err)
	a.reexecFailed <- struct{}{}
}

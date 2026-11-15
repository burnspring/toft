//go:build linux

package agent

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// The agent's own log is one logfmt line per thing that happened to a
// workload — started, exited, restarting, stopped, deleted, adopted, over its
// memory — plus the agent's diagnostics, in the shape slog's text handler
// writes. It is the only record of what the agent did, and the platform
// re-logs every line into its own log (runtime.followAgentLog), so a journal or
// a cloud log group has the agent's side next to the platform's.

// agentLog is where the agent says things. Main points it at the rotating
// file; a test's in-process agent keeps the default, its stderr.
var agentLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// event is one thing that happened to a workload: msg names it, args are its
// fields, always led by the workload's own name.
func event(msg string, args ...any) { agentLog.Info(msg, args...) }

// warn is an event that went wrong without stopping anything.
func warn(msg string, args ...any) { agentLog.Warn(msg, args...) }

// logf is the agent's own diagnostics, as one message.
func logf(format string, args ...any) { agentLog.Info(fmt.Sprintf(format, args...)) }

// workloadAttrs are the fields every workload event leads with.
func workloadAttrs(spec Spec) []any {
	return []any{"workload", spec.Name, "kind", spec.Kind, "app", spec.AppID}
}

// LogMaxBytes is where the agent's log is rotated to LogPath+".1" — the same
// 4 MiB a workload's own log keeps, for the same reason: bounded on a machine
// nobody is watching.
const LogMaxBytes = 4 << 20

// rotatingLog is the agent's log file, rotated by size. After a rotation the
// new file is also duplicated onto stdout and stderr, so a panic — which the
// runtime writes to fd 2 — lands in the current file, not the renamed one.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openRotatingLog(path string) (*rotatingLog, error) {
	r := &rotatingLog{path: path}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingLog) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > LogMaxBytes && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate is called with r.mu held. A failure keeps the current file: a log
// that grows past its bound is better than an agent that stops saying things.
func (r *rotatingLog) rotate() {
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return
	}
	old := r.f
	if err := r.open(); err != nil {
		_ = os.Rename(r.path+".1", r.path)
		return
	}
	old.Close()
	fd := int(r.f.Fd())
	_ = unix.Dup2(fd, 1)
	_ = unix.Dup2(fd, 2)
}

// useLog makes path the agent's log: the event logger and the standard log
// package both write there.
func useLog(path string) error {
	r, err := openRotatingLog(path)
	if err != nil {
		return err
	}
	agentLog = slog.New(slog.NewTextHandler(r, nil))
	log.SetOutput(r)
	return nil
}

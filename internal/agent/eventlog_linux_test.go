//go:build linux

package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureEvents points the agent's event log at a buffer for one test.
func captureEvents(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := agentLog
	agentLog = slog.New(slog.NewTextHandler(buf, nil))
	t.Cleanup(func() { agentLog = prev })
	return buf
}

// Every thing that happens to a workload is one line naming it: a run that
// starts and exits says both, with its code; a service that is stopped and
// deleted says both.
func TestEachWorkloadEventIsALine(t *testing.T) {
	buf := captureEvents(t)
	f := newFixture(t)
	spec := f.spec("code-1", "exit:3")
	spec.Restart, spec.Kind = RestartNever, KindRun
	f.start(spec)
	until(t, 5*time.Second, "the exit", func() bool { return f.status("code-1").State == StateExited })

	f.start(f.spec("svc-1", "serve"))
	if err := f.c.Stop(context.Background(), "svc-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(context.Background(), "svc-1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`msg="workload started" workload=code-1 kind=run`,
		`msg="workload exited" workload=code-1 kind=run`,
		` code=3 `,
		`msg="workload started" workload=svc-1 kind=service`,
		`msg="workload stopped" workload=svc-1`,
		`msg="workload deleted" workload=svc-1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A workload that keeps exiting says each restart it is about to make, and
// when it is held back.
func TestARestartIsSaidWithItsDelay(t *testing.T) {
	buf := captureEvents(t)
	f := newFixture(t)
	f.start(f.spec("loop-1", "exit:7"))
	until(t, 5*time.Second, "two restarts", func() bool { return f.status("loop-1").Restarts >= 2 })
	out := buf.String()
	if !strings.Contains(out, `msg="workload restarting" workload=loop-1`) ||
		!strings.Contains(out, "crash_looping=true") {
		t.Errorf("the restarts were not said:\n%s", out)
	}
}

// The agent's log is bounded: past LogMaxBytes it moves to .1 and a new file
// starts, and nothing written is lost across the move.
func TestTheAgentsLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	r, err := openRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	// The test's own fds 1 and 2 are re-pointed by a rotation; put them back.
	saved1, saved2 := dupFD(t, 1), dupFD(t, 2)
	t.Cleanup(func() { restoreFD(saved1, 1); restoreFD(saved2, 2) })

	line := []byte(strings.Repeat("x", 1023) + "\n")
	n := LogMaxBytes/len(line) + 10
	for i := 0; i < n; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.Stat(path)
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if old.Size() > LogMaxBytes || cur.Size() > LogMaxBytes {
		t.Errorf("sizes %d and %d, want both within %d", old.Size(), cur.Size(), LogMaxBytes)
	}
	if got := (old.Size() + cur.Size()) / int64(len(line)); got != int64(n) {
		t.Errorf("%d lines across the two files, want %d", got, n)
	}
}

// The agent's figures count what it holds and the logs it keeps open: a run
// that has ended holds none.
func TestStatsCountWorkloadsAndOpenLogs(t *testing.T) {
	f := newFixture(t)
	f.start(f.spec("svc-1", "serve"))
	spec := f.spec("run-1", "exit:0")
	spec.Restart, spec.Kind = RestartNever, KindRun
	f.start(spec)
	until(t, 5*time.Second, "the run's end", func() bool { return f.status("run-1").State == StateExited })

	st, err := f.c.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Workloads["service/online"] != 1 || st.Workloads["run/exited"] != 1 {
		t.Errorf("workloads %v (total %d)", st.Workloads, st.Total)
	}
	if st.OpenLogs != 1 || st.EndedLogsOpen != 0 {
		t.Errorf("open logs = %d (%d ended), want the service's alone", st.OpenLogs, st.EndedLogsOpen)
	}
	if st.FDLimit < st.FDs {
		t.Errorf("fd limit %d below the %d open", st.FDLimit, st.FDs)
	}
	if st.PID != os.Getpid() || st.FDs == 0 || st.RSSKB == 0 || st.Goroutines == 0 || st.Protocol != ProtocolVersion {
		t.Errorf("figures %+v", st)
	}
}

func dupFD(t *testing.T, fd int) int {
	t.Helper()
	n, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func restoreFD(saved, fd int) {
	_ = unix.Dup2(saved, fd)
	_ = unix.Close(saved)
}

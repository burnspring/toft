//go:build linux

package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/agent"
)

// Host runs placed by a real agent on a temporary socket, whose runner is a
// shell script (ADR §133).
func newAgentHostJobs(t *testing.T, script string) (*HostJobBackend, WorkloadSpec, string) {
	t.Helper()
	return newAgentHostJobsWith(t, script, os.Getenv("TOFT_TEST_CGROUP_PARENT"))
}

func newAgentHostJobsWith(t *testing.T, script, cgroupParent string) (*HostJobBackend, WorkloadSpec, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "hj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	exe := filepath.Join(root, "fake-toft")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(root, "release")
	os.MkdirAll(release, 0o755)
	dir := filepath.Join(root, "agent")
	a, err := agent.New(agent.Config{Dir: dir, Exe: exe, Args: []string{"runner"}, StopGrace: time.Second,
		WatchInterval: 20 * time.Millisecond, CgroupParent: cgroupParent})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { a.Serve(ctx); close(served) }()
	t.Cleanup(func() { cancel(); <-served; a.Close(true) })

	r := NewAgentRuntime(Isolation{}, LocalResolver{}, nil, filepath.Join(root, "apps"), dir).
		WithLogsDir(filepath.Join(root, "logs"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.client.List(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h, err := r.HostJobs("http://127.0.0.1:1", func(appID, runID, workload string) (string, error) {
		return "token-for-" + runID, nil
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return h, WorkloadSpec{AppID: "app-1", Slug: "demo", Name: "run-1", Release: LocalRelease(release)}, dir
}

// The run's own exit is its status, read from the agent; the runner's last
// words are in the message, and the run is gone from the agent and the disk
// once it has been read.
func TestAnAgentPlacedRunsExitIsItsStatus(t *testing.T) {
	h, spec, _ := newAgentHostJobs(t, `test "$1" = runner && test -s "$TOFT_IDENTITY_FILE" && test "$TOFT_RUNNER_MODE" = job || exit 9
echo "[2026-10-26T12:00:00.000Z platform/stderr] could not reach the platform" >&2
exit 3`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !IsHostHandle(handle) {
		t.Errorf("handle %q lost its host: shape", handle)
	}
	st := waitTerminal(t, h, handle)
	if st.State != JobFailed || st.ExitCode != 3 {
		t.Fatalf("status = %+v, want failed with exit 3 (9 means the runner's inputs were wrong)", st)
	}
	if !strings.Contains(st.Message, "could not reach the platform") {
		t.Errorf("the runner's last words are missing from %q", st.Message)
	}
	if _, err := h.agent.Get(context.Background(), "run-1"); err == nil {
		t.Error("a finished run is still held by the agent")
	}
	if _, err := os.Stat(h.runDir("app-1", "run-1")); !os.IsNotExist(err) {
		t.Error("a finished run's directory is still on disk")
	}
	// Read again, the same answer: the platform may ask twice.
	if again, _ := h.Status(context.Background(), handle); again.State != JobFailed {
		t.Errorf("a second read = %+v", again)
	}
}

func TestAnAgentPlacedRunThatSucceeds(t *testing.T) {
	h, spec, _ := newAgentHostJobs(t, `exit 0`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if st := waitTerminal(t, h, handle); st.State != JobSucceeded {
		t.Fatalf("status = %+v", st)
	}
}

// A run is placed by the agent, so a platform that restarts — a new backend
// over the same agent — still sees it running, and then sees it end.
func TestAnAgentPlacedRunIsWatchedAcrossAPlatformRestart(t *testing.T) {
	h, spec, dir := newAgentHostJobs(t, `sleep 1; exit 4`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// The platform restarts: a new backend, nothing in memory, the same agent.
	r2 := NewAgentRuntime(Isolation{}, LocalResolver{}, nil, h.host.appsDir, dir).WithLogsDir(h.logsDir)
	h2, _ := r2.HostJobs("http://127.0.0.1:1", nil, "")
	if st, err := h2.Status(context.Background(), handle); err != nil || st.State != JobRunning {
		t.Fatalf("after the restart: %+v, %v — an adopted run must be watched", st, err)
	}
	if st := waitTerminal(t, h2, handle); st.State != JobFailed || st.ExitCode != 4 {
		t.Errorf("the adopted run's end: %+v", st)
	}
}

// A cancel reaches the run's whole group through the agent.
func TestAnAgentPlacedRunIsCancelled(t *testing.T) {
	h, spec, _ := newAgentHostJobs(t, `trap 'exit 143' TERM; while :; do sleep 0.1; done`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Cancel(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if st := waitTerminal(t, h, handle); st.State == JobSucceeded || st.State == JobRunning {
		t.Errorf("a cancelled run reads %+v", st)
	}
}

func TestTerminalRunStatusSaysWhatEndedIt(t *testing.T) {
	three, zero := 3, 0
	for _, c := range []struct {
		st    agent.Status
		state JobState
		want  string
	}{
		{agent.Status{ExitCode: &zero}, JobSucceeded, ""},
		{agent.Status{ExitCode: &three}, JobFailed, "exited 3"},
		{agent.Status{ExitSignal: "SIGKILL"}, JobFailed, "ended by SIGKILL"},
		{agent.Status{OOMKilled: true, MemoryLimitMB: 128, PeakMemoryMB: 128}, JobResourceExceeded, "128 MB, its memory limit"},
		{agent.Status{}, JobFailed, "could not read how"},
	} {
		got := terminalRunStatus(c.st, "")
		if got.State != c.state || !strings.Contains(got.Message, c.want) {
			t.Errorf("%+v -> %+v, want %s containing %q", c.st, got, c.state, c.want)
		}
	}
}

// The runner's own last words are the platform-voice lines at the end of the
// run's log, unmarked — never the app's, which the shipper already carried.
func TestRunLogTailKeepsTheRunnersEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if got := runLogTail(path); got != "" {
		t.Errorf("no log should be no tail, got %q", got)
	}
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "[2026-10-26T12:00:00.000Z platform/stderr] line %d\n", i)
		fmt.Fprintf(&b, "[2026-10-26T12:00:00.000Z app/stdout] app %d\n", i)
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	lines := strings.Split(runLogTail(path), "\n")
	if len(lines) != 20 || lines[0] != "line 31" || lines[19] != "line 50" {
		t.Errorf("tail = %q", lines)
	}
}

type recordingSink struct {
	mu    sync.Mutex
	lines []OutputLine
}

func (r *recordingSink) Line(l OutputLine) {
	r.mu.Lock()
	r.lines = append(r.lines, l)
	r.mu.Unlock()
}

// A run's output reaches the platform's stream once — at ingest, after it is
// stored (api.copyRunLines) — and never a second time from the agent's follow.
func TestARunsOutputIsNotCopiedFromTheAgent(t *testing.T) {
	h, spec, _ := newAgentHostJobs(t, `echo "[2026-10-26T12:00:00.000Z app/stdout] a run's own line"; sleep 0.5; exit 0`)
	sink := &recordingSink{}
	h.rt.StartOutput(sink)
	defer h.rt.StopOutput()
	time.Sleep(200 * time.Millisecond) // the follow is open before the run starts
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, h, handle)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, l := range sink.lines {
		if strings.Contains(l.Line, "a run's own line") {
			t.Errorf("a run's line was copied from the agent: %+v", l)
		}
	}
}

// A run that reported for itself is forgotten once its runner exits: the agent
// lets it go and its directory goes, without anybody polling its status —
// which, with the row already finished by the report, nobody would.
func TestAReportedRunIsForgottenOnceItExits(t *testing.T) {
	h, spec, _ := newAgentHostJobs(t, `sleep 0.3; exit 0`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	h.Forget(handle) // the report, which arrives before the runner exits
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := h.agent.Get(context.Background(), "run-1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a run that reported for itself is still held by the agent")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(h.runDir("app-1", "run-1")); !os.IsNotExist(err) {
		t.Error("its directory is still on disk")
	}
}

// End is the app delete's: it returns with the run stopped and gone — the
// agent holds nothing and the log under LOGS_DIR/<app> is removed.
func TestEndReturnsWithTheRunGone(t *testing.T) {
	h, spec, _ := newAgentHostJobs(t, `echo started; trap 'exit 143' TERM; while :; do sleep 0.1; done`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.End(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if _, err := h.agent.Get(context.Background(), "run-1"); err == nil {
		t.Error("the agent still holds the run End returned for")
	}
	if _, err := os.Stat(h.runLogPath("app-1", "run-1")); !os.IsNotExist(err) {
		t.Errorf("the run's log is still on disk: %v", err)
	}
}

// Readers racing over a finished run all get its end. The jobs service polls a
// shared run from two loops and Forget reads it too; the one that took the end
// deleted the run from the agent before keeping the answer, so a second reader
// in between found it gone and recorded an out-of-memory run as lost while
// nothing watched it (BB-007).
func TestConcurrentReadsOfAFinishedRunAgree(t *testing.T) {
	for i := 0; i < 20; i++ {
		h, spec, _ := newAgentHostJobs(t, `exit 3`)
		handle, err := h.Start(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			st, err := h.agent.Get(context.Background(), spec.Name)
			if err == nil && (st.State == agent.StateExited || st.State == agent.StateStopped) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the run never ended: %+v %v", st, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		var wg sync.WaitGroup
		results := make([]JobStatus, 8)
		for j := range results {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				results[j], _ = h.Status(context.Background(), handle)
			}(j)
		}
		wg.Wait()
		for j, st := range results {
			if st.State != JobFailed || st.ExitCode != 3 {
				t.Fatalf("iteration %d reader %d = %+v, want the run's own exit 3", i, j, st)
			}
		}
	}
}

// A host run's calls to the agent go through the shared runtime's recover: when
// nothing answers on the socket, a new agent is started before the call is
// given up on (RT-6). The exe here records that it was asked and fails, which
// is enough to see the restart attempted for Status and for End.
func TestHostJobCallsRestartAnAgentThatDoesNotAnswer(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "asked")
	exe := filepath.Join(root, "fake-toft")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho >> "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "agent") // no socket: nothing answers
	r := NewAgentRuntime(Isolation{}, LocalResolver{}, nil, filepath.Join(root, "apps"), dir)
	if err := r.EnsureAgent(exe, AgentOptions{}); err == nil {
		t.Fatal("the fake agent started")
	}
	_ = os.Remove(marker)
	h, err := r.HostJobs("http://127.0.0.1:1", func(string, string, string) (string, error) { return "t", nil }, "")
	if err != nil {
		t.Fatal(err)
	}
	handle := fmt.Sprintf("%s%d:%d:%s:%s", hostHandlePrefix, 1, 0, "app-1", "run-1")

	if _, err := h.Status(context.Background(), handle); err == nil || !strings.Contains(err.Error(), "starting a new agent") {
		t.Errorf("Status = %v, want the agent restart attempted", err)
	}
	if err := h.End(context.Background(), handle); err == nil || !strings.Contains(err.Error(), "starting a new agent") {
		t.Errorf("End = %v, want the agent restart attempted", err)
	}
	b, _ := os.ReadFile(marker)
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Errorf("the agent was asked to start %d times, want 2", n)
	}
}

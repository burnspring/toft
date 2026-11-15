package runtime

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
)

// What the agent's text handler writes is what the follower reads back:
// level, message and every field, quoting included.
func TestALogfmtLineReadsBackItsFields(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Warn("workload exited",
		"workload", "demo-1", "error", `exec: "x" not found`, "ran", 1500*time.Millisecond)
	pairs, ok := parseLogfmt(strings.TrimSpace(buf.String()))
	if !ok {
		t.Fatalf("not read as logfmt: %q", buf.String())
	}
	got := map[string]string{}
	for _, kv := range pairs {
		got[kv[0]] = kv[1]
	}
	if got["level"] != "WARN" || got["msg"] != "workload exited" || got["workload"] != "demo-1" ||
		got["error"] != `exec: "x" not found` || got["ran"] != "1.5s" {
		t.Errorf("read back %v", got)
	}
	if _, ok := parseLogfmt("panic: runtime error: index out of range"); ok {
		t.Error("a panic line was read as logfmt")
	}
}

// syncBuffer is a bytes.Buffer the follower and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The follower says what the agent writes after it started, not before, keeps
// the agent's level, and carries on across a rotation — the renamed file's
// last lines first, then the new file from its start.
func TestTheAgentsLogIsSaidAgainAcrossARotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	agentOut := func(f *os.File, level slog.Level, msg string, args ...any) {
		slog.New(slog.NewTextHandler(f, nil)).Log(context.Background(), level, msg, args...)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	agentOut(f, slog.LevelInfo, "before the platform started")

	var got syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go followAgentLog(ctx, path, slog.New(slog.NewTextHandler(&got, nil)), 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	agentOut(f, slog.LevelInfo, "workload started", "workload", "demo-1", "pid", 42)
	agentOut(f, slog.LevelWarn, "workload over its memory limit, stopping it", "workload", "demo-1")
	f.WriteString("goroutine 1 [running]:\n")
	time.Sleep(60 * time.Millisecond)
	agentOut(f, slog.LevelInfo, "last line of the old file")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	nf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()
	agentOut(nf, slog.LevelInfo, "first line of the new file")

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(got.String(), "first line of the new file") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	out := got.String()
	for _, want := range []string{
		`level=INFO msg="agent: workload started" source=agent workload=demo-1 pid=42`,
		`level=WARN msg="agent: workload over its memory limit, stopping it" source=agent workload=demo-1`,
		`level=WARN msg="agent: goroutine 1 [running]:" source=agent`,
		`msg="agent: last line of the old file"`,
		`msg="agent: first line of the new file"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "before the platform started") {
		t.Error("a line written before the platform started was said again")
	}
}

//go:build unix

package builder

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A step that leaves a daemon holding its output returns at its own exit, not
// at the daemon's, and the daemon does not outlive it — before, the scan read
// to EOF and the build worker waited out the daemon (or the ten-minute clock).
func TestAStepsDaemonNeitherHangsTheBuildNorOutlivesIt(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "daemon.pid")
	b := &Builder{}
	var logged []string
	start := time.Now()
	err := b.runCommand("d1", "", "sh", []string{"-c",
		"sleep 60 & echo $! > " + pidFile + "; echo built"}, nil,
		func(l string) { logged = append(logged, l) })
	if err != nil {
		t.Fatalf("a step that exited 0 failed: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the step took %s; its daemon held the build", took)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "built") {
		t.Fatalf("the step's output was lost: %q", logged)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the step's daemon %d outlived it", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The timeout kills the step's whole group, so a child holding the output
// cannot keep the worker past the clock.
func TestTheTimeoutKillsTheStepsWholeGroup(t *testing.T) {
	b := &Builder{}
	start := time.Now()
	err := b.runCommandFor(time.Second, "d1", "", "sh", []string{"-c", "sleep 60 & sleep 60"}, nil, func(string) {})
	if err == nil {
		t.Fatal("a step past its clock returned no error")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the timed-out step took %s to end", took)
	}
}

// A step its clock stopped says so in its type, and a step that failed on its
// own does not — the render's "raise the timeout" remedy keys on the
// difference (docs/bugs 2026-10-28 #4).
func TestATimedOutStepIsToldFromAFailedOne(t *testing.T) {
	b := &Builder{}
	err := b.runCommandFor(time.Second, "d1", "", "sh", []string{"-c", "sleep 60"}, nil, func(string) {})
	if !timedOut(err) || !strings.Contains(err.Error(), "stopped after 1s") {
		t.Errorf("a step past its clock = %v, want a timeout", err)
	}
	err = b.runCommandFor(time.Minute, "d1", "", "sh", []string{"-c", "echo boom; exit 1"}, nil, func(string) {})
	if err == nil || timedOut(err) {
		t.Errorf("a step that failed on its own = %v, want a failure that is not a timeout", err)
	}
}

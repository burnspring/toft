//go:build linux

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newTestHostJobs builds a host backend over a real agent whose "platform
// binary" is a shell script, so the lifecycle — start, exit, kill, adopt — is
// exercised for real without a runner or a platform to call home to.
func newTestHostJobs(t *testing.T, script string) (*HostJobBackend, WorkloadSpec) {
	t.Helper()
	h, spec, _ := newAgentHostJobs(t, script)
	return h, spec
}

func waitTerminal(t *testing.T, h *HostJobBackend, handle string) JobStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := h.Status(context.Background(), handle)
		if err != nil {
			t.Fatal(err)
		}
		if st.State.Terminal() {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s never finished", handle)
	return JobStatus{}
}

// The runner's exit is the run's outcome when nothing else reported one, and
// the run's directory — identity file included — goes with it.
func TestHostRunExitIsItsStatus(t *testing.T) {
	h, spec := newTestHostJobs(t, `test "$1" = runner && test -s "$TOFT_IDENTITY_FILE" && test "$TOFT_RUNNER_MODE" = job || exit 9
exit 3`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	st := waitTerminal(t, h, handle)
	if st.State != JobFailed || st.ExitCode != 3 {
		t.Fatalf("status = %+v, want failed with exit 3 (9 means the runner's inputs were wrong)", st)
	}
	dir := filepath.Join(h.host.appsDir, "app-1", "runs", "run-1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the run directory outlived the run: %v", err)
	}
}

// JOB_RUN_DIR moves the whole run — identity, scratch, the release copy —
// off APPS_DIR, which on a network filesystem made a large release minutes of
// queue (trytoft drills #5), and the run's directory still goes with the run.
func TestJobRunDirHoldsTheRunInPlaceOfAppsDir(t *testing.T) {
	h, spec := newTestHostJobs(t, `case "$TOFT_WORK_DIR" in */job-runs/app-1/run-1) ;; *) exit 9 ;; esac
test -s "$TOFT_WORK_DIR/identity" || exit 9
exit 0`)
	h.runRoot = filepath.Join(t.TempDir(), "job-runs")
	want := filepath.Join(h.runRoot, "app-1", "run-1")
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if st := waitTerminal(t, h, handle); st.State != JobSucceeded {
		t.Fatalf("status = %+v, want succeeded (exit 9: the run was not under JOB_RUN_DIR)", st)
	}
	if _, err := os.Stat(filepath.Join(h.host.appsDir, "app-1", "runs")); !os.IsNotExist(err) {
		t.Errorf("a run directory was made under APPS_DIR as well: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("the run directory outlived the run: %v", err)
	}
}

// A JOB_RUN_DIR that already exists is the operator's: /tmp keeps its mode
// rather than becoming 0711 on the first run, and one an app's account could
// not pass through is refused by name rather than failing inside the run.
func TestJobRunDirLeavesAnExistingDirectorysModeAlone(t *testing.T) {
	h, spec := newTestHostJobs(t, "exit 0")
	h.runRoot = filepath.Join(t.TempDir(), "shared-tmp")
	if err := os.Mkdir(h.runRoot, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.runRoot, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, h, handle)
	info, err := os.Stat(h.runRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (os.ModePerm | os.ModeSticky); got != 0o777|os.ModeSticky {
		t.Errorf("JOB_RUN_DIR's mode = %v, want it left 1777", got)
	}

	if err := os.Chmod(h.runRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	spec.Name = "run-2"
	if _, err := h.Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "JOB_RUN_DIR") {
		t.Errorf("Start under an untraversable JOB_RUN_DIR = %v, want a refusal naming it", err)
	}
}

// The runner gets the environment the host backend names and nothing of the
// platform's: a deployed app that could read DATABASE_URL could read every app.
func TestHostRunGetsNoPlatformSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	h, spec := newTestHostJobs(t, `test -z "$DATABASE_URL" || exit 7
test -n "$TOFT_RELEASE_DIR" && test -n "$TOFT_RUNNER_SCRATCH" || exit 8
exit 0`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if st := waitTerminal(t, h, handle); st.State != JobSucceeded {
		t.Fatalf("status = %+v (7: a platform secret leaked; 8: an input was missing)", st)
	}
}

// Past its limit, the run is killed and reported as out of memory — recorded
// before the kill, so the signalled exit cannot make it read as a failure —
// and the peak it reached travels with it. The fake runner stays the parent,
// as the real one does: what is measured is the app under it, not the runner.
func TestHostRunPastItsMemoryLimitIsResourceExceeded(t *testing.T) {
	h, spec := newTestHostJobs(t, `python3 -c 'import time; x = bytearray(40 * 1024 * 1024); time.sleep(30)'`)
	spec.MemoryLimitMB = 10
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	st := waitTerminal(t, h, handle)
	if st.State != JobResourceExceeded || !strings.Contains(st.Message, "10 MB") {
		t.Fatalf("status = %+v, want resource_exceeded naming the limit", st)
	}
	// The watchdog catches a run after it has passed its limit; the kernel
	// stops it at the limit, so its peak is the limit.
	if h.HostMemoryEnforcement() == "cgroup" {
		if st.PeakMemoryMB < 10 {
			t.Errorf("peak = %d MB, want the limit it was held to", st.PeakMemoryMB)
		}
	} else if st.PeakMemoryMB <= 10 {
		t.Errorf("peak = %d MB, want the figure past the limit it was stopped at", st.PeakMemoryMB)
	}
}

// With no limit the run is still measured: a host run with no cgroup of its
// own has no other measurement, and the runner is told not to read the
// platform's cgroup as the run's.
func TestHostRunWithNoLimitStillReportsItsPeak(t *testing.T) {
	// Told the platform measures exactly when the run has no cgroup of its
	// own to read: the runner must not report the platform's as the run's.
	// Read from inside: in a cgroup of the agent's the run reports for
	// itself (exit 8 if it was told otherwise); in none, the platform measures
	// it (exit 7 if it was not told).
	h, spec := newTestHostJobs(t, `case "$(cat /proc/self/cgroup)" in
  *toft-workloads*) [ -z "$TOFT_CONSUMPTION" ] || exit 8 ;;
  *) [ "$TOFT_CONSUMPTION" = host ] || exit 7 ;;
esac
python3 -c 'import time; x = bytearray(30 * 1024 * 1024); time.sleep(0.5)'`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	st := waitTerminal(t, h, handle)
	if st.State != JobSucceeded {
		t.Fatalf("status = %+v, want succeeded (7: not told the platform measures; 8: told so inside a cgroup of its own)", st)
	}
	if st.PeakMemoryMB < 30 || st.PeakMemoryMB > 200 {
		t.Errorf("peak = %d MB, want the app's ~30 MB", st.PeakMemoryMB)
	}
}

// Cancel stops the whole process group, children included.
func TestHostRunCancelStopsTheGroup(t *testing.T) {
	h, spec := newTestHostJobs(t, `sleep 30 & wait`)
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	pid, _, _, _, _ := parseHostHandle(handle)
	time.Sleep(100 * time.Millisecond)
	if err := h.Cancel(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, h, handle)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(-pid, 0); err == nil {
		t.Error("a process in the run's group survived the cancel")
	}
}

// A release fetched into the run's directory is owned with it, in one walk;
// the app's own tree, elsewhere, is still handed over on its own.
func TestInsideDirDecidesWhetherTheReleaseNeedsItsOwnWalk(t *testing.T) {
	run := "/apps/a1/runs/r1"
	for path, want := range map[string]bool{
		"/apps/a1/runs/r1/release":   true,
		"/apps/a1/runs/r1":           true,
		"/apps/a1/releases/d1":       false,
		"/apps/a1/runs/r10/release":  false,
		"/apps/a1/runs/r1/../r2/rel": false,
		"/apps/a1/runs/r1/..release": true,
	} {
		if got := insideDir(path, run); got != want {
			t.Errorf("insideDir(%q) = %v, want %v", path, got, want)
		}
	}
}

// Where the machine lets the platform create cgroups, the kernel holds a run's
// memory limit: the run is stopped at its limit rather than caught by a sample
// after passing it, and reported as out of memory with the figure. Skipped
// where the machine does not — the watchdog test above covers that case.
// TOFT_TEST_CGROUP_PARENT names a delegated cgroup to use.
func TestHostRunIsHeldToItsLimitByTheKernel(t *testing.T) {
	h, spec, _ := newAgentHostJobsWith(t, `exec python3 -c 'x = bytearray(200 * 1024 * 1024)'`,
		os.Getenv("TOFT_TEST_CGROUP_PARENT"))
	if h.HostMemoryEnforcement() != "cgroup" {
		t.Skip("no writable cgroup here")
	}
	spec.MemoryLimitMB = 64
	handle, err := h.Start(context.Background(), spec)
	if err != nil {
		t.Skipf("this machine would not start a process in the run's cgroup: %v", err)
	}
	st := waitTerminal(t, h, handle)
	if st.State != JobResourceExceeded || !strings.Contains(st.Message, "MB") {
		t.Fatalf("status = %+v, want out of memory naming the figure", st)
	}
}

// A Start that fails after the run directory is made — here a storage mapping
// Attach refuses — takes the directory with it: nothing else ever removes a
// run that returned no handle, and a scheduled run that failed this way every
// time left a release copy behind each time (code review 2026-11-11, Q2-2).
func TestAFailedHostStartLeavesNoRunDirectory(t *testing.T) {
	h, spec := newTestHostJobs(t, "exit 0")
	missing := filepath.Join(t.TempDir(), "efs-not-mounted")
	h.host.storage = NewSharedStorage([]StorageRoot{{Name: "data", Path: missing}}, h.host.appsDir, "")
	spec.Mounts = []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume,
		Access: AccessWrite, Root: "data"}}
	if _, err := h.Start(context.Background(), spec); err == nil {
		t.Fatal("a start with an unmountable root succeeded")
	}
	dir := filepath.Join(h.host.appsDir, "app-1", "runs", "run-1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the failed start's run directory was left behind: %v", err)
	}
}

// An app's run parent is the platform's own: a link planted at its name, or a
// file, is refused rather than written through (code review 2026-11-11, Q2-5).
func TestEnsureOwnRunParentRefusesALinkOrAFile(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "app-fresh")
	if err := ensureOwnRunParent(fresh); err != nil {
		t.Fatalf("a parent to make: %v", err)
	}
	if err := ensureOwnRunParent(fresh); err != nil {
		t.Fatalf("the parent this process made: %v", err)
	}
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, filepath.Join(root, "app-linked"))
	if err := ensureOwnRunParent(filepath.Join(root, "app-linked")); err == nil {
		t.Error("a link at the run parent was accepted")
	}
	os.WriteFile(filepath.Join(root, "app-file"), []byte("x"), 0o644)
	if err := ensureOwnRunParent(filepath.Join(root, "app-file")); err == nil {
		t.Error("a file at the run parent was accepted")
	}
}

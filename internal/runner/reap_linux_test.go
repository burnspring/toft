package runner

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// zombieChildren is every zombie this process is the parent of.
func zombieChildren() []int {
	var z []int
	for _, pid := range childPIDs() {
		if isZombie(pid) {
			z = append(z, pid)
		}
	}
	return z
}

// An app that double-forks and abandons the grandchild leaves no zombie: the
// grandchild is reparented to the runner (here as subreaper, as on a host; in a
// pod as PID 1, the same loop) and collected when it exits. And the app's own
// exit status still reaches cmd.Wait — the reaper never takes that pid.
func TestTheRunnerReapsWhatItsAppOrphans(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("the test process is PID 1")
	}
	// The subshell forks a short sleep and exits at once, so the sleep is
	// orphaned; the app itself lives on, and exits 7.
	cmd := exec.Command("/bin/sh", "-c", "( sleep 0.2 & ) ; sleep 1.5; exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := startReaper(cmd.Process.Pid)

	deadline := time.Now().Add(1200 * time.Millisecond)
	sawOrphan := false
	for time.Now().Before(deadline) {
		for _, pid := range childPIDs() {
			if pid != cmd.Process.Pid {
				sawOrphan = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawOrphan {
		t.Fatal("the grandchild was never reparented to this process; subreaper not in effect")
	}
	if z := zombieChildren(); len(z) != 0 {
		t.Errorf("zombies left behind: %v", z)
	}

	err := cmd.Wait()
	stop()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 7 {
		t.Errorf("the app's own exit = %v, want status 7 delivered to cmd.Wait", err)
	}
}

// The kept pid is left alone even as a zombie: it is cmd.Wait's to collect.
func TestTheReaperLeavesTheAppsOwnExitToWait(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // a zombie now
	if n := reapOrphans(cmd.Process.Pid); n != 0 {
		t.Fatalf("reaped %d with the app's pid kept", n)
	}
	if err := cmd.Wait(); err == nil || err.(*exec.ExitError).ExitCode() != 3 {
		t.Errorf("Wait = %v, want exit status 3", err)
	}
}

func TestProcStatReadsPastAParenthesisedName(t *testing.T) {
	ppid, state := procStat(os.Getpid())
	if ppid != os.Getppid() || state == "" {
		t.Errorf("procStat(self) = %d %q, want parent %d", ppid, state, os.Getppid())
	}
}

// startApp starts script under /bin/sh with its output pumped into out, the
// way execApp does, and the reaper running against it.
func startApp(t *testing.T, script string) (cmd *exec.Cmd, pumps *sync.WaitGroup, out *bytes.Buffer, stop func()) {
	t.Helper()
	cmd = exec.Command("/bin/sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	out = &bytes.Buffer{}
	pumps = &sync.WaitGroup{}
	pumps.Add(1)
	go func() { defer pumps.Done(); _, _ = io.Copy(out, stdout) }()
	return cmd, pumps, out, startReaper(cmd.Process.Pid)
}

// pidIn reads the pid a script wrote to file.
func pidIn(t *testing.T, file string) int {
	t.Helper()
	for i := 0; i < 40; i++ {
		if b, err := os.ReadFile(file); err == nil && len(bytes.TrimSpace(b)) > 0 {
			pid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never written", file)
	return 0
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !isZombie(pid)
}

// A crash is noticed while a daemon holds stdout. The daemon setsid's out of
// the app's group with a copy of its stdout; waiting on EOF first, the runner
// would hang here until the daemon died, and nothing reports a hang.
func TestACrashIsReportedWhileADaemonHoldsStdout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "daemon")
	cmd, pumps, _, stop := startApp(t, "setsid sleep 30 & echo $! > "+pidFile+"; exit 3")
	defer stop()
	daemon := pidIn(t, pidFile)

	done := make(chan error, 1)
	go func() { done <- awaitApp(cmd, pumps) }()
	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 3 {
			t.Fatalf("awaitApp = %v, want exit status 3", err)
		}
	case <-time.After(sweepGrace + 3*time.Second):
		_ = syscall.Kill(daemon, syscall.SIGKILL)
		t.Fatal("the runner never returned: it is waiting on the daemon's stdout")
	}
	if alive(daemon) {
		_ = syscall.Kill(daemon, syscall.SIGKILL)
		t.Error("the daemon outlived its app")
	}
}

// The sweep reaches a grandchild that ignores SIGTERM and whose parent dies
// mid-sweep: it arrives a generation late and still gets its SIGKILL.
func TestTheSweepReachesAGrandchildOrphanedMidSweep(t *testing.T) {
	saved := sweepGrace
	sweepGrace = 300 * time.Millisecond
	defer func() { sweepGrace = saved }()

	pidFile := filepath.Join(t.TempDir(), "grandchild")
	cmd, pumps, _, stop := startApp(t,
		`( (trap "" TERM; exec sleep 30) & echo $! > `+pidFile+`; wait ) & exit 0`)
	defer stop()
	grandchild := pidIn(t, pidFile)

	if err := awaitApp(cmd, pumps); err != nil {
		t.Fatalf("awaitApp = %v, want a clean exit", err)
	}
	if alive(grandchild) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Error("the grandchild survived the sweep")
	}
	for _, pid := range childPIDs() {
		if !isZombie(pid) {
			t.Errorf("child %d still running after the sweep", pid)
		}
	}
}

// On a stop the app is signalled and drains first; the sweep follows its exit.
func TestAStopDrainsTheAppBeforeTheSweep(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "daemon")
	cmd, pumps, out, stop := startApp(t, "setsid sleep 30 & echo $! > "+pidFile+
		"; trap 'echo drained; [ -d /proc/$(cat "+pidFile+") ] && echo daemon-still-up; exit 0' TERM;"+
		" while :; do sleep 0.05; done")
	defer stop()
	daemon := pidIn(t, pidFile)

	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := awaitApp(cmd, pumps); err != nil {
		t.Fatalf("awaitApp = %v, want the app's clean exit", err)
	}
	if !strings.Contains(out.String(), "drained") || !strings.Contains(out.String(), "daemon-still-up") {
		t.Errorf("output = %q: the app should drain while its daemon is still up", out.String())
	}
	if alive(daemon) {
		_ = syscall.Kill(daemon, syscall.SIGKILL)
		t.Error("the daemon outlived the stop")
	}
}

//go:build linux

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCPUMaxIsAnAmountOfTimePerPeriod(t *testing.T) {
	for m, want := range map[int]string{1000: "100000 100000", 500: "50000 100000", 2500: "250000 100000", 50: "5000 100000"} {
		if got := cpuMax(m); got != want {
			t.Errorf("cpuMax(%d) = %q, want %q", m, got, want)
		}
	}
}

// cpu.max is written at creation, and a busy loop inside the group is held to
// it — throttled, by the kernel's own count. As root under
// TOFT_TEST_CGROUP_PARENT only: an agent given no parent, as root, rearranges
// the login session's cgroup.
func TestACPULimitIsHeldByTheKernel(t *testing.T) {
	parent := os.Getenv("TOFT_TEST_CGROUP_PARENT")
	if os.Geteuid() != 0 || parent == "" {
		t.Skip("needs root and TOFT_TEST_CGROUP_PARENT")
	}
	cg, err := newCgroups(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !cg.cpu {
		t.Skip("the cpu controller is not delegated under " + parent)
	}
	dir, fd, err := cg.create("cpu-test", 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeCgroup(dir) })
	b, _ := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if got := strings.TrimSpace(string(b)); got != "20000 100000" {
		t.Fatalf("cpu.max = %q, want 20000 100000", got)
	}

	cmd := exec.Command("/bin/sh", "-c", "while :; do :; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	syscall.Close(fd)
	time.Sleep(1500 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	stat := filepath.Join(dir, "cpu.stat")
	usage := eventCount(stat, "usage_usec")
	throttled := eventCount(stat, "nr_throttled")
	if throttled == 0 {
		t.Errorf("a busy loop at a 0.2-core limit was never throttled (cpu.stat %s)", stat)
	}
	// 1.5 s at a fifth of a core is 300 ms; allow the scheduler its slack.
	if usage > 600000 {
		t.Errorf("a busy loop at a 0.2-core limit used %d µs of CPU in 1.5 s", usage)
	}
}

// Without a CPU limit cpu.max is left at the kernel's "max".
func TestNoCPULimitLeavesCPUMaxUnset(t *testing.T) {
	parent := os.Getenv("TOFT_TEST_CGROUP_PARENT")
	if os.Geteuid() != 0 || parent == "" {
		t.Skip("needs root and TOFT_TEST_CGROUP_PARENT")
	}
	cg, err := newCgroups(parent)
	if err != nil || !cg.cpu {
		t.Skip("no delegated cpu controller")
	}
	dir, fd, err := cg.create("cpu-none", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Close(fd)
	t.Cleanup(func() { removeCgroup(dir) })
	b, _ := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if !strings.HasPrefix(string(b), "max ") {
		t.Errorf("cpu.max = %q, want max", b)
	}
}

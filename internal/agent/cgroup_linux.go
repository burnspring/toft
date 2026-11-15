//go:build linux

package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cgroups places each workload that has a memory limit in a cgroup of its own,
// so it is stopped by the kernel at its limit rather than caught by a sample
// after it has gone past it (the watchdog measured runs at 131–207 MB against
// 128 MB). The limit belongs to the cgroup, not to the process watching it, so
// it holds whether or not the agent is still there.
//
// Best effort, and said once when it is not available: a cgroup v2 hierarchy
// the agent may write to is not something every machine grants. Where it is
// missing, the watchdog holds the limit.
type cgroups struct {
	// base is the directory each workload's cgroup is created under:
	// <parent>/toft-workloads.
	base string
	// cpu is whether the cpu controller reached base's children: asked for
	// best effort, and without it a workload's CPU limit is not held — which
	// the agent's listing says (List.CPUHeldBy), so no screen calls it
	// enforced.
	cpu bool
}

const cgroupRoot = "/sys/fs/cgroup"

// parentFor is the cgroup an agent places workloads under when it was told
// none: its own — unless its own is the leaf an earlier agent moved the
// platform into, after a crash, a re-exec or a platform restart that found
// none. Then the leaf's parent is the parent: taking the leaf would nest a
// toft-platform inside a toft-platform at every start, and put new workloads
// in a toft-workloads beside the adopted ones'.
func parentFor(self string) string {
	if filepath.Base(self) == platformLeaf {
		return filepath.Dir(self)
	}
	return self
}

// platformLeaf is where the platform and the agent are moved when the cgroup
// they started in is to hand controllers to the workloads beside them.
const platformLeaf = "toft-platform"

// newCgroups finds or makes the parent cgroup for workloads. parent is
// AGENT_CGROUP_PARENT — a delegated directory the operator named — and empty
// means the agent's own cgroup, which is the platform's, and works where the
// platform runs as root in a container or as a systemd service with
// Delegate=yes.
func newCgroups(parent string) (*cgroups, error) {
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil, errors.New("no cgroup v2 hierarchy at /sys/fs/cgroup")
	}
	if parent == "" {
		self, err := ownCgroup()
		if err != nil {
			return nil, err
		}
		parent = parentFor(self)
	} else if !strings.HasPrefix(parent, cgroupRoot) {
		parent = filepath.Join(cgroupRoot, parent)
	}
	if !hasWord(filepath.Join(parent, "cgroup.controllers"), "memory") {
		return nil, fmt.Errorf("the memory controller is not available in %s", parent)
	}

	// cgroup v2 lets a cgroup hand a controller to its children only while it
	// holds no processes of its own. The agent's cgroup holds the platform and
	// the agent, so their processes move into a leaf beside the workloads
	// first.
	if !hasWord(filepath.Join(parent, "cgroup.subtree_control"), "memory") {
		if err := moveProcsToLeaf(parent, platformLeaf); err != nil {
			return nil, err
		}
	}
	base := filepath.Join(parent, "toft-workloads")
	// Memory is required; pids and cpu are what stop a fork bomb and share the
	// CPU by weight, and a hierarchy that offers neither still holds memory —
	// so they are asked for, and their absence is said rather than refused.
	for _, dir := range []string{parent, base} {
		if dir == base {
			if err := os.MkdirAll(base, 0o755); err != nil {
				return nil, creatingErr(base, err)
			}
		}
		for _, ctl := range []string{"memory", "pids", "cpu"} {
			if hasWord(filepath.Join(dir, "cgroup.subtree_control"), ctl) {
				continue
			}
			err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+"+ctl), 0)
			if err != nil && ctl == "memory" {
				return nil, fmt.Errorf("enabling the memory controller under %s: %w", dir, err)
			}
		}
	}
	c := &cgroups{base: base, cpu: hasWord(filepath.Join(base, "cgroup.subtree_control"), "cpu")}
	if !c.cpu {
		warn("the cpu controller is not delegated here, so CPU limits are not held", "cgroup", base)
	}
	if err := c.trial(); err != nil {
		return nil, err
	}
	return c, nil
}

// trial starts a process inside a cgroup of the tree just made. A subtree the
// agent may create and not enter is common — a user's delegated cgroup reached
// from a session that is not under it, where the move's common ancestor is
// root's — and without this every workload's start would fail at clone.
func (c *cgroups) trial() error {
	dir, fd, err := c.create("trial", 0, 0)
	if err != nil {
		return fmt.Errorf("making a cgroup under %s: %w", c.base, err)
	}
	defer removeCgroup(dir)
	defer syscall.Close(fd)
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("a process may not be started in a cgroup under %s: %v", c.base, err)
	}
	return nil
}

// create makes a workload's cgroup with its memory and CPU limits and returns it
// opened, for SysProcAttr.CgroupFD: the runner starts inside it, so there is no
// instant at which it runs unlimited. The caller closes the fd once the process
// started.
//
// **One cgroup per process, never per workload.** A workload's name outlives
// every process it has, and a cgroup left behind by one that could not be
// removed — something of its tree still in it — would hand the next its
// memory.max when the new limit is none, and its oom_kill count, so the next
// ordinary exit would read as a memory kill.
func (c *cgroups) create(name string, limitMB, cpuMillicores int) (string, int, error) {
	dir := filepath.Join(c.base, fmt.Sprintf("%s.%d", name, time.Now().UnixNano()))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", -1, err
	}
	// memory.max is the spec's limit, and absent at 0 — no cap is a fact,
	// and the group still measures. pids.max stops a fork bomb (nothing in
	// the gallery comes near it: gunicorn with four workers is under fifty
	// threads); cpu.weight is the default share under contention, and
	// cpu.max the spec's CPU limit, an amount of time per period. The three
	// are best effort, as their controllers are: a hierarchy without the cpu
	// controller still holds memory.
	if limitMB > 0 {
		limit := strconv.FormatInt(int64(limitMB)*1024*1024, 10)
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(limit), 0); err != nil {
			_ = os.Remove(dir)
			return "", -1, fmt.Errorf("setting the memory limit: %w", err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "pids.max"), []byte(strconv.Itoa(PidsMax)), 0)
	_ = os.WriteFile(filepath.Join(dir, "cpu.weight"), []byte("100"), 0)
	if cpuMillicores > 0 && c.cpu {
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(cpuMax(cpuMillicores)), 0); err != nil {
			warn("workload's CPU limit was not set", "cgroup", dir, "cpu_limit_millicores", cpuMillicores,
				"error", err.Error())
		}
	}
	// No swap past the limit: a workload that swaps instead of stopping is
	// slower and still over. Not every kernel offers it, which is fine.
	_ = os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0)
	// The tree goes together: the app and the runner that reports it.
	_ = os.WriteFile(filepath.Join(dir, "memory.oom.group"), []byte("1"), 0)
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(dir)
		return "", -1, err
	}
	return dir, fd, nil
}

// cpuMax is cpu.max for a limit in millicores: that many hundredths of a
// millisecond of CPU in every 100 ms period, on any of the machine's cores.
func cpuMax(millicores int) string {
	return strconv.Itoa(millicores*cpuPeriodUsec/1000) + " " + strconv.Itoa(cpuPeriodUsec)
}

// cpuPeriodUsec is cpu.max's period, the kernel's default: 100 ms.
const cpuPeriodUsec = 100000

// oomKilled reports whether the kernel killed anything in a cgroup for its
// memory limit.
func oomKilled(dir string) bool {
	return eventCount(filepath.Join(dir, "memory.events"), "oom_kill") > 0
}

// peakMB is the most memory a cgroup ever held, or 0 when the kernel does not
// record it.
func peakMB(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "memory.peak"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return int((n + 1024*1024 - 1) / (1024 * 1024))
}

// PidsMax is every workload's process ceiling at the cgroup tier: a constant,
// said on the Info card, with no setting until somebody needs one (ADR §128).
const PidsMax = 4096

// cgroupUsage is a cgroup's CPU seconds and current memory, which at the
// cgroup tier are the workload's exact figures — the app and the runner that
// reports them.
func cgroupUsage(dir string) (cpuSeconds float64, memoryBytes int64, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, 0, false
	}
	memoryBytes, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	usec := eventCount(filepath.Join(dir, "cpu.stat"), "usage_usec")
	return float64(usec) / 1e6, memoryBytes, true
}

// removeCgroup removes a finished process's cgroup, killing whatever is still
// in it first. Every caller is past the runner's exit, so anything left is a
// process that left the runner's process group — a daemon that called setsid,
// which the group's SIGTERM and SIGKILL never reached. Left alone it outlives
// a stop or a sleep, keeps the cgroup from going, and can hold the port the
// workload's next start has to bind. The cgroup is the one handle on it below
// the namespace tier; at that tier the PID namespace already took it.
//
// cgroup.kill (Linux 5.14) is atomic against a fork; an older kernel gets
// SIGKILL per pid in cgroup.procs, repeated until the group is empty.
func removeCgroup(dir string) {
	if populated(dir) {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0); err != nil {
			killProcs(dir)
		}
		deadline := time.Now().Add(cgroupDrain)
		for populated(dir) && time.Now().Before(deadline) {
			killProcs(dir)
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		logf("removing the cgroup %s: %v", dir, err)
	}
}

// cgroupDrain is how long removeCgroup waits for a killed cgroup to empty. A
// SIGKILL is not delayable, so this is the kernel tearing processes down, not
// a grace anybody is owed.
const cgroupDrain = 2 * time.Second

// populated reports whether a cgroup or any cgroup under it holds a process.
func populated(dir string) bool {
	return eventCount(filepath.Join(dir, "cgroup.events"), "populated") > 0
}

// killProcs sends SIGKILL to every process cgroup.procs lists.
func killProcs(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return
	}
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(cgroupRoot, rest), nil
		}
	}
	return "", errors.New("this process is not in a cgroup v2 hierarchy")
}

// creatingErr names the directory that could not be made once: a
// *fs.PathError already carries the path, and wrapping it again printed it
// twice in the tier reason an administrator reads on System checks.
func creatingErr(dir string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("creating %s: %w", pe.Path, pe.Err)
	}
	return fmt.Errorf("creating %s: %w", dir, err)
}

// moveProcsToLeaf moves every process in dir into dir/leaf.
func moveProcsToLeaf(dir, leaf string) error {
	target := filepath.Join(dir, leaf)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return creatingErr(target, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(b)) {
		if err := os.WriteFile(filepath.Join(target, "cgroup.procs"), []byte(pid), 0); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("moving process %s into %s: %w", pid, target, err)
		}
	}
	return nil
}

func hasWord(path, word string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, w := range strings.Fields(string(b)) {
		if w == word {
			return true
		}
	}
	return false
}

func eventCount(path, key string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// enterCgroup starts the process inside the cgroup fd names (clone3's
// CLONE_INTO_CGROUP).
func enterCgroup(attr *syscall.SysProcAttr, fd int) {
	attr.UseCgroupFD = true
	attr.CgroupFD = fd
}

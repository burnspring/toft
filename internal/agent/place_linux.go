//go:build linux

package agent

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"toft/internal/procstat"
	"toft/internal/runner"
)

// spawn starts one process of a workload: the runner, as the workload's
// account, in a session of its own, with its output on the workload's FIFOs and
// an identity issued for this process alone.
//
// No shell and no helper. The account is the clone's own credential — uid, gid
// and the whole supplementary list — so nothing between the agent and the
// runner can drop a group the way PM2's own setuid did. The umask the gates
// need is set by the runner, which is the app's parent (Go cannot set a
// child's).
func (a *Agent) spawn(w *workload) (*process, error) {
	spec := w.spec
	// A service's identity is the agent's to issue, per process. A run keeps
	// the platform's own single-use token, already named in its environment:
	// single use is stronger than "while alive" for something that runs once.
	var token, idFile string
	if spec.Kind == KindService {
		var err error
		if token, idFile, err = a.issueIdentity(spec); err != nil {
			return nil, fmt.Errorf("issuing the workload's identity: %w", err)
		}
	}

	stdout, stderr, err := w.out.writers()
	if err != nil {
		if token != "" {
			a.removeIdentityFile(spec.Name, token)
		}
		return nil, fmt.Errorf("opening the workload's output: %w", err)
	}
	defer stdout.Close()
	defer stderr.Close()

	cmd := exec.Command(a.cfg.Exe, a.cfg.Args...)
	// The namespace tier starts the shim in place of the runner, in a
	// network, mount and PID namespace of its own; the shim builds the
	// workload's view of the machine and execs the runner (shim_linux.go).
	var ns *nsStart
	if a.net != nil {
		var err error
		if ns, err = a.prepareNamespaces(spec, idFile); err != nil {
			if token != "" {
				a.removeIdentityFile(spec.Name, token)
			}
			return nil, err
		}
		defer ns.close()
		cmd = exec.Command(a.cfg.Exe, a.cfg.ShimArgs...)
		cmd.ExtraFiles = []*os.File{ns.readyR}
	}
	cmd.Dir = spec.Dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	var extra []string
	if idFile != "" {
		extra = append(extra, runner.EnvIdentityFile+"="+idFile)
	}

	pidfd := -1
	attr := &syscall.SysProcAttr{Setsid: true, PidFD: &pidfd}

	// A cgroup of its own wherever the machine allows it — the cgroup tier
	// (ADR §128) — entered at clone so there is no instant the process is
	// unlimited. A workload whose group cannot be made is started without one
	// and the watchdog holds it; said, and never a refusal to start.
	cgroupDir := ""
	if a.cg != nil {
		dir, fd, err := a.cg.create(spec.Name, spec.MemoryLimitMB, spec.CPULimitMillicores)
		if err != nil {
			warn("workload has no cgroup, the watchdog holds its memory", append(workloadAttrs(spec),
				"error", err.Error())...)
		} else {
			cgroupDir = dir
			attr.UseCgroupFD, attr.CgroupFD = true, fd
			defer syscall.Close(fd)
		}
	}
	// Measurement has one owner (ADR §132). In a cgroup of its own the runner
	// reads it and reports, as in a pod; without one it would read the
	// platform's, so it reports nothing and the platform measures the tree.
	if cgroupDir == "" {
		extra = append(extra, runner.EnvConsumption+"="+runner.ConsumptionByHost)
	}
	if ns != nil {
		extra = append(extra, ns.env)
		attr.Cloneflags = nsFlags
	}
	cmd.Env = envList(spec.Env, extra...)
	if ns == nil && spec.UID >= 0 && (spec.UID != os.Geteuid() || spec.GID != os.Getegid() || len(spec.Groups) > 0) {
		groups := make([]uint32, 0, len(spec.Groups))
		for _, g := range spec.Groups {
			groups = append(groups, uint32(g))
		}
		attr.Credential = &syscall.Credential{Uid: uint32(spec.UID), Gid: uint32(spec.GID), Groups: groups}
	}
	cmd.SysProcAttr = attr

	err = cmd.Start()
	if err != nil && cgroupDir != "" {
		// Refused entry to its own cgroup: started without one, and the
		// watchdog holds it (ADR §128) — a workload is never refused over its
		// cgroup. The tier's trial makes this rare; a parent changed under a
		// running agent is what is left.
		warn("workload refused entry to its cgroup, starting it without one", append(workloadAttrs(spec),
			"error", err.Error())...)
		removeCgroup(cgroupDir)
		cgroupDir = ""
		attr.UseCgroupFD, attr.CgroupFD = false, 0
		cmd = rebuilt(cmd, envList(spec.Env, append(extra, runner.EnvConsumption+"="+runner.ConsumptionByHost)...))
		err = cmd.Start()
	}
	if err != nil {
		if token != "" {
			a.removeIdentityFile(spec.Name, token)
		}
		if cgroupDir != "" {
			removeCgroup(cgroupDir)
		}
		return nil, fmt.Errorf("starting the runner: %w", err)
	}
	pid := cmd.Process.Pid
	// The agent waits on its own pidfd, never through exec.Cmd, so that a
	// re-executed agent can keep waiting on the same descriptor. Release hands
	// back Go's duplicate and nothing else.
	_ = cmd.Process.Release()

	if ns != nil {
		// Plugged in from outside, then told to go: until the line arrives
		// the shim waits, so the runner never starts on a namespace with no
		// network.
		ns.readyR.Close()
		if err := ns.plug(a.net, pid); err != nil {
			// A start that produced no process leaves none: killed, reaped —
			// the agent is its parent, so unwaited it stayed a zombie for the
			// agent's life — and its cgroup removed.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			var info unix.Siginfo
			for {
				if err := unix.Waitid(unix.P_PIDFD, pidfd, &info, unix.WEXITED, nil); err != unix.EINTR {
					break
				}
			}
			_ = unix.Close(pidfd)
			if cgroupDir != "" {
				removeCgroup(cgroupDir)
			}
			if token != "" {
				a.removeIdentityFile(spec.Name, token)
			}
			return nil, fmt.Errorf("plugging the workload into %s: %w", a.net.bridge, err)
		}
	}

	start, _, _ := procStart(pid)
	p := &process{
		pid: pid, pidfd: pidfd, started: time.Now().UTC(), startTicks: start,
		child: true, token: token, cgroup: cgroupDir, exited: make(chan struct{}),
	}
	if token != "" {
		a.rememberToken(token, spec.Name)
	}
	return p, nil
}

// envList renders a description's environment, plus extra, deterministically.
func envList(env map[string]string, extra ...string) []string {
	out := make([]string, 0, len(env)+len(extra))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return append(out, extra...)
}

// appMemory is what the app's own processes hold: the runner's tree, less the
// runner.
func (a *Agent) appMemory(name string, pid int) (int64, bool) {
	stat, err := a.tree.Sample(name, pid)
	if err != nil {
		return 0, false
	}
	used := stat.RSSBytes
	if self, err := procstat.Read(pid); err == nil {
		used -= self.RSSBytes
	}
	return used, true
}

// nsStart is one namespace-tier start in progress: the address, the shim's
// configuration, and the pipe it waits on.
type nsStart struct {
	addr           netip.Addr
	env            string
	readyR, readyW *os.File
	peer           string
}

// alwaysMasked is what no namespace-tier workload sees whatever the platform
// asked for. /var/run/secrets is where Kubernetes puts the platform pod's
// ServiceAccount token: a shared app placed inside that pod has no business
// with a credential against the cluster, and the file's 0600 is only the
// lower tiers' protection.
var alwaysMasked = []string{"/var/run/secrets"}

// namespaceMask is the directories a namespace-tier workload has covered: the
// agent's own, the fixed ones above, and whatever the platform named.
func namespaceMask(agentDir string, requested []string) []string {
	mask := append([]string{agentDir}, alwaysMasked...)
	return append(mask, requested...)
}

func (a *Agent) prepareNamespaces(spec Spec, idFile string) (*nsStart, error) {
	a.mu.Lock()
	addr, err := a.allocate(spec.Name)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	n := a.net
	host := vethName(addr)
	ns := &nsStart{addr: addr, peer: "tp" + host[2:]}

	mask := namespaceMask(a.cfg.Dir, spec.Mask)
	keep := append([]string{spec.Dir}, spec.Keep...)
	if idFile != "" {
		keep = append(keep, filepath.Dir(idFile))
	}
	cfg := shimConfig{
		Address: netip.PrefixFrom(addr, n.prefix.Bits()).String(),
		Gateway: n.gateway.String(), Peer: ns.peer,
		Mask: mask, Keep: keep, ResolvConf: n.resolvConf,
		UID: spec.UID, GID: spec.GID, Groups: spec.Groups,
		Exe: a.cfg.Exe, Args: a.cfg.Args,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	ns.env = envShim + "=" + string(b)
	if ns.readyR, ns.readyW, err = os.Pipe(); err != nil {
		return nil, err
	}
	return ns, nil
}

func (ns *nsStart) plug(n *netConfig, pid int) error {
	if _, err := n.plug(pid, ns.addr); err != nil {
		return err
	}
	_, err := ns.readyW.WriteString("go\n")
	return err
}

func (ns *nsStart) close() {
	ns.readyR.Close()
	ns.readyW.Close()
}

// rebuilt is cmd again with a new environment: an exec.Cmd is single-use.
func rebuilt(cmd *exec.Cmd, env []string) *exec.Cmd {
	n := exec.Command(cmd.Path, cmd.Args[1:]...)
	n.Dir, n.Stdout, n.Stderr, n.ExtraFiles, n.SysProcAttr = cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.ExtraFiles, cmd.SysProcAttr
	n.Env = env
	return n
}

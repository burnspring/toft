//go:build linux

package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"toft/internal/procstat"
)

// workload is one name the agent holds. A Deploy of a name it holds replaces
// the whole of this: a new description is a new workload, with a fresh count.
type workload struct {
	spec    Spec
	created time.Time
	out     *output

	// Guarded by Agent.mu.
	state        string
	detail       string
	restarts     int
	crashes      int // consecutive, for the backoff; reset by a long enough life
	crashLooping bool
	lastExit     exitInfo
	oomKilled    bool
	peakMB       int
	proc         *process

	quit chan struct{} // closed to end supervision: a stop, a delete, a replacement
	done chan struct{} // closed when supervision has ended
	once sync.Once
}

func (w *workload) end() { w.once.Do(func() { close(w.quit) }) }

// process is one start of a workload: the runner the agent placed.
type process struct {
	pid        int
	pidfd      int
	started    time.Time
	startTicks uint64
	// child says the agent is this process's parent and can read its exit
	// status. An adopted process whose parent was a previous agent is not.
	child bool
	token string
	// cgroup is the process's own, when the kernel holds its memory limit.
	cgroup string
	// killedForMemory is set by the memory watchdog before it kills, and
	// peakMB is the most its samples saw.
	killedForMemory atomic.Bool
	peakMB          atomic.Int64
	exited          chan struct{}
}

type exitInfo struct {
	code   *int
	signal string
	at     time.Time
}

// start places a workload under spec's name, replacing whatever held it. The
// first process is started here, synchronously: a clone or exec that fails is
// this call's error, and nothing is recorded as started.
func (a *Agent) start(spec Spec) (Status, error) {
	defer a.holdName(spec.Name)()
	a.replace(spec.Name)

	// replace took the old workload out of the table; whichever way this
	// start ends, the state file has to say so. Returned without a save, the
	// file still listed the old spec as online under a dead pid, and an agent
	// re-exec in that window started the old release again while the platform
	// had been told nothing ran under the name (code review 2026-11-11, Q-3).
	out, err := a.openOutput(spec)
	if err != nil {
		a.saveState()
		return Status{}, fmt.Errorf("opening the workload's output: %w", err)
	}
	w := &workload{
		spec: spec, created: time.Now().UTC(), out: out,
		state: StateLaunching,
		quit:  make(chan struct{}), done: make(chan struct{}),
	}
	p, err := a.spawn(w)
	if err != nil {
		out.close()
		out.removeFIFOs()
		a.removeIdentity(spec.Name)
		a.saveState()
		return Status{}, err
	}
	a.mu.Lock()
	w.proc = p
	w.state = StateOnline
	a.workloads[spec.Name] = w
	a.mu.Unlock()
	a.saveState()
	a.publish(Event{Type: EventStart, Workload: spec.Name, AppID: spec.AppID, Labels: spec.Labels})
	event("workload started", append(workloadAttrs(spec), "pid", p.pid, "restart", 0,
		"cgroup", p.cgroup != "", "memory_limit_mb", spec.MemoryLimitMB)...)
	go a.supervise(w)
	return a.status(spec.Name), nil
}

// holdName takes name's lock and returns its release: one start, stop or
// delete of a name at a time, the second waiting and then acting on what the
// first left.
func (a *Agent) holdName(name string) func() {
	m, _ := a.names.LoadOrStore(name, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// replace ends the workload holding name, if any, and waits for its process to
// be gone — so a replacement never runs beside what it replaces.
func (a *Agent) replace(name string) {
	a.mu.Lock()
	old := a.workloads[name]
	delete(a.workloads, name)
	a.mu.Unlock()
	if old == nil {
		return
	}
	a.terminate(old)
	old.out.close()
}

// terminate ends supervision and the current process, and waits for both.
func (a *Agent) terminate(w *workload) {
	w.end()
	a.mu.Lock()
	p := w.proc
	a.mu.Unlock()
	if p != nil {
		a.kill(p)
	}
	<-w.done
}

// kill asks the process group to stop, and makes it after the grace. What
// left the group — a daemon that called setsid — is reached through the tree:
// read before the signal, while the runner still holds it, and again before the
// SIGKILL, and each one still alive once the runner has gone is killed. The
// runner ends its own tree when its app exits; this is the case it cannot, a
// runner killed before it got there. At the cgroup tier removeCgroup does the
// same from the cgroup, and at the namespace tier the PID namespace does.
func (a *Agent) kill(p *process) {
	strays := treeOf(p.pid)
	_ = syscall.Kill(-p.pid, syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(a.cfg.StopGrace):
		strays = append(strays, treeOf(p.pid)...)
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.exited
	}
	killStrays(strays)
}

// proc is a process told apart from a later one given the same pid.
type proc struct {
	pid   int
	ticks uint64
}

// treeOf is every process under pid, by parent.
func treeOf(pid int) []proc {
	var out []proc
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range procstat.Children(next) {
			if seen[c] {
				continue
			}
			seen[c] = true
			queue = append(queue, c)
			if ticks, _, err := procStart(c); err == nil {
				out = append(out, proc{c, ticks})
			}
		}
	}
	return out
}

// killStrays kills each process of a tree read earlier that is still the
// process it was.
func killStrays(strays []proc) {
	for _, s := range strays {
		if alive(s.pid, s.ticks) {
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
	}
}

// stop halts a workload and keeps it: the name stays held, listed as stopped,
// until a start replaces it or a delete removes it.
func (a *Agent) stop(name string, forget bool) bool {
	defer a.holdName(name)()
	a.mu.Lock()
	w := a.workloads[name]
	a.mu.Unlock()
	if w == nil {
		return false
	}
	a.terminate(w)
	a.mu.Lock()
	w.state = StateStopped
	w.crashLooping = false
	if forget {
		delete(a.workloads, name)
	}
	a.mu.Unlock()
	a.saveState()
	event("workload stopped", workloadAttrs(w.spec)...)
	return true
}

// delete removes a workload, its identity and its output.
func (a *Agent) delete(name string) bool {
	defer a.holdName(name)()
	a.mu.Lock()
	w := a.workloads[name]
	delete(a.workloads, name)
	a.mu.Unlock()
	if w == nil {
		return false
	}
	a.terminate(w)
	w.out.close()
	w.out.removeFIFOs()
	w.out.removeFiles()
	a.removeIdentity(name)
	a.mu.Lock()
	delete(a.addrs, name)
	a.mu.Unlock()
	a.saveState()
	event("workload deleted", workloadAttrs(w.spec)...)
	return true
}

// supervise waits on the workload's process and restarts it under the policy,
// until something ends supervision. **It never gives up** (ADR §138): a loop is
// reported, as CrashLooping and by the health watcher's app_flapping, and the
// one a restart cannot fix — a memory kill — is ended by the watcher's stop.
func (a *Agent) supervise(w *workload) {
	defer close(w.done)
	for {
		a.mu.Lock()
		p := w.proc
		a.mu.Unlock()

		if p != nil {
			if p.cgroup == "" {
				go a.watchMemory(w, p)
			}
			exit := a.wait(p)
			a.exited(w, p, exit)
		}

		select {
		case <-w.quit:
			return
		default:
		}
		if w.spec.Restart == RestartNever {
			// Its output closes with it: the file a run appended to is the
			// platform's to read and remove, and an agent holding it open until
			// somebody deleted the run kept a descriptor per finished run — and
			// on NFS kept the file, as .nfs*, in a directory an app delete then
			// could not remove. Drained first, so the last lines are in the
			// file before the state says the run has ended.
			w.out.drain(2 * time.Second)
			w.out.close()
			a.mu.Lock()
			w.state = StateExited
			a.mu.Unlock()
			a.saveState()
			return
		}

		a.mu.Lock()
		delay := a.cfg.Backoff.delay(w.crashes)
		w.crashes++
		w.crashLooping = delay > 0
		w.state = StateErrored
		attempt := w.restarts + 1
		a.mu.Unlock()
		a.saveState()
		event("workload restarting", append(workloadAttrs(w.spec), "attempt", attempt,
			"in", delay, "crash_looping", delay > 0)...)

		if delay > 0 {
			select {
			case <-w.quit:
				return
			case <-time.After(delay):
			}
		}
		select {
		case <-w.quit:
			return
		default:
		}

		np, err := a.spawn(w)
		a.mu.Lock()
		w.restarts++
		if err != nil {
			// A start that could not happen is a crash of zero length, and is
			// backed off like one.
			w.proc = nil
			w.detail = err.Error()
			w.lastExit = exitInfo{at: time.Now().UTC()}
			a.mu.Unlock()
			warn("workload did not restart", append(workloadAttrs(w.spec), "attempt", w.restarts,
				"error", err.Error())...)
			a.saveState()
			continue
		}
		w.proc = np
		w.detail = ""
		w.state = StateOnline
		a.mu.Unlock()
		// Ended while this start was under way: terminate read no process
		// and is waiting for this loop, so the new one is stopped here and
		// the wait at the top of the loop sees it go.
		select {
		case <-w.quit:
			go a.kill(np)
		default:
		}
		a.saveState()
		a.publish(Event{Type: EventStart, Workload: w.spec.Name, AppID: w.spec.AppID, Labels: w.spec.Labels})
		event("workload started", append(workloadAttrs(w.spec), "pid", np.pid, "restart", w.restarts,
			"cgroup", np.cgroup != "", "memory_limit_mb", w.spec.MemoryLimitMB)...)
	}
}

// exited records how a process ended.
func (a *Agent) exited(w *workload, p *process, exit exitInfo) {
	if p.token != "" {
		a.forgetToken(p.token)
		a.removeIdentityFile(w.spec.Name, p.token)
	}
	oom, peak := p.killedForMemory.Load(), int(p.peakMB.Load())
	if p.cgroup != "" {
		// The kernel held the limit, and kept the figures.
		peak = peakMB(p.cgroup)
		oom = oomKilled(p.cgroup)
		removeCgroup(p.cgroup)
	}
	a.mu.Lock()
	w.lastExit = exit
	w.oomKilled, w.peakMB = oom, peak
	if exit.at.Sub(p.started) >= a.cfg.Backoff.Reset {
		w.crashes = 0
	}
	if w.proc == p {
		w.proc = nil
	}
	a.mu.Unlock()
	a.tree.Forget(w.spec.Name)
	a.publish(Event{Type: EventExit, Workload: w.spec.Name, AppID: w.spec.AppID, Labels: w.spec.Labels,
		ExitCode: exit.code, ExitSignal: exit.signal})
	attrs := append(workloadAttrs(w.spec), "pid", p.pid)
	switch {
	case exit.code != nil:
		attrs = append(attrs, "code", *exit.code)
	case exit.signal != "":
		attrs = append(attrs, "signal", exit.signal)
	default:
		// An adopted process: watched, not reaped, so its end carries nothing.
		attrs = append(attrs, "code", "unknown")
	}
	attrs = append(attrs, "ran", exit.at.Sub(p.started).Round(time.Millisecond),
		"oom_killed", oom, "peak_mb", peak)
	event("workload exited", attrs...)
}

// wait blocks until the process has ended, and says how when it can: a child
// is reaped through its pidfd with its status, an adopted process is only
// watched, and its end carries no code.
func (a *Agent) wait(p *process) exitInfo {
	defer close(p.exited)
	defer unix.Close(p.pidfd)
	if p.child {
		for {
			var info unix.Siginfo
			err := unix.Waitid(unix.P_PIDFD, p.pidfd, &info, unix.WEXITED, nil)
			if err == unix.EINTR {
				continue
			}
			if err == nil {
				return decodeExit(&info)
			}
			// ECHILD: not ours after all. Fall through to watching.
			break
		}
	}
	for {
		fds := []unix.PollFd{{Fd: int32(p.pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, -1)
		if err == unix.EINTR || (err == nil && n == 0) {
			continue
		}
		return exitInfo{at: time.Now().UTC()}
	}
}

// watchMemory measures a process with no cgroup of its own, and holds it to
// its limit by sampling: the runner's tree, minus the runner, which is the
// platform's binary and no part of what the app asked for (the host
// watchdog's rule). It keeps the peak whether or not there is a limit — it is
// the only measurement such a process has — and records why before it kills,
// so the end is reported as what it was.
func (a *Agent) watchMemory(w *workload, p *process) {
	limit := int64(w.spec.MemoryLimitMB) * 1024 * 1024
	interval := a.cfg.WatchInterval
	if w.spec.Kind == KindService {
		interval *= 10
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-p.exited:
			return
		case <-t.C:
		}
		used, ok := a.appMemory(w.spec.Name, p.pid)
		if !ok {
			continue
		}
		if mb := (used + 1<<20 - 1) >> 20; mb > p.peakMB.Load() {
			p.peakMB.Store(mb)
		}
		if limit <= 0 || used <= limit {
			continue
		}
		p.killedForMemory.Store(true)
		warn("workload over its memory limit, stopping it", append(workloadAttrs(w.spec),
			"used_mb", used>>20, "limit_mb", w.spec.MemoryLimitMB)...)
		strays := treeOf(p.pid)
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		killStrays(strays)
		return
	}
}

// list is every workload, by name.
func (a *Agent) list() List {
	a.mu.Lock()
	names := make([]string, 0, len(a.workloads))
	for n := range a.workloads {
		names = append(names, n)
	}
	a.mu.Unlock()
	sort.Strings(names)
	out := List{Protocol: ProtocolVersion, Tier: a.tier(), TierReason: a.tierReason(), MemoryHeldBy: "watchdog"}
	out.CPUHeldBy = "none"
	if a.cg != nil {
		out.MemoryHeldBy = "cgroup"
		if a.cg.cpu {
			out.CPUHeldBy = "cgroup"
		}
	}
	if a.net != nil {
		out.Bridge, out.Gateway, out.DNS = a.net.prefix.String(), a.net.gateway.String(), a.net.dns
	}
	for _, n := range names {
		if st := a.status(n); st.Name != "" {
			out.Workloads = append(out.Workloads, st)
		}
	}
	return out
}

// tier is this agent's containment, fixed at its start.
func (a *Agent) tier() string {
	switch {
	case a.net != nil:
		return TierNamespace
	case a.cg != nil:
		return TierCgroup
	}
	return TierProcess
}

// tierReason is why the next tier up was refused.
func (a *Agent) tierReason() string {
	switch {
	case a.cg == nil:
		return "no cgroup v2 subtree this agent may write: " + a.cgReason
	case a.net == nil:
		return a.nsReason
	}
	return ""
}

// status is one workload's report, or the zero Status for a name not held.
func (a *Agent) status(name string) Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.workloads[name]
	if w == nil {
		return Status{}
	}
	st := Status{
		Name: w.spec.Name, AppID: w.spec.AppID, Kind: w.spec.Kind, Labels: w.spec.Labels,
		State: w.state, CreatedAt: w.created, Restarts: w.restarts,
		ExitCode: w.lastExit.code, ExitSignal: w.lastExit.signal,
		OOMKilled: w.oomKilled, MemoryLimitMB: w.spec.MemoryLimitMB, Detail: w.detail,
		PeakMemoryMB: w.peakMB, ExitPeakMemoryMB: w.peakMB,
	}
	if addr, ok := a.addrs[name]; ok {
		st.Address = addr.String()
	}
	if p := w.proc; p != nil {
		st.PID, st.StartedAt = p.pid, p.started
		if p.cgroup != "" {
			st.PeakMemoryMB = peakMB(p.cgroup)
			st.Cgroup = p.cgroup
			if cpu, mem, ok := cgroupUsage(p.cgroup); ok {
				st.CPUSeconds, st.MemoryBytes = &cpu, &mem
			}
		} else {
			st.PeakMemoryMB = int(p.peakMB.Load())
		}
		// A loop that has stopped looping: up long enough to have reset.
		st.CrashLooping = w.crashLooping && time.Since(p.started) < a.cfg.Backoff.Reset
	} else {
		st.CrashLooping = w.crashLooping
	}
	return st
}

// si_code values for SIGCHLD (asm-generic/siginfo.h), which x/sys does not name.
const (
	cldExited = 1
	cldKilled = 2
	cldDumped = 3
)

// decodeExit reads a CLD_* siginfo. x/sys hides the union, so the status is
// read at its offset: si_pid, si_uid, then si_status, after the 16-byte head.
func decodeExit(info *unix.Siginfo) exitInfo {
	raw := (*[128]byte)(unsafe.Pointer(info))
	status := int32(raw[24]) | int32(raw[25])<<8 | int32(raw[26])<<16 | int32(raw[27])<<24
	e := exitInfo{at: time.Now().UTC()}
	switch info.Code {
	case cldExited:
		code := int(status)
		e.code = &code
	case cldKilled, cldDumped:
		e.signal = unix.SignalName(syscall.Signal(status))
		if e.signal == "" {
			e.signal = fmt.Sprintf("signal %d", status)
		}
	}
	return e
}

// alive reports whether pid is the process that started at startTicks.
func alive(pid int, startTicks uint64) bool {
	got, state, err := procStart(pid)
	return err == nil && got == startTicks && state != 'Z'
}

// reapEnded collects the status of pid when it is the process that started at
// startTicks, has ended, and is this process's child — a workload that exited
// while a re-exec was under way, which the new image is still the parent of.
func reapEnded(pid int, startTicks uint64) (exitInfo, bool) {
	got, state, err := procStart(pid)
	if err != nil || got != startTicks || state != 'Z' || !isChild(pid) {
		return exitInfo{}, false
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG, nil); err != nil || info.Signo == 0 {
		return exitInfo{}, false
	}
	return decodeExit(&info), true
}

// procStart is a process's start time, in clock ticks since boot, and its state
// letter — the pair that tells a process from a later one given its pid.
func procStart(pid int) (uint64, byte, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("unreadable stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 || f[0] == "" {
		return 0, 0, fmt.Errorf("short stat")
	}
	var start uint64
	if _, err := fmt.Sscan(f[19], &start); err != nil {
		return 0, 0, err
	}
	return start, f[0][0], nil
}

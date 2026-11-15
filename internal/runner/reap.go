package runner

// The runner reaps what its app orphans (ADR §136).
//
// In a pod the runner is PID 1 — docker/runner/Dockerfile's ENTRYPOINT has no
// init in front of it — so every process an app double-forks and abandons is
// reparented to the runner when its parent exits. Nothing here waited on them:
// a gunicorn that respawns workers, or a Shiny app shelling out, left one zombie
// per exit until the pid table filled. Off PID 1 (on a host) the runner asks to
// be the *subreaper* of its tree, so the same orphans come to it instead of to
// an init that may be the platform, which never reaps either.
//
// The one child it must never take is the app itself: cmd.Wait owns that pid,
// and a reaper that collected it would turn the app's exit status into ECHILD.
// So the loop reaps zombies by pid, every child of the runner except the app's,
// and never calls wait(-1). That is safe because execApp's process is the only
// child the runner starts while the reaper runs — the build steps are finished
// before it begins.

import (
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// startReaper collects every zombie child of this process except keep, on
// each SIGCHLD and on a slow tick (a signal coalesces, so a burst of exits can
// arrive as one). The returned stop ends it, after one last sweep.
func startReaper(keep int) (stop func()) {
	becomeSubreaper()
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGCHLD)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			reapOrphans(keep)
			select {
			case <-sigs:
			case <-tick.C:
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
		<-finished
		reapOrphans(keep)
	}
}

// reapOrphans waits on each child of this process that is a zombie, other than
// keep. It returns how many it collected.
func reapOrphans(keep int) int {
	n := 0
	for _, pid := range childPIDs() {
		if pid == keep || !isZombie(pid) {
			continue
		}
		var ws syscall.WaitStatus
		if got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); err == nil && got == pid {
			n++
		}
	}
	return n
}

// childPIDs is every direct child of this process: from the kernel's own
// per-thread children lists where it keeps them, otherwise by reading every
// process's parent out of /proc — slower, and only a host without
// CONFIG_PROC_CHILDREN pays it.
func childPIDs() []int {
	self := os.Getpid()
	var pids []int
	listed := false
	tasks, _ := filepath.Glob("/proc/self/task/*/children")
	for _, f := range tasks {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		listed = true
		for _, field := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(field); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	if listed {
		return pids
	}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, _ := procStat(pid); ppid == self {
			pids = append(pids, pid)
		}
	}
	return pids
}

func isZombie(pid int) bool {
	_, state := procStat(pid)
	return state == "Z"
}

// procStat is a process's parent and state from /proc/<pid>/stat. The command
// name is parenthesised and may hold spaces or parentheses itself, so the
// fields are read after the last ')'.
func procStat(pid int) (ppid int, state string) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, ""
	}
	ppid, _ = strconv.Atoi(f[1])
	return ppid, f[0]
}

// sweepGrace is how long a leftover gets between SIGTERM and SIGKILL: half
// the agent's StopGrace, so the sweep finishes inside the agent's window
// rather than being cut short by its SIGKILL of the runner.
var sweepGrace = 5 * time.Second

// killChildren ends every live child of this process other than keep — the
// app, which has exited by now and whose zombie is cmd.Wait's. A workload is
// its process tree: what the app left running (a daemon that setsid'd out of
// its group, a worker it never waited for) is reparented here, because the
// runner is its tree's subreaper, so a list of direct children is the whole of
// what is left. Each gets SIGTERM when first seen and SIGKILL grace later, and
// the list is read again until it is empty, because a dying grandchild's own
// children arrive one generation at a time. It gives up grace after the first
// SIGKILL round; by then the agent has killed the runner anyway.
func killChildren(keep int, grace time.Duration) {
	termed := map[int]time.Time{}
	var giveUp time.Time
	for {
		live := 0
		for _, pid := range childPIDs() {
			if pid == keep || isZombie(pid) {
				continue
			}
			live++
			at, ok := termed[pid]
			switch {
			case !ok:
				termed[pid] = time.Now()
				_ = syscall.Kill(pid, syscall.SIGTERM)
			case time.Since(at) >= grace:
				_ = syscall.Kill(pid, syscall.SIGKILL)
				if giveUp.IsZero() {
					giveUp = time.Now().Add(grace)
				}
			}
		}
		if live == 0 || (!giveUp.IsZero() && time.Now().After(giveUp)) {
			return
		}
		reapOrphans(keep)
		time.Sleep(50 * time.Millisecond)
	}
}

//go:build linux

package agent

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The agent's state: every workload it holds, with the process behind each.
//
// Written on every change, so an agent that dies — or is replaced — leaves
// behind exactly what the next one needs to adopt the estate rather than
// restart it. In /run, so a reboot, after which every pid in it is somebody
// else's, empties it.

type stateFile struct {
	Protocol  string          `json:"protocol"`
	Workloads []stateWorkload `json:"workloads"`
	// Bridge and Addresses are the namespace tier's: the range, picked once and
	// kept, and the address each workload holds in it.
	Bridge    string            `json:"bridge,omitempty"`
	Addresses map[string]string `json:"addresses,omitempty"`
}

type stateWorkload struct {
	Spec     Spec      `json:"spec"`
	Created  time.Time `json:"created"`
	State    string    `json:"state"`
	Restarts int       `json:"restarts"`
	Crashes  int       `json:"crashes"`
	// PID and StartTicks name the running process, or are zero.
	PID        int       `json:"pid,omitempty"`
	StartTicks uint64    `json:"start_ticks,omitempty"`
	Started    time.Time `json:"started,omitzero"`
	Cgroup     string    `json:"cgroup,omitempty"`
}

func (a *Agent) statePath() string { return filepath.Join(a.cfg.Dir, "state.json") }

// saveState writes the table, atomically. A failure is said and survived: the
// state is what a successor adopts from, and a workload must never stop
// because it could not be written down.
func (a *Agent) saveState() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	a.mu.Lock()
	st := stateFile{Protocol: ProtocolVersion, Addresses: a.sortedAddrs()}
	if a.net != nil {
		st.Bridge = a.net.prefix.String()
	}
	for _, w := range a.workloads {
		sw := stateWorkload{
			Spec: w.spec, Created: w.created, State: w.state,
			Restarts: w.restarts, Crashes: w.crashes,
		}
		if p := w.proc; p != nil {
			sw.PID, sw.StartTicks, sw.Started, sw.Cgroup = p.pid, p.startTicks, p.started, p.cgroup
		}
		st.Workloads = append(st.Workloads, sw)
	}
	a.mu.Unlock()

	data, err := json.Marshal(st)
	if err != nil {
		logf("writing the state: %v", err)
		return
	}
	tmp := a.statePath() + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logf("writing the state: %v", err)
		return
	}
	if err := os.Rename(tmp, a.statePath()); err != nil {
		logf("writing the state: %v", err)
	}
}

// readStateNetwork reads the namespace tier's half of the state before the
// tier is detected: the range comes first, because a workload adopted across
// an agent restart keeps the address it has in it.
func (a *Agent) readStateNetwork() {
	data, err := os.ReadFile(a.statePath())
	if err != nil {
		return
	}
	var st stateFile
	if json.Unmarshal(data, &st) != nil {
		return
	}
	a.stateBridge = st.Bridge
	for name, v := range st.Addresses {
		if addr, err := netip.ParseAddr(v); err == nil {
			a.addrs[name] = addr
		}
	}
}

// restore rebuilds the table from the state a previous agent left. A process
// still alive — the same pid, started at the same tick — is adopted, and keeps
// its identity; a service whose process is gone is started again, as its
// policy would have; a stopped one stays stopped.
func (a *Agent) restore() {
	data, err := os.ReadFile(a.statePath())
	if err != nil {
		return
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		logf("the state left behind is unreadable, starting empty: %v", err)
		return
	}
	adopted, restarted := 0, 0
	for _, sw := range st.Workloads {
		out, err := a.openOutput(sw.Spec)
		if err != nil {
			// Not adopted — there is no log to pump into — but not left
			// running either: a live process this table forgets holds its
			// port unsupervised, the platform's Stop answers "no such
			// workload", and the next Deploy starts a second copy beside it
			// (code review 2026-11-11, Q-2). Ended, so its restart starts
			// clean when the log directory is back.
			warn("workload's output could not be reopened", append(workloadAttrs(sw.Spec),
				"error", err.Error())...)
			if sw.PID > 0 && alive(sw.PID, sw.StartTicks) {
				warn("workload ended: it cannot be supervised without its output", append(workloadAttrs(sw.Spec), "pid", sw.PID)...)
				_ = syscall.Kill(-sw.PID, syscall.SIGKILL)
			}
			if sw.Cgroup != "" {
				removeCgroup(sw.Cgroup)
			}
			continue
		}
		w := &workload{
			spec: sw.Spec, created: sw.Created, out: out,
			state: sw.State, restarts: sw.Restarts, crashes: sw.Crashes,
			quit: make(chan struct{}), done: make(chan struct{}),
		}
		// A child that ended while the exec was under way is a zombie of this
		// image's, with its real status: collected here, so it is neither
		// left a zombie for the agent's life nor reported as having no code.
		if sw.PID > 0 {
			if exit, ok := reapEnded(sw.PID, sw.StartTicks); ok {
				w.lastExit = exit
				event("workload exited", append(workloadAttrs(sw.Spec), "pid", sw.PID, "during", "re-exec")...)
			}
		}
		fd := -1
		if sw.PID > 0 && alive(sw.PID, sw.StartTicks) {
			var err error
			if fd, err = unix.PidfdOpen(sw.PID, 0); err != nil {
				// Not adopted, and not left listed online with no process
				// and no supervision: taken for gone, as below. One still
				// running — a descriptor limit, not an exit — is ended
				// first, so its restart does not run beside it.
				warn("workload could not be adopted", append(workloadAttrs(sw.Spec), "pid", sw.PID,
					"error", err.Error())...)
				if alive(sw.PID, sw.StartTicks) {
					_ = syscall.Kill(-sw.PID, syscall.SIGKILL)
				}
				fd = -1
			}
		}
		switch {
		case fd >= 0:
			p := &process{
				pid: sw.PID, pidfd: fd, started: sw.Started, startTicks: sw.StartTicks,
				child: isChild(sw.PID), cgroup: sw.Cgroup,
				exited: make(chan struct{}),
			}
			if sw.Spec.Kind == KindService {
				if p.token = a.readIdentity(sw.Spec.Name); p.token != "" {
					a.tokens[digest(p.token)] = sw.Spec.Name
				}
			}
			w.proc, w.state = p, StateOnline
			adopted++
			event("workload adopted", append(workloadAttrs(sw.Spec), "pid", sw.PID,
				"child", p.child, "cgroup", p.cgroup != "")...)
		case w.state == StateStopped || w.state == StateExited:
			// Wanted down, or a run that ended: held as it was.
		case sw.Spec.Restart == RestartAlways:
			w.state = StateErrored
			if w.lastExit.at.IsZero() {
				w.lastExit = exitInfo{at: time.Now().UTC()}
			}
			restarted++
			event("workload gone while no agent watched, starting it again", workloadAttrs(sw.Spec)...)
		default:
			w.state = StateExited
		}
		if w.proc == nil && sw.Cgroup != "" {
			// The process it was made for is gone: one cgroup per process.
			removeCgroup(sw.Cgroup)
		}
		if w.proc == nil && w.spec.Restart == RestartNever {
			// A run that has ended keeps no descriptor on its log, as when it
			// ended under this agent (supervise).
			out.close()
		}
		a.workloads[sw.Spec.Name] = w
		if w.proc != nil || w.state == StateErrored {
			go a.supervise(w)
		} else {
			close(w.done)
		}
	}
	if adopted+restarted > 0 {
		logf("adopted %d running workload(s); %d whose process was gone are being started again", adopted, restarted)
	}
	a.saveState()
}

// isChild reports whether pid's parent is this process: true after a re-exec,
// false after a predecessor that died.
func isChild(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	text := string(raw)
	i := strings.LastIndexByte(text, ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(text[i+1:])
	if len(f) < 2 {
		return false
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid == os.Getpid()
}

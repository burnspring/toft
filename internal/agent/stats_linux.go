//go:build linux

package agent

import (
	"bufio"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func (a *Agent) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.stats())
}

func (a *Agent) stats() Stats {
	s := Stats{PID: os.Getpid(), StartedAt: a.startedAt, Protocol: ProtocolVersion,
		Goroutines: runtime.NumGoroutine(), Workloads: map[string]int{}}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		s.FDs = len(fds)
	}
	var lim unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &lim) == nil {
		s.FDLimit = int(min(lim.Cur, 1<<31-1))
	}
	s.RSSKB = rssKB()
	if info, err := os.Stat(LogPath(a.cfg.Dir)); err == nil {
		s.LogBytes = info.Size()
	}
	a.mu.Lock()
	for _, w := range a.workloads {
		s.Workloads[w.spec.Kind+"/"+w.state]++
		s.Total++
		if w.out.isOpen() {
			s.OpenLogs++
			if w.state == StateExited {
				s.EndedLogsOpen++
			}
		}
	}
	a.mu.Unlock()
	return s
}

// rssKB is VmRSS from /proc/self/status.
func rssKB() int {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
			return n
		}
	}
	return 0
}

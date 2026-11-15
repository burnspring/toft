package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"toft/internal/agent"
	"toft/internal/runner"
)

// A host run placed by the shared-mode agent (ADR §133).
//
// Everything up to and including the identity file and the storage attach is
// the platform's, as it always was; then the run is described to the agent —
// `kind: run`, never restarted — and the agent starts it, holds its memory
// limit (a cgroup where the machine allows, its watchdog otherwise) and keeps
// it. The handle keeps its host: shape. Because the agent outlives the
// platform, a run that was in flight across a platform restart is still
// watched, which a run the platform placed itself never was.

// runLogPath is where the agent appends a run's runner output: LOGS_DIR/<app>,
// platform-owned 0700, never the run's own directory, which the app's account
// owns and root must not write into.
func (h *HostJobBackend) runLogPath(appID, runID string) string {
	if h.logsDir == "" || appID == "" || strings.ContainsAny(appID, `/\`) {
		return ""
	}
	return filepath.Join(h.logsDir, appID, "run-"+runID+".log")
}

func (h *HostJobBackend) startOnAgent(ctx context.Context, spec WorkloadSpec, dir string, env map[string]string, account *AppAccount) (string, error) {
	runID := spec.Name
	logPath := h.runLogPath(spec.AppID, runID)
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return "", fmt.Errorf("preparing the run's log directory: %w", err)
		}
	}
	desc := agent.Spec{
		Name: runID, AppID: spec.AppID, Kind: agent.KindRun, UID: -1,
		Dir: dir, Env: env, MemoryLimitMB: spec.MemoryLimitMB,
		CPULimitMillicores: spec.CPULimitMillicores,
		Restart:            agent.RestartNever, LogPath: logPath,
		Labels: map[string]string{"run_id": runID},
	}
	if ns, gateway := h.rt.namespaced(); ns {
		desc.Env[runner.EnvCallbackURL] = h.rt.callbackFor(gateway)
		desc.Mask = append(desc.Mask, h.rt.maskDirs...)
		desc.Keep = append(desc.Keep, filepath.Join(h.host.appsDir, spec.AppID), dir)
		mask, keep := h.host.storage.NamespaceView(spec.Mounts)
		desc.Mask = append(desc.Mask, mask...)
		desc.Keep = append(desc.Keep, keep...)
	}
	if account != nil {
		groups, err := accountGroups(account)
		if err != nil {
			return "", fmt.Errorf("reading %s's groups: %w", account.Username, err)
		}
		desc.UID, desc.GID, desc.Groups = account.UID, account.GID, groups
	}
	var st agent.Status
	err := h.recover(func() error {
		var serr error
		st, serr = h.agent.Start(ctx, desc)
		return serr
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("starting the runner: %w", err)
	}
	start, _, err := procStartAndState(st.PID)
	if err != nil {
		start = 0
	}
	return fmt.Sprintf("%s%d:%d:%s:%s", hostHandlePrefix, st.PID, start, spec.AppID, runID), nil
}

// agentStatus asks the agent. A terminal answer is read once: the run is then
// removed from the agent and its directory from disk, and the answer kept here.
func (h *HostJobBackend) agentStatus(ctx context.Context, appID, runID string) (JobStatus, error) {
	h.mu.Lock()
	if st, ok := h.final[runID]; ok {
		h.mu.Unlock()
		return st, nil
	}
	h.mu.Unlock()

	var st agent.Status
	err := h.recover(func() error {
		var gerr error
		st, gerr = h.agent.Get(ctx, runID)
		return gerr
	})
	if errors.Is(err, agent.ErrNotFound) {
		// A concurrent read may have just taken the run's end and removed it
		// from the agent — the jobs service polls shared runs from more than
		// one loop, and Forget reads too. That reader stored the answer before
		// the delete, so it is here (BB-007: an out-of-memory run recorded as
		// lost).
		h.mu.Lock()
		if final, ok := h.final[runID]; ok {
			h.mu.Unlock()
			return final, nil
		}
		h.mu.Unlock()
		// The agent holds no such run: it was lost with the agent's state,
		// which only a reboot empties.
		_ = os.RemoveAll(h.runDir(appID, runID))
		return JobStatus{State: JobInfrastructureErr,
			Message: "the run ended while nothing was watching it, and never reported — " +
				"usually the machine restarted and took the run with it"}, nil
	}
	if err != nil {
		return JobStatus{}, err
	}
	switch st.State {
	case agent.StateExited, agent.StateStopped:
	default:
		return JobStatus{State: JobRunning, PeakMemoryMB: st.PeakMemoryMB}, nil
	}

	out := terminalRunStatus(st, runLogTail(h.runLogPath(appID, runID)))
	// Kept before the agent forgets the run, never after: a reader that finds
	// the run gone must find this instead.
	h.mu.Lock()
	if first, ok := h.final[runID]; ok {
		out = first
	} else {
		h.final[runID] = out
	}
	h.mu.Unlock()
	if err := h.recover(func() error { return h.agent.Delete(ctx, runID) }); err != nil && !errors.Is(err, agent.ErrNotFound) {
		log.Printf("host jobs: removing finished run %s from the agent: %v", runID, err)
	}
	_ = os.RemoveAll(h.runDir(appID, runID))
	return out, nil
}

// terminalRunStatus is a finished run, as the agent saw it end.
func terminalRunStatus(st agent.Status, tail string) JobStatus {
	code := 0
	if st.ExitCode != nil {
		code = *st.ExitCode
	}
	if st.OOMKilled {
		msg := fmt.Sprintf("stopped at %d MB, its memory limit", st.MemoryLimitMB)
		if st.PeakMemoryMB > 0 && st.PeakMemoryMB < st.MemoryLimitMB {
			msg = fmt.Sprintf("stopped at %d MB (its limit is %d MB)", st.PeakMemoryMB, st.MemoryLimitMB)
		}
		return JobStatus{State: JobResourceExceeded, ExitCode: code, Message: msg, PeakMemoryMB: st.PeakMemoryMB}
	}
	if st.ExitCode != nil && code == 0 {
		return JobStatus{State: JobSucceeded, PeakMemoryMB: st.PeakMemoryMB}
	}
	// The runner reports the app's own exit before it ends, and the first
	// terminal status wins; this is what is left when it could not.
	var msg string
	switch {
	case st.ExitSignal != "":
		msg = "the run was ended by " + st.ExitSignal
	case st.ExitCode == nil:
		msg = "the run ended and the agent could not read how"
	default:
		msg = fmt.Sprintf("the run exited %d", code)
	}
	if tail != "" {
		msg += "; the runner said: " + tail
	}
	if st.ExitCode == nil {
		code = -1
	}
	return JobStatus{State: JobFailed, ExitCode: code, Stage: FailureExec, Message: msg, PeakMemoryMB: st.PeakMemoryMB}
}

// runLogTail is the runner's own last words in a run's log: its platform-voice
// lines, unmarked, bounded. The app's lines reached the run's log through the
// runner's shipper already.
func runLogTail(path string) string {
	if path == "" {
		return ""
	}
	var keep []string
	for _, l := range lastLines(path, 200) {
		_, source, _, text := runner.ParseLogLine(l)
		if source == runner.LogSourcePlatform {
			keep = append(keep, text)
		}
	}
	if len(keep) > 20 {
		keep = keep[len(keep)-20:]
	}
	out := strings.Join(keep, "\n")
	if len(out) > 4000 {
		out = out[len(out)-4000:]
	}
	return out
}

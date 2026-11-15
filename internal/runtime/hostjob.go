package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"toft/internal/agent"
	"toft/internal/runner"
	"toft/internal/toolchain"
)

// HostRunnerCommand is the platform binary's subcommand that runs one job on
// this machine. The host backend starts it; nothing else should.
const HostRunnerCommand = "runner"

// hostHandlePrefix marks a handle as this backend's, so a dispatcher holding
// two backends routes a Status or a Cancel to the one that started the run.
const hostHandlePrefix = "host:"

// IsHostHandle reports whether a run's stored handle belongs to the host
// backend.
func IsHostHandle(handle string) bool { return strings.HasPrefix(handle, hostHandlePrefix) }

// HostJobBackend runs job runs as processes on the platform's own machine —
// the backend an install with no cluster uses (ADR §106).
//
// **It runs the runner, not the job.** The runner is the program that already
// turns a bootstrap into a run with shipped logs, a result and an exit code, so
// a host run and a pod speak one contract and differ only in where the process
// lives. The shared-mode agent places it (hostjob_agent.go), never to be
// restarted: for a job a restart is a second run nobody asked for.
//
// Each run gets a directory of its own under APPS_DIR/<app>/runs/<run>, holding
// its identity file and its scratch, and removed when the run is over. The
// release is the one the shared backend would start — resolved, repaired and
// handed to the app's account by the same functions.
type HostJobBackend struct {
	host hostSide
	// agent is the shared-mode agent the runs are placed by (ADR
	// §133): it starts, watches and holds each run, and
	// because it outlives the platform an adopted run is watched too.
	agent *agent.Client
	// logsDir is LOGS_DIR, where the agent appends a run's runner output.
	logsDir string
	// rt is the shared runtime, for its tier: a run at the namespace tier
	// reaches the platform on the gateway and sees only its own app's tree.
	rt *AgentRuntime

	callbackURL string
	// mint issues the single-use bootstrap credential the runner presents in
	// place of a projected ServiceAccount token.
	mint func(appID, runID, workloadName string) (string, error)
	// runRoot, when set, holds each run's directory as <runRoot>/<app>/<run>
	// in place of APPS_DIR/<app>/runs/<run> (JOB_RUN_DIR). The run's copy of
	// the release is unpacked there and handed to the app's account a file at
	// a time, which on an APPS_DIR on a network filesystem is minutes of queue
	// for a release with a venv or node_modules in it; on local disk, seconds.
	runRoot string

	mu sync.Mutex
	// final holds a run's terminal status once it has been read, because
	// reading it is what removes the run from the agent.
	final map[string]JobStatus
}

// HostMemoryEnforcement says how a host run's memory limit is held: by the
// kernel, or by the agent's sampling watchdog.
func (h *HostJobBackend) HostMemoryEnforcement() string {
	var l agent.List
	err := h.recover(func() error {
		var lerr error
		l, lerr = h.agent.List(context.Background())
		return lerr
	})
	if err == nil && l.MemoryHeldBy != "" {
		return l.MemoryHeldBy
	}
	return "watchdog"
}

// recover is the shared runtime's: a call nothing answered on the socket
// starts a new agent, which adopts every run still going, and is made again
// (ADR §134). Without it a run dispatched while the agent was down failed for
// good, and a stop in the same window was lost. It retries only when nothing
// answered, so a Start cannot run twice.
func (h *HostJobBackend) recover(call func() error) error {
	if h.rt == nil {
		return call()
	}
	return h.rt.recover(call)
}

// hostSide is what a host run shares with the app's own service: one
// resolver, one repairer, one account policy and one storage root, so a run
// and its app see the same release as the same account.
type hostSide struct {
	isolation Isolation
	resolver  ReleaseResolver
	repairer  Repairer
	storage   *SharedStorage
	appsDir   string
}

// HostJobs builds the host job backend over the agent's shared runtime: the
// same preparation, and the agent places, watches and holds every run.
// runRoot is JOB_RUN_DIR — empty keeps every run under APPS_DIR.
func (r *AgentRuntime) HostJobs(callbackURL string, mint func(appID, runID, workloadName string) (string, error), runRoot string) (*HostJobBackend, error) {
	return &HostJobBackend{
		host:  hostSide{r.isolation, r.resolver, r.repairer, r.storage, r.appsDir},
		agent: r.client, logsDir: r.logsDir, rt: r,
		callbackURL: callbackURL, mint: mint, runRoot: runRoot,
		final: map[string]JobStatus{},
	}, nil
}

// Start resolves the release, prepares the run's directory and starts the
// runner under the app's account, in a session of its own.
func (h *HostJobBackend) Start(ctx context.Context, spec WorkloadSpec) (string, error) {
	runID := spec.Name
	if spec.AppID == "" || runID == "" {
		return "", errors.New("a host run needs an app and a run")
	}
	p := h.host
	if p.storage == nil {
		if err := RejectMounts(spec); err != nil {
			return "", err
		}
	}

	if err := h.prepareRunRoot(); err != nil {
		return "", err
	}
	dir := h.runDir(spec.AppID, runID)
	// The app's run parent is the platform's own or it is not used: under an
	// operator's shared JOB_RUN_DIR (/tmp is contemplated above) another
	// account could create <root>/<appID> first, or plant a link there, and
	// root's writes for this app's runs — its identity token among them — went
	// through a directory the other account controlled (code review
	// 2026-11-11, Q2-5).
	if err := ensureOwnRunParent(filepath.Dir(dir)); err != nil {
		return "", err
	}
	scratch := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", fmt.Errorf("creating the run directory: %w", err)
	}
	// Whatever fails from here until the agent holds the run takes the
	// directory with it. Only Status, Forget and End remove a run directory,
	// and none of them ever sees a run that returned no handle — so a storage
	// mapping Attach refused on every scheduled run left a private release
	// copy behind each time until the disk filled (code review 2026-11-11,
	// Q2-2).
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(dir)
		}
	}()

	// The run gets a release of its own wherever one can be fetched, never the
	// app's tree: resolving an artifact in place swaps the directory the app's
	// service is running out of. See runtime.PrivateReleaseResolver.
	var release string
	var err error
	if private, ok := p.resolver.(PrivateReleaseResolver); ok {
		release, err = private.ResolvePrivate(spec.Release, filepath.Join(dir, "release"))
	} else {
		release, err = p.resolver.Resolve(spec.Release)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("resolving the release for run %s: %w", runID, err)
	}
	if p.repairer != nil {
		if err := p.repairer.Repair(spec, release); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("repairing the release for run %s: %w", runID, err)
		}
	}

	account, err := p.isolation.resolve(spec.Slug)
	if err != nil {
		return "", fmt.Errorf("resolving app account: %w", err)
	}

	token, err := h.mint(spec.AppID, runID, runID)
	if err != nil {
		return "", fmt.Errorf("issuing the run's bootstrap credential: %w", err)
	}
	identity := filepath.Join(dir, "identity")
	if err := os.WriteFile(identity, []byte(token), 0o400); err != nil {
		return "", fmt.Errorf("writing the run's identity: %w", err)
	}

	env := map[string]string{}
	for _, kv := range BaseEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	env["HOME"] = dir
	// Where the operator put the interpreters: the runner resolves the
	// release's own from them (runner.InterpreterForRelease), and BaseEnv
	// does not carry them.
	for _, kv := range toolchain.DeclarationEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	env[runner.EnvCallbackURL] = h.callbackURL
	env[runner.EnvMode] = "job"
	env[runner.EnvWorkDir] = dir
	env["TOFT_WORKLOAD_NAME"] = runID
	env[runner.EnvIdentityFile] = identity
	env[runner.EnvReleaseDir] = release
	env[runner.EnvScratchPath] = scratch

	if account != nil {
		// The gate, then the trees — Deploy's order, for Deploy's reason: the
		// app's directory stops being reachable by other app accounts before
		// this app's code and credential are handed to it.
		if err := gateAppDir(p.appsDir, spec.AppID, account); err != nil {
			return "", fmt.Errorf("gating app directory for %s: %w", account.Username, err)
		}
		if h.runRoot != "" {
			if err := gateDir(filepath.Dir(dir), account); err != nil {
				return "", fmt.Errorf("gating the app's run directory for %s: %w", account.Username, err)
			}
		}
		// A release fetched into the run's own directory is handed over with
		// that directory, in one walk. Walking it twice — once as the
		// release, once inside the run — doubled the costliest part of a
		// host start: a chown per file of a venv or node_modules tree, which
		// on a network filesystem is a round trip each, and all of it is
		// time the run spends queued.
		if !insideDir(release, dir) {
			if err := ownTree(release, account); err != nil {
				return "", fmt.Errorf("handing release to %s: %w", account.Username, err)
			}
		}
		if err := ownTree(dir, account); err != nil {
			return "", fmt.Errorf("handing the run directory to %s: %w", account.Username, err)
		}
	}
	// Attached with no mounts too: that is what takes the account out of a gate
	// it no longer holds. See SharedStorage.Attach.
	if p.storage != nil && (len(spec.Mounts) > 0 || account != nil) {
		attached, err := p.storage.Attach(spec.AppID, account, spec.Mounts)
		if err != nil {
			return "", fmt.Errorf("attaching storage for run %s: %w", runID, err)
		}
		if len(attached) > 0 {
			env[runner.EnvStorage] = DescribeMounts(attached)
		}
	}

	handle, err := h.startOnAgent(ctx, spec, dir, env, account)
	started = err == nil
	return handle, err
}

// Status asks the agent, which watched the run whether or not this platform
// process was alive for all of it.
func (h *HostJobBackend) Status(ctx context.Context, handle string) (JobStatus, error) {
	_, _, appID, runID, err := parseHostHandle(handle)
	if err != nil {
		return JobStatus{}, err
	}
	return h.agentStatus(ctx, appID, runID)
}

// Cancel asks the agent to stop the run's whole process group: SIGTERM, and
// SIGKILL after the grace the runner forwards to the app. The agent waits for
// both, so this does not.
func (h *HostJobBackend) Cancel(ctx context.Context, handle string) error {
	_, _, _, runID, err := parseHostHandle(handle)
	if err != nil {
		return err
	}
	go func() {
		if err := h.recover(func() error { return h.agent.Stop(context.Background(), runID) }); err != nil && !errors.Is(err, agent.ErrNotFound) {
			log.Printf("host jobs: cancelling run %s: %v", runID, err)
		}
	}()
	return nil
}

// Forget reads the run's end once its runner has exited, which removes it from
// the agent and its directory from disk (agentStatus). In the background and
// bounded: the runner reports before it exits, and one that never does is the
// deadline sweep's.
func (h *HostJobBackend) Forget(handle string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for {
			if st, err := h.Status(ctx, handle); err == nil && st.State.Terminal() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// End is Cancel waited for, and the run forgotten: the agent stops its process
// group and removes it with its output, and the run's directory goes too. For
// the app's delete, which removes the app's files next.
func (h *HostJobBackend) End(ctx context.Context, handle string) error {
	_, _, appID, runID, err := parseHostHandle(handle)
	if err != nil {
		return err
	}
	if err := h.recover(func() error { return h.agent.Delete(ctx, runID) }); err != nil && !errors.Is(err, agent.ErrNotFound) {
		return err
	}
	_ = os.RemoveAll(h.runDir(appID, runID))
	return nil
}

// prepareRunRoot makes JOB_RUN_DIR a directory every app account can pass
// through and none can list. One the platform creates is made 0711; one that
// already exists is the operator's and is never chmodded — JOB_RUN_DIR=/tmp
// would otherwise lose its sticky, world-writable mode on the first run — so
// it must already be traversable, and is refused by name when it is not.
func (h *HostJobBackend) prepareRunRoot() error {
	if h.runRoot == "" {
		return nil
	}
	info, err := os.Stat(h.runRoot)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(h.runRoot, 0o711); err != nil {
			return fmt.Errorf("creating JOB_RUN_DIR %s: %w", h.runRoot, err)
		}
		// MkdirAll's mode is under the umask.
		return os.Chmod(h.runRoot, 0o711)
	}
	if err != nil {
		return fmt.Errorf("reading JOB_RUN_DIR %s: %w", h.runRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("JOB_RUN_DIR %s is not a directory", h.runRoot)
	}
	if info.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("JOB_RUN_DIR %s is %#o: an app's account cannot reach its run under it — "+
			"make it traversable (0711), or name a directory the platform may create", h.runRoot, info.Mode().Perm())
	}
	return nil
}

// runDir is where one run's identity, scratch and release copy live, and what
// is removed when the run is over.
func (h *HostJobBackend) runDir(appID, runID string) string {
	if h.runRoot != "" {
		return filepath.Join(h.runRoot, appID, runID)
	}
	return filepath.Join(h.host.appsDir, appID, "runs", runID)
}

// insideDir reports whether path is dir or lies beneath it, lexically.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func parseHostHandle(handle string) (pid int, start uint64, appID, runID string, err error) {
	parts := strings.SplitN(strings.TrimPrefix(handle, hostHandlePrefix), ":", 4)
	if !IsHostHandle(handle) || len(parts) != 4 {
		return 0, 0, "", "", fmt.Errorf("not a host run handle: %q", handle)
	}
	if pid, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, "", "", fmt.Errorf("not a host run handle: %q", handle)
	}
	if start, err = strconv.ParseUint(parts[1], 10, 64); err != nil {
		return 0, 0, "", "", fmt.Errorf("not a host run handle: %q", handle)
	}
	return pid, start, parts[2], parts[3], nil
}

// procStartAndState reads a process's start time (field 22, in clock ticks
// since boot) and its state letter from /proc/<pid>/stat. The start time is
// what tells a run from a later process that was given the same pid.
func procStartAndState(pid int) (uint64, byte, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	// The command name is parenthesised and may contain spaces; everything
	// after the last ')' is space-separated.
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, errors.New("unreadable stat")
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is state (field 3); starttime is field 22, i.e. fields[19].
	if len(fields) < 20 || len(fields[0]) == 0 {
		return 0, 0, errors.New("short stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return start, fields[0][0], nil
}

// ensureOwnRunParent makes parent exist as a directory this process created
// (or one it owns), never a link and never another account's (Q2-5).
func ensureOwnRunParent(parent string) error {
	fi, err := os.Lstat(parent)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(parent), 0o711); err != nil {
			return fmt.Errorf("creating the run root: %w", err)
		}
		if err := os.Mkdir(parent, 0o710); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating the app's run directory: %w", err)
		}
		if fi, err = os.Lstat(parent); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("reading the app's run directory: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a plain directory; it was not made by this platform and is not used", parent)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to uid %d, not to this platform; it was not made here and is not used", parent, st.Uid)
	}
	return nil
}

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"toft/internal/agent"
	"toft/internal/procstat"
	"toft/internal/runner"
	"toft/internal/toolchain"
)

// AgentRuntime is the shared backend over the shared-mode agent (ADR
// §129): the platform prepares a workload exactly as it always has
// — resolve, repair, account, gate, own the tree, attach storage, wipe scratch
// — and then hands the agent a description with no secret in it. The agent
// runs `toft runner` as the app's account, and the runner bootstraps like a
// pod: the app's environment, its token and its role key come from the
// platform at bootstrap, under an identity the agent issued for that process.
//
// Port allocation and loopback binding stay here, the backend's own facts, so
// no caller of the seam learns about either.
type AgentRuntime struct {
	isolation Isolation
	resolver  ReleaseResolver
	allocator PortAllocator
	appsDir   string
	// callbackURL is where a runner reaches the platform: its bootstrap, its
	// reports, and the app's SDK after it (runner.appEnv).
	callbackURL string
	repairer    Repairer
	narrator    Narrator
	storage     *SharedStorage
	logsDir     string

	client *agent.Client
	// exe and opts are what EnsureAgent started the agent with, so a call
	// that finds no agent can start the next one the same way.
	exe      string
	opts     AgentOptions
	ensureMu sync.Mutex
	// The agent's tier, and at the namespace tier its range and the gateway
	// the platform answers on: read after every start of an agent, and fixed
	// for that agent's life.
	tier, bridge, gateway string
	// maskDirs are what a namespace-tier workload must not see: APPS_DIR,
	// LOGS_DIR, CACHE_DIR, JOB_RUN_DIR (WithMaskDirs).
	maskDirs []string

	mu       sync.Mutex
	bindings map[string]Endpoint

	cacheMu  sync.Mutex
	cached   []agent.Status
	cachedAt time.Time
	cacheErr error

	sampleMu sync.Mutex
	samples  map[string]cpuSample
	now      func() time.Time
	tree     *procstat.TreeSampler

	outMu   sync.Mutex
	outStop context.CancelFunc
}

// NewAgentRuntime is the shared backend over the agent whose directory is dir.
func NewAgentRuntime(isolation Isolation, resolver ReleaseResolver, allocator PortAllocator, appsDir, dir string) *AgentRuntime {
	if resolver == nil {
		resolver = LocalResolver{}
	}
	return &AgentRuntime{
		isolation: isolation, resolver: resolver, allocator: allocator, appsDir: appsDir,
		client:   agent.NewClient(dir),
		bindings: map[string]Endpoint{},
		samples:  map[string]cpuSample{},
		now:      time.Now,
		tree:     procstat.NewTreeSampler(),
	}
}

func (r *AgentRuntime) WithLogsDir(dir string) *AgentRuntime { r.logsDir = dir; return r }

// WithCallbackURL is where a runner reaches the platform.
func (r *AgentRuntime) WithCallbackURL(url string) *AgentRuntime {
	r.callbackURL = strings.TrimSuffix(url, "/")
	return r
}

func (r *AgentRuntime) WithRepairer(rep Repairer) *AgentRuntime          { r.repairer = rep; return r }
func (r *AgentRuntime) WithNarrator(n Narrator) *AgentRuntime            { r.narrator = n; return r }
func (r *AgentRuntime) WithSharedStorage(s *SharedStorage) *AgentRuntime { r.storage = s; return r }

// Client is the agent's, for the platform's own wiring.
func (r *AgentRuntime) Client() *agent.Client { return r.client }

// Containment is the agent's tier and why the next one up was refused, or
// two empty strings when it does not answer.
func (r *AgentRuntime) Containment() (tier, reason string) {
	l, err := r.client.List(context.Background())
	if err != nil {
		return "", ""
	}
	return l.Tier, l.TierReason
}

// AgentStats is the agent's own figures (agent.Stats): what a soak, or an
// administrator wondering about the agent itself, watches for climbing.
func (r *AgentRuntime) AgentStats(ctx context.Context) (agent.Stats, error) {
	return r.client.Stats(ctx)
}

// AgentList is the agent's own listing, tier and all: what the system checks
// read to say how each workload is actually held.
func (r *AgentRuntime) AgentList(ctx context.Context) (agent.List, error) {
	return r.client.List(ctx)
}

// AgentOptions are the agent's own configuration, which reaches it only when
// it is started: an agent already running keeps the options it was started
// with, and a re-exec passes them on.
type AgentOptions struct {
	// CgroupParent is AGENT_CGROUP_PARENT.
	CgroupParent string
	// Namespaces asks for the namespace tier, reached only where the machine
	// allows every part of it (ADR §128, §135, §136). PlatformPort is the port a
	// namespaced workload reaches the platform on, at the gateway.
	Namespaces   bool
	PlatformPort int
	// PlatformHost is HOST, the address the platform listens on.
	PlatformHost string
	// BridgeCIDR is the range to use: AGENT_BRIDGE_CIDR, or the one this
	// install picked before and remembers (D13).
	BridgeCIDR    string
	BlockMetadata bool
}

func (o AgentOptions) args() []string {
	var out []string
	if o.CgroupParent != "" {
		out = append(out, "--cgroup-parent", o.CgroupParent)
	}
	if o.Namespaces {
		out = append(out, "--namespaces", "--platform-port", strconv.Itoa(o.PlatformPort))
		if o.PlatformHost != "" {
			out = append(out, "--platform-host", o.PlatformHost)
		}
	}
	if o.BridgeCIDR != "" {
		out = append(out, "--bridge-cidr", o.BridgeCIDR)
	}
	if o.BlockMetadata {
		out = append(out, "--block-metadata")
	}
	return out
}

// WithMaskDirs names what a namespace-tier workload must not see.
func (r *AgentRuntime) WithMaskDirs(dirs ...string) *AgentRuntime {
	for _, d := range dirs {
		if d != "" {
			r.maskDirs = append(r.maskDirs, d)
		}
	}
	return r
}

// EnsureAgent makes sure the install's agent is running, starting it from exe
// with the allowlisted base environment when nothing answers. The agent keeps
// that environment for its life and hands it to every runner it starts, so —
// as PM2's daemon was — it must never be first started from a request path.
func (r *AgentRuntime) EnsureAgent(exe string, o AgentOptions) error {
	r.ensureMu.Lock()
	defer r.ensureMu.Unlock()
	r.exe, r.opts = exe, o
	_, err := agent.Ensure(context.Background(), r.client.Dir(), exe, BaseEnv(), o.args()...)
	var pe *agent.ProtocolError
	if errors.As(err, &pe) {
		// A running agent that speaks another protocol is replaced in place
		// (ADR §134): it re-executes the binary on disk, keeping its pid and
		// with it every workload. Only a protocol change does this — a
		// rebuild with the same protocol keeps the agent it finds.
		log.Printf("the shared-mode agent speaks protocol %s and this platform %s; replacing it in place", pe.Agent, pe.Caller)
		if err := r.client.Reexec(context.Background()); err != nil {
			return fmt.Errorf("replacing the shared-mode agent: %w", err)
		}
		err = fmt.Errorf("the shared-mode agent was asked to replace itself and has not answered since")
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			if l, lerr := r.client.List(context.Background()); lerr == nil {
				log.Printf("the agent was replaced; %d workload(s) carried over", len(l.Workloads))
				err = nil
				break
			}
		}
	}
	if err != nil {
		return err
	}
	l, err := r.client.List(context.Background())
	if err != nil {
		return err
	}
	r.tier, r.bridge, r.gateway = l.Tier, l.Bridge, l.Gateway
	return nil
}

// Bridge is the range the agent's namespace tier uses, or "" below it.
func (r *AgentRuntime) Bridge() string {
	r.ensureMu.Lock()
	defer r.ensureMu.Unlock()
	return r.bridge
}

// namespaced reports whether workloads get namespaces of their own, and the
// gateway the platform answers them on.
func (r *AgentRuntime) namespaced() (bool, string) {
	r.ensureMu.Lock()
	defer r.ensureMu.Unlock()
	return r.tier == agent.TierNamespace && r.gateway != "", r.gateway
}

// callbackFor is the platform's address as a workload reaches it: the gateway
// at the namespace tier, where its own loopback is its own.
func (r *AgentRuntime) callbackFor(gateway string) string {
	if gateway == "" {
		return r.callbackURL
	}
	u, err := url.Parse(r.callbackURL)
	if err != nil {
		return r.callbackURL
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(r.opts.PlatformPort)
	}
	u.Host = net.JoinHostPort(gateway, port)
	return u.String()
}

// recover runs call, and when nothing answered on the socket starts a new
// agent and runs it again (ADR §134: the agent died while the platform was up).
// The new agent adopts every workload still running, so the apps never
// noticed; their next exit is reported with no code. A runtime never started
// with EnsureAgent — a test's — has nothing to start and reports the failure.
func (r *AgentRuntime) recover(call func() error) error {
	err := call()
	if err == nil || !agent.Unreachable(err) {
		return err
	}
	r.ensureMu.Lock()
	exe, opts := r.exe, r.opts
	r.ensureMu.Unlock()
	if exe == "" {
		return err
	}
	log.Printf("the shared-mode agent did not answer (%v); starting a new one, which adopts what is running", err)
	if eerr := r.EnsureAgent(exe, opts); eerr != nil {
		return fmt.Errorf("%w; starting a new agent: %v", err, eerr)
	}
	return call()
}

func (r *AgentRuntime) RootsDeclared() bool { return r.storage.HasRoots() }

func (r *AgentRuntime) ResolvedMounts(appID string, mounts []Mount) []Mount {
	return r.storage.ResolvedMounts(appID, mounts)
}

func (r *AgentRuntime) say(spec WorkloadSpec, stream, format string, args ...any) {
	if r.narrator == nil || spec.AppID == "" {
		return
	}
	r.narrator.Say(spec.AppID, spec.ProcessName(), stream, fmt.Sprintf(format, args...))
}

func (r *AgentRuntime) appLogDir(appID string) string {
	if r.logsDir == "" || appID == "" || strings.ContainsAny(appID, `/\`) || appID == "." || appID == ".." {
		return ""
	}
	return filepath.Join(r.logsDir, appID)
}

// logPath is where the agent appends a workload's output: LOGS_DIR/<app>,
// platform-owned 0700, which is the gate between one app's log and another
// account's (the invariant's gate, now kept by the agent).
func (r *AgentRuntime) logPath(appID, name string) string {
	if dir := r.appLogDir(appID); dir != "" {
		return filepath.Join(dir, name+".log")
	}
	return ""
}

// Deploy prepares the release as the shared backend always has, then asks the
// agent to run the runner over it. The endpoint is chosen here.
func (r *AgentRuntime) Deploy(spec WorkloadSpec) (Endpoint, error) {
	name := spec.ProcessName()
	if !r.RootsDeclared() {
		if err := RejectMounts(spec); err != nil {
			return Endpoint{}, err
		}
	}
	if r.narrator != nil && spec.AppID != "" {
		r.narrator.Starting(spec.AppID, name)
	}
	r.say(spec, LogStreamStdout, "==> Starting %s", releaseLabel(spec.Release))

	dir, err := r.resolver.Resolve(spec.Release)
	if err != nil {
		r.say(spec, LogStreamStderr, "This release could not be fetched, so the app was not started.")
		return Endpoint{}, fmt.Errorf("resolving release for %s: %w", name, err)
	}
	if r.repairer != nil {
		if err := r.repairer.Repair(spec, dir); err != nil {
			r.say(spec, LogStreamStderr,
				"This release's dependencies could not be rebuilt, so the app was not started. Redeploy it.")
			return Endpoint{}, fmt.Errorf("repairing the release for %s: %w", name, err)
		}
	} else if broken, what := runner.Unrunnable(spec.Framework, dir); broken {
		log.Printf("start: %s cannot run as fetched (%s) and this backend has no "+
			"repairer wired, so it is being started unrepaired", name, what)
		r.say(spec, LogStreamStderr,
			"This release cannot run as fetched (%s) and could not be rebuilt here. "+
				"It is being started anyway and will probably fail; redeploy it.", what)
	}

	// At the namespace tier the workload's address is its own and the port
	// the same for every one; below it, a port on loopback from the range.
	ns, gateway := r.namespaced()
	var endpoint Endpoint
	started := false
	bindAddr := spec.BindAddress
	if bindAddr == "" {
		bindAddr = BindLoopback
	}
	if ns {
		endpoint.Port, bindAddr = agent.WorkloadPort, BindAll
	} else {
		var fresh bool
		endpoint, fresh, err = r.bind(name)
		if err != nil {
			return Endpoint{}, err
		}
		defer func() {
			if !started && fresh {
				r.releaseBinding(name)
			}
		}()
	}
	if dir := r.appLogDir(spec.AppID); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Endpoint{}, fmt.Errorf("preparing the log directory for %s: %w", name, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return Endpoint{}, fmt.Errorf("preparing the log directory for %s: %w", name, err)
		}
	}

	env := map[string]string{}
	for _, kv := range BaseEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}

	// Where the operator put the interpreters: the runner resolves the
	// release's own from them (runner.InterpreterForRelease), and BaseEnv
	// does not carry them.
	for _, kv := range toolchain.DeclarationEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	account, err := r.isolation.resolve(spec.Slug)
	if err != nil {
		return Endpoint{}, fmt.Errorf("resolving app account: %w", err)
	}
	if account != nil {
		if err := gateAppDir(r.appsDir, spec.AppID, account); err != nil {
			return Endpoint{}, fmt.Errorf("gating app directory for %s: %w", account.Username, err)
		}
		if err := ownTree(dir, account); err != nil {
			return Endpoint{}, fmt.Errorf("handing release to %s: %w", account.Username, err)
		}
		// Frameworks write caches under $HOME, and the platform's is neither
		// writable by nor appropriate for the app's account.
		env["HOME"] = dir
	}

	if len(spec.Mounts) > 0 || (account != nil && r.storage != nil) {
		attached := unattachedStores(spec.Mounts)
		if r.storage != nil {
			attached, err = r.storage.Attach(spec.AppID, account, spec.Mounts)
			if err != nil {
				r.say(spec, LogStreamStderr,
					"This app's durable storage could not be attached, so it was not started.")
				return Endpoint{}, fmt.Errorf("attaching storage for %s: %w", name, err)
			}
		}
		if len(attached) > 0 {
			env[runner.EnvStorage] = DescribeMounts(attached)
		}
	}

	scratch := ""
	if r.appsDir != "" && spec.AppID != "" {
		scratch = ScratchDir(r.appsDir, spec.AppID)
		if err := wipeScratch(scratch); err != nil {
			return Endpoint{}, fmt.Errorf("wiping scratch dir: %w", err)
		}
		if err := os.MkdirAll(scratch, 0o700); err != nil {
			return Endpoint{}, fmt.Errorf("creating scratch dir: %w", err)
		}
		if account != nil {
			if err := ownTree(scratch, account); err != nil {
				return Endpoint{}, fmt.Errorf("handing scratch dir to %s: %w", account.Username, err)
			}
		}
		env[runner.EnvScratchPath] = scratch
	}

	// The runner's inputs, every one unset before the app runs
	// (runner.OptionsFromEnv). No secret: the app's own environment arrives
	// at bootstrap.
	env[runner.EnvCallbackURL] = r.callbackFor(gateway)
	env[runner.EnvMode] = "serve"
	env[runner.EnvWorkDir] = filepath.Join(r.appsDir, spec.AppID)
	env["TOFT_WORKLOAD_NAME"] = name
	env[runner.EnvReleaseDir] = dir
	env[runner.EnvPort] = strconv.Itoa(endpoint.Port)
	env[runner.EnvBind] = bindAddr

	desc := agent.Spec{
		Name: name, AppID: spec.AppID, Kind: agent.KindService,
		UID: -1, Dir: dir, Env: env,
		MemoryLimitMB: spec.MemoryLimitMB, CPULimitMillicores: spec.CPULimitMillicores,
		Restart: agent.RestartAlways,
		LogPath: r.logPath(spec.AppID, name),
		Labels:  map[string]string{},
	}
	if deployID := releaseDeployment(r.appsDir, spec.AppID, dir); deployID != "" {
		desc.Labels["deployment_id"] = deployID
	}
	if account != nil {
		groups, err := accountGroups(account)
		if err != nil {
			return Endpoint{}, fmt.Errorf("reading %s's groups: %w", account.Username, err)
		}
		desc.UID, desc.GID, desc.Groups = account.UID, account.GID, groups
		desc.IdentityGID = account.GroupGID
	}

	if ns {
		// Everything that is not this app's: every other app's tree, every
		// app's log, the build caches. Its own tree is put back.
		desc.Mask = append(desc.Mask, r.maskDirs...)
		desc.Keep = append(desc.Keep, filepath.Join(r.appsDir, spec.AppID))
		// A folder mapping is the folder: its resource's directory is
		// covered and the folder put back (SharedStorage.NamespaceView).
		mask, keep := r.storage.NamespaceView(spec.Mounts)
		desc.Mask = append(desc.Mask, mask...)
		desc.Keep = append(desc.Keep, keep...)
	}

	defer r.invalidate()
	var placed agent.Status
	if err := r.recover(func() error {
		var err error
		placed, err = r.client.Start(context.Background(), desc)
		return err
	}); err != nil {
		r.say(spec, LogStreamStderr, "This app's process did not start, so the app is not running.")
		return Endpoint{}, fmt.Errorf("starting %s: %w", name, err)
	}
	if ns {
		if placed.Address == "" {
			return Endpoint{}, fmt.Errorf("starting %s: the agent placed it in a namespace and reported no address", name)
		}
		endpoint.Host = placed.Address
	}
	r.forgetSample(name)
	r.say(spec, LogStreamStdout, "==> Started")
	started = true
	return endpoint, nil
}

// accountGroups is the account's whole supplementary list, read the way
// initgroups(3) reads it — the per-app group and every storage gate it was
// just put in among them — because the agent's clone sets exactly this list.
func accountGroups(a *AppAccount) ([]int, error) {
	u, err := user.Lookup(a.Username)
	if err != nil {
		return nil, err
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var out []int
	for _, s := range ids {
		if g, err := strconv.Atoi(s); err == nil && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	if a.GroupGID > 0 && !seen[a.GroupGID] {
		out = append(out, a.GroupGID)
	}
	sort.Ints(out)
	return out, nil
}

// releaseDeployment reads the deployment off a release directory,
// APPS_DIR/<app>/releases/<deployment>, or "" for any other shape.
func releaseDeployment(appsDir, appID, dir string) string {
	if appsDir == "" || appID == "" {
		return ""
	}
	rel, err := filepath.Rel(filepath.Join(appsDir, appID, "releases"), dir)
	if err != nil || rel == "." || strings.ContainsRune(rel, filepath.Separator) || strings.HasPrefix(rel, "..") {
		return ""
	}
	return rel
}

func (r *AgentRuntime) bind(name string) (Endpoint, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep, ok := r.bindings[name]; ok {
		return ep, false, nil
	}
	if r.allocator == nil {
		return Endpoint{}, false, fmt.Errorf("no port allocator configured")
	}
	port, err := r.allocator.Allocate()
	if err != nil {
		return Endpoint{}, false, fmt.Errorf("allocating port for %s: %w", name, err)
	}
	ep := Endpoint{Host: BindLoopback, Port: port}
	r.bindings[name] = ep
	return ep, true, nil
}

func (r *AgentRuntime) releaseBinding(name string) {
	r.mu.Lock()
	ep, ok := r.bindings[name]
	delete(r.bindings, name)
	r.mu.Unlock()
	// Only a loopback port came from the allocator. A namespace-tier
	// workload's endpoint is its own address on the one workload port.
	if ok && r.allocator != nil && ep.Host == BindLoopback {
		r.allocator.Release(ep.Port)
	}
}

// Adopt records an endpoint handed out in a previous platform lifetime, so a
// restarted platform does not allocate a second port for a workload the agent
// is still running.
func (r *AgentRuntime) Adopt(name string, ep Endpoint) {
	// A namespaced workload's endpoint is the agent's, kept by the agent
	// across a platform restart; only a port from the range is adopted.
	if name == "" || ep.IsZero() || ep.Host != BindLoopback {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.bindings[name]; !ok {
		r.bindings[name] = ep
	}
}

// Stop halts the workload and keeps its port. A name the agent does not hold
// is an absence, the isolated backend's word for a pod that is not there.
func (r *AgentRuntime) Stop(name string) error {
	defer r.invalidate()
	if err := r.recover(func() error { return r.client.Stop(context.Background(), name) }); err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			return Absent(LogAbsentNoWorkload)
		}
		return err
	}
	return nil
}

// Delete removes the workload, its output and its identity, and gives its port
// back — the only place a port returns to the pool.
func (r *AgentRuntime) Delete(name string) error {
	defer r.invalidate()
	defer r.releaseBinding(name)
	defer r.forgetSample(name)
	if err := r.recover(func() error { return r.client.Delete(context.Background(), name) }); err != nil && !errors.Is(err, agent.ErrNotFound) {
		return err
	}
	return nil
}

// Logs is the newest lines of the workload's file and its earlier generation,
// in the runner's marked format — the one internal/kube reads a pod's log in.
func (r *AgentRuntime) Logs(name string, lines int) ([]LogLine, error) {
	st, err := r.statusOf(name)
	if err != nil {
		return nil, err
	}
	path := r.logPath(st.AppID, name)
	if path == "" {
		return nil, nil
	}
	raw := lastLines(path, lines)
	if len(raw) < lines {
		raw = append(lastLines(path+".1", lines-len(raw)), raw...)
	}
	out := make([]LogLine, 0, len(raw))
	for _, l := range raw {
		out = append(out, markedLine(l))
	}
	return out, nil
}

// markedLine reads one line of the agent's log back into its labels.
func markedLine(text string) LogLine {
	at, source, stream, line := runner.ParseLogLine(text)
	l := LogLine{Source: source, Stream: stream, Line: line}
	if !at.IsZero() {
		l.At = &at
	}
	return l
}

// LogStream follows the workload's output from now on.
func (r *AgentRuntime) LogStream(name string) (LogReader, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := r.client.Follow(ctx, name)
	if err != nil {
		cancel()
		if errors.Is(err, agent.ErrNotFound) {
			return nil, Absent(LogAbsentNoWorkload)
		}
		return nil, err
	}
	return &agentLogReader{s: s, cancel: cancel}, nil
}

type agentLogReader struct {
	s      *agent.Stream
	cancel context.CancelFunc
}

func (a *agentLogReader) Next() (LogLine, error) {
	for {
		e, err := a.s.Next()
		if err != nil {
			return LogLine{}, err
		}
		if e.Type == agent.EventLine {
			return markedLine(e.Line), nil
		}
	}
}

func (a *agentLogReader) Close() error {
	a.cancel()
	return a.s.Close()
}

// statuses is the agent's table, cached briefly so a page rendering N apps
// asks once, and the time it was fetched. That time travels with the table
// into reading(): the cumulative CPU counters in it were read by the agent
// then, not when a caller happens to look, so a rate is derived over the
// interval between fetches — never between calls served from one fetch (BB-23).
func (r *AgentRuntime) statuses() ([]agent.Status, time.Time, error) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if !r.cachedAt.IsZero() && r.now().Sub(r.cachedAt) < listCacheTTL {
		return r.cached, r.cachedAt, r.cacheErr
	}
	var l agent.List
	err := r.recover(func() error {
		var err error
		l, err = r.client.List(context.Background())
		return err
	})
	r.cached, r.cacheErr, r.cachedAt = l.Workloads, err, r.now()
	return r.cached, r.cachedAt, err
}

func (r *AgentRuntime) invalidate() {
	r.cacheMu.Lock()
	r.cachedAt = time.Time{}
	r.cacheMu.Unlock()
}

func (r *AgentRuntime) statusOf(name string) (agent.Status, error) {
	st, _, err := r.statusAt(name)
	return st, err
}

// statusAt is one workload's row and the time the table holding it was fetched.
func (r *AgentRuntime) statusAt(name string) (agent.Status, time.Time, error) {
	sts, at, err := r.statuses()
	if err != nil {
		return agent.Status{}, at, err
	}
	for _, s := range sts {
		if s.Name == name {
			return s, at, nil
		}
	}
	return agent.Status{}, at, Absent(LogAbsentNoWorkload)
}

// Metrics is one workload's report.
func (r *AgentRuntime) Metrics(name string) (*AppMetrics, error) {
	st, fetched, err := r.statusAt(name)
	if err != nil {
		if AbsenceReason(err) != "" {
			return nil, fmt.Errorf("process %s not found", name)
		}
		return nil, err
	}
	return r.metrics(st, fetched), nil
}

// metrics is one row of the agent's table as the seam reports it; fetched is
// when that table was read from the agent.
func (r *AgentRuntime) metrics(st agent.Status, fetched time.Time) *AppMetrics {
	m := &AppMetrics{
		Restarts: st.Restarts, ExitCode: st.ExitCode, ExitSignal: st.ExitSignal,
		CreatedAt: st.CreatedAt, Status: statusWord(st.State), PID: st.PID,
		MemoryLimitMB: st.MemoryLimitMB, OOMKilled: st.OOMKilled,
		ExitPeakMemoryMB: st.ExitPeakMemoryMB,
		CrashLooping:     st.CrashLooping, Detail: st.Detail,
		Wrapped: true,
	}
	if !st.StartedAt.IsZero() && st.PID > 0 {
		if s := int64(time.Since(st.StartedAt).Seconds()); s > 0 {
			m.Uptime = s
		}
	}
	m.CPU, m.Memory = r.reading(st, fetched)
	// In a cgroup of its own the runner reports for itself, from that group,
	// as in a pod (ADR §132): the collector must not bill it again.
	m.SelfReported = st.Cgroup != ""
	return m
}

// statusWord is the seam's vocabulary for an agent state. A run's `exited` is
// a stopped workload as far as a service reader is concerned.
func statusWord(state string) string {
	switch state {
	case agent.StateOnline:
		return StatusOnline
	case agent.StateLaunching:
		return StatusLaunching
	case agent.StateErrored:
		return StatusErrored
	default:
		return StatusStopped
	}
}

// reading is the workload's figures: its cgroup's, exactly, at the cgroup
// tier; otherwise the runner's tree less the runner, which is the platform's
// binary and no part of what the app is billed for.
//
// The CPU rate is derived between two readings of the counter, each stamped
// with when it was read: fetched for the agent's cgroup figures (the cached
// table's fetch time), now for the tree this process samples itself. Every
// caller shares the previous sample, so a caller that arrives with the same
// reading, or too soon after the last one to derive anything honest, gets the
// last rate derived — not a zero over a counter that did not move because it
// was not re-read, and not an absence. Absent is only "no rate yet for this
// pid" (BB-23).
func (r *AgentRuntime) reading(st agent.Status, fetched time.Time) (cpu, memory *float64) {
	name, pid := st.Name, st.PID
	if pid <= 0 {
		return nil, nil
	}
	var stat procstat.Stat
	at := fetched
	if st.CPUSeconds != nil && st.MemoryBytes != nil {
		stat = procstat.Stat{CPUSeconds: *st.CPUSeconds, RSSBytes: *st.MemoryBytes}
	} else {
		at = r.now()
		var err error
		stat, err = r.tree.Sample(name, pid)
		if err != nil {
			return nil, nil
		}
		if self, err := procstat.Read(pid); err == nil {
			stat.RSSBytes -= self.RSSBytes
			stat.CPUSeconds -= self.CPUSeconds
		}
	}
	if stat.RSSBytes < 0 {
		stat.RSSBytes = 0
	}
	memory = Measured(float64(stat.RSSBytes) / 1024 / 1024)

	if at.IsZero() {
		at = r.now()
	}
	r.sampleMu.Lock()
	defer r.sampleMu.Unlock()
	prev, known := r.samples[name]
	if !known || prev.pid != pid {
		r.samples[name] = cpuSample{pid: pid, cpuSeconds: stat.CPUSeconds, at: at}
		return nil, memory
	}
	// The same reading as last time, or one too close to it: the last rate
	// stands, and the sample is kept so the next interval is a real one.
	gap := at.Sub(prev.at)
	if gap < minSampleGap {
		return prev.lastRate(), memory
	}
	burnt := stat.CPUSeconds - prev.cpuSeconds
	next := cpuSample{pid: pid, cpuSeconds: stat.CPUSeconds, at: at, rate: prev.rate}
	if burnt >= 0 {
		next.rate = Measured(burnt / gap.Seconds() * 100)
	}
	r.samples[name] = next
	return next.lastRate(), memory
}

func (r *AgentRuntime) forgetSample(name string) {
	r.sampleMu.Lock()
	delete(r.samples, name)
	r.sampleMu.Unlock()
	r.tree.Forget(name)
}

func (r *AgentRuntime) SystemMetrics() (*SystemMetrics, error) { return getSystemMetrics() }

// List is every service the agent holds.
func (r *AgentRuntime) List() (map[string]*AppMetrics, error) {
	sts, fetched, err := r.statuses()
	if err != nil {
		return nil, err
	}
	out := map[string]*AppMetrics{}
	live := []string{}
	for _, st := range sts {
		if st.Kind != agent.KindService {
			continue
		}
		out[st.Name] = r.metrics(st, fetched)
		live = append(live, st.Name)
	}
	r.sampleMu.Lock()
	for name := range r.samples {
		if _, ok := out[name]; !ok {
			delete(r.samples, name)
		}
	}
	r.sampleMu.Unlock()
	r.tree.Retain(live)
	return out, nil
}

// VerifyWorkloadIdentity asks the agent who holds a token. Only a service's:
// a run keeps the platform's own single-use token (ADR §130).
func (r *AgentRuntime) VerifyWorkloadIdentity(ctx context.Context, token string) (WorkloadIdentity, error) {
	v, err := r.client.Verify(ctx, token)
	if err != nil {
		return WorkloadIdentity{}, err
	}
	if v.Kind != agent.KindService {
		return WorkloadIdentity{}, errors.New("not a service's identity")
	}
	return WorkloadIdentity{AppID: v.AppID, WorkloadName: v.Workload}, nil
}

// StartOutput copies every shared service's output to sink, as the agent hands
// it over: the app's own lines, never the platform's, which reach the platform
// log through the runner's shipper — and never a run's, which is copied at
// ingest. Reconnects for as long as it runs.
func (r *AgentRuntime) StartOutput(sink AppOutput) {
	ctx, cancel := context.WithCancel(context.Background())
	r.outMu.Lock()
	if r.outStop != nil {
		r.outMu.Unlock()
		cancel()
		return
	}
	r.outStop = cancel
	r.outMu.Unlock()
	go func() {
		for ctx.Err() == nil {
			s, err := r.client.Follow(ctx, "")
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			for {
				e, err := s.Next()
				if err != nil {
					if !errors.Is(err, io.EOF) && ctx.Err() == nil {
						log.Printf("agent output: %v", err)
					}
					break
				}
				// A run's lines reach the stream at ingest, once stored
				// (api.copyRunLines); copied here too they would be twice.
				if e.Type != agent.EventLine || sink == nil || e.Labels["run_id"] != "" {
					continue
				}
				at, source, stream, line := runner.ParseLogLine(e.Line)
				if source != runner.LogSourceApp {
					continue
				}
				ol := OutputLine{AppID: e.AppID, Workload: e.Workload, Stream: stream, Line: line,
					DeploymentID: e.Labels["deployment_id"], RunID: e.Labels["run_id"], Job: e.Labels["job"]}
				if !at.IsZero() {
					ol.At = &at
				}
				sink.Line(ol)
			}
			s.Close()
		}
	}()
}

// StopOutput ends StartOutput.
func (r *AgentRuntime) StopOutput() {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if r.outStop != nil {
		r.outStop()
		r.outStop = nil
	}
}

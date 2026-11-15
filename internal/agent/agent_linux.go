//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"toft/internal/procstat"
)

// Backoff is the restart policy's clock: the first restart after First, then
// Base doubling to Cap, and back to the first once a process has stayed up for
// Reset. The kubelet's numbers by default (DefaultBackoff), so the health
// watcher reads both modes' restarts on one scale.
type Backoff struct {
	First, Base, Cap, Reset time.Duration
}

// DefaultBackoff is immediate, then 10 s doubling to five minutes, reset after
// ten minutes up.
var DefaultBackoff = Backoff{First: 0, Base: 10 * time.Second, Cap: 5 * time.Minute, Reset: 10 * time.Minute}

// delay is the wait before restart number n of a run of crashes (n from 0).
func (b Backoff) delay(n int) time.Duration {
	if n == 0 {
		return b.First
	}
	d := b.Base
	for i := 1; i < n && d < b.Cap; i++ {
		d *= 2
	}
	if d > b.Cap {
		d = b.Cap
	}
	return d
}

// Config is one agent's.
type Config struct {
	// Dir holds the socket, the state and the identity files (DefaultDir).
	Dir string
	// Exe is the binary the agent starts, with Args (`runner`). Resolved once
	// at start from os.Executable and kept as a path, so a binary replaced on
	// disk — an upgrade, a rebuild — is the runner the next start runs.
	Exe  string
	Args []string
	// ShimArgs start the shim at the namespace tier (ShimCommand).
	ShimArgs []string
	// StopGrace is how long a stopped workload's process group has between
	// SIGTERM and SIGKILL.
	StopGrace time.Duration
	Backoff   Backoff
	// WatchInterval is the memory watchdog's sample for a run with a limit
	// and no cgroup to hold it; a service is sampled at ten times it. A run is
	// only ever caught at a sample, so the interval is its overshoot: at a
	// tenth of a second a 128 MB job was measured at 209–345 MB, at two
	// seconds at 976–2593 MB.
	WatchInterval time.Duration
	// CgroupParent is AGENT_CGROUP_PARENT: a cgroup v2 directory the agent may
	// create each workload's cgroup under. Empty is the agent's own.
	CgroupParent string

	// Namespaces asks for the namespace tier (ADR §135, §136), which is reached
	// only where every one of its preconditions holds and is otherwise refused
	// with the reason. PlatformPort is the platform's own port, the one thing
	// of the host's a namespaced workload may reach; BridgeCIDR is the range
	// the platform remembers or AGENT_BRIDGE_CIDR names; BlockMetadata is
	// BLOCK_AWS_METADATA.
	Namespaces    bool
	PlatformPort  int
	PlatformHost  string
	BridgeCIDR    string
	BlockMetadata bool
	// BridgeName and ChainPrefix name the bridge and the agent's iptables
	// chains: toft0 and TOFT, and something else only in a test.
	BridgeName  string
	ChainPrefix string
	// ResolvConf and UpstreamResolvConf are the host's resolver files the
	// namespace tier reads to decide a workload's DNS (dns_linux.go):
	// /etc/resolv.conf and systemd-resolved's upstream list, and something
	// else only in a test.
	ResolvConf         string
	UpstreamResolvConf string
}

// flags are the command-line form of the configuration the platform starts an
// agent with, which a re-exec passes on unchanged.
func (c *Config) flags() []string {
	var out []string
	if c.CgroupParent != "" {
		out = append(out, "--cgroup-parent", c.CgroupParent)
	}
	if c.Namespaces {
		out = append(out, "--namespaces", "--platform-port", strconv.Itoa(c.PlatformPort))
		if c.PlatformHost != "" {
			out = append(out, "--platform-host", c.PlatformHost)
		}
	}
	if c.BridgeCIDR != "" {
		out = append(out, "--bridge-cidr", c.BridgeCIDR)
	}
	if c.BlockMetadata {
		out = append(out, "--block-metadata")
	}
	return out
}

func (c *Config) defaults() error {
	if c.Dir == "" {
		c.Dir = DefaultDir
	}
	if c.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("finding the binary to run workloads with: %w", err)
		}
		c.Exe = strings.TrimSuffix(exe, " (deleted)")
	}
	if c.Args == nil {
		c.Args = []string{"runner"}
	}
	if c.ShimArgs == nil {
		c.ShimArgs = []string{ShimCommand}
	}
	if c.StopGrace == 0 {
		c.StopGrace = 10 * time.Second
	}
	if c.Backoff == (Backoff{}) {
		c.Backoff = DefaultBackoff
	}
	if c.WatchInterval == 0 {
		c.WatchInterval = 100 * time.Millisecond
	}
	if c.BridgeName == "" {
		c.BridgeName = "toft0"
	}
	if c.ChainPrefix == "" {
		c.ChainPrefix = "TOFT"
	}
	return nil
}

// Agent is the daemon.
type Agent struct {
	cfg       Config
	startedAt time.Time

	// names serialises start, stop and delete per name (holdName): two in
	// flight for one name each replaced and spawned, and the loser's process
	// was supervised and restarted forever with nothing holding it.
	names sync.Map // string → *sync.Mutex

	mu        sync.Mutex
	workloads map[string]*workload
	// tokens maps a live identity token's digest to the workload holding it.
	tokens map[string]string
	subs   map[*subscriber]struct{}

	tree *procstat.TreeSampler

	// cg holds memory limits in the kernel where the machine allows it; nil
	// keeps the watchdog, and cgReason says why.
	cg       *cgroups
	cgReason string

	// net is the namespace tier, nil below it, and nsReason why. addrs is every
	// workload's address in the bridge's range, kept by name; stateBridge is
	// the range a previous agent's state recorded.
	net *netConfig
	// fwd relays a namespaced workload's DNS when nothing names an upstream
	// it can reach (dns_linux.go); nil otherwise.
	fwd         *dnsForwarder
	nsReason    string
	addrs       map[string]netip.Addr
	stateBridge string

	stateMu sync.Mutex
	lock    *os.File
	ln      net.Listener
	srv     *http.Server
	// reexecing is set while a re-exec is under way, so Serve's return is not
	// taken for the agent's end; reexecFailed says the exec did not happen.
	reexecing    atomic.Bool
	reexecFailed chan struct{}
}

// New prepares an agent over cfg's directory: it takes the directory's lock, so
// a second agent for the same install refuses rather than racing the first, and
// adopts whatever the state left behind says is still running.
func New(cfg Config) (*Agent, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	// 0711: the identity files under it are read by app accounts, which may
	// pass through and may not list. The socket and the state are 0600.
	if err := os.MkdirAll(cfg.Dir, 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Dir, 0o711); err != nil {
		return nil, err
	}
	for _, sub := range []string{"id", "out"} {
		mode := os.FileMode(0o711)
		if sub == "out" {
			mode = 0o700
		}
		if err := os.MkdirAll(filepath.Join(cfg.Dir, sub), mode); err != nil {
			return nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(cfg.Dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another agent holds %s: %w", cfg.Dir, err)
	}
	a := &Agent{
		cfg:       cfg,
		startedAt: time.Now().UTC(),
		workloads: map[string]*workload{},
		tokens:    map[string]string{},
		subs:      map[*subscriber]struct{}{},
		tree:      procstat.NewTreeSampler(),
		lock:      lock,

		reexecFailed: make(chan struct{}, 1),
		addrs:        map[string]netip.Addr{},
	}
	if cg, err := newCgroups(cfg.CgroupParent); err != nil {
		a.cgReason = err.Error()
		logf("memory limits are sampled, not held by the kernel: %v", err)
	} else {
		a.cg = cg
	}
	a.readStateNetwork()
	if a.cg == nil {
		a.nsReason = "it needs the cgroup tier first"
	} else if n, err := a.detectNamespaces(); err != nil {
		a.nsReason = err.Error()
	} else {
		a.net = n
		n.dns = a.startDNS(n, n.plan)
		logf("namespace tier: bridge %s on %s, gateway %s, DNS %s", n.bridge, n.prefix, n.gateway, n.dns)
	}
	a.restore()
	return a, nil
}

// Serve answers the socket until ctx ends. The workloads outlive it: an agent
// that exits leaves every app running, for the next agent to adopt.
func (a *Agent) Serve(ctx context.Context) error {
	sock := SocketPath(a.cfg.Dir)
	ln, err := listenPrivately(sock)
	if err != nil {
		return err
	}
	// The listener unlinked its socket on close when it was bound in place;
	// bound elsewhere and moved, the path is removed here instead.
	defer os.Remove(sock)
	a.ln = ln
	a.srv = &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		a.srv.Close()
	}()
	err = a.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listenPrivately binds the agent's socket at sock with no instant it can be
// connected to by anybody else. The directory is 0711, so its identity files
// can be reached by app accounts, and a socket bound there in place is created
// 0777 less the umask and only then chmodded: an app account connecting in that
// window held a connection that could start a runner as any account it named.
// So it is bound in a 0700 directory of its own, made 0600 there, and renamed
// into place — the path never names a socket that is not already root's alone.
func listenPrivately(sock string) (net.Listener, error) {
	dir, err := os.MkdirTemp(filepath.Dir(sock), ".bind")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Rename(tmp, sock); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ServeUntilDone is Serve for the daemon: it waits out a re-exec — which
// closes the listener and, when it succeeds, never returns — and answers again
// if the exec failed.
func (a *Agent) ServeUntilDone(ctx context.Context) error {
	for {
		err := a.Serve(ctx)
		if !a.reexecing.Load() {
			return err
		}
		<-a.reexecFailed
		a.reexecing.Store(false)
	}
}

// Close stops answering and, with kill, stops every workload too — which only
// a test wants: the daemon's own shutdown leaves the estate running.
func (a *Agent) Close(kill bool) {
	if a.srv != nil {
		a.srv.Close()
	}
	if kill {
		a.mu.Lock()
		names := make([]string, 0, len(a.workloads))
		for n := range a.workloads {
			names = append(names, n)
		}
		a.mu.Unlock()
		for _, n := range names {
			a.stop(n, true)
		}
	}
	a.mu.Lock()
	outs := make([]*output, 0, len(a.workloads))
	for _, w := range a.workloads {
		outs = append(outs, w.out)
	}
	a.mu.Unlock()
	// Outside the lock: a reader finishing its last line publishes, and
	// publishing takes it.
	for _, o := range outs {
		o.close()
	}
	if a.fwd != nil {
		a.fwd.close()
	}
	if a.lock != nil {
		a.lock.Close()
	}
}

func (a *Agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workloads/{name}", a.handleStart)
	mux.HandleFunc("POST /workloads/{name}/stop", a.handleStop)
	mux.HandleFunc("DELETE /workloads/{name}", a.handleDelete)
	mux.HandleFunc("GET /workloads", a.handleList)
	mux.HandleFunc("GET /stats", a.handleStats)
	mux.HandleFunc("GET /workloads/{name}", a.handleGet)
	mux.HandleFunc("GET /workloads/{name}/follow", a.handleFollow)
	mux.HandleFunc("GET /follow", a.handleFollow)
	mux.HandleFunc("POST /identity/verify", a.handleVerify)
	gated := protocolGate(mux)
	outer := http.NewServeMux()
	outer.HandleFunc("POST /reexec", a.handleReexec) // any protocol: see reexec_linux.go
	outer.Handle("/", gated)
	return outer
}

// protocolGate answers every request with this agent's protocol, and refuses
// one that names another: a platform speaking a different protocol must
// replace the agent rather than half-talk to it.
func protocolGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ProtocolHeader, ProtocolVersion)
		if v := r.Header.Get(ProtocolHeader); v != "" && v != ProtocolVersion {
			writeJSON(w, http.StatusConflict, errorBody{
				Error:    fmt.Sprintf("this agent speaks protocol %s, the caller %s", ProtocolVersion, v),
				Protocol: ProtocolVersion,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Agent) handleStart(w http.ResponseWriter, r *http.Request) {
	var spec Spec
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unreadable description: " + err.Error()})
		return
	}
	if name := r.PathValue("name"); spec.Name == "" {
		spec.Name = name
	} else if spec.Name != name {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "the description names another workload"})
		return
	}
	if err := validSpec(spec); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	st, err := a.start(spec)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *Agent) handleStop(w http.ResponseWriter, r *http.Request) {
	if !a.stop(r.PathValue("name"), false) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such workload"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !a.delete(r.PathValue("name")) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such workload"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.list())
}

func (a *Agent) handleGet(w http.ResponseWriter, r *http.Request) {
	st := a.status(r.PathValue("name"))
	if st.Name == "" {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such workload"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *Agent) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req VerifyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Token == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "no token"})
		return
	}
	resp, ok := a.verify(req.Token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not a live workload's identity"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// validSpec refuses a description the agent cannot place safely.
func validSpec(s Spec) error {
	if s.Name == "" || strings.ContainsAny(s.Name, `/\`) || s.Name == "." || s.Name == ".." {
		return fmt.Errorf("invalid workload name %q", s.Name)
	}
	if s.Kind != KindService && s.Kind != KindRun {
		return fmt.Errorf("invalid workload kind %q", s.Kind)
	}
	if s.Restart != RestartAlways && s.Restart != RestartNever {
		return fmt.Errorf("invalid restart policy %q", s.Restart)
	}
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("the working directory must be absolute, got %q", s.Dir)
	}
	if s.LogPath != "" && !filepath.IsAbs(s.LogPath) {
		return fmt.Errorf("the log path must be absolute, got %q", s.LogPath)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

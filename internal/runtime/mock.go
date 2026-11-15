package runtime

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type MockRuntime struct {
	// allocator is nil in tests that only need a process registered — with no
	// allocator there is no port and no listener, which is what lets a log or
	// authorization test skip standing up a backend.
	allocator PortAllocator
	resolver  ReleaseResolver

	mu    sync.RWMutex
	procs map[string]*mockProcess
}

type mockProcess struct {
	spec      WorkloadSpec
	endpoint  Endpoint
	status    string
	startedAt time.Time
	restarts  int
	looping   bool
	server    *http.Server
	// listener is held beside the server because closing it is what actually
	// frees the address, and it has to happen *before* Deploy binds the
	// replacement. http.Server.Close only closes listeners Serve has already
	// registered, and Serve runs on its own goroutine — so a replacement fast
	// enough to beat it found the port still held.
	listener net.Listener
}

func NewMockRuntime(allocator PortAllocator) *MockRuntime {
	return &MockRuntime{
		allocator: allocator,
		resolver:  LocalResolver{},
		procs:     make(map[string]*mockProcess),
	}
}

// Deploy starts a placeholder HTTP server on a port it allocates itself, and
// reports the endpoint. Serving real traffic keeps the mock honest: the
// readiness gate and the reverse proxy exercise the same code paths they do
// against a real runtime.
func (m *MockRuntime) Deploy(spec WorkloadSpec) (Endpoint, error) {
	// The mock stands in for the shared backend, so it must be no more capable
	// than the thing it stands in for — a mock that accepted mounts would let a
	// spec that fails in production pass in development.
	if err := RejectMounts(spec); err != nil {
		return Endpoint{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	name := spec.ProcessName()

	restarts := 0
	endpoint := Endpoint{}
	if proc, exists := m.procs[name]; exists {
		m.shutdown(proc)
		restarts = proc.restarts + 1
		// Redeploying a known name keeps its endpoint, matching the shared
		// backend: a caller that persisted the address stays correct.
		endpoint = proc.endpoint
	}

	if endpoint.IsZero() && m.allocator != nil {
		port, err := m.allocator.Allocate()
		if err != nil {
			return Endpoint{}, fmt.Errorf("mock runtime allocating port: %w", err)
		}
		endpoint = Endpoint{Host: BindLoopback, Port: port}
	}

	server, ln, err := m.listen(spec, endpoint)
	if err != nil {
		if m.allocator != nil && !endpoint.IsZero() {
			m.allocator.Release(endpoint.Port)
		}
		return Endpoint{}, err
	}

	m.procs[name] = &mockProcess{
		spec:      spec,
		endpoint:  endpoint,
		status:    "online",
		startedAt: time.Now(),
		restarts:  restarts,
		server:    server,
		listener:  ln,
	}
	return endpoint, nil
}

func (m *MockRuntime) listen(spec WorkloadSpec, ep Endpoint) (*http.Server, net.Listener, error) {
	if ep.Port == 0 {
		return nil, nil, nil
	}

	ln, err := net.Listen("tcp", ep.Addr())
	if err != nil {
		return nil, nil, fmt.Errorf("mock runtime listening on %s: %w", ep.Addr(), err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<!doctype html><title>%s</title><h1>%s</h1>"+
				"<p>Mock runtime. Base path: <code>%s</code>. Request path: <code>%s</code>.</p>",
				spec.Slug, spec.Slug, spec.BasePath, r.URL.Path)
		}),
	}
	go server.Serve(ln)
	return server, ln, nil
}

func (m *MockRuntime) shutdown(proc *mockProcess) {
	if proc.server != nil {
		proc.server.Close()
		proc.server = nil
	}
	// The listener, explicitly and second. Close above only reaches listeners
	// Serve has registered, which is a goroutine's worth of scheduling away
	// from where the address is actually taken.
	if proc.listener != nil {
		proc.listener.Close()
		proc.listener = nil
	}
	proc.status = "stopped"
}

func (m *MockRuntime) Stop(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, exists := m.procs[name]
	if !exists {
		// The third answer this used to give was the only one the API tests
		// ever saw, which is how the two real backends disagreed for as long
		// as both existed. It says what they say now.
		return Absent(LogAbsentNoWorkload)
	}
	m.shutdown(proc)
	return nil
}

func (m *MockRuntime) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if proc, exists := m.procs[name]; exists {
		m.shutdown(proc)
		if m.allocator != nil && !proc.endpoint.IsZero() {
			m.allocator.Release(proc.endpoint.Port)
		}
	}
	delete(m.procs, name)
	return nil
}

func (m *MockRuntime) Logs(name string, lines int) ([]LogLine, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Absence is not a failure. This used to be a third answer beside the two
	// real backends' — PM2, as the shared backend was then, returning success
	// with an empty tail, the cluster
	// returning an error — which is how the disagreement stayed hidden: the
	// API tests only ever saw the mock's.
	if _, exists := m.procs[name]; !exists {
		return nil, Absent(LogAbsentNoWorkload)
	}

	now := time.Now()
	out := make([]LogLine, 0, lines)
	for i := 0; i < lines; i++ {
		at := now
		out = append(out, LogLine{
			At: &at, Source: LogSourceApp, Stream: LogStreamStdout,
			Line: fmt.Sprintf("%s - mock log line %d", name, i+1),
		})
	}
	return out, nil
}

func (m *MockRuntime) LogStream(name string) (LogReader, error) {
	m.mu.RLock()
	if _, exists := m.procs[name]; !exists {
		m.mu.RUnlock()
		return nil, Absent(LogAbsentNoWorkload)
	}
	m.mu.RUnlock()

	return &mockLogReader{name: name, done: make(chan struct{})}, nil
}

// mockLogReader emits a line every couple of seconds, as the pipe-and-goroutine
// version it replaced did, minus the goroutine that outlived a closed reader.
type mockLogReader struct {
	name string
	sent int
	done chan struct{}
}

func (r *mockLogReader) Next() (LogLine, error) {
	if r.sent >= 100 {
		return LogLine{}, io.EOF
	}
	select {
	case <-r.done:
		return LogLine{}, io.EOF
	case <-time.After(2 * time.Second):
	}
	r.sent++
	at := time.Now()
	return LogLine{
		At: &at, Source: LogSourceApp, Stream: LogStreamStdout,
		Line: fmt.Sprintf("%s - log line %d", r.name, r.sent),
	}, nil
}

func (r *mockLogReader) Close() error {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	return nil
}

// CrashLoop puts a workload where the agent holds one between restarts of a
// loop: errored, with restarts counted and the backend's own verdict set.
func (m *MockRuntime) CrashLoop(name string, restarts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if proc, ok := m.procs[name]; ok {
		proc.status, proc.restarts, proc.looping = StatusErrored, restarts, true
	}
}

func (m *MockRuntime) Metrics(name string) (*AppMetrics, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	proc, exists := m.procs[name]
	if !exists {
		return nil, fmt.Errorf("process %s not found", name)
	}

	return m.reading(proc), nil
}

// reading is what this mock says about one process, and it is the one place
// the shared backend's shape is imitated.
//
// Both figures are answered for a workload that is up, deliberately: this mock
// stands in for the shared backend, and one that returned nil would leave the
// measured case untested everywhere while pretending to cover the unmeasured
// one. Neither is answered for one that is not, for the same reason
// AgentRuntime answers neither — there is no pid to read /proc for, and 2.5%
// attributed to a process that does not exist is the reassuring half of a bug
// rather than a gap in coverage. The uptime goes with them: an entry the agent
// keeps `stopped` so a later start finds its port still remembers when the
// process that is gone began.
func (m *MockRuntime) reading(proc *mockProcess) *AppMetrics {
	out := &AppMetrics{Status: proc.status, Restarts: proc.restarts, CrashLooping: proc.looping}
	if !WorkloadIsUp(proc.status) {
		return out
	}
	out.CPU, out.Memory = Measured(2.5), Measured(64.0)
	out.Uptime = int64(time.Since(proc.startedAt).Seconds())
	out.PID = 12345
	return out
}

func (m *MockRuntime) SystemMetrics() (*SystemMetrics, error) {
	return getSystemMetrics()
}

func (m *MockRuntime) List() (map[string]*AppMetrics, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Every process this mock holds, stopped ones included — `Stop` keeps the
	// entry the way the agent keeps a stopped workload, and a mock that dropped
	// it would hide from
	// every API test the one case that only shared mode produces.
	result := make(map[string]*AppMetrics)
	for name, proc := range m.procs {
		result[name] = m.reading(proc)
	}
	return result, nil
}

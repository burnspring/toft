//go:build linux

package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"toft/internal/agent"
	"toft/internal/runner"
)

// The shared backend over a real agent on a temporary socket, whose runner is
// this test binary: TestAgentFakeRunner, which skips unless the agent started
// it, is the runner's stand-in — it listens where the placer said, dumps the
// environment it was given and prints a marked line.

func TestAgentFakeRunner(t *testing.T) {
	if os.Getenv(runner.EnvMode) == "" {
		t.Skip("the agent's stand-in runner; it only runs when an agent starts it")
	}
	env := strings.Join(os.Environ(), "\n")
	os.WriteFile(filepath.Join(os.Getenv(runner.EnvWorkDir), "runner.env"), []byte(env), 0o600)
	fmt.Println(runner.LogMarker(time.Now(), runner.LogSourceApp, "stdout") + "hello from " + os.Getenv("TOFT_WORKLOAD_NAME"))
	fmt.Println(runner.LogMarker(time.Now(), runner.LogSourcePlatform, "stdout") + "==> the runner speaking")
	ln, err := net.Listen("tcp", net.JoinHostPort(os.Getenv(runner.EnvBind), os.Getenv(runner.EnvPort)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	<-sigs
	os.Exit(0)
}

// TestAgentShim is the namespace tier's shim, run by an agent at that tier in
// this test binary; it skips otherwise.
func TestAgentShim(t *testing.T) {
	if os.Getenv("TOFT_AGENT_SHIM") == "" {
		t.Skip("the agent's shim; it only runs when an agent at the namespace tier starts it")
	}
	if err := agent.ShimMain(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type agentFixture struct {
	rt      *AgentRuntime
	agent   *agent.Agent
	dir     string
	appsDir string
}

func newAgentFixture(t *testing.T, mutate ...func(*agent.Config)) *agentFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "ar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "agent")
	appsDir := filepath.Join(root, "apps")
	logsDir := filepath.Join(root, "logs")
	for _, d := range []string{appsDir, logsDir} {
		os.MkdirAll(d, 0o755)
	}
	cfg := agent.Config{
		Dir: dir, Exe: exe, Args: []string{"-test.run=^TestAgentFakeRunner$"},
		ShimArgs:  []string{"-test.run=^TestAgentShim$"},
		StopGrace: 2 * time.Second, CgroupParent: os.Getenv("TOFT_TEST_CGROUP_PARENT"),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { a.Serve(ctx); close(served) }()
	t.Cleanup(func() { cancel(); <-served; a.Close(true) })

	rt := NewAgentRuntime(Isolation{}, LocalResolver{}, &stubAllocator{next: freePort(t)}, appsDir, dir).
		WithLogsDir(logsDir).WithCallbackURL("http://127.0.0.1:1").WithMaskDirs(appsDir, logsDir)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := rt.client.List(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// What EnsureAgent would have read off a started agent.
	l, _ := rt.client.List(context.Background())
	rt.tier, rt.bridge, rt.gateway = l.Tier, l.Bridge, l.Gateway
	return &agentFixture{rt: rt, agent: a, dir: dir, appsDir: appsDir}
}

// freePort is a port nothing listens on, as the base of the stub's range.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func (f *agentFixture) spec(t *testing.T, appID string) WorkloadSpec {
	release := filepath.Join(f.appsDir, appID, "releases", "dep-1234567890")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	return WorkloadSpec{
		AppID: appID, Slug: "demo", Name: "demo-dep12345", Framework: "python",
		Release: LocalRelease(release), BasePath: "/apps/demo",
		Env: map[string]string{"SECRET": "canary-7f3e1d"},
	}
}

func waitServing(t *testing.T, ep Endpoint) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(ep.URL()); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing answered on %s", ep.URL())
}

func TestTheAgentRuntimeSatisfiesTheSeam(t *testing.T) {
	var _ ServiceRuntime = (*AgentRuntime)(nil)
	var _ IdentityVerifier = (*AgentRuntime)(nil)
}

// A deploy places the runner where the backend allocated, on loopback, with
// the runner's inputs and the process tier's consumption rule — and the
// workload answers there.
func TestAnAgentDeployServesOnTheAllocatedLoopbackPort(t *testing.T) {
	t.Setenv("TOFT_NODE_PATH", "/opt/node/20/bin/node")
	f := newAgentFixture(t)
	spec := f.spec(t, "app-1")
	ep, err := f.rt.Deploy(spec)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if ep.Host != BindLoopback {
		t.Errorf("endpoint host %q, want loopback", ep.Host)
	}
	waitServing(t, ep)

	env, _ := os.ReadFile(filepath.Join(f.appsDir, "app-1", "runner.env"))
	for _, want := range []string{
		fmt.Sprintf("%s=%d", runner.EnvPort, ep.Port),
		runner.EnvBind + "=127.0.0.1",
		runner.EnvMode + "=serve",
		runner.EnvReleaseDir + "=" + spec.Release.Value,
		runner.EnvIdentityFile + "=",
		// The operator's interpreter declarations, or a release pinned below
		// the default runs the default (LANG.15): the runner resolves them.
		"TOFT_NODE_PATH=/opt/node/20/bin/node",
		// The app's scratch, wiped at every start, which the runner points
		// TMPDIR and TOFT_SCRATCH_DIR at — beside the address, so an install
		// without per-app accounts still gets its apps off the platform's TMPDIR.
		runner.EnvScratchPath + "=" + ScratchDir(f.appsDir, "app-1"),
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("the runner's environment lacks %s", want)
		}
	}

	// One owner of the figures (ADR §132): in a cgroup of its own the runner
	// reports, and without one it is told the platform measures.
	st, _ := f.rt.statusOf(spec.Name)
	if hostMeasured := strings.Contains(string(env), runner.EnvConsumption+"="+runner.ConsumptionByHost); hostMeasured == (st.Cgroup != "") {
		t.Errorf("cgroup %q, and TOFT_CONSUMPTION=host told: %v — exactly one of them must measure", st.Cgroup, hostMeasured)
	}

	m, err := f.rt.Metrics(spec.Name)
	if err != nil || m.Status != StatusOnline || m.PID <= 0 {
		t.Fatalf("metrics: %+v, %v", m, err)
	}
	var lines []LogLine
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		lines, _ = f.rt.Logs(spec.Name, 50)
		if len(lines) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var app, platform bool
	for _, l := range lines {
		if l.Line == "hello from demo-dep12345" && l.Source == LogSourceApp && l.At != nil {
			app = true
		}
		if strings.Contains(l.Line, "the runner speaking") && l.Source == LogSourcePlatform {
			platform = true
		}
	}
	if !app || !platform {
		t.Errorf("logs did not carry both voices, labelled: %+v", lines)
	}
}

// ADR §129: no secret reaches the agent. The app's environment travels
// at bootstrap, from the platform to the runner; the description holds none of
// it, so neither does anything the agent writes down — nor the runner's own
// environment, which the agent built.
func TestNoSecretReachesTheAgent(t *testing.T) {
	f := newAgentFixture(t)
	spec := f.spec(t, "app-1")
	ep, err := f.rt.Deploy(spec)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	waitServing(t, ep)

	for _, path := range []string{
		filepath.Join(f.dir, "state.json"),
		agent.LogPath(f.dir),
		filepath.Join(f.appsDir, "app-1", "runner.env"),
	} {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "canary-7f3e1d") {
			t.Errorf("the app's secret reached %s", path)
		}
	}
	state, _ := os.ReadFile(filepath.Join(f.dir, "state.json"))
	if !strings.Contains(string(state), spec.Name) {
		t.Fatal("the state does not describe the workload, so the check above checked nothing")
	}
}

// A redeploy of a name keeps its address; Delete gives it back.
func TestAnAgentRedeployKeepsTheAddressAndDeleteReleasesIt(t *testing.T) {
	f := newAgentFixture(t)
	alloc := f.rt.allocator.(*stubAllocator)
	spec := f.spec(t, "app-1")
	first, err := f.rt.Deploy(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitServing(t, first)
	second, err := f.rt.Deploy(spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("address moved across a replacement: %v -> %v", first, second)
	}
	waitServing(t, second)

	if err := f.rt.Stop(spec.Name); err != nil {
		t.Fatal(err)
	}
	if m, err := f.rt.Metrics(spec.Name); err != nil || m.Status != StatusStopped {
		t.Errorf("after stop: %+v, %v", m, err)
	}
	if err := f.rt.Delete(spec.Name); err != nil {
		t.Fatal(err)
	}
	if len(alloc.released) != 1 || alloc.released[0] != first.Port {
		t.Errorf("released %v, want [%d]", alloc.released, first.Port)
	}
	if _, err := f.rt.Logs(spec.Name, 10); AbsenceReason(err) != LogAbsentNoWorkload {
		t.Errorf("logs of a deleted workload: %v, want an absence", err)
	}
	if err := f.rt.Stop(spec.Name); AbsenceReason(err) != LogAbsentNoWorkload {
		t.Errorf("stop of a deleted workload: %v, want an absence", err)
	}
}

// The platform verifies a shared workload's identity by asking the agent, and
// learns the app and workload from the agent's own record.
func TestTheAgentRuntimeVerifiesALiveWorkloadsIdentity(t *testing.T) {
	f := newAgentFixture(t)
	spec := f.spec(t, "app-1")
	ep, err := f.rt.Deploy(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitServing(t, ep)
	tok, err := os.ReadFile(filepath.Join(f.dir, "id", spec.Name, "token"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.rt.VerifyWorkloadIdentity(context.Background(), strings.TrimSpace(string(tok)))
	if err != nil || id.AppID != "app-1" || id.WorkloadName != spec.Name || id.RunID != "" {
		t.Errorf("identity: %+v, %v", id, err)
	}
	if err := f.rt.Delete(spec.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rt.VerifyWorkloadIdentity(context.Background(), strings.TrimSpace(string(tok))); err == nil {
		t.Error("a deleted workload's identity still verifies")
	}
}

// The deployment a release belongs to is read off its directory, for the
// output's labels; any other shape is no answer rather than a guess.
func TestReleaseDeployment(t *testing.T) {
	for dir, want := range map[string]string{
		"/apps/a1/releases/dep-1":   "dep-1",
		"/apps/a1/releases/dep-1/x": "",
		"/apps/a1/current":          "",
		"/apps/a2/releases/dep-1":   "",
	} {
		if got := releaseDeployment("/apps", "a1", dir); got != want {
			t.Errorf("releaseDeployment(%q) = %q, want %q", dir, got, want)
		}
	}
}

// At the namespace tier a workload's endpoint is its own address on the one
// workload port, which the platform reaches from the host — the proxy's path —
// and nothing comes from the port range. Root only (it builds a bridge, under
// a test name of its own); run as the agent package's namespace test is.
func TestANamespacedDeployServesOnItsOwnAddress(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("TOFT_TEST_CGROUP_PARENT") == "" {
		t.Skip("needs root and TOFT_TEST_CGROUP_PARENT: it builds a bridge and iptables chains")
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	f := newAgentFixture(t, func(c *agent.Config) {
		c.Namespaces, c.PlatformPort = true, port
		c.BridgeName, c.ChainPrefix, c.BridgeCIDR = "toftr0", "TOFTR", "10.248.0.0/24"
	})
	t.Cleanup(func() {
		for _, args := range [][]string{
			{"iptables", "-D", "FORWARD", "-j", "TOFTR-FWD"}, {"iptables", "-F", "TOFTR-FWD"}, {"iptables", "-X", "TOFTR-FWD"},
			{"iptables", "-D", "INPUT", "-j", "TOFTR-IN"}, {"iptables", "-F", "TOFTR-IN"}, {"iptables", "-X", "TOFTR-IN"},
			{"iptables", "-t", "nat", "-D", "POSTROUTING", "-j", "TOFTR-NAT"}, {"iptables", "-t", "nat", "-F", "TOFTR-NAT"},
			{"iptables", "-t", "nat", "-X", "TOFTR-NAT"}, {"ip", "link", "del", "toftr0"},
		} {
			exec.Command(args[0], args[1:]...).Run()
		}
	})
	if ns, gw := f.rt.namespaced(); !ns || gw != "10.248.0.1" {
		t.Fatalf("not at the namespace tier: %v %q", ns, gw)
	}
	alloc := f.rt.allocator.(*stubAllocator)
	before := alloc.next

	spec := f.spec(t, "app-1")
	ep, err := f.rt.Deploy(spec)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if ep.Port != agent.WorkloadPort || !strings.HasPrefix(ep.Host, "10.248.0.") {
		t.Errorf("endpoint %v, want an address in the bridge's range on %d", ep, agent.WorkloadPort)
	}
	if alloc.next != before {
		t.Error("a namespaced deploy took a port from the range")
	}
	waitServing(t, ep)

	env, _ := os.ReadFile(filepath.Join(f.appsDir, "app-1", "runner.env"))
	if want := fmt.Sprintf("%s=http://10.248.0.1:1", runner.EnvCallbackURL); !strings.Contains(string(env), want) {
		t.Errorf("the runner was not told the platform's address on the gateway (%s)", want)
	}
	if !strings.Contains(string(env), runner.EnvBind+"=0.0.0.0") {
		t.Error("a namespaced workload was told to bind loopback, which nothing outside its namespace reaches")
	}
	if err := f.rt.Delete(spec.Name); err != nil {
		t.Fatal(err)
	}
	if len(alloc.released) != 0 {
		t.Errorf("a namespaced workload's delete released %v to the port range", alloc.released)
	}
}

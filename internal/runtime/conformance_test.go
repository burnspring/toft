package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This is the conformance suite: the contract every execution backend must
// satisfy, written to describe the *contract* rather than any one backend's
// behaviour. It is the only thing that can hold two isolated implementations
// at parity when one of them cannot be run locally, which is why it is a
// product artifact and not test scaffolding.
//
// The rules below are deliberately asserted against the **shared** backend,
// because shared mode is accidentally *more* capable than isolated mode and is
// therefore the implementation that would otherwise pass by being more
// permissive than the contract allows. Constraining it down to the
// intersection is what keeps mode selection a knob rather than a one-way door.

// stubAllocator hands out a fixed sequence so a test can assert which address
// a workload was given without standing up a real allocator.
type stubAllocator struct {
	next     int
	released []int
	fail     error
}

func (s *stubAllocator) Allocate() (int, error) {
	if s.fail != nil {
		return 0, s.fail
	}
	if s.next == 0 {
		s.next = 4000
	}
	p := s.next
	s.next++
	return p, nil
}

func (s *stubAllocator) Release(port int) { s.released = append(s.released, port) }

// --- The seam ---

// A caller hands over a spec and receives an address. It never picks one, and
// it must not be able to: a Port back on the spec is how "an app is a port on
// this machine" gets reintroduced one caller at a time.
//
// BindAddress is deliberately allowed. It selects loopback versus all
// interfaces, which is a property of the execution *mode*, not a location the
// caller is choosing.
func TestSpecCannotCarryAnAddress(t *testing.T) {
	allowed := map[string]bool{"BindAddress": true}
	banned := []string{"port", "host", "addr", "endpoint", "url", "ip"}

	specType := reflect.TypeOf(WorkloadSpec{})
	for i := 0; i < specType.NumField(); i++ {
		name := specType.Field(i).Name
		if allowed[name] {
			continue
		}
		lower := strings.ToLower(name)
		for _, b := range banned {
			if strings.Contains(lower, b) {
				t.Errorf("WorkloadSpec.%s lets a caller choose an address; "+
					"the backend returns an Endpoint instead", name)
			}
		}
	}
}

func TestReleaseReferenceIsOpaqueUntilABackendResolvesIt(t *testing.T) {
	ref := LocalRelease("/srv/apps/demo/releases/dep1")

	if ref.Kind != ReleaseLocal {
		t.Errorf("kind = %q, want %q", ref.Kind, ReleaseLocal)
	}
	// Round-trips through persistence without losing which kind it is.
	if got := ParseReleaseRef(ref.String()); got != ref {
		t.Errorf("round trip: got %+v, want %+v", got, ref)
	}

	dir, err := LocalResolver{}.Resolve(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dir != "/srv/apps/demo/releases/dep1" {
		t.Errorf("resolved to %q", dir)
	}
}

// A reference persisted before the artifact store existed is a bare path.
// Reading it as a local release is what keeps an upgrade from orphaning every
// running app.
func TestBareReleaseReferenceReadsAsLocal(t *testing.T) {
	got := ParseReleaseRef("/srv/apps/demo/releases/dep1")
	if got.Kind != ReleaseLocal || got.Value != "/srv/apps/demo/releases/dep1" {
		t.Errorf("got %+v, want a local reference", got)
	}
}

// A resolver that cannot fetch artifacts must say so rather than guess. On a
// VM install this is the error an operator sees if an artifact reference ever
// reaches them, and it names the missing capability.
func TestLocalResolverRefusesArtifactReferences(t *testing.T) {
	_, err := LocalResolver{}.Resolve(ArtifactRelease("apps/demo/dep1.tar.zst"))
	if !errors.Is(err, ErrUnresolvableRelease) {
		t.Fatalf("err = %v, want ErrUnresolvableRelease", err)
	}
	if !strings.Contains(err.Error(), "artifact store") {
		t.Errorf("error should name the missing capability, got: %v", err)
	}
}

func TestEndpointFormatting(t *testing.T) {
	ep := Endpoint{Host: "127.0.0.1", Port: 3100}
	if ep.Addr() != "127.0.0.1:3100" {
		t.Errorf("Addr() = %q", ep.Addr())
	}
	if ep.URL() != "http://127.0.0.1:3100" {
		t.Errorf("URL() = %q", ep.URL())
	}
	if (Endpoint{}).IsZero() != true {
		t.Error("zero endpoint should report IsZero")
	}

	// An isolated backend reports a task IP or service name, and the same
	// helpers must render it — the gateway has one code path, not two.
	svc := Endpoint{Host: "demo.apps.svc.cluster.local", Port: 8080}
	if svc.URL() != "http://demo.apps.svc.cluster.local:8080" {
		t.Errorf("URL() = %q", svc.URL())
	}
}

// --- Address ownership ---

// Redeploying a name the backend already knows keeps its address, so a caller
// that persisted the endpoint stays correct across a restart in place.
func TestRedeployKeepsTheSameAddress(t *testing.T) {
	alloc := &stubAllocator{}
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, alloc, "", "")

	first, _, err := p.bind("demo-abc123")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	second, fresh, err := p.bind("demo-abc123")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if fresh {
		t.Error("second bind of a known name should not take a new address")
	}
	if first != second {
		t.Errorf("address moved: %v -> %v", first, second)
	}
}

// Delete is the only thing that returns an address to the pool. A deploy that
// fails partway and a release that is retired both go through it, which is
// what stops the range leaking.
func TestDeleteReturnsTheAddress(t *testing.T) {
	alloc := &stubAllocator{}
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, alloc, "", "")

	ep, _, err := p.bind("demo-abc123")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	p.releaseBinding("demo-abc123")

	if len(alloc.released) != 1 || alloc.released[0] != ep.Port {
		t.Errorf("released = %v, want [%d]", alloc.released, ep.Port)
	}
	// And the name is free to take a different address afterwards.
	next, fresh, _ := p.bind("demo-abc123")
	if !fresh || next == ep {
		t.Errorf("after release the name should rebind fresh, got %v (fresh=%v)", next, fresh)
	}
}

// A restarted platform has no memory of the addresses it handed out, but the
// process manager is still running the workloads. Adopting them is what stops
// a later deploy being given an address something is already listening on.
func TestAdoptedAddressesAreNotReissued(t *testing.T) {
	alloc := &stubAllocator{next: 4000}
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, alloc, "", "")

	p.Adopt("survivor-abc123", Endpoint{Host: BindLoopback, Port: 4000})

	got, fresh, err := p.bind("survivor-abc123")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if fresh {
		t.Error("an adopted workload must not be given a second address")
	}
	if got.Port != 4000 {
		t.Errorf("port = %d, want the adopted 4000", got.Port)
	}
}

// Adoption must not overwrite an address the backend is actively using — the
// live binding is the truth, a persisted row is a memory of one.
func TestAdoptDoesNotOverwriteALiveBinding(t *testing.T) {
	alloc := &stubAllocator{next: 4000}
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, alloc, "", "")

	live, _, _ := p.bind("demo-abc123")
	p.Adopt("demo-abc123", Endpoint{Host: BindLoopback, Port: 9999})

	got, _, _ := p.bind("demo-abc123")
	if got != live {
		t.Errorf("adopt clobbered a live binding: %v -> %v", live, got)
	}
}

// --- The intersection rules ---

// Durable storage is an isolated-mode capability, and the shared backend must
// *refuse* a spec carrying it rather than start the workload without it.
//
// Ignoring the field is the failure that matters. An app told it has a durable
// volume, running as a shared process, would write into a release directory
// that the next deploy abandons and pruning deletes — data loss that looks
// exactly like a successful deploy until someone goes looking for the data.
func TestSharedModeRefusesDurableStorage(t *testing.T) {
	spec := WorkloadSpec{
		Slug: "demo", Name: "demo-abc123", Framework: "nextjs",
		Release: LocalRelease("/srv/apps/demo/releases/dep1"),
		Mounts: []Mount{
			// A volume: the mount that needs a root. A store is reached over
			// signed URLs and is described rather than refused — see
			// TestOnlyAMountThatNeedsARootIsRefused.
			{Name: "reports", Claim: "lpv-reports", Path: "/data/reports", Kind: MountVolume, Access: AccessRead},
		},
	}

	for name, rt := range map[string]ServiceRuntime{
		"agent": NewAgentRuntime(Isolation{}, LocalResolver{}, &stubAllocator{}, "", ""),
		"mock":  NewMockRuntime(&stubAllocator{}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rt.Deploy(spec)
			if !errors.Is(err, ErrStorageNotSupported) {
				t.Fatalf("err = %v, want ErrStorageNotSupported", err)
			}
			// The reader is looking at a deploy that used to work; the message
			// must say which of two things to change.
			for _, want := range []string{"demo-abc123", "reports", "isolated mode"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal should mention %q, got: %v", want, err)
				}
			}
		})
	}
}

// A spec with no mounts is the overwhelmingly common case and must not be
// disturbed by the check above.
func TestNoMountsIsNotARefusal(t *testing.T) {
	if err := RejectMounts(WorkloadSpec{Slug: "demo"}); err != nil {
		t.Fatalf("a spec with no mounts must deploy: %v", err)
	}
}

// TOFT_STORAGE is how app code finds a resource by name instead of
// hardcoding a path an administrator may move. It is rendered from the same
// slice the backend mounts from, so the description and the filesystem cannot
// disagree.
func TestMountsAreDeclaredNotDiscovered(t *testing.T) {
	got := DescribeMounts([]Mount{
		{Name: "cache", Path: "/data/cache", Kind: MountVolume, Access: AccessFull},
		// An unset access level reads as the least privileged one, never as
		// "unrestricted".
		{Name: "models", Path: "/data/models", Kind: MountStore},
	})

	var descs []MountDescriptor
	if err := json.Unmarshal([]byte(got), &descs); err != nil {
		t.Fatalf("TOFT_STORAGE must be valid JSON: %v (%s)", err, got)
	}
	if len(descs) != 2 {
		t.Fatalf("got %d descriptors, want 2", len(descs))
	}
	if descs[0].Name != "cache" || descs[0].Path != "/data/cache" || descs[0].Access != AccessFull {
		t.Errorf("first descriptor = %+v", descs[0])
	}
	if descs[1].Access != AccessRead {
		t.Errorf("an unset access level must read as %q, got %q", AccessRead, descs[1].Access)
	}

	// Empty renders as an empty array, not as the word "null": app code doing
	// JSON.parse on it must get something iterable.
	if DescribeMounts(nil) != "[]" {
		t.Errorf("no mounts should render as [], got %q", DescribeMounts(nil))
	}
}

// --- Mode dispatch ---

// A caller with an app in hand asks for a mode and gets the backend that owns
// that mode's workloads. Before this existed the platform held one backend, so
// promoting an app changed a row and left the app running as a process.
func TestRuntimesDispatchOnMode(t *testing.T) {
	shared := NewMockRuntime(nil)
	isolated := NewMockRuntime(nil)
	rts := NewRuntimes(shared, isolated)

	got, err := rts.For(ModeShared)
	if err != nil || got != ServiceRuntime(shared) {
		t.Errorf("shared dispatched to %v (err %v)", got, err)
	}
	got, err = rts.For(ModeIsolated)
	if err != nil || got != ServiceRuntime(isolated) {
		t.Errorf("isolated dispatched to %v (err %v)", got, err)
	}
	// An empty stored mode is shared, so rows written before modes existed need
	// no backfill here either.
	if got, err := rts.For(""); err != nil || got != ServiceRuntime(shared) {
		t.Errorf("empty mode dispatched to %v (err %v)", got, err)
	}
}

// An app row can say `isolated` on an install whose cluster configuration was
// removed. The deploy that discovers it must fail with an explanation naming
// what would fix it — not return a nil some caller dereferences later.
func TestIsolatedWithoutABackendIsAnExplainedError(t *testing.T) {
	rts := NewRuntimes(NewMockRuntime(nil), nil)

	if rts.IsolatedConfigured() {
		t.Error("no isolated backend was supplied")
	}
	rt, err := rts.For(ModeIsolated)
	if err == nil {
		t.Fatal("expected an error for isolated mode with no backend")
	}
	if rt != nil {
		t.Error("a failed lookup must not also return a backend")
	}
	for _, want := range []string{"KUBE_NAMESPACE", "shared mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("explanation should mention %q, got: %v", want, err)
		}
	}
}

// Whole-machine metrics must be expressible as "not available". A backend that
// answers with a zeroed struct instead would render as an idle, empty host.
func TestNotSupportedIsDistinctFromFailure(t *testing.T) {
	if errors.Is(errors.New("read failed"), ErrNotSupported) {
		t.Error("an ordinary failure must not read as ErrNotSupported")
	}
	// A backend signals absence by wrapping the sentinel.
	wrapped := errors.Join(ErrNotSupported, errors.New("isolated workloads have no host to report"))
	if !errors.Is(wrapped, ErrNotSupported) {
		t.Error("a wrapped sentinel must still be recognisable")
	}
}

// --- One verb starts a workload ---

// The seam offers exactly one way to start a workload, and this test is the
// specification of that rather than a restatement of it.
//
// `Restart(name, appID)` used to sit beside Deploy and could not be implemented
// correctly from its arguments in either backend: the isolated one cannot
// rebuild a pod spec from a name, since every part of one lives in the
// platform's database, so it deleted the pod, returned nil and started nothing;
// the shared one bounced the process
// and replayed the environment PM2 saved at creation, bringing the old
// configuration back under the new one's name. A name is not enough information
// to start anything correctly, so the interface no longer offers a method that
// takes only one.
//
// Written against the method set rather than against a list of forbidden names,
// because the failure this guards is somebody adding `Restart`, `Bounce` or
// `Reload` back for a caller that finds Deploy inconvenient.
func TestOnlyDeployStartsAWorkload(t *testing.T) {
	iface := reflect.TypeOf((*ServiceRuntime)(nil)).Elem()

	// Everything the seam may do. A method not on this list is either a start
	// verb in disguise or a capability that has not been argued for here.
	known := map[string]bool{
		"Deploy": true, "Stop": true, "Delete": true,
		"Logs": true, "LogStream": true,
		"Metrics": true, "SystemMetrics": true, "List": true,
	}
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		if !known[name] {
			t.Errorf("ServiceRuntime.%s is new: if it starts a workload, Deploy is "+
				"the only verb that may — see the interface comment on Deploy", name)
		}
	}

	// And Deploy takes the whole description, never a bare name.
	deploy, ok := iface.MethodByName("Deploy")
	if !ok {
		t.Fatal("ServiceRuntime has no Deploy")
	}
	if got := deploy.Type.In(0); got != reflect.TypeOf(WorkloadSpec{}) {
		t.Errorf("Deploy takes %v; it must take a WorkloadSpec, because a process "+
			"name does not decompose back into a workload", got)
	}
}

// The same rule, on the backend a test can run end to end: redeploying a name
// replaces the workload and the description it is running under, and keeps the
// address a caller has persisted.
func TestMockStartReplacesTheWorkloadAndKeepsTheAddress(t *testing.T) {
	m := NewMockRuntime(&stubAllocator{})

	first, err := m.Deploy(WorkloadSpec{
		Slug: "demo", Name: "demo-abc123",
		Env: map[string]string{"API_SECRET": "first"},
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	second, err := m.Deploy(WorkloadSpec{
		Slug: "demo", Name: "demo-abc123",
		Env: map[string]string{"API_SECRET": "second"},
	})
	if err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	if first != second {
		t.Errorf("address moved across a replacement: %v -> %v", first, second)
	}

	proc, ok := m.procs["demo-abc123"]
	if !ok {
		t.Fatal("no workload under the name after a replacement")
	}
	if got := proc.spec.Env["API_SECRET"]; got != "second" {
		t.Errorf("running spec carries API_SECRET=%q, want the current %q", got, "second")
	}
	if proc.status != "online" {
		t.Errorf("status after a replacement = %q, want online", proc.status)
	}
}

// --- Backends implement the same interface ---

func TestBackendsSatisfyTheServiceContract(t *testing.T) {
	var _ ServiceRuntime = (*AgentRuntime)(nil)
	var _ ServiceRuntime = (*MockRuntime)(nil)
}

// A backend that cannot place a workload reports it as a start rejection
// rather than returning a zero endpoint a caller might dial.
func TestNoAddressAvailableIsAnError(t *testing.T) {
	alloc := &stubAllocator{fail: errors.New("no available ports in range 3100-3200")}
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, alloc, "", "")

	_, _, err := p.bind("demo-abc123")
	if err == nil {
		t.Fatal("expected an error when no address can be allocated")
	}
	if !strings.Contains(err.Error(), "demo-abc123") {
		t.Errorf("error should name the workload, got: %v", err)
	}
}

// Deploy refuses a release it cannot resolve before it touches the process
// manager, and says which workload and why.
func TestDeployRejectsAnUnresolvableRelease(t *testing.T) {
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, &stubAllocator{}, "", "")

	_, err := p.Deploy(WorkloadSpec{Slug: "demo", Name: "demo-abc123", Framework: "nextjs"})
	if !errors.Is(err, ErrUnresolvableRelease) {
		t.Fatalf("err = %v, want ErrUnresolvableRelease", err)
	}
	if !strings.Contains(err.Error(), "demo-abc123") {
		t.Errorf("error should name the workload, got: %v", err)
	}
}

// --- Execution mode ---

// Shared is the default everywhere, so the common case — many small internal
// tools — is dense unless someone decides otherwise, and every topology
// behaves the same way out of the box.
func TestSharedIsTheDefaultInEveryTopology(t *testing.T) {
	for _, c := range []Capabilities{
		VMCapabilities(),
		{Topology: TopologyECS},
		{Topology: TopologyECS, IsolatedPossible: true, IsolatedEnabled: true, JobsEnabled: true},
		{Topology: TopologyKubernetes, IsolatedPossible: true, IsolatedEnabled: true, JobsEnabled: true},
	} {
		if !c.Allows(ModeShared) {
			t.Errorf("%s does not offer shared mode", c.Topology)
		}
	}

	// An empty stored value is shared, so rows written before modes existed
	// need no backfill.
	if m, err := ParseMode(""); err != nil || m != ModeShared {
		t.Errorf("ParseMode(\"\") = (%q, %v), want shared", m, err)
	}
}

// A VM install has no cluster to place a workload into, so isolated mode is
// permanently unavailable there. This is the one hard ceiling in the product
// and it must be stated, not discovered.
func TestVMInstallsCanNeverOfferIsolatedMode(t *testing.T) {
	c := VMCapabilities()

	if c.Allows(ModeIsolated) {
		t.Fatal("a VM install must not offer isolated mode")
	}
	if c.BackgroundJobsAvailable() {
		t.Error("a VM install must not offer background jobs")
	}

	why := c.ExplainUnavailable(ModeIsolated)
	if !strings.Contains(why, "single machine") || !strings.Contains(why, "cluster") {
		t.Errorf("the explanation should say why, got: %q", why)
	}
}

// Enabling isolated mode on ECS is an in-place upgrade, and the one-time
// rebuild of every app must be stated up front rather than discovered
// mid-change.
func TestECSExplainsTheRebuildBeforeIsolatedModeIsEnabled(t *testing.T) {
	// Infrastructure is in place, but nobody has turned it on yet.
	c := Capabilities{Topology: TopologyECS, IsolatedPossible: true}

	if c.Allows(ModeIsolated) {
		t.Fatal("isolated mode should be off until an administrator enables it")
	}
	why := c.ExplainUnavailable(ModeIsolated)
	if !strings.Contains(why, "every app must be rebuilt") {
		t.Errorf("the operator must be told about the one-time rebuild, got: %q", why)
	}
	// And told where the switch is, because this one is fixable from inside
	// the product — unlike a missing cluster.
	if !strings.Contains(why, "Settings") {
		t.Errorf("the explanation should point at the setting, got: %q", why)
	}

	enabled := Capabilities{Topology: TopologyECS, IsolatedPossible: true, IsolatedEnabled: true, JobsEnabled: true}
	if !enabled.Allows(ModeIsolated) || !enabled.BackgroundJobsAvailable() {
		t.Error("enabling isolated mode should also enable background jobs")
	}
	if enabled.ExplainUnavailable(ModeIsolated) != "" {
		t.Error("an available mode needs no explanation")
	}
}

// An app's own execution mode has no say in whether it may start background
// work, and every refusal that remains is about the install.
//
// This used to assert the opposite, and the reversal is the point rather than a
// relaxation. A job run is an isolated workload whichever mode its app is in —
// the same code from the same release with a different entrypoint — so refusing
// a shared-mode app withheld no credential (the app token is injected in both
// modes anyway) and bought nothing. What it did force was the app running the
// work *inside* the shared process instead, which is worse on exactly the axis
// the refusal claimed to protect. See docs/architecture-decisions.md §26.
func TestJobsAvailabilityIsAboutTheInstallAndNotTheApp(t *testing.T) {
	enabled := Capabilities{Topology: TopologyKubernetes, IsolatedPossible: true, IsolatedEnabled: true, JobsEnabled: true}

	for _, m := range []Mode{ModeShared, ModeIsolated} {
		if why := enabled.ExplainJobsUnavailable(m); why != "" {
			t.Errorf("a %s app on a jobs-capable install must not be refused, got: %q", m, why)
		}
	}

	// Jobs switched off is an administrator's decision, and saying which switch
	// and where is the difference between a dead end and a next step.
	off := Capabilities{Topology: TopologyKubernetes, IsolatedPossible: true, IsolatedEnabled: true}
	why := off.ExplainJobsUnavailable(ModeShared)
	for _, want := range []string{"turned off", "Settings"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal should mention %q, got: %q", want, why)
		}
	}
	// And it must not have become a lecture about execution mode again: the
	// app's mode is not why this one failed.
	if strings.Contains(why, "isolated mode") {
		t.Errorf("a switched-off install must not blame the app's mode, got: %q", why)
	}

	// Nowhere to place a workload is the operator's problem, and on a VM it is
	// permanent.
	vm := VMCapabilities().ExplainJobsUnavailable(ModeShared)
	if !strings.Contains(vm, "does not offer") {
		t.Errorf("a VM install should explain that jobs are not offered at all, got: %q", vm)
	}
	if !strings.Contains(vm, "single machine") {
		t.Errorf("a VM install should say why it cannot, got: %q", vm)
	}

	// A VM with a shared runtime runs jobs on itself (ADR §106), and the
	// placement it reports is that machine.
	host := VMCapabilities()
	host.HostJobsPossible, host.JobsEnabled = true, true
	if why := host.ExplainJobsUnavailable(ModeShared); why != "" {
		t.Errorf("a VM with a shared runtime must offer jobs, got: %q", why)
	}
	if host.JobPlacement() != ModeShared {
		t.Errorf("placement = %q, want shared — this machine", host.JobPlacement())
	}
	if enabled.JobPlacement() != ModeIsolated {
		t.Errorf("an install with isolated available places runs in pods, got %q", enabled.JobPlacement())
	}
}

// The stored value is a mode, never a backend. An install that changes cluster
// technology must not have to rewrite every app row.
func TestBackendNamesAreNotValidModes(t *testing.T) {
	for _, backend := range []string{"fargate", "pod", "ecs", "kubernetes", "k8s", "pm2"} {
		if _, err := ParseMode(backend); err == nil {
			t.Errorf("%q was accepted as an execution mode; only shared/isolated are vocabulary", backend)
		}
	}
}

// --- The job contract ---

// A job killed for exceeding its resources or its deadline reports that
// distinctly from a crash, so a developer fixes the right problem.
func TestResourceAndTimeoutFailuresAreDistinctFromCrashes(t *testing.T) {
	distinct := []JobState{JobFailed, JobTimedOut, JobResourceExceeded, JobCancelled, JobInfrastructureErr}
	seen := map[JobState]bool{}
	for _, s := range distinct {
		if seen[s] {
			t.Errorf("%q is not distinct", s)
		}
		seen[s] = true
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []JobState{JobPending, JobRunning} {
		if s.Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

// Failure stages exist so an operator debugs in Toft rather than in a
// cloud console. Each names a different owner of the problem.
func TestFailureStagesCoverEachDistinctOwner(t *testing.T) {
	stages := []FailureStage{
		FailureImagePull,           // the operator's registry
		FailureArtifactFetch,       // the artifact store
		FailureBuild,               // the app's code
		FailureExec,                // the app's code, at run time
		FailureCallbackUnreachable, // the platform's reachability
	}
	seen := map[FailureStage]bool{}
	for _, s := range stages {
		if s == FailureNone {
			t.Error("a failure stage must not be the empty/no-failure value")
		}
		if seen[s] {
			t.Errorf("%q is not distinct", s)
		}
		seen[s] = true
	}
}

// Isolated mode requires both halves: infrastructure an operator configured
// and a switch an administrator threw. Either alone must not offer it.
//
// The asymmetry is deliberate. An admin must not be able to enable something
// the install cannot do; and an operator adding cluster configuration must not
// thereby switch it on, because the first enable forces a rebuild of every app
// and that has to be a planned window.
func TestIsolatedModeNeedsBothPossibleAndEnabled(t *testing.T) {
	cases := []struct {
		name      string
		caps      Capabilities
		available bool
		explain   string // a phrase the refusal must contain
	}{
		{"neither", Capabilities{Topology: TopologyKubernetes}, false, "no isolated backend configured"},
		{"possible but not enabled", Capabilities{Topology: TopologyKubernetes, IsolatedPossible: true}, false, "turned off"},
		{"enabled but not possible", Capabilities{Topology: TopologyKubernetes, IsolatedEnabled: true}, false, "no isolated backend configured"},
		{"both", Capabilities{Topology: TopologyKubernetes, IsolatedPossible: true, IsolatedEnabled: true}, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caps.IsolatedAvailable(); got != tc.available {
				t.Errorf("IsolatedAvailable() = %v, want %v", got, tc.available)
			}
			why := tc.caps.ExplainUnavailable(ModeIsolated)
			if tc.available {
				if why != "" {
					t.Errorf("an available mode needs no explanation, got %q", why)
				}
				return
			}
			if !strings.Contains(why, tc.explain) {
				t.Errorf("explanation should mention %q, got: %q", tc.explain, why)
			}
		})
	}
}

// Jobs are gated separately from isolated mode: an operator may reasonably
// want isolated apps without opening the job system.
func TestJobsAreGatedSeparatelyFromIsolatedMode(t *testing.T) {
	isolatedOnly := Capabilities{Topology: TopologyKubernetes, IsolatedPossible: true, IsolatedEnabled: true}

	if !isolatedOnly.Allows(ModeIsolated) {
		t.Fatal("isolated mode should be available")
	}
	if isolatedOnly.BackgroundJobsAvailable() {
		t.Error("jobs should be off until separately enabled")
	}
	why := isolatedOnly.ExplainJobsUnavailable(ModeIsolated)
	if !strings.Contains(why, "turned off") {
		t.Errorf("the refusal should say jobs are off, not that isolation is: %q", why)
	}
	// And must not misdirect the reader to a problem they do not have.
	if strings.Contains(why, "no cluster") {
		t.Errorf("the refusal blames the wrong thing: %q", why)
	}
}

// --- The narrator ---

// A backend narrates through the seam and never reaches a store.
//
// The seam is an interface declared *here* because internal/database imports
// this package, so a backend that talked to the store directly would be an
// import cycle — the same reason Repairer is an interface here, pointing the
// other way. This test is what stops the cycle being reintroduced as a
// concrete type: a Narrator whose methods take only strings cannot be a
// database handle.
func TestTheNarratorSeamCarriesNothingButWords(t *testing.T) {
	nt := reflect.TypeOf((*Narrator)(nil)).Elem()
	if nt.NumMethod() != 2 {
		t.Fatalf("Narrator has %d methods; two is the specification", nt.NumMethod())
	}
	for i := 0; i < nt.NumMethod(); i++ {
		m := nt.Method(i)
		if m.Type.NumOut() != 0 {
			t.Errorf("%s returns something. A workload may never fail to start "+
				"because its narration could not be written down", m.Name)
		}
		for j := 0; j < m.Type.NumIn(); j++ {
			if m.Type.In(j).Kind() != reflect.String {
				t.Errorf("%s takes a %s. The seam carries words and nothing else — "+
					"an error value here is how the operator's register leaks into "+
					"the owner's", m.Name, m.Type.In(j))
			}
		}
	}
}

// Nil is a real configuration and not a gap: a backend standing on its own has
// nothing to narrate to, and a start must not depend on one being wired.
func TestAStartWithNoNarratorIsSilentRatherThanFatal(t *testing.T) {
	p := NewAgentRuntime(Isolation{}, LocalResolver{}, &stubAllocator{}, t.TempDir(), "")
	if p.narrator != nil {
		t.Fatal("a backend constructed without one has a narrator")
	}
	// The failure below is the resolver's, not the narrator's: what is being
	// asserted is that the narration on the way to it does not panic.
	_, err := p.Deploy(WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "demo",
		Release: ArtifactRelease("nope"), Framework: "python",
	})
	if err == nil {
		t.Fatal("expected the unresolvable release to fail the start")
	}
}

// A release is named to its owner and never located.
//
// ReleaseRef.String() is the operator's answer — an absolute directory under
// APPS_DIR, or a store key — and both describe the install's layout rather
// than the app. This is the one place a narrated line derives from a value the
// platform did not compose, so it is the one that needs a test.
func TestAReleaseIsNamedNotLocated(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  ReleaseRef
		want string
	}{
		{"a local release directory", LocalRelease("/srv/toft/apps/b8c9/releases/8f8ab979"), "release 8f8ab979"},
		{"the legacy current symlink", LocalRelease("/srv/toft/apps/b8c9/current"), "the current release"},
		{"a trailing slash", LocalRelease("/srv/apps/b8c9/releases/8f8ab979/"), "release 8f8ab979"},
		{"an artifact key", ArtifactRelease("apps/b8c9/releases/8f8ab979.tar.zst"), "release 8f8ab979"},
		{"nothing at all", ReleaseRef{}, "the current release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := releaseLabel(tc.ref)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "/") {
				t.Errorf("%q carries a path; an owner is owed the release's name, not the install's layout", got)
			}
		})
	}
}

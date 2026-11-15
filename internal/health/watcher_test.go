package health

import (
	"errors"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/events"
	mailer "toft/internal/mail"
	"toft/internal/runtime"
)

// --- doubles ---

type fakeStore struct {
	apps       []database.App
	pending    map[string]bool
	pendingErr error
	statuses   map[string]string
	// listed, when set, runs after each ListAllApps: a person's write landing
	// between the sweep's read and its own.
	listed func()
}

// ListAllApps returns a copy, as the database does: the sweep works on a
// snapshot and a later write to the row does not reach it.
func (f *fakeStore) ListAllApps() ([]database.App, error) {
	out := append([]database.App(nil), f.apps...)
	if f.listed != nil {
		f.listed()
	}
	return out, nil
}

// setStatus is a write from outside the watcher — a person's Stop.
func (f *fakeStore) setStatus(id, status string) {
	for i := range f.apps {
		if f.apps[i].ID == id {
			f.apps[i].Status = status
		}
	}
}

func (f *fakeStore) UpdateAppStatusFrom(id, from, status string) (bool, error) {
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	for i := range f.apps {
		if f.apps[i].ID == id {
			if f.apps[i].Status != from {
				return false, nil
			}
			f.apps[i].Status = status
			f.statuses[id] = status
			return true, nil
		}
	}
	return false, nil
}

// InFlightDeployments reads the same map: a pending deploy is in flight.
func (f *fakeStore) InFlightDeployments(appID string, since time.Time) (bool, []string, error) {
	if f.pendingErr != nil {
		return false, nil, f.pendingErr
	}
	return f.pending[appID], nil, nil
}

func (f *fakeStore) HasPendingDeployment(appID string, since time.Time) (bool, error) {
	if f.pendingErr != nil {
		return false, f.pendingErr
	}
	return f.pending[appID], nil
}

type recordingStore struct {
	events []*database.AppEvent
	// cleared counts Clear calls per kind.
	cleared map[string]int
}

func (r *recordingStore) InsertAppEvent(e *database.AppEvent) error {
	e.ID = "e"
	copied := *e
	r.events = append(r.events, &copied)
	return nil
}
func (r *recordingStore) ClaimEventNotify(string, string, string, string, time.Duration) (bool, error) {
	return false, nil
}
func (r *recordingStore) MarkEventNoticeSent(string) error { return nil }
func (r *recordingStore) ResolveAppEvents(appID, kind, _ string) (int, error) {
	if r.cleared == nil {
		r.cleared = map[string]int{}
	}
	r.cleared[kind]++
	return 0, nil
}
func (r *recordingStore) AppNotifyFailures(string) (bool, error) { return true, nil }
func (r *recordingStore) ListAppRecipients(string) ([]database.Recipient, error) {
	return nil, nil
}
func (r *recordingStore) GetApp(string) (*database.App, error) { return nil, nil }

// flaps is how many restart-loop events were recorded — the verdict, apart
// from the per-restart app_restarted rows every restart now leaves.
func (r *recordingStore) flaps() int {
	n := 0
	for _, e := range r.events {
		if e.Kind == string(events.KindAppFlapping) {
			n++
		}
	}
	return n
}

// without drops one kind, for the tests about everything else.
func (r *recordingStore) without(kind events.Kind) []*database.AppEvent {
	var out []*database.AppEvent
	for _, e := range r.events {
		if e.Kind != string(kind) {
			out = append(out, e)
		}
	}
	return out
}

func (r *recordingStore) kinds() []string {
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

// stubRuntime answers List() from a fixed table. Everything else is unused —
// the watcher only ever lists.
type stubRuntime struct {
	runtime.ServiceRuntime
	workloads map[string]*runtime.AppMetrics
	err       error
	stopped   []string
	stopErr   error
}

func (s *stubRuntime) List() (map[string]*runtime.AppMetrics, error) {
	return s.workloads, s.err
}

// Stop is the one action the watcher takes, and only for the memory ending.
func (s *stubRuntime) Stop(name string) error {
	s.stopped = append(s.stopped, name)
	if s.stopErr != nil {
		return s.stopErr
	}
	delete(s.workloads, name)
	return nil
}

func newWatcher(t *testing.T, apps []database.App, workloads map[string]*runtime.AppMetrics) (*Watcher, *fakeStore, *recordingStore, *stubRuntime) {
	t.Helper()
	return newWatcherOn(t, apps, workloads, nil)
}

func newWatcherOn(t *testing.T, apps []database.App, workloads map[string]*runtime.AppMetrics,
	onCrash func(*database.App)) (*Watcher, *fakeStore, *recordingStore, *stubRuntime) {
	t.Helper()
	store := &fakeStore{apps: apps, pending: map[string]bool{}}
	rec := &recordingStore{}
	rt := &stubRuntime{workloads: workloads}
	w := New(store, runtime.NewRuntimes(rt, nil),
		events.NewRecorder(rec, nil, nil), true, onCrash)
	return w, store, rec, rt
}

func runningApp() database.App {
	return database.App{
		ID: "app-1", Slug: "reports", Name: "Reports",
		Status: database.AppStatusRunning, CurrentRelease: "reports-abc12345",
	}
}

func online(restarts int) map[string]*runtime.AppMetrics {
	return map[string]*runtime.AppMetrics{"reports-abc12345": {Status: "online", Restarts: restarts}}
}

// --- tests ---

// One bad sweep records nothing. A single missed sample must not mark an app
// crashed; two consecutive ones must.
func TestTwoConsecutiveBadSweepsBeforeCrash(t *testing.T) {
	w, store, rec, _ := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})

	w.tick()
	if store.statuses["app-1"] != "" {
		t.Fatalf("one bad sweep already changed the status to %q", store.statuses["app-1"])
	}
	if len(rec.events) != 0 {
		t.Fatalf("one bad sweep recorded %d events, want 0", len(rec.events))
	}

	w.tick()
	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatalf("status = %q after two bad sweeps, want crashed", store.statuses["app-1"])
	}
	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppCrashed) {
		t.Fatalf("recorded %v, want one app_crashed", rec.kinds())
	}
	if rec.events[0].Fingerprint != "reports-abc12345" {
		t.Errorf("fingerprint = %q, want the release — a new release is a new problem",
			rec.events[0].Fingerprint)
	}
}

// A Stop that lands while a sweep is walking the estate is the person's
// decision: the crash the sweep reached on its stale row is not written over
// it, and the supervisor is not handed an app to restart (JOBS-3).
func TestAStopDuringTheSweepIsNotOverwrittenWithCrashed(t *testing.T) {
	var crashed []*database.App
	w, store, rec, _ := newWatcherOn(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{},
		func(a *database.App) { crashed = append(crashed, a) })

	w.tick() // bad = 1
	store.listed = func() { store.setStatus("app-1", database.AppStatusStopped) }
	w.tick() // bad = 2, on a row that said running when it was read

	if got := store.apps[0].Status; got != database.AppStatusStopped {
		t.Fatalf("status = %q, want stopped — the sweep overwrote the person's Stop", got)
	}
	if len(crashed) != 0 {
		t.Errorf("onCrash called %d time(s) for an app a person stopped", len(crashed))
	}
	if len(rec.events) != 0 {
		t.Errorf("recorded %v for an app that did not crash", rec.kinds())
	}
}

// The heal is as careful: a crashed app a person stopped while the sweep was
// running is not called running because its workload is still up.
func TestAStopDuringTheSweepIsNotHealedToRunning(t *testing.T) {
	app := runningApp()
	app.Status = database.AppStatusCrashed
	w, store, _, _ := newWatcher(t, []database.App{app}, online(0))
	store.listed = func() { store.setStatus("app-1", database.AppStatusStopped) }

	w.tick()
	if got := store.apps[0].Status; got != database.AppStatusStopped {
		t.Fatalf("status = %q, want stopped", got)
	}
}

// A workload that comes back between sweeps resets the counter, so an app is
// never marked crashed on two *non-consecutive* bad samples.
func TestHealthySweepResetsTheCounter(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})

	w.tick()
	rt.workloads = online(0)
	w.tick()
	rt.workloads = map[string]*runtime.AppMetrics{}
	w.tick()

	if store.statuses["app-1"] == database.AppStatusCrashed {
		t.Fatal("two non-consecutive bad sweeps marked the app crashed")
	}
	if len(rec.events) != 0 {
		t.Fatalf("recorded %v, want nothing", rec.kinds())
	}
}

// An "errored" workload is as bad as a missing one, and the detail says which.
func TestErroredWorkloadCounts(t *testing.T) {
	w, store, rec, _ := newWatcher(t, []database.App{runningApp()},
		map[string]*runtime.AppMetrics{"reports-abc12345": {Status: "errored"}})

	w.tick()
	w.tick()
	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatal("an errored workload did not produce a crash")
	}
	if got := rec.events[0].Detail; !strings.Contains(got, "errored") {
		t.Errorf("detail does not say what the workload reported: %q", got)
	}
}

// A workload the backend is still starting is neither healthy nor a crash: it
// must not advance the counter, and it must not reset it either.
func TestLaunchingIsIndeterminate(t *testing.T) {
	w, store, _, rt := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})

	w.tick() // bad = 1
	rt.workloads = map[string]*runtime.AppMetrics{"reports-abc12345": {Status: "launching"}}
	w.tick() // indeterminate — no crash yet
	if store.statuses["app-1"] == database.AppStatusCrashed {
		t.Fatal("a launching workload was treated as a crash")
	}
	rt.workloads = map[string]*runtime.AppMetrics{}
	w.tick() // bad = 2
	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatal("the counter was reset by a launching sweep")
	}
}

// Healing is the recovery the platform previously only performed at boot.
func TestCrashedAppHealsWhenItComesBack(t *testing.T) {
	app := runningApp()
	app.Status = database.AppStatusCrashed
	w, store, rec, _ := newWatcher(t, []database.App{app}, online(3))

	w.tick()
	if store.statuses["app-1"] != database.AppStatusRunning {
		t.Fatalf("status = %q, want running", store.statuses["app-1"])
	}
	if rec.cleared[string(events.KindAppCrashed)] != 1 {
		t.Errorf("app_crashed was not cleared: %v", rec.cleared)
	}
	if rec.cleared[string(events.KindAppFlapping)] != 1 {
		t.Errorf("app_flapping was not cleared: %v", rec.cleared)
	}
	if len(rec.events) != 0 {
		t.Errorf("healing recorded %v, want nothing", rec.kinds())
	}
}

// A loop is online for the seconds each start lives, and the agent goes on
// calling it a loop until a start outlives its backoff's reset. That online
// half is not a recovery: healing on it marked a 25-second loop running and
// closed its app_crashed once per backoff cycle (the soak, 2026-10-28).
func TestAnOnlineWorkloadStillCalledALoopIsNotHealed(t *testing.T) {
	app := runningApp()
	app.Status = database.AppStatusCrashed
	up := online(3)
	up["reports-abc12345"].CrashLooping = true
	w, store, rec, _ := newWatcher(t, []database.App{app}, up)

	w.tick()
	if got := store.statuses["app-1"]; got == database.AppStatusRunning {
		t.Fatalf("a workload the agent still calls a loop was healed to running")
	}
	if n := rec.cleared[string(events.KindAppCrashed)]; n != 0 {
		t.Errorf("app_crashed was cleared %d time(s) while the loop went on", n)
	}
}

// A crashed app that stays crashed is not re-recorded every minute.
func TestCrashedAppIsNotReRecorded(t *testing.T) {
	app := runningApp()
	app.Status = database.AppStatusCrashed
	w, _, rec, _ := newWatcher(t, []database.App{app}, map[string]*runtime.AppMetrics{})

	w.tick()
	w.tick()
	w.tick()
	if len(rec.events) != 0 {
		t.Fatalf("an already-crashed app recorded %v", rec.kinds())
	}
}

// Every status that is mid-transition or somebody's decision is skipped.
func TestMidTransitionStatusesAreSkipped(t *testing.T) {
	for _, status := range []string{
		database.AppStatusBuilding,
		database.AppStatusSleeping,
		database.AppStatusStopped,
		database.AppStatusFailed,
	} {
		t.Run(status, func(t *testing.T) {
			app := runningApp()
			app.Status = status
			w, store, rec, _ := newWatcher(t, []database.App{app}, map[string]*runtime.AppMetrics{})

			w.tick()
			w.tick()
			if store.statuses["app-1"] != "" {
				t.Fatalf("a %s app had its status changed to %q", status, store.statuses["app-1"])
			}
			if len(rec.events) != 0 {
				t.Fatalf("a %s app recorded %v", status, rec.kinds())
			}
		})
	}
}

// A deploy in flight is exactly when a naive sweep would report a crash: the
// old workload is gone and the row does not name the new one yet.
func TestPendingDeploymentSuppressesTheSweep(t *testing.T) {
	w, store, rec, _ := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})
	store.pending["app-1"] = true

	w.tick()
	w.tick()
	if store.statuses["app-1"] != "" {
		t.Fatalf("an app mid-deploy was marked %q", store.statuses["app-1"])
	}
	if len(rec.events) != 0 {
		t.Fatalf("recorded %v during a deploy", rec.kinds())
	}
}

// A backend that cannot be listed abandons the whole sweep. Acting on an empty
// listing would mark every app on the install crashed at once.
func TestBackendFailureAbandonsTheSweep(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, nil)
	rt.err = errors.New("pm2 daemon is restarting")

	w.tick()
	w.tick()
	if store.statuses["app-1"] != "" {
		t.Fatalf("an unreadable backend marked the app %q", store.statuses["app-1"])
	}
	if len(rec.events) != 0 {
		t.Fatalf("recorded %v with no backend answer", rec.kinds())
	}
}

// A restart delta over the threshold is a restart loop.
func TestRestartDeltaTripsFlapping(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(2))

	// First sighting establishes the baseline and cannot be a delta: a
	// long-running app's lifetime restart count is not this minute's.
	w.tick()
	if len(rec.events) != 0 {
		t.Fatalf("the first sighting recorded %v", rec.kinds())
	}

	rt.workloads = online(4) // +2, under the threshold
	w.tick()
	if rec.flaps() != 0 {
		t.Fatalf("a small delta recorded %v", rec.kinds())
	}

	rt.workloads = online(20) // +16
	w.tick()
	flapping := rec.without(events.KindAppRestarted)
	if len(flapping) != 1 || flapping[0].Kind != string(events.KindAppFlapping) {
		t.Fatalf("recorded %v, want one app_flapping", rec.kinds())
	}
	if flapping[0].Severity != database.EventSeverityWarning {
		t.Errorf("severity = %q, want warning — the app is answering some requests",
			flapping[0].Severity)
	}
}

// A workload that never starts is not "still starting".
//
// A pod nothing can place stays Pending, and therefore `launching`, for as long
// as nobody looks — and the sweep returned early on exactly that state. The row
// went on saying `running`, no event was recorded, and an owner running the two
// commands the CLI offers for this was told twice that nothing was wrong while
// visitors got the unavailable page. The cluster had a precise reason the whole
// time, which is what the detail carries.
func TestAWorkloadThatNeverStartsIsEventuallyCalledCrashed(t *testing.T) {
	launching := map[string]*runtime.AppMetrics{"reports-abc12345": {
		Status: "launching",
		Detail: `persistentvolumeclaim "lpv-drop-read" is being deleted`,
	}}
	w, store, rec, _ := newWatcher(t, []database.App{runningApp()}, launching)
	at := time.Now().UTC()
	w.now = func() time.Time { return at }

	// Inside the grace it is a cold start, and nothing is said — an image pull
	// and an artifact fetch legitimately take tens of seconds.
	for i := 0; i < 3; i++ {
		at = at.Add(time.Minute)
		w.tick()
	}
	if len(rec.events) != 0 || store.statuses["app-1"] != "" {
		t.Fatalf("a workload three minutes into starting was already judged: %v %q",
			rec.kinds(), store.statuses["app-1"])
	}

	// Past it, the two-sweep guard applies as it does to any other bad sweep.
	at = at.Add(launchGrace)
	w.tick()
	w.tick()

	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatalf("status = %q, want crashed", store.statuses["app-1"])
	}
	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppCrashed) {
		t.Fatalf("recorded %v, want one app_crashed", rec.kinds())
	}
	// The backend's own sentence, which is the one an owner can act on.
	if !strings.Contains(rec.events[0].Detail, "is being deleted") {
		t.Errorf("the event does not carry the cluster's reason: %q", rec.events[0].Detail)
	}
	if !strings.Contains(rec.events[0].Detail, "starting for over") {
		t.Errorf("the event does not say what happened: %q", rec.events[0].Detail)
	}
}

// And a workload that starts within the grace is left alone — the clock is per
// start, not per app.
func TestASlowStartThatSucceedsIsNotJudged(t *testing.T) {
	launching := map[string]*runtime.AppMetrics{"reports-abc12345": {Status: "launching"}}
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, launching)
	at := time.Now().UTC()
	w.now = func() time.Time { return at }

	at = at.Add(4 * time.Minute)
	w.tick()
	rt.workloads = online(0)
	at = at.Add(time.Minute)
	w.tick()

	// A later start begins its own grace rather than inheriting the first's.
	rt.workloads = launching
	at = at.Add(2 * time.Minute)
	w.tick()
	w.tick()

	if len(rec.events) != 0 || store.statuses["app-1"] != "" {
		t.Fatalf("a workload that started, then started again, was judged: %v %q",
			rec.kinds(), store.statuses["app-1"])
	}
}

// The loop a per-sweep threshold cannot see.
//
// The kubelet backs off — 10s, 20s, 40s, and on to five minutes — so an
// isolated app in a genuine crash loop contributes about one restart per sweep
// and never five inside one: `KindAppFlapping` could not be raised for an
// isolated app at all, and the loop was visible as `errored` with nothing
// counting it. Restarts accumulate over flapWindow now, which is the shape that
// fits a backend that restarts immediately *and* one that backs off.
func TestRestartsAccumulateAcrossSweepsIntoOneLoop(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	at := time.Now().UTC()
	w.now = func() time.Time { return at }

	w.tick() // the baseline sighting

	// One restart per sweep, two minutes apart — the shape of a kubelet's
	// backoff, and four short of the old per-sweep threshold every time.
	for i := 1; i <= 4; i++ {
		at = at.Add(2 * time.Minute)
		rt.workloads = online(i)
		w.tick()
		if rec.flaps() != 0 {
			t.Fatalf("restart %d recorded %v; four in eight minutes is not yet a loop", i, rec.kinds())
		}
	}

	at = at.Add(2 * time.Minute)
	rt.workloads = online(5)
	w.tick()
	if got := rec.without(events.KindAppRestarted); len(got) != 1 || got[0].Kind != string(events.KindAppFlapping) {
		t.Fatalf("recorded %v, want one app_flapping", rec.kinds())
	}
}

// A crash loop the backend is holding back says so, rather than "the workload
// reports status errored" — which is true of every kind of down.
func TestACrashLoopIsNamedInTheCrashedEvent(t *testing.T) {
	looping := online(7)
	looping["reports-abc12345"].Status = "errored"
	looping["reports-abc12345"].CrashLooping = true

	w, _, rec, _ := newWatcher(t, []database.App{runningApp()}, looping)
	w.tick()
	w.tick() // the second consecutive bad sweep is the one that decides

	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppCrashed) {
		t.Fatalf("recorded %v, want one app_crashed", rec.kinds())
	}
	if !strings.Contains(rec.events[0].Detail, "keeps starting and exiting") {
		t.Errorf("the detail does not name the loop: %q", rec.events[0].Detail)
	}
	if !strings.Contains(rec.events[0].Detail, "7 time(s)") {
		t.Errorf("the detail does not carry the count: %q", rec.events[0].Detail)
	}
}

// The window is a window: restarts spread thinly enough are an app that falls
// over now and then, which is not the same finding.
func TestRestartsSpreadPastTheWindowAreNotALoop(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	at := time.Now().UTC()
	w.now = func() time.Time { return at }
	w.tick()

	for i := 1; i <= 6; i++ {
		at = at.Add(flapWindow + time.Minute)
		rt.workloads = online(i)
		w.tick()
	}
	if rec.flaps() != 0 {
		t.Fatalf("recorded %v; one restart every sixteen minutes is not a loop", rec.kinds())
	}
}

// Memory keeps its precedence. The specific answer has already been given —
// "the app keeps restarting" arriving beside "it ran out of memory" is the
// general one second, reading as a second problem.
func TestAMemoryKillDoesNotAlsoReportALoop(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(1))
	w.tick()

	oom := online(9) // well past the threshold, and every one of them explained
	oom["reports-abc12345"].OOMKilled = true
	oom["reports-abc12345"].MemoryLimitMB = 512
	rt.workloads = oom
	w.tick()

	for _, e := range rec.events {
		if e.Kind == string(events.KindAppFlapping) {
			t.Fatalf("the general answer was recorded beside the specific one: %v", rec.kinds())
		}
	}
}

// A restart count that goes down is a fresh workload, not negative restarts.
func TestRestartCountResetDoesNotFlap(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(50))
	w.tick()
	rt.workloads = online(0)
	w.tick()
	if len(rec.events) != 0 {
		t.Fatalf("a reset restart count recorded %v", rec.kinds())
	}
}

// With a mock shared backend there are no real processes to watch, so shared
// apps are left alone entirely.
func TestSharedAppsSkippedWhenTheBackendIsNotReal(t *testing.T) {
	store := &fakeStore{apps: []database.App{runningApp()}, pending: map[string]bool{}}
	rec := &recordingStore{}
	rt := &stubRuntime{workloads: map[string]*runtime.AppMetrics{}}
	w := New(store, runtime.NewRuntimes(rt, nil),
		events.NewRecorder(rec, nil, nil), false, nil)

	w.tick()
	w.tick()
	if store.statuses["app-1"] != "" {
		t.Fatalf("a shared app was marked %q against a mock backend", store.statuses["app-1"])
	}
	if len(rec.events) != 0 {
		t.Fatalf("recorded %v", rec.kinds())
	}
}

// A deleted app's state does not linger to be inherited by whatever comes next.
func TestStateIsForgottenWithTheApp(t *testing.T) {
	w, store, _, _ := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})
	w.tick()
	if len(w.state) != 1 {
		t.Fatalf("state holds %d entries after a sweep, want 1", len(w.state))
	}
	store.apps = nil
	w.tick()
	if len(w.state) != 0 {
		t.Fatalf("state holds %d entries after the app was deleted, want 0", len(w.state))
	}
}

// A watcher with no recorder is the ordinary case for an install that has not
// wired one.
func TestWithoutRecorder(t *testing.T) {
	store := &fakeStore{apps: []database.App{runningApp()}, pending: map[string]bool{}}
	rt := &stubRuntime{workloads: map[string]*runtime.AppMetrics{}}
	w := New(store, runtime.NewRuntimes(rt, nil), nil, true, nil)
	w.tick()
	w.tick()
	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatal("the status is still written with no recorder attached")
	}
}

// The transition that is easy to miss: Builder.Reconcile restarts a dead
// workload at boot and heals the row to running itself, so the watcher never
// sees a crashed status — but the event is still open and nobody has been told
// it ended. The first healthy sighting has to close it.
func TestFirstHealthySightingClearsAStaleOpenEvent(t *testing.T) {
	// The app is already running and healthy when the watcher first looks,
	// exactly as it would be after a reconcile at boot.
	w, _, rec, _ := newWatcher(t, []database.App{runningApp()}, online(0))

	w.tick()
	if rec.cleared[string(events.KindAppCrashed)] != 1 {
		t.Fatalf("the first healthy sweep did not close open events: %v", rec.cleared)
	}

	// And it does not keep doing it every minute forever.
	w.tick()
	w.tick()
	if rec.cleared[string(events.KindAppCrashed)] != 1 {
		t.Fatalf("a healthy app cleared %d times, want 1", rec.cleared[string(events.KindAppCrashed)])
	}
}

// After a real crash and recovery it clears again — the "already cleared" flag
// is reset by anything that looks wrong.
func TestClearHappensAgainAfterACrash(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))

	w.tick() // healthy: clears once
	rt.workloads = map[string]*runtime.AppMetrics{}
	w.tick()
	w.tick() // crashed
	rt.workloads = online(0)
	w.tick() // healthy again

	if got := rec.cleared[string(events.KindAppCrashed)]; got != 2 {
		t.Fatalf("cleared %d times across a crash and recovery, want 2", got)
	}
}

// The callback is the seam between deciding and acting, and it fires once per
// transition — not once per sweep in which the app is still crashed, which
// would restart an app that is already being restarted.
func TestCrashCallbackFiresOncePerTransition(t *testing.T) {
	var told []string
	w, store, _, rt := newWatcherOn(t, []database.App{runningApp()},
		map[string]*runtime.AppMetrics{}, func(app *database.App) {
			told = append(told, app.ID+"/"+app.Status)
		})

	w.tick() // one bad sweep: the guard has not tripped
	if len(told) != 0 {
		t.Fatalf("told %v after one bad sweep, want nothing", told)
	}
	w.tick() // two: crashed
	w.tick() // and again, with the row already saying crashed
	w.tick()
	if len(told) != 1 {
		t.Fatalf("told %v, want exactly one crash", told)
	}
	// The app handed to the callback carries the status that was just written,
	// not the one this sweep found — a supervisor that re-read the row would
	// get `crashed`, and the one it is handed must agree.
	if told[0] != "app-1/"+database.AppStatusCrashed {
		t.Fatalf("callback saw %q", told[0])
	}

	// Healing and crashing again is a second transition and is told again.
	rt.workloads["reports-abc12345"] = &runtime.AppMetrics{Status: "online"}
	store.apps[0].Status = database.AppStatusCrashed
	w.tick()
	store.apps[0].Status = database.AppStatusRunning
	delete(rt.workloads, "reports-abc12345")
	w.tick()
	w.tick()
	if len(told) != 2 {
		t.Fatalf("told %v, want a second crash after a recovery", told)
	}
}

// An install with no supervisor watches exactly as it did.
func TestNilCrashCallback(t *testing.T) {
	w, store, _, _ := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})
	w.tick()
	w.tick()
	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatal("the status is still written with no callback attached")
	}
}

// atMemory is a workload reporting a reading against a cap the backend applied.
func atMemory(restarts int, memMB float64, limitMB int) map[string]*runtime.AppMetrics {
	return map[string]*runtime.AppMetrics{"reports-abc12345": {
		Status: "online", Restarts: restarts,
		Memory: runtime.Measured(memMB), MemoryLimitMB: limitMB,
	}}
}

// A workload killed for its memory cap is recorded on the first restart.
//
// The case the flap threshold cannot reach, and the reason this is its own
// kind. TADAShiny took minutes to grow into a 512 MB cap, so it restarted about
// once per sweep — forever — and five-in-one-sweep never happened: app_events
// was empty while PM2 had counted thirteen restarts. Nothing anywhere said the
// word memory, and the app's own log could not: a workload over its cap is sent
// a signal, so R printed "Execution halted" with no error above it.
func TestARestartAgainstTheMemoryCapIsRecorded(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, atMemory(1, 500, 512))

	// Baseline. One sighting is not a delta.
	w.tick()
	if len(rec.events) != 0 {
		t.Fatalf("the first sighting recorded %v", rec.kinds())
	}

	// One restart — far under flapRestartThreshold — with the previous sweep's
	// reading against the cap.
	rt.workloads = atMemory(2, 40, 512)
	w.tick()
	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppMemoryLimit) {
		t.Fatalf("recorded %v, want one app_memory_limit", rec.kinds())
	}
	// Both numbers, because an admin acts on the gap between them.
	for _, want := range []string{"500", "512", "the app's Runtime settings"} {
		if !strings.Contains(rec.events[0].Detail, want) {
			t.Errorf("detail does not mention %q: %s", want, rec.events[0].Detail)
		}
	}

	// Said once per restart, not once per sweep: an app sitting against its cap
	// is measured every minute and restarted every few.
	rt.workloads = atMemory(2, 505, 512)
	w.tick()
	if len(rec.events) != 1 {
		t.Fatalf("the same restart was recorded twice: %v", rec.kinds())
	}
}

// A kill is reported with the killed process's own peak, not the sweep before.
//
// Seen on trytoft: the sweep read 22 MB, the app grew to its 1024 MB cap inside
// the next minute and was killed, and the event said "22 MB against a 1024 MB
// limit" — a figure that argues the limit was never the problem.
func TestAMemoryKillIsReportedAtItsPeak(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, atMemory(0, 22, 1024))
	w.tick()

	killed := atMemory(1, 22, 1024)
	killed["reports-abc12345"].OOMKilled = true
	killed["reports-abc12345"].ExitPeakMemoryMB = 1024
	rt.workloads = killed
	w.tick()

	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppMemoryLimit) {
		t.Fatalf("recorded %v, want one app_memory_limit", rec.kinds())
	}
	if d := rec.events[0].Detail; !strings.Contains(d, "peaked at 1024 MB against a 1024 MB limit") || strings.Contains(d, "22 MB") {
		t.Errorf("detail does not report the peak: %s", d)
	}
}

// notifyingStore is recordingStore with someone to tell: the claim is granted
// and the app has an owner, so the recorder sends rather than collapsing.
type notifyingStore struct{ *recordingStore }

func (notifyingStore) ClaimEventNotify(string, string, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (notifyingStore) ListAppRecipients(string) ([]database.Recipient, error) {
	return []database.Recipient{{Address: "owner@example.com", DisplayName: "Owner"}}, nil
}
func (notifyingStore) GetApp(string) (*database.App, error) {
	app := runningApp()
	return &app, nil
}

type sentMail struct{ sent []mailer.Message }

func (m *sentMail) SendAsync(msg mailer.Message) { m.sent = append(m.sent, msg) }
func (m *sentMail) BaseURL() string              { return "https://toft.internal" }

// A memory kill reaches the app's people, not only its event list.
//
// The app's own log cannot say it was killed — it was sent a signal — so this
// mail is how an owner who is not watching the Events tab finds out, and it
// has to carry the figure they act on. Every later kill inside the cooldown is
// a row and not another message.
func TestAMemoryKillIsMailedToTheAppsPeople(t *testing.T) {
	rec := &recordingStore{}
	mail := &sentMail{}
	rt := &stubRuntime{workloads: atMemory(0, 22, 1024)}
	w := New(&fakeStore{apps: []database.App{runningApp()}, pending: map[string]bool{}},
		runtime.NewRuntimes(rt, nil), events.NewRecorder(notifyingStore{rec}, mail), true, nil)
	w.tick()

	killed := atMemory(1, 22, 1024)
	killed["reports-abc12345"].OOMKilled = true
	killed["reports-abc12345"].ExitPeakMemoryMB = 1024
	rt.workloads = killed
	w.tick()

	if len(mail.sent) != 1 {
		t.Fatalf("sent %d messages for one memory kill, want 1", len(mail.sent))
	}
	msg := mail.sent[0]
	if len(msg.To) != 1 || msg.To[0] != "owner@example.com" {
		t.Errorf("recipients = %v, want the app's owner", msg.To)
	}
	if !strings.Contains(msg.Subject, "too much memory") {
		t.Errorf("subject %q does not say memory", msg.Subject)
	}
	if !strings.Contains(msg.Text, "peaked at 1024 MB against a 1024 MB limit") {
		t.Errorf("body does not carry the peak: %s", msg.Text)
	}
	if !strings.Contains(msg.Text, "https://toft.internal/apps/app-1") {
		t.Errorf("body does not link to the app's page: %s", msg.Text)
	}
}

// The two facts the attribution needs are the backend's, and neither is
// inferable. A backend that does not measure memory reports nil and one that
// applied no cap reports 0 — a restart under either is just a restart, and
// guessing would put the word "memory" on every crash loop in the estate.
func TestARestartWithNoReadingOrNoCapIsNotBlamedOnMemory(t *testing.T) {
	cases := map[string]func(int) map[string]*runtime.AppMetrics{
		"no cap applied": func(r int) map[string]*runtime.AppMetrics {
			return atMemory(r, 5000, 0)
		},
		"backend does not measure memory": func(r int) map[string]*runtime.AppMetrics {
			return online(r)
		},
		"comfortably under the cap": func(r int) map[string]*runtime.AppMetrics {
			return atMemory(r, 100, 512)
		},
	}
	for name, mk := range cases {
		w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, mk(1))
		w.tick()
		rt.workloads = mk(2)
		w.tick()
		for _, e := range rec.events {
			if e.Kind == string(events.KindAppMemoryLimit) {
				t.Errorf("%s: blamed memory anyway", name)
			}
		}
	}
}

// atOOM is the isolated backend's answer: the kernel's own verdict, the cap off
// the pod spec, and no reading at all.
func atOOM(restarts int, limitMB int) map[string]*runtime.AppMetrics {
	return map[string]*runtime.AppMetrics{"reports-abc12345": {
		Status: "online", Restarts: restarts, MemoryLimitMB: limitMB, OOMKilled: true,
	}}
}

// The third memory kill on one release stops the workload and crashes the app.
//
// The behaviour the warning alone did not produce. Neither backend stops on its
// own — PM2 restarts a killed process in under a second and kubelet restarts
// the container in place — so before this an app that grew into its cap every
// few minutes was restarted forever, the row said `running`, and the only
// symptom was a restart count nobody watches.
func TestThreeMemoryKillsStopTheWorkloadAndCrashTheApp(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, atMemory(1, 500, 512))
	w.tick() // baseline

	for _, restarts := range []int{2, 3} {
		rt.workloads = atMemory(restarts, 500, 512)
		w.tick()
		if store.statuses["app-1"] != "" {
			t.Fatalf("stopped after %d kill(s); the tolerance is %d", restarts-1, memoryKillsBeforeStop)
		}
	}

	rt.workloads = atMemory(4, 500, 512)
	w.tick()

	if len(rt.stopped) != 1 || rt.stopped[0] != "reports-abc12345" {
		t.Fatalf("stopped %v, want the workload once", rt.stopped)
	}
	if got := store.statuses["app-1"]; got != database.AppStatusCrashed {
		t.Fatalf("status %q, want crashed", got)
	}
	kinds := rec.kinds()
	last := rec.events[len(rec.events)-1]
	if last.Kind != string(events.KindAppOutOfMemory) {
		t.Fatalf("recorded %v, want app_out_of_memory last", kinds)
	}
	if last.Severity != database.EventSeverityError {
		t.Errorf("severity %q, want error", last.Severity)
	}
	// The ending names the release, both numbers, and the lever.
	for _, want := range []string{"reports-abc12345", "500", "512", "Admin \u2192 Settings"} {
		if !strings.Contains(last.Detail, want) {
			t.Errorf("detail does not mention %q: %s", want, last.Detail)
		}
	}

	// And nothing restarts it: the next sweep sees a crashed app with no
	// workload and leaves it alone.
	w.tick()
	if len(rt.stopped) != 1 || store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatalf("the sweep after the ending acted again: stopped=%v status=%q",
			rt.stopped, store.statuses["app-1"])
	}
	if !w.OutOfMemory(&store.apps[0]) {
		t.Error("OutOfMemory is false, so the interstitial would promise a restart nothing is performing")
	}
}

// The supervisor is not told, because its whole job is to start the workload
// again — into the same cap, for another five rounds, which is the loop this
// ending exists to close.
func TestTheMemoryEndingDoesNotWakeTheSupervisor(t *testing.T) {
	var notices int
	w, _, _, rt := newWatcherOn(t, []database.App{runningApp()}, atMemory(1, 500, 512),
		func(*database.App) { notices++ })
	for _, restarts := range []int{1, 2, 3, 4} {
		rt.workloads = atMemory(restarts, 500, 512)
		w.tick()
	}
	if notices != 0 {
		t.Fatalf("the supervisor was told %d time(s) about a memory ending", notices)
	}
}

// The isolated backend takes no reading, and does not need one: it reports the
// kernel's verdict. Before this it reported neither, so a pod that kubelet
// OOM-killed and restarted in place produced no event of any kind.
func TestTheKernelsVerdictIsEnoughWithoutAReading(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, atOOM(1, 512))
	w.tick()
	for _, restarts := range []int{2, 3, 4} {
		rt.workloads = atOOM(restarts, 512)
		w.tick()
	}
	if got := store.statuses["app-1"]; got != database.AppStatusCrashed {
		t.Fatalf("status %q, want crashed: %v", got, rec.kinds())
	}
	// No reading was taken, so no figure is invented for one.
	last := rec.events[len(rec.events)-1]
	if !strings.Contains(last.Detail, "512") {
		t.Errorf("the cap is not named: %s", last.Detail)
	}
}

// A deploy is a clean slate, and so is a person pressing Restart. The ending's
// own sentence is "until you deploy or restart it".
func TestADeployOrARestartClearsTheMemoryTolerance(t *testing.T) {
	t.Run("deploy", func(t *testing.T) {
		app := runningApp()
		w, store, _, rt := newWatcher(t, []database.App{app}, atMemory(1, 500, 512))
		w.tick()
		for _, restarts := range []int{2, 3} {
			rt.workloads = atMemory(restarts, 500, 512)
			w.tick()
		}
		// A new release under the same app: the counter starts over, so the
		// third kill of the *old* release is the first of the new one.
		store.apps[0].CurrentRelease = "reports-def67890"
		rt.workloads = map[string]*runtime.AppMetrics{"reports-def67890": {
			Status: "online", Restarts: 1, Memory: runtime.Measured(500), MemoryLimitMB: 512,
		}}
		w.tick()
		rt.workloads = map[string]*runtime.AppMetrics{"reports-def67890": {
			Status: "online", Restarts: 2, Memory: runtime.Measured(500), MemoryLimitMB: 512,
		}}
		w.tick()
		if got := store.statuses["app-1"]; got == database.AppStatusCrashed {
			t.Fatal("the new release inherited the old one's kills")
		}
	})

	t.Run("restart", func(t *testing.T) {
		w, store, _, rt := newWatcher(t, []database.App{runningApp()}, atMemory(1, 500, 512))
		w.tick()
		for _, restarts := range []int{2, 3} {
			rt.workloads = atMemory(restarts, 500, 512)
			w.tick()
		}
		w.Reset("app-1")
		rt.workloads = atMemory(4, 500, 512)
		w.tick() // a fresh baseline, not a third kill
		rt.workloads = atMemory(5, 500, 512)
		w.tick()
		if got := store.statuses["app-1"]; got == database.AppStatusCrashed {
			t.Fatal("Reset did not clear what was counted before the restart")
		}
	})
}

// A stop the backend refuses leaves the row alone. Writing `crashed` over a
// workload that is still serving would take an app down on paper while it
// answered requests, which is the one outcome worse than the loop.
func TestAStopThatFailedDoesNotCrashTheRow(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, atMemory(1, 500, 512))
	rt.stopErr = errors.New("pm2 socket")
	w.tick()
	for _, restarts := range []int{2, 3, 4} {
		rt.workloads = atMemory(restarts, 500, 512)
		w.tick()
	}
	if store.statuses["app-1"] == database.AppStatusCrashed {
		t.Fatal("the row was crashed even though the workload was not stopped")
	}
	for _, e := range rec.events {
		if e.Kind == string(events.KindAppOutOfMemory) {
			t.Fatal("an ending was announced that did not happen")
		}
	}
}

// A burst inside one sweep spends the tolerance in one go.
//
// The tolerance is three restarts, not three sweeps, and the difference is the
// whole of it: measured on the dev install, an app growing into a 64 MB cap in
// eight seconds restarted eight times over three minutes before a per-sweep
// count reached three. The faster the leak, the longer it was tolerated.
func TestABurstOfKillsInOneSweepStopsTheWorkloadAtOnce(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, atMemory(1, 500, 512))
	w.tick() // baseline

	// Four restarts since the last sweep, with the reading against the cap.
	rt.workloads = atMemory(5, 500, 512)
	w.tick()

	if got := store.statuses["app-1"]; got != database.AppStatusCrashed {
		t.Fatalf("status %q, want crashed: %v", got, rec.kinds())
	}
	if len(rt.stopped) != 1 {
		t.Fatalf("stopped %v, want the workload once", rt.stopped)
	}
	// The ending is what gets said, rather than a warning counting past the
	// number it stops at.
	if k := rec.kinds(); len(k) != 1 || k[0] != string(events.KindAppOutOfMemory) {
		t.Fatalf("recorded %v, want app_out_of_memory alone", k)
	}
	// And it says how many actually happened, not the cap.
	if !strings.Contains(rec.events[0].Detail, "4 times") {
		t.Errorf("the ending does not name the four kills: %s", rec.events[0].Detail)
	}
}

// --- orphaned workloads ---------------------------------------------------
//
// A workload whose app has been deleted is never visited by the sweep above,
// because that loop iterates apps. Before the collection existed, one survived
// every full end-to-end run against the cluster — holding its whole memory
// reservation, with no owner reference and nothing that would ever remove it.

// orphanRuntime is a backend that can name the app behind each workload, which
// is what makes collection answerable at all.
type orphanRuntime struct {
	stubRuntime
	// byApp is workload name -> the app id the backend recorded for it.
	byApp   map[string]string
	ages    map[string]time.Duration
	deleted []string
	findErr error
	delErr  error
}

func (o *orphanRuntime) Orphans(live map[string]string, grace time.Duration) ([]runtime.Orphan, error) {
	if o.findErr != nil {
		return nil, o.findErr
	}
	var out []runtime.Orphan
	for workload, appID := range o.byApp {
		if appID == "" {
			continue
		}
		age := o.ages[workload]
		if current, ok := live[appID]; ok {
			if current != "" && workload != current && age >= grace {
				out = append(out, runtime.Orphan{Workload: workload, AppID: appID, Age: age, Superseded: true})
			}
			continue
		}
		if age < grace {
			continue
		}
		out = append(out, runtime.Orphan{Workload: workload, AppID: appID, Age: age})
	}
	return out, nil
}

func (o *orphanRuntime) Delete(name string) error {
	if o.delErr != nil {
		return o.delErr
	}
	o.deleted = append(o.deleted, name)
	delete(o.workloads, name)
	return nil
}

func newOrphanWatcher(t *testing.T, apps []database.App, rt *orphanRuntime) *Watcher {
	t.Helper()
	store := &fakeStore{apps: apps, pending: map[string]bool{}}
	return New(store, runtime.NewRuntimes(rt, nil),
		events.NewRecorder(&recordingStore{}, nil, nil), true, nil)
}

// The bug, stated: the app is gone, its workload is not, and one sweep is what
// removes it.
func TestASweepCollectsAWorkloadWhoseAppWasDeleted(t *testing.T) {
	rt := &orphanRuntime{
		stubRuntime: stubRuntime{workloads: map[string]*runtime.AppMetrics{
			"reports-abc12345": {Status: "online"},
			"gone-def67890":    {Status: "online"},
		}},
		byApp: map[string]string{"reports-abc12345": "app-1", "gone-def67890": "app-deleted"},
		ages:  map[string]time.Duration{"gone-def67890": time.Hour, "reports-abc12345": time.Hour},
	}
	w := newOrphanWatcher(t, []database.App{runningApp()}, rt)
	w.tick()

	if len(rt.deleted) != 1 || rt.deleted[0] != "gone-def67890" {
		t.Fatalf("the orphan should have been collected, got %v", rt.deleted)
	}
	if _, still := rt.workloads["reports-abc12345"]; !still {
		t.Fatal("the live app's workload must be left alone")
	}
}

// The mistake that would delete something serving: `live` is a snapshot taken
// at the top of the sweep, so an app created a moment ago has a workload no
// entry in it claims. Age is the guard, and it has to be checked before
// absence is allowed to mean anything.
func TestAWorkloadYoungerThanTheGraceIsNeverCollected(t *testing.T) {
	rt := &orphanRuntime{
		stubRuntime: stubRuntime{workloads: map[string]*runtime.AppMetrics{
			"fresh-00000000": {Status: "online"},
		}},
		byApp: map[string]string{"fresh-00000000": "app-created-since-the-listing"},
		ages:  map[string]time.Duration{"fresh-00000000": 5 * time.Second},
	}
	w := newOrphanWatcher(t, []database.App{runningApp()}, rt)
	w.tick()

	if len(rt.deleted) != 0 {
		t.Fatalf("a workload younger than the grace must survive, got %v", rt.deleted)
	}
}

// Absent is not "no app". A backend that cannot say which app a workload
// belongs to must have that read as "cannot tell", never as "nobody claims
// it" -- the two collapsing into a deletion is the whole hazard here.
func TestAWorkloadThatNamesNoAppIsLeftAlone(t *testing.T) {
	rt := &orphanRuntime{
		stubRuntime: stubRuntime{workloads: map[string]*runtime.AppMetrics{
			"unlabelled-1": {Status: "online"},
		}},
		byApp: map[string]string{"unlabelled-1": ""},
		ages:  map[string]time.Duration{"unlabelled-1": time.Hour},
	}
	w := newOrphanWatcher(t, []database.App{runningApp()}, rt)
	w.tick()

	if len(rt.deleted) != 0 {
		t.Fatalf("a workload naming no app must survive, got %v", rt.deleted)
	}
}

// A backend that cannot answer at all is skipped, not guessed at: the plain
// stubRuntime implements no OrphanFinder and the sweep still completes.
func TestABackendThatCannotFindOrphansIsSkipped(t *testing.T) {
	w, _, _, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	w.tick()
	if _, still := rt.workloads["reports-abc12345"]; !still {
		t.Fatal("a backend with no orphan support must have its workloads left alone")
	}
}

// Boot reconciliation restores apps one after another, for minutes. An app it
// has not reached yet is not running, and two sweeps used to call that a crash
// and email its owner — every platform restart, for apps back seconds later.
func TestSweepsWaitForTheBootReconcile(t *testing.T) {
	w, store, rec, _ := newWatcher(t, []database.App{runningApp()}, map[string]*runtime.AppMetrics{})
	hold := make(chan struct{})
	w.WithHold(hold)

	w.tick()
	w.tick()
	w.tick()
	if store.statuses["app-1"] != "" || len(rec.events) != 0 {
		t.Fatalf("judged an app before the reconcile finished: status %q, events %v",
			store.statuses["app-1"], rec.kinds())
	}

	close(hold)
	w.tick()
	w.tick()
	if store.statuses["app-1"] != database.AppStatusCrashed {
		t.Fatalf("after the reconcile, status = %q, want crashed", store.statuses["app-1"])
	}
}

// Every restart the backend made is said, even one far below the loop
// threshold: an app that exited with code 7 four times used to leave no line
// and no event, and `toft events` said nothing had gone wrong.
func TestEveryRestartIsSaidWithItsExitCode(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	w.tick() // the baseline

	code := 7
	rt.workloads = online(1)
	rt.workloads["reports-abc12345"].ExitCode = &code
	w.tick()

	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppRestarted) {
		t.Fatalf("recorded %v, want one app_restarted", rec.kinds())
	}
	if !strings.Contains(rec.events[0].Summary, "code 7") {
		t.Errorf("summary = %q, want the exit code", rec.events[0].Summary)
	}
	// A fact about the past: closed as soon as it is written.
	if rec.cleared[string(events.KindAppRestarted)] != 1 {
		t.Errorf("app_restarted was not closed; cleared = %v", rec.cleared)
	}

	w.tick() // no movement, nothing new
	if len(rec.events) != 1 {
		t.Errorf("a sweep with no restart recorded %v", rec.kinds())
	}
}

// A process a signal ended is said to have been killed by it, never to have
// exited with a code: PM2 records a segfault as code 0, and "exited with code
// 0" sent an owner looking for a clean shutdown.
func TestARestartBySignalNamesTheSignal(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	w.tick()

	rt.workloads = online(1)
	rt.workloads["reports-abc12345"].ExitSignal = "SIGSEGV"
	w.tick()

	if len(rec.events) != 1 {
		t.Fatalf("recorded %v, want one app_restarted", rec.kinds())
	}
	if s := rec.events[0].Summary; !strings.Contains(s, "killed by SIGSEGV") || strings.Contains(s, "code") {
		t.Errorf("summary = %q, want the signal and no exit code", s)
	}
}

// bornAt is a workload the backend created at a given moment, the way PM2's
// created_at and a pod's creation time describe it.
func bornAt(restarts int, created time.Time) map[string]*runtime.AppMetrics {
	m := online(restarts)
	m["reports-abc12345"].CreatedAt = created
	return m
}

// A crash PM2 recovered before the first sweep of a new workload is still said.
//
// The first sighting used to be the baseline whatever it held, so an app that
// was deployed, exited with code 7 and was brought back by PM2 inside the
// minute before the first sweep left no event and no line — the external test
// round's finding exactly.
func TestACrashBeforeTheFirstSweepOfANewWorkloadIsSaid(t *testing.T) {
	w, _, rec, _ := newWatcher(t, []database.App{runningApp()}, nil)
	w.started = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	code := 7
	wl := bornAt(1, w.started.Add(time.Hour))
	wl["reports-abc12345"].ExitCode = &code
	w.rts = runtime.NewRuntimes(&stubRuntime{workloads: wl}, nil)

	w.tick()
	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppRestarted) {
		t.Fatalf("recorded %v, want one app_restarted", rec.kinds())
	}
	if !strings.Contains(rec.events[0].Summary, "code 7") {
		t.Errorf("summary = %q, want the exit code", rec.events[0].Summary)
	}
	w.tick()
	if len(rec.events) != 1 {
		t.Errorf("the same restart was said again: %v", rec.kinds())
	}
}

// A workload that existed before the watcher did keeps the old rule: its
// lifetime count is history, not this minute's news.
func TestAWorkloadOlderThanTheWatcherIsBaselined(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, nil)
	w.started = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	born := w.started.Add(-time.Hour)
	rt.workloads = bornAt(3, born)
	w.tick()
	if len(rec.events) != 0 {
		t.Fatalf("a pre-existing count was reported: %v", rec.kinds())
	}
	rt.workloads = bornAt(4, born)
	w.tick()
	if len(rec.events) != 1 || rec.events[0].Kind != string(events.KindAppRestarted) {
		t.Fatalf("recorded %v, want one app_restarted", rec.kinds())
	}
}

// Restarts already counted on a workload are not counted again when the
// watcher forgets the app and then sees the same workload — a bad sweep, a
// stop, a person's restart racing the sweep — while a new workload is.
func TestCountedRestartsSurviveForgettingTheApp(t *testing.T) {
	w, store, rec, rt := newWatcher(t, []database.App{runningApp()}, nil)
	w.started = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	born := w.started.Add(time.Minute)
	rt.workloads = bornAt(2, born)
	w.tick()
	if len(rec.events) != 1 {
		t.Fatalf("recorded %v, want one app_restarted", rec.kinds())
	}

	w.Reset("app-1")
	store.apps[0].Status = database.AppStatusStopped
	w.tick()
	store.apps[0].Status = database.AppStatusRunning
	w.tick()
	if len(rec.events) != 1 {
		t.Fatalf("the same workload's restarts were said again: %v", rec.kinds())
	}

	// A bad sweep in between does not lose the count either.
	rt.workloads = map[string]*runtime.AppMetrics{"reports-abc12345": {Status: "stopping", CreatedAt: born, Restarts: 2}}
	w.tick()
	rt.workloads = bornAt(3, born)
	w.tick()
	if len(rec.events) != 2 {
		t.Fatalf("recorded %v, want a second app_restarted for the one new restart", rec.kinds())
	}
	if !strings.Contains(rec.events[1].Summary, "restart 3") || strings.Contains(rec.events[1].Summary, "times") {
		t.Errorf("summary = %q, want exactly one restart", rec.events[1].Summary)
	}

	// A different workload — the next deploy — starts from zero.
	rt.workloads = bornAt(1, born.Add(time.Hour))
	w.tick()
	if len(rec.events) != 3 {
		t.Fatalf("a new workload's restart was not said: %v", rec.kinds())
	}
}

// A crash loop is one row a sweep: the restarts are said one by one under the
// loop threshold, and the loop is said instead of them past it.
func TestALoopIsSaidInsteadOfEachRestart(t *testing.T) {
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	w.tick()
	rt.workloads = online(flapRestartThreshold + 3)
	w.tick()
	if got := rec.kinds(); len(got) != 1 || got[0] != string(events.KindAppFlapping) {
		t.Fatalf("recorded %v, want only app_flapping", got)
	}
}

// A loop the kubelet is holding back is caught in backoff by most sweeps, and
// those sweeps count its restarts too: five inside the window is a loop even
// when no sweep ever saw the workload up (RUN.29 on EKS never got one).
func TestACrashLoopCaughtInBackoffStillFlaps(t *testing.T) {
	loop := func(n int) map[string]*runtime.AppMetrics {
		m := online(n)
		m["reports-abc12345"].Status = "errored"
		m["reports-abc12345"].CrashLooping = true
		return m
	}
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	at := time.Now().UTC()
	w.now = func() time.Time { return at }
	w.tick() // the baseline, up

	for i := 1; i <= 5; i++ {
		at = at.Add(2 * time.Minute)
		rt.workloads = loop(i)
		w.tick()
	}
	if rec.flaps() != 1 {
		t.Fatalf("recorded %v, want one app_flapping", rec.kinds())
	}
}

// A loop under a backoff alternates: held back, it is called crashed; up for
// its short life, it is healed. The heal books the restarts since the last
// sweep before it moves the baseline — without that, every other restart was
// absorbed by a heal and the loop never reached the threshold (RUN.29 under
// the agent).
func TestALoopThatHealsBetweenRestartsStillFlaps(t *testing.T) {
	loop := func(n int) map[string]*runtime.AppMetrics {
		m := online(n)
		m["reports-abc12345"].Status = "errored"
		m["reports-abc12345"].CrashLooping = true
		return m
	}
	w, _, rec, rt := newWatcher(t, []database.App{runningApp()}, online(0))
	at := time.Now().UTC()
	w.now = func() time.Time { return at }
	w.tick() // the baseline, up

	for _, sighting := range []map[string]*runtime.AppMetrics{
		loop(1), loop(1), online(2), // crashed, then healed with a restart unseen
		loop(3), loop(3), online(4),
		loop(5),
	} {
		at = at.Add(time.Minute)
		rt.workloads = sighting
		w.tick()
	}
	if rec.flaps() != 1 {
		t.Fatalf("recorded %v, want one app_flapping", rec.kinds())
	}
}

// A live app's workload that is not its current release — a retired release
// whose delete failed at the flip — is collected; while a deploy is in flight
// it is not, because the pod the deploy started is about to become current
// (security review 2026-11-14, K8-9).
func TestASweepCollectsASupersededReleaseOnlyWithNoDeployInFlight(t *testing.T) {
	newRT := func() *orphanRuntime {
		return &orphanRuntime{
			stubRuntime: stubRuntime{workloads: map[string]*runtime.AppMetrics{
				"reports-abc12345": {Status: "online"},
				"reports-old00000": {Status: "online"},
			}},
			byApp: map[string]string{"reports-abc12345": "app-1", "reports-old00000": "app-1"},
			ages:  map[string]time.Duration{"reports-abc12345": time.Hour, "reports-old00000": time.Hour},
		}
	}

	rt := newRT()
	store := &fakeStore{apps: []database.App{runningApp()}, pending: map[string]bool{"app-1": true}}
	w := New(store, runtime.NewRuntimes(rt, nil), events.NewRecorder(&recordingStore{}, nil, nil), true, nil)
	w.tick()
	if len(rt.deleted) != 0 {
		t.Fatalf("a deploy is in flight, so nothing of the app may be collected, got %v", rt.deleted)
	}

	rt = newRT()
	w = newOrphanWatcher(t, []database.App{runningApp()}, rt)
	w.tick()
	if len(rt.deleted) != 1 || rt.deleted[0] != "reports-old00000" {
		t.Fatalf("the superseded release should have been collected, got %v", rt.deleted)
	}
}

package supervisor

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
)

// A store holding several apps, which the crash-recovery fake does not need.
type multiStore struct {
	mu       sync.Mutex
	apps     map[string]*database.App
	pending  map[string]bool
	statuses map[string][]string
}

func newMultiStore(apps ...*database.App) *multiStore {
	m := &multiStore{
		apps: map[string]*database.App{}, pending: map[string]bool{},
		statuses: map[string][]string{},
	}
	for _, a := range apps {
		m.apps[a.ID] = a
	}
	return m
}

func (m *multiStore) GetApp(id string) (*database.App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return nil, nil
	}
	copied := *a
	return &copied, nil
}

func (m *multiStore) UpdateAppStatus(id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[id] = append(m.statuses[id], status)
	if a, ok := m.apps[id]; ok {
		a.Status = status
	}
	return nil
}

func (m *multiStore) UpdateAppStatusFrom(id, from, status string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok || a.Status != from {
		return false, nil
	}
	m.statuses[id] = append(m.statuses[id], status)
	a.Status = status
	return true, nil
}

func (m *multiStore) HasPendingDeployment(appID string, since time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[appID], nil
}

// countingStarter records which apps were started, in a set rather than a
// count, because the D7 table is about *which* one.
type countingStarter struct {
	mu      sync.Mutex
	started []string
	err     error
	// during runs inside Start, before it returns: a Stop or a lock landing
	// while the start waits on readiness.
	during  func(app *database.App)
	halted  []string
	restops []string
}

func (c *countingStarter) Start(app *database.App) error {
	if c.during != nil {
		c.during(app)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, app.Slug)
	return c.err
}

func (c *countingStarter) Halt(app *database.App) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halted = append(c.halted, app.Slug)
	return nil
}

func (c *countingStarter) StopIfNoLongerWanted(appID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restops = append(c.restops, appID)
}

func (c *countingStarter) slugs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]string{}, c.started...)
	return out
}

func remountApp(slug string, mutate func(*database.App)) *database.App {
	app := &database.App{
		ID: slug, Slug: slug, Name: slug,
		Status:         database.AppStatusRunning,
		RunningMode:    string(runtime.ModeIsolated),
		CurrentRelease: slug + "-abc12345",
		ReleaseRef:     "oci://registry/" + slug + ":abc12345",
	}
	if mutate != nil {
		mutate(app)
	}
	return app
}

func remountTargets(apps ...*database.App) []RestartTarget {
	out := make([]RestartTarget, 0, len(apps))
	for _, a := range apps {
		out = append(out, RestartTarget{ID: a.ID, Slug: a.Slug})
	}
	return out
}

// The D7 table, in one test. Eight apps in, the three running ones out, in
// either mode.
//
// This is the list of what a revocation restarts, and each exclusion has its
// own reason — most of them "nothing is mounted, because nothing is running".
// The one that is not about running is the locked app, and it is asserted
// separately below because getting it wrong would be an eighth lock enforcement
// point wired backwards.
func TestRemountRestartsOnlyRunningApps(t *testing.T) {
	locked := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	apps := []*database.App{
		remountApp("isolated-running", nil),
		remountApp("shared", func(a *database.App) { a.RunningMode = string(runtime.ModeShared) }),
		remountApp("shared-running", func(a *database.App) {
			a.RunningMode = string(runtime.ModeShared)
			a.Status = database.AppStatusRunning
		}),
		remountApp("stopped", func(a *database.App) { a.Status = database.AppStatusStopped }),
		remountApp("locked", func(a *database.App) { a.LockedAt = &locked }),
		remountApp("crashed", func(a *database.App) { a.Status = database.AppStatusCrashed }),
		remountApp("sleeping", func(a *database.App) { a.Status = database.AppStatusSleeping }),
		remountApp("no-release", func(a *database.App) { a.ReleaseRef = "" }),
	}
	store := newMultiStore(apps...)
	starter := &countingStarter{}
	s := New(store, starter, events.NewRecorder(&recordingStore{}, nil, nil))

	s.Remount(remountTargets(apps...), "a delegation was revoked")
	s.wg.Wait()

	got := starter.slugs()
	sort.Strings(got)
	if strings.Join(got, ",") != "isolated-running,shared,shared-running" {
		t.Fatalf("started %v, want the three running apps", got)
	}
}

// Stated on its own, because a revocation that started a locked app would be a
// lock enforcement point wired backwards — the rule is that unlocking never
// starts a workload, so nothing else may either.
func TestRemountNeverStartsALockedApp(t *testing.T) {
	locked := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	app := remountApp("retired", func(a *database.App) { a.LockedAt = &locked })
	store := newMultiStore(app)
	starter := &countingStarter{}
	s := New(store, starter, events.NewRecorder(&recordingStore{}, nil, nil))

	s.Remount(remountTargets(app), "revoked")
	s.wg.Wait()

	if got := starter.slugs(); len(got) != 0 {
		t.Fatalf("started %v — a locked app must never be started", got)
	}
	if got := store.statuses["retired"]; len(got) != 0 {
		t.Errorf("a locked app's status was written: %v", got)
	}
}

// A deploy in flight will read the mappings itself, after the revocation's
// transaction committed. Racing it with a restart is how two workloads end up
// under one name.
func TestRemountLeavesADeployInFlightAlone(t *testing.T) {
	app := remountApp("building", nil)
	store := newMultiStore(app)
	store.pending["building"] = true
	starter := &countingStarter{}
	s := New(store, starter, events.NewRecorder(&recordingStore{}, nil, nil))

	s.Remount(remountTargets(app), "revoked")
	s.wg.Wait()

	if got := starter.slugs(); len(got) != 0 {
		t.Fatalf("started %v — a deploy is already replacing that workload", got)
	}
}

// An app that does not come back is `crashed` with an event on it, and the
// revocation stands regardless.
//
// This is the accepted cost of "revoked means unmounted": an administrator's
// permission change can leave somebody's app down. It is in the confirmation
// copy before the click rather than discovered after it, and from here the
// recovery loop that already exists takes over.
func TestRemountRecordsAnAppThatDoesNotComeBack(t *testing.T) {
	app := remountApp("reports", nil)
	store := newMultiStore(app)
	starter := &countingStarter{err: errors.New("readiness timed out")}
	rec := &recordingStore{}
	s := New(store, starter, events.NewRecorder(rec, nil, nil))

	s.Remount(remountTargets(app), "a delegation was revoked")
	s.wg.Wait()

	if got := store.statuses["reports"]; len(got) != 1 || got[0] != database.AppStatusCrashed {
		t.Fatalf("statuses = %v, want one crashed", got)
	}
	kinds := rec.kinds()
	if len(kinds) != 1 || kinds[0] != string(events.KindAppCrashed) {
		t.Fatalf("event kinds = %v, want one app_crashed", kinds)
	}
	detail := rec.events[0].Detail
	if !strings.Contains(detail, "storage") || !strings.Contains(detail, "current release") {
		t.Errorf("the event should say a storage change restarted it onto its current release, got: %q", detail)
	}
	if !strings.Contains(detail, "Nothing was rebuilt") {
		t.Errorf("the event should say no code changed — this was a start, not a deploy: %q", detail)
	}
}

// A failed restart halts the old workload: the start may have failed before it
// replaced anything, and the old process would otherwise keep the revoked
// access while the watcher healed the row back to running (RT-1). An isolated
// app is then handed to the recovery loop.
func TestRemountHaltsTheOldWorkloadWhenTheRestartFails(t *testing.T) {
	app := remountApp("reports", nil)
	store := newMultiStore(app)
	starter := &countingStarter{err: errors.New("decrypting env vars: no key")}
	s := New(store, starter, events.NewRecorder(&recordingStore{}, nil, nil))

	s.Remount(remountTargets(app), "a delegation was revoked")
	s.wg.Wait()

	if len(starter.halted) != 1 || starter.halted[0] != "reports" {
		t.Fatalf("halted = %v, want reports once", starter.halted)
	}
	if got := store.apps["reports"].Status; got != database.AppStatusCrashed {
		t.Fatalf("status = %q, want crashed", got)
	}
	s.mu.Lock()
	st := s.state["reports"]
	s.mu.Unlock()
	if st == nil || st.nextAt.IsZero() {
		t.Errorf("an isolated app whose remount failed was not handed to the recovery loop: %+v", st)
	}
}

// A shared-mode app whose remount failed is halted and nothing restarts it, so
// the crashed interstitial must not promise a refresh — until a person's start
// forgets it, or a deploy moves the release.
func TestASharedAppHaltedByARemountIsNotRetrying(t *testing.T) {
	app := remountApp("notes", func(a *database.App) { a.RunningMode = string(runtime.ModeShared) })
	store := newMultiStore(app)
	starter := &countingStarter{err: errors.New("agent unreachable")}
	s := New(store, starter, events.NewRecorder(&recordingStore{}, nil, nil))
	if !s.Retrying(app) {
		t.Fatal("a running shared app reads not retrying before anything happened")
	}

	s.Remount(remountTargets(app), "a delegation was revoked")
	s.wg.Wait()

	halted := *store.apps["notes"]
	if halted.Status != database.AppStatusCrashed {
		t.Fatalf("status = %q, want crashed", halted.Status)
	}
	if s.Retrying(&halted) {
		t.Error("Retrying = true for a shared app a failed remount halted — the page promises a refresh that never comes")
	}
	redeployed := halted
	redeployed.CurrentRelease = "notes-new"
	if !s.Retrying(&redeployed) {
		t.Error("a new release of the app still reads as halted")
	}
	s.Reset(app.ID)
	if !s.Retrying(&halted) {
		t.Error("a person's restart did not forget the halt")
	}
}

// A superseded or locked start is not a failure: nothing is halted, no status
// is written and no event recorded (RT-1).
func TestRemountTreatsASupersededStartAsNothingWrong(t *testing.T) {
	for _, startErr := range []error{runtime.ErrStartSuperseded, runtime.ErrAppLocked} {
		app := remountApp("reports", nil)
		store := newMultiStore(app)
		starter := &countingStarter{err: fmt.Errorf("start: %w", startErr)}
		rec := &recordingStore{}
		s := New(store, starter, events.NewRecorder(rec, nil, nil))

		s.Remount(remountTargets(app), "revoked")
		s.wg.Wait()

		if got := store.statuses["reports"]; len(got) != 0 {
			t.Errorf("%v: statuses = %v, want none", startErr, got)
		}
		if kinds := rec.kinds(); len(kinds) != 0 {
			t.Errorf("%v: events = %v, want none", startErr, kinds)
		}
		if len(starter.halted) != 0 {
			t.Errorf("%v: halted = %v, want none", startErr, starter.halted)
		}
	}
}

// A Stop made while the restart was in flight stands, whether the start then
// fails or succeeds (RT-4). On success the workload the start placed is
// stopped again.
func TestRemountLeavesAStopMadeDuringTheRestart(t *testing.T) {
	for _, startErr := range []error{errors.New("it stopped"), nil} {
		app := remountApp("reports", nil)
		store := newMultiStore(app)
		starter := &countingStarter{err: startErr, during: func(a *database.App) {
			_ = store.UpdateAppStatus(a.ID, database.AppStatusStopped)
		}}
		rec := &recordingStore{}
		s := New(store, starter, events.NewRecorder(rec, nil, nil))

		s.Remount(remountTargets(app), "revoked")
		s.wg.Wait()

		if got := store.apps["reports"].Status; got != database.AppStatusStopped {
			t.Errorf("start err %v: status = %q, want stopped", startErr, got)
		}
		if kinds := rec.kinds(); len(kinds) != 0 {
			t.Errorf("start err %v: events = %v, want none", startErr, kinds)
		}
		if startErr == nil && len(starter.restops) != 1 {
			t.Errorf("a successful start under a Stop did not stop its workload again: %v", starter.restops)
		}
	}
}

// A successful remount writes `running`, and does not touch the recovery loop's
// state — the two are different mechanisms and this one has no attempts to
// spend.
func TestRemountDoesNotConsumeRecoveryAttempts(t *testing.T) {
	app := remountApp("reports", nil)
	store := newMultiStore(app)
	starter := &countingStarter{}
	s := New(store, starter, events.NewRecorder(&recordingStore{}, nil, nil))

	s.Remount(remountTargets(app), "revoked")
	s.wg.Wait()

	// The row said running and still does: a successful remount writes nothing.
	if got := store.statuses["reports"]; len(got) != 0 {
		t.Fatalf("statuses = %v, want none written", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state) != 0 {
		t.Errorf("the remount left recovery state behind: %+v", s.state)
	}
}

// A nil supervisor and an empty list are both no-ops, because an install with
// no isolated backend has no supervisor and the revocation still has to work.
func TestRemountIsSafeWithNothingToDo(t *testing.T) {
	var nilSup *Supervisor
	nilSup.Remount(remountTargets(remountApp("reports", nil)), "revoked")

	s := New(newMultiStore(), &countingStarter{}, events.NewRecorder(&recordingStore{}, nil, nil))
	s.Remount(nil, "revoked")
	s.wg.Wait()
}

// A storage revocation restarts a shared app, and getting this wrong is
// invisible.
//
// It used to carry an isolated-only scope, on the belief that nothing is mounted
// in shared mode. A host root mounts something: a gated directory whose group
// the app holds until a start takes it away. Skipping the restart was no error
// and no restart, and a permission reading as revoked while still live.
func TestAStorageRevocationRestartsASharedApp(t *testing.T) {
	shared := remountApp("reports", func(a *database.App) {
		a.RunningMode = string(runtime.ModeShared)
	})
	starter := &countingStarter{}
	s := New(newMultiStore(shared), starter, events.NewRecorder(&recordingStore{}, nil, nil))
	s.Remount(remountTargets(shared), "a storage delegation was revoked")
	s.wg.Wait()
	if len(starter.slugs()) != 1 {
		t.Errorf("a shared app was restarted %d time(s) for a storage revocation, want 1",
			len(starter.slugs()))
	}
}

// The other isolated-only assumption, and it hid behind the first.
//
// A published artifact reference is what an isolated workload fetches its
// release *from*; a shared workload's release is a directory on this host, and
// an install with no artifact store configured — the default — has no app
// carrying a reference at all. Requiring one for every scope made a credential
// detach a silent no-op on exactly those installs: the API answered
// `restarting: [...]`, nothing restarted, and the revoked value stayed live in
// the running process.
//
// Found by attaching a credential to a real app and watching it keep working
// after the detach, which is what a fixture with ReleaseRef always set could
// not show.
func TestASharedAppWithNoArtifactReferenceIsStillRestarted(t *testing.T) {
	app := remountApp("probe", func(a *database.App) {
		a.RunningMode = string(runtime.ModeShared)
		a.ReleaseRef = "" // no artifact store on this install
	})

	starter := &countingStarter{}
	s := New(newMultiStore(app), starter, events.NewRecorder(&recordingStore{}, nil, nil))
	s.Remount(remountTargets(app), "a credential was detached")
	s.wg.Wait()

	if len(starter.slugs()) != 1 {
		t.Errorf("restarted %d time(s) — a shared app has its release on disk and needs no "+
			"artifact reference to start", len(starter.slugs()))
	}

	// An isolated app genuinely cannot start without one, and that exclusion
	// stands.
	isolated := remountApp("reports", func(a *database.App) { a.ReleaseRef = "" })
	starter = &countingStarter{}
	s = New(newMultiStore(isolated), starter, events.NewRecorder(&recordingStore{}, nil, nil))
	s.Remount(remountTargets(isolated), "a storage delegation was revoked")
	s.wg.Wait()
	if len(starter.slugs()) != 0 {
		t.Error("an isolated app with no artifact reference was started; there is nothing to fetch")
	}
}

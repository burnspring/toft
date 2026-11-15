package idle

import (
	"errors"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/runtime"
)

// fakeStore is the reaper's whole database surface, so a sweep can be driven
// without Postgres.
type fakeStore struct {
	apps     []database.App
	settings database.PlatformSettings

	settingsErr error

	statuses map[string]string

	// listed, when set, runs after each ListAllApps: a person's write landing
	// while the sweep walks its snapshot.
	listed func()
}

// ListAllApps returns a copy, as the database does.
func (f *fakeStore) ListAllApps() ([]database.App, error) {
	out := append([]database.App(nil), f.apps...)
	if f.listed != nil {
		f.listed()
	}
	return out, nil
}

func (f *fakeStore) GetApp(id string) (*database.App, error) {
	for i := range f.apps {
		if f.apps[i].ID == id {
			a := f.apps[i]
			return &a, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) UpdateAppStatusFrom(id, from, status string) (bool, error) {
	for i := range f.apps {
		if f.apps[i].ID == id && f.apps[i].Status == from {
			return true, f.UpdateAppStatus(id, status)
		}
	}
	return false, nil
}

func (f *fakeStore) GetPlatformSettings() (database.PlatformSettings, error) {
	return f.settings, f.settingsErr
}

func (f *fakeStore) UpdateAppStatus(id, status string) error {
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	f.statuses[id] = status
	for i := range f.apps {
		if f.apps[i].ID == id {
			f.apps[i].Status = status
		}
	}
	return nil
}

func sharedApp(id string) database.App {
	return database.App{
		ID: id, Slug: id, Name: id,
		Status:         database.AppStatusRunning,
		CurrentRelease: id + "-rel",
		RunningMode:    string(runtime.ModeShared),
	}
}

// newReaper wires a reaper over a mock backend whose processes are all deployed
// and online, with the clock parked at base.
func newReaper(t *testing.T, store *fakeStore) (*Reaper, *Tracker, *runtime.MockRuntime, *clock) {
	t.Helper()

	mock := runtime.NewMockRuntime(nil)
	for _, app := range store.apps {
		if app.CurrentRelease == "" {
			continue
		}
		if _, err := mock.Deploy(runtime.WorkloadSpec{
			AppID: app.ID, Slug: app.Slug, Name: app.CurrentRelease,
		}); err != nil {
			t.Fatalf("seeding mock workload for %s: %v", app.Slug, err)
		}
	}

	tr, c := newAt(base)
	r := NewReaper(store, runtime.NewRuntimes(mock, nil), tr)
	r.now = c.now
	return r, tr, mock, c
}

// stopped reports whether the backend has stopped this release's workload.
func stopped(t *testing.T, mock *runtime.MockRuntime, release string) bool {
	t.Helper()
	m, err := mock.Metrics(release)
	if err != nil {
		t.Fatalf("metrics for %s: %v", release, err)
	}
	return m.Status != "online"
}

func TestReaper_SleepsAnIdleSharedApp(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, _, mock, c := newReaper(t, store)

	// First sweep only seeds: an app nobody has vouched for gets a full grace
	// period rather than being reaped on sight.
	r.tick()
	if store.statuses["a"] != "" {
		t.Fatalf("first sweep slept an app it had never seen: %q", store.statuses["a"])
	}

	c.advance(6 * time.Minute)
	r.tick()

	if got := store.statuses["a"]; got != database.AppStatusSleeping {
		t.Fatalf("status = %q, want %q", got, database.AppStatusSleeping)
	}
	if !stopped(t, mock, "a-rel") {
		t.Fatal("the workload is still running after the app was marked sleeping")
	}
	// Stop, not Delete: the address must survive so the wake lands on the one
	// the row already holds.
	if _, err := mock.Metrics("a-rel"); err != nil {
		t.Fatalf("the workload was deleted rather than stopped: %v", err)
	}
}

func TestReaper_TouchKeepsAnAppAwake(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, tr, _, c := newReaper(t, store)

	for i := 0; i < 4; i++ {
		c.advance(4 * time.Minute)
		tr.Touch("a")
		r.tick()
	}
	if store.statuses["a"] != "" {
		t.Fatalf("an app touched every four minutes was slept: %q", store.statuses["a"])
	}
}

func TestReaper_NeverSleepsWhatItMustNot(t *testing.T) {
	isolated := sharedApp("isolated")
	isolated.RunningMode = string(runtime.ModeIsolated)

	stoppedApp := sharedApp("stopped")
	stoppedApp.Status = database.AppStatusStopped

	building := sharedApp("building")
	building.Status = database.AppStatusBuilding

	crashed := sharedApp("crashed")
	crashed.Status = database.AppStatusCrashed

	noRelease := sharedApp("norelease")
	noRelease.CurrentRelease = ""

	store := &fakeStore{
		apps: []database.App{
			isolated, stoppedApp, building, crashed, noRelease, sharedApp("eligible"),
		},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, _, _, c := newReaper(t, store)

	r.tick()
	c.advance(time.Hour)
	r.tick()

	for _, id := range []string{"isolated", "stopped", "building", "crashed", "norelease"} {
		if got := store.statuses[id]; got == database.AppStatusSleeping {
			t.Errorf("%s was slept and must never be", id)
		}
	}
	if store.statuses["eligible"] != database.AppStatusSleeping {
		t.Error("the one eligible app was not slept, so the test proves nothing")
	}
}

func TestReaper_OffByDefault(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.DefaultPlatformSettings(),
	}
	r, _, mock, c := newReaper(t, store)

	r.tick()
	c.advance(30 * 24 * time.Hour)
	r.tick()

	if store.statuses["a"] != "" {
		t.Fatalf("idle sleep is off by default but an app was slept: %q", store.statuses["a"])
	}
	if stopped(t, mock, "a-rel") {
		t.Fatal("a workload was stopped with idle sleep off")
	}
}

func TestReaper_FailsClosedOnAReadError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*fakeStore)
	}{
		{"settings", func(f *fakeStore) { f.settingsErr = errors.New("boom") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{
				apps:     []database.App{sharedApp("a")},
				settings: database.PlatformSettings{IdleSleepMinutes: 5},
			}
			r, tr, _, c := newReaper(t, store)
			tr.Touch("a")
			c.advance(time.Hour)

			tc.break_(store)
			r.tick()

			if store.statuses["a"] == database.AppStatusSleeping {
				t.Fatal("a failed read stopped someone's app; it must skip the sweep instead")
			}
		})
	}
}

func TestReaper_ForgetsDeletedApps(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, tr, _, _ := newReaper(t, store)
	tr.Touch("gone")

	r.tick()

	tr.mu.Lock()
	_, leaked := tr.apps["gone"]
	tr.mu.Unlock()
	if leaked {
		t.Fatal("the tracker kept an entry for an app that no longer exists")
	}
}

// An administrator's exemption is the other reason an app is left alone, and
// unlike a schedule it is not something the app's own team can lift. ADR §16
// used to say there was no per-app opt-out at all; this is the reversal, and
// what makes it safe is that the app page shows the owner it is in force.
func TestReaper_AdminExemptedAppNeverSleeps(t *testing.T) {
	exempt := sharedApp("exempt")
	exempt.NeverSleep = true

	store := &fakeStore{
		apps:     []database.App{exempt, sharedApp("eligible")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, _, _, c := newReaper(t, store)

	r.tick()
	c.advance(time.Hour)
	r.tick()

	if store.statuses["exempt"] == database.AppStatusSleeping {
		t.Error("an app an administrator exempted was slept")
	}
	if store.statuses["eligible"] != database.AppStatusSleeping {
		t.Fatal("the unexempted app was not slept, so the test proves nothing")
	}

	// Lifting the exemption must take effect on the next sweep, not at the
	// next restart — the flag is read from the app row every tick.
	for i := range store.apps {
		if store.apps[i].ID == "exempt" {
			store.apps[i].NeverSleep = false
		}
	}
	r.tick()
	if store.statuses["exempt"] != database.AppStatusSleeping {
		t.Error("lifting the exemption did not let the app sleep")
	}
}

// TestReaper_ARestartedWorkloadGetsItsOwnClock is the bug this closes.
//
// An app that crashed after a quiet hour and was then restarted by hand was
// slept by the very next sweep — seconds after the person who restarted it let
// go of the mouse, under a line telling its owner it had gone five minutes
// without a request. The entry the tracker held belonged to the workload that
// died, and nothing told it a new one had started.
func TestReaper_ARestartedWorkloadGetsItsOwnClock(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, tr, mock, c := newReaper(t, store)

	// Somebody used it, and then nobody did for far longer than the threshold.
	tr.Touch("a")
	r.tick()
	c.advance(time.Hour)

	// It crashes. The sweep passes over it — a crashed app has no workload to
	// stop — and the entry left behind is now an hour old.
	store.apps[0].Status = database.AppStatusCrashed
	r.tick()

	// The owner restarts it, and the next sweep lands seconds later.
	c.advance(time.Minute)
	store.apps[0].Status = database.AppStatusRunning
	r.tick()

	if got := store.statuses["a"]; got == database.AppStatusSleeping {
		t.Fatal("a workload seconds old was slept for an hour of the previous one's silence")
	}
	if stopped(t, mock, "a-rel") {
		t.Fatal("the restarted workload was stopped")
	}

	// And it is not exempt from here on. The clock restarted; it did not stop.
	c.advance(6 * time.Minute)
	r.tick()

	if got := store.statuses["a"]; got != database.AppStatusSleeping {
		t.Fatalf("status = %q, want %q", got, database.AppStatusSleeping)
	}
}

// TestReaper_AStandingWorkloadKeepsItsClock is the other half: the sweep must
// not hand a grace period to an app that has simply been up all along, or idle
// sleep would never fire at all.
func TestReaper_AStandingWorkloadKeepsItsClock(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, tr, _, c := newReaper(t, store)

	tr.Touch("a")
	// Six sweeps a minute apart, the app running throughout and nobody asking
	// for it. The last one is past the threshold and must sleep it.
	for i := 0; i < 6; i++ {
		r.tick()
		c.advance(time.Minute)
	}
	r.tick()

	if got := store.statuses["a"]; got != database.AppStatusSleeping {
		t.Fatalf("status = %q, want %q — a standing workload was given a fresh clock every sweep", got, database.AppStatusSleeping)
	}
}

// A slow release is replaced while the row reads `running` on every sweep, so
// the transition the reaper watches for never happens. The row's own flip time
// is the evidence: a marimo deploy was put to sleep eighteen seconds after it
// began serving, for the previous release's silence (trytoft drills rerun #2).
func TestReaper_AFlipTheSweepNeverSawRestartsTheClock(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("a")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5},
	}
	r, tr, mock, c := newReaper(t, store)

	tr.Touch("a")
	r.tick()
	c.advance(4 * time.Minute)
	r.tick()

	// Three minutes of a new release starting, with the row `running` across
	// every sweep, then the flip — and the next sweep lands seconds later.
	c.advance(3 * time.Minute)
	flip := c.now()
	store.apps[0].ServingSince = &flip
	c.advance(20 * time.Second)
	r.tick()

	if got := store.statuses["a"]; got == database.AppStatusSleeping {
		t.Fatal("a release twenty seconds into serving was slept for the previous one's silence")
	}
	if stopped(t, mock, "a-rel") {
		t.Fatal("the new release was stopped")
	}

	// The clock moved; it did not stop.
	c.advance(6 * time.Minute)
	r.tick()
	if got := store.statuses["a"]; got != database.AppStatusSleeping {
		t.Fatalf("status = %q, want %q", got, database.AppStatusSleeping)
	}
}

// A Stop or a Restart a person makes while the sweep walks its snapshot is
// theirs: the reaper re-reads the row and the idle clock, and neither stops the
// workload a restart just started nor writes `sleeping` over a Stop (JOBS-6).
func TestReaper_AChangeMidSweepIsNotUndone(t *testing.T) {
	flipped := base.Add(5 * time.Minute)
	for _, tc := range []struct {
		name   string
		change func(*database.App, *Tracker)
		want   string
	}{
		{"stopped", func(a *database.App, _ *Tracker) { a.Status = database.AppStatusStopped }, database.AppStatusStopped},
		// A Restart of a running app leaves the row as it was; the handler
		// restarts the tracker's clock, and that is what the reaper sees.
		{"restarted in place", func(_ *database.App, tr *Tracker) { tr.Started("a") }, database.AppStatusRunning},
		{"flipped", func(a *database.App, _ *Tracker) { a.ServingSince = &flipped }, database.AppStatusRunning},
		{"redeployed", func(a *database.App, _ *Tracker) { a.CurrentRelease = "a-rel2" }, database.AppStatusRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{
				apps:     []database.App{sharedApp("a")},
				settings: database.PlatformSettings{IdleSleepMinutes: 5},
			}
			r, tr, mock, c := newReaper(t, store)
			r.tick()
			c.advance(6 * time.Minute)
			store.listed = func() { tc.change(&store.apps[0], tr) }
			r.tick()

			if got := store.apps[0].Status; got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
			if tc.want == database.AppStatusRunning && stopped(t, mock, "a-rel") {
				t.Error("the reaper stopped a workload a person had just started")
			}
		})
	}
}

// newSplitReaper is newReaper with a backend per mode, each holding the
// workloads of the apps that run there — or no isolated backend at all when
// isolated is false, the install with no cluster.
func newSplitReaper(t *testing.T, store *fakeStore, isolated bool) (*Reaper, *runtime.MockRuntime, *runtime.MockRuntime, *clock) {
	t.Helper()
	shared := runtime.NewMockRuntime(nil)
	var cluster *runtime.MockRuntime
	if isolated {
		cluster = runtime.NewMockRuntime(nil)
	}
	for _, app := range store.apps {
		if app.CurrentRelease == "" {
			continue
		}
		rt := shared
		if app.RunningModeOrShared() == runtime.ModeIsolated {
			if cluster == nil {
				continue
			}
			rt = cluster
		}
		if _, err := rt.Deploy(runtime.WorkloadSpec{AppID: app.ID, Slug: app.Slug, Name: app.CurrentRelease}); err != nil {
			t.Fatalf("seeding mock workload for %s: %v", app.Slug, err)
		}
	}
	tr, c := newAt(base)
	var iso runtime.ServiceRuntime
	if cluster != nil {
		iso = cluster
	}
	r := NewReaper(store, runtime.NewRuntimes(shared, iso), tr)
	r.now = c.now
	return r, shared, cluster, c
}

func isolatedApp(id string) database.App {
	a := sharedApp(id)
	a.RunningMode = string(runtime.ModeIsolated)
	return a
}

// The two timeouts are two settings: each mode sleeps on its own number and
// only on it, and either can be on while the other is off.
func TestReaper_EachModeSleepsOnItsOwnTimeout(t *testing.T) {
	t.Run("isolated on, shared off", func(t *testing.T) {
		store := &fakeStore{
			apps:     []database.App{sharedApp("s"), isolatedApp("i")},
			settings: database.PlatformSettings{IsolatedIdleSleepMinutes: 30},
		}
		r, shared, cluster, c := newSplitReaper(t, store, true)

		r.tick()
		c.advance(20 * time.Minute)
		r.tick()
		if store.statuses["i"] != "" {
			t.Fatalf("slept on the shared timeout's absence rather than its own 30 minutes: %q", store.statuses["i"])
		}
		c.advance(15 * time.Minute)
		r.tick()

		if store.statuses["i"] != database.AppStatusSleeping {
			t.Fatalf("isolated app status = %q, want sleeping", store.statuses["i"])
		}
		if !stopped(t, cluster, "i-rel") {
			t.Fatal("the pod was not stopped on the isolated backend")
		}
		if _, err := cluster.Metrics("i-rel"); err != nil {
			t.Fatalf("the isolated workload was deleted rather than stopped — its Service is the address a wake lands on: %v", err)
		}
		if store.statuses["s"] != "" || stopped(t, shared, "s-rel") {
			t.Fatal("a shared app was slept with shared idle sleep off")
		}
	})

	t.Run("shared on, isolated off", func(t *testing.T) {
		store := &fakeStore{
			apps:     []database.App{sharedApp("s"), isolatedApp("i")},
			settings: database.PlatformSettings{IdleSleepMinutes: 5},
		}
		r, _, cluster, c := newSplitReaper(t, store, true)

		r.tick()
		c.advance(time.Hour)
		r.tick()

		if store.statuses["s"] != database.AppStatusSleeping {
			t.Fatalf("shared app status = %q, want sleeping", store.statuses["s"])
		}
		if store.statuses["i"] != "" || stopped(t, cluster, "i-rel") {
			t.Fatal("an isolated app was slept with isolated idle sleep off")
		}
	})
}

// Off is the default for the isolated half as much as the shared one: an
// upgrade must not start deleting pods because a setting appeared.
func TestReaper_IsolatedOffByDefault(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{isolatedApp("i")},
		settings: database.DefaultPlatformSettings(),
	}
	if store.settings.IsolatedIdleSleepMinutes != 0 {
		t.Fatalf("default isolated timeout = %d, want 0 (off)", store.settings.IsolatedIdleSleepMinutes)
	}
	r, _, cluster, c := newSplitReaper(t, store, true)
	r.tick()
	c.advance(30 * 24 * time.Hour)
	r.tick()
	if store.statuses["i"] != "" || stopped(t, cluster, "i-rel") {
		t.Fatal("an isolated app was slept with nothing configured")
	}
}

// An isolated timeout on an install with no cluster backend sleeps nothing
// isolated and does not stop the shared half doing its job.
func TestReaper_NoClusterLeavesTheSharedHalfWorking(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("s"), isolatedApp("i")},
		settings: database.PlatformSettings{IdleSleepMinutes: 5, IsolatedIdleSleepMinutes: 5},
	}
	r, _, _, c := newSplitReaper(t, store, false)
	r.tick()
	c.advance(time.Hour)
	r.tick()
	if store.statuses["s"] != database.AppStatusSleeping {
		t.Fatalf("shared app status = %q, want sleeping", store.statuses["s"])
	}
	if store.statuses["i"] != "" {
		t.Fatalf("an isolated app with no backend to stop it was marked %q", store.statuses["i"])
	}
}

// Turning isolated sleep on starts every isolated app's clock then, not at
// its last visit: a pod nobody opened last week is not deleted on the first
// tick after the switch, which would read as the setting misfiring.
func TestReaper_TurningIsolatedOnGivesAFreshGrace(t *testing.T) {
	store := &fakeStore{
		apps:     []database.App{sharedApp("s"), isolatedApp("i")},
		settings: database.PlatformSettings{IdleSleepMinutes: 60},
	}
	r, _, _, c := newSplitReaper(t, store, true)

	// A week of sweeps with only the shared half on. The shared app is
	// touched so it stays out of the picture.
	r.tick()
	for i := 0; i < 7*24; i++ {
		c.advance(time.Hour)
		r.tracker.Touch("s")
		r.tick()
	}

	store.settings.IsolatedIdleSleepMinutes = 30
	c.advance(time.Minute)
	r.tick()
	if store.statuses["i"] != "" {
		t.Fatalf("the first sweep after isolated sleep was turned on slept an app at once: %q", store.statuses["i"])
	}
	c.advance(31 * time.Minute)
	r.tick()
	if store.statuses["i"] != database.AppStatusSleeping {
		t.Fatalf("status = %q, want sleeping a full timeout after the switch", store.statuses["i"])
	}
}

// windowSettings is an isolated timeout of 30 minutes confined to a
// 19:00–07:00 UTC window.
func windowSettings() database.PlatformSettings {
	return database.PlatformSettings{
		IsolatedIdleSleepMinutes:   30,
		IsolatedSleepWindowEnabled: true,
		IsolatedSleepWindowStart:   "19:00",
		IsolatedSleepWindowEnd:     "07:00",
	}
}

// With the window on an isolated app sleeps only inside it — an app idle all
// afternoon is slept on the first sweep after the window opens, not before.
func TestReaper_TheWindowConfinesIsolatedSleep(t *testing.T) {
	store := &fakeStore{apps: []database.App{isolatedApp("i")}, settings: windowSettings()}
	r, _, cluster, c := newSplitReaper(t, store, true)

	// base is 12:00 UTC.
	r.tick()
	c.advance(6 * time.Hour) // 18:00, idle for six hours
	r.tick()
	if store.statuses["i"] != "" || stopped(t, cluster, "i-rel") {
		t.Fatalf("slept outside the window: %q", store.statuses["i"])
	}
	c.advance(time.Hour) // 19:00
	r.tick()
	if store.statuses["i"] != database.AppStatusSleeping {
		t.Fatalf("status = %q at the window's start, want sleeping", store.statuses["i"])
	}
}

// The window does not reach shared apps: their timeout decides at any hour.
func TestReaper_TheWindowIsIsolatedOnly(t *testing.T) {
	settings := windowSettings()
	settings.IdleSleepMinutes = 5
	store := &fakeStore{apps: []database.App{sharedApp("s")}, settings: settings}
	r, _, _, c := newSplitReaper(t, store, true)
	r.tick()
	c.advance(time.Hour) // 13:00, outside the window
	r.tick()
	if store.statuses["s"] != database.AppStatusSleeping {
		t.Fatalf("shared app status = %q at midday, want sleeping — the window is isolated-only", store.statuses["s"])
	}
}

// Once the window has closed every isolated app it left asleep is woken, one
// at a time, and nothing else is: not a locked app, not a shared one, and not
// one somebody stopped.
func TestReaper_AClosedWindowWakesItsAppsOneByOne(t *testing.T) {
	locked := isolatedApp("locked")
	now := base
	locked.LockedAt = &now
	stoppedApp := isolatedApp("stopped")
	stoppedApp.Status = database.AppStatusStopped
	sharedAsleep := sharedApp("shared")
	sharedAsleep.Status = database.AppStatusSleeping

	a, b := isolatedApp("a"), isolatedApp("b")
	a.Status, b.Status, locked.Status = database.AppStatusSleeping, database.AppStatusSleeping, database.AppStatusSleeping

	settings := windowSettings()
	settings.IdleSleepMinutes = 600 // shared on, so the shared app is a real candidate it must skip
	store := &fakeStore{apps: []database.App{a, b, locked, stoppedApp, sharedAsleep}, settings: settings}
	r, _, _, _ := newSplitReaper(t, store, true)

	var mu sync.Mutex
	var order []string
	inFlight, most := 0, 0
	done := make(chan struct{}, 8)
	r.WithWaker(NewWakeGroup(), func(app *database.App) error {
		mu.Lock()
		inFlight++
		if inFlight > most {
			most = inFlight
		}
		order = append(order, app.ID)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_ = store.UpdateAppStatus(app.ID, database.AppStatusRunning)
		done <- struct{}{}
		return nil
	})

	r.tick() // 12:00 UTC: the window is closed
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v were woken", order)
		}
	}
	// A second sweep while nothing is left asleep starts nothing.
	r.tick()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("woken = %v, want [a b]", order)
	}
	if most != 1 {
		t.Fatalf("%d wakes ran at once, want one at a time", most)
	}
}

// Inside the window nothing is woken, and with the window off a sleeping app
// waits for its visitor as it always has.
func TestReaper_NoQueueInsideTheWindowOrWithoutIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings database.PlatformSettings
		advance  time.Duration
	}{
		{"inside the window", windowSettings(), 8 * time.Hour}, // 20:00
		{"window off", func() database.PlatformSettings {
			s := windowSettings()
			s.IsolatedSleepWindowEnabled = false
			return s
		}(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := isolatedApp("a")
			a.Status = database.AppStatusSleeping
			store := &fakeStore{apps: []database.App{a}, settings: tc.settings}
			r, _, _, c := newSplitReaper(t, store, true)
			woken := make(chan string, 1)
			r.WithWaker(NewWakeGroup(), func(app *database.App) error { woken <- app.ID; return nil })
			c.advance(tc.advance)
			r.tick()
			select {
			case id := <-woken:
				t.Fatalf("%s was woken", id)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

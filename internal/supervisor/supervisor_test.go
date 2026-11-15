package supervisor

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
)

// --- doubles ---

type fakeStore struct {
	mu         sync.Mutex
	app        *database.App
	pending    bool
	pendingErr error
	statuses   []string
	missing    bool
}

func (f *fakeStore) GetApp(id string) (*database.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing {
		return nil, nil
	}
	copied := *f.app
	return &copied, nil
}

func (f *fakeStore) UpdateAppStatus(id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, status)
	f.app.Status = status
	return nil
}

func (f *fakeStore) UpdateAppStatusFrom(id, from, status string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.app.Status != from {
		return false, nil
	}
	f.statuses = append(f.statuses, status)
	f.app.Status = status
	return true, nil
}

func (f *fakeStore) HasPendingDeployment(appID string, since time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, f.pendingErr
}

type fakeStarter struct {
	mu    sync.Mutex
	calls int
	err   error
	// afterN makes the start succeed from the Nth call onward.
	afterN int
}

func (f *fakeStarter) Start(app *database.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.afterN > 0 && f.calls >= f.afterN {
		return nil
	}
	return f.err
}

func (f *fakeStarter) Halt(app *database.App) error { return nil }

func (f *fakeStarter) StopIfNoLongerWanted(appID string) {}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recordingStore struct {
	mu     sync.Mutex
	events []*database.AppEvent
}

func (r *recordingStore) InsertAppEvent(e *database.AppEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.ID = "e"
	copied := *e
	r.events = append(r.events, &copied)
	return nil
}
func (r *recordingStore) ClaimEventNotify(string, string, string, string, time.Duration) (bool, error) {
	return false, nil
}
func (r *recordingStore) MarkEventNoticeSent(string) error                     { return nil }
func (r *recordingStore) ResolveAppEvents(string, string, string) (int, error) { return 0, nil }
func (r *recordingStore) AppNotifyFailures(string) (bool, error)               { return true, nil }
func (r *recordingStore) ListAppRecipients(string) ([]database.Recipient, error) {
	return nil, nil
}
func (r *recordingStore) GetApp(string) (*database.App, error) { return nil, nil }

func (r *recordingStore) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

// --- fixtures ---

func crashedApp() *database.App {
	return &database.App{
		ID: "app-1", Slug: "reports", Name: "Reports",
		Status:         database.AppStatusCrashed,
		RunningMode:    string(runtime.ModeIsolated),
		CurrentRelease: "reports-abc12345",
		ReleaseRef:     "oci://registry/reports:abc12345",
	}
}

// newSupervisor returns a supervisor whose clock the test drives.
func newSupervisor(t *testing.T, app *database.App, starter *fakeStarter) (*Supervisor, *fakeStore, *recordingStore, *time.Time) {
	t.Helper()
	store := &fakeStore{app: app}
	rec := &recordingStore{}
	clock := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(store, starter, events.NewRecorder(rec, nil, nil))
	s.now = func() time.Time { return clock }
	return s, store, rec, &clock
}

// advance moves the supervisor's clock and runs a tick, synchronously — tick
// dispatches attempts on goroutines it tracks, so waiting on the group is what
// makes the assertions that follow deterministic.
func advance(s *Supervisor, clock *time.Time, d time.Duration) {
	*clock = clock.Add(d)
	s.tick()
	s.wg.Wait()
}

// --- eligibility: the list is the specification ---

func TestOnlyIsolatedCrashedAppsAreScheduled(t *testing.T) {
	locked := time.Now()
	cases := []struct {
		name   string
		mutate func(*database.App)
	}{
		{"shared mode is PM2's", func(a *database.App) { a.RunningMode = string(runtime.ModeShared) }},
		{"a locked app is never started", func(a *database.App) { a.LockedAt = &locked }},
		{"no release to start", func(a *database.App) { a.CurrentRelease = "" }},
		{"no published release", func(a *database.App) { a.ReleaseRef = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := crashedApp()
			tc.mutate(app)
			starter := &fakeStarter{}
			s, _, _, clock := newSupervisor(t, app, starter)

			s.Notice(app)
			advance(s, clock, time.Hour)

			if starter.count() != 0 {
				t.Fatalf("started %d time(s), want none", starter.count())
			}
		})
	}
}

// `stopped` is somebody's decision and a supervisor that argues with a person
// is not a supervisor. The status is re-read at the attempt, so a stop that
// lands between the crash and the restart cancels it.
func TestAStopBetweenTheCrashAndTheAttemptCancelsIt(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{}
	s, store, _, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	store.mu.Lock()
	store.app.Status = database.AppStatusStopped
	store.mu.Unlock()

	advance(s, clock, 2*time.Minute)
	if starter.count() != 0 {
		t.Fatalf("started a stopped app %d time(s)", starter.count())
	}
	if len(s.state) != 0 {
		t.Fatalf("state kept for an app that is no longer this loop's business: %v", s.state)
	}
}

// A deploy in flight is already replacing the workload.
func TestADeployInFlightSuppressesTheAttempt(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{}
	s, store, _, clock := newSupervisor(t, app, starter)
	store.pending = true

	s.Notice(app)
	advance(s, clock, 2*time.Minute)
	if starter.count() != 0 {
		t.Fatalf("started %d time(s) with a deploy in flight", starter.count())
	}
}

// --- the schedule ---

func TestTheFirstAttemptWaitsAMinute(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{err: errors.New("no")}
	s, _, _, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	advance(s, clock, 59*time.Second)
	if starter.count() != 0 {
		t.Fatalf("started after %s, want a minute's wait", 59*time.Second)
	}
	advance(s, clock, 2*time.Second)
	if starter.count() != 1 {
		t.Fatalf("started %d time(s) after a minute, want 1", starter.count())
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := backoff(i); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i, got, w)
		}
	}
}

// Five attempts, then an ending with words on it.
func TestItGivesUpAfterFiveAttempts(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{err: errors.New("image pull failed")}
	s, store, rec, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	// Every delay in the sequence, plus a long tail that would carry a sixth.
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
		advance(s, clock, d)
	}

	if starter.count() != maxAttempts {
		t.Fatalf("started %d time(s), want %d", starter.count(), maxAttempts)
	}
	kinds := rec.kinds()
	if len(kinds) != 1 || kinds[0] != string(events.KindAppNotRestarting) {
		t.Fatalf("recorded %v, want one %s", kinds, events.KindAppNotRestarting)
	}
	ev := rec.events[0]
	if ev.Fingerprint != app.CurrentRelease {
		t.Errorf("fingerprint %q, want the release %q", ev.Fingerprint, app.CurrentRelease)
	}
	if ev.Severity != database.EventSeverityError {
		t.Errorf("severity %q, want error", ev.Severity)
	}
	// 6c: the copy names the release and the count and says what to press.
	for _, want := range []string{app.CurrentRelease, "5 times", "deploy or restart"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("the ending does not mention %q: %s", want, ev.Detail)
		}
	}
	// D6: no sixth status. The app is left crashed.
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.app.Status != database.AppStatusCrashed {
		t.Fatalf("status is %q, want it left crashed", store.app.Status)
	}
}

// The ending is what the interstitial reads to stop promising a refresh.
func TestRetryingStopsAtTheEnding(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{err: errors.New("no")}
	s, _, _, clock := newSupervisor(t, app, starter)

	if !s.Retrying(app) {
		t.Fatal("a crashed isolated app with nothing recorded is still going to be tried")
	}
	s.Notice(app)
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute} {
		advance(s, clock, d)
	}
	if s.Retrying(app) {
		t.Fatal("still promising a retry after the supervisor gave up")
	}
}

// A shared-mode app is PM2's, and a nil supervisor is an install without one.
// Both keep the promise the page has always made.
func TestRetryingIsTrueWhereThisLoopHasNoSay(t *testing.T) {
	shared := crashedApp()
	shared.RunningMode = string(runtime.ModeShared)
	s, _, _, _ := newSupervisor(t, shared, &fakeStarter{})
	if !s.Retrying(shared) {
		t.Fatal("a shared app's page stopped promising a refresh")
	}
	var none *Supervisor
	if !none.Retrying(crashedApp()) {
		t.Fatal("an install with no supervisor stopped promising a refresh")
	}
}

// A start that works writes `running` — from what was observed, since Start
// returns once a readiness probe has answered.
func TestASuccessfulRestartMarksTheAppRunning(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{afterN: 1}
	s, store, rec, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	advance(s, clock, time.Minute)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.statuses) != 1 || store.statuses[0] != database.AppStatusRunning {
		t.Fatalf("statuses written %v, want one running", store.statuses)
	}
	if len(rec.kinds()) != 0 {
		t.Fatalf("recorded %v on a successful restart", rec.kinds())
	}
}

// The count survives a restart that comes up and falls over again inside the
// same release — otherwise a workload that dies every few minutes is restarted
// forever and the cap means nothing.
func TestTheCountIsKeptAcrossARecoveryWithinTheSameRelease(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{afterN: 1} // always succeeds
	s, store, rec, clock := newSupervisor(t, app, starter)

	for i := 0; i < maxAttempts; i++ {
		store.mu.Lock()
		store.app.Status = database.AppStatusCrashed
		store.mu.Unlock()
		app.Status = database.AppStatusCrashed
		s.Notice(app)
		advance(s, clock, time.Hour)
	}
	if starter.count() != maxAttempts {
		t.Fatalf("started %d time(s), want %d", starter.count(), maxAttempts)
	}
	// The sixth crash on the same release is not scheduled, and it is where the
	// ending is said: five restarts that each held for a few minutes is still
	// an app that did not stay up.
	store.mu.Lock()
	store.app.Status = database.AppStatusCrashed
	store.mu.Unlock()
	app.Status = database.AppStatusCrashed
	s.Notice(app)
	advance(s, clock, time.Hour)
	if starter.count() != maxAttempts {
		t.Fatalf("started %d time(s) on the same release, want the cap to hold at %d", starter.count(), maxAttempts)
	}
	if kinds := rec.kinds(); len(kinds) != 1 || kinds[0] != string(events.KindAppNotRestarting) {
		t.Fatalf("recorded %v, want one %s once the attempts were spent", kinds, events.KindAppNotRestarting)
	}
	if s.Retrying(app) {
		t.Fatal("still promising a retry with the attempts spent")
	}
}

// A deploy is a clean slate, and nothing has to clear the state for that to be
// true: the attempts are counted against the release.
func TestANewReleaseResetsTheCount(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{err: errors.New("no")}
	s, store, _, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute} {
		advance(s, clock, d)
	}
	if s.Retrying(app) {
		t.Fatal("expected the ending before the deploy")
	}

	deployed := crashedApp()
	deployed.CurrentRelease = "reports-def67890"
	store.mu.Lock()
	store.app = deployed
	store.mu.Unlock()

	if !s.Retrying(deployed) {
		t.Fatal("a new release is still refused after the previous one's ending")
	}
	s.Notice(deployed)
	advance(s, clock, time.Minute)
	if starter.count() != maxAttempts+1 {
		t.Fatalf("started %d time(s), want the new release to have been tried", starter.count())
	}
}

// "until you deploy or restart it" — the second half of the sentence.
func TestAManualRestartResetsTheCount(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{err: errors.New("no")}
	s, _, _, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute} {
		advance(s, clock, d)
	}
	if s.Retrying(app) {
		t.Fatal("expected the ending")
	}

	s.Reset(app.ID)
	if !s.Retrying(app) {
		t.Fatal("a person pressing Restart did not clear the ending")
	}
	s.Notice(app)
	advance(s, clock, time.Minute)
	if starter.count() != maxAttempts+1 {
		t.Fatalf("started %d time(s), want another attempt after a manual restart", starter.count())
	}
}

// One crash noticed twice is one schedule.
func TestNoticeIsIdempotentWhileAnAttemptIsPending(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{err: errors.New("no")}
	s, _, _, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	s.Notice(app)
	s.Notice(app)
	advance(s, clock, time.Minute)
	if starter.count() != 1 {
		t.Fatalf("started %d time(s) for one crash, want 1", starter.count())
	}
}

// An app that has been deleted is not restarted, and its state does not linger.
func TestADeletedAppIsForgotten(t *testing.T) {
	app := crashedApp()
	starter := &fakeStarter{}
	s, store, _, clock := newSupervisor(t, app, starter)

	s.Notice(app)
	store.mu.Lock()
	store.missing = true
	store.mu.Unlock()

	advance(s, clock, time.Minute)
	if starter.count() != 0 {
		t.Fatalf("started a deleted app %d time(s)", starter.count())
	}
	if len(s.state) != 0 {
		t.Fatalf("state kept for a deleted app: %v", s.state)
	}
}

// stoppingStarter is a start during which somebody presses Stop: the row says
// stopped by the time Start returns.
type stoppingStarter struct {
	fakeStarter
	store *fakeStore
}

func (s *stoppingStarter) Start(app *database.App) error {
	s.store.mu.Lock()
	s.store.app.Status = database.AppStatusStopped
	s.store.mu.Unlock()
	return s.fakeStarter.Start(app)
}

// A Stop that lands while the attempt's Start is in flight stands: the
// supervisor writes running only over the crashed it saw (code review
// 2026-11-11, Q-4).
func TestAStopDuringTheAttemptIsNotOverwrittenWithRunning(t *testing.T) {
	app := crashedApp()
	starter := &stoppingStarter{fakeStarter: fakeStarter{afterN: 1}}
	s, store, _, clock := newSupervisor(t, app, &starter.fakeStarter)
	starter.store = store
	s.starter = starter

	s.Notice(app)
	advance(s, clock, 2*time.Minute)
	if starter.count() != 1 {
		t.Fatalf("started %d time(s), want one attempt", starter.count())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.app.Status != database.AppStatusStopped {
		t.Fatalf("status = %q after a Stop during the start, want stopped", store.app.Status)
	}
}

// Package supervisor restarts an isolated workload that has crashed, a bounded
// number of times, and says so out loud when it stops trying.
//
// Shared mode has a process supervisor — the shared-mode agent — which restarts
// a dead process in place, on the kubelet's backoff and without ever giving
// up, and keeps its own counter. Its restarts carry no stale credential: the
// agent issues each process's identity as it starts it. Isolated mode has none, and that
// reads like an oversight but is not: Kubernetes *is* a supervisor, and the
// isolated backend disables the only recovery it knows how to perform, as a
// consequence of how a workload is given its secrets. A serve pod's spec
// carries a bootstrap token that expires fifteen minutes after the platform
// minted it; a kubelet restart re-execs the runner against the same immutable
// spec and the same dead token, is refused, and tries again. What an operator
// sees is CrashLoopBackOff and a 401, which points at the wrong subsystem.
//
// So the platform supervises, and the shape is Nomad's rather than kubelet's:
// a bounded number of attempts, and then a declared ending. Unbounded retry
// against a genuinely broken release is a busy loop that reads as activity;
// the useful thing is an ending with words on it, which is what §3d's event and
// the interstitial's dropped promise are between them.
//
// Three properties are what make it safe to leave running:
//
//   - It never starts anything the platform did not already believe should be
//     running. Eligibility is re-read from the database immediately before
//     every attempt, so a lock, a stop, or a deploy in flight that arrived
//     after the crash cancels the restart rather than racing it.
//   - Attempts are counted per *release*, so a deploy is a clean slate without
//     anything having to clear state.
//   - The state is in memory and a platform restart clears it. Boot Reconcile
//     already retries the same restore, and an operator who has just restarted
//     the platform is entitled to one more attempt.
package supervisor

import (
	"fmt"
	"log"
	"sync"
	"time"

	"toft/internal/applog"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
)

const (
	// maxAttempts is the cap. Five, and the number is in the giving-up event's
	// text: a setting here would invite tuning a symptom, and the honest answer
	// to "five is sometimes wrong" is better copy rather than a knob.
	maxAttempts = 5
	// backoffStart is the first delay, and it doubles from there. The first
	// attempt is already two minutes after the crash, because the health
	// watcher's two-sweep guard runs first — so a transient failure is covered
	// early and a broken release stops burning a pod schedule every minute
	// inside half an hour.
	backoffStart = time.Minute
	// backoffMax caps the doubling: 1, 2, 4, 8, 15.
	backoffMax = 15 * time.Minute
	// tickInterval is how often the schedule is consulted. It walks an
	// in-memory map and touches the database only for an attempt that is
	// actually due, so it is cheap enough to be finer-grained than the delays
	// it is dispatching.
	tickInterval = 15 * time.Second
	// pendingWindow matches the health watcher's: a pending deployment row
	// older than this is an orphan of a restart rather than work in flight, and
	// must not suppress recovery forever.
	pendingWindow = 30 * time.Minute
)

// Store is the database surface this needs, named here rather than taken as
// *database.DB so the queries it depends on are visible in one place and a test
// can substitute one.
type Store interface {
	GetApp(id string) (*database.App, error)
	UpdateAppStatus(id, status string) error
	// UpdateAppStatusFrom writes status only while the row still says from:
	// the attempt's start runs for a whole readiness window, and a Stop made
	// in it stands (Q-4).
	UpdateAppStatusFrom(id, from, status string) (bool, error)
	HasPendingDeployment(appID string, since time.Time) (bool, error)
}

// Starter is the one action. It is Builder.Start, and there is no second start
// path — the supervisor is simply the fifth caller on that function's list.
//
// Halt and StopIfNoLongerWanted are the two things a start that did not go as
// planned owes afterwards, and both are Builder methods for the same reason
// Start is: the builder holds the start lock they must take.
type Starter interface {
	Start(app *database.App) error
	// Halt removes the app's current workload, for a restart that was owed and
	// failed before it replaced the old one (Builder.Halt).
	Halt(app *database.App) error
	// StopIfNoLongerWanted re-reads the row after a start whose guarded status
	// write did not move, and stops the workload again if the app is now
	// stopped or locked (Builder.StopIfNoLongerWanted).
	StopIfNoLongerWanted(appID string)
}

// Supervisor restarts crashed isolated workloads, bounded, and records the
// ending.
type Supervisor struct {
	store   Store
	starter Starter
	events  *events.Recorder

	// narrator is where this actor's decisions about an app reach that app's
	// owner. Nil is a real configuration and every call through it is then a
	// no-op — the operator's log.Printf beside each one is unaffected either
	// way.
	narrator *applog.Sink

	// now is swappable so a test drives time rather than sleeping through it.
	now func() time.Time

	mu    sync.Mutex
	state map[string]*appState
	// halted is the shared-mode apps a failed remount halted and marked
	// crashed, by app id to the release halted. Nothing restarts those — the
	// agent's workload is gone, and this loop is the isolated backend's — so
	// Retrying must not promise the refresh. Cleared by forget (a person's
	// start) and moot once the release changes (a deploy).
	halted map[string]string

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

type appState struct {
	// release is what the attempts are counted against. A different one means
	// a deploy happened and this is a new problem.
	release string
	// attempts is how many starts have been made for this release.
	attempts int
	// nextAt is when the next attempt is due; zero means nothing is scheduled.
	nextAt time.Time
	// inFlight is an attempt currently running. A start can take as long as the
	// readiness timeout, which is longer than the tick interval.
	inFlight bool
	// gaveUp is the ending. It is what the interstitial reads to stop promising
	// a refresh, and it is cleared only by a deploy or a deliberate restart.
	gaveUp bool
}

func New(store Store, starter Starter, rec *events.Recorder) *Supervisor {
	return &Supervisor{
		store: store, starter: starter, events: rec,
		now:   func() time.Time { return time.Now().UTC() },
		state: map[string]*appState{},
		stop:  make(chan struct{}),
	}
}

// WithNarrator wires the owner-facing half of this actor's log.
func (s *Supervisor) WithNarrator(n *applog.Sink) *Supervisor {
	s.narrator = n
	return s
}

// Start runs the schedule for the life of the process.
func (s *Supervisor) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(tickInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.tick()
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Supervisor) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Notice is the health watcher's callback, invoked after it has written
// `crashed`. The watcher decides; this acts — which is why the recovery loop is
// somewhere a reader can find without reading a sweep.
//
// It schedules; it does not start. Nothing here blocks the sweep that called
// it.
func (s *Supervisor) Notice(app *database.App) {
	if s == nil || app == nil {
		return
	}
	// The cheap half of eligibility, so an app this will never restart is never
	// given a schedule. The expensive half is re-read at the attempt.
	if !eligible(app) {
		s.forget(app.ID)
		return
	}

	s.mu.Lock()
	st, ok := s.state[app.ID]
	if !ok || st.release != app.CurrentRelease {
		// A new release is a clean slate, including one that follows an ending.
		st = &appState{release: app.CurrentRelease}
		s.state[app.ID] = st
	}
	if st.gaveUp || st.inFlight || !st.nextAt.IsZero() {
		s.mu.Unlock()
		return
	}
	if st.attempts >= maxAttempts {
		s.mu.Unlock()
		// The attempts are spent on a release that came back and fell over
		// again. Five restarts that each held for a few minutes is still an app
		// that did not stay up, and the cap has to mean the same thing there as
		// it does when the starts fail outright — otherwise a workload that
		// dies every few minutes is restarted forever.
		s.giveUp(app)
		return
	}
	st.nextAt = s.now().Add(backoff(st.attempts))
	attempts := st.attempts
	s.mu.Unlock()

	log.Printf("supervisor: %s crashed on release %s — restart attempt %d of %d in %s",
		app.Slug, app.CurrentRelease, attempts+1, maxAttempts, backoff(attempts))
}

// Retrying reports whether this app is still being restarted, which is what the
// crashed interstitial's promise of an automatic refresh depends on. A page
// that promises to refresh and does not is worse than one that never promised.
//
// A nil supervisor answers yes, and so does a shared-mode app: this is the
// isolated backend's recovery loop, and the agent is still restarting the other
// kind — except a shared app a failed remount halted, which nothing restarts.
func (s *Supervisor) Retrying(app *database.App) bool {
	if s == nil || app == nil {
		return true
	}
	if app.RunningModeOrShared() != runtime.ModeIsolated {
		s.mu.Lock()
		halted, ok := s.halted[app.ID]
		s.mu.Unlock()
		// Except one a failed remount halted: its workload is gone and the
		// agent has nothing left to restart.
		return !ok || halted != app.CurrentRelease
	}
	if !eligible(app) {
		// Never going to be restarted by anything: no release to start, or a
		// lock, which the proxy answers before this in any case.
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[app.ID]
	if !ok || st.release != app.CurrentRelease {
		// Nothing recorded against this release — a platform restart clears the
		// state, and boot Reconcile is the attempt that follows it.
		return true
	}
	return !st.gaveUp
}

// Reset forgets everything counted against an app, and is called when a person
// starts or restarts it by hand. The ending says "until you deploy or restart
// it", and this is the second half of that sentence; the first is the release
// changing under a deploy.
func (s *Supervisor) Reset(appID string) {
	s.forget(appID)
}

func (s *Supervisor) forget(appID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.state, appID)
	delete(s.halted, appID)
	s.mu.Unlock()
}

// markHalted records that a failed remount halted a shared-mode app on this
// release (see halted).
func (s *Supervisor) markHalted(appID, release string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.halted == nil {
		s.halted = map[string]string{}
	}
	s.halted[appID] = release
}

// tick dispatches every attempt that is due.
func (s *Supervisor) tick() {
	now := s.now()
	var due []string

	s.mu.Lock()
	for id, st := range s.state {
		if st.gaveUp || st.inFlight || st.nextAt.IsZero() || st.nextAt.After(now) {
			continue
		}
		// Claimed here rather than in the attempt, so a slow start cannot be
		// dispatched twice by the next tick.
		st.nextAt = time.Time{}
		st.inFlight = true
		due = append(due, id)
	}
	s.mu.Unlock()

	for _, id := range due {
		s.wg.Add(1)
		go func(id string) {
			defer s.wg.Done()
			s.attempt(id)
		}(id)
	}
}

// attempt is one restart, from the app as it is now rather than as it was when
// the crash was noticed.
func (s *Supervisor) attempt(id string) {
	app, err := s.store.GetApp(id)
	if err != nil || app == nil {
		if err != nil {
			log.Printf("supervisor: reading app %s: %v", id, err)
		}
		s.forget(id)
		return
	}
	if !s.eligibleNow(app) {
		// Somebody stopped it, locked it, deployed it, or it came back on its
		// own. None of those is this loop's business any more.
		s.forget(id)
		return
	}

	s.mu.Lock()
	st, ok := s.state[id]
	if !ok || st.release != app.CurrentRelease {
		// The release changed between the schedule and the attempt.
		s.mu.Unlock()
		s.forget(id)
		return
	}
	st.attempts++
	n := st.attempts
	s.mu.Unlock()

	log.Printf("supervisor: restarting %s (release %s, attempt %d of %d)", app.Slug, app.CurrentRelease, n, maxAttempts)
	// Every attempt, not only the ending. The event for the ending already
	// exists and says "we stopped trying"; what an owner cannot see today is
	// that the platform tried at all, which is the difference between an app
	// nobody noticed and an app the platform fought for and lost.
	s.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStdout,
		"Restarting after a crash — attempt %d of %d.", n, maxAttempts)
	startErr := s.starter.Start(app)

	s.mu.Lock()
	st, ok = s.state[id]
	if ok {
		st.inFlight = false
	}
	s.mu.Unlock()
	if !ok {
		// Reset while the start was in flight — a deploy or a person. Whatever
		// it did is theirs now.
		return
	}

	if startErr == nil {
		// Status is written from what was observed: Start returns once a
		// readiness probe has answered. The event stays open until the watcher
		// sees the workload online, which is the same recovery every other
		// path gets.
		// From crashed, which is what eligibleNow saw: a Stop or a lock that
		// landed while Start ran wrote its own word, and this must not write
		// running over it (code review 2026-11-11, Q-4).
		if moved, err := s.store.UpdateAppStatusFrom(app.ID, database.AppStatusCrashed, database.AppStatusRunning); err != nil {
			log.Printf("supervisor: marking %s running: %v", app.Slug, err)
		} else if !moved {
			// The row is theirs; the workload this start placed is not left
			// live under it (DB-U1).
			log.Printf("supervisor: %s was stopped or locked while it restarted; leaving the row as it is", app.Slug)
			s.starter.StopIfNoLongerWanted(app.ID)
		}
		log.Printf("supervisor: %s is running again after attempt %d", app.Slug, n)
		s.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStdout,
			"The app started and is answering again after attempt %d.", n)
		// The count is deliberately kept. An app that comes back and falls over
		// again within the same release is on its next attempt, not its first —
		// otherwise a workload that dies every three minutes is restarted
		// forever and the cap means nothing.
		return
	}

	log.Printf("supervisor: restarting %s failed: %v", app.Slug, startErr)
	// startErr is not relayed. It is whatever the backend wrapped — the
	// agent's socket, an artifact endpoint, a `database/sql` error — and the backend
	// has already said its own owner-facing sentence about the failure through
	// its narrator. This line is the supervisor's fact: the attempt did not
	// take.
	s.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStderr,
		"Attempt %d did not bring the app back up.", n)
	if n >= maxAttempts {
		s.giveUp(app)
		return
	}
	s.mu.Lock()
	if st, ok := s.state[id]; ok && !st.gaveUp {
		st.nextAt = s.now().Add(backoff(n))
	}
	s.mu.Unlock()
}

// giveUp is the ending, and it has words on it.
func (s *Supervisor) giveUp(app *database.App) {
	s.mu.Lock()
	if st, ok := s.state[app.ID]; ok {
		st.gaveUp = true
		st.nextAt = time.Time{}
	}
	s.mu.Unlock()

	log.Printf("supervisor: giving up on %s after %d attempts on release %s", app.Slug, maxAttempts, app.CurrentRelease)
	// The last line of this release's narration, and it says what happens next
	// rather than only what stopped — the event beside it carries the same
	// fact to whoever is on the notification list.
	s.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStderr,
		"The platform has stopped restarting this app after %d attempts. "+
			"It will not be restarted again until you deploy or restart it.", maxAttempts)

	// Fingerprinted on the release, so a broken release produces exactly one of
	// these however many times it is rediscovered — and so an owner gets "it
	// crashed" and then "we stopped trying", rather than seven of the first.
	s.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppNotRestarting,
		Severity: database.EventSeverityError,
		Source:   database.EventSourceHealth,
		Summary:  "The platform has stopped restarting this app",
		// Was "The app's logs will say why it is exiting", which was a hedge
		// about a pane that had nothing in it: by the time an owner followed
		// this the workload was gone and so was everything it printed. Each
		// attempt is narrated into the app's log now, and that log outlives the
		// workload, so this names what is actually there.
		Detail: fmt.Sprintf("Release %s was restarted %d times without staying up, so the platform has "+
			"stopped trying. It will not be restarted again until you deploy or restart it. "+
			"The app's log lists every attempt.", app.CurrentRelease, maxAttempts),
		Fingerprint: app.CurrentRelease,
	})
}

// eligibleNow is eligible plus the two questions only the database can answer.
func (s *Supervisor) eligibleNow(app *database.App) bool {
	if !eligible(app) {
		return false
	}
	if app.Status != database.AppStatusCrashed {
		return false
	}
	// A deploy in flight is already replacing this workload, and starting the
	// old release underneath it is how two workloads end up holding one
	// address.
	pending, err := s.store.HasPendingDeployment(app.ID, s.now().Add(-pendingWindow))
	if err != nil {
		log.Printf("supervisor: %s: checking pending deployments: %v", app.Slug, err)
		return false
	}
	return !pending
}

// eligible is what can be decided from the row alone. Each exclusion has a
// reason and the list is the specification:
//
//   - **Locked apps.** Unlocking never starts a workload, and a supervisor that
//     started one would be an eighth enforcement point wired backwards.
//   - **`stopped`.** Somebody decided that. A supervisor that argues with a
//     person is not a supervisor.
//   - **`sleeping`.** Shared mode only, and the proxy owns waking it.
//   - **Shared-mode apps.** The agent supervises them, restarting in place as
//     the kubelet does.
//   - **No release, or no published release reference.** There is nothing to
//     start: an isolated workload fetches its release rather than finding it on
//     disk, so a row without one can only be fixed by a deploy.
func eligible(app *database.App) bool {
	if app.Locked() {
		return false
	}
	if app.RunningModeOrShared() != runtime.ModeIsolated {
		return false
	}
	return app.CurrentRelease != "" && app.ReleaseRef != ""
}

// backoff is the delay before the attempt after n already made: 1, 2, 4, 8, 15.
func backoff(attempts int) time.Duration {
	d := backoffStart << attempts
	if d > backoffMax || d <= 0 {
		return backoffMax
	}
	return d
}

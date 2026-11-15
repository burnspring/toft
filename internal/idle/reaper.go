package idle

import (
	"context"
	"log"
	"sync"
	"time"

	"toft/internal/applog"
	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/telemetry"
)

// sweepInterval is how often the reaper looks. A minute is fine-grained enough
// that an administrator's change to the timeout takes effect while they are
// still on the page, and coarse enough that the sweep is invisible: it is two
// queries and a map walk.
const sweepInterval = time.Minute

// Store is the database surface the reaper needs, named here rather than taken
// as *database.DB so the set of queries it depends on is visible in one place
// and a test can substitute one.
type Store interface {
	ListAllApps() ([]database.App, error)
	GetPlatformSettings() (database.PlatformSettings, error)
	// GetApp and UpdateAppStatusFrom are the second look: the sweep's list is
	// a snapshot, and each stop before this one waits out a grace, so the row
	// is re-read before its workload is stopped and written only from the
	// `running` that was read.
	GetApp(id string) (*database.App, error)
	UpdateAppStatusFrom(id, from, status string) (bool, error)
}

// Reaper stops apps nobody has used for the configured timeout — shared-mode
// apps under one setting and isolated ones under another (ADR §16), each off
// unless an administrator sets it.
type Reaper struct {
	store   Store
	rts     *runtime.Runtimes
	tracker *Tracker

	// narrator is where this actor's decisions about an app reach that app's
	// owner. Nil is a real configuration and every call through it is then a
	// no-op — the operator's log.Printf beside each one is unaffected either
	// way.
	narrator *applog.Sink

	// now is swappable so a test drives time rather than sleeping through it.
	now func() time.Time

	// running is the set of apps that were `running` on the previous sweep, and
	// it exists to answer one question: is this workload new? An app in this
	// set has been up since at least the last tick; one that is running and
	// absent from it started in between, and its idle clock has to start with
	// it rather than carry on from the workload it replaced.
	//
	// Held here rather than in the tracker because the tracker cannot see a
	// transition — it is told about requests, and a restart is not one. This
	// sweep already reads every app's status once a minute, which makes it the
	// only thing on the install that watches every app come up, whoever started
	// it: a person pressing Restart, a deploy flipping to a new release, the
	// supervisor bringing a crashed workload back, or a wake.
	//
	// Empty at boot, so the first sweep treats every running app as new. That
	// is the grace period the package comment already promises after a platform
	// restart, now said out loud rather than left to the tracker's seeding.
	running map[string]struct{}

	// wakes and wake are the end-of-window wake (WithWaker): the gateway's own
	// WakeGroup, so a visitor arriving while the queue reaches their app joins
	// that start rather than making a second one, and the builder's Wake. Nil,
	// the window still confines sleep and nothing is woken when it closes.
	wakes *WakeGroup
	wake  func(*database.App) error
	// morning is true while a queue is working through the apps a closed
	// window left asleep. One queue at a time is the point: the window's apps
	// come back one by one, and a sweep that started a second beside the first
	// would make it two by two.
	morning   bool
	morningMu sync.Mutex

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func NewReaper(store Store, rts *runtime.Runtimes, tracker *Tracker) *Reaper {
	return &Reaper{
		store: store, rts: rts, tracker: tracker,
		now:     func() time.Time { return time.Now().UTC() },
		running: map[string]struct{}{},
		stop:    make(chan struct{}),
	}
}

// WithNarrator wires the owner-facing half of this actor's log.
func (r *Reaper) WithNarrator(n *applog.Sink) *Reaper {
	r.narrator = n
	return r
}

// WithWaker wires the end-of-window wake: wakes is the WakeGroup the gateway
// wakes through, wake the builder's Wake.
func (r *Reaper) WithWaker(wakes *WakeGroup, wake func(*database.App) error) *Reaper {
	r.wakes, r.wake = wakes, wake
	return r
}

// Start runs the sweep loop for the life of the process.
func (r *Reaper) Start() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.tick()
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *Reaper) Close() {
	r.once.Do(func() { close(r.stop) })
	r.wg.Wait()
}

// tick sleeps everything eligible, once.
func (r *Reaper) tick() {
	ctx, span := telemetry.Start(context.Background(), "idle.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepIdle, outcome, time.Since(start))
	}()

	// Read per tick, never cached at boot: an administrator turning idle sleep
	// off must see it stop within the minute, and a reaper that kept sleeping
	// apps afterwards would make the switch a lie. The same rule the job
	// scheduler follows for its own enabled flag.
	settings, err := r.store.GetPlatformSettings()
	if err != nil {
		// Fail closed. A settings read that fails must not stop anyone's app.
		log.Printf("idle: reading platform settings, skipping sweep: %v", err)
		return
	}
	if settings.IdleSleepMinutes <= 0 && settings.IsolatedIdleSleepMinutes <= 0 {
		// Off is not a failure. A sweep that ran and found the feature disabled
		// did exactly what it was supposed to.
		outcome = telemetry.OutcomeSkipped
		return
	}

	apps, err := r.store.ListAllApps()
	if err != nil {
		log.Printf("idle: listing apps: %v", err)
		return
	}

	// One backend per mode, asked for once a sweep and only for a mode whose
	// timeout is on. A mode this install does not have — no cluster — leaves
	// its apps alone and the other mode's sweep unaffected: an isolated
	// timeout set on a box that has since lost its cluster must not stop
	// shared apps sleeping.
	backends := map[runtime.Mode]runtime.ServiceRuntime{}
	for _, mode := range []runtime.Mode{runtime.ModeShared, runtime.ModeIsolated} {
		if settings.IdleSleepMinutesFor(mode == runtime.ModeIsolated) <= 0 {
			continue
		}
		rt, err := r.rts.For(mode)
		if err != nil {
			log.Printf("idle: no %s backend, not sleeping %s apps: %v", mode, mode, err)
			continue
		}
		backends[mode] = rt
	}

	now := r.now()
	live := make([]string, 0, len(apps))
	// Rebuilt from scratch each sweep rather than mutated, so an app that stops,
	// is deleted or changes mode leaves the set by simply not being added to it.
	nowRunning := make(map[string]struct{}, len(r.running))
	for i := range apps {
		app := &apps[i]
		live = append(live, app.ID)

		// `running` alone excludes building, crashed, stopped, failed, and
		// already-sleeping — every one of which is either mid-flight or a state
		// a person chose.
		if app.Status != database.AppStatusRunning || app.CurrentRelease == "" {
			continue
		}
		// The running mode, not the desired one: an app promoted but not yet
		// redeployed is still a process here, and one demoted but not yet
		// redeployed is still a pod there — and it is the running workload
		// that the timeout and the backend below are about.
		//
		// An app whose mode is off is skipped before the bookkeeping, exactly
		// as every app is while the whole feature is off: it never joins the
		// running set, so the sweep that first finds its mode on reads it as
		// newly started and its clock begins then. Turning isolated sleep on
		// must not sleep, on the next tick, every pod nobody visited last week.
		mode := app.RunningModeOrShared()
		minutes := settings.IdleSleepMinutesFor(mode == runtime.ModeIsolated)
		rt := backends[mode]
		if minutes <= 0 || rt == nil {
			continue
		}
		threshold := time.Duration(minutes) * time.Minute
		// A workload this sweep has not seen running before started since the
		// last tick, and its idle clock starts with it. Without this the entry
		// the tracker holds is the *previous* workload's — an app that crashed
		// after a quiet hour comes back up already past every threshold, and the
		// next sweep sleeps it seconds after somebody pressed Restart, under a
		// line that says it went five minutes without a request.
		//
		// Whoever started it: this is deliberately not a list of the ways a
		// workload can come up, because such a list is exactly what drifts. A
		// restart, a deploy, a rollback, the supervisor after a crash and a
		// remount all arrive here as the same fact.
		//
		// The window it does not close is a workload that dies and is replaced
		// entirely between two ticks — the status read either side says
		// `running`, so nothing here can tell it apart from one that never
		// stopped. That is under a minute of a process nobody has asked for,
		// and closing it would mean asking the backend for an uptime per app
		// per sweep, which is what keeps this loop two queries and a map walk.
		if _, seen := r.running[app.ID]; !seen {
			r.tracker.Started(app.ID)
		}
		// The window that comment names is not under a minute for a slow app:
		// a marimo deploy on EFS was three minutes from start to flip, the row
		// read `running` throughout, and the new release was put to sleep
		// eighteen seconds after it began serving (trytoft drills rerun #2).
		// The row records the flip and every change of status, so the clock
		// starts no earlier than either.
		if app.ServingSince != nil {
			r.tracker.StartedAt(app.ID, *app.ServingSince)
		}
		if app.StatusChangedAt != nil {
			r.tracker.StartedAt(app.ID, *app.StatusChangedAt)
		}
		nowRunning[app.ID] = struct{}{}

		// Asked before the vetoes below, not after, because this is also what
		// seeds an app the tracker has not seen. Skipping it for exempt apps
		// would hand one a brand-new grace period the moment its exemption was
		// lifted, which reads as the exemption still applying.
		idle := r.tracker.IdleSince(app.ID, now, threshold)
		if !idle {
			continue
		}
		// The window confines when an isolated app may be slept, never whether
		// its clock runs: one idle since mid-afternoon is slept on the first
		// sweep inside the window.
		if mode == runtime.ModeIsolated && !settings.IsolatedSleepAllowedAt(now) {
			continue
		}
		// The administrator's exemption. Read from the app row this sweep
		// already loaded rather than a query of its own, and therefore as
		// fresh as everything else here: an exemption granted now takes effect
		// on the next tick.
		//
		// An app already `sleeping` when it is exempted stays asleep until
		// something wakes it — the exemption stops it being slept, it does not
		// start workloads. Nothing here can act on it anyway: the loop only
		// considers apps that are running.
		if app.NeverSleep {
			continue
		}

		// The row as it is now, not as the sweep found it. Every stop earlier in
		// this loop waited out a grace, so the list can be a minute old: a
		// person who pressed Stop since decided the app is stopped, and one who
		// pressed Restart or deployed started the workload this would kill.
		// Anything but the same `running` row is left alone, and so is an app
		// whose clock moved meanwhile — a request, or a Restart of a running
		// app, which the row cannot show (the word and the release stay) and
		// which restarts the tracker's clock for that reason.
		if !r.stillTheSame(app) || !r.tracker.IdleSince(app.ID, r.now(), threshold) {
			continue
		}

		// Stop, never Delete: Delete is the only thing that returns a port to
		// the pool, and the wake has to land on the address the row already
		// holds.
		// An absence is the state this sweep was going to produce, so it is not
		// a reason to leave the row saying `running`: the workload is gone and
		// the next request has to wake it.
		if err := rt.Stop(app.ProcessName()); err != nil && runtime.AbsenceReason(err) == "" {
			log.Printf("idle: stopping %s: %v", app.Slug, err)
			continue
		}
		// From `running` only: a Stop landing during the grace above is a
		// person's decision, and `sleeping` over it would let the next request
		// wake an app its owner stopped.
		moved, err := r.store.UpdateAppStatusFrom(app.ID, database.AppStatusRunning, database.AppStatusSleeping)
		if err != nil {
			log.Printf("idle: marking %s sleeping: %v", app.Slug, err)
			continue
		}
		if !moved {
			continue
		}
		// The counterpart of the wake latency the gateway records. Together they
		// are the cost and the saving of idle sleep in one query, which is the
		// only honest way to argue about the feature.
		telemetry.RecordAppTransition(ctx,
			database.AppStatusRunning, database.AppStatusSleeping, telemetry.ReasonIdle)
		log.Printf("idle: %s (%s) slept after %d minute(s) without a request", app.Slug, mode, minutes)
		// Why their app was not running at 3am. Nothing else on the install
		// tells an owner this: sleeping is not a failure, so it writes no
		// event, and the pane's own copy explains the state without ever
		// saying when it was entered.
		unit := "minutes"
		if minutes == 1 {
			unit = "minute"
		}
		r.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStdout,
			"Put to sleep after %d %s without a request. The next request wakes it.",
			minutes, unit)
	}

	// The app list this sweep already holds is the only place the tracker can
	// learn an app was deleted.
	r.tracker.Retain(live)
	// Only on a sweep that got this far. A tick that returned early — settings
	// unreadable, the feature off, no shared backend — read no statuses, and
	// replacing the set with what it did not look at would make every app read
	// as newly started on the tick after the feature is turned back on.
	r.running = nowRunning
	r.wakeAfterWindow(settings, apps, now)
	outcome = telemetry.OutcomeSucceeded
}

// wakeAfterWindow starts the queue that wakes, one at a time, every isolated
// app a closed sleep window left asleep.
//
// **It follows the state, not the moment.** Outside the window no isolated app
// is slept, so one found `sleeping` there is the window's to give back,
// whenever the window closed: a platform down across 07:00 wakes them on its
// first sweep, and one restarted mid-queue picks up where the last stopped.
// Watching for the instant the window closed would lose both.
//
// Only while the isolated timeout and the window are both on. With either off
// a sleeping app is what it has always been — asleep until somebody visits —
// and turning a setting off is not a reason to start pods.
func (r *Reaper) wakeAfterWindow(settings database.PlatformSettings, apps []database.App, now time.Time) {
	if r.wake == nil || r.wakes == nil || settings.IsolatedIdleSleepMinutes <= 0 ||
		!settings.IsolatedSleepWindowEnabled || settings.IsolatedSleepAllowedAt(now) {
		return
	}
	var queue []string
	for i := range apps {
		if wakeable(&apps[i]) {
			queue = append(queue, apps[i].ID)
		}
	}
	if len(queue) == 0 {
		return
	}
	r.morningMu.Lock()
	if r.morning {
		r.morningMu.Unlock()
		return
	}
	r.morning = true
	r.morningMu.Unlock()

	log.Printf("idle: the isolated sleep window has closed; waking %d app(s) one at a time", len(queue))
	// Not counted in r.wg: Close must not wait out a pod start that can take
	// minutes. The queue checks for a stop between apps, and a wake cut off by
	// the process exiting leaves a row reading `sleeping`, which the next
	// process's first sweep outside the window queues again.
	go func() {
		defer func() {
			r.morningMu.Lock()
			r.morning = false
			r.morningMu.Unlock()
		}()
		for _, id := range queue {
			select {
			case <-r.stop:
				return
			default:
			}
			r.wakeOne(id)
		}
	}()
}

// wakeable is an app the end-of-window queue may start: isolated, asleep, with
// a release, and not locked — a lock means nothing starts it, and unlocking
// never does either.
func wakeable(app *database.App) bool {
	return app.Status == database.AppStatusSleeping && app.CurrentRelease != "" &&
		app.RunningModeOrShared() == runtime.ModeIsolated && !app.Locked()
}

// wakeOne wakes one app, waiting for its start to finish before the queue
// moves on. The row is read again inside the wake group: a visitor may have
// woken it, or somebody stopped or locked it, since the sweep listed it.
func (r *Reaper) wakeOne(id string) {
	ctx := context.Background()
	began := time.Now()
	woke := false
	err := r.wakes.Do(ctx, id, func() error {
		app, err := r.store.GetApp(id)
		if err != nil || app == nil || !wakeable(app) {
			return err
		}
		woke = true
		return r.wake(app)
	})
	if !woke {
		return
	}
	telemetry.RecordWake(ctx, id, telemetry.Outcome(err), time.Since(began))
	if err != nil {
		// Wake has already written `crashed`, which the supervisor answers;
		// the queue goes on to the next app rather than stopping on this one.
		log.Printf("idle: waking %s after the sleep window: %v", id, err)
		return
	}
	telemetry.RecordAppTransition(ctx,
		database.AppStatusSleeping, database.AppStatusRunning, telemetry.ReasonWake)
}

// stillTheSame re-reads the app and reports whether it is the row this sweep
// judged idle: still `running`, the same release, its status unchanged since.
// An unreadable row is not slept — the next sweep looks again.
func (r *Reaper) stillTheSame(app *database.App) bool {
	fresh, err := r.store.GetApp(app.ID)
	if err != nil || fresh == nil {
		return false
	}
	if fresh.Status != database.AppStatusRunning || fresh.CurrentRelease != app.CurrentRelease {
		return false
	}
	return sameTime(fresh.StatusChangedAt, app.StatusChangedAt) &&
		sameTime(fresh.ServingSince, app.ServingSince)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

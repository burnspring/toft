// Package health notices when a deployed app has stopped working.
//
// Before it, nothing did. `apps.status = crashed` was written in exactly two
// places, both inside Builder.Reconcile, which runs once — at boot. A process
// that died at 2am left its row saying `running` indefinitely; the only symptom
// was 502s from the gateway and an "errored" badge on a health page nobody had
// a reason to open.
//
// The watcher is a sweep rather than a subscription because that is what both
// backends can answer cheaply and identically: one List() call per backend
// returns every workload and its status, in a vocabulary the shared-mode agent
// and Kubernetes already share (online | launching | stopped | errored). The resource
// collector has been making the same call every 15 seconds and throwing the
// status away.
//
// Two guards are what keep it from crying wolf, and neither is optional:
//
//  1. A workload has to look wrong on two consecutive sweeps. A single missed
//     sample during a deploy flip, an agent being restarted, or a briefly
//     unreachable cluster is not a crash.
//  2. Apps mid-transition are skipped entirely — building, sleeping, stopped,
//     failed, or with a deployment still pending. A sleeping app has no
//     workload *by design*, and reporting that as a crash would turn the idle
//     reaper into an alarm generator.
package health

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"toft/internal/applog"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
	"toft/internal/telemetry"
)

const (
	// sweepInterval matches the idle reaper's. A minute is short enough that a
	// crash is reported while somebody is still wondering why the page is
	// broken, and long enough that the two-sweep guard costs at most two
	// minutes of delay on a real crash.
	sweepInterval = time.Minute
	// badSweepsBeforeCrash is guard (1). Two, not three: every extra sweep is
	// another minute before anyone is told.
	badSweepsBeforeCrash = 2
	// orphanGrace is how long a workload must have been up before the absence
	// of its app counts as evidence the app is gone. Ten minutes against a
	// one-minute sweep, because the only thing on the other side of this
	// number is a workload that has just started under an app the sweep's
	// listing predates — and collecting one of those deletes something that is
	// serving, while waiting costs nothing but a stale pod for a few minutes.
	orphanGrace = 10 * time.Minute
	// flapRestartThreshold is how many restarts inside one sweep read as a
	// restart loop rather than a workload that fell over once and came back.
	// Both backends back off on the kubelet's scale, so a genuinely broken app
	// reaches five inside a few minutes rather than inside one sweep, which is
	// why it is counted over flapWindow below.
	flapRestartThreshold = 5
	// launchGrace is how long a workload may be *starting* before the platform
	// stops calling it that.
	//
	// Five minutes, which is the isolated backend's own readiness budget
	// (`kube.waitTimeout`): a start that is going to succeed has succeeded, and
	// what is left after it is a workload that is not coming up. Shorter would
	// call a slow cold start — image pull plus artifact fetch — a failure.
	launchGrace = 5 * time.Minute
	// flapWindow is how long those restarts may be spread over and still be one
	// loop.
	//
	// Fifteen minutes, from the kubelet's own backoff: 10s, 20s, 40s, 80s,
	// 160s, 300s is five restarts inside about ten, and the cap is five minutes
	// apart forever after. A window shorter than that is a threshold an
	// isolated app cannot reach however broken it is, which is what this
	// replaces. It is long enough that an app restarting once every ten minutes
	// — annoying, not looping — never trips it.
	flapWindow = 15 * time.Minute
	// memoryNearLimit is how close to its cap a workload has to have been on
	// the previous sweep for a restart to be attributed to memory
	// (runtime.MemoryNearLimit, which says why it is short of 1.0).
	memoryNearLimit = runtime.MemoryNearLimit
	// memoryKillsBeforeStop is how many restarts attributed to memory one
	// release is allowed before the platform stops the workload and calls the
	// app crashed. Three, and it is a tolerance rather than a threshold: one
	// kill is a bad afternoon's dataset, and an app that reaches three on the
	// same release is not going to stop on its own.
	//
	// Counted in *restarts*, which is not the same as sweeps: an app that grows
	// into its cap in eight seconds restarts four times between two sweeps, and
	// crediting it with one made the tolerance a function of how fast the app
	// leaks — the faster the leak, the longer the platform tolerated it.
	//
	// The shape is the supervisor's, and deliberately: a bounded number of
	// restarts and then an ending with words on it. What is bounded here is
	// the *backend's* restarts rather than the platform's, which is why the
	// ending is an act — a workload nobody stops is restarted forever by the
	// agent or by kubelet, and forever reads as a running app.
	memoryKillsBeforeStop = 3
	// pendingWindow matches the auto-deploy poller's: a pending deployment row
	// older than this is an orphan of a restart, not work in flight, and must
	// not suppress crash detection forever.
	pendingWindow = 30 * time.Minute
)

// Store is the database surface the watcher needs, named here rather than
// taken as *database.DB so the queries it depends on are visible in one place
// and a test can substitute one.
type Store interface {
	ListAllApps() ([]database.App, error)
	// UpdateAppStatusFrom is the only status write: every judgement here is
	// made on the sweep's snapshot, and a Stop, Restart or deploy that lands
	// between the read and the write is kept rather than overwritten.
	UpdateAppStatusFrom(id, from, status string) (bool, error)
	HasPendingDeployment(appID string, since time.Time) (bool, error)
}

// ThrottleStore is the half of a store the CPU throttling check reads. Optional:
// a store without it is an install that measures no throttling.
type ThrottleStore interface {
	AppCPUThrottling(appID string, since time.Time) (database.CPUThrottling, error)
	HasOpenAppEvent(appID, kind string) (bool, error)
}

// The app_cpu_throttled rule (ADR §128). An hour, because a cold start hits
// the cap by design and an event that fired on every deploy would teach
// owners to filter it; raised past a quarter of the hour's periods, cleared
// under a tenth. Judged only once the hour is nearly whole, and checked every
// few sweeps rather than every one: an hour's figure moves slowly.
const (
	throttleWindow     = time.Hour
	throttleCheckEvery = 5 * time.Minute
	throttleRaiseShare = 0.25
	throttleClearShare = 0.10
	// throttleCoverage is how much of the hour the measured intervals must
	// cover before it is judged.
	throttleCoverage = 0.9
	// cpuPeriodSeconds is the kernel's enforcement period, 100 ms: what
	// turns a count of periods into minutes.
	cpuPeriodSeconds = 0.1
	// throttleMinPeriods is the least work an hour must hold before it can
	// raise: five minutes of active periods. nr_periods advances only while
	// the group runs, so a mostly idle app's hour can be a few hundred
	// periods, and one short burst a minute is then "83% held at its limit".
	// Clearing has no floor: an open event still clears on a quiet hour.
	throttleMinPeriods = 3000
)

// Watcher marks apps crashed when their workload is gone, heals them when it
// comes back, and reports both.
type Watcher struct {
	// hold, when set, keeps every sweep from judging anything until it closes.
	// Boot reconciliation restores apps one after another for minutes, and an
	// app it has not reached yet is not running — which two sweeps used to
	// turn into "crashed" and an email to its owner, for an app that was back
	// seconds later.
	hold <-chan struct{}

	store  Store
	rts    *runtime.Runtimes
	events *events.Recorder

	// cpuLimitFor is an app's CPU limit in millicores, for the sentence an
	// owner reads; nil says "its CPU limit".
	cpuLimitFor func(*database.App) int

	// narrator is where this actor's decisions about an app reach that app's
	// owner. Nil is a real configuration and every call through it is then a
	// no-op — the operator's log.Printf beside each one is unaffected either
	// way.
	narrator *applog.Sink

	// onCrash is told about a crash transition, after the status has been
	// written. It is the seam between deciding and acting: this package
	// observes and reports, and the recovery loop that consumes this lives in
	// internal/supervisor where a reader can find it without reading a sweep.
	// Optional — an install with no supervisor watches exactly as it did.
	onCrash func(*database.App)

	// watchShared is false when the shared backend is the mock one. The mock
	// has no real processes and its idea of "running" is a map entry, so
	// watching it would report on a simulation. Same reason main.go restricts
	// Builder.Reconcile to a real shared backend.
	watchShared bool

	// now is swappable so a test drives time rather than sleeping through it.
	now func() time.Time

	// started is when this watcher was built. A workload the backend created
	// after it has had every one of its restarts happen while this process was
	// up — so its first sighting can be measured from zero rather than taken
	// as a baseline, which is what lost a crash that happened in the minute
	// between a deploy and the first sweep.
	started time.Time

	// state is what makes the two-sweep guard and the restart delta possible,
	// and it is deliberately in memory: like presence and idle activity, this
	// is node-local observation, not a fact about the install. A restart means
	// every app starts from a clean slate and needs two fresh sweeps before it
	// can be called crashed, which is the conservative direction.
	mu    sync.Mutex
	state map[string]*appState

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

type appState struct {
	// throttleChecked is when app_cpu_throttled was last judged for this app.
	throttleChecked time.Time
	// bad counts consecutive sweeps in which the workload looked wrong.
	bad int
	// cleared is whether an open crash has already been closed for this app
	// since it was last seen healthy. Without it, the boot-time recovery in
	// Builder.Reconcile — which heals a crashed row to running before this
	// watcher ever sees it — would leave the event open forever and no
	// recovery notice would ever be sent. With it, the cost is one UPDATE per
	// app per platform restart rather than one per app per sweep.
	cleared bool
	// launchingFrom is when this app's workload was first seen starting, and is
	// cleared the moment it is seen as anything else. A start that never ends
	// is the one state a sweep used to ignore indefinitely.
	launchingFrom time.Time
	// restartsInWindow is how many restarts this app has been observed to make
	// inside flapWindow, and windowFrom is when that window opened.
	//
	// **A window rather than a sweep, because a sweep only works on one
	// backend.** PM2, the shared backend when this was written, restarted a
	// crashed process immediately, so a broken app racked up five inside a
	// minute; the kubelet backs off 10s, 20s, 40s and on to five minutes, so the
	// same broken app contributes one restart per sweep and a per-sweep
	// threshold is never reached. The agent now backs off on the kubelet's
	// numbers too, so accumulating is the only shape that fits either, and it
	// subsumes the old test: five in one sweep is five in the window too.
	restartsInWindow int
	windowFrom       time.Time
	// memory is the last reading taken for this app and haveMemory says whether
	// one was ever taken, because a backend that does not measure memory
	// reports nil and 0 would read as an idle workload. Kept so that a restart
	// seen on *this* sweep can be attributed to what the workload was doing on
	// the last one — after the kill the reading is a fresh process near zero,
	// which is exactly when the number that explains it is gone.
	memory     float64
	haveMemory bool
	// memoryTold is the restart count at which this app was last told it had
	// crossed its cap, so a workload that sits against the limit for an hour
	// is recorded once per restart rather than once per sweep.
	memoryTold  int
	haveMemTold bool
	// memKills counts the restarts attributed to memory on memRelease, and
	// stopping is what happens when it reaches memoryKillsBeforeStop. Counted
	// against the release for the reason the supervisor counts its attempts
	// there: a deploy is a clean slate without anything having to clear state,
	// and the next release is a different app as far as its appetite goes.
	memKills   int
	memRelease string
	// memStopped is the ending, and it is what the crashed interstitial reads
	// so it stops promising a refresh nothing is going to perform. Cleared by
	// a deploy — a different memRelease — or by Reset, which is a person
	// starting the app by hand.
	memStopped bool
	// restarts is the count seen on the previous sweep, and haveRestarts says
	// whether there was one — a first sighting has no delta, and treating a
	// long-running app's lifetime restart count as this minute's delta would
	// report flapping the moment the platform restarted.
	restarts     int
	haveRestarts bool
	// counted is the restart count already accounted for on the workload the
	// backend created at countedOf. Unlike restarts above it survives the
	// resets a bad sweep, a stop for memory and forget make, because it is
	// tagged with the workload it describes: the same entry seen again
	// continues its count, and a different one is recognised as different.
	// Zero countedOf means the backend does not say when it created a
	// workload, and the untagged baseline above is all there is.
	counted   int
	countedOf time.Time
}

// New builds a watcher. onCrash may be nil, and is invoked once per crash
// transition, after the status write.
func New(store Store, rts *runtime.Runtimes, rec *events.Recorder, watchShared bool, onCrash func(*database.App)) *Watcher {
	return &Watcher{
		store: store, rts: rts, events: rec, watchShared: watchShared, onCrash: onCrash,
		now:     func() time.Time { return time.Now().UTC() },
		started: time.Now().UTC(),
		state:   map[string]*appState{},
		stop:    make(chan struct{}),
	}
}

// WithHold makes the sweep wait for ch to close before it judges any app.
func (w *Watcher) WithHold(ch <-chan struct{}) *Watcher {
	w.hold = ch
	return w
}

// WithCPULimit says how to resolve an app's CPU limit, for app_cpu_throttled's
// sentence.
func (w *Watcher) WithCPULimit(f func(*database.App) int) *Watcher {
	w.cpuLimitFor = f
	return w
}

// WithNarrator wires the owner-facing half of this actor's log.
func (w *Watcher) WithNarrator(n *applog.Sink) *Watcher {
	w.narrator = n
	return w
}

// Start runs the sweep loop for the life of the process.
func (w *Watcher) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.tick()
			case <-w.stop:
				return
			}
		}
	}()
}

func (w *Watcher) Close() {
	w.once.Do(func() { close(w.stop) })
	w.wg.Wait()
}

// tick is one sweep.
func (w *Watcher) tick() {
	ctx, span := telemetry.Start(context.Background(), "health.sweep")
	start := time.Now()
	// A sweep that is abandoned records `failed` rather than nothing. The whole
	// point of the abandonment below is that the platform declines to act on an
	// answer it does not trust — which is a decision worth being able to see the
	// rate of, and is invisible in a log nobody tails.
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepHealth, outcome, time.Since(start))
	}()

	if w.hold != nil {
		select {
		case <-w.hold:
		default:
			outcome = telemetry.OutcomeSkipped
			return
		}
	}

	apps, err := w.store.ListAllApps()
	if err != nil {
		log.Printf("health: listing apps: %v", err)
		return
	}

	// One call for every workload on the install. A backend that fails is the
	// one case where the whole sweep is abandoned rather than partially
	// applied: a listing that came back empty because the agent was
	// restarting is indistinguishable, app by app, from every app having
	// crashed at once — and acting on it would mark the entire install broken.
	workloads, err := w.rts.List()
	if err != nil {
		log.Printf("health: listing workloads, skipping sweep: %v", err)
		return
	}

	live := make(map[string]struct{}, len(apps))
	for i := range apps {
		app := &apps[i]
		live[app.ID] = struct{}{}
		w.check(app, workloads)
	}

	// Forget apps that no longer exist, so a deleted app's state cannot
	// resurface against a new app that happens to reuse an id.
	w.mu.Lock()
	for id := range w.state {
		if _, ok := live[id]; !ok {
			delete(w.state, id)
		}
	}
	w.mu.Unlock()

	// And collect the workloads those deleted apps left behind. This is the
	// same sentence one level down: the loop above visits *apps*, so a
	// workload whose app is gone is never visited, never restarted and — until
	// now — never removed. It went on holding its memory reservation until
	// somebody noticed it by hand.
	//
	// It belongs here rather than in the delete handler because the delete
	// handler already tries, and best-effort is the right behaviour there: an
	// administrator getting rid of a row must not be blocked by a backend that
	// is down, and a workload recreated between that stop and the row's
	// deletion is nobody's to catch at that moment. A sweep that runs every
	// minute is what catches it afterwards.
	w.collect(w.currentReleases(apps))
	outcome = telemetry.OutcomeSucceeded
}

// inFlightStore is the store half that can tell a deploy is in flight, queued
// or building. Optional: without it no live app's workloads are judged.
type inFlightStore interface {
	InFlightDeployments(appID string, since time.Time) (uncloned bool, building []string, err error)
}

// currentReleases maps every live app to the release that serves it, or to ""
// when that is not certain — no release yet, a deploy queued or building that
// may be about to flip to a pod already started, or a store that cannot say.
// "" keeps every workload of that app; a wrong name would delete what serves.
func (w *Watcher) currentReleases(apps []database.App) map[string]string {
	live := make(map[string]string, len(apps))
	fs, canTell := w.store.(inFlightStore)
	for i := range apps {
		app := &apps[i]
		live[app.ID] = ""
		if app.CurrentRelease == "" || !canTell {
			continue
		}
		uncloned, building, err := fs.InFlightDeployments(app.ID, time.Time{})
		if err != nil || uncloned || len(building) > 0 {
			continue
		}
		live[app.ID] = app.CurrentRelease
	}
	return live
}

// collect removes workloads whose app has been deleted.
//
// `orphanGrace` is what keeps this honest against its own snapshot: `live` was
// read at the top of this sweep, and an app created since then has a workload
// no app in that map claims. A workload has to have been up for longer than
// any sweep takes before its app's absence is allowed to mean anything, and an
// orphan is never urgent enough to shorten it.
func (w *Watcher) collect(live map[string]string) {
	removed, err := w.rts.CollectOrphans(live, orphanGrace)
	if err != nil {
		log.Printf("health: collecting orphaned workloads: %v", err)
	}
	for _, o := range removed {
		if o.Superseded {
			log.Printf("health: removed workload %s, a release app %s no longer serves (up %s)",
				o.Workload, o.AppID, o.Age.Round(time.Second))
			continue
		}
		// Said out loud, and not to the app's own log: there is no app left to
		// say it to. This is the platform talking about its own housekeeping,
		// which is what the platform log is for.
		log.Printf("health: removed workload %s, orphaned by the deletion of app %s (up %s)",
			o.Workload, o.AppID, o.Age.Round(time.Second))
	}
}

func (w *Watcher) check(app *database.App, workloads map[string]*runtime.AppMetrics) {
	// Only two statuses are the watcher's business. `building` is mid-flight,
	// `sleeping` has no workload by design, and `stopped` and `failed` are
	// states a person or a build already explained.
	if app.Status != database.AppStatusRunning && app.Status != database.AppStatusCrashed {
		w.forget(app.ID)
		return
	}
	if app.CurrentRelease == "" {
		w.forget(app.ID)
		return
	}
	if app.RunningModeOrShared() == runtime.ModeShared && !w.watchShared {
		return
	}
	// A deploy in flight replaces the workload; the window where the old one
	// is gone and the new one is not yet named in the row is exactly when a
	// naive sweep reports a crash.
	if pending, err := w.store.HasPendingDeployment(app.ID, w.now().Add(-pendingWindow)); err != nil || pending {
		if err != nil {
			log.Printf("health: %s: checking pending deployments: %v", app.Slug, err)
		}
		return
	}

	m, found := workloads[app.CurrentRelease]
	// A workload the backend is still starting is neither healthy nor evidence
	// of a crash: it neither clears the counter nor advances it.
	//
	// **But not forever.** A pod nothing can place stays `Pending` — and so
	// `launching` — for as long as nobody looks, and this early return was the
	// whole of why: the row went on saying `running`, no event was recorded,
	// and an owner running the two commands the CLI offers for exactly this was
	// told twice that nothing was wrong while visitors got the unavailable
	// page. The cluster had a precise reason the entire time.
	//
	// So a start is given a grace and then read as what it is. Generous,
	// because a cold start legitimately takes tens of seconds — an image pull
	// and an artifact fetch — and `launchGrace` is comfortably past the
	// backend's own readiness budget, so a deploy that is going to work has
	// finished long before this.
	if found && m.Status == runtime.StatusLaunching && !w.launchingTooLong(app.ID) {
		return
	}
	healthy := found && m.Status == runtime.StatusOnline

	if app.Status == database.AppStatusCrashed {
		switch {
		case found && m.CrashLooping:
			// Called crashed, and still starting and exiting: the loop goes on
			// being counted, or it is never called one.
			//
			// **Ahead of healthy, and on purpose.** A loop is online for the
			// seconds each start lives, and the agent goes on calling it a loop
			// until a start outlives its backoff's reset; healing on the
			// online half marked a 25-second loop running and closed its
			// app_crashed once per backoff cycle (the soak, 2026-10-28).
			w.countRestarts(app, m)
		case healthy:
			w.heal(app, m)
		}
		return
	}

	if !healthy {
		w.observeBad(app, found, m)
		return
	}
	w.observeHealthy(app, m)
	w.observeThrottling(app)
}

// observeThrottling raises app_cpu_throttled when the app spent more than a
// quarter of its last hour's work held at its CPU limit, and clears it after
// an hour under a tenth. A release serving for less than an hour has not
// earned a verdict, and any event still open is the previous release's: it
// clears, and the new release starts its own hour.
func (w *Watcher) observeThrottling(app *database.App) {
	ts, ok := w.store.(ThrottleStore)
	if !ok || w.events == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	st := w.stateFor(app.ID)
	if !st.throttleChecked.IsZero() && now.Sub(st.throttleChecked) < throttleCheckEvery {
		w.mu.Unlock()
		return
	}
	st.throttleChecked = now
	w.mu.Unlock()

	open, err := ts.HasOpenAppEvent(app.ID, string(events.KindAppCPUThrottled))
	if err != nil {
		return
	}
	since := now.Add(-throttleWindow)
	if app.ServingSince != nil && app.ServingSince.After(since) {
		if open {
			w.events.Clear(app.ID, events.KindAppCPUThrottled)
		}
		return
	}
	t, err := ts.AppCPUThrottling(app.ID, since)
	if err != nil || t.Periods <= 0 || t.MeasuredSeconds < throttleWindow.Seconds()*throttleCoverage {
		return
	}
	share := float64(t.ThrottledPeriods) / float64(t.Periods)
	switch {
	case !open && share > throttleRaiseShare && t.Periods >= throttleMinPeriods:
		w.sayThrottled(app, t, share)
	case open && share < throttleClearShare:
		w.events.Clear(app.ID, events.KindAppCPUThrottled)
	}
}

// workWords is a span of CPU work in the unit an owner can picture: minutes,
// or seconds when it rounds to no minutes.
func workWords(seconds float64) string {
	if m := int(math.Round(seconds / 60)); m > 0 {
		if m == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	s := int(math.Round(seconds))
	if s == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", s)
}

// sayThrottled records app_cpu_throttled, in minutes an owner can picture.
func (w *Watcher) sayThrottled(app *database.App, t database.CPUThrottling, share float64) {
	limit := "its CPU limit"
	if w.cpuLimitFor != nil {
		if m := w.cpuLimitFor(app); m > 0 {
			limit = "its " + coresWord(m) + " limit"
		}
	}
	held := workWords(float64(t.ThrottledPeriods) * cpuPeriodSeconds)
	work := workWords(float64(t.Periods) * cpuPeriodSeconds)
	log.Printf("health: %s held at its CPU limit for %d%% of its last hour's periods", app.Slug, int(share*100))
	w.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppCPUThrottled,
		Severity: database.EventSeverityWarning,
		Source:   database.EventSourceHealth,
		Summary:  "The app is being held at its CPU limit",
		Detail: fmt.Sprintf("In the last hour it was held at %s for %s of its %s of work (%d%%). "+
			"Requests will be slow while this lasts. An administrator can raise its CPU limit in the "+
			"app's Runtime settings.", limit, held, work, int(math.Round(share*100))),
		Fingerprint: app.CurrentRelease,
	})
}

// coresWord is a CPU limit as the sentence says it: "1-core", "0.5-core".
func coresWord(millicores int) string {
	return strconv.FormatFloat(float64(millicores)/1000, 'f', -1, 64) + "-core"
}

// launchingTooLong reports whether this app's workload has been starting past
// the grace, and starts the clock on the first sighting.
//
// The clock is per app and in memory, like every other judgement this watcher
// makes: a platform restart re-baselines it, which costs one grace period
// before a stuck workload is called stuck again.
func (w *Watcher) launchingTooLong(appID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.stateFor(appID)
	if st.launchingFrom.IsZero() {
		st.launchingFrom = w.now()
		return false
	}
	return w.now().Sub(st.launchingFrom) > launchGrace
}

// observeBad advances the two-sweep guard and, on the second, marks the app
// crashed and says so.
func (w *Watcher) observeBad(app *database.App, found bool, m *runtime.AppMetrics) {
	// **A crash loop is restarts, and they are counted here too.** A workload
	// the kubelet is holding back between starts is caught in backoff by most
	// sweeps, and only a healthy sweep used to book restarts into the window —
	// so an isolated app exiting every 25 seconds was called crashed, restarted
	// by the supervisor, and never called flapping (RUN.29 on EKS). The workload
	// still exists and its count still means something.
	looping := found && m.CrashLooping
	if looping {
		w.countRestarts(app, m)
	}

	w.mu.Lock()
	st := w.stateFor(app.ID)
	st.bad++
	bad := st.bad
	// The restart baseline is meaningless once the workload is gone, and
	// whatever was cleared before is no longer the current state of things.
	if !looping {
		st.haveRestarts = false
	}
	st.haveMemory = false
	st.haveMemTold = false
	st.cleared = false
	w.mu.Unlock()

	if bad < badSweepsBeforeCrash {
		return
	}

	// From the status this sweep read, or not at all: a person who pressed
	// Stop while the sweep was walking the estate decided the app is stopped,
	// and a crash written over that would hand it to the supervisor to restart.
	moved, err := w.store.UpdateAppStatusFrom(app.ID, app.Status, database.AppStatusCrashed)
	if err != nil {
		log.Printf("health: marking %s crashed: %v", app.Slug, err)
		return
	}
	if !moved {
		w.forget(app.ID)
		return
	}
	telemetry.RecordAppTransition(context.Background(),
		app.Status, database.AppStatusCrashed, telemetry.ReasonCrash)
	log.Printf("health: %s is not running — status set to crashed", app.Slug)

	detail := "The platform can no longer find this app's workload."
	switch {
	case found && m.Status == runtime.StatusLaunching:
		// A workload that has been starting past the grace. The backend's own
		// sentence is the whole value of this branch — *persistentvolumeclaim
		// "lpv-x" is being deleted* is actionable, and "the workload is present
		// but reports status launching" is not.
		detail = fmt.Sprintf("The workload has been starting for over %s and has not come up.",
			launchGrace)
		if m.Detail != "" {
			detail += " " + m.Detail
		}
	case found && m.CrashLooping:
		// The backend's own verdict, and the specific answer where there is
		// one: the workload is not simply down, it is starting, dying and being
		// held back before the next attempt — each wait longer than the last,
		// which is why an app in this state looks *stuck* rather than gone.
		// Both backends back off and both say this — the agent from its
		// second restart, the kubelet as CrashLoopBackOff.
		detail = fmt.Sprintf("The workload keeps starting and exiting — it has restarted "+
			"%d time(s) and the platform is waiting longer before each attempt. The app's "+
			"logs will say why it is exiting.", m.Restarts)
	case found:
		detail = "The workload is present but reports status " + m.Status + "."
	}
	// Fingerprinted on the release: a new deploy that crashes is a new problem
	// and is worth a second email, while the same broken release rediscovered
	// every minute is not.
	w.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppCrashed,
		Severity: database.EventSeverityError,
		Source:   database.EventSourceHealth,
		Summary:  "The app stopped running",
		// The pointer used to hedge, because the pane it pointed at was usually
		// empty by the time anybody followed it: the workload was gone and the
		// app's output went with it. The platform's own account of the app is
		// kept now, so this can say what is there rather than what might be.
		Detail:      detail + " The app's log holds the platform's account of it and outlives the workload.",
		Fingerprint: app.CurrentRelease,
	})

	// The narration beside the verdict. The event above is the fact — one row,
	// fingerprinted on the release, notifiable — and this is the moment it
	// happened, in the log the event's own detail points at. Neither is the
	// other: an owner reading the pane sees when the platform noticed, and an
	// owner reading their mail sees that it did.
	w.narrator.For(app.ID, app.ProcessName(), "").Say(applog.StreamStderr,
		"The platform can no longer see this app running, and has marked it crashed.")

	// Last, and once: the branch above runs only while the row still says
	// running, and the next sweep takes the crashed path instead. A callback
	// that fired every sweep would restart an app that is already being
	// restarted.
	//
	// The row is brought into step with what was just written first, so what
	// the callback receives is the app as the database now has it rather than
	// as this sweep found it. The slice is this sweep's and is discarded at the
	// end of it.
	if w.onCrash != nil {
		app.Status = database.AppStatusCrashed
		w.onCrash(app)
	}
}

// observeHealthy resets the guard and watches for a restart loop.
func (w *Watcher) observeHealthy(app *database.App, m *runtime.AppMetrics) {
	w.mu.Lock()
	st := w.stateFor(app.ID)
	st.bad = 0
	// It started, so the next start is timed from its own beginning.
	st.launchingFrom = time.Time{}
	prev, had := w.restartBaseline(st, m)
	st.restarts, st.haveRestarts = m.Restarts, true
	if !m.CreatedAt.IsZero() {
		st.counted, st.countedOf = m.Restarts, m.CreatedAt
	}
	// The previous sweep's reading, taken before this sweep overwrites it: a
	// workload that was just killed for memory is measured again as a fresh
	// process using almost nothing, so the number that explains the restart is
	// the one from before it.
	lastMem, hadMem := st.memory, st.haveMemory
	if m.Memory != nil {
		st.memory, st.haveMemory = *m.Memory, true
	}
	toldAt, hadTold := st.memoryTold, st.haveMemTold
	clear := !st.cleared
	st.cleared = true
	w.mu.Unlock()

	// Close anything still open the first time this app is seen healthy.
	//
	// The transition this catches is not the obvious one: Builder.Reconcile
	// restarts a dead workload at boot and heals the row to running itself, so
	// by the time this watcher looks there is no crashed status left to
	// observe — but the event is still open and nobody has been told it ended.
	// ResolveAppEvents matches nothing in the ordinary case, and a Clear that
	// closes nothing sends nothing.
	if clear {
		w.events.Clear(app.ID, events.KindAppCrashed)
		w.events.Clear(app.ID, events.KindAppFlapping)
		w.events.Clear(app.ID, events.KindAppMemoryLimit)
		w.events.Clear(app.ID, events.KindAppOutOfMemory)
		w.events.Clear(app.ID, events.KindAppNotRestarting)
	}

	if !had {
		return
	}
	delta := m.Restarts - prev

	// Memory first, because it is the specific answer and flapping is the
	// general one. An app killed for its cap restarts about once a sweep and
	// never reaches the flap threshold, so this is not a nicer message for a
	// case already covered — it is the only message that case gets.
	//
	// The test is the *previous* sweep's reading against the cap the backend
	// actually applied. Both have to be known: a backend that does not measure
	// memory reports nil, and one that applied no cap reports 0, and neither is
	// evidence of anything. memoryNearLimit is deliberately short of 100% —
	// the sample that crosses the cap is usually never taken, because the kill
	// happens between sweeps.
	// Two ways to know, and neither is a guess. The isolated backend reports
	// the kernel's own verdict — a container killed for its limit is terminated
	// with reason `OOMKilled`, kept on the container's last state across the
	// restart — and so does the shared one: the agent sets OOMKilled when its
	// own watchdog killed for memory or the kernel's cgroup did. The *previous*
	// sweep's reading against the cap the backend applied is the second test,
	// for a kill no backend attributed. Both have to be known for it: a
	// backend that does not measure memory reports nil and one that applied no
	// cap reports 0, and neither is evidence of anything. memoryNearLimit is
	// deliberately short of 100% — the sample that crosses the cap is usually
	// never taken, because the kill happens between sweeps.
	byLimit := hadMem && m.MemoryLimitMB > 0 && lastMem >= float64(m.MemoryLimitMB)*memoryNearLimit
	if delta >= 1 && (m.OOMKilled || byLimit) {
		// Once per restart, not once per sweep: an app that sits against its
		// cap is measured every minute and restarted every few.
		if !hadTold || m.Restarts != toldAt {
			w.mu.Lock()
			st := w.stateFor(app.ID)
			st.memoryTold, st.haveMemTold = m.Restarts, true
			// A different release is a different appetite, and the count starts
			// over — the same clean slate a deploy gives the supervisor.
			if st.memRelease != app.CurrentRelease {
				st.memRelease, st.memKills, st.memStopped = app.CurrentRelease, 0, false
			}
			// The whole delta, not one. Counting sweeps rather than restarts
			// made the tolerance a function of how fast the app leaks: an app
			// that grows into its cap in eight seconds restarts four times
			// between two sweeps and was credited with one, so it took three
			// minutes and eight restarts to reach a cap of three. Measured on
			// the dev install, which is where the number 8 comes from.
			st.memKills += delta
			kills := st.memKills
			w.mu.Unlock()

			// At or over the tolerance the ending is what gets said, rather
			// than a warning counting past the number it stops at. A burst of
			// three inside one sweep is not less certain than three spread over
			// three — it is the same evidence arriving faster.
			if kills >= memoryKillsBeforeStop {
				w.stopForMemory(app, m, lastMem, hadMem, kills)
				return
			}
			w.sayMemoryKill(app, m, lastMem, hadMem, kills)
		}
	}

	// The window and what is said from it are bookRestarts and sayRestarts.
	//
	// Memory keeps its precedence. The sweep above has already said the
	// specific thing about a workload restarting for its cap; "the app keeps
	// restarting" arriving beside "it ran out of memory" is the general answer
	// second, reading as a second problem.
	byMemory := delta >= 1 && (m.OOMKilled || byLimit)
	if byMemory {
		return
	}

	w.mu.Lock()
	inWindow, since := w.bookRestarts(w.stateFor(app.ID), delta)
	w.mu.Unlock()
	w.sayRestarts(app, m, delta, inWindow, since)
}

// countRestarts books a crash-looping workload's restarts the way a healthy
// sweep does — the same baseline, the same window — and says them.
func (w *Watcher) countRestarts(app *database.App, m *runtime.AppMetrics) {
	w.mu.Lock()
	st := w.stateFor(app.ID)
	prev, had := w.restartBaseline(st, m)
	st.restarts, st.haveRestarts = m.Restarts, true
	if !m.CreatedAt.IsZero() {
		st.counted, st.countedOf = m.Restarts, m.CreatedAt
	}
	if !had {
		w.mu.Unlock()
		return
	}
	delta := m.Restarts - prev
	inWindow, since := w.bookRestarts(st, delta)
	w.mu.Unlock()
	w.sayRestarts(app, m, delta, inWindow, since)
}

// bookRestarts adds delta to the flap window and answers what it now holds.
// Called with w.mu held.
//
// **Restarts accumulate over a window; they used to have to arrive inside
// one sweep.** A count that went *down* is a fresh workload — a deploy, or a
// backend that lost its history — not a negative number of restarts, and it
// opens a new window rather than contributing to the old one.
//
// The per-sweep test only worked on one backend. PM2, the shared backend when
// this was written, restarted a crashed process immediately, so five inside a
// minute was easy; the kubelet backs off 10s, 20s, 40s and on to five minutes,
// so a genuinely broken isolated app contributes about one restart per sweep
// and *never* reached the threshold. Accumulating over flapWindow is the shape
// that fits both, and the agent's backoff — the kubelet's numbers, after one
// immediate restart — is why shared mode needs it now too.
func (w *Watcher) bookRestarts(st *appState, delta int) (int, time.Duration) {
	now := w.now()
	switch {
	case delta < 0:
		// A different workload's history. Whatever it did before this pod
		// existed is not evidence about this one.
		st.restartsInWindow, st.windowFrom = 0, now
	case st.windowFrom.IsZero() || now.Sub(st.windowFrom) > flapWindow:
		st.restartsInWindow, st.windowFrom = delta, now
	default:
		st.restartsInWindow += delta
	}
	return st.restartsInWindow, now.Sub(st.windowFrom)
}

// sayRestarts says this sweep's restarts: each on its own until there are
// enough of them to be a loop, and from then on the loop is what is said. A
// crash loop is one row a sweep either way, and a row per restart beside the
// flapping event's own would be the same fact twice.
func (w *Watcher) sayRestarts(app *database.App, m *runtime.AppMetrics, delta, inWindow int, since time.Duration) {
	if inWindow < flapRestartThreshold {
		if delta >= 1 {
			w.sayRestarted(app, m, delta)
		}
		return
	}
	log.Printf("health: %s restarted %d time(s) in %s", app.Slug, inWindow, since.Round(time.Second))
	// The restarts themselves are the narration; the verdict "it keeps
	// restarting" is the event's, below, and is fingerprinted so it is said
	// once. Without this line an owner is told an app is flapping and has no
	// record of when it flapped.
	//
	// The period is stated rather than assumed to be a minute: on a backend
	// that backs off, the same five restarts are spread over ten, and a line
	// claiming they happened in the last minute would be wrong in the one mode
	// this was extended to reach.
	w.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStderr,
		"The workload restarted %d time(s) in the last %s.", inWindow, humanMinutes(since))
	w.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppFlapping,
		Severity: database.EventSeverityWarning,
		Source:   database.EventSourceHealth,
		Summary:  "The app keeps restarting",
		Detail: "The workload restarted repeatedly in a short period. It is answering " +
			"some requests and failing others; the app's logs will say why it is exiting.",
		Fingerprint: app.CurrentRelease,
	})
}

// sayRestarted is every exit the backend restarted, said where an owner looks.
//
// The backend restarts a crashed process in place (the agent's first restart
// is immediate) and the row never leaves `running`, so an app that exited with
// code 7 four times — under the flapping threshold — left no line in the
// platform log and no event: `toft events` answered "nothing has gone wrong"
// about an app that had died four times. The line goes to the platform log;
// the event is recorded silently and closed at once, because it is a fact about
// the past rather than a condition, and the news — if the restarts become one
// — is the flapping event below.
func (w *Watcher) sayRestarted(app *database.App, m *runtime.AppMetrics, delta int) {
	// A signal is said as one: "exited with code 0" about a segfault sends an
	// owner looking for a clean shutdown that never happened.
	how, last := "and was restarted", ""
	switch {
	case m.ExitSignal != "":
		how, last = "was killed by "+m.ExitSignal+" and restarted", "; the last was killed by "+m.ExitSignal
	case m.ExitCode != nil:
		how, last = fmt.Sprintf("exited with code %d and was restarted", *m.ExitCode), fmt.Sprintf("; the last exit was code %d", *m.ExitCode)
	default:
		how = "exited and was restarted"
	}
	what := "The process " + how
	if delta > 1 {
		what = fmt.Sprintf("The process exited %d times since the last check and was restarted each time", delta) + last
	}
	line := fmt.Sprintf("%s (restart %d since it was started).", what, m.Restarts)
	log.Printf("health: %s: %s", app.Slug, line)
	w.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStderr, "%s", line)
	w.events.Record(events.Event{
		AppID:       app.ID,
		Kind:        events.KindAppRestarted,
		Severity:    database.EventSeverityWarning,
		Source:      database.EventSourceHealth,
		Summary:     line,
		Detail:      "The app's own logs, just before this, say why it exited: toft logs --since 10m.",
		Fingerprint: app.CurrentRelease,
		Silent:      true,
	})
	w.events.Clear(app.ID, events.KindAppRestarted)
}

// humanMinutes renders how long a run of restarts took, for a line an app's
// owner reads.
//
// Whole minutes, and never "0 minutes": under a second-scale backend five
// restarts really can arrive inside one sweep, and "in the last minute" is both
// true and what that log line has always said.
func humanMinutes(d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins <= 1 {
		return "minute"
	}
	return fmt.Sprintf("%d minutes", mins)
}

// sayMemoryKill records one restart the platform attributes to memory, in the
// app's own log and as an event.
//
// Said by the platform because the app cannot say it: a workload over its cap
// is sent a signal, so R prints "Execution halted" with no error above it and a
// Python app prints nothing whatsoever. The one place the reason exists is here.
func (w *Watcher) sayMemoryKill(app *database.App, m *runtime.AppMetrics, lastMem float64, hadMem bool, kills int) {
	log.Printf("health: %s restarted for memory (%s), %d of %d on release %s",
		app.Slug, memoryPhrase(m, lastMem, hadMem), kills, memoryKillsBeforeStop, app.CurrentRelease)
	w.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStderr,
		"The workload ran out of memory (%s) and was restarted — %d of %d before the "+
			"platform stops it. Nothing in the app's own output will say so: it was sent "+
			"a signal, not an error.",
		memoryPhrase(m, lastMem, hadMem), kills, memoryKillsBeforeStop)
	w.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppMemoryLimit,
		Severity: database.EventSeverityWarning,
		Source:   database.EventSourceHealth,
		Summary:  "The app is being restarted for using too much memory",
		Detail: fmt.Sprintf("The workload ran out of memory (%s) and was restarted. This is %d of "+
			"%d: after the third the platform stops the app rather than let it be killed and "+
			"restarted indefinitely. It will keep happening until the app uses less or an "+
			"administrator raises its memory limit in the app's Runtime settings. The app's own logs "+
			"will not explain it \u2014 "+
			"a workload over its cap is sent a signal rather than an error.",
			memoryPhrase(m, lastMem, hadMem), kills, memoryKillsBeforeStop),
		Fingerprint: app.CurrentRelease,
	})
}

// stopForMemory is the ending: the platform stops the workload itself and calls
// the app crashed.
//
// The act, not just the verdict. Neither backend will stop on its own — the
// agent never gives up on a service and kubelet restarts the container in
// place — so a workload that grows into its cap every few minutes
// is restarted forever, the row keeps saying `running`, and the only symptom is
// a restart count nobody is watching. An app that is down and says so is worth
// more than an app that is down two minutes in every five and says nothing.
//
// It does not call onCrash. The supervisor's job is to bring a crashed workload
// back, and bringing this one back means starting it into the same cap for
// another five rounds — which is the loop this exists to end.
func (w *Watcher) stopForMemory(app *database.App, m *runtime.AppMetrics, lastMem float64, hadMem bool, kills int) {
	rt, err := w.rts.For(app.RunningModeOrShared())
	if err != nil {
		log.Printf("health: %s: no runtime to stop: %v", app.Slug, err)
		return
	}
	// Stop, never Delete: Delete is what returns a shared workload's port to
	// the pool, and a person restarting this app has to land on the address the
	// row already holds. An absence is the state this was going to produce, so
	// it is not a reason to leave the row saying `running`.
	if err := rt.Stop(app.ProcessName()); err != nil && runtime.AbsenceReason(err) == "" {
		log.Printf("health: %s: stopping after repeated memory kills: %v", app.Slug, err)
		return
	}

	w.mu.Lock()
	st := w.stateFor(app.ID)
	st.memStopped = true
	// The workload is gone, so the restart and memory baselines are about a
	// process that no longer exists.
	st.haveRestarts, st.haveMemory, st.haveMemTold = false, false, false
	st.cleared = false
	w.mu.Unlock()

	moved, err := w.store.UpdateAppStatusFrom(app.ID, app.Status, database.AppStatusCrashed)
	if err != nil {
		log.Printf("health: marking %s crashed after repeated memory kills: %v", app.Slug, err)
		return
	}
	if !moved {
		// Somebody stopped or redeployed it in the meantime; their status stands.
		return
	}
	telemetry.RecordAppTransition(context.Background(),
		app.Status, database.AppStatusCrashed, telemetry.ReasonOutOfMemory)
	log.Printf("health: %s stopped after %d memory kills on release %s — status set to crashed",
		app.Slug, kills, app.CurrentRelease)

	w.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStderr,
		"The workload ran out of memory %d times on this release, so the platform has "+
			"stopped it rather than keep restarting it. It will not start again until you "+
			"deploy or restart it, and it will do the same thing unless the app uses less "+
			"memory or an administrator raises the limit.", kills)
	w.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppOutOfMemory,
		Severity: database.EventSeverityError,
		Source:   database.EventSourceHealth,
		Summary:  "The app was stopped for running out of memory",
		Detail: fmt.Sprintf("Release %s ran out of memory %d times (%s), so the platform stopped "+
			"the workload instead of letting it be killed and restarted indefinitely. The app is "+
			"crashed and will not start again until you deploy or restart it. Restarting it "+
			"unchanged will end the same way: the app needs to use less memory, or an "+
			"administrator needs to raise the memory limit in Admin \u2192 Settings.",
			app.CurrentRelease, kills, memoryPhrase(m, lastMem, hadMem)),
		Fingerprint: app.CurrentRelease,
	})
}

// memoryPhrase is runtime.MemoryPhrase, which the builder's readiness wait
// says too: a deploy killed for memory before it answered is told in the same
// words as a running app killed for it.
func memoryPhrase(m *runtime.AppMetrics, lastMem float64, hadMem bool) string {
	return runtime.MemoryPhrase(m, lastMem, hadMem)
}

// OutOfMemory reports whether this app is the one the platform stopped for
// running out of memory, on the release it currently holds. It is what the
// crashed interstitial reads to drop its promise of an automatic refresh —
// nothing is restarting this app, in either mode, and a page that promises to
// refresh into a working app keeps a visitor waiting for one.
//
// In memory, and a platform restart clears it: boot Reconcile is entitled to
// one more attempt, exactly as it is after the supervisor's ending.
func (w *Watcher) OutOfMemory(app *database.App) bool {
	if w == nil || app == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.state[app.ID]
	return ok && st.memStopped && st.memRelease == app.CurrentRelease
}

// Reset forgets everything counted against an app, and is called when a person
// starts or restarts it by hand — the same call the supervisor gets, for the
// same reason. The ending says "until you deploy or restart it", and this is
// the second half of that sentence; the first is the release changing under a
// deploy.
func (w *Watcher) Reset(appID string) {
	if w == nil {
		return
	}
	w.forget(appID)
}

// heal writes the recovery the platform previously only performed at boot.
func (w *Watcher) heal(app *database.App, m *runtime.AppMetrics) {
	// Only from crashed: a row a person stopped, or a deploy set building,
	// since this sweep read it is not the watcher's to call running.
	moved, err := w.store.UpdateAppStatusFrom(app.ID, database.AppStatusCrashed, database.AppStatusRunning)
	if err != nil {
		log.Printf("health: healing %s: %v", app.Slug, err)
		return
	}
	if !moved {
		return
	}
	log.Printf("health: %s is online again — status healed to running", app.Slug)
	// The recovery, said as plainly as the failure was. An app log that
	// records only what went wrong reads as an app that never came back.
	w.narrator.For(app.ID, app.ProcessName(), "").Say(applog.StreamStdout,
		"The app is answering again; the platform has marked it running.")

	w.mu.Lock()
	st := w.stateFor(app.ID)
	st.bad = 0
	// **The restarts since the last sweep are booked before the baseline
	// moves.** A loop under a backoff is called crashed while it is held back
	// and healed while it is up, and every restart in between landed on one of
	// those transitions: re-baselined here without being counted, the window
	// never reached flapThreshold and the loop was never called one (RUN.29
	// under the agent).
	prev, had := w.restartBaseline(st, m)
	st.restarts, st.haveRestarts = m.Restarts, true
	if !m.CreatedAt.IsZero() {
		st.counted, st.countedOf = m.Restarts, m.CreatedAt
	}
	delta, inWindow, since := 0, 0, time.Duration(0)
	if had && m.Restarts > prev {
		delta = m.Restarts - prev
		inWindow, since = w.bookRestarts(st, delta)
	}
	st.cleared = true
	// Whatever brought it back — a person, boot Reconcile — is the fresh start
	// the ending promised, so the tolerance starts over with it.
	st.memKills, st.memStopped = 0, false
	w.mu.Unlock()

	w.events.Clear(app.ID, events.KindAppCrashed)
	w.events.Clear(app.ID, events.KindAppFlapping)
	w.events.Clear(app.ID, events.KindAppMemoryLimit)
	w.events.Clear(app.ID, events.KindAppOutOfMemory)
	// Including the supervisor's ending: an app that is online is one nobody
	// needs to be told has stopped being restarted.
	w.events.Clear(app.ID, events.KindAppNotRestarting)
	// After the clears: a loop that is still one is said again, not cleared
	// by the moment it happened to be up.
	if delta > 0 {
		w.sayRestarts(app, m, delta, inWindow, since)
	}
}

// stateFor must be called with the lock held.
func (w *Watcher) stateFor(appID string) *appState {
	st, ok := w.state[appID]
	if !ok {
		st = &appState{}
		w.state[appID] = st
	}
	return st
}

// forget drops everything judged about an app except the restarts already
// counted on a workload it can recognise again. Forgetting that would make the
// next sighting of the same agent entry — still there after a failed redeploy,
// or seen between a person's restart being asked for and performed — report
// restarts it had already reported.
func (w *Watcher) forget(appID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.state[appID]
	if !ok {
		return
	}
	if st.countedOf.IsZero() {
		delete(w.state, appID)
		return
	}
	w.state[appID] = &appState{counted: st.counted, countedOf: st.countedOf}
}

// restartBaseline is the count this sighting's restarts are measured against,
// and whether there is one. Must be called with the lock held.
//
// **A workload created after this watcher started is measured from zero.**
// The untagged baseline is taken on the first sighting, and was the only rule:
// a new release's first sweep comes up to a minute after it started, so an app
// that crashed and was brought back by its backend inside that minute —
// exactly what a test of the restart reporting does — had its restart folded
// into the baseline and said nowhere. A workload that existed before this process did
// is still baselined, because its lifetime count is not this minute's.
func (w *Watcher) restartBaseline(st *appState, m *runtime.AppMetrics) (int, bool) {
	if !m.CreatedAt.IsZero() {
		if st.countedOf.Equal(m.CreatedAt) {
			return st.counted, true
		}
		if m.CreatedAt.After(w.started) {
			return 0, true
		}
	}
	return st.restarts, st.haveRestarts
}

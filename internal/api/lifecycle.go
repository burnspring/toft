package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/schedule"
)

// The restart, as a function rather than a request handler.
//
// Two callers: a person pressing Restart (handleRestart, which keeps the
// authorization and the mapping of the outcome to a status code) and an app's
// `[restart]` schedule (restartScheduler). What separates them is not the
// start — that is one wake group, one status write and one audit row whoever
// asked — but whether the restart *forgives* the app: a person's restart is the
// second half of the supervisor's "until you deploy or restart it", and a
// timetable's must not be, or every verdict the platform reaches about a broken
// app would be undone at four in the morning (ADR §145).

// errRestartLocked and errRestartNoRelease are restartApp's own refusals, for
// a caller that has not already checked. handleRestart has, and answers them
// with the sentences a person reads; the scheduler records them as a decline.
var (
	errRestartLocked    = errors.New("the app is locked")
	errRestartNoRelease = errors.New("the app has never been deployed")
)

// restartOpts is who asked for a restart, and what that asking allows.
type restartOpts struct {
	// Asked is when the instruction was given. A start already in flight that
	// began before it is waited out rather than joined (wakes.DoAfter).
	Asked time.Time
	// The audit row's actor and request metadata, captured by the caller
	// because the start may outlive whatever carried them.
	ActorType string
	ActorID   string
	Meta      database.RequestMeta
	// Forgive resets the supervisor's attempts and the health sweep's memory
	// tolerance. True for a person, false for a schedule.
	Forgive bool
	// Via is recorded on the audit row's `after` when set: "schedule" for a
	// firing, so the row says it was not a person's.
	Via string
}

// restartApp starts or restarts app and records it.
//
// It returns the audit action ("app.restart" or "app.start") and the start's
// error. ctx bounds only the wait: when it ends first the start goes on, the
// closure writes the status and the audit row when it finishes, and the error
// returned is ctx's.
func (s *Server) restartApp(ctx context.Context, app *database.App, o restartOpts) (string, error) {
	if app.Locked() {
		return "", errRestartLocked
	}
	if app.CurrentRelease == "" {
		return "", errRestartNoRelease
	}

	was := app.Status
	// Bringing a stopped app back up and bouncing a running one are one
	// endpoint but two different facts to read a year later, so they are two
	// actions rather than one with a field.
	//
	// A static app asleep is the exception: it has no process of its own to
	// wake, so bringing it back started nothing, and saying `start` beside the
	// note that nothing changed contradicted itself (drills 2026-10-29 #3).
	// Stopped is still a start — a stopped static app was not being served.
	static := builder.Framework(app.Framework) == builder.FrameworkStatic
	action := "app.restart"
	if was == database.AppStatusStopped || (was == database.AppStatusSleeping && !static) {
		action = "app.start"
	}

	// The builder starts it, not the backend. A backend handed a process name
	// cannot start an isolated workload at all and would silently replay a
	// shared one's saved environment; Start assembles the whole description
	// from the app row and the database, which is also what makes a changed
	// variable take effect without a rebuild.
	//
	// The status is written from what was *observed* rather than from a nil
	// error, and it is written in here rather than in the caller because on
	// the 202 path there is no caller left to write it. `running` means a
	// readiness probe answered — which changes shared mode too, where the
	// agent answers once the runner is placed, long before the app is
	// listening.
	if o.Forgive {
		// The supervisor's ending says "until you deploy or restart it", and
		// this is the second half of that sentence — the first is a deploy
		// changing the release the attempts are counted against. Cleared
		// before the start rather than after it: what a person asked for is a
		// fresh five attempts whether or not this one comes up.
		s.supervisor.Reset(app.ID)
		// And the health sweep's memory tolerance, for the same sentence: an
		// app stopped for running out of memory three times says it will not
		// start again until you deploy or restart it, and this is the restart.
		s.health.Reset(app.ID)
		if s.forgiven != nil {
			s.forgiven(app.ID)
		}
	}
	// And the idle clock: a restarted workload has had no chance to be asked
	// for, and a running app restarted in place reads `running` throughout, so
	// this is the only way the reaper mid-sweep learns not to stop it.
	s.idle.Started(app.ID)

	start := func() error {
		// The status this start may change is the one the row holds as it
		// begins — read here, because the start may have waited behind another
		// in the wake group. A deploy's flip, a Stop or a lock that lands while
		// it runs writes its own word, and this must not write over it (DB-4).
		seen := was
		if fresh, err := s.db.GetApp(app.ID); err == nil && fresh != nil {
			seen = fresh.Status
		}
		err := s.builder.Start(app)
		if builder.MissingRelease(err) {
			// Nothing was started and nothing crashed: there is no code on this
			// machine to run. Writing `crashed` here would blame the app for
			// the state of the disk, and the status it already has is the true
			// one.
			return err
		}
		switch {
		case errors.Is(err, builder.ErrStartSuperseded):
			// A deploy flipped first: its workload is the one serving, and its
			// flip wrote the status. Nothing failed.
			return nil
		case errors.Is(err, builder.ErrAppLocked):
			// A lock landed; its own stop took the workload down.
			return errRestartLocked
		}
		to := database.AppStatusRunning
		if err != nil {
			to = database.AppStatusCrashed
		}
		moved, werr := s.db.UpdateAppStatusFrom(app.ID, seen, to)
		if werr != nil {
			log.Printf("restart: marking %s %s: %v", app.Slug, to, werr)
		}
		if moved {
			s.writeLifecycleAuditVia(o.ActorType, o.ActorID, o.Meta, app, action, was, to, o.Via)
		} else if werr == nil && err == nil {
			// A Stop that landed before this start placed its workload found
			// nothing to stop and was answered as a success; the workload is
			// stopped again rather than left live under its `stopped` row.
			s.builder.StopIfNoLongerWanted(app.ID)
		}
		return err
	}

	// The wake group is what collapses two people pressing Restart at the same
	// moment into one start, and it is the same group the proxy's wake uses, so
	// a restart and a wake of one app cannot race each other either.
	//
	// **A restart must not be answered by a start that began before it.** The
	// wake group collapses concurrent starts, which is right for the burst of
	// requests a slept app receives and wrong for an instruction: an
	// administrator attaches storage, the developer restarts, and joining the
	// start already in flight returned that one's success — a pod whose
	// workload description was assembled before the mapping existed, serving
	// with no volumes and exit 0. A second restart, nothing else changed,
	// brought the mounts.
	//
	// `Asked` is when the instruction arrived, so a call older than it is
	// waited out rather than joined.
	return action, s.wakes.DoAfter(ctx, app.ID, o.Asked, start)
}

// --- The scheduled restart (`[restart]` in toft.toml) ---

// restartConcurrency is how many scheduled restarts run at once. `0 4 * * *` is
// what everybody types, and on a shared box fifty R apps starting at once is a
// load spike; four is a queue that drains in minutes.
const restartConcurrency = 4

// restartFiringTimeout bounds one firing's wait: the isolated readiness
// timeout and a margin for the pod to be scheduled, never the HTTP hold.
const restartFiringTimeout = 3 * time.Minute

// The reasons a firing declines, recorded on the row. The list is the
// specification (ADR §145): a schedule restarts a running app and
// nothing else.
const (
	declineSleeping      = "sleeping"
	declineStopped       = "stopped"
	declineCrashed       = "crashed"
	declineDeployPending = "deploy_pending"
	declineLocked        = "locked"
	declineNoProcess     = "no_process"
)

// restartable is why a scheduled restart must not touch app, or "" when it may.
//
//   - sleeping: the next request is a fresh start, which is what the restart
//     was for — and a firing that woke it would hold it awake every night.
//   - stopped: a person stopped it, and only a person starts it again.
//   - crashed: the supervisor or the health sweep reached a verdict, and a
//     timetable does not reset it.
//   - building, failed, ready, or a release with no process of its own
//     (batch, static, a document): there is nothing to restart. Detect refuses
//     `[restart]` on the last three; this is the backstop.
//   - locked: a lock refuses restarts from anyone.
func restartable(app *database.App) string {
	if app.Locked() {
		return declineLocked
	}
	fw := app.Framework
	if builder.IsBatchFramework(fw) || fw == string(builder.FrameworkStatic) || builder.Rerenderable(fw) {
		return declineNoProcess
	}
	switch app.Status {
	case database.AppStatusRunning:
		return ""
	case database.AppStatusSleeping:
		return declineSleeping
	case database.AppStatusStopped:
		return declineStopped
	case database.AppStatusCrashed:
		return declineCrashed
	case database.AppStatusReady:
		return declineNoProcess
	}
	return declineNoProcess
}

// RestartScheduler fires `[restart]` schedules. It lives here rather than in
// internal/builder, beside the re-render's, because what it calls is
// restartApp — the wake group, the status write and the audit row are this
// package's.
type RestartScheduler struct {
	s      *Server
	events *events.Recorder
	sem    chan struct{}

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// NewRestartScheduler is the loop over app_restart_schedules.
func NewRestartScheduler(s *Server, rec *events.Recorder) *RestartScheduler {
	return &RestartScheduler{s: s, events: rec, sem: make(chan struct{}, restartConcurrency), stop: make(chan struct{})}
}

// Start launches the tick loop. Started whatever the estate holds, as the
// re-render's is: the claim is one indexed query against a table that is empty
// on an install where no app declares `[restart]`.
func (rs *RestartScheduler) Start() {
	schedule.Loop(rs.stop, &rs.wg, schedule.TickInterval, rs.tick)
}

// Close stops the loop and waits for the firings in flight.
func (rs *RestartScheduler) Close() {
	rs.once.Do(func() { close(rs.stop) })
	rs.wg.Wait()
}

func (rs *RestartScheduler) tick() {
	due, disabled, err := rs.s.db.ClaimDueRestartSchedules(schedule.ClaimLimit, time.Now().UTC(), schedule.Next)
	if err != nil {
		log.Printf("restart schedule: claiming due schedules: %v", err)
		return
	}
	for _, d := range disabled {
		rs.events.Record(events.Event{
			AppID: d.AppID, Kind: events.KindScheduleDisabled,
			Severity: database.EventSeverityWarning, Source: database.EventSourceBuilder,
			Summary: fmt.Sprintf("The scheduled restart was turned off because %q is not a valid cron expression",
				d.Schedule),
			Detail:      "Correct [restart] in the app's toft.toml and deploy.",
			Fingerprint: d.ID,
		})
	}
	// Each in its own goroutine, four at a time: a restart waits up to the
	// readiness timeout, and an inline firing would hold every app behind it.
	for _, sch := range due {
		rs.wg.Add(1)
		go func(sch database.RestartSchedule) {
			defer rs.wg.Done()
			rs.sem <- struct{}{}
			defer func() { <-rs.sem }()
			rs.fire(sch)
		}(sch)
	}
}

// fire restarts one app for one claimed schedule, or records why it did not.
// Every firing leaves state on the row: a decline is a row, never a gap.
func (rs *RestartScheduler) fire(sch database.RestartSchedule) {
	now := time.Now().UTC()
	app, err := rs.s.db.GetApp(sch.AppID)
	if err != nil || app == nil {
		return // the row cascades with the app; this is a race, not a state
	}
	record := func(outcome, reason string) {
		if err := rs.s.db.RecordRestartOutcome(sch.ID, now, outcome, reason); err != nil {
			log.Printf("restart schedule: recording the outcome for %s: %v", app.Slug, err)
		}
	}
	if why := restartable(app); why != "" {
		record(database.RestartOutcomeSkipped, why)
		return
	}
	// The deploy starts a fresh process anyway, and a restart now would only
	// race it.
	pending, err := rs.s.db.HasPendingDeployment(app.ID, now.Add(-builder.PendingDeployWindow))
	if err != nil {
		log.Printf("restart schedule: reading pending deployments for %s: %v", app.Slug, err)
		return
	}
	if pending {
		record(database.RestartOutcomeSkipped, declineDeployPending)
		return
	}
	if rs.s.builder == nil {
		record(database.RestartOutcomeSkipped, declineNoProcess)
		return
	}
	if _, err := rs.s.runtimeFor(app); err != nil {
		record(database.RestartOutcomeSkipped, declineNoProcess)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), restartFiringTimeout)
	defer cancel()
	// Not forgiven: a timetable is not the "or restart it" the supervisor's
	// ending promises. The system as the actor, and `via` says which system.
	_, err = rs.s.restartApp(ctx, app, restartOpts{
		Asked: now, ActorType: "system", Via: "schedule",
	})
	switch {
	case err == nil:
		record(database.RestartOutcomeRestarted, "")
	case errors.Is(err, errRestartLocked):
		record(database.RestartOutcomeSkipped, declineLocked)
	case errors.Is(err, errRestartNoRelease), builder.MissingRelease(err):
		record(database.RestartOutcomeSkipped, declineNoProcess)
	case ctx.Err() != nil:
		record(database.RestartOutcomeFailed, "the app did not answer within "+restartFiringTimeout.String())
	default:
		// The status is crashed now, and the supervisor takes it from there;
		// a scheduled restart is never retried by the schedule.
		record(database.RestartOutcomeFailed, err.Error())
	}
}

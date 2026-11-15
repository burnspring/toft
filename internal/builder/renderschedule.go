package builder

import (
	"fmt"
	"log"
	"sync"
	"time"

	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/schedule"
)

// The second scheduler loop, and the smaller of the two.
//
// It is here, in `internal/builder`, rather than beside the jobs loop, because
// what it fires is a *deploy* — the same `BuildJob` the Redeploy button
// enqueues. It has no runs, no overlap policy, no retries, no sweep and no
// timeout of its own: a re-render's history is the app's deployment history,
// its concurrency is the build queue's, and its outcome is a deployment row.
// One thing happens per claimed schedule, and it is `Enqueue`.
//
// **Why a re-render is a deploy and not either kind of automation** (ADR §33).
// Not a scheduled task: a task is an HTTP request to a path the app published,
// and a file server has no such path. Not a job: a job needs `jobs_enabled` and
// an isolated backend, which would make the headline half of notebooks absent
// on the one-box shared install — and, decisively, a job cannot change what the
// workload serves, because a start replaces the tree. A re-render not published
// as a new release is a re-render that vanishes at the next wake.
//
// The costs of that, stated rather than designed around: nightly git traffic, a
// dependency gate that can now refuse a nightly report, and a release per night
// against version retention.

// RenderScheduler enqueues a build for every notebook whose schedule is due.
type RenderScheduler struct {
	db      *database.DB
	builder *Builder
	events  *events.Recorder

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func NewRenderScheduler(db *database.DB, b *Builder, rec *events.Recorder) *RenderScheduler {
	return &RenderScheduler{db: db, builder: b, events: rec, stop: make(chan struct{})}
}

// Start launches the tick loop. Started whatever the estate holds: the claim is
// one indexed query against a table that is empty on an install with no
// notebooks, which is cheaper than a capability check that would have to be
// re-read anyway.
func (s *RenderScheduler) Start() {
	schedule.Loop(s.stop, &s.wg, schedule.TickInterval, s.tick)
}

// Close stops the loop and waits for the tick in flight.
func (s *RenderScheduler) Close() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *RenderScheduler) tick() {
	due, disabled, err := s.db.ClaimDueRenderSchedules(schedule.ClaimLimit, time.Now().UTC(), schedule.Next)
	if err != nil {
		log.Printf("render schedule: claiming due schedules: %v", err)
		return
	}
	s.reportDisabled(disabled)
	for _, sch := range due {
		s.fire(sch)
	}
}

// fire turns one claimed schedule into one queued build.
//
// Everything that could refuse it is checked by the same code a manual redeploy
// goes through — the lock, the source policy, the dependency gate, the version
// cap — because this makes the *same job*. What is checked here is only what
// would be wasted work or a lie: an app that has gone, one that is not a
// notebook any more, and a build already queued for it.
func (s *RenderScheduler) fire(sch database.RenderSchedule) {
	app, err := s.db.GetApp(sch.AppID)
	if err != nil || app == nil {
		// The row cascades with the app, so this is a race rather than a state.
		return
	}

	// A backstop: detect refuses [render] on a release that is not a
	// document (render_not_a_document), so a row reaching here for one is a
	// race with a deploy in flight. It simply does not fire.
	if !Rerenderable(app.Framework) {
		return
	}

	// Locked apps refuse deploys, and enqueuing one would produce a failed
	// deployment row every night for a decision an administrator already made.
	if app.LockedAt != nil {
		return
	}

	// A person's Stop stands. Every other timetable in the product declines a
	// stopped app — auto-deploy, the scheduled restart, a move — and this one
	// did not: the flip wrote `running` over `stopped` and mailed the
	// recipients a link to a document somebody had taken down (code review
	// 2026-11-11, C-2). A sleeping app is the platform's doing, not a
	// person's, and is re-rendered.
	if !renderFiresFor(app.Status) {
		log.Printf("render schedule: %s is stopped; a person starts it again, and the schedule resumes then", app.Slug)
		return
	}

	// One queued build per app, the same coalescing the Redeploy handler does.
	// A schedule that fires while last night's build is still queued must not
	// stack a second one behind it.
	pending, err := s.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-PendingDeployWindow))
	if err != nil {
		log.Printf("render schedule: reading pending deployments for %s: %v", app.Slug, err)
		return
	}
	if pending {
		return
	}

	deploy := &database.Deployment{
		AppID:       app.ID,
		Branch:      app.Branch,
		Status:      database.DeployStatusPending,
		TriggeredBy: database.DeployTriggerRender,
	}
	// The system's, with no actor: a schedule fires without anyone, and who
	// declared it is the deploy that introduced its [render]. A re-render re-clones the
	// branch tip, so new code reaches viewers through this door nightly.
	via := "git"
	if IsUploadedSource(app.RepoURL) {
		via = "upload"
	}
	if err := s.db.CreateDeployment(deploy, &database.DeployRecord{ActorType: "system", Via: via}); err != nil {
		log.Printf("render schedule: creating a deployment for %s: %v", app.Slug, err)
		return
	}

	// An uploaded app has no remote to clone from; its new release is seeded
	// from the current one, exactly as a manual redeploy of one is.
	s.builder.Enqueue(&BuildJob{
		App:        app,
		Deployment: deploy,
		SkipClone:  IsUploadedSource(app.RepoURL),
	})
}

// reportDisabled tells an app's owners that its schedule was switched off
// because the platform can no longer read it — the same treatment tasks and
// jobs get, and for the same reason: a nightly report that simply stops, with
// nothing anywhere saying why, is the exact failure app events exist for.
func (s *RenderScheduler) reportDisabled(disabled []database.DisabledSchedule) {
	for _, d := range disabled {
		s.events.Record(events.Event{
			AppID:    d.AppID,
			Kind:     events.KindScheduleDisabled,
			Severity: database.EventSeverityWarning,
			Source:   database.EventSourceBuilder,
			Summary: fmt.Sprintf("The re-render schedule was turned off because %q is not a valid cron expression",
				d.Schedule),
			Detail:      "Correct [render] in the app's toft.toml and deploy.",
			Fingerprint: d.ID,
		})
	}
}

// Rerenderable reports whether an app's framework produces a rendered document
// at deploy time, which is what a render schedule re-runs: a notebook, an R
// Markdown document, a Quarto document under knitr. The three are one shape to
// this mechanism — executed in the build, served as files — and a report written
// in R is the reason a schedule exists at all as often as one written in Python.
//
// Not marimo, which executes per viewer, and nothing that runs a server. The
// list is the specification: the API, this scheduler and the SPA's
// `isRerenderable` all ask it, so a document framework added to one is added to
// all three.
func Rerenderable(framework string) bool {
	switch Framework(framework) {
	case FrameworkNotebook, FrameworkRMarkdown, FrameworkQuarto:
		return true
	}
	return false
}

// renderFiresFor is whether a scheduled re-render fires for an app in this
// status: every status but stopped, the same rule as autodeploy.deploysOnPush.
func renderFiresFor(status string) bool {
	return status != database.AppStatusStopped
}

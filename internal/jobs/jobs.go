// Package jobs is the whole background-job lifecycle: the schedule, the
// queue's state machine, the dispatcher that starts work, and — the part that
// actually earns its keep — the reconciler that finds work the platform lost
// track of.
//
// The single deep module of the job system. Everything else is an adapter
// around it: the API is a thin authorization layer over these calls, the
// runner is a client of the contract, and both isolated backends are just
// places to put a workload.
//
// Three properties shape it, and none is optional:
//
//   - **Durable.** The queue is Postgres. A build that vanishes with a restart
//     is visible; a job that vanishes is not, and a nightly report that
//     silently never ran is worse than one that failed loudly.
//   - **Multi-node safe.** Every claim is `FOR UPDATE SKIP LOCKED`. Two
//     dispatchers partition the queue rather than racing, because a job that
//     runs twice is not a recoverable mistake for a payment batch.
//   - **A reconciler, not a launcher.** In-flight state lives in the database
//     and is re-derived from the *backend* on boot. Nothing important is held
//     in memory, because the process holding it is the thing that restarts.
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/events"

	"toft/internal/runtime"
	"toft/internal/schedule"
	"toft/internal/telemetry"
	"toft/internal/textutil"
)

// Tick intervals. Deliberately coarse: a minute-granularity cron needs no
// finer resolution, and a tighter loop would mean more claim queries for no
// behaviour anyone can observe.
const (
	dispatchInterval = 5 * time.Second
	// sweepInterval bounds how long a stranded claim survives, and how late a
	// timeout is enforced when the timer armed at start did not survive (a
	// platform restart) — the timer is what makes it on time.
	sweepInterval = 30 * time.Second
	// claimTimeout is how long a claimed-but-not-started run waits before it
	// is assumed orphaned and returned to the pool. Generous, because starting
	// a workload on a cold cluster is genuinely slow, and a run returned to
	// the pool while its workload is actually starting would run twice.
	claimTimeout = 10 * time.Minute

	// Retention, when the settings table cannot be read. Logs go first and rows
	// outlive them: a run's cost and outcome stay interesting long after its
	// output does.
	//
	// A run's *output object* is on the row clock rather than the log clock,
	// and deliberately so: an artifact that died at 14 days would leave
	// `mine()` listing a succeeded run with nothing behind it, which is the
	// worst failure shape available here — status outliving its payload.
	//
	// These are a fallback now rather than the policy. The policy is
	// job_log_retention_days and job_run_retention_days, read at sweep time so
	// a change takes effect on the next sweep rather than on the next restart —
	// the figures happen to be the same, because these were the figures the
	// defaults were taken from.
	fallbackLogRetention = 14 * 24 * time.Hour
	fallbackRowRetention = 90 * 24 * time.Hour

	// derivedKeyWindow is how long a start with the platform's *derived* key
	// (the actor and the parameters) returns the run it matched rather than
	// beginning a new one. Long enough to swallow a double-click or a
	// Streamlit rerun, short enough that pressing the button again a minute
	// later means what the person pressing it thinks it means.
	derivedKeyWindow = 60 * time.Second
	// explicitKeyWindow is the same for a key the caller chose: the run that
	// key started is the answer for a day, whatever has happened to it since.
	// A caller that names a key is saying "this piece of work, once".
	explicitKeyWindow = 24 * time.Hour
)

// Backend is the slice of the isolated job runtime this package needs. It is
// an interface here rather than a concrete type so the ECS and Kubernetes
// backends are interchangeable, and so tests can drive the lifecycle without
// a cluster.
type Backend interface {
	Start(ctx context.Context, spec runtime.WorkloadSpec) (handle string, err error)
	Status(ctx context.Context, handle string) (runtime.JobStatus, error)
	Cancel(ctx context.Context, handle string) error
}

// Store is the database surface. Named so tests can substitute one, and so the
// set of queries this package depends on is visible in one place.
type Store interface {
	// ClaimDueSchedules seeds, claims and advances due schedules in one pass,
	// and hands back anything it disabled so this package can say so. The claim
	// itself is shared with the task scheduler (database/schedules.go).
	ClaimDueSchedules(limit int, now time.Time, next database.NextFunc) ([]database.DueJob, []database.DisabledSchedule, error)
	SetNextRunAt(id string, at *time.Time) error

	// EnqueueJobRunIdempotent returns a non-nil run when an existing one
	// matched r's key and nothing was inserted.
	EnqueueJobRunIdempotent(r *database.JobRun, maxQueued int, window time.Duration) (*database.JobRun, error)
	// EnqueueScheduledJobRun reports true when the firing became a skipped
	// run — the overlap policy, or a full queue, which also returns its
	// *database.QueueLimitError. database.ErrTickAlreadyEnqueued is a tick that
	// already has its run.
	EnqueueScheduledJobRun(r *database.JobRun, overlap string, maxQueued int) (bool, error)
	ClaimJobRuns(nodeID string, limit, perAppRunning int) ([]database.JobRun, error)
	// MarkJobRunStarted refuses (sql.ErrNoRows) unless nodeID still holds
	// the claim.
	MarkJobRunStarted(runID, nodeID, externalID string, deadline time.Time) error
	// FinishJobRunOnce reports whether this call made the run terminal; see
	// finish.
	FinishJobRunOnce(runID, status string, exitCode *int, stage, reason string) (bool, error)
	// TimeOutJobRun reports whether this call moved the run; see timeOut.
	TimeOutJobRun(runID, reason string) (bool, error)
	ListActiveJobRuns() ([]database.JobRun, error)
	ListOverdueJobRuns(now time.Time) ([]database.JobRun, error)
	ReleaseStaleClaims(olderThan time.Time) (int64, error)
	// ReleaseClaimsOf returns to the queue every claim nodeID holds and never
	// started: at boot, the previous process of this node's.
	ReleaseClaimsOf(nodeID string) (int64, error)
	// The stop a terminal run's workload is still owed (job_runs.stop_owed_at):
	// TimeOutJobRun and CancelJobRun set it with their terminal write; every
	// other path that stops a terminal run's workload marks it first, and the
	// sweep asks again until the backend says the workload is gone.
	MarkJobRunStopOwed(runID, handle string) error
	ListStopOwedJobRuns(limit int) ([]database.StopOwedRun, error)
	ClearStopOwed(runID string) error
	ExpireJobRuns(logsBefore, rowsBefore time.Time) (database.ExpiredRuns, error)
	ListOverdueJobs(now time.Time) ([]database.OverdueJob, error)
	LastStderrLines(runID string, n int) ([]string, error)
	GetJobRun(runID string) (*database.JobRun, error)
	// NoteJobRunPeakMemory raises the run's recorded peak, never lowers it.
	NoteJobRunPeakMemory(runID string, mb int) error

	GetApp(id string) (*database.App, error)
	// AppMounts is the app's durable storage, in the seam's vocabulary. A job
	// gets exactly what its app's service pods get — same function, same rows —
	// which is what makes sharing between an app and its own background work
	// need no additional concept.
	AppMounts(appID string) ([]runtime.Mount, error)
	GetJobDefinition(appID, id string) (*database.JobDefinition, error)
	GetJobDefinitionByName(appID, name string) (*database.JobDefinition, error)
	GetPlatformSettings() (database.PlatformSettings, error)
}

type Options struct {
	// Host is the backend that runs a job on the platform's own machine
	// (ADR §106) — where a run is placed when isolated execution is not
	// available. Nil on an install that runs no shared-mode workloads.
	Host Backend

	// NodeID identifies this dispatcher in claim rows, so a stranded claim can
	// be attributed to the node that died holding it. Stable across restarts
	// (main passes the hostname), which is what lets boot Reconcile return the
	// previous process's unstarted claims at once; empty draws a fresh one.
	NodeID string
	// MaxConcurrentDispatch bounds how many runs this node starts per tick.
	// Starting a workload is a network call to a cluster, and an unbounded
	// burst would be a self-inflicted thundering herd.
	MaxConcurrentDispatch int
	// Events reports a run that ended badly to the app's owners. A job run is
	// the most invisible thing on the platform — no request is waiting on it
	// and no page shows it unless somebody goes looking.
	//
	// May be nil; every call through it is nil-safe.
	Events *events.Recorder
	// Outputs removes a run's output object when its row expires. Nil on an
	// install with no object store, where there is nothing to remove and no
	// jobs either.
	Outputs OutputStore
	// Dials resolves the operating envelope at dispatch — the memory ceiling and
	// the scheduler's two figures, applied to a job pod exactly as they are to a
	// service.
	//
	// Read at dispatch rather than held: a run whose definition names no memory
	// is capped by the app's ceiling or the install's, and until this was a
	// resolver it was a copy of APP_MEMORY_LIMIT_MB taken at process start — so
	// an administrator who raised the setting raised it for services and not for
	// job runs, and nothing on any screen said so.
	//
	// A job definition may raise its own memory, and deliberately not its own
	// CPU. Memory is per-workload because a report that needs 4 GB genuinely
	// needs 4 GB and failing without it is the whole outcome; CPU is the
	// operator's arithmetic about a *node*, and a definition that could raise
	// its reservation would be an app deciding how much of a shared machine it
	// is worth. That is the same argument as build parameters being the
	// operator's, and it is why there is no per-definition field here.
	Dials *config.Resolver
}

// OutputStore is the slice of the artifact store the sweep needs: one verb, so
// that pinning an object's lifetime to its row costs this package no knowledge
// of key layout, presigning, or which backend is configured.
type OutputStore interface {
	Delete(ctx context.Context, key string) error
}

// Service owns the scheduler, the dispatcher, and the sweep.
type Service struct {
	store   Store
	backend Backend
	outputs OutputStore
	opts    Options

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once

	// wake asks the dispatcher to look now rather than at its next tick. A
	// queued run sat up to the whole dispatch interval before anything claimed
	// it, which is most of the wait somebody pressing "run now" sees.
	wake chan struct{}

	// starting holds one token per start in progress, MaxConcurrentDispatch
	// deep. The dispatcher claims only as many runs as there are free tokens
	// and never waits for a start to finish: a host start resolves the run's
	// release first — a download and an unpack, ten seconds and more on a
	// network filesystem — and while the loop waited on it a run queued
	// behind it sat at `pending` with its wake already spent, so its queue
	// time was the whole of somebody else's start plus its own.
	starting chan struct{}

	// now is swappable so tests drive time rather than sleeping through it.
	now func() time.Time
}

func New(store Store, backend Backend, opts Options) *Service {
	if opts.NodeID == "" {
		opts.NodeID = fmt.Sprintf("node-%d", time.Now().UnixNano())
	}
	if opts.MaxConcurrentDispatch < 1 {
		opts.MaxConcurrentDispatch = 5
	}
	return &Service{
		store: store, backend: backend, outputs: opts.Outputs, opts: opts,
		stop:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		starting: make(chan struct{}, opts.MaxConcurrentDispatch),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Start runs the three loops. Reconciliation happens first and synchronously
// enough to matter: dispatching new work before re-adopting what is already
// running would let the concurrency limit be exceeded by exactly the runs the
// platform had forgotten about.
func (s *Service) Start() {
	s.Reconcile()

	// One loop helper and one schedule interval, shared with the re-render
	// scheduler.
	// Dispatch and sweep keep cadences of their own: they are not scheduling,
	// and what bounds them is how fast a queue should drain and how late a
	// deadline may be enforced.
	schedule.Loop(s.stop, &s.wg, schedule.TickInterval, s.tickSchedules)
	s.dispatchLoop()
	schedule.Loop(s.stop, &s.wg, sweepInterval, s.tickSweep)
}

// dispatchLoop is schedule.Loop with a second way in: the tick still drains
// what another node queued or a slot that freed, and wake answers a run this
// node just queued without waiting for it.
func (s *Service) dispatchLoop() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(dispatchInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.pollShared()
			case <-s.wake:
			case <-s.stop:
				return
			}
			// Not waited on: see Service.starting.
			s.dispatch()
		}
	}()
}

// wakeDispatch nudges the dispatcher. Never blocks: one pending nudge is as
// good as many, because a tick claims everything that is due.
func (s *Service) wakeDispatch() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Close() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// --- Scheduling ---

// tickSchedules fires everything due and enqueues a run for each.
//
// Seeding, claiming and advancing are one call, shared with the re-render
// scheduler (database/schedules.go) — that is where the multi-node claim lives
// and it is the piece worth having exactly one of. What stays here is what a
// job's firing actually is: a row on a durable queue, or a skipped one when the
// job's overlap policy says so.
func (s *Service) tickSchedules() {
	if !s.enabled() {
		return
	}

	due, disabled, err := s.store.ClaimDueSchedules(schedule.ClaimLimit, s.now(), schedule.Next)
	if err != nil {
		log.Printf("jobs: claiming due schedules: %v", err)
		return
	}

	// A schedule whose expression no longer parses is disabled by the claim and
	// reported here, because internal/database cannot import internal/events
	// without a cycle. This is the branch that never once ran before the two
	// schedulers were merged: the old claim filtered disabled rows out *before*
	// returning them, so a nightly job simply stopped and nothing anywhere said
	// why — the exact shape of failure app events exist for.
	for _, d := range disabled {
		s.opts.Events.Record(events.Event{
			AppID:    d.AppID,
			Kind:     events.KindScheduleDisabled,
			Severity: database.EventSeverityWarning,
			Source:   database.EventSourceJobs,
			Summary: fmt.Sprintf("The schedule for %q was turned off because %q is not a valid cron expression",
				d.Name, d.Schedule),
			Detail:      "Fix the schedule in the app's toft.toml and deploy.",
			Fingerprint: d.ID,
		})
	}

	for _, def := range due {
		// The tick in its schedule's zone, so the run records the offset and
		// every read of it — toft job runs, jobs.get, a rerun — says
		// 12:34:00-05:00 as TOFT_SCHEDULED_FOR does, not 17:34:00Z (BB-050,
		// fifth 2026-11-03 black-box list).
		tick := TickInZone(def.Tick, def.Schedule)
		_, skipped, err := s.startRun(StartRequest{
			AppID: def.AppID, Job: def.Name, StartedBy: database.JobStartedBySchedule,
			ScheduledFor: &tick,
		})
		switch {
		case err == nil && skipped:
			telemetry.RecordJobRun(context.Background(), telemetry.OutcomeSkipped,
				telemetry.TriggerSchedule, 0, 0)
		case errors.Is(err, database.ErrTickAlreadyEnqueued):
			// The tick already has its run: nothing was written, and there is
			// nothing to tell anybody. Reaching here means next_run_at came
			// round to a tick twice, which the store refuses rather than trusts.
			log.Printf("jobs: scheduled run for %s/%s at %s already enqueued", def.AppID, def.Name, tick.UTC().Format(time.RFC3339))
		case errors.Is(err, ErrAppLocked), errors.Is(err, errNotPublished):
			// Not a failure to report: the lock is the explanation, and the
			// app already says it is locked wherever anybody looks; an app
			// still on its first build has nothing to run yet.
		case err != nil:
			if skipped {
				// A full queue: the store wrote the skipped row, and the event
				// below is the part an owner is alerted by.
				telemetry.RecordJobRun(context.Background(), telemetry.OutcomeSkipped,
					telemetry.TriggerSchedule, 0, 0)
			}
			s.opts.Events.Record(events.Event{
				AppID:    def.AppID,
				Kind:     events.KindJobScheduleSkipped,
				Severity: database.EventSeverityWarning,
				Source:   database.EventSourceJobs,
				Summary:  fmt.Sprintf("The scheduled run of %q did not start: %s", def.Name, skipReason(err)),
				Detail: "The schedule has moved on to its next time, and this firing will not run. " +
					"Runs already waiting count against the app's queue until they start.",
				Fingerprint: def.ID + ":skipped",
			})
			// A schedule that cannot enqueue is *not* re-fired into the next
			// window: the claim already advanced next_run_at, and re-firing here
			// would turn a busy app's full queue into a backlog that never
			// drains.
			log.Printf("jobs: scheduled run for %s/%s: %v", def.AppID, def.Name, err)
		}
	}
}

// --- Enqueue ---

// ErrJobsUnavailable means this install or this app cannot run jobs. Distinct
// from a validation error so the API can answer with the explanation rather
// than a generic refusal.
var ErrJobsUnavailable = errors.New("background jobs are not available")

// ErrAppLocked refuses a run of a locked app. A job is a different entrypoint of
// the same app, so a lock that stopped the front door and left this one open
// would run the app's own code under a decision that it must not run.
var ErrAppLocked = errors.New("this app is locked")

// errNotPublished is a schedule firing for an app that has never published a
// release. A toft.toml's jobs are recorded at the deploy's detect step, long
// before the build publishes anything, so an every-minute schedule on a slow
// first build fired into dispatch's "no published release" — a failed run, an
// event and, with notify = "failures", a message to the owners, all before the
// app had ever served (drills on trytoft, 2026-10-23). The firing is not a
// failure: there is nothing to run yet, and the next one finds the release.
var errNotPublished = errors.New("this app has not published a release yet")

// StartRequest is one start of a named job, from whichever of the five ways
// in: the schedule, the CLI, the app as itself, the app for a viewer, or Run
// now in Toft's UI.
type StartRequest struct {
	AppID string
	Job   string
	// StartedBy is database.JobStartedBy*. ActorID is the person, for viewer
	// and ui starts and for a person's key; empty for the app and the schedule.
	StartedBy string
	ActorID   string
	Params    []byte
	// IdempotencyKey collapses a repeated start onto the run it matched.
	// ExplicitKey says the caller chose it — matched for a day — rather than
	// the platform deriving it — matched for a minute.
	IdempotencyKey string
	ExplicitKey    bool
	// ScheduledFor is the tick, or the instant a person named with --for.
	ScheduledFor *time.Time
	// RerunOf is the run this one deliberately repeats.
	RerunOf string
}

// StartRun queues a run and returns it, or the run an idempotency key matched —
// the second return reports which. Nothing waits on the work.
//
// The app's own execution mode is not consulted. A run is placed by the
// install (a container where a cluster is available, this machine otherwise),
// so the only questions are install-level ones, which ExplainJobsUnavailable
// answers.
func (s *Service) StartRun(req StartRequest) (*database.JobRun, bool, error) {
	run, deduped, err := s.startRun(req)
	if err != nil {
		return nil, false, err
	}
	if run != nil && run.Status == database.JobRunSkipped {
		// Only the schedule passes an overlap policy, so this is unreachable
		// from here; said rather than assumed.
		return run, false, nil
	}
	return run, deduped, err
}

// Rerun starts a new run of a finished run's job, with the same params and
// scheduled_for, recorded as a rerun of it. Running something again is a
// deliberate act and this is the one way to do it: nothing re-runs by itself.
func (s *Service) Rerun(appID, runID, startedBy, actorID string) (*database.JobRun, error) {
	orig, err := s.store.GetJobRun(runID)
	if err != nil {
		return nil, err
	}
	if orig == nil || orig.AppID != appID {
		return nil, ErrNoSuchRun
	}
	if !database.JobRunTerminal(orig.Status) {
		return nil, ErrRunStillOpen
	}
	run, _, err := s.startRun(StartRequest{
		AppID: appID, Job: orig.JobName, StartedBy: startedBy, ActorID: actorID,
		Params: orig.Params, ScheduledFor: orig.ScheduledFor, RerunOf: orig.ID,
	})
	return run, err
}

// ErrNoSuchRun and ErrRunStillOpen are Rerun's two refusals.
var (
	ErrNoSuchRun    = errors.New("no such run")
	ErrRunStillOpen = errors.New("this run has not finished; cancel it or wait for it before running it again")
)

// ErrNoSuchJob is a start naming a job toft.toml does not declare.
var ErrNoSuchJob = errors.New("no such job")

func (s *Service) startRun(req StartRequest) (*database.JobRun, bool, error) {
	app, err := s.store.GetApp(req.AppID)
	if err != nil || app == nil {
		return nil, false, fmt.Errorf("app not found")
	}
	// Here rather than in each caller, because this is the one function every
	// start passes through. The schedule had no check of its own once, so a
	// locked app's nightly job kept running while the proxy, deploy and the Run
	// button all refused.
	if app.Locked() {
		return nil, false, ErrAppLocked
	}
	if req.StartedBy == database.JobStartedBySchedule && app.ReleaseRef == "" {
		return nil, false, errNotPublished
	}

	caps := s.capabilities()
	if why := caps.ExplainJobsUnavailable(runtime.Mode(app.ExecutionMode)); why != "" {
		return nil, false, fmt.Errorf("%w: %s", ErrJobsUnavailable, why)
	}

	// The run references its definition, not just its name: everything
	// downstream reads the command, timeout and memory through this link. The
	// *name* is denormalized alongside so history survives the definition
	// leaving toft.toml.
	def, err := s.store.GetJobDefinitionByName(req.AppID, req.Job)
	if err != nil {
		return nil, false, err
	}
	if def == nil {
		return nil, false, fmt.Errorf("%w: this app's toft.toml declares no job called %s", ErrNoSuchJob, req.Job)
	}

	params, err := checkParams(def.ParamsSpec, req.Params)
	if err != nil {
		return nil, false, err
	}

	settings, _ := s.store.GetPlatformSettings()
	run := &database.JobRun{
		DefinitionID:   def.ID,
		AppID:          req.AppID,
		JobName:        req.Job,
		StartedBy:      req.StartedBy,
		ActorID:        req.ActorID,
		Params:         params,
		IdempotencyKey: req.IdempotencyKey,
		// Where the install places runs, not the app's own mode. The run's mode
		// is what cost reporting classifies by, and what actually happens is
		// decided once for the install.
		ExecutionMode: string(caps.JobPlacement()),
		ReleaseRef:    app.ReleaseRef,
		ScheduledFor:  req.ScheduledFor,
		RerunOf:       req.RerunOf,
	}

	if req.StartedBy == database.JobStartedBySchedule {
		skipped, err := s.store.EnqueueScheduledJobRun(run, def.Overlap, settings.MaxQueuedRunsPerApp)
		if err != nil {
			// skipped beside an error is a full queue, recorded as a skipped
			// run; the caller still needs the error for its event.
			return nil, skipped, err
		}
		if !skipped {
			s.wakeDispatch()
		}
		return run, skipped, nil
	}

	window := derivedKeyWindow
	if req.ExplicitKey {
		window = explicitKeyWindow
	}
	existing, err := s.store.EnqueueJobRunIdempotent(run, settings.MaxQueuedRunsPerApp, window)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		// A key names one piece of work. The same key carrying a different
		// job, different params or a different scheduled_for is a caller bug —
		// a reused counter, a key built from the wrong field — and handing back
		// the old run would report the new work as started when it never was.
		if !sameStart(existing, run) {
			return nil, false, ErrIdempotencyKeyReused
		}
		// An explicit key from an app is shared by every viewer the app's
		// code sends it for, and there is no cross-person coalescing (ADR
		// §26): a viewer's start matching another person's run is refused,
		// not handed that run as theirs. Not in sameStart, which the CLI and
		// UI route share, where two editors replaying one key may both read
		// the run.
		if req.ExplicitKey && (existing.StartedBy == database.JobStartedByViewer || run.StartedBy == database.JobStartedByViewer) &&
			existing.ActorID != run.ActorID {
			return nil, false, ErrIdempotencyKeyReused
		}
		return existing, true, nil
	}
	s.wakeDispatch()
	return run, false, nil
}

// ErrIdempotencyKeyReused is a start whose idempotency key matched a run of
// different work (BB-010): 409 idempotency_key_reused, never the old run.
var ErrIdempotencyKeyReused = errors.New("this idempotency key already started a run with a " +
	"different job, params or scheduled_for, or for a different person — a key names one piece of work; " +
	"use a new key for new work")

// sameStart says whether a matched run is the work a new start asks for. Params
// compare as JSON values, so key order and whitespace never count as a change;
// both sides have been through checkParams, so a declared default is filled in
// on each alike.
func sameStart(existing, wanted *database.JobRun) bool {
	if existing.JobName != wanted.JobName {
		return false
	}
	if (existing.ScheduledFor == nil) != (wanted.ScheduledFor == nil) ||
		(existing.ScheduledFor != nil && !existing.ScheduledFor.Equal(*wanted.ScheduledFor)) {
		return false
	}
	return sameJSON(existing.Params, wanted.Params)
}

func sameJSON(a, b []byte) bool {
	norm := func(raw []byte) (any, bool) {
		if len(bytes.TrimSpace(raw)) == 0 {
			raw = []byte("{}")
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false
		}
		return v, true
	}
	va, okA := norm(a)
	vb, okB := norm(b)
	if !okA || !okB {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(va, vb)
}

// --- Dispatch ---

// tickDispatch claims pending runs and starts them, and returns once every
// start it began has finished. The loop does not wait (it calls dispatch);
// this is the whole pass, for a caller that wants its outcome.
func (s *Service) tickDispatch() {
	s.dispatch().Wait()
}

// dispatch claims as many queued runs as there are free start slots — and as
// each app's runs-at-once limit allows — and starts each on its own goroutine,
// returning without waiting for them.
//
// Side by side, and not waited on. One at a time, a host run resolving its
// release or a pod waiting on a cold node held every run claimed behind it at
// `claimed`; waited on as a group, it held every run *queued* behind it,
// because the loop could not claim again until the slowest start of
// the last pass returned. Each claim is its own row, so nothing is shared
// between the starts but the backend, which already serves concurrent
// requests. A finished start wakes the dispatcher, since the slot it frees may
// be the one a queued run is waiting for.
func (s *Service) dispatch() *sync.WaitGroup {
	var wg sync.WaitGroup
	if !s.enabled() || !s.anyBackend() {
		return &wg
	}
	// Only this goroutine takes slots, so the count can only grow between
	// here and the claim — a start finishing frees one, never takes one.
	free := cap(s.starting) - len(s.starting)
	if free < 1 {
		return &wg
	}

	perApp := 1
	if settings, err := s.store.GetPlatformSettings(); err == nil {
		perApp = settings.JobConcurrency
	}
	claimed, err := s.store.ClaimJobRuns(s.opts.NodeID, free, perApp)
	if err != nil {
		log.Printf("jobs: claiming runs: %v", err)
		return &wg
	}
	for i := range claimed {
		s.starting <- struct{}{}
		wg.Add(1)
		s.wg.Add(1)
		go func(run *database.JobRun) {
			defer s.wg.Done()
			defer wg.Done()
			s.start(run)
			<-s.starting
			s.wakeDispatch()
		}(&claimed[i])
	}
	return &wg
}

// start places one claimed run on the backend.
func (s *Service) start(run *database.JobRun) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The dispatch, as a span. Its trace id is what reaches the workload as
	// TRACEPARENT (ADR §31 D9), so an operator who has instrumented their own
	// job code gets one connected trace rather than two unrelated ones.
	//
	// It covers dispatch only, and not — as the PRD imagined — the whole run
	// through to the callback. A span held open across an async boundary that
	// routinely outlives the process is a span that silently vanishes on every
	// restart, and the run row is already the durable record of what happened.
	// What the trace adds is the join, and the join needs only the id.
	ctx, span := telemetry.Start(ctx, "job.dispatch",
		attribute.String("toft.app.id", telemetry.AppAttr(run.AppID)),
		attribute.String("toft.job.run.id", run.ID),
		attribute.String("toft.trigger", jobProvenance(run)),
		attribute.String("toft.execution_mode", run.ExecutionMode),
	)
	var dispatchErr error
	defer func() { span.End(dispatchErr) }()

	fail := func(stage, reason string) {
		dispatchErr = errors.New(stage)
		// The platform's failure, not the app's: the run never got as far as
		// executing anything the developer wrote, and the stage says so.
		if stage == "" {
			stage = stagePlatform
		}
		s.finish(run, database.JobRunFailed, nil, stage, reason)
	}

	app, err := s.store.GetApp(run.AppID)
	if err != nil || app == nil {
		fail("", "the app no longer exists")
		return
	}
	// A run queued before the lock and dispatched after it. Cancelled rather
	// than failed: nothing went wrong with it, somebody decided it must not run.
	if app.Locked() {
		dispatchErr = ErrAppLocked
		s.finishLocked(run)
		return
	}

	def, _ := s.store.GetJobDefinition(run.AppID, run.DefinitionID)
	// The limits as they are *now*, not as they were when this process
	// started: the timeout the job declared under the install's default and
	// ceiling, and the memory and CPU an administrator set on this job or for
	// every job — never the app's.
	env := s.opts.Dials.Envelope()
	settings, _ := s.store.GetPlatformSettings()
	limits := EffectiveLimits(def, settings, env)
	if limits.Clamped() && def != nil {
		s.reportClamp(def, limits)
	}
	timeout := limits.Timeout()
	memory := limits.MemoryMB.Value

	// The release recorded on the run, not the app's current one: a job must
	// execute the code it was queued against, or a deploy landing mid-queue
	// would silently change what a queued run does.
	ref := run.ReleaseRef
	if ref == "" {
		ref = app.ReleaseRef
	}
	if ref == "" {
		fail(string(runtime.FailureArtifactFetch),
			"this app has no published release; deploy it before running jobs")
		return
	}

	// The app's storage, on its job pods too. Read at dispatch rather than
	// snapshotted at enqueue: a run that started after an administrator revoked
	// a mapping must not still hold the mount.
	mounts, err := s.store.AppMounts(run.AppID)
	if err != nil {
		fail("", fmt.Sprintf("reading this app's storage mappings: %v", err))
		return
	}

	spec := runtime.WorkloadSpec{
		AppID:         app.ID,
		Slug:          app.Slug,
		Name:          run.ID,
		Release:       runtime.ParseReleaseRef(ref),
		BindAddress:   runtime.BindAll,
		Framework:     app.Framework,
		MemoryLimitMB: memory,
		Mounts:        mounts,

		// The request never above the limit: a per-job override set before
		// the install's request was raised would otherwise make a pod the
		// cluster refuses (H-5).
		CPURequestMillicores: min(env.AppCPURequestMillicores, orUnbounded(limits.CPUMillicores.Value)),
		CPULimitMillicores:   limits.CPUMillicores.Value,

		// The cluster-side backstop for the stored deadline: a backend that
		// can bound a run itself does, so a run stays bounded while the
		// platform that enforces the deadline is down.
		Timeout: timeout,
	}

	backend := s.backendFor(run.ExecutionMode)
	if backend == nil {
		fail("", "this install has nowhere to run it: "+run.ExecutionMode+" execution is not available")
		return
	}
	handle, err := backend.Start(ctx, spec)
	if err != nil {
		// The platform's stage, not app_exec: this one never became a
		// workload, and labelling it app_exec would tell the developer their
		// code failed when the cluster refused to schedule it.
		fail(stagePlatform, err.Error())
		return
	}

	// The deadline is stored, not held in memory, so a platform restart does
	// not leave a job running with nothing left to enforce its timeout.
	if err := s.store.MarkJobRunStarted(run.ID, s.opts.NodeID, handle, s.now().Add(timeout)); err != nil {
		// The row moved out from under us — almost always a cancel that landed
		// while the backend was starting the workload, or a claim the stale
		// sweep released and another node took. Stop what we just started
		// rather than leaving it running unattached.
		log.Printf("jobs: run %s was not claimed when it started (cancelled?): %v", run.ID, err)
		// Owed before it is attempted, so a failed stop is asked again by the
		// sweep. Marked only on a terminal row: a claim another node now holds
		// is that node's to run.
		if merr := s.store.MarkJobRunStopOwed(run.ID, handle); merr != nil {
			log.Printf("jobs: recording the stop owed to orphaned workload %s: %v", handle, merr)
		}
		if cerr := backend.Cancel(ctx, handle); cerr != nil {
			log.Printf("jobs: stopping orphaned workload %s: %v", handle, cerr)
		} else {
			forgetHandle(backend, handle)
		}
		return
	}

	// And a timer as well, because the sweep enforcing the stored deadline
	// runs every thirty seconds: a five-second timeout ended anywhere from
	// seven to thirty-five seconds in. The timer is the precise half and the
	// sweep the durable one — it still catches a run whose timer died with
	// the process that armed it.
	runID := run.ID
	time.AfterFunc(timeout, func() {
		select {
		case <-s.stop:
			return
		default:
		}
		s.enforceDeadline(runID)
	})
}

// enforceDeadline times one run out if it is still going past its stored
// deadline. Read again rather than trusted from the timer: the run may have
// finished, been cancelled, or been timed out by the sweep in the meantime.
func (s *Service) enforceDeadline(runID string) {
	overdue, err := s.store.ListOverdueJobRuns(s.now())
	if err != nil {
		return
	}
	for i := range overdue {
		if overdue[i].ID == runID {
			s.timeOut(&overdue[i])
			return
		}
	}
}

// timeOut records a run as timed out and stops its workload.
func (s *Service) timeOut(run *database.JobRun) {
	// A timeout is not a failure: the fix is a longer limit or less work, not
	// a code change, and collapsing the two sends the developer to debug the
	// wrong thing.
	reason := "timed out"
	if run.StartedAt != nil && run.DeadlineAt != nil {
		// Rounded, not truncated: the deadline is stamped from the service's
		// clock a moment before the database stamps started_at, so the
		// difference is a few milliseconds short of the limit and a 10s
		// timeout read "timed out after 9s".
		reason = "timed out after " + HumanSeconds(int(math.Round(run.DeadlineAt.Sub(*run.StartedAt).Seconds())))
	}
	// Only the caller that moved the row goes on: the timer and the sweep can
	// both reach one run, and recording it twice would count it twice.
	if moved, err := s.store.TimeOutJobRun(run.ID, reason); err != nil || !moved {
		return
	}
	s.recordRunOutcome(run, database.JobRunTimedOut, "", reason)
	// TimeOutJobRun recorded the stop as owed, so one that fails here is asked
	// again by the sweep (settleOwedStops).
	if run.ExternalID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := s.Cancel(ctx, run.ExternalID); err != nil {
			log.Printf("jobs: stopping timed-out run %s: %v", run.ID, err)
		}
		cancel()
	}
}

// --- Reconciliation and the sweep ---

// Reconcile re-derives in-flight state from the backend.
//
// This is what makes the dispatcher a reconciler rather than a launcher. On
// boot the platform believes some runs are in flight and has no idea whether
// they are: the process that started them is gone. Asking the *backend* about
// each stored handle is the only way to find out, and it is what turns a
// restart from "every running job is lost" into a pause.
func (s *Service) Reconcile() {
	if !s.anyBackend() {
		return
	}

	// This node's own claims that never reached the backend: the previous
	// process on this node died between claim and start. Nothing of this
	// process is starting them yet — Reconcile runs before any dispatch — so
	// they go straight back to the queue, as the stale sweep would have put
	// them ten minutes from now, rather than failing with a failure mail.
	// Listed after, so the loop below does not see them.
	if n, err := s.store.ReleaseClaimsOf(s.opts.NodeID); err != nil {
		log.Printf("jobs: returning this node's unstarted claims to the queue: %v", err)
	} else if n > 0 {
		log.Printf("jobs: returned %d claim(s) this node never started to the queue", n)
	}

	active, err := s.store.ListActiveJobRuns()
	if err != nil {
		log.Printf("jobs: listing active runs: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	adopted, finished, lost := 0, 0, 0
	for i := range active {
		run := &active[i]

		if run.ExternalID == "" {
			// Claimed but never started, by another node (this node's own went
			// back to the queue above), and failed: it never reached the
			// backend, so nothing is left running.
			//
			// Only a claim that is provably nobody's start in flight: one
			// older than the claim timeout. Every other row is a live replica
			// inside start() right now — a rolling deploy of a two-replica
			// install restarts one while the other works — and failing it
			// would make that node cancel the workload it had just started. A
			// dead node's fresh claim is the stale sweep's.
			if !s.claimIsOrphaned(run) {
				continue
			}
			const reason = "the dispatcher restarted before this run started"
			if s.finish(run, database.JobRunFailed, nil, stagePlatform, reason) {
				lost++
			}
			continue
		}

		status, err := s.statusOf(ctx, run.ExternalID)
		if errors.Is(err, runtime.ErrJobNotFound) {
			// The backend has no record. Either it completed and its record
			// aged out, or it never existed. Both mean the platform will never
			// learn more, and leaving the row `running` forever is the one
			// outcome that helps nobody.
			const reason = "the workload could not be found after a platform restart"
			if s.finish(run, database.JobRunFailed, nil, stagePlatform, reason) {
				lost++
			}
			continue
		}
		if err != nil {
			// Trouble reaching the backend — the API server, the agent's
			// socket, or a backend not constructed on this boot — says nothing
			// about the run. It stays open for the poll, exactly as pollActive
			// treats the same error (C-3).
			log.Printf("jobs: reconcile: run %s: %v — left running for the poll", run.ID, err)
			adopted++
			continue
		}

		s.notePeak(run.ID, status)
		if !status.State.Terminal() {
			adopted++
			continue
		}

		if s.finish(run, StatusFor(status.State), recordedExit(status), recordedStage(status), status.Message) {
			finished++
		}
		s.stopAfterInfrastructureErr(ctx, run, status)
	}

	if adopted+finished+lost > 0 {
		log.Printf("jobs: reconciled — %d still running, %d completed while we were away, %d lost",
			adopted, finished, lost)
	}
}

// recordedExit is the exit code a finished run is stored with. A run stopped
// for its memory limit was killed by a signal and has none: the host backend
// reports it as 0, which a script reading exit_code took for a clean exit
// beside "out of memory" (drills 2026-10-28 #2). It is stored as no exit, as a
// timed-out run is; a real code — Kubernetes' 137 — is kept.
func recordedExit(status runtime.JobStatus) *int {
	if status.State == runtime.JobResourceExceeded && status.ExitCode == 0 {
		return nil
	}
	// An infrastructure failure never ran the app to an exit; a 0 here is
	// "nothing known", not a clean exit (K8-6).
	if status.State == runtime.JobInfrastructureErr && status.ExitCode == 0 {
		return nil
	}
	exit := status.ExitCode
	return &exit
}

// recordedStage is the failure stage a finished run is stored with: the
// backend's, or — for an infrastructure failure the backend could not place —
// the platform's, since nothing says the app's code was what failed.
func recordedStage(status runtime.JobStatus) string {
	if status.State == runtime.JobInfrastructureErr && status.Stage == runtime.FailureNone {
		return stagePlatform
	}
	return string(status.Stage)
}

// claimIsOrphaned reports whether another node's claimed, unstarted run can
// belong to no start in flight: claimed longer ago than any start is allowed
// to take.
func (s *Service) claimIsOrphaned(run *database.JobRun) bool {
	return run.ClaimedAt == nil || s.now().Sub(*run.ClaimedAt) > claimTimeout
}

// tickSweep polls for completions, enforces deadlines, recovers stranded
// claims, and trims history.
func (s *Service) tickSweep() {
	sweepCtx, span := telemetry.Start(context.Background(), "jobs.sweep")
	sweepStart := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(sweepCtx, telemetry.SweepJobs, outcome, time.Since(sweepStart))
	}()

	now := s.now()

	// Completion detection is polling-based. The runner reports its own result
	// over the callback channel and that is the fast path — but a workload
	// that dies without reporting (OOM-killed, evicted, image would not start)
	// never sends anything, and without this poll it would sit at `running`
	// until its deadline. That is not a rare corner: a container that fails to
	// start is exactly the case an operator most needs told about quickly.
	s.pollActive()

	s.checkOverdue(now)

	// Timeouts. Enforced against the *stored* deadline, so a platform restart
	// does not reset every running job's clock.
	if overdue, err := s.store.ListOverdueJobRuns(now); err == nil {
		for i := range overdue {
			s.timeOut(&overdue[i])
		}
	}

	// Stops that were owed and did not land: a terminal row is skipped by every
	// control above, so this is the only thing that asks again.
	s.settleOwedStops()

	// Claims held by a node that never started them. Without this they are
	// stranded forever: no longer pending, so no dispatcher looks at them.
	if n, err := s.store.ReleaseStaleClaims(now.Add(-claimTimeout)); err == nil && n > 0 {
		log.Printf("jobs: returned %d stranded claim(s) to the queue", n)
	}

	// Read per sweep, not per process. An administrator who shortens retention
	// during a disk scare should see it applied on the next pass, and a sweep
	// is thirty seconds apart — this is not a hot path.
	logRetention, rowRetention := fallbackLogRetention, fallbackRowRetention
	if ps, err := s.store.GetPlatformSettings(); err == nil {
		logRetention, rowRetention = ps.JobLogRetention(), ps.JobRunRetention()
	} else {
		log.Printf("jobs: reading retention settings, using defaults: %v", err)
	}
	// The row horizon is the span's cutoff, for the reason cron's is: rows
	// outlive logs, so it is the older of the two clocks.
	cutoff := now.Add(-rowRetention)
	span.Set(telemetry.AttrSweepCutoff.String(cutoff.Format(time.RFC3339)))

	expired, err := s.store.ExpireJobRuns(now.Add(-logRetention), cutoff)
	if err != nil {
		log.Printf("jobs: expiring history: %v", err)
	} else {
		outcome = telemetry.OutcomeSucceeded
	}
	objects := s.dropOutputs(expired.Outputs)
	telemetry.RecordSweepRemoved(sweepCtx, span, telemetry.SweepJobs, telemetry.Removed{
		Rows:     expired.Rows,
		LogLines: expired.LogLines,
		Objects:  int64(objects),
	})

	// Said out loud, like every other sweep in the system. An install with no
	// collector configured has this line and nothing else, and "where did the
	// run go" is a question somebody asks about a row that has silently
	// stopped existing.
	if expired.Rows > 0 || expired.LogLines > 0 || objects > 0 {
		log.Printf("jobs: expired %d run(s), %d log line(s), %d output object(s)",
			expired.Rows, expired.LogLines, objects)
	}
}

// dropOutputs removes the objects belonging to rows the sweep just deleted, and
// returns how many actually went.
//
// After the row rather than before: an object deleted first, for a row whose
// DELETE then failed, would leave a run that still reads `succeeded` with
// nothing behind it — the one failure shape output lifecycle exists to prevent.
// The other order leaves an orphaned object at worst, which is invisible and
// which a store lifecycle rule collects anyway.
//
// The count is of deletions that succeeded, not of objects attempted: a store
// that refused is already a line of its own above, and reporting the attempt
// would have the sweep claim to have reclaimed storage it did not.
func (s *Service) dropOutputs(expired []database.ExpiredOutput) int {
	if s.outputs == nil || len(expired) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dropped := 0
	for _, e := range expired {
		if err := s.outputs.Delete(ctx, e.OutputKey); err != nil {
			log.Printf("jobs: removing the expired output of %s: %v", e.RunID, err)
			continue
		}
		dropped++
	}
	return dropped
}

// pollShared is the completion poll for runs on this machine, at the
// dispatcher's cadence rather than the sweep's. A run the kernel stops at its
// memory limit takes its runner with it (memory.oom.group), so nothing reports
// over the callback, and at the sweep's thirty seconds the out-of-memory answer
// landed some twenty-five seconds after the kill. Asking the agent is a local
// call, so the shared half can afford the shorter wait; a pod's status is a
// cluster API call and stays on the sweep, which also keeps the locked-app stop
// and the peak of a run still going.
func (s *Service) pollShared() {
	if s.opts.Host == nil {
		return
	}
	active, err := s.store.ListActiveJobRuns()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchInterval)
	defer cancel()
	for i := range active {
		run := &active[i]
		if !runtime.IsHostHandle(run.ExternalID) {
			continue
		}
		status, err := s.statusOf(ctx, run.ExternalID)
		if err != nil || !status.State.Terminal() {
			continue
		}
		s.notePeak(run.ID, status)
		s.finish(run, StatusFor(status.State), recordedExit(status), recordedStage(status), status.Message)
	}
}

// pollActive asks the backend about every run the platform believes is in
// flight, and records any that have finished.
//
// This shares its logic with Reconcile deliberately: "ask the backend what
// actually happened" is one operation, and having a boot version and a steady
// version that could disagree is how a run ends up in a state only one of them
// produces.
func (s *Service) pollActive() {
	if !s.anyBackend() {
		return
	}
	active, err := s.store.ListActiveJobRuns()
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Locking stops the workload, and for an app's background work the runs
	// in flight are the workload. Here rather than in the lock handler because
	// an app is locked from two places — an administrator, and the dependency
	// sweep — and the sweep is the one place both are seen.
	locked := map[string]bool{}
	for i := range active {
		run := &active[i]
		isLocked, seen := locked[run.AppID]
		if !seen {
			app, err := s.store.GetApp(run.AppID)
			isLocked = err == nil && app.Locked()
			locked[run.AppID] = isLocked
		}
		if isLocked {
			s.finishLocked(run)
			if run.ExternalID != "" {
				s.stopOwed(ctx, run.ID, run.ExternalID, "of a locked app")
			}
			continue
		}
		if run.ExternalID == "" {
			// Claimed but not yet started. The stale-claim sweep below owns
			// this case; giving up on it here would race a dispatcher that is
			// still starting the workload.
			continue
		}
		status, err := s.statusOf(ctx, run.ExternalID)
		if err != nil {
			continue
		}
		s.notePeak(run.ID, status)
		if !status.State.Terminal() {
			continue
		}
		s.finish(run, StatusFor(status.State), recordedExit(status), recordedStage(status), status.Message)
		s.stopAfterInfrastructureErr(ctx, run, status)
	}
}

// stopAfterInfrastructureErr stops the workload of a run the backend reported
// as an infrastructure failure. Every other terminal state is a workload that
// ended; this one is the backend saying the run cannot go on — a container
// that could not be created, an image name that will never resolve, a Job
// whose pod is gone — and the Job behind it is still in the cluster, pinning
// its claims and free to start late with the app's environment (K8-4).
func (s *Service) stopAfterInfrastructureErr(ctx context.Context, run *database.JobRun, status runtime.JobStatus) {
	if status.State != runtime.JobInfrastructureErr || run.ExternalID == "" {
		return
	}
	s.stopOwed(ctx, run.ID, run.ExternalID, "after an infrastructure failure")
}

// stopOwed records that a terminal run's workload is owed a stop and makes the
// first attempt. The record is what makes the stop durable: a Cancel that fails
// once was only logged, every later control skips a terminal row, and the
// workload ran on with the app's environment and mounts. The sweep asks again
// until the backend says the workload is gone (settleOwedStops).
func (s *Service) stopOwed(ctx context.Context, runID, handle, why string) {
	if err := s.store.MarkJobRunStopOwed(runID, handle); err != nil {
		log.Printf("jobs: recording the stop owed to run %s: %v", runID, err)
	}
	if err := s.Cancel(ctx, handle); err != nil {
		log.Printf("jobs: stopping run %s %s: %v", runID, why, err)
	}
}

// stopOwedBatch bounds one sweep's re-asks; stopOwedGiveUp is when a stop no
// backend on this install can answer for is dropped, so it does not hold the
// head of the list for ever.
const (
	stopOwedBatch  = 100
	stopOwedGiveUp = 24 * time.Hour
)

// settleOwedStops asks the backend about every terminal run still owed a stop:
// gone (terminal, or not found) clears the record, anything else is stopped
// again. A Status error says nothing about the workload and is asked next
// sweep.
func (s *Service) settleOwedStops() {
	owed, err := s.store.ListStopOwedJobRuns(stopOwedBatch)
	if err != nil {
		log.Printf("jobs: listing owed stops: %v", err)
		return
	}
	if len(owed) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, r := range owed {
		b := s.backendForHandle(r.ExternalID)
		if b == nil {
			if s.now().Sub(r.OwedSince) > stopOwedGiveUp {
				log.Printf("jobs: run %s is owed a stop no backend on this install can make; giving up on it", r.ID)
				_ = s.store.ClearStopOwed(r.ID)
			}
			continue
		}
		st, err := b.Status(ctx, r.ExternalID)
		if errors.Is(err, runtime.ErrJobNotFound) || (err == nil && st.State.Terminal()) {
			if err := s.store.ClearStopOwed(r.ID); err != nil {
				log.Printf("jobs: clearing the owed stop of run %s: %v", r.ID, err)
			}
			continue
		}
		if err != nil {
			continue
		}
		log.Printf("jobs: run %s ended but its workload is still running; stopping it again", r.ID)
		if err := s.Cancel(ctx, r.ExternalID); err != nil {
			log.Printf("jobs: stopping run %s again: %v", r.ID, err)
		}
	}
}

// checkOverdue raises job_run_overdue for each job that has gone its
// expect_within without a success (ADR §105). The query leaves out any job that
// already has one open, so a stuck job is one event and one mail under the
// cooldown, not a row per sweep; its next success resolves it.
func (s *Service) checkOverdue(now time.Time) {
	if s.opts.Events == nil {
		return
	}
	overdue, err := s.store.ListOverdueJobs(now)
	if err != nil {
		log.Printf("jobs: listing overdue jobs: %v", err)
		return
	}
	for _, o := range overdue {
		window := time.Duration(o.ExpectWithinSec) * time.Second
		s.opts.Events.Record(events.Event{
			AppID:    o.AppID,
			Kind:     events.KindJobRunOverdue,
			Severity: database.EventSeverityWarning,
			Source:   database.EventSourceJobs,
			Summary:  fmt.Sprintf("The job %q has not succeeded in %s", o.Name, humanWindow(window)),
			Detail: fmt.Sprintf("It was expected to succeed at least every %s, and has not since %s. "+
				"Check whether it is running, failing or being refused, on the app's Automation tab.",
				humanWindow(window), o.Since.UTC().Format(time.RFC3339)),
			Fingerprint: o.ID + ":overdue",
		})
	}
}

// humanWindow writes an expect_within the way it was most likely written:
// whole days as days, otherwise hours.
func humanWindow(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	}
	h := int(d / time.Hour)
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}

// skipReason is the refusal in the words an owner reads. A full queue is the
// case that happens, and its error carries the count.
func skipReason(err error) string {
	if errors.Is(err, database.ErrQueueLimit) {
		return "the app's queue was full (" +
			strings.TrimPrefix(err.Error(), database.ErrQueueLimit.Error()+": ") + ")"
	}
	return err.Error()
}

// stagePlatform is the failure stage of a run the platform could not start or
// lost track of: nothing the developer wrote ran, or the platform cannot say
// what it did.
const stagePlatform = "platform"

// StatusFor maps a backend's state onto a run status. Two backend states fold:
// an infrastructure error is a failed run whose stage says it was the
// platform's, and a resource kill is out_of_memory — memory being the one
// resource a run is killed for.
func StatusFor(state runtime.JobState) string {
	switch state {
	case runtime.JobSucceeded:
		return database.JobRunSucceeded
	case runtime.JobTimedOut:
		return database.JobRunTimedOut
	case runtime.JobResourceExceeded:
		return database.JobRunOutOfMemory
	case runtime.JobCancelled:
		return database.JobRunCancelled
	case runtime.JobRunning:
		return database.JobRunRunning
	case runtime.JobPending:
		return database.JobRunClaimed
	default:
		return database.JobRunFailed
	}
}

// reportClamp tells an app that an administrator lowered a ceiling under one of
// its declarations. Once per job, on its fingerprint: the run still goes, at the
// lower figure, and the app page and the next toft check say so.
func (s *Service) reportClamp(def *database.JobDefinition, l Limits) {
	s.opts.Events.Record(events.Event{
		AppID:    def.AppID,
		Kind:     events.KindLimitClamped,
		Severity: database.EventSeverityWarning,
		Source:   database.EventSourceJobs,
		Summary: fmt.Sprintf("The job %q runs with timeout %s (declared %s): an administrator lowered this install's ceiling",
			def.Name, HumanSeconds(l.TimeoutSec.Value), HumanSeconds(l.TimeoutSec.Declared)),
		Detail: fmt.Sprintf("Lower the value in [jobs.%s] in toft.toml to match, or ask an administrator "+
			"to raise %s.", def.Name, database.SettingMaxJobTimeout),
		Fingerprint: def.ID + ":clamped",
	})
}

// anyBackend reports whether this service can place a run anywhere at all.
func (s *Service) anyBackend() bool { return s.backend != nil || s.opts.Host != nil }

// HostPossible reports whether runs can be placed on this machine — the API's
// half of the capability, which it cannot see from configuration alone.
func (s *Service) HostPossible() bool { return s != nil && s.opts.Host != nil }

// backendFor is where a run of this execution mode is placed: a pod for an
// isolated run, this machine for a shared one.
func (s *Service) backendFor(mode string) Backend {
	if mode == string(runtime.ModeShared) {
		return s.opts.Host
	}
	return s.backend
}

// backendForHandle is the backend that started the run a handle names. The
// handle says, rather than the row, so a Cancel that has only the handle in hand
// reaches the right one.
func (s *Service) backendForHandle(handle string) Backend {
	if runtime.IsHostHandle(handle) {
		return s.opts.Host
	}
	return s.backend
}

func (s *Service) statusOf(ctx context.Context, handle string) (runtime.JobStatus, error) {
	b := s.backendForHandle(handle)
	if b == nil {
		return runtime.JobStatus{}, errors.New("no backend for this run's handle")
	}
	return b.Status(ctx, handle)
}

// notePeak records the memory peak a backend measured itself — the host
// backend's, for a run with no cgroup the runner could read. Before the run is
// finished, so an out-of-memory run carries the figure that explains it.
func (s *Service) notePeak(runID string, status runtime.JobStatus) {
	if status.PeakMemoryMB <= 0 {
		return
	}
	if err := s.store.NoteJobRunPeakMemory(runID, status.PeakMemoryMB); err != nil {
		log.Printf("jobs: recording the peak of %s: %v", runID, err)
	}
}

// NoteBackendPeak asks the backend that started a run for the peak it measured
// and records it. The runner's own exit report is the usual way a run ends, and
// it closes the row before any poll sees the run again — so without this a run
// shorter than the poll interval, on a machine where the backend's watchdog is
// the only measurement (no cgroup per run), finished with no peak at all.
// Called by that report before the row is finished; the runner reports before
// it exits, so the watchdog still holds the figure.
func (s *Service) NoteBackendPeak(ctx context.Context, runID string) {
	run, err := s.store.GetJobRun(runID)
	if err != nil || run == nil || run.ExternalID == "" {
		return
	}
	status, err := s.statusOf(ctx, run.ExternalID)
	if err != nil {
		return
	}
	s.notePeak(runID, status)
}

// finishLocked closes a run because its app is locked. The first terminal status
// wins, so a run that finished in the same instant keeps its own answer.
func (s *Service) finishLocked(run *database.JobRun) {
	s.finish(run, database.JobRunCancelled, nil, "", "the app was locked")
}

// finish makes a run terminal and, **only when this call is the one that did**,
// records its outcome — the notification, the metric. Every writer of a
// terminal status goes through here or through RecordOutcome after its own
// FinishJobRunOnce, so a run is reported exactly once whichever writer saw it
// end first.
func (s *Service) finish(run *database.JobRun, status string, exit *int, stage, reason string) bool {
	reason = s.ReasonFor(run.ID, status, exit, reason)
	moved, err := s.store.FinishJobRunOnce(run.ID, status, exit, stage, reason)
	if err != nil {
		log.Printf("jobs: finishing run %s as %s: %v", run.ID, status, err)
		return false
	}
	if moved {
		s.recordRunOutcome(run, status, stage, reason)
	}
	return moved
}

// ReasonFor is a failed run's reason in words: the one given, or else the last
// line the run wrote to stderr, or else its exit code. "exit 3: connection
// refused" is what a developer wants to read in a list of runs; an empty
// reason sends them to the log for something the platform already had.
func (s *Service) ReasonFor(runID, status string, exit *int, reason string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	if status == database.JobRunOutOfMemory {
		// The peak, where the run reported one: "stopped at 1024 MB" says
		// both what happened and what to raise.
		if run, err := s.store.GetJobRun(runID); err == nil && run != nil && run.PeakMemoryMB != nil {
			return fmt.Sprintf("stopped at %d MB", *run.PeakMemoryMB)
		}
		return "ran out of memory"
	}
	if status != database.JobRunFailed {
		return reason
	}
	lines, _ := s.store.LastStderrLines(runID, reasonLookback)
	line := reasonLine(lines)
	if len(line) > 300 {
		// On a rune boundary: a byte cut through a multi-byte character
		// made a reason Postgres refused, and the run sat `running` until its
		// deadline (security review 2026-11-15, LB-6).
		line = textutil.Clip(line, 300) + "…"
	}
	switch {
	case exit != nil && line != "":
		return fmt.Sprintf("exit %d: %s", *exit, line)
	case exit != nil:
		return fmt.Sprintf("exit %d", *exit)
	}
	return line
}

// reasonLookback is how far back reasonLine may look: a Node stack trace with
// an error object's properties runs to a few dozen lines.
const reasonLookback = 60

// nodeBanner is the line Node prints last on an uncaught exception or
// rejection, after the stack: its own version, which said nothing about the
// failure — "exit 1: Node.js v24.22.0" (drills on trytoft, 2026-10-23).
var nodeBanner = regexp.MustCompile(`^Node\.js v\d+\.\d+`)

// errorLine is how a thrown error names itself: "Error: …", "TypeError: …",
// "ToftError [no_such_job]: …". The prefix is optional — a bare `Error:` is
// the commonest of them, and the first spelling of this missed it.
var errorLine = regexp.MustCompile(`^(?:[A-Za-z_$][\w$.]*)?(?:Error|Exception)\b[^:]*:\s`)

// reasonLine is the stderr line a failed run's reason quotes: the last one,
// unless the last is Node's banner, in which case the last line that names an
// error — the banner and the stack's tail (a bare "}") say nothing.
func reasonLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	last := lines[len(lines)-1]
	if !nodeBanner.MatchString(last) {
		return last
	}
	for i := len(lines) - 2; i >= 0; i-- {
		if errorLine.MatchString(lines[i]) {
			return lines[i]
		}
	}
	return last
}

// RecordOutcome is recordRunOutcome for a run some other writer made terminal
// — the runner's own report, which the API receives. The caller must be the
// one whose FinishJobRunOnce moved the row.
func (s *Service) RecordOutcome(run *database.JobRun, status, stage, reason string) {
	s.recordRunOutcome(run, status, stage, reason)
}

// Cancel stops a run at the backend. The database row is already cancelled by
// the caller; this is the half that reaches the workload.
func (s *Service) Cancel(ctx context.Context, externalID string) error {
	if externalID == "" {
		return nil
	}
	b := s.backendForHandle(externalID)
	if b == nil {
		return nil
	}
	if err := b.Cancel(ctx, externalID); err != nil {
		return err
	}
	// Every caller has already made the row terminal — a person's cancel, a
	// timeout, a lock — so nothing polls this run again, and the host
	// backend's agent keeps a run until its end is read. Read it once the
	// stop lands, or a runner that never reports (SIGKILLed after the grace)
	// leaves its agent entry and its release copy behind for good.
	forgetHandle(b, externalID)
	return nil
}

// forgetHandle asks a backend that keeps a run until its end is read to read
// it; a backend that keeps nothing has no Forget.
func forgetHandle(b Backend, handle string) {
	if f, ok := b.(interface{ Forget(handle string) }); ok {
		f.Forget(handle)
	}
}

// Forget lets the backend drop a run its own report has finished. The report
// arrives before the runner exits, and a backend that is otherwise asked about
// a run only while its row is open — the host one, whose agent keeps a run
// until the platform reads its end — would keep every run that reported for
// itself.
//
// **And it makes sure the workload is gone.** Job code holds the run token —
// the runner hands it over on purpose — so it can post its own result and
// keep running: the row is terminal, and every control keys on an open row —
// the deadline, the per-app cap, cancel, the lock sweep, an app's delete —
// so the process ran on unseen with the app's environment and mounts
// (security review 2026-11-10, C-1). A runner that reported exits within
// moments; one still alive reportedExitGrace later is stopped.
func (s *Service) Forget(runID string) {
	run, err := s.store.GetJobRun(runID)
	if err != nil || run == nil || run.ExternalID == "" {
		return
	}
	if b := s.backendForHandle(run.ExternalID); b != nil {
		forgetHandle(b, run.ExternalID)
		handle := run.ExternalID
		go settleReported(b, runID, handle, reportedExitGrace, func() {
			if err := s.store.MarkJobRunStopOwed(runID, handle); err != nil {
				log.Printf("jobs: recording the stop owed to reported run %s: %v", runID, err)
			}
		})
	}
}

// reportedExitGrace is how long a run that has reported may take to exit
// before it is stopped: the runner's own shutdown — flushing its log, the
// SDK's last write — and nothing an app should spend working.
var reportedExitGrace = 60 * time.Second

// settleReported waits up to grace for a reported run's workload to end, and
// stops it if it has not. A Status error is not taken as gone — a backend
// that cannot answer has not said the process ended — so it is stopped too;
// stopping one already gone is a no-op at every backend.
//
// owe, when set, records the stop as owed before it is attempted, so the sweep
// asks again if it fails.
func settleReported(b Backend, runID, handle string, grace time.Duration, owe func()) {
	ctx, cancel := context.WithTimeout(context.Background(), grace+30*time.Second)
	defer cancel()
	deadline := time.Now().Add(grace)
	for {
		if st, err := b.Status(ctx, handle); err == nil && st.State.Terminal() {
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	log.Printf("jobs: run %s reported its result and was still running %s later; stopping it", runID, grace)
	if owe != nil {
		owe()
	}
	if err := b.Cancel(ctx, handle); err != nil {
		log.Printf("jobs: stopping run %s after its report: %v", runID, err)
		return
	}
	forgetHandle(b, handle)
}

// EndAppRuns ends every run of an app that is being deleted, and returns once
// each workload is gone: the delete removes the app's files next, and a run
// still writing its log into LOGS_DIR/<app> made that fail; one still running
// outlived its app with nothing left to watch it, since its row goes with the
// app's. The rows are left for that cascade — a run of a deleted app has no
// outcome anybody is told about.
func (s *Service) EndAppRuns(ctx context.Context, appID string) {
	active, err := s.store.ListActiveJobRuns()
	if err != nil {
		log.Printf("jobs: listing the runs of deleted app %s: %v", appID, err)
		return
	}
	for _, run := range active {
		if run.AppID != appID || run.ExternalID == "" {
			continue
		}
		b := s.backendForHandle(run.ExternalID)
		if b == nil {
			continue
		}
		cancel := b.Cancel
		if w, ok := b.(interface {
			End(ctx context.Context, handle string) error
		}); ok {
			cancel = w.End
		}
		if err := cancel(ctx, run.ExternalID); err != nil {
			log.Printf("jobs: ending run %s of deleted app %s: %v", run.ID, appID, err)
		}
	}
}

// --- Capabilities ---

// enabled reports whether the job system should do anything at all. Checked
// per tick rather than at boot because an administrator can turn jobs off, and
// a scheduler that kept firing afterwards would make the switch a lie.
func (s *Service) enabled() bool {
	return s.capabilities().BackgroundJobsAvailable()
}

func (s *Service) capabilities() runtime.Capabilities {
	caps := runtime.Capabilities{IsolatedPossible: s.backend != nil, HostJobsPossible: s.opts.Host != nil}
	settings, err := s.store.GetPlatformSettings()
	if err != nil {
		// Fail closed: a settings read that fails must not start work nobody
		// asked for.
		return runtime.Capabilities{}
	}
	caps.IsolatedEnabled = settings.IsolatedEnabled
	caps.JobsEnabled = settings.JobsEnabled
	return caps
}

// recordRunOutcome reports a finished job run to the app's people.
//
// Every failed, timed-out or out-of-memory run is an app event, whoever
// started it, and the app's owners hear about it the way they hear about any
// other failure — collapsed under the cooldown, so a job failing every minute
// is one message rather than sixty. A success clears it.
//
// The job's own `notify` asks for more, on top: `failures` is one message per
// failed run, `always` one per run of any outcome. Skipped and cancelled runs
// are a policy and a person's decision, and say nothing.
func (s *Service) recordRunOutcome(run *database.JobRun, status, stage, reason string) {
	if run == nil {
		return
	}
	// The metric first, and outside every early return below: an operator's
	// dashboard is not a notification, and every terminal run counts.
	recordRunMetric(run, status)

	if s.opts.Events == nil {
		return
	}
	notify := database.JobNotifyNone
	if def, err := s.store.GetJobDefinition(run.AppID, run.DefinitionID); err == nil && def != nil {
		notify = def.Notify
	}
	name := run.JobName
	if name == "" {
		name = run.DefinitionID
	}

	switch status {
	case database.JobRunSucceeded:
		// A success answers every question this job's events were asking: it
		// failed, it was skipped, it went quiet. Each resolves on its own
		// fingerprint, so another job's open events are untouched.
		for _, kind := range []events.Kind{events.KindJobRunFailed, events.KindJobScheduleSkipped, events.KindJobRunOverdue} {
			s.opts.Events.ClearAutomation(run.AppID, kind, run.DefinitionID, run.JobName)
		}
		if notify == database.JobNotifyAlways {
			// Not a failure, so not the failure switch's (BB-97).
			s.opts.Events.NotifyRequested("app_event", run.AppID,
				fmt.Sprintf("The job %q succeeded", name), runLine(run), database.EventSeverityInfo)
		}
		return
	case database.JobRunCancelled, database.JobRunSkipped:
		return
	}

	summary := fmt.Sprintf("The job %q %s", name, jobOutcomeWord(status))
	if reason != "" {
		summary += ": " + reason
	}
	detail := reason
	if stage != "" {
		detail = "stage: " + stage + "\n" + reason
	}
	if line := runLine(run); line != "" {
		detail = strings.TrimSpace(detail + "\n\n" + line)
	}
	told := s.opts.Events.RecordNotified(events.Event{
		AppID:       run.AppID,
		Kind:        events.KindJobRunFailed,
		Severity:    database.EventSeverityError,
		Source:      database.EventSourceJobs,
		Summary:     summary,
		Detail:      detail,
		Fingerprint: run.DefinitionID + ":" + stage,
	})
	// One failed run is one message. The event above mails the first failure
	// of a streak; `failures` and `always` ask for every run, so the per-run
	// message goes only when the event's did not — the two together sent the
	// same "the job failed" mail twice for one run.
	if !told && (notify == database.JobNotifyFailures || notify == database.JobNotifyAlways) {
		s.opts.Events.Notify("app_event", run.AppID, summary, detail, database.EventSeverityError)
	}
}

// runLine is the one line that identifies a run in a message: its id and the
// command to run it again.
func runLine(run *database.JobRun) string {
	short := run.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("run %s — run it again with: toft job rerun %s", short, short)
}

// recordRunMetric times a finished run.
//
// The duration is from *start* rather than from enqueue: the queue wait is its
// own question with its own gauge, and folding the two would make a run that
// waited an hour behind a busy app's limit look like a run that took an hour.
func recordRunMetric(run *database.JobRun, status string) {
	if run.StartedAt == nil {
		// Never dispatched — an infra error or a cancel while queued. There is
		// no execution to time, and recording a zero would drag the whole
		// distribution towards the smallest bucket.
		return
	}
	end := time.Now().UTC()
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	telemetry.RecordJobRun(context.Background(),
		jobOutcome(status), jobProvenance(run), end.Sub(*run.StartedAt), 0)
}

// jobOutcome maps the run statuses onto the closed metric enum. `timed_out` and
// `infra_error` both read as failed on a dashboard; the distinction between
// them lives in app_events, where a person is the reader.
func jobOutcome(status string) string {
	switch status {
	case database.JobRunSucceeded:
		return telemetry.OutcomeSucceeded
	case database.JobRunCancelled:
		return telemetry.OutcomeCancelled
	default:
		return telemetry.OutcomeFailed
	}
}

// jobProvenance is the three-way distinction ADR §26 insists stays visible: a
// person, a schedule, or the app itself. Never the actor's id — that is a
// person, and a person is not a metric attribute.
func jobProvenance(run *database.JobRun) string {
	switch run.StartedBy {
	case database.JobStartedBySchedule:
		return telemetry.TriggerSchedule
	case database.JobStartedByApp:
		return telemetry.TriggerApp
	default:
		return telemetry.TriggerPerson
	}
}

// jobOutcomeWord says what happened in the words a developer would use, rather
// than the status string. "timed_out" in a subject line reads as a bug in the
// platform's copy, not as a fact about their job.
func jobOutcomeWord(status string) string {
	switch status {
	case database.JobRunTimedOut:
		return "ran out of time"
	case database.JobRunOutOfMemory:
		return "ran out of memory"
	default:
		return "failed"
	}
}

// TickInZone is a schedule's tick in the schedule's own zone — UTC for a
// schedule naming none, or a zone this host cannot load.
func TickInZone(tick time.Time, sched string) time.Time {
	if zone, _ := schedule.Split(sched); zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return tick.In(loc)
		}
	}
	return tick.UTC()
}

// orUnbounded reads a limit of 0 — uncapped — as no bound on the request.
func orUnbounded(limit int) int {
	if limit <= 0 {
		return int(^uint(0) >> 1)
	}
	return limit
}

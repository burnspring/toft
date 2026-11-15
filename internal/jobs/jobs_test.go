package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/events"
	mailer "toft/internal/mail"
	"toft/internal/runtime"
)

// These test the lifecycle's externally observable behaviour, and mostly the
// unhappy half: what happens when a node dies mid-claim, when a workload
// vanishes, when a deadline passes with nobody watching. The happy path is the
// easy part and the part that gets exercised anyway.

// --- Fakes ---

type fakeStore struct {
	mu sync.Mutex

	settings database.PlatformSettings
	apps     map[string]*database.App
	defs     map[string]*database.JobDefinition
	runs     map[string]*database.JobRun
	due      []database.DueJob
	disabled []database.DisabledSchedule
	mounts   map[string][]runtime.Mount

	enqueued  []*database.JobRun
	claimed   []string
	finishes  []finishCall
	nextRunAt map[string]time.Time
	released  int64
	expired   bool
	// expiredOutputs is what the sweep hands back for the artifact store to
	// remove — the other half of "an output object expires with its run row".
	expiredOutputs []database.ExpiredOutput

	enqueueErr error
	startedErr error
	overdue    []database.OverdueJob
	// stderr is what LastStderrLine answers, by run.
	stderr map[string]string
	// owed is stop_owed_at: run id -> handle.
	owed map[string]string
}

type finishCall struct {
	runID, status, stage, reason string
	exitCode                     *int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		settings: database.PlatformSettings{
			IsolatedEnabled: true, JobsEnabled: true, JobConcurrency: 3,
			DefaultJobTimeoutSecs: 3600, MaxJobTimeoutSecs: 86400,
			MaxQueuedRunsPerApp: 50,
		},
		apps:      map[string]*database.App{},
		defs:      map[string]*database.JobDefinition{},
		runs:      map[string]*database.JobRun{},
		nextRunAt: map[string]time.Time{},
		stderr:    map[string]string{},
	}
}

func (f *fakeStore) GetPlatformSettings() (database.PlatformSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings, nil
}

func (f *fakeStore) GetApp(id string) (*database.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apps[id], nil
}

// mounts is what AppMounts answers with; empty in every test but the one that
// checks a job inherits its app's storage.
func (f *fakeStore) AppMounts(appID string) ([]runtime.Mount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mounts[appID], nil
}

func (f *fakeStore) GetJobDefinition(appID, id string) (*database.JobDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.defs[id]
	if d != nil && d.AppID != appID {
		return nil, nil
	}
	return d, nil
}

func (f *fakeStore) GetJobDefinitionByName(appID, name string) (*database.JobDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.defs {
		if d.AppID == appID && d.Name == name {
			return d, nil
		}
	}
	// Tests that only exercise the queue enqueue jobs with no definition row;
	// synthesise one so they are not all forced to seed the shape they do not
	// care about.
	return &database.JobDefinition{ID: "def-" + name, AppID: appID, Name: name,
		Command: "echo " + name, TimeoutSec: 3600, Enabled: true,
		Overlap: database.JobOverlapQueue, Notify: database.JobNotifyNone}, nil
}

// ClaimDueSchedules stands in for the shared claim (database/schedules.go):
// what it hands back is what the real one does — the schedules to fire, and
// separately the ones it disabled because their expression no longer parses.
func (f *fakeStore) ClaimDueSchedules(limit int, now time.Time, next database.NextFunc) ([]database.DueJob, []database.DisabledSchedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	due, disabled := f.due, f.disabled
	f.due, f.disabled = nil, nil
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, disabled, nil
}

func (f *fakeStore) SetNextRunAt(id string, at *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at != nil {
		f.nextRunAt[id] = *at
	}
	return nil
}

// EnqueueScheduledJobRun mirrors the real store: a tick that already has a
// scheduled run is refused; under overlap = "skip" a job with an open run, and
// under a full queue any job, gets a skipped row instead of a queued one — the
// full queue returning its error beside it.
func (f *fakeStore) EnqueueScheduledJobRun(r *database.JobRun, overlap string, maxQueued int) (bool, error) {
	f.mu.Lock()
	if r.ScheduledFor != nil {
		for _, existing := range f.runs {
			if existing.AppID == r.AppID && existing.JobName == r.JobName &&
				existing.StartedBy == database.JobStartedBySchedule &&
				existing.ScheduledFor != nil && existing.ScheduledFor.Equal(*r.ScheduledFor) {
				f.mu.Unlock()
				return false, database.ErrTickAlreadyEnqueued
			}
		}
	}
	if f.enqueueErr != nil {
		defer f.mu.Unlock()
		if errors.Is(f.enqueueErr, database.ErrQueueLimit) {
			if r.ID == "" {
				r.ID = fmt.Sprintf("skipped-%d", len(f.enqueued)+1)
			}
			r.Status = database.JobRunSkipped
			f.runs[r.ID] = r
			f.enqueued = append(f.enqueued, r)
			return true, f.enqueueErr
		}
		return false, f.enqueueErr
	}
	if overlap == database.JobOverlapSkip {
		for _, existing := range f.runs {
			if existing.AppID == r.AppID && existing.JobName == r.JobName && database.JobRunOpen(existing.Status) {
				if r.ID == "" {
					r.ID = fmt.Sprintf("skipped-%d", len(f.enqueued)+1)
				}
				r.Status = database.JobRunSkipped
				f.runs[r.ID] = r
				f.enqueued = append(f.enqueued, r)
				f.mu.Unlock()
				return true, nil
			}
		}
	}
	f.mu.Unlock()
	_, err := f.EnqueueJobRunIdempotent(r, maxQueued, 0)
	return false, err
}

func (f *fakeStore) LastStderrLines(runID string, n int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stderr[runID] == "" {
		return nil, nil
	}
	lines := strings.Split(f.stderr[runID], "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func (f *fakeStore) NoteJobRunPeakMemory(runID string, mb int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.runs[runID]; r != nil && (r.PeakMemoryMB == nil || *r.PeakMemoryMB < mb) {
		r.PeakMemoryMB = &mb
	}
	return nil
}

func (f *fakeStore) GetJobRun(runID string) (*database.JobRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.runs[runID]; r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}

// EnqueueJobRunIdempotent mirrors the real store's contract: a non-nil first
// return means an existing run matched and nothing was inserted. The window is
// not modelled — the fake keeps every run forever, so "in flight or recently
// finished" collapses to "we have seen this key", which is enough to exercise
// the service's branch. The windowing itself is tested against real Postgres.
func (f *fakeStore) EnqueueJobRunIdempotent(r *database.JobRun, perAppLimit int, window time.Duration) (*database.JobRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	if r.IdempotencyKey != "" {
		for _, existing := range f.enqueued {
			if existing.AppID == r.AppID && existing.IdempotencyKey == r.IdempotencyKey {
				return existing, nil
			}
		}
	}
	if r.ID == "" {
		r.ID = "run-" + r.JobName + "-" + time.Now().Format("150405.000000000")
	}
	r.Status = database.JobRunQueued
	f.runs[r.ID] = r
	f.enqueued = append(f.enqueued, r)
	return nil, nil
}

func (f *fakeStore) ClaimJobRuns(nodeID string, limit, perAppRunning int) ([]database.JobRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	busy := map[string]int{}
	for _, r := range f.runs {
		if r.Status == database.JobRunClaimed || r.Status == database.JobRunRunning {
			busy[r.AppID]++
		}
	}
	var out []database.JobRun
	for _, r := range f.runs {
		if r.Status == database.JobRunQueued && len(out) < limit && busy[r.AppID] < perAppRunning {
			busy[r.AppID]++
			r.Status = database.JobRunClaimed
			r.ClaimedBy = nodeID
			f.claimed = append(f.claimed, r.ID)
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkJobRunStarted(runID, nodeID, externalID string, deadline time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startedErr != nil {
		return f.startedErr
	}
	r := f.runs[runID]
	if r == nil || r.Status != database.JobRunClaimed || (r.ClaimedBy != "" && r.ClaimedBy != nodeID) {
		return errors.New("not claimed")
	}
	r.Status = database.JobRunRunning
	r.ExternalID = externalID
	r.DeadlineAt = &deadline
	return nil
}

func (f *fakeStore) FinishJobRunOnce(runID, status string, exitCode *int, stage, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[runID]
	if r != nil {
		if database.JobRunTerminal(r.Status) {
			return false, nil // first terminal answer wins
		}
		r.Status = status
	}
	f.finishes = append(f.finishes, finishCall{runID, status, stage, reason, exitCode})
	return true, nil
}

func (f *fakeStore) TimeOutJobRun(runID, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[runID]
	if r == nil || (r.Status != database.JobRunClaimed && r.Status != database.JobRunRunning) {
		return false, nil
	}
	r.Status = database.JobRunTimedOut
	f.finishes = append(f.finishes, finishCall{runID: runID, status: database.JobRunTimedOut, reason: reason})
	if r.ExternalID != "" {
		if f.owed == nil {
			f.owed = map[string]string{}
		}
		f.owed[runID] = r.ExternalID
	}
	return true, nil
}

// owed is the fake's stop_owed_at: run id -> handle, while a stop is owed.
func (f *fakeStore) MarkJobRunStopOwed(runID, handle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[runID]
	if r == nil || !database.JobRunTerminal(r.Status) {
		return nil
	}
	if r.ExternalID == "" {
		r.ExternalID = handle
	}
	if f.owed == nil {
		f.owed = map[string]string{}
	}
	f.owed[runID] = r.ExternalID
	return nil
}

func (f *fakeStore) ListStopOwedJobRuns(limit int) ([]database.StopOwedRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []database.StopOwedRun
	for id, h := range f.owed {
		out = append(out, database.StopOwedRun{ID: id, ExternalID: h, OwedSince: time.Now()})
	}
	return out, nil
}

func (f *fakeStore) ClearStopOwed(runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.owed, runID)
	return nil
}

func (f *fakeStore) isOwed(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.owed[runID]
	return ok
}

func (f *fakeStore) ListActiveJobRuns() ([]database.JobRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []database.JobRun
	for _, r := range f.runs {
		if r.Status == database.JobRunClaimed || r.Status == database.JobRunRunning {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) ListOverdueJobRuns(now time.Time) ([]database.JobRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []database.JobRun
	for _, r := range f.runs {
		if r.DeadlineAt != nil && r.DeadlineAt.Before(now) && !database.JobRunTerminal(r.Status) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) ListOverdueJobs(now time.Time) ([]database.OverdueJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overdue, nil
}

func (f *fakeStore) ReleaseStaleClaims(olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released, nil
}

func (f *fakeStore) ReleaseClaimsOf(nodeID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.runs {
		if r.Status == database.JobRunClaimed && r.ClaimedBy == nodeID && r.ExternalID == "" {
			r.Status, r.ClaimedBy, r.ClaimedAt = database.JobRunQueued, "", nil
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) ExpireJobRuns(logsBefore, rowsBefore time.Time) (database.ExpiredRuns, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = true
	return database.ExpiredRuns{
		Rows:    int64(len(f.expiredOutputs)),
		Outputs: f.expiredOutputs,
	}, nil
}

func (f *fakeStore) finishFor(runID string) *finishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.finishes {
		if f.finishes[i].runID == runID {
			return &f.finishes[i]
		}
	}
	return nil
}

type fakeBackend struct {
	mu sync.Mutex

	started   []runtime.WorkloadSpec
	cancelled []string
	forgotten []string
	statuses  map[string]runtime.JobStatus
	startErr  error
	statusErr error
	// cancelFails makes that many Cancels fail before one lands.
	cancelFails int
	// prefix goes before every handle: "host:" makes this the host backend
	// as backendForHandle sees it.
	prefix string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{statuses: map[string]runtime.JobStatus{}}
}

func (b *fakeBackend) Start(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return "", b.startErr
	}
	b.started = append(b.started, spec)
	return b.prefix + "handle-" + spec.Name, nil
}

func (b *fakeBackend) Status(ctx context.Context, handle string) (runtime.JobStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.statusErr != nil {
		return runtime.JobStatus{}, b.statusErr
	}
	st, ok := b.statuses[handle]
	if !ok {
		return runtime.JobStatus{}, fmt.Errorf("no such workload: %w", runtime.ErrJobNotFound)
	}
	return st, nil
}

func (b *fakeBackend) Cancel(ctx context.Context, handle string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, handle)
	if b.cancelFails > 0 {
		b.cancelFails--
		return errors.New("fake: the API server refused the delete")
	}
	// A stop that lands ends the workload, as every real backend's does.
	if st, ok := b.statuses[handle]; ok && !st.State.Terminal() {
		b.statuses[handle] = runtime.JobStatus{State: runtime.JobCancelled}
	}
	return nil
}

func (b *fakeBackend) cancelCount(handle string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, h := range b.cancelled {
		if h == handle {
			n++
		}
	}
	return n
}

// Forget is the host backend's: it reads a run's end so the agent drops it.
func (b *fakeBackend) Forget(handle string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forgotten = append(b.forgotten, handle)
}

func (b *fakeBackend) wasForgotten(handle string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, h := range b.forgotten {
		if h == handle {
			return true
		}
	}
	return false
}

func (b *fakeBackend) wasCancelled(handle string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, h := range b.cancelled {
		if h == handle {
			return true
		}
	}
	return false
}

func newService(t *testing.T) (*Service, *fakeStore, *fakeBackend) {
	t.Helper()
	store, backend := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{
		ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated),
		ReleaseRef:    "artifact:releases/app-1/dep-1.tar.gz",
	}
	svc := New(store, backend, Options{NodeID: "node-a", Dials: envelope(512)})
	return svc, store, backend
}

// --- Enqueue gating ---

// A shared-mode app may enqueue, and the run it produces is recorded as the
// isolated workload it will actually be.
//
// The mode on the *run* is what cost reporting classifies by, and what happens
// is a pod either way — only the app's own service is placed differently.
// Copying a shared app's mode onto the run would file that pod's CPU seconds
// under shared density and quietly make the two halves of a mixed install
// incomparable.
func TestASharedModeAppMayStartJobsAndTheRunIsIsolated(t *testing.T) {
	svc, store, _ := newService(t)
	store.apps["app-1"].ExecutionMode = string(runtime.ModeShared)

	run, err := svc.Enqueue("app-1", "report", database.JobStartedByApp, "", nil)
	if err != nil {
		t.Fatalf("a shared-mode app must be able to start a job: %v", err)
	}
	if got := runtime.Mode(run.ExecutionMode); got != runtime.ModeIsolated {
		t.Errorf("run execution mode = %q, want %q — the workload is isolated in both modes",
			got, runtime.ModeIsolated)
	}
}

// What survives the removal of the shared-mode refusal is install-level, and it
// still explains itself rather than failing generically: what is wrong is a
// decision somebody has to make, and a bare error does not say there is one.
func TestAnInstallWithJobsOffRefusesWithTheExplanation(t *testing.T) {
	svc, store, _ := newService(t)
	store.settings.JobsEnabled = false

	_, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err == nil {
		t.Fatal("an install with jobs switched off must refuse")
	}
	if !errors.Is(err, ErrJobsUnavailable) {
		t.Errorf("err = %v, want ErrJobsUnavailable so the API can answer specifically", err)
	}
	for _, want := range []string{"turned off", "Settings"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
}

// The dedup contract, at the service boundary: a repeated start with the same
// key returns the run that already exists and says so, rather than queueing a
// second one or failing.
func TestARepeatedStartReturnsTheRunAlreadyInFlight(t *testing.T) {
	svc, _, _ := newService(t)

	first, dup, err := svc.EnqueueIdempotent("app-1", "report", database.JobStartedByApp, "alice", nil, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("the first start of a key is not a duplicate")
	}

	second, dup, err := svc.EnqueueIdempotent("app-1", "report", database.JobStartedByApp, "alice", nil, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("a repeated start must be reported as deduplicated, never silently")
	}
	if second.ID != first.ID {
		t.Errorf("got run %s, want the original %s", second.ID, first.ID)
	}

	// A different key is a different run: dedup is a coalescing rule, not a
	// per-app lock.
	other, dup, err := svc.EnqueueIdempotent("app-1", "report", database.JobStartedByApp, "bob", nil, "k2")
	if err != nil {
		t.Fatal(err)
	}
	if dup || other.ID == first.ID {
		t.Error("a different key must produce a different run")
	}
}

// Turning jobs off must actually stop them, including for schedules that were
// already due — a scheduler that kept firing would make the switch a lie.
func TestSchedulesDoNotFireWhenJobsAreDisabled(t *testing.T) {
	svc, store, _ := newService(t)
	store.settings.JobsEnabled = false
	store.due = []database.DueJob{{JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "nightly", Schedule: "0 3 * * *"}}}

	svc.tickSchedules()

	if len(store.enqueued) != 0 {
		t.Errorf("a schedule fired with jobs disabled: %+v", store.enqueued)
	}
}

// The run records the mode and release it was queued against, so a deploy or a
// promotion landing mid-queue does not silently change what a queued run does.
func TestARunPinsItsModeAndRelease(t *testing.T) {
	svc, store, _ := newService(t)

	run, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExecutionMode != string(runtime.ModeIsolated) {
		t.Errorf("execution mode = %q", run.ExecutionMode)
	}
	if run.ReleaseRef != "artifact:releases/app-1/dep-1.tar.gz" {
		t.Errorf("release ref = %q", run.ReleaseRef)
	}

	// The app moves on; the queued run must not.
	store.apps["app-1"].ReleaseRef = "artifact:releases/app-1/dep-2.tar.gz"
	svc.tickDispatch()

	if len(backendOf(svc).started) != 1 {
		t.Fatalf("started %d workloads", len(backendOf(svc).started))
	}
	got := backendOf(svc).started[0].Release
	if got.Value != "releases/app-1/dep-1.tar.gz" {
		t.Errorf("ran against %q — a deploy changed what a queued run executes", got.Value)
	}
}

func backendOf(s *Service) *fakeBackend { return s.backend.(*fakeBackend) }

// --- Dispatch failures ---

// A backend that will not start the work is an infrastructure failure, not the
// app's: the run never reached anything the developer wrote.
func TestABackendThatCannotStartIsNotTheAppsFault(t *testing.T) {
	svc, store, backend := newService(t)
	backend.startErr = errors.New("cluster is full")

	run, _ := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	svc.tickDispatch()

	got := store.finishFor(run.ID)
	if got == nil {
		t.Fatal("the run was left claimed forever")
	}
	if got.status != database.JobRunFailed {
		t.Errorf("status = %q, want infrastructure_error", got.status)
	}
	if !strings.Contains(got.reason, "cluster is full") {
		t.Errorf("the backend's own error should travel with it, got %q", got.reason)
	}
}

// An app with no published release cannot run a job, and the message should
// say what to do rather than fail at artifact fetch inside a container.
func TestARunWithNoReleaseFailsBeforeStarting(t *testing.T) {
	svc, store, backend := newService(t)
	store.apps["app-1"].ReleaseRef = ""

	run, _ := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	svc.tickDispatch()

	if len(backend.started) != 0 {
		t.Error("a workload was started for a run with no release")
	}
	got := store.finishFor(run.ID)
	if got == nil || !strings.Contains(got.reason, "deploy it") {
		t.Errorf("finish = %+v, want an actionable reason", got)
	}
}

// A cancel that lands while the backend is starting the workload must not
// leave it running unattached — the row is terminal and nothing would ever
// look at it again.
func TestAWorkloadStartedIntoACancelIsStopped(t *testing.T) {
	svc, store, backend := newService(t)
	store.startedErr = errors.New("not claimed")

	run, _ := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	svc.tickDispatch()

	if !backend.wasCancelled("handle-" + run.ID) {
		t.Error("the orphaned workload was left running")
	}
}

// --- Reconciliation ---

// The property that makes a restart a pause rather than a loss: in-flight runs
// are re-derived from the backend, not from memory.
func TestReconcileAdoptsRunsThatAreStillRunning(t *testing.T) {
	svc, store, backend := newService(t)

	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", JobName: "report",
		Status: database.JobRunRunning, ExternalID: "handle-run-1",
	}
	backend.statuses["handle-run-1"] = runtime.JobStatus{State: runtime.JobRunning}

	svc.Reconcile()

	if got := store.finishFor("run-1"); got != nil {
		t.Errorf("a still-running job was marked %q on boot", got.status)
	}
	if store.runs["run-1"].Status != database.JobRunRunning {
		t.Errorf("status = %q, want it left alone", store.runs["run-1"].Status)
	}
}

// A run that finished while the platform was down is recorded with its real
// outcome, not guessed at.
func TestReconcileRecordsRunsThatFinishedWhileWeWereAway(t *testing.T) {
	svc, store, backend := newService(t)

	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning, ExternalID: "handle-run-1",
	}
	backend.statuses["handle-run-1"] = runtime.JobStatus{
		State: runtime.JobResourceExceeded, ExitCode: 137,
		Stage: runtime.FailureExec, Message: "exceeded its memory limit",
	}

	svc.Reconcile()

	got := store.finishFor("run-1")
	if got == nil {
		t.Fatal("the completed run was not recorded")
	}
	// The distinction survives reconciliation: an out-of-memory kill is not a
	// crash, and the fix is different.
	if got.status != database.JobRunOutOfMemory {
		t.Errorf("status = %q, want out_of_memory", got.status)
	}
	if got.exitCode == nil || *got.exitCode != 137 {
		t.Errorf("exit code not carried through: %+v", got.exitCode)
	}
}

// A workload the backend has never heard of is not left running forever. The
// platform will never learn more about it, and a permanently `running` row
// helps nobody.
func TestReconcileGivesUpOnAWorkloadTheBackendLost(t *testing.T) {
	svc, store, _ := newService(t)

	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning, ExternalID: "handle-gone",
	}

	svc.Reconcile()

	got := store.finishFor("run-1")
	if got == nil {
		t.Fatal("a lost workload was left running forever")
	}
	if got.status != database.JobRunFailed {
		t.Errorf("status = %q, want infrastructure_error", got.status)
	}
}

// A backend that cannot be reached at boot says nothing about the run: it is
// left open for the poll, not recorded as lost while it runs on (code review
// 2026-11-11, C-3).
func TestReconcileLeavesARunAloneWhenTheBackendIsUnreachable(t *testing.T) {
	svc, store, backend := newService(t)
	backend.statusErr = errors.New("dial unix /run/toft/agent.sock: connection refused")
	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning, ExternalID: "handle-1",
	}

	svc.Reconcile()

	if got := store.finishFor("run-1"); got != nil {
		t.Fatalf("a run was finished as %q while its backend was merely unreachable", got.status)
	}
	if len(backend.cancelled) != 0 {
		t.Error("the workload was cancelled")
	}
}

// Claimed but never started: the node died in the window between. Safe to fail
// outright precisely because it never reached the backend.
func TestReconcileFailsRunsThatNeverReachedTheBackend(t *testing.T) {
	svc, store, backend := newService(t)

	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunClaimed, ExternalID: "",
	}

	svc.Reconcile()

	if len(backend.cancelled) != 0 {
		t.Error("the backend was asked about a workload that was never started")
	}
	got := store.finishFor("run-1")
	if got == nil || got.status != database.JobRunFailed {
		t.Errorf("finish = %+v, want an infrastructure failure", got)
	}
}

// Boot reconcile on one replica leaves alone a fresh claim another replica is
// starting right now, returns this node's own unstarted claim to the queue
// (RT-7), and still fails a claim older than any start may take (JOBS-4).
func TestReconcileLeavesAnotherNodesFreshClaimAlone(t *testing.T) {
	svc, store, _ := newService(t)
	fresh := time.Now().UTC().Add(-time.Minute)
	old := time.Now().UTC().Add(-claimTimeout - time.Minute)
	store.runs["theirs"] = &database.JobRun{ID: "theirs", AppID: "app-1",
		Status: database.JobRunClaimed, ClaimedBy: "node-b", ClaimedAt: &fresh}
	store.runs["mine"] = &database.JobRun{ID: "mine", AppID: "app-1",
		Status: database.JobRunClaimed, ClaimedBy: "node-a", ClaimedAt: &fresh}
	store.runs["stale"] = &database.JobRun{ID: "stale", AppID: "app-1",
		Status: database.JobRunClaimed, ClaimedBy: "node-c", ClaimedAt: &old}

	svc.Reconcile()

	if got := store.finishFor("theirs"); got != nil {
		t.Errorf("another node's fresh claim was finished %q — that node is starting it", got.status)
	}
	if got := store.finishFor("stale"); got == nil || got.status != database.JobRunFailed {
		t.Errorf("stale: finish = %+v, want failed", got)
	}
	if got := store.finishFor("mine"); got != nil {
		t.Errorf("this node's own unstarted claim was finished %q, want it requeued", got.status)
	}
	if got := store.runs["mine"].Status; got != database.JobRunQueued {
		t.Errorf("this node's own unstarted claim is %q, want queued", got)
	}
}

// A starter whose claim was released and taken by another node stops what it
// started rather than writing its handle over the new holder's (DB2-4).
func TestAStartWhoseClaimMovedIsStopped(t *testing.T) {
	svc, store, backend := newService(t)
	store.runs["run-1"] = &database.JobRun{ID: "run-1", AppID: "app-1", JobName: "report",
		Status: database.JobRunClaimed, ClaimedBy: "node-b"}

	svc.start(store.runs["run-1"])

	if store.runs["run-1"].ExternalID != "" {
		t.Errorf("external_id = %q written over node-b's claim", store.runs["run-1"].ExternalID)
	}
	if len(backend.cancelled) != 1 {
		t.Errorf("cancelled %v, want the workload this node started", backend.cancelled)
	}
}

// A run ended from the platform's side — timed out, cancelled, its app locked —
// is terminal before its workload stops, so nothing polls it again: its end is
// read once the stop lands, or the host backend's agent keeps it, and its
// release copy, for good (JOBS-5).
func TestARunEndedByThePlatformIsForgotten(t *testing.T) {
	svc, store, backend := newService(t)
	past := time.Now().UTC().Add(-time.Minute)
	started := past.Add(-time.Minute)
	store.runs["run-1"] = &database.JobRun{ID: "run-1", AppID: "app-1",
		Status: database.JobRunRunning, ExternalID: "handle-run-1",
		StartedAt: &started, DeadlineAt: &past}

	svc.enforceDeadline("run-1")

	if !backend.wasCancelled("handle-run-1") {
		t.Fatal("the timed-out run was not stopped")
	}
	if !backend.wasForgotten("handle-run-1") {
		t.Error("the timed-out run was stopped but its end is never read")
	}
}

// --- The sweep ---

// A timeout is enforced against the *stored* deadline, so a platform restart
// does not reset every running job's clock — and it is reported as a timeout
// rather than a failure, because the fix is different.
func TestTheSweepEnforcesStoredDeadlines(t *testing.T) {
	svc, store, backend := newService(t)

	past := time.Now().UTC().Add(-time.Minute)
	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning,
		ExternalID: "handle-run-1", DeadlineAt: &past,
	}

	svc.tickSweep()

	got := store.finishFor("run-1")
	if got == nil {
		t.Fatal("an overdue run was not timed out")
	}
	if got.status != database.JobRunTimedOut {
		t.Errorf("status = %q, want timed_out — not a plain failure", got.status)
	}
	// And the workload is actually stopped, or it keeps consuming capacity
	// while the platform believes it is finished.
	if !backend.wasCancelled("handle-run-1") {
		t.Error("the timed-out workload was left running")
	}
}

// The reason names the limit the run was given. The deadline is stamped a few
// milliseconds before started_at, so a truncated difference said 9s for 10s.
func TestATimeoutNamesTheLimitNotOneSecondLess(t *testing.T) {
	svc, store, _ := newService(t)

	started := time.Now().UTC().Add(-time.Minute)
	deadline := started.Add(10*time.Second - 3*time.Millisecond)
	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning,
		ExternalID: "handle-run-1", StartedAt: &started, DeadlineAt: &deadline,
	}

	svc.tickSweep()

	if got := store.finishFor("run-1"); got == nil || got.reason != "timed out after 10s" {
		t.Errorf("finish = %+v, want \"timed out after 10s\"", got)
	}
}

// A host run's peak is the backend's own measurement, recorded before the run
// is finished: an out-of-memory run whose runner was killed before it reported
// anything carried a null peak beside "the run used 340 MB".
func TestTheBackendsPeakIsRecordedBeforeTheRunEnds(t *testing.T) {
	svc, store, backend := newService(t)

	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning, ExternalID: "handle-run-1",
	}
	backend.statuses["handle-run-1"] = runtime.JobStatus{State: runtime.JobResourceExceeded,
		Message: "the run used 140 MB, past its limit of 128 MB, and was stopped", PeakMemoryMB: 140}

	svc.tickSweep()

	run, _ := store.GetJobRun("run-1")
	if run.PeakMemoryMB == nil || *run.PeakMemoryMB != 140 {
		t.Errorf("peak = %v, want 140", run.PeakMemoryMB)
	}
	if got := store.finishFor("run-1"); got == nil || got.status != database.JobRunOutOfMemory {
		t.Errorf("finish = %+v, want out_of_memory", got)
	}
}

// A run inside its deadline is untouched.
func TestTheSweepLeavesHealthyRunsAlone(t *testing.T) {
	svc, store, backend := newService(t)

	future := time.Now().UTC().Add(time.Hour)
	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning,
		ExternalID: "handle-run-1", DeadlineAt: &future,
	}

	svc.tickSweep()

	if got := store.finishFor("run-1"); got != nil {
		t.Errorf("a healthy run was marked %q", got.status)
	}
	if len(backend.cancelled) != 0 {
		t.Error("a healthy run's workload was cancelled")
	}
}

func TestTheSweepTrimsHistory(t *testing.T) {
	svc, store, _ := newService(t)
	svc.tickSweep()
	if !store.expired {
		t.Error("retention was not applied")
	}
}

// --- Capabilities ---

func TestJobsNeedABackendAndBothSwitches(t *testing.T) {
	cases := []struct {
		name       string
		hasBackend bool
		isolated   bool
		jobs       bool
		want       bool
	}{
		{"everything on", true, true, true, true},
		{"no backend", false, true, true, false},
		{"isolation off", true, false, true, false},
		{"jobs off", true, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.settings.IsolatedEnabled = tc.isolated
			store.settings.JobsEnabled = tc.jobs

			var backend Backend
			if tc.hasBackend {
				backend = newFakeBackend()
			}
			svc := New(store, backend, Options{Dials: envelope(512)})

			if got := svc.enabled(); got != tc.want {
				t.Errorf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// A run's params are its declaration's: a default fills what was not sent, a
// string from the CLI arrives as the declared type, and a value outside the
// declared choices is refused at the start rather than inside the job.
func TestARunsParamsAreHeldToTheDeclaration(t *testing.T) {
	svc, store, _ := newService(t)
	store.defs["d1"] = &database.JobDefinition{
		ID: "d1", AppID: "app-1", Name: "report", Command: "python report.py",
		ParamsSpec: []byte(`{"region":{"type":"string","default":"emea","choices":["emea","amer"]},` +
			`"days":{"type":"int"}}`),
		Enabled: true,
	}

	run, err := svc.Enqueue("app-1", "report", database.JobStartedBySchedule, "", []byte(`{"days":"7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(run.Params) != `{"days":7,"region":"emea"}` {
		t.Errorf("params = %s, want the default filled and the int typed", run.Params)
	}

	_, err = svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", []byte(`{"region":"apac"}`))
	var pe *ParamError
	if !errors.As(err, &pe) || pe.Code != "param_choice" || pe.Example != "emea" {
		t.Errorf("err = %v, want param_choice with an example to paste", err)
	}
}

// --- The scheduler's own pass (ADR §33, one claim for both kinds) ---

// eventStore is the slice of internal/events' Store the recorder touches on the
// path this test drives: write the row, and decide whether to notify.
type eventStore struct {
	mu       sync.Mutex
	inserted []database.AppEvent
}

func (e *eventStore) InsertAppEvent(ev *database.AppEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inserted = append(e.inserted, *ev)
	return nil
}

func (e *eventStore) ClaimEventNotify(string, string, string, string, time.Duration) (bool, error) {
	// Never notify: this test is about the row, and every occurrence is written
	// whether or not it is emailed.
	return false, nil
}
func (e *eventStore) MarkEventNoticeSent(string) error                       { return nil }
func (e *eventStore) ResolveAppEvents(string, string, string) (int, error)   { return 0, nil }
func (e *eventStore) AppNotifyFailures(string) (bool, error)                 { return true, nil }
func (e *eventStore) ListAppRecipients(string) ([]database.Recipient, error) { return nil, nil }
func (e *eventStore) GetApp(id string) (*database.App, error) {
	return &database.App{ID: id, Name: "Demo"}, nil
}

// A schedule the claim switched off is recorded against its app.
//
// This branch had never once run. The old claim filtered disabled rows out
// *before* returning them, so the `if !def.Enabled` that was supposed to record
// this could not be reached — a job's schedule stopped and nothing anywhere said
// why. Merging the two schedulers is what surfaced it: the shared claim hands
// back what it disabled instead of swallowing it.
func TestADisabledScheduleIsRecordedAgainstItsApp(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)

	store.disabled = []database.DisabledSchedule{
		{ID: "d1", AppID: "app-1", Name: "nightly", Schedule: "not a cron expression"},
	}

	svc.tickSchedules()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 1 {
		t.Fatalf("recorded %d events, want 1", len(sink.inserted))
	}
	got := sink.inserted[0]
	if got.Kind != string(events.KindScheduleDisabled) || got.AppID != "app-1" {
		t.Errorf("event = %+v", got)
	}
	// The row is off, so the message is all somebody has to find it by.
	if !strings.Contains(got.Summary, "nightly") ||
		!strings.Contains(got.Summary, "not a cron expression") {
		t.Errorf("summary does not name the schedule and its expression: %q", got.Summary)
	}
	if !strings.Contains(got.Detail, "toft.toml") {
		t.Errorf("detail should say where to fix it: %q", got.Detail)
	}
	if got.Severity != database.EventSeverityWarning {
		t.Errorf("severity = %q, want warning", got.Severity)
	}
}

// Nothing is disabled on an ordinary pass, and what is due is enqueued.
func TestAnOrdinarySchedulePassEnqueuesAndRecordsNothing(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	store.due = []database.DueJob{
		{JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report", Schedule: "0 3 * * *", Enabled: true}},
	}

	svc.tickSchedules()

	if len(store.enqueued) != 1 || store.enqueued[0].JobName != "report" {
		t.Fatalf("enqueued %+v, want one run of report", store.enqueued)
	}
	// A scheduled firing has no actor: it must never render as a person.
	if store.enqueued[0].StartedBy != database.JobStartedBySchedule || store.enqueued[0].ActorID != "" {
		t.Errorf("run = %+v, want a cron trigger with no actor", store.enqueued[0])
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 0 {
		t.Errorf("recorded %d events on a healthy pass", len(sink.inserted))
	}
}

// --- Nothing re-runs by itself ---

// A failed run is one run, shown once, whoever started it. The retry chain
// produced several unlinked rows for one failure; a failure is final now, and
// running it again is a deliberate act (Rerun).
func TestAFailedRunIsNeverQueuedAgain(t *testing.T) {
	svc, store, _ := newService(t)
	store.defs["d1"] = &database.JobDefinition{
		ID: "d1", AppID: "app-1", Name: "report", Command: "python report.py", Enabled: true,
	}
	for _, by := range []string{database.JobStartedBySchedule, database.JobStartedByApp, database.JobStartedByUI} {
		run := &database.JobRun{ID: "run-" + by, DefinitionID: "d1", AppID: "app-1", JobName: "report",
			StartedBy: by}
		svc.recordRunOutcome(run, database.JobRunFailed, "", "exit 1")
	}
	if len(store.enqueued) != 0 {
		t.Fatalf("enqueued %d runs after failures, want none", len(store.enqueued))
	}
}

// mailingEvents is an event store that lets the first occurrence of each
// fingerprint notify, as the real cooldown does, and has one recipient.
type mailingEvents struct {
	eventStore
	claimed map[string]bool
}

func (e *mailingEvents) ClaimEventNotify(_, _, _, fingerprint string, _ time.Duration) (bool, error) {
	if e.claimed[fingerprint] {
		return false, nil
	}
	e.claimed[fingerprint] = true
	return true, nil
}
func (e *mailingEvents) ListAppRecipients(string) ([]database.Recipient, error) {
	return []database.Recipient{{Address: "owner@example.com", DisplayName: "Owner"}}, nil
}

type countingMailer struct{ sent []mailer.Message }

func (m *countingMailer) SendAsync(msg mailer.Message) { m.sent = append(m.sent, msg) }
func (m *countingMailer) BaseURL() string              { return "https://toft.internal" }

// **One failed run is one message.** The run's event mails the first failure
// of a streak and `notify = "failures"` asks for every run; the two together
// sent the same "the job failed" mail twice for one run (the Migration
// Console's tick, 2026-10-23). The per-run message now goes only when the
// event's did not, so a streak is still one message per run.
func TestAFailedRunWithNotifyFailuresMailsOnce(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &mailingEvents{claimed: map[string]bool{}}
	mail := &countingMailer{}
	svc.opts.Events = events.NewRecorder(sink, mail)
	store.defs["d1"] = &database.JobDefinition{
		ID: "d1", AppID: "app-1", Name: "tick", Command: "tick", Enabled: true,
		Notify: database.JobNotifyFailures,
	}
	for i, want := range []int{1, 2, 3} {
		run := &database.JobRun{ID: fmt.Sprintf("run-%d", i), DefinitionID: "d1", AppID: "app-1",
			JobName: "tick", StartedBy: database.JobStartedBySchedule}
		svc.recordRunOutcome(run, database.JobRunFailed, "", "exit 1")
		if len(mail.sent) != want {
			t.Fatalf("after %d failed runs, %d messages, want %d", i+1, len(mail.sent), want)
		}
	}
}

// A re-run is a new run of the same job with the same params and
// scheduled_for, pointing at the original — and refused while the original is
// still open.
func TestARerunRepeatsTheRunAndPointsAtIt(t *testing.T) {
	svc, store, _ := newService(t)
	tick := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	store.runs["run-1"] = &database.JobRun{ID: "run-1", AppID: "app-1", JobName: "report",
		Status: database.JobRunFailed, Params: []byte(`{"region":"emea"}`), ScheduledFor: &tick}

	run, err := svc.Rerun("app-1", "run-1", database.JobStartedByCLI, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if run.RerunOf != "run-1" || string(run.Params) != `{"region":"emea"}` ||
		run.ScheduledFor == nil || !run.ScheduledFor.Equal(tick) {
		t.Errorf("rerun = %+v", run)
	}

	store.runs["run-2"] = &database.JobRun{ID: "run-2", AppID: "app-1", JobName: "report",
		Status: database.JobRunRunning}
	if _, err := svc.Rerun("app-1", "run-2", database.JobStartedByCLI, "alice"); !errors.Is(err, ErrRunStillOpen) {
		t.Errorf("rerun of a running run: %v, want ErrRunStillOpen", err)
	}
	if _, err := svc.Rerun("app-2", "run-1", database.JobStartedByCLI, "alice"); !errors.Is(err, ErrNoSuchRun) {
		t.Errorf("rerun of another app's run: %v, want ErrNoSuchRun", err)
	}
}

// overlap = "skip": a scheduled firing while the last run is still going is a
// skipped row, never a gap and never a second run.
func TestASkipOverlapRecordsASkippedRun(t *testing.T) {
	svc, store, _ := newService(t)
	store.defs["d1"] = &database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report",
		Command: "x", Enabled: true, Overlap: database.JobOverlapSkip}
	store.runs["open"] = &database.JobRun{ID: "open", AppID: "app-1", JobName: "report",
		Status: database.JobRunRunning}
	store.due = []database.DueJob{{JobDefinition: *store.defs["d1"]}}

	svc.tickSchedules()

	if len(store.enqueued) != 1 || store.enqueued[0].Status != database.JobRunSkipped {
		t.Fatalf("enqueued %+v, want one skipped run", store.enqueued)
	}
}

// A failed run with no reason of its own takes its last stderr line and exit
// code: what a developer wants to read in a list of runs.
func TestAFailedRunsReasonIsItsLastStderrLine(t *testing.T) {
	svc, store, _ := newService(t)
	store.stderr["r1"] = "ConnectionRefusedError: connection refused"
	exit := 3
	if got := svc.ReasonFor("r1", database.JobRunFailed, &exit, ""); got != "exit 3: ConnectionRefusedError: connection refused" {
		t.Errorf("reason = %q", got)
	}
	if got := svc.ReasonFor("r1", database.JobRunFailed, &exit, "the platform said"); got != "the platform said" {
		t.Errorf("a given reason was replaced: %q", got)
	}
}

// LB-6 (security review 2026-11-15): the reason was cut by byte, and a cut
// through a multi-byte character made a reason Postgres refused — the run sat
// `running` until its deadline. 400 bytes of 3-byte characters puts the 300th
// byte mid-rune.
func TestAFailedRunsReasonIsCutOnARuneBoundary(t *testing.T) {
	svc, store, _ := newService(t)
	store.stderr["r1"] = "a" + strings.Repeat("错", 133)
	exit := 1
	got := svc.ReasonFor("r1", database.JobRunFailed, &exit, "")
	if !utf8.ValidString(got) {
		t.Errorf("reason is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a cut reason does not say so: %q", got)
	}
}

// Node ends an uncaught exception with its version banner, and the line before
// that is often the error object's closing brace: the reason is the error.
func TestANodeFailureIsItsErrorNotItsVersion(t *testing.T) {
	svc, store, _ := newService(t)
	store.stderr["r1"] = strings.Join([]string{
		"file:///app/jobs/refresh.mjs:12",
		"throw new ToftError('no such table: apac', { code: 'no_table' });",
		"^",
		"ToftError [no_table]: no such table: apac",
		"    at file:///app/jobs/refresh.mjs:12:7",
		"  code: 'no_table'",
		"}",
		"Node.js v24.22.0",
	}, "\n")
	exit := 1
	if got := svc.ReasonFor("r1", database.JobRunFailed, &exit, ""); got != "exit 1: ToftError [no_table]: no such table: apac" {
		t.Errorf("reason = %q", got)
	}
	// The commonest throw of all, a bare Error — the shape measured on trytoft.
	store.stderr["r3"] = strings.Join([]string{
		"const err = new Error(\"boom: no such table apac\");",
		"            ^",
		"Error: boom: no such table apac",
		"    at file:///release/jobs/boom.js:2:13 {",
		"  code: 'no_table'",
		"}",
		"Node.js v24.22.0",
	}, "\n")
	if got := svc.ReasonFor("r3", database.JobRunFailed, &exit, ""); got != "exit 1: Error: boom: no such table apac" {
		t.Errorf("bare Error: reason = %q", got)
	}
	// Not Node: the last line, as before, even when an earlier one names an error.
	store.stderr["r2"] = "ValueError: x\nTraceback done: region apac missing"
	if got := svc.ReasonFor("r2", database.JobRunFailed, &exit, ""); got != "exit 1: Traceback done: region apac missing" {
		t.Errorf("reason = %q", got)
	}
}

// The developer declares the timeout, the administrator sets its default and
// ceiling, and memory and CPU are the administrator's alone: the job's own
// override, else the install's job defaults — never the app's.
func TestEffectiveLimitsNameTheirSource(t *testing.T) {
	settings := database.PlatformSettings{DefaultJobTimeoutSecs: 3600, MaxJobTimeoutSecs: 900}
	env := config.Envelope{AppMemoryLimitMB: 4096, AppCPULimitMillicores: 1000,
		JobMemoryLimitMB: 1024, JobCPULimitMillicores: 2000}
	l := EffectiveLimits(&database.JobDefinition{TimeoutSec: 2700}, settings, env)
	if l.TimeoutSec.Value != 900 || l.TimeoutSec.Source != SourceClamped || l.TimeoutSec.Declared != 2700 {
		t.Errorf("timeout = %+v, want 900 clamped from 2700", l.TimeoutSec)
	}
	if l.MemoryMB.Value != 1024 || l.MemoryMB.Source != SourceDefault {
		t.Errorf("memory = %+v, want the 1024 job default, not the app's 4096", l.MemoryMB)
	}
	if l.CPUMillicores.Value != 2000 || l.CPUMillicores.Source != SourceDefault {
		t.Errorf("cpu = %+v, want the 2000m job default", l.CPUMillicores)
	}
	settings.MaxJobTimeoutSecs = 86400
	l = EffectiveLimits(&database.JobDefinition{OverrideMemoryMB: 6144, OverrideCPUMillicores: 500}, settings, env)
	if l.TimeoutSec.Value != 3600 || l.TimeoutSec.Source != SourceDefault {
		t.Errorf("timeout = %+v, want the install default", l.TimeoutSec)
	}
	if l.MemoryMB.Value != 6144 || l.MemoryMB.Source != SourceOverride || l.Clamped() {
		t.Errorf("memory = %+v, want the 6144 override", l.MemoryMB)
	}
	if l.CPUMillicores.Value != 500 || l.CPUMillicores.Source != SourceOverride {
		t.Errorf("cpu = %+v, want the 500m override", l.CPUMillicores)
	}

	err := CheckCeilings([]database.JobDefinition{{Name: "nightly", TimeoutSec: 2700}},
		database.PlatformSettings{MaxJobTimeoutSecs: 900})
	var ce *CeilingError
	if !errors.As(err, &ce) || ce.Hint() != `set timeout = "15m" in [jobs.nightly], or ask an admin to raise max_job_timeout_seconds` {
		t.Errorf("err = %v", err)
	}
}

// A job pod is scheduled onto the same nodes the apps are, so it carries the
// same reservation; its CPU limit is the install's job default, never the
// app's, and nothing the definition says.
func TestAJobPodCarriesTheInstallsCPUDials(t *testing.T) {
	store, backend := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{
		ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated),
		ReleaseRef:    "artifact:releases/app-1/dep-1.tar.gz",
	}
	svc := New(store, backend, Options{NodeID: "node-a",
		Dials: config.NewResolver(&config.Config{
			AppMemoryLimitMB:        512,
			AppCPURequestMillicores: 250,
			AppCPULimitMillicores:   750,
			JobCPULimitMillicores:   1500,
		}, nil),
	})

	if _, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil); err != nil {
		t.Fatal(err)
	}
	svc.tickDispatch()

	if len(backend.started) != 1 {
		t.Fatalf("started %d workloads", len(backend.started))
	}
	spec := backend.started[0]
	if spec.CPURequestMillicores != 250 {
		t.Errorf("job CPU request = %d, want 250", spec.CPURequestMillicores)
	}
	if spec.CPULimitMillicores != 1500 {
		t.Errorf("job CPU limit = %d, want the job default 1500", spec.CPULimitMillicores)
	}
}

// envelope is an install whose APP_MEMORY_LIMIT_MB is n and whose settings
// document says nothing — the environment's number, which is what every test
// here that does not exercise the dial itself wants.
func envelope(mb int) *config.Resolver {
	return config.NewResolver(&config.Config{AppMemoryLimitMB: mb}, nil)
}

// --- The install's memory ceiling ---

// A run whose definition names no memory is capped by the number in force
// *now*, which is the administrator's dial when they have set one.
//
// This is the failure nothing on any screen would show. Until the envelope,
// this service held APP_MEMORY_LIMIT_MB in a field set once at construction, so
// an administrator who raised app_memory_limit_mb raised it for every service
// start and for no job run — and a report that needed the new ceiling went on
// being killed at the old one, with the settings page saying the opposite.
func TestAJobRunIsCappedByTheAdministratorsNumberAndNotTheProcessStarts(t *testing.T) {
	store, backend := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{
		ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated),
		ReleaseRef:    "artifact:releases/app-1/dep-1.tar.gz",
	}
	// A definition that names no memory of its own: the install's ceiling is
	// the only thing deciding this run's cap.
	store.defs["def-report"] = &database.JobDefinition{
		ID: "def-report", AppID: "app-1", Name: "report",
		Command: "echo report", TimeoutSec: 3600, Enabled: true,
	}

	dials := &settingsDials{}
	svc := New(store, backend, Options{NodeID: "node-a",
		Dials: config.NewResolver(&config.Config{JobMemoryLimitMB: 1024}, dials)})

	// Said after the service was constructed, which is the whole point: a
	// number resolved at construction could not hear this.
	dials.memoryMB = 4096

	if _, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil); err != nil {
		t.Fatal(err)
	}
	svc.tickDispatch()

	if len(backend.started) != 1 {
		t.Fatalf("started %d workloads, want 1", len(backend.started))
	}
	if got := backend.started[0].MemoryLimitMB; got != 4096 {
		t.Errorf("job run capped at %d MB, want 4096 — the administrator raised the "+
			"ceiling and this run was still held at the number the process started with", got)
	}
}

// The other direction, and the rule that makes an unset row safe: zero in the
// settings document is "the operator's number", never "uncapped".
func TestAnUnsetDialLeavesTheOperatorsCeilingStanding(t *testing.T) {
	store, backend := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{
		ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated),
		ReleaseRef:    "artifact:releases/app-1/dep-1.tar.gz",
	}
	store.defs["def-report"] = &database.JobDefinition{
		ID: "def-report", AppID: "app-1", Name: "report",
		Command: "echo report", TimeoutSec: 3600, Enabled: true,
	}

	svc := New(store, backend, Options{NodeID: "node-a",
		Dials: config.NewResolver(&config.Config{AppMemoryLimitMB: 2048, JobMemoryLimitMB: 512}, &settingsDials{})})

	if _, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil); err != nil {
		t.Fatal(err)
	}
	svc.tickDispatch()

	if got := backend.started[0].MemoryLimitMB; got != 512 {
		t.Errorf("job run capped at %d MB, want the environment's job default 512, not the app's 2048", got)
	}
}

// settingsDials is a settings document an administrator can change under a
// running service, which is what the dial is for.
type settingsDials struct{ memoryMB int }

func (d *settingsDials) Dials() config.DialOverrides {
	return config.DialOverrides{JobMemoryLimitMB: d.memoryMB}
}

// --- Locking (invariants § App locking) ---

func lockApp(store *fakeStore, id string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	at := time.Now().UTC()
	store.apps[id].LockedAt = &at
}

// The hole this closes was silent: the proxy, deploy and the Run button all
// refused a locked app, while its nightly job kept running from the schedule,
// which consulted nothing.
func TestScheduledJobOnLockedAppDoesNotRun(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	lockApp(store, "app-1")
	store.due = []database.DueJob{
		{JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report", Schedule: "0 3 * * *", Enabled: true}},
	}

	svc.tickSchedules()

	if len(store.enqueued) != 0 {
		t.Fatalf("a locked app's schedule enqueued %d runs", len(store.enqueued))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 0 {
		t.Errorf("recorded %d events: the lock is the explanation, not a failure", len(sink.inserted))
	}
}

// A schedule that fires during an app's first build — its jobs are recorded at
// detect, its release only at publish — enqueues nothing and reports nothing.
func TestAScheduleBeforeTheFirstReleaseDoesNotFire(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	store.apps["app-1"].ReleaseRef = ""
	store.due = []database.DueJob{
		{JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report", Schedule: "* * * * *", Enabled: true}},
	}

	svc.tickSchedules()

	if len(store.enqueued) != 0 {
		t.Fatalf("an unpublished app's schedule enqueued %d runs", len(store.enqueued))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 0 {
		t.Errorf("recorded %d events for a firing with nothing to run", len(sink.inserted))
	}
	// A person's start is not this case: it is accepted and dispatch says
	// "deploy it before running jobs".
	if _, _, err := svc.EnqueueIdempotent("app-1", "report", database.JobStartedByUI, "", nil, ""); errors.Is(err, errNotPublished) {
		t.Errorf("a manual start was refused as unpublished: %v", err)
	}
}

// Every start passes through EnqueueIdempotent, so one refusal covers the app's
// own trigger as well as the schedule.
func TestEveryStartOfALockedAppIsRefused(t *testing.T) {
	svc, store, _ := newService(t)
	lockApp(store, "app-1")

	for _, trigger := range []string{database.JobStartedByUI, database.JobStartedByApp, database.JobStartedBySchedule} {
		if _, _, err := svc.EnqueueIdempotent("app-1", "report", trigger, "", nil, ""); !errors.Is(err, ErrAppLocked) {
			t.Errorf("trigger %s: err = %v, want ErrAppLocked", trigger, err)
		}
	}
}

// A run queued before the lock and dispatched after it never reaches the
// backend, and says why.
func TestQueuedRunOnLockedAppIsCancelledAtDispatch(t *testing.T) {
	svc, store, backend := newService(t)
	run, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	lockApp(store, "app-1")

	svc.tickDispatch()

	if len(backend.started) != 0 {
		t.Error("a locked app's queued run was started")
	}
	got := store.finishFor(run.ID)
	if got == nil || got.status != database.JobRunCancelled || !strings.Contains(got.reason, "locked") {
		t.Errorf("finish = %+v, want cancelled because the app was locked", got)
	}
}

// Locking stops the workload, and a run in flight is one. The sweep is where
// it happens, because an app is locked from two places — an administrator and
// the dependency sweep — and only here are both seen.
func TestRunningRunOfLockedAppIsStopped(t *testing.T) {
	svc, store, backend := newService(t)
	run, _ := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	svc.tickDispatch()
	handle := "handle-" + run.ID
	backend.statuses[handle] = runtime.JobStatus{State: runtime.JobRunning}
	lockApp(store, "app-1")

	svc.pollActive()

	if !backend.wasCancelled(handle) {
		t.Error("the running workload of a locked app was left running")
	}
	if got := store.finishFor(run.ID); got == nil || got.status != database.JobRunCancelled {
		t.Errorf("finish = %+v, want cancelled", got)
	}
}

// A scheduled firing the queue refuses is gone — the claim already moved the
// schedule on — and used to leave only a log line. "It did not happen" must be
// recorded, never inferred from a gap in the run history.
func TestAQueueRefusedScheduledFiringIsRecorded(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	store.enqueueErr = fmt.Errorf("%w: %d of %d", database.ErrQueueLimit, 3, 3)
	store.due = []database.DueJob{
		{JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report", Schedule: "0 3 * * *", Enabled: true}},
	}

	svc.tickSchedules()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 1 {
		t.Fatalf("recorded %d events, want one skipped firing", len(sink.inserted))
	}
	got := sink.inserted[0]
	if got.Kind != string(events.KindJobScheduleSkipped) || got.Fingerprint != "d1:skipped" {
		t.Errorf("event = %s/%s, want job_schedule_skipped under the definition", got.Kind, got.Fingerprint)
	}
	if !strings.Contains(got.Summary, `"report"`) || !strings.Contains(got.Summary, "3 of 3") {
		t.Errorf("summary = %q, want the job and the counts", got.Summary)
	}
}

// A queue-refused firing is a skipped row as well as the event: `toft job
// runs` must not read as if the schedule never fired, the way an overlap skip
// never does.
func TestAQueueRefusedScheduledFiringIsASkippedRun(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	store.enqueueErr = &database.QueueLimitError{Queued: 2, Limit: 2}
	tick := time.Date(2026, 10, 28, 12, 38, 0, 0, time.UTC)
	store.due = []database.DueJob{{Tick: tick,
		JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report", Schedule: "* * * * *", Enabled: true}}}

	svc.tickSchedules()

	if len(store.enqueued) != 1 || store.enqueued[0].Status != database.JobRunSkipped ||
		store.enqueued[0].ScheduledFor == nil || !store.enqueued[0].ScheduledFor.Equal(tick) {
		t.Fatalf("enqueued %+v, want one skipped run for the tick", store.enqueued)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 1 || sink.inserted[0].Kind != string(events.KindJobScheduleSkipped) {
		t.Errorf("events = %+v, want the one job_schedule_skipped", sink.inserted)
	}
}

// A tick that already has its run is refused by the store and is nothing more:
// no second run, and no event telling an owner a firing was lost when none was.
func TestATickAlreadyEnqueuedIsNotASecondRunOrAnEvent(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	tick := time.Date(2026, 10, 28, 12, 33, 0, 0, time.UTC)
	due := database.DueJob{Tick: tick,
		JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report", Schedule: "* * * * *", Enabled: true}}

	store.due = []database.DueJob{due}
	svc.tickSchedules()
	store.due = []database.DueJob{due}
	svc.tickSchedules()

	if len(store.enqueued) != 1 {
		t.Fatalf("enqueued %d runs for one tick, want one", len(store.enqueued))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 0 {
		t.Errorf("events = %+v, want none", sink.inserted)
	}
}

// A scheduled run carries the tick it was claimed for, not the moment it was
// enqueued — a run that waits behind its app's queue still knows it is the
// 03:00 run.
func TestAScheduledRunCarriesItsTick(t *testing.T) {
	svc, store, _ := newService(t)
	tick := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	store.due = []database.DueJob{{
		JobDefinition: database.JobDefinition{ID: "d1", AppID: "app-1", Name: "report",
			Schedule: "0 3 * * *", Enabled: true},
		Tick: tick,
	}}
	store.defs["d1"] = &store.due[0].JobDefinition

	svc.tickSchedules()

	if len(store.enqueued) != 1 || store.enqueued[0].ScheduledFor == nil || !store.enqueued[0].ScheduledFor.Equal(tick) {
		t.Fatalf("enqueued %+v, want one run for %s", store.enqueued, tick)
	}
}

// A run nobody gave an instant has none. Inventing now would make a re-run of
// last Tuesday's work silently process today.
func TestAManualRunHasNoScheduledForUnlessGiven(t *testing.T) {
	svc, store, _ := newService(t)
	if _, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC)
	if _, err := svc.EnqueueFor("app-1", "report", database.JobStartedByUI, "alice", nil, &at); err != nil {
		t.Fatal(err)
	}
	if store.enqueued[0].ScheduledFor != nil {
		t.Errorf("a plain manual run carries %v", store.enqueued[0].ScheduledFor)
	}
	if store.enqueued[1].ScheduledFor == nil || !store.enqueued[1].ScheduledFor.Equal(at) {
		t.Errorf("a run given --for carries %v", store.enqueued[1].ScheduledFor)
	}
}

// --- Overdue (ADR §105) ---

// A job that has gone its window without a success is reported once, under its
// own fingerprint, which its next success resolves.
func TestAnOverdueJobIsRecordedUnderItsOwnFingerprint(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	store.overdue = []database.OverdueJob{{ID: "d1", AppID: "app-1", Name: "extract",
		ExpectWithinSec: 26 * 3600, Since: time.Now().Add(-30 * time.Hour)}}

	svc.checkOverdue(time.Now())

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 1 {
		t.Fatalf("recorded %d events, want one", len(sink.inserted))
	}
	got := sink.inserted[0]
	if got.Kind != string(events.KindJobRunOverdue) || got.Fingerprint != "d1:overdue" {
		t.Errorf("event = %s/%s", got.Kind, got.Fingerprint)
	}
	if !strings.Contains(got.Summary, `"extract"`) || !strings.Contains(got.Summary, "26 hours") {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestHumanWindow(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: "1 hour", 26 * time.Hour: "26 hours", 7 * 24 * time.Hour: "7 days", 24 * time.Hour: "24 hours",
	} {
		if got := humanWindow(d); got != want {
			t.Errorf("humanWindow(%s) = %q, want %q", d, got, want)
		}
	}
}

// --- Placement (ADR §106) ---

// Where isolated execution is available a run is a pod, exactly as before; where
// it is not, the run goes to this machine. The install decides, never the app.
func TestARunIsPlacedOnTheHostWhenIsolatedIsNotAvailable(t *testing.T) {
	store, isolated, host := newFakeStore(), newFakeBackend(), newFakeBackend()
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeShared), ReleaseRef: "/apps/app-1/releases/d1"}
	store.settings.IsolatedEnabled = false
	svc := New(store, isolated, Options{NodeID: "node-a", Dials: envelope(512), Host: host})

	run, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution mode = %q, want shared — the run records where it actually ran", run.ExecutionMode)
	}
	svc.tickDispatch()
	if len(host.started) != 1 || len(isolated.started) != 0 {
		t.Fatalf("host started %d, cluster started %d; want the host only", len(host.started), len(isolated.started))
	}

	store.settings.IsolatedEnabled = true
	run2, _ := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if run2.ExecutionMode != string(runtime.ModeIsolated) {
		t.Errorf("with isolated available the run is %q, want isolated", run2.ExecutionMode)
	}
}

// An install with no cluster and no host backend has nowhere to run anything,
// and says so rather than queueing work that will never start.
func TestJobsNeedSomewhereToRun(t *testing.T) {
	store := newFakeStore()
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", ReleaseRef: "x"}
	svc := New(store, nil, Options{NodeID: "node-a", Dials: envelope(512)})
	if _, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "", nil); !errors.Is(err, ErrJobsUnavailable) {
		t.Errorf("err = %v, want ErrJobsUnavailable", err)
	}
	withHost := New(store, nil, Options{NodeID: "node-a", Dials: envelope(512), Host: newFakeBackend()})
	if _, err := withHost.Enqueue("app-1", "report", database.JobStartedByUI, "", nil); err != nil {
		t.Errorf("with a host backend the run was refused: %v", err)
	}
}

// A shared run the kernel stopped at its memory limit took its runner with it,
// so nothing reported, and the answer waited for the thirty-second sweep: the
// drills' hog was killed 1.3 s in and recorded 25 s later. The dispatcher's
// tick asks the agent, so the end lands within one dispatch interval — and
// leaves a pod's run, whose status is a cluster call, to the sweep.
func TestASharedRunThatEndedUnreportedIsSeenOnTheDispatchTick(t *testing.T) {
	store, isolated, host := newFakeStore(), newFakeBackend(), newFakeBackend()
	host.prefix = "host:"
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeShared), ReleaseRef: "/apps/app-1/releases/d1"}
	store.settings.IsolatedEnabled = false
	svc := New(store, isolated, Options{NodeID: "node-a", Dials: envelope(512), Host: host})

	run, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.tickDispatch()
	handle := "host:handle-" + run.ID
	host.statuses[handle] = runtime.JobStatus{State: runtime.JobRunning, PeakMemoryMB: 40}

	svc.pollShared()
	if got := store.finishFor(run.ID); got != nil {
		t.Fatalf("a run still going was finished: %+v", got)
	}

	host.statuses[handle] = runtime.JobStatus{State: runtime.JobResourceExceeded,
		Message: "stopped at 128 MB, its memory limit", PeakMemoryMB: 128}
	svc.pollShared()
	got := store.finishFor(run.ID)
	if got == nil || got.status != database.JobRunOutOfMemory {
		t.Fatalf("finish = %+v, want out_of_memory on the dispatch tick", got)
	}
}

// --- On time ---

// A run's timeout is kept by a timer armed at start, not by the thirty-second
// sweep: a five-second job used to end anywhere from seven to thirty-five
// seconds in.
func TestATimeoutIsEnforcedOnTimeNotOnTheNextSweep(t *testing.T) {
	svc, store, backend := newService(t)
	store.defs["def-quick"] = &database.JobDefinition{ID: "def-quick", AppID: "app-1", Name: "quick",
		Command: "sleep 60", TimeoutSec: 1, Enabled: true}
	store.runs["run-1"] = &database.JobRun{ID: "run-1", AppID: "app-1", DefinitionID: "def-quick",
		JobName: "quick", Status: database.JobRunQueued, ReleaseRef: "artifact:releases/app-1/dep-1.tar.gz"}

	svc.tickDispatch()
	deadline := time.Now().Add(3 * time.Second)
	for store.finishFor("run-1") == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	got := store.finishFor("run-1")
	if got == nil || got.status != database.JobRunTimedOut {
		t.Fatalf("a 1s run was not timed out within 3s, with no sweep running: %+v", got)
	}
	if !backend.wasCancelled("handle-run-1") {
		t.Error("the timed-out workload was left running")
	}

	// The sweep finding the same run afterwards changes nothing and records
	// nothing a second time.
	svc.tickSweep()
	n := 0
	store.mu.Lock()
	for _, f := range store.finishes {
		if f.runID == "run-1" {
			n++
		}
	}
	store.mu.Unlock()
	if n != 1 {
		t.Errorf("the run was finished %d times, want once", n)
	}
}

// A queued run wakes the dispatcher: it is claimed at once rather than at the
// next five-second tick.
func TestAQueuedRunIsDispatchedWithoutWaitingForTheTick(t *testing.T) {
	svc, store, backend := newService(t)
	svc.dispatchLoop()
	defer svc.Close()

	run, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(dispatchInterval / 2)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		n := len(backend.started)
		backend.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	t.Fatalf("run %s was not started within half a dispatch interval (status %s)",
		run.ID, store.runs[run.ID].Status)
}

// slowBackend takes a while to start each workload, as a host run resolving
// its release or a pod on a cold node does.
type slowBackend struct {
	*fakeBackend
	delay time.Duration
}

func (b *slowBackend) Start(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	time.Sleep(b.delay)
	return b.fakeBackend.Start(ctx, spec)
}

// Runs claimed together start together: one slow start no longer holds the
// next run at `claimed` behind it.
func TestClaimedRunsStartSideBySide(t *testing.T) {
	store, fb := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated), ReleaseRef: "artifact:releases/app-1/dep-1.tar.gz"}
	svc := New(store, &slowBackend{fb, 300 * time.Millisecond}, Options{NodeID: "node-a", Dials: envelope(512)})
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		store.runs[id] = &database.JobRun{ID: id, AppID: "app-1", JobName: "report",
			Status: database.JobRunQueued, ReleaseRef: "artifact:releases/app-1/dep-1.tar.gz"}
	}

	began := time.Now()
	svc.tickDispatch()
	if took := time.Since(began); took > 700*time.Millisecond {
		t.Errorf("three 300ms starts took %v — they ran one after another", took)
	}
	if len(fb.started) != 3 {
		t.Errorf("started %d runs, want 3", len(fb.started))
	}
}

// gatedBackend holds its first `hold` starts until release is closed, as a
// host run resolving a large release onto a network filesystem does.
type gatedBackend struct {
	*fakeBackend
	hold    int32
	calls   atomic.Int32
	release chan struct{}
	entered chan string
}

func (b *gatedBackend) Start(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	b.entered <- spec.Name
	if b.calls.Add(1) <= b.hold {
		<-b.release
	}
	return b.fakeBackend.Start(ctx, spec)
}

// A run queued while another is still starting starts at once. The loop used
// to wait for every start of its last pass before claiming again, so a run
// queued behind a ten-second host start sat at `pending` for the rest of it —
// with the wake its enqueue sent already spent — and its queue time was that
// start plus its own.
func TestARunQueuedBehindASlowStartIsNotHeldByIt(t *testing.T) {
	store, fb := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated), ReleaseRef: "artifact:releases/app-1/dep-1.tar.gz"}
	gb := &gatedBackend{fakeBackend: fb, hold: 1, release: make(chan struct{}), entered: make(chan string, 8)}
	svc := New(store, gb, Options{NodeID: "node-a", Dials: envelope(512)})
	svc.dispatchLoop()
	defer svc.Close()
	defer close(gb.release)

	_, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gb.entered:
	case <-time.After(dispatchInterval / 2):
		t.Fatal("the first run was not dispatched on its wake")
	}

	second, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "bob", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-gb.entered:
		if name != second.ID {
			t.Fatalf("started %s, want the second run %s", name, second.ID)
		}
	case <-time.After(time.Second):
		store.mu.Lock()
		defer store.mu.Unlock()
		t.Fatalf("the second run waited behind the first one's start (status %s)",
			store.runs[second.ID].Status)
	}
}

// The slots bound starts in progress, not runs: a pass claims only as many as
// are free, and a finished start frees its slot for the next.
func TestStartsInProgressAreBoundedBySlots(t *testing.T) {
	store, fb := newFakeStore(), newFakeBackend()
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeIsolated), ReleaseRef: "artifact:releases/app-1/dep-1.tar.gz"}
	gb := &gatedBackend{fakeBackend: fb, hold: 3, release: make(chan struct{}), entered: make(chan string, 8)}
	svc := New(store, gb, Options{NodeID: "node-a", Dials: envelope(512), MaxConcurrentDispatch: 2})
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		store.runs[id] = &database.JobRun{ID: id, AppID: "app-1", JobName: "report",
			Status: database.JobRunQueued, ReleaseRef: "artifact:releases/app-1/dep-1.tar.gz"}
	}

	wg := svc.dispatch()
	<-gb.entered
	<-gb.entered
	// Both slots are held: another pass claims nothing.
	svc.dispatch().Wait()
	store.mu.Lock()
	pending := 0
	for _, r := range store.runs {
		if r.Status == database.JobRunQueued {
			pending++
		}
	}
	store.mu.Unlock()
	if pending != 1 {
		t.Fatalf("%d runs pending with both slots busy, want 1", pending)
	}

	close(gb.release)
	wg.Wait()
	svc.tickDispatch()
	if len(fb.started) != 3 {
		t.Errorf("started %d runs once the slots freed, want 3", len(fb.started))
	}
}

// Only the writer that ends a run records its outcome. The runner's own report
// is the usual end, and the poller that backs it up must not report a run the
// report already finished — one failure is one event.
func TestOnlyTheWriterThatEndsARunRecordsIt(t *testing.T) {
	svc, store, _ := newService(t)
	sink := &eventStore{}
	svc.opts.Events = events.NewRecorder(sink, nil, nil)
	store.defs["d1"] = &database.JobDefinition{
		ID: "d1", AppID: "app-1", Name: "report", Command: "c", Enabled: true,
	}
	run := &database.JobRun{
		ID: "run-1", DefinitionID: "d1", AppID: "app-1", JobName: "report",
		StartedBy: database.JobStartedByApp, Status: database.JobRunRunning,
	}
	store.runs[run.ID] = run

	if !svc.finish(run, database.JobRunFailed, nil, "app_exec", "exit 1") {
		t.Fatal("the first terminal write did not report moving the run")
	}
	// The poller arriving second: the row is already terminal.
	if svc.finish(run, database.JobRunFailed, nil, "app_exec", "exit 1") {
		t.Fatal("a second terminal write reported moving the run")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.inserted) != 1 {
		t.Fatalf("recorded %d events, want one for one failure", len(sink.inserted))
	}
	if len(store.enqueued) != 0 {
		t.Errorf("enqueued %d runs — nothing re-runs by itself", len(store.enqueued))
	}
}

// The three spellings the tests were written against, over StartRun. Kept here
// rather than in the package: production code says what it means with a
// StartRequest, and these say it compactly where the request is not the point.
func (s *Service) Enqueue(appID, job, startedBy, actor string, params []byte) (*database.JobRun, error) {
	run, _, err := s.StartRun(StartRequest{AppID: appID, Job: job, StartedBy: startedBy, ActorID: actor, Params: params})
	return run, err
}

func (s *Service) EnqueueFor(appID, job, startedBy, actor string, params []byte, at *time.Time) (*database.JobRun, error) {
	run, _, err := s.StartRun(StartRequest{AppID: appID, Job: job, StartedBy: startedBy, ActorID: actor,
		Params: params, ScheduledFor: at})
	return run, err
}

func (s *Service) EnqueueIdempotent(appID, job, startedBy, actor string, params []byte, key string) (*database.JobRun, bool, error) {
	return s.StartRun(StartRequest{AppID: appID, Job: job, StartedBy: startedBy, ActorID: actor,
		Params: params, IdempotencyKey: key, ExplicitKey: key != ""})
}

// A ceiling an administrator lowers does not break a running app: the same
// declaration redeployed is capped and warned about, and only a new or changed
// declaration above the ceiling is refused.
func TestALoweredCeilingCapsExistingDeclarationsAndRefusesNewOnes(t *testing.T) {
	settings := database.PlatformSettings{DefaultJobTimeoutSecs: 600, MaxJobTimeoutSecs: 900}
	deployed := []database.JobDefinition{{Name: "nightly", TimeoutSec: 2700}}

	if err := CheckCeilingsAgainst(deployed, deployed, settings); err != nil {
		t.Errorf("an unchanged declaration was refused: %v", err)
	}
	if w := ClampWarnings(deployed, settings); len(w) != 1 || !strings.Contains(w[0], `timeout = "15m"`) {
		t.Errorf("warnings = %v, want one naming the fix", w)
	}
	changed := []database.JobDefinition{{Name: "nightly", TimeoutSec: 3600}}
	if err := CheckCeilingsAgainst(changed, deployed, settings); err == nil {
		t.Error("a changed declaration above the ceiling was accepted")
	}
	added := []database.JobDefinition{deployed[0], {Name: "weekly", TimeoutSec: 1200}}
	if err := CheckCeilingsAgainst(added, deployed, settings); err == nil {
		t.Error("a new declaration above the ceiling was accepted")
	}
}

// A run that ends by the runner's own report — the usual way — still carries
// the peak the backend measured. Without NoteBackendPeak a run shorter than
// the poll interval, on a machine with no cgroup per run, finished with no
// peak at all (drills on trytoft, 2026-10-23: every short run's detail had no
// peak-memory line).
func TestTheRunnersReportCarriesTheBackendsPeak(t *testing.T) {
	svc, store, backend := newService(t)
	store.runs["run-1"] = &database.JobRun{ID: "run-1", AppID: "app-1", JobName: "mail",
		Status: database.JobRunRunning, ExternalID: "handle-run-1"}
	backend.statuses["handle-run-1"] = runtime.JobStatus{State: runtime.JobRunning, PeakMemoryMB: 17}

	svc.NoteBackendPeak(context.Background(), "run-1")

	got, _ := store.GetJobRun("run-1")
	if got.PeakMemoryMB == nil || *got.PeakMemoryMB != 17 {
		t.Fatalf("peak = %v, want the backend's 17 MB", got.PeakMemoryMB)
	}

	// A run with no handle yet, or a backend with no record, records nothing
	// and does not fail.
	store.runs["run-2"] = &database.JobRun{ID: "run-2", AppID: "app-1", JobName: "mail",
		Status: database.JobRunRunning}
	svc.NoteBackendPeak(context.Background(), "run-2")
	svc.NoteBackendPeak(context.Background(), "no-such-run")
	if got, _ := store.GetJobRun("run-2"); got.PeakMemoryMB != nil {
		t.Errorf("a run with no handle got a peak: %d", *got.PeakMemoryMB)
	}
}

// endingBackend is a backend whose Cancel returns before the workload is gone,
// and which offers End for a caller that has to wait — the host backend's shape.
type endingBackend struct {
	*fakeBackend
	ended []string
}

func (b *endingBackend) End(ctx context.Context, handle string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ended = append(b.ended, handle)
	return nil
}

// Deleting an app ends its runs in flight and waits for them — the app's files
// are removed next — and touches no other app's run, nor one that never
// reached the backend.
func TestEndAppRunsEndsOnlyThatAppsStartedRuns(t *testing.T) {
	store := newFakeStore()
	backend := &endingBackend{fakeBackend: newFakeBackend()}
	svc := New(store, backend, Options{NodeID: "node-a", Dials: envelope(512)})
	store.runs["mine"] = &database.JobRun{ID: "mine", AppID: "app-1", Status: database.JobRunRunning, ExternalID: "handle-mine"}
	store.runs["claimed"] = &database.JobRun{ID: "claimed", AppID: "app-1", Status: database.JobRunClaimed}
	store.runs["theirs"] = &database.JobRun{ID: "theirs", AppID: "app-2", Status: database.JobRunRunning, ExternalID: "handle-theirs"}

	svc.EndAppRuns(context.Background(), "app-1")

	if len(backend.ended) != 1 || backend.ended[0] != "handle-mine" {
		t.Errorf("ended %v, want only the deleted app's started run", backend.ended)
	}
	if len(backend.cancelled) != 0 {
		t.Errorf("cancelled %v: a backend that can wait is asked to", backend.cancelled)
	}
}

// An out-of-memory run is a job_run_failed event like a failed or timed-out
// one (BB-026), on every path that finishes it.
func TestAnOutOfMemoryRunRaisesJobRunFailed(t *testing.T) {
	store, isolated, host := newFakeStore(), newFakeBackend(), newFakeBackend()
	host.prefix = "host:"
	store.apps["app-1"] = &database.App{ID: "app-1", Slug: "demo", Framework: "python",
		ExecutionMode: string(runtime.ModeShared), ReleaseRef: "/apps/app-1/releases/d1"}
	store.settings.IsolatedEnabled = false
	sink := &eventStore{}
	svc := New(store, isolated, Options{NodeID: "node-a", Dials: envelope(512), Host: host,
		Events: events.NewRecorder(sink, nil, nil)})

	run, err := svc.Enqueue("app-1", "report", database.JobStartedByUI, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.tickDispatch()
	host.statuses["host:handle-"+run.ID] = runtime.JobStatus{State: runtime.JobResourceExceeded,
		Message: "stopped at 128 MB, its memory limit", PeakMemoryMB: 128}
	svc.pollShared()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, ev := range sink.inserted {
		if ev.Kind == string(events.KindJobRunFailed) {
			return
		}
	}
	t.Fatalf("no job_run_failed event for an out-of-memory run; recorded %+v", sink.inserted)
}

// A stop that fails once is asked again: the timed-out row is terminal, so
// nothing but the owed-stop record reaches its workload a second time (RT-2).
func TestAFailedCancelIsRetriedUntilTheWorkloadIsGone(t *testing.T) {
	svc, store, backend := newService(t)

	started := time.Now().UTC().Add(-time.Hour)
	deadline := started.Add(time.Minute)
	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning,
		ExternalID: "handle-run-1", StartedAt: &started, DeadlineAt: &deadline,
	}
	backend.statuses["handle-run-1"] = runtime.JobStatus{State: runtime.JobRunning}
	backend.cancelFails = 1

	svc.tickSweep() // times it out; the Cancel fails, and the retry in the same sweep lands
	if n := backend.cancelCount("handle-run-1"); n != 2 {
		t.Fatalf("cancels after the first sweep = %d, want 2 (the failed one, then the owed retry)", n)
	}
	if !store.isOwed("run-1") {
		t.Fatal("the owed stop was cleared before the backend reported the workload gone")
	}
	svc.tickSweep() // the backend now says cancelled: the record clears
	if store.isOwed("run-1") {
		t.Error("the owed stop was not cleared once the workload was gone")
	}
	if n := backend.cancelCount("handle-run-1"); n != 2 {
		t.Errorf("cancels = %d, want no third once the workload is gone", n)
	}
}

// A backend-reported infrastructure failure stops the Job behind it, which
// would otherwise stay in the cluster (K8-4), and records no exit code (K8-6).
func TestAnInfrastructureErrCancelsTheWorkload(t *testing.T) {
	svc, store, backend := newService(t)
	store.runs["run-1"] = &database.JobRun{
		ID: "run-1", AppID: "app-1", Status: database.JobRunRunning, ExternalID: "handle-run-1",
	}
	backend.statuses["handle-run-1"] = runtime.JobStatus{State: runtime.JobInfrastructureErr,
		Stage: runtime.FailureImagePull, Message: "InvalidImageName"}

	svc.pollActive()

	if n := backend.cancelCount("handle-run-1"); n != 1 {
		t.Errorf("cancels = %d, want 1", n)
	}
	got := store.finishFor("run-1")
	if got == nil || got.status != database.JobRunFailed || got.exitCode != nil {
		t.Errorf("finish = %+v, want failed with no exit code", got)
	}
}

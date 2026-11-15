package database_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func seedJobApp(t *testing.T, db *database.DB, slug string) *database.App {
	t.Helper()
	if err := db.UpsertUser(&database.User{ID: "alice", Login: "alice"}); err != nil {
		t.Fatal(err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "alice",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
		ExecutionMode: "isolated",
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return app
}

func seedJobDef(t *testing.T, db *database.DB, appID, name, schedule string) *database.JobDefinition {
	t.Helper()
	d := &database.JobDefinition{
		AppID: appID, Name: name, Command: "python report.py", Schedule: schedule,
		TimeoutSec: 3600, Enabled: true,
	}
	if err := upsertJobDef(db, d); err != nil {
		t.Fatalf("upsert definition: %v", err)
	}
	return d
}

// --- The claim, which is what makes the queue multi-node safe ---

// A claimed run is invisible to a second dispatcher. Two nodes polling
// simultaneously must partition the queue, not both start the same run — the
// failure here is a job running twice, which for a report or a payment batch
// is not a recoverable mistake.
func TestAClaimedRunIsInvisibleToASecondDispatcher(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	const runs = 20
	for i := 0; i < runs; i++ {
		r := &database.JobRun{AppID: app.ID, JobName: "report", ExecutionMode: "isolated"}
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
	}

	// Eight dispatchers claim concurrently, as eight nodes would.
	var (
		mu     sync.Mutex
		claims = map[string]int{}
		wg     sync.WaitGroup
	)
	for node := 0; node < 8; node++ {
		wg.Add(1)
		go func(node int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				got, err := db.ClaimJobRuns("node-"+string(rune('a'+node)), 3, 100)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				for _, r := range got {
					claims[r.ID]++
				}
				mu.Unlock()
			}
		}(node)
	}
	wg.Wait()

	if len(claims) != runs {
		t.Errorf("claimed %d distinct runs, want %d", len(claims), runs)
	}
	for id, n := range claims {
		if n != 1 {
			t.Errorf("run %s was claimed %d times — it would have executed more than once", id, n)
		}
	}
}

// A run claimed by a node that then died must return to the pool. Without
// this it is stranded forever: no longer pending, so no dispatcher will ever
// look at it again.
func TestStaleClaimsReturnToThePool(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	if err := db.EnqueueJobRun(r, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJobRuns("doomed-node", 10, 100); err != nil {
		t.Fatal(err)
	}

	// Nothing to claim while the claim is fresh.
	again, err := db.ClaimJobRuns("other-node", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a fresh claim was stolen by another node")
	}

	n, err := db.ReleaseStaleClaims(time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("released %d stale claims, want 1", n)
	}

	recovered, err := db.ClaimJobRuns("other-node", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != r.ID {
		t.Errorf("the stranded run was not recovered: %+v", recovered)
	}
}

// A claim released as stale and taken by another node is that node's: the
// first starter, stalled rather than dead, cannot write its handle over it.
func TestAStaleStarterCannotMarkARunAnotherNodeClaimed(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	if err := db.EnqueueJobRun(r, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJobRuns("slow-node", 10, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReleaseStaleClaims(time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ClaimJobRuns("other-node", 10, 100); len(got) != 1 {
		t.Fatalf("the released run was not re-claimed: %+v", got)
	}

	if err := db.MarkJobRunStarted(r.ID, "slow-node", "wl-slow", time.Now().Add(time.Hour)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the slow starter marked a run another node holds: err = %v, want ErrNoRows", err)
	}
	if err := db.MarkJobRunStarted(r.ID, "other-node", "wl-other", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("the claim's holder could not start it: %v", err)
	}
	if got, _ := db.GetJobRun(r.ID); got.ExternalID != "wl-other" {
		t.Errorf("external_id = %q, want the holder's handle", got.ExternalID)
	}
}

// A cancel while pending must mean the run never starts.
func TestCancelDuringPendingNeverStarts(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	if err := db.EnqueueJobRun(r, 0); err != nil {
		t.Fatal(err)
	}

	_, wasRunning, _, err := db.CancelJobRun(app.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wasRunning {
		t.Error("a pending run does not need stopping at the backend")
	}

	claimed, err := db.ClaimJobRuns("node-a", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Errorf("a cancelled run was claimed and would have executed: %+v", claimed)
	}

	got, _ := db.GetJobRun(r.ID)
	if got.Status != database.JobRunCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}

// Cancelling a *running* run reports that the backend still has to be told.
func TestCancelDuringRunningReportsItNeedsStopping(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	if err := db.MarkJobRunStarted(r.ID, "node-a", "pod/report-abc", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	_, wasRunning, _, err := db.CancelJobRun(app.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !wasRunning {
		t.Error("a running run must be stopped at the backend, not just marked")
	}
}

// The first terminal answer wins. A completion callback arriving after the
// sweep has already timed a run out must not resurrect it — otherwise a run
// flips from timed_out to succeeded and the timeout becomes unenforceable.
func TestATerminalStatusIsNotOverwritten(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(r.ID, "node-a", "pod/x", time.Now().Add(time.Hour))

	if err := db.FinishJobRun(r.ID, database.JobRunTimedOut, nil, "", "exceeded 1h"); err != nil {
		t.Fatal(err)
	}
	// A late success callback.
	zero := 0
	if err := db.FinishJobRun(r.ID, database.JobRunSucceeded, &zero, "", ""); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetJobRun(r.ID)
	if got.Status != database.JobRunTimedOut {
		t.Errorf("status = %q — a late callback overwrote a terminal status", got.Status)
	}
}

// Progress from a finished run is ignored rather than un-finishing it.
func TestProgressAfterCompletionIsIgnored(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(r.ID, "node-a", "pod/x", time.Now().Add(time.Hour))
	db.SetJobRunProgress(r.ID, 50, "")
	zero := 0
	db.FinishJobRun(r.ID, database.JobRunSucceeded, &zero, "", "")

	db.SetJobRunProgress(r.ID, 90, "")

	got, _ := db.GetJobRun(r.ID)
	if got.Progress != 50 {
		t.Errorf("progress = %d, want the value at completion (50)", got.Progress)
	}
	if got.Status != database.JobRunSucceeded {
		t.Errorf("status = %q", got.Status)
	}
}

// The per-app concurrency limit protects the shared footprint, and must hold
// under concurrent enqueues — two callers each reading "2 of 3" and both
// proceeding is exactly the race the transaction exists to close.
func TestPerAppConcurrencyLimitHoldsUnderRace(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	const limit = 3
	var (
		mu       sync.Mutex
		accepted int
		wg       sync.WaitGroup
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &database.JobRun{AppID: app.ID, JobName: "report"}
			err := db.EnqueueJobRun(r, limit)
			switch {
			case err == nil:
				mu.Lock()
				accepted++
				mu.Unlock()
			case errors.Is(err, database.ErrQueueLimit):
				// Expected for the losers.
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if accepted != limit {
		t.Errorf("accepted %d runs, want exactly %d", accepted, limit)
	}
}

// --- Scheduling ---

// A schedule due during downtime is skipped, never fired late: a backlog of
// nightly reports all firing at once on boot is worse than skipping them.
func TestMissedTicksAreSkippedNotFiredLate(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	def := seedJobDef(t, db, app.ID, "nightly", "0 3 * * *")

	// The schedule was due three days ago and nothing ran.
	overdue := time.Now().UTC().Add(-72 * time.Hour)
	if err := db.SetNextRunAt(def.ID, &overdue); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	fired, _, err := db.ClaimDueSchedules(100, now, func(_ string, after time.Time) (time.Time, error) {
		return after.Add(24 * time.Hour), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 {
		t.Fatalf("fired %d schedules, want 1 — the missed window collapses to a single fire", len(fired))
	}

	// The next fire is computed from now, not from the missed time, so the
	// two lost days do not queue up.
	got, _ := db.GetJobDefinition(app.ID, def.ID)
	if got.NextRunAt == nil || got.NextRunAt.Before(now) {
		t.Errorf("next_run_at = %v, want a time after now", got.NextRunAt)
	}

	// Immediately claiming again finds nothing.
	again, _, err := db.ClaimDueSchedules(100, now, func(_ string, after time.Time) (time.Time, error) {
		return after.Add(24 * time.Hour), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("the same schedule fired twice")
	}
}

// Two schedulers must not both fire the same schedule.
func TestConcurrentSchedulersFireEachScheduleOnce(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	past := time.Now().UTC().Add(-time.Minute)
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		d := seedJobDef(t, db, app.ID, name, "* * * * *")
		db.SetNextRunAt(d.ID, &past)
	}

	var (
		mu    sync.Mutex
		fired = map[string]int{}
		wg    sync.WaitGroup
	)
	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := db.ClaimDueSchedules(100, now, func(_ string, after time.Time) (time.Time, error) {
				return after.Add(time.Minute), nil
			})
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			for _, d := range got {
				fired[d.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(fired) != 6 {
		t.Errorf("fired %d distinct schedules, want 6", len(fired))
	}
	for id, n := range fired {
		if n != 1 {
			t.Errorf("schedule %s fired %d times", id, n)
		}
	}
}

// A schedule that no longer parses is disabled, not retried forever.
func TestUnparseableScheduleIsDisabled(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	def := seedJobDef(t, db, app.ID, "broken", "not a cron expression")

	past := time.Now().UTC().Add(-time.Minute)
	db.SetNextRunAt(def.ID, &past)

	fired, disabled, err := db.ClaimDueSchedules(100, time.Now().UTC(), func(s string, _ time.Time) (time.Time, error) {
		return time.Time{}, errors.New("cannot parse " + s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 0 {
		t.Error("a job with an unparseable schedule should not fire")
	}
	// Handed back so the scheduler can record an app event. The old claim
	// filtered these out before returning, which is why the branch meant to
	// report them had never run.
	if len(disabled) != 1 || disabled[0].ID != def.ID || disabled[0].AppID != app.ID {
		t.Fatalf("disabled = %+v, want the one broken schedule with its app", disabled)
	}

	got, _ := db.GetJobDefinition(app.ID, def.ID)
	if got.Enabled {
		t.Error("an unparseable schedule should be disabled rather than retried forever")
	}
}

// --- Definitions ---

// toft.toml is the only place a job is defined, so a deploy that no longer
// declares one deletes it — and its runs stay, under the name they ran as.
func TestManifestReconcileDeletesWhatIsNoLongerDeclaredButKeepsHistory(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	manifestV1 := []database.JobDefinition{
		{Name: "nightly", Command: "python nightly.py", Schedule: "0 3 * * *", Overlap: "skip", Notify: "failures"},
		{Name: "weekly", Args: []string{"job", "weekly"}, Schedule: "0 4 * * 0"},
	}
	if err := db.ReconcileManifestJobs(app.ID, manifestV1); err != nil {
		t.Fatal(err)
	}
	weekly, _ := db.GetJobDefinitionByName(app.ID, "weekly")
	if len(weekly.Args) != 2 || weekly.Overlap != "queue" || weekly.Notify != "none" {
		t.Errorf("weekly = %+v, want its args and the default overlap and notify", weekly)
	}
	nightly, _ := db.GetJobDefinitionByName(app.ID, "nightly")
	if nightly.Overlap != "skip" || nightly.Notify != "failures" {
		t.Errorf("nightly = %+v", nightly)
	}

	run := &database.JobRun{DefinitionID: weekly.ID, AppID: app.ID, JobName: "weekly"}
	if err := db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	// The next deploy drops "weekly".
	if err := db.ReconcileManifestJobs(app.ID, manifestV1[:1]); err != nil {
		t.Fatal(err)
	}
	if gone, _ := db.GetJobDefinitionByName(app.ID, "weekly"); gone != nil {
		t.Error("a job removed from the manifest should be deleted")
	}
	if got, _ := db.GetJobRun(run.ID); got == nil {
		t.Error("run history was deleted with its definition")
	} else if got.JobName != "weekly" {
		t.Errorf("run lost its job name: %q", got.JobName)
	}
}

// A definition lookup must be scoped by app, or authorizing against the app in
// the path and then fetching by ID alone is an IDOR.
func TestJobLookupsAreScopedByApp(t *testing.T) {
	db := dbtest.New(t)
	mine := seedJobApp(t, db, "mine")
	theirs := seedJobApp(t, db, "theirs")

	def := seedJobDef(t, db, theirs.ID, "secret-report", "")
	run := &database.JobRun{DefinitionID: def.ID, AppID: theirs.ID, JobName: "secret-report"}
	db.EnqueueJobRun(run, 0)

	if got, _ := db.GetJobDefinition(mine.ID, def.ID); got != nil {
		t.Error("another app's job definition was readable through this app")
	}
	if got, _ := db.GetAppJobRun(mine.ID, run.ID, ""); got != nil {
		t.Error("another app's run was readable through this app")
	}
	if _, _, _, err := db.CancelJobRun(mine.ID, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("another app's run was cancellable through this app: %v", err)
	}
}

// --- Reconciliation and retention ---

func TestOverdueRunsAreFound(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	overdue := &database.JobRun{AppID: app.ID, JobName: "slow"}
	db.EnqueueJobRun(overdue, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(overdue.ID, "node-a", "pod/slow", time.Now().Add(-time.Minute))

	fine := &database.JobRun{AppID: app.ID, JobName: "quick"}
	db.EnqueueJobRun(fine, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(fine.ID, "node-a", "pod/quick", time.Now().Add(time.Hour))

	got, err := db.ListOverdueJobRuns(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != overdue.ID {
		t.Errorf("overdue = %+v, want just the slow run", got)
	}
}

// The deadline is stored, not held in memory, so a platform restart does not
// leave a job running past its timeout with nothing left to enforce it.
func TestDeadlineSurvivesInMemoryState(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "slow"}
	db.EnqueueJobRun(r, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	deadline := time.Now().UTC().Add(30 * time.Minute)
	db.MarkJobRunStarted(r.ID, "node-a", "pod/slow", deadline)

	// A "restarted" platform reads the active set back from the database.
	active, err := db.ListActiveJobRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1", len(active))
	}
	if active[0].DeadlineAt == nil {
		t.Fatal("deadline was not persisted — a restart would lose timeout enforcement")
	}
	if active[0].ExternalID != "pod/slow" {
		t.Errorf("external id = %q — the reconciler could not ask the backend about this run", active[0].ExternalID)
	}
}

func TestJobLogsRoundTripInOrder(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)

	if err := db.AppendJobRunLogs(r.ID, []database.JobLogLine{
		{Seq: 1, Stream: "stdout", Line: "starting"},
		{Seq: 2, Stream: "stderr", Line: "a warning"},
		{Seq: 3, Stream: "stdout", Line: "done"},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := db.GetJobRunLogs(r.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Line != "starting" || all[2].Line != "done" {
		t.Fatalf("logs out of order or incomplete: %+v", all)
	}
	if all[1].Stream != "stderr" {
		t.Errorf("stream lost: %+v", all[1])
	}

	// Tailing from a cursor returns only what is new.
	tail, _ := db.GetJobRunLogs(r.ID, 2, 100)
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Errorf("tail from seq 2 = %+v, want just seq 3", tail)
	}
}

// Retention is tiered: logs go first, run rows outlive them, because a run's
// cost and outcome stay interesting long after its output does.
func TestRetentionDropsLogsBeforeRows(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "old"}
	db.EnqueueJobRun(r, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(r.ID, "node-a", "pod/old", time.Now().Add(time.Hour))
	zero := 0
	db.FinishJobRun(r.ID, database.JobRunSucceeded, &zero, "", "")
	db.AppendJobRunLogs(r.ID, []database.JobLogLine{{Seq: 1, Line: "output"}})

	now := time.Now().UTC()
	// Logs expire; rows do not yet. The counts are separate because the clocks
	// are, and because the sweep reports them — a pass that took lines and no
	// rows has to be able to say so.
	first, err := db.ExpireJobRuns(now.Add(time.Minute), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.LogLines != 1 || first.Rows != 0 {
		t.Errorf("first pass removed %d row(s) and %d log line(s), want 0 and 1", first.Rows, first.LogLines)
	}
	if logs, _ := db.GetJobRunLogs(r.ID, 0, 10); len(logs) != 0 {
		t.Errorf("logs should have expired: %+v", logs)
	}
	if got, _ := db.GetJobRun(r.ID); got == nil {
		t.Error("the run row expired with its logs; rows should outlive them")
	}

	// Later, the row goes too.
	second, err := db.ExpireJobRuns(now.Add(time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Rows != 1 || second.LogLines != 0 {
		t.Errorf("second pass removed %d row(s) and %d log line(s), want 1 and 0", second.Rows, second.LogLines)
	}
	if got, _ := db.GetJobRun(r.ID); got != nil {
		t.Error("the run row should have expired")
	}
}

func TestEnqueueDefaultsParamsToAnObject(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	if err := db.EnqueueJobRun(r, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetJobRun(r.ID)

	var params map[string]any
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("params are not a JSON object: %q", got.Params)
	}
}

// --- Idempotency ---

// The 60-second trailing window, which is the half the unique index cannot
// express: Postgres has no time-boxed uniqueness constraint, so a finished run
// is matched by the *query* rather than by the index.
//
// Three cases, and each is a different real click. In flight is the
// double-click and the Streamlit rerun. Just finished is a job that took two
// seconds and a rerun that arrived at three. Long finished is somebody pressing
// the button again a minute later, which means what they think it means.
func TestEnqueueIdempotentCoalescesInFlightAndRecentRuns(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "recon")

	priya := mkActor(t, db, "priya")
	newRun := func(key string) *database.JobRun {
		return &database.JobRun{
			AppID: app.ID, JobName: "reconcile", StartedBy: database.JobStartedByApp,
			ActorID: priya, ExecutionMode: "isolated", IdempotencyKey: key,
		}
	}

	first := newRun("k1")
	existing, err := db.EnqueueJobRunIdempotent(first, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if existing != nil {
		t.Fatal("the first start of a key must insert")
	}

	// In flight: the repeat gets that run's handle rather than a second run.
	existing, err = db.EnqueueJobRunIdempotent(newRun("k1"), 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if existing == nil || existing.ID != first.ID {
		t.Fatalf("in-flight repeat = %v, want the original run %s", existing, first.ID)
	}

	// Finished, inside the window.
	zero := 0
	if err := db.FinishJobRun(first.ID, database.JobRunSucceeded, &zero, "", ""); err != nil {
		t.Fatal(err)
	}
	existing, err = db.EnqueueJobRunIdempotent(newRun("k1"), 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if existing == nil || existing.ID != first.ID {
		t.Fatalf("a run that finished seconds ago should still coalesce, got %v", existing)
	}

	// Finished, outside it: a deliberate re-run is a new run.
	third := newRun("k1")
	existing, err = db.EnqueueJobRunIdempotent(third, 0, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if existing != nil {
		t.Fatalf("past the window this must be a new run, got %s", existing.ID)
	}
	if third.ID == first.ID {
		t.Error("the new run reused the old run's id")
	}

	// And the key is recorded, so the partial unique index has something to
	// hold two racing dispatchers to.
	got, err := db.GetAppJobRun(app.ID, third.ID, "")
	if err != nil || got == nil {
		t.Fatalf("reading the run back: %v", err)
	}
	if got.IdempotencyKey != "k1" {
		t.Errorf("idempotency_key = %q, want k1", got.IdempotencyKey)
	}
}

// A run with no key is never coalesced with anything — the platform's own
// trigger endpoint enqueues this way, and two people pressing "run now" must
// produce two runs.
func TestEnqueueWithoutAKeyNeverCoalesces(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "recon")

	for i := 0; i < 2; i++ {
		r := &database.JobRun{
			AppID: app.ID, JobName: "reconcile", StartedBy: database.JobStartedByUI,
			ExecutionMode: "isolated",
		}
		existing, err := db.EnqueueJobRunIdempotent(r, 0, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if existing != nil {
			t.Fatal("a keyless enqueue must always insert")
		}
	}
	runs, err := db.ListJobRuns(app.ID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Errorf("got %d runs, want 2", len(runs))
	}
}

// scope=viewer, at the query level: one person's runs of one app, and never an
// app-started one — whose empty actor would otherwise match a blank filter and
// hand every viewer the app's own history.
func TestListActorJobRunsIsScopedToOnePerson(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "recon")

	mk := func(actor string) string {
		r := &database.JobRun{
			AppID: app.ID, JobName: "reconcile", StartedBy: database.JobStartedByApp,
			ActorID: actor, ExecutionMode: "isolated",
		}
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	priya, sam := mkActor(t, db, "priya"), mkActor(t, db, "sam")
	hers, his, its := mk(priya), mk(sam), mk("")

	got, err := db.ListActorJobRuns(app.ID, priya, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != hers {
		t.Fatalf("got %d runs %v, want exactly %s", len(got), got, hers)
	}
	for _, r := range got {
		if r.ID == his || r.ID == its {
			t.Errorf("leaked run %s", r.ID)
		}
	}

	// An empty actor is refused rather than treated as "no filter". The other
	// reading turns a missing viewer into a listing of everything.
	if got, err := db.ListActorJobRuns(app.ID, "", "", "", 50); err != nil || len(got) != 0 {
		t.Errorf("an empty actor returned %d runs, want none", len(got))
	}
}

// Queue position is the difference between a traceback in the middle of
// somebody's page and a sentence they can act on.
func TestQueuePositionCountsDownAsTheQueueDrains(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "recon")

	var runs []*database.JobRun
	for i := 0; i < 3; i++ {
		r := &database.JobRun{
			AppID: app.ID, JobName: "reconcile", StartedBy: database.JobStartedByApp,
			ExecutionMode: "isolated",
		}
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
		if pos, err := db.QueuePosition(r); err != nil || pos != i+1 {
			t.Fatalf("run %d: position %d (err %v), want %d", i, pos, err, i+1)
		}
	}

	// The front of the queue starts; everyone behind moves up. A run has to be
	// claimed before it can start — MarkJobRunStarted refuses otherwise, which
	// is what stops a cancel that landed mid-dispatch being overwritten.
	if _, err := db.ClaimJobRuns("node-a", 1, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkJobRunStarted(runs[0].ID, "node-a", "wl-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	third, err := db.GetAppJobRun(app.ID, runs[2].ID, "")
	if err != nil || third == nil {
		t.Fatal(err)
	}
	if pos, err := db.QueuePosition(third); err != nil || pos != 2 {
		t.Errorf("position %d (err %v), want 2 after the front of the queue started", pos, err)
	}

	// A run that is no longer pending has no position at all, rather than a
	// stale one.
	started, _ := db.GetAppJobRun(app.ID, runs[0].ID, "")
	if pos, err := db.QueuePosition(started); err != nil || pos != 0 {
		t.Errorf("a running run reported position %d, want 0", pos)
	}
}

// An output object expires with its run row, which means the sweep has to hand
// back what it deleted — nothing else knows the keys once the rows are gone.
func TestExpiringRunsReportsTheirOutputObjects(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "recon")

	withOutput := &database.JobRun{AppID: app.ID, JobName: "reconcile", ExecutionMode: "isolated"}
	without := &database.JobRun{AppID: app.ID, JobName: "reconcile", ExecutionMode: "isolated"}
	for _, r := range []*database.JobRun{withOutput, without} {
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
		zero := 0
		if err := db.FinishJobRun(r.ID, database.JobRunSucceeded, &zero, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	key := "runs/" + app.ID + "/" + withOutput.ID + "/outputs/output"
	if err := db.SetJobRunOutputKey(app.ID, withOutput.ID, key); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	expired, err := db.ExpireJobRuns(now.Add(time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired.Outputs) != 1 || expired.Outputs[0].OutputKey != key {
		t.Fatalf("expired = %+v, want exactly the one object that exists", expired.Outputs)
	}
	if expired.Outputs[0].RunID != withOutput.ID {
		t.Errorf("run id = %s, want %s", expired.Outputs[0].RunID, withOutput.ID)
	}
}

// The install-wide read is what Admin → Job Runs is built on, and the thing it
// must get right is that it crosses apps: a run started by an app, on an app
// the operator holds no grant on, is exactly the one that is otherwise
// invisible until somebody complains.
func TestListAllJobRunsCrossesApps(t *testing.T) {
	db := dbtest.New(t)
	one := seedJobApp(t, db, "reports")
	two := seedJobApp(t, db, "billing")

	// One finished run on each app, plus a pending one on the second.
	for _, app := range []*database.App{one, two} {
		r := &database.JobRun{AppID: app.ID, JobName: "nightly", ExecutionMode: "isolated"}
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
		zero := 0
		if err := db.FinishJobRun(r.ID, database.JobRunSucceeded, &zero, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	pending := &database.JobRun{
		AppID: two.ID, JobName: "invoice", ExecutionMode: "isolated",
		StartedBy: database.JobStartedByApp,
	}
	if err := db.EnqueueJobRun(pending, 0); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListAllJobRuns(database.JobRunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %d runs across two apps, want 3", len(all))
	}

	// Every row carries enough of its app to render without a second read, and
	// the owner is joined at read time rather than denormalized.
	for _, r := range all {
		if r.AppName == "" || r.AppSlug == "" {
			t.Errorf("run %s carries no app: %+v", r.ID, r)
		}
		if r.OwnerLogin != "alice" {
			t.Errorf("owner = %q, want alice", r.OwnerLogin)
		}
	}

	// The pseudo-status is the page's default filter, and it must mean the
	// three live states rather than any one of them.
	live, err := db.ListAllJobRuns(database.JobRunFilter{Status: database.JobRunInFlight})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != pending.ID {
		t.Fatalf("in-flight filter returned %+v, want just the pending run", live)
	}

	// The other pseudo-status groups the four ways a run ends badly, and must
	// not sweep in a cancel — somebody stopping a run on purpose is not a
	// failure to go and look at.
	zero := 0
	bad := &database.JobRun{AppID: one.ID, JobName: "nightly", ExecutionMode: "isolated"}
	if err := db.EnqueueJobRun(bad, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJobRun(bad.ID, database.JobRunTimedOut, &zero, "run", "deadline"); err != nil {
		t.Fatal(err)
	}
	stopped := &database.JobRun{AppID: one.ID, JobName: "nightly", ExecutionMode: "isolated"}
	if err := db.EnqueueJobRun(stopped, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJobRun(stopped.ID, database.JobRunCancelled, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	unsuccessful, err := db.ListAllJobRuns(database.JobRunFilter{Status: database.JobRunUnsuccessful})
	if err != nil {
		t.Fatal(err)
	}
	if len(unsuccessful) != 1 || unsuccessful[0].ID != bad.ID {
		t.Fatalf("unsuccessful filter returned %+v, want just the run that timed out", unsuccessful)
	}

	// A concrete status still filters to exactly itself.
	done, err := db.ListAllJobRuns(database.JobRunFilter{Status: database.JobRunSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Errorf("succeeded filter returned %d runs, want 2", len(done))
	}

	// The window is on queued_at, so a run that started before it is excluded
	// even though it is still going.
	future := time.Now().UTC().Add(time.Hour)
	none, err := db.ListAllJobRuns(database.JobRunFilter{Since: &future})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("window returned %d runs from the future", len(none))
	}

	// Scoping to one app is the same query, and must not leak the other's.
	scoped, err := db.ListAllJobRuns(database.JobRunFilter{AppID: one.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) == 0 {
		t.Fatalf("app filter returned nothing for %s", one.ID)
	}
	for _, r := range scoped {
		if r.AppID != one.ID {
			t.Errorf("app filter leaked a run from %s", r.AppID)
		}
	}
}

// --- Retries and idempotency (ADR §33 D12) ---

// A pause lasts until the next deploy: the deploy re-applies toft.toml, and
// toft.toml says the job runs.
func TestADeployResumesAPausedJob(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	manifest := []database.JobDefinition{
		{Name: "nightly", Command: "python nightly.py", Schedule: "0 3 * * *"},
	}
	if err := db.ReconcileManifestJobs(app.ID, manifest); err != nil {
		t.Fatal(err)
	}
	nightly, _ := db.GetJobDefinitionByName(app.ID, "nightly")
	if err := db.SetJobEnabled(app.ID, nightly.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if paused, _ := db.GetJobDefinitionByName(app.ID, "nightly"); paused.Enabled {
		t.Fatal("setup: the pause did not take")
	}

	next := time.Now().Add(time.Hour).UTC()
	manifest[0].Command = "python nightly.py --fast"
	manifest[0].NextRunAt = &next
	if err := db.ReconcileManifestJobs(app.ID, manifest); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetJobDefinitionByName(app.ID, "nightly")
	if !after.Enabled || after.NextRunAt == nil {
		t.Errorf("after a deploy: enabled %v, next run %v — want resumed", after.Enabled, after.NextRunAt)
	}
	if after.Command != "python nightly.py --fast" {
		t.Errorf("the command was not reconciled: %q", after.Command)
	}
}

// The claim never starts more of one app's runs than its runs-at-once limit:
// the rest wait, and another app's runs go past them.
func TestTheClaimHoldsEachAppToItsRunsAtOnce(t *testing.T) {
	db := dbtest.New(t)
	busy := seedJobApp(t, db, "busy")
	quiet := seedJobApp(t, db, "quiet")
	for i := 0; i < 4; i++ {
		if err := db.EnqueueJobRun(&database.JobRun{AppID: busy.ID, JobName: "report"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EnqueueJobRun(&database.JobRun{AppID: quiet.ID, JobName: "report"}, 0); err != nil {
		t.Fatal(err)
	}

	claimed, err := db.ClaimJobRuns("node-a", 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	perApp := map[string]int{}
	for _, r := range claimed {
		perApp[r.AppID]++
	}
	if perApp[busy.ID] != 2 || perApp[quiet.ID] != 1 {
		t.Fatalf("claimed %v, want two of busy's and quiet's one", perApp)
	}
	// Nothing more of busy's until one of its two finishes.
	if again, _ := db.ClaimJobRuns("node-a", 10, 2); len(again) != 0 {
		t.Errorf("claimed %d more while busy was at its limit", len(again))
	}
	// Finish one of busy's — by app, not by position: the claim is an UPDATE …
	// RETURNING, whose rows come back in no promised order, and finishing
	// quiet's run (when it came first) frees nothing busy can use.
	var busyRun string
	for _, r := range claimed {
		if r.AppID == busy.ID {
			busyRun = r.ID
			break
		}
	}
	exit := 0
	if _, err := db.FinishJobRunOnce(busyRun, database.JobRunSucceeded, &exit, "", ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.ClaimJobRuns("node-a", 10, 2); len(again) != 1 || again[0].AppID != busy.ID {
		t.Errorf("claimed %+v after a slot freed, want one of busy's", again)
	}
}

// overlap = "skip" records a skipped run while the job has an open one, and
// the queue cap refuses a start only when the queue itself is full.
func TestScheduledSkipsAndTheQueueCap(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	if err := db.EnqueueJobRun(&database.JobRun{AppID: app.ID, JobName: "nightly"}, 0); err != nil {
		t.Fatal(err)
	}
	skipped, err := db.EnqueueScheduledJobRun(&database.JobRun{AppID: app.ID, JobName: "nightly",
		StartedBy: database.JobStartedBySchedule}, database.JobOverlapSkip, 0)
	if err != nil || !skipped {
		t.Fatalf("skipped = %v, err %v — want a skipped run", skipped, err)
	}
	runs, _ := db.ListJobRuns(app.ID, "nightly", database.JobRunSkipped, 10)
	if len(runs) != 1 || runs[0].FinishedAt == nil || runs[0].FailureReason == "" {
		t.Errorf("skipped runs = %+v, want one, finished, with its reason", runs)
	}
	queued, err := db.EnqueueScheduledJobRun(&database.JobRun{AppID: app.ID, JobName: "nightly",
		StartedBy: database.JobStartedBySchedule}, database.JobOverlapQueue, 0)
	if err != nil || queued {
		t.Fatalf("overlap queue: skipped %v, err %v — want it queued", queued, err)
	}
	if err := db.EnqueueJobRun(&database.JobRun{AppID: app.ID, JobName: "other"}, 2); !errors.Is(err, database.ErrQueueLimit) {
		t.Errorf("a third queued run past a cap of 2: %v, want ErrQueueLimit", err)
	}
}

// The eight characters `toft job runs` prints resolve to the run, within its
// app and only when exactly one matches.
func TestARunIsFoundByItsShortID(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	r := &database.JobRun{AppID: app.ID, JobName: "nightly"}
	if err := db.EnqueueJobRun(r, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ResolveJobRunID(app.ID, r.ID[:8]); got != r.ID {
		t.Errorf("resolved %q, want %q", got, r.ID)
	}
	other := seedJobApp(t, db, "other")
	if got, _ := db.ResolveJobRunID(other.ID, r.ID[:8]); got != "" {
		t.Errorf("another app's run resolved: %q", got)
	}
}

// Only the first of two callers timing a run out moves it, and only that one
// is told it did — the timer and the sweep both reach an overdue run.
func TestOnlyOneCallerTimesARunOut(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(r.ID, "node-a", "pod/x", time.Now().Add(-time.Second))

	first, err := db.TimeOutJobRun(r.ID, "the run exceeded its timeout")
	if err != nil || !first {
		t.Fatalf("first TimeOutJobRun = %v, %v; want it to move the run", first, err)
	}
	second, err := db.TimeOutJobRun(r.ID, "the run exceeded its timeout")
	if err != nil || second {
		t.Fatalf("second TimeOutJobRun = %v, %v; want false — it was already timed out", second, err)
	}
	got, _ := db.GetJobRun(r.ID)
	if got.Status != database.JobRunTimedOut || got.FinishedAt == nil {
		t.Errorf("run = %s finished %v, want timed_out with a finish time", got.Status, got.FinishedAt)
	}

	done := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(done, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	db.MarkJobRunStarted(done.ID, "node-a", "pod/y", time.Now().Add(-time.Second))
	zero := 0
	db.FinishJobRun(done.ID, database.JobRunSucceeded, &zero, "", "")
	if moved, _ := db.TimeOutJobRun(done.ID, "late"); moved {
		t.Error("a succeeded run was timed out after the fact")
	}
}

// A cancel that commits while a claim is waiting on the run's row lock wins:
// the claim never takes a run that is no longer queued. The claim's UPDATE
// picks its ids in a subquery, and Postgres re-checks only the outer WHERE
// against the row a concurrent writer left — so without the status there, a
// run cancelled mid-claim went back to `claimed`, was started, and held one of
// its app's slots until something cancelled it again (trytoft, JOB.6 beside
// a 53-start burst).
func TestACancelLandingDuringTheClaimIsNotOverwritten(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	run := &database.JobRun{AppID: app.ID, JobName: "report", ExecutionMode: "isolated"}
	if err := db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	// The cancel's half, held open: the row locked and already cancelled.
	tx, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM job_runs WHERE id=? FOR UPDATE`, run.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE job_runs SET status=?, finished_at=? WHERE id=?`,
		database.JobRunCancelled, time.Now().UTC(), run.ID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		runs []database.JobRun
		err  error
	}
	done := make(chan result, 1)
	go func() {
		runs, err := db.ClaimJobRuns("node-a", 10, 3)
		done <- result{runs, err}
	}()
	// Long enough for the claim to have chosen the run and blocked on its lock.
	select {
	case r := <-done:
		t.Fatalf("the claim returned %d run(s) without waiting on the locked row: %v", len(r.runs), r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.runs) != 0 {
		t.Errorf("claimed %d run(s), want none: the only one was cancelled", len(r.runs))
	}
	got, err := db.GetAppJobRun(app.ID, run.ID, "")
	if err != nil || got == nil {
		t.Fatalf("reading the run: %v", err)
	}
	if got.Status != database.JobRunCancelled {
		t.Errorf("status = %q, want the cancel to stand", got.Status)
	}
}

// A job printing binary used to lose the whole batch to Postgres's refusal of
// a NUL (JOBS-1): the line is cleaned, and a row still refused costs only itself.
func TestJobLogsSurviveABadLine(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "binlog")
	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)

	if err := db.AppendJobRunLogs(r.ID, []database.JobLogLine{
		{Seq: 1, Stream: "stdout", Line: "a\x00b"},
		{Seq: 1 << 40, Stream: "stdout", Line: "refused"},
		{Seq: 3, Stream: "stdout", Line: "\xffend"},
	}); err != nil {
		t.Fatal(err)
	}
	all, err := db.GetJobRunLogs(r.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Line != "a�b" || all[1].Line != "�end" {
		t.Fatalf("logs = %+v, want the two good lines cleaned", all)
	}
	if err := db.AppendJobRunLogs("no-such-run", []database.JobLogLine{{Seq: 1, Line: "x"}, {Seq: 2, Line: "y"}}); err == nil {
		t.Error("a batch no row of which can be written must still report its error")
	}
}

// A deploy that does not change a live schedule keeps its stored next_run_at.
// The manifest computes its value from "now" at detect: taken as is, a deploy
// just after a cron minute overwrote the due tick with the next one (the firing
// was lost), and one spanning the sweep put back a tick already fired (the
// minute ran twice). A changed schedule takes the manifest's value.
func TestADeployKeepsAnUnchangedSchedulesNextRun(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	due := time.Now().Add(-5 * time.Second).UTC().Truncate(time.Minute)
	manifest := []database.JobDefinition{
		{Name: "nightly", Command: "python nightly.py", Schedule: "* * * * *", NextRunAt: &due},
	}
	if err := db.ReconcileManifestJobs(app.ID, manifest); err != nil {
		t.Fatal(err)
	}

	recomputed := due.Add(time.Minute)
	manifest[0].Command = "python nightly.py --fast"
	manifest[0].NextRunAt = &recomputed
	if err := db.ReconcileManifestJobs(app.ID, manifest); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetJobDefinitionByName(app.ID, "nightly")
	if after.NextRunAt == nil || !after.NextRunAt.Equal(due) {
		t.Errorf("next run after a redeploy = %v, want the stored %v", after.NextRunAt, due)
	}
	if after.Command != "python nightly.py --fast" {
		t.Errorf("the command was not reconciled: %q", after.Command)
	}

	changed := due.Add(time.Hour)
	manifest[0].Schedule = "0 * * * *"
	manifest[0].NextRunAt = &changed
	if err := db.ReconcileManifestJobs(app.ID, manifest); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetJobDefinitionByName(app.ID, "nightly"); got.NextRunAt == nil || !got.NextRunAt.Equal(changed) {
		t.Errorf("next run after a schedule change = %v, want %v", got.NextRunAt, changed)
	}
}

// One run per tick: a second scheduled enqueue for the same job and
// scheduled_for is refused and writes nothing, whatever let it come round.
func TestATickIsEnqueuedOnce(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	tick := time.Date(2026, 10, 28, 12, 33, 0, 0, time.UTC)
	run := func() *database.JobRun {
		at := tick
		return &database.JobRun{AppID: app.ID, JobName: "nightly",
			StartedBy: database.JobStartedBySchedule, ScheduledFor: &at}
	}
	if _, err := db.EnqueueScheduledJobRun(run(), database.JobOverlapQueue, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueScheduledJobRun(run(), database.JobOverlapQueue, 0); !errors.Is(err, database.ErrTickAlreadyEnqueued) {
		t.Errorf("second enqueue of the tick: %v, want ErrTickAlreadyEnqueued", err)
	}
	// A person's run for the same time (--for, a rerun) is not the schedule's.
	at := tick
	if err := db.EnqueueJobRun(&database.JobRun{AppID: app.ID, JobName: "nightly",
		StartedBy: database.JobStartedByCLI, ScheduledFor: &at}, 0); err != nil {
		t.Fatal(err)
	}
	runs, _ := db.ListJobRuns(app.ID, "nightly", "", 10)
	if len(runs) != 2 {
		t.Errorf("runs = %d, want the schedule's one and the person's", len(runs))
	}
}

// A firing the queue refuses is a skipped row with its reason, and the error
// still comes back so the caller records the event.
func TestAQueueFullFiringIsASkippedRow(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	for i := 0; i < 2; i++ {
		if err := db.EnqueueJobRun(&database.JobRun{AppID: app.ID, JobName: "other"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	tick := time.Date(2026, 10, 28, 12, 38, 0, 0, time.UTC)
	skipped, err := db.EnqueueScheduledJobRun(&database.JobRun{AppID: app.ID, JobName: "nightly",
		StartedBy: database.JobStartedBySchedule, ScheduledFor: &tick}, database.JobOverlapQueue, 2)
	var full *database.QueueLimitError
	if !skipped || !errors.As(err, &full) || full.Queued != 2 {
		t.Fatalf("skipped %v, err %v — want a skipped row and the QueueLimitError", skipped, err)
	}
	runs, _ := db.ListJobRuns(app.ID, "nightly", database.JobRunSkipped, 10)
	if len(runs) != 1 || runs[0].FinishedAt == nil || runs[0].ScheduledFor == nil ||
		!runs[0].ScheduledFor.Equal(tick) || !strings.Contains(runs[0].FailureReason, "queue was full") {
		t.Errorf("skipped runs = %+v, want one for the tick, finished, saying the queue was full", runs)
	}
}

// overlap = "queue" waits: a scheduled run is not claimed while another run of
// its job is open ahead of it, and takes no slot from another job meanwhile.
// A person's start is not held.
func TestAScheduledRunWaitsBehindItsOwnJob(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	sched := func(minute int) *database.JobRun {
		at := time.Date(2026, 10, 28, 12, minute, 0, 0, time.UTC)
		return &database.JobRun{AppID: app.ID, JobName: "refresh",
			StartedBy: database.JobStartedBySchedule, ScheduledFor: &at}
	}
	first := sched(40)
	if _, err := db.EnqueueScheduledJobRun(first, database.JobOverlapQueue, 0); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimJobRuns("node-a", 10, 3)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("claimed %+v, err %v — want the 12:40 run", claimed, err)
	}

	second, third := sched(41), sched(42)
	for _, r := range []*database.JobRun{second, third} {
		if _, err := db.EnqueueScheduledJobRun(r, database.JobOverlapQueue, 0); err != nil {
			t.Fatal(err)
		}
	}
	other := &database.JobRun{AppID: app.ID, JobName: "other", StartedBy: database.JobStartedBySchedule}
	if err := db.EnqueueJobRun(other, 0); err != nil {
		t.Fatal(err)
	}
	manual := &database.JobRun{AppID: app.ID, JobName: "refresh", StartedBy: database.JobStartedByCLI}
	if err := db.EnqueueJobRun(manual, 0); err != nil {
		t.Fatal(err)
	}

	claimed, err = db.ClaimJobRuns("node-a", 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range claimed {
		got[r.ID] = true
	}
	if len(claimed) != 2 || !got[other.ID] || !got[manual.ID] {
		t.Fatalf("claimed %d (%v), want the other job's run and the person's, not 12:41 or 12:42", len(claimed), got)
	}

	exit := 0
	for _, id := range []string{first.ID, manual.ID} {
		if _, err := db.FinishJobRunOnce(id, database.JobRunSucceeded, &exit, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	claimed, _ = db.ClaimJobRuns("node-a", 10, 3)
	if len(claimed) != 1 || claimed[0].ID != second.ID {
		t.Fatalf("claimed %+v after 12:40 finished, want 12:41 alone", claimed)
	}
	if again, _ := db.ClaimJobRuns("node-a", 10, 3); len(again) != 0 {
		t.Errorf("claimed %+v while 12:41 runs, want 12:42 still waiting", again)
	}
}

// BB-045 (fourth 2026-11-03 black-box list): an instant a person named with an
// offset reads back in that offset, so TOFT_SCHEDULED_FOR and every read of
// the run say the text as given; a UTC one and a tick read in UTC.
func TestANamedInstantKeepsItsOffset(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	named, err := time.Parse(time.RFC3339, "2026-11-03T09:00:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	r := &database.JobRun{AppID: app.ID, JobName: "report", StartedBy: database.JobStartedByCLI, ScheduledFor: &named}
	if err := db.EnqueueJobRun(r, 0); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetJobRun(r.ID)
	if err != nil || got == nil || got.ScheduledFor == nil {
		t.Fatalf("GetJobRun: %v %+v", err, got)
	}
	if s := got.ScheduledFor.Format(time.RFC3339); s != "2026-11-03T09:00:00-05:00" {
		t.Errorf("read back as %s, want the text as named", s)
	}
	utc := named.UTC()
	u := &database.JobRun{AppID: app.ID, JobName: "report", StartedBy: database.JobStartedByCLI, ScheduledFor: &utc}
	if err := db.EnqueueJobRun(u, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetJobRun(u.ID); got.ScheduledFor.Format(time.RFC3339) != "2026-11-03T14:00:00Z" {
		t.Errorf("a UTC instant read back as %s", got.ScheduledFor.Format(time.RFC3339))
	}
}

// BB-089: a run stopped before it finished — timed out, out of memory,
// cancelled while it ran — is at app_exec; one cancelled before it started
// (queued, or claimed and not yet started), or one that exited non-zero, has
// no stage.
func TestARunStoppedBeforeItFinishedIsAtAppExec(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	start := func() *database.JobRun {
		r := &database.JobRun{AppID: app.ID, JobName: "report"}
		db.EnqueueJobRun(r, 0)
		db.ClaimJobRuns("node-a", 10, 100)
		db.MarkJobRunStarted(r.ID, "node-a", "pod/x", time.Now().Add(time.Hour))
		return r
	}
	stage := func(r *database.JobRun) string {
		got, _ := db.GetJobRun(r.ID)
		return got.FailureStage
	}

	timed := start()
	db.TimeOutJobRun(timed.ID, "timed out after 5s")
	oom := start()
	db.FinishJobRun(oom.ID, database.JobRunOutOfMemory, nil, "", "stopped at 1024 MB")
	cancelled := start()
	db.CancelJobRun(app.ID, cancelled.ID)
	for name, r := range map[string]*database.JobRun{"timed out": timed, "out of memory": oom, "cancelled running": cancelled} {
		if got := stage(r); got != database.StageAppExec {
			t.Errorf("%s: failure_stage = %q, want app_exec", name, got)
		}
	}

	exited := start()
	one := 1
	db.FinishJobRun(exited.ID, database.JobRunFailed, &one, "", "exit 1")
	queued := &database.JobRun{AppID: app.ID, JobName: "other"}
	db.EnqueueJobRun(queued, 0)
	db.CancelJobRun(app.ID, queued.ID)
	// Claimed but never started (2026-11-04 bug report §6): the app never ran,
	// so no stage — but its process may be mid-spawn, so it is still stopped.
	claimed := &database.JobRun{AppID: app.ID, JobName: "third"}
	db.EnqueueJobRun(claimed, 0)
	db.ClaimJobRuns("node-a", 10, 100)
	if _, wasRunning, _, _ := db.CancelJobRun(app.ID, claimed.ID); !wasRunning {
		t.Error("cancelled claimed: not reported as needing a stop")
	}
	for name, r := range map[string]*database.JobRun{"exited 1": exited, "cancelled queued": queued, "cancelled claimed": claimed} {
		if got := stage(r); got != "" {
			t.Errorf("%s: failure_stage = %q, want none", name, got)
		}
	}
}

// A progress message is clipped on a character and cleaned of NUL, never cut
// mid-sequence: Postgres refused the byte-slice cut and the progress call
// answered 500 inside the job (code review 2026-11-11, H-2).
func TestProgressMessageIsClippedOnACharacter(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "progress")
	def := seedJobDef(t, db, app.ID, "nightly", "")
	run := &database.JobRun{DefinitionID: def.ID, AppID: app.ID, JobName: "nightly"}
	if err := db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("処理中…", 60) // 3-byte characters, well over 200 bytes
	if err := db.SetJobRunProgress(run.ID, 40, long); err != nil {
		t.Fatalf("a long multi-byte message: %v", err)
	}
	if err := db.SetJobRunProgress(run.ID, 41, "a\x00b"); err != nil {
		t.Fatalf("a NUL in the message: %v", err)
	}
}

// The drop notice shares the seq of the line before it, and the log is read
// in (seq, id) order: at the next seq it took the number of the next real
// line, which a follower that had read the notice then skipped (code review
// 2026-11-11, H-8).
func TestANoticeAtItsLinesSeqIsReadAfterItAndBeforeTheNext(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "noisy")
	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)
	if err := db.AppendJobRunLogs(r.ID, []database.JobLogLine{
		{Seq: 5, Stream: "stdout", Line: "line five"},
		{Seq: 5, Stream: "stderr", Line: "… 200 line(s) dropped"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendJobRunLogs(r.ID, []database.JobLogLine{{Seq: 6, Stream: "stdout", Line: "line six"}}); err != nil {
		t.Fatal(err)
	}
	all, _ := db.GetJobRunLogs(r.ID, 0, 100)
	if len(all) != 3 || all[0].Line != "line five" || all[1].Line != "… 200 line(s) dropped" || all[2].Line != "line six" {
		t.Fatalf("order = %+v", all)
	}
	// A follower that read through the notice (after = 5) sees the next line.
	tail, _ := db.GetJobRunLogs(r.ID, 5, 100)
	if len(tail) != 1 || tail[0].Seq != 6 {
		t.Errorf("tail from 5 = %+v, want line six alone", tail)
	}
}

// A full page never ends between a line and the drop notice that shares its
// seq: every client advances `after` to the last seq it was given, so a notice
// cut off by the limit could never be read again (DB-3).
func TestANoticeCutByThePageLimitIsStillRead(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "flood")
	r := &database.JobRun{AppID: app.ID, JobName: "report"}
	db.EnqueueJobRun(r, 0)
	if err := db.AppendJobRunLogs(r.ID, []database.JobLogLine{
		{Seq: 1, Stream: "stdout", Line: "one"},
		{Seq: 2, Stream: "stdout", Line: "two"},
		{Seq: 3, Stream: "stdout", Line: "three"},
		{Seq: 3, Stream: "stderr", Line: "… 9 line(s) dropped"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendJobRunLogs(r.ID, []database.JobLogLine{{Seq: 4, Stream: "stdout", Line: "four"}}); err != nil {
		t.Fatal(err)
	}
	page, err := db.GetJobRunLogs(r.ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 4 || page[2].Line != "three" || page[3].Line != "… 9 line(s) dropped" {
		t.Fatalf("page = %+v, want three lines and the notice", page)
	}
	next, _ := db.GetJobRunLogs(r.ID, 3, 3)
	if len(next) != 1 || next[0].Line != "four" {
		t.Errorf("next page = %+v, want line four alone", next)
	}
}

// A timeout or a cancel of a run with a workload records the stop it owes, so
// one that fails at the backend is asked again; a queued run owes nothing
// (RT-2). The marker clears, and an orphaned start can be marked by handle.
func TestATerminalRunWithAWorkloadOwesAStop(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")

	owed := func() map[string]string {
		t.Helper()
		rows, err := db.ListStopOwedJobRuns(100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range rows {
			out[r.ID] = r.ExternalID
		}
		return out
	}

	queued := &database.JobRun{AppID: app.ID, JobName: "queued"}
	if err := db.EnqueueJobRun(queued, 0); err != nil {
		t.Fatal(err)
	}
	timed := &database.JobRun{AppID: app.ID, JobName: "timed"}
	cancelled := &database.JobRun{AppID: app.ID, JobName: "cancelled"}
	orphan := &database.JobRun{AppID: app.ID, JobName: "orphan"}
	for _, r := range []*database.JobRun{timed, cancelled, orphan} {
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ClaimJobRuns("node-a", 10, 100); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*database.JobRun{timed, cancelled} {
		if err := db.MarkJobRunStarted(r.ID, "node-a", "job-"+r.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, _, err := db.CancelJobRun(app.ID, queued.ID); err != nil {
		t.Fatal(err)
	}
	if moved, err := db.TimeOutJobRun(timed.ID, "timed out"); err != nil || !moved {
		t.Fatalf("time out: %v %v", moved, err)
	}
	if _, _, _, err := db.CancelJobRun(app.ID, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	// The orphan: its start was refused, so it has no handle yet; a cancel made
	// it terminal while the backend was starting it.
	if _, _, _, err := db.CancelJobRun(app.ID, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkJobRunStopOwed(orphan.ID, "job-orphan"); err != nil {
		t.Fatal(err)
	}

	got := owed()
	if _, ok := got[queued.ID]; ok {
		t.Error("a run cancelled in the queue owes no stop")
	}
	if got[timed.ID] != "job-"+timed.ID || got[cancelled.ID] != "job-"+cancelled.ID {
		t.Errorf("owed = %v, want the timed-out and cancelled runs by their handles", got)
	}
	if got[orphan.ID] != "job-orphan" {
		t.Errorf("the orphaned start was not marked by its handle: %v", got)
	}

	if err := db.ClearStopOwed(timed.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := owed()[timed.ID]; ok {
		t.Error("ClearStopOwed left the marker")
	}
}

// A node's own unstarted claims go back to the queue; another node's, and a
// claim with a workload, are left alone (RT-7).
func TestReleaseClaimsOfReturnsOnlyThisNodesUnstartedClaims(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	mine := &database.JobRun{AppID: app.ID, JobName: "mine"}
	started := &database.JobRun{AppID: app.ID, JobName: "started"}
	theirs := &database.JobRun{AppID: app.ID, JobName: "theirs"}
	for _, r := range []*database.JobRun{mine, started} {
		if err := db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ClaimJobRuns("node-a", 10, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueJobRun(theirs, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJobRuns("node-b", 10, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkJobRunStarted(started.ID, "node-a", "job-started", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	n, err := db.ReleaseClaimsOf("node-a")
	if err != nil || n != 1 {
		t.Fatalf("released %d (%v), want 1", n, err)
	}
	for id, want := range map[string]string{mine.ID: database.JobRunQueued,
		started.ID: database.JobRunRunning, theirs.ID: database.JobRunClaimed} {
		if got, _ := db.GetJobRun(id); got.Status != want {
			t.Errorf("%s = %q, want %q", id, got.Status, want)
		}
	}
}

// The jobs twin: a re-render's flip keeps a paused job paused (RT-11).
func TestAReRenderKeepsAPausedJob(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	manifest := []database.JobDefinition{
		{Name: "nightly", Command: "python nightly.py", Schedule: "0 3 * * *"},
	}
	if err := db.ReconcileManifestJobs(app.ID, manifest); err != nil {
		t.Fatal(err)
	}
	nightly, _ := db.GetJobDefinitionByName(app.ID, "nightly")
	if err := db.SetJobEnabled(app.ID, nightly.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Hour).UTC()
	manifest[0].NextRunAt = &next
	if err := db.ReconcileManifestJobsKeeping(app.ID, manifest, true); err != nil {
		t.Fatal(err)
	}
	if after, _ := db.GetJobDefinitionByName(app.ID, "nightly"); after.Enabled || after.NextRunAt != nil {
		t.Errorf("after a re-render: enabled %v, next run %v — want still paused", after.Enabled, after.NextRunAt)
	}
	if err := db.ReconcileManifestJobsKeeping(app.ID, manifest, false); err != nil {
		t.Fatal(err)
	}
	if after, _ := db.GetJobDefinitionByName(app.ID, "nightly"); !after.Enabled || after.NextRunAt == nil {
		t.Errorf("after a person's deploy: enabled %v, next run %v — want resumed", after.Enabled, after.NextRunAt)
	}
}

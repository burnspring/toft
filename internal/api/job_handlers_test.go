package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/schedule"
)

// jobReq issues a request through the real router as the named user.
//
// Distinct from the package's sendAs because these routes are writes and the
// origin check applies to them; sending without an Origin header would test a
// path a browser never takes.
func jobReq(t *testing.T, srv *Server, router http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

type jobEnv struct {
	srv    *Server
	router http.Handler
	app    *database.App
}

func newJobEnv(t *testing.T) *jobEnv {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "viewer", database.SystemRoleViewer)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	app := &database.App{
		Slug: "demo", Name: "Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return &jobEnv{srv: srv, router: srv.Router(http.NotFoundHandler()), app: app}
}

// createJob declares a job the way a deploy does: the app's toft.toml now holds
// exactly the jobs created so far plus this one. body is the JSON shape of a
// definition — name, command, schedule, params_spec, timeout_sec.
func (e *jobEnv) createJob(t *testing.T, body string) *database.JobDefinition {
	t.Helper()
	var d database.JobDefinition
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("job body: %v", err)
	}
	defs, err := e.srv.db.ListJobDefinitions(e.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.db.ReconcileManifestJobs(e.app.ID, append(defs, d)); err != nil {
		t.Fatalf("declaring %s: %v", d.Name, err)
	}
	got, err := e.srv.db.GetJobDefinitionByName(e.app.ID, d.Name)
	if err != nil || got == nil {
		t.Fatalf("reading %s back: %v", d.Name, err)
	}
	return got
}

// --- Definitions come from toft.toml ---

// Nothing creates, edits or deletes a job through the API: a definition is
// declared in toft.toml and reconciled by a deploy. A PATCH may only pause or
// resume a schedule, and says so when asked for anything else.
func TestJobsAreNotDefinedThroughTheAPI(t *testing.T) {
	e := newJobEnv(t)
	def := e.createJob(t, `{"name":"nightly","command":"a","schedule":"0 3 * * *"}`)
	base := "/api/v1/apps/" + e.app.ID + "/jobs"

	if rec := jobReq(t, e.srv, e.router, "owner", http.MethodPost, base, `{"name":"x","command":"y"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /jobs = %d, want 405", rec.Code)
	}
	if rec := jobReq(t, e.srv, e.router, "owner", http.MethodDelete, base+"/"+def.ID, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /jobs/{id} = %d, want 405", rec.Code)
	}
	rec := jobReq(t, e.srv, e.router, "owner", http.MethodPatch, base+"/"+def.ID, `{"command":"rm -rf /"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "toft.toml") {
		t.Errorf("PATCH with a command = %d %s, want 400 naming toft.toml", rec.Code, rec.Body)
	}
	if got, _ := e.srv.db.GetJobDefinition(e.app.ID, def.ID); got.Command != "a" {
		t.Errorf("the command changed: %q", got.Command)
	}
}

// Pausing a schedule is the one thing the platform changes on a definition, it
// is audited, and resuming never fires a window that passed while paused.
func TestPausingAndResumingASchedule(t *testing.T) {
	e := newJobEnv(t)
	def := e.createJob(t, `{"name":"nightly","command":"a","schedule":"0 3 * * *"}`)
	path := "/api/v1/apps/" + e.app.ID + "/jobs/" + def.ID
	before, _ := e.srv.db.CountAuditEntries()

	if rec := jobReq(t, e.srv, e.router, "owner", http.MethodPatch, path, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body)
	}
	paused, _ := e.srv.db.GetJobDefinition(e.app.ID, def.ID)
	if paused.Enabled || paused.NextRunAt != nil {
		t.Errorf("paused = %+v, want disabled with no next run", paused)
	}
	list := jobReq(t, e.srv, e.router, "owner", http.MethodGet, "/api/v1/apps/"+e.app.ID+"/jobs", "")
	if !strings.Contains(list.Body.String(), `"paused":true`) {
		t.Errorf("the list does not say it is paused: %s", list.Body)
	}

	if rec := jobReq(t, e.srv, e.router, "owner", http.MethodPatch, path, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body)
	}
	resumed, _ := e.srv.db.GetJobDefinition(e.app.ID, def.ID)
	if !resumed.Enabled || resumed.NextRunAt == nil || !resumed.NextRunAt.After(time.Now()) {
		t.Errorf("resumed = %+v, want enabled with a next run in the future", resumed)
	}
	if after, _ := e.srv.db.CountAuditEntries(); after < before+2 {
		t.Errorf("pause and resume wrote %d audit entries, want 2", after-before)
	}
	if rec := jobReq(t, e.srv, e.router, "viewer", http.MethodPatch, path, `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer pause = %d, want 403", rec.Code)
	}
}

// A locked app's schedule is held, and the list says so: held_by_lock and no
// next_run_at, because the scheduler skips every firing until the lock is
// lifted — the CLI's "held by the app's lock" in JSON (drills D15, 2026-10-28).
func TestTheJobListSaysALockedAppsScheduleIsHeld(t *testing.T) {
	e := newJobEnv(t)
	e.createJob(t, `{"name":"nightly","command":"a","schedule":"0 3 * * *"}`)
	e.createJob(t, `{"name":"adhoc","command":"b"}`)
	admin := seedUser(t, e.srv.db, "locking-admin", database.SystemRoleAdmin)
	if err := e.srv.db.SetAppLock(e.app.ID, admin.ID, "Held for review", time.Now().UTC(), e.app.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("locking the app: %v", err)
	}

	rec := jobReq(t, e.srv, e.router, "owner", http.MethodGet, "/api/v1/apps/"+e.app.ID+"/jobs", "")
	var body struct {
		Jobs []map[string]any `json:"jobs"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	got := map[string]map[string]any{}
	for _, j := range body.Jobs {
		got[j["name"].(string)] = j
	}
	if n := got["nightly"]; n == nil || n["held_by_lock"] != true || n["next_run_at"] != nil {
		t.Errorf("a locked app's schedule = %v, want held_by_lock and no next_run_at", n)
	}
	if a := got["adhoc"]; a == nil || a["held_by_lock"] != false {
		t.Errorf("an on-demand job = %v, want held_by_lock false: no schedule to hold", a)
	}
}

// A person starts a job here as an owner or editor. A viewer starts one through
// the app's own button, never on the platform's route.
func TestViewersCannotStartJobsOnThePlatformRoute(t *testing.T) {
	e := newJobEnv(t)
	e.createJob(t, `{"name":"refresh","command":"a"}`)
	for _, path := range []string{"/job-runs", "/jobs/refresh/runs"} {
		rec := jobReq(t, e.srv, e.router, "viewer", http.MethodPost,
			"/api/v1/apps/"+e.app.ID+path, `{"job":"refresh"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d, want 403", path, rec.Code)
		}
	}
}

// The list carries each job's effective limits with their source: what a
// developer needs before a run is stopped by a limit they did not know about.
func TestTheJobListCarriesEffectiveLimits(t *testing.T) {
	e := newJobEnv(t)
	e.createJob(t, `{"name":"declared","command":"a","timeout_sec":1800}`)
	e.createJob(t, `{"name":"undeclared","command":"b"}`)

	rec := jobReq(t, e.srv, e.router, "owner", http.MethodGet, "/api/v1/apps/"+e.app.ID+"/jobs", "")
	var body struct {
		Jobs []struct {
			Name   string `json:"name"`
			Limits struct {
				Timeout struct {
					Value  int    `json:"value"`
					Source string `json:"source"`
				} `json:"timeout"`
			} `json:"limits"`
		} `json:"jobs"`
		MaxQueued int `json:"max_queued"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	got := map[string]string{}
	for _, j := range body.Jobs {
		got[j.Name] = j.Limits.Timeout.Source
	}
	if got["declared"] != "declared" || got["undeclared"] != "install default" {
		t.Errorf("sources = %v", got)
	}
	if body.MaxQueued != 50 {
		t.Errorf("max_queued = %d, want the default 50", body.MaxQueued)
	}
}

// An app a caller cannot read must 404, not 403: a 403 confirms it exists.
func TestJobsOnAnUnreadableAppAre404(t *testing.T) {
	e := newJobEnv(t)
	rungOff(t, e.srv)
	// Make the app private so a stranger cannot read it.
	if err := e.srv.db.UpdateApp(&database.App{
		ID: e.app.ID, Slug: e.app.Slug, Name: e.app.Name,
		Status: e.app.Status, Visibility: database.VisibilityPrivate,
	}); err != nil {
		t.Fatal(err)
	}

	rec := jobReq(t, e.srv, e.router, "stranger", http.MethodGet, "/api/v1/apps/"+e.app.ID+"/jobs", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a 403 would confirm the app exists", rec.Code)
	}
}

// A stranger cannot learn which jobs an app declares by triggering names: a
// real name and a made-up one get the same refusal. The lookup used to come
// before authorization, so a made-up name answered `no_such_job` and a real one
// the authorization refusal (docs/bugs 2026-09-04-api-review #27).
func TestTriggeringAJobDoesNotTellAStrangerWhichJobsExist(t *testing.T) {
	e := newJobEnv(t)
	rungOff(t, e.srv)
	e.createJob(t, `{"name":"export","command":"echo hi"}`)
	if err := e.srv.db.UpdateApp(&database.App{
		ID: e.app.ID, Slug: e.app.Slug, Name: e.app.Name,
		Status: e.app.Status, Visibility: database.VisibilityPrivate,
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/apps/" + e.app.ID + "/job-runs"
	real := jobReq(t, e.srv, e.router, "stranger", http.MethodPost, path, `{"job":"export"}`)
	madeUp := jobReq(t, e.srv, e.router, "stranger", http.MethodPost, path, `{"job":"nonsense"}`)
	if real.Code != madeUp.Code || strings.Contains(madeUp.Body.String(), "no_such_job") {
		t.Errorf("a stranger told the names apart: real %d %s, made-up %d %s",
			real.Code, real.Body.String(), madeUp.Code, madeUp.Body.String())
	}
	if madeUp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a 403 would confirm the app exists", madeUp.Code)
	}
}

// An install with nowhere to place a workload refuses, and names the real
// blocker. What is wrong is a decision somebody has to make, and a generic
// failure does not tell them there is one.
//
// The app's own execution mode is deliberately not part of this any more. A run
// is an isolated workload whichever mode its app runs in, so a shared-mode app
// is refused here for the same reason an isolated one would be — this install
// has no cluster — and for no reason of its own.
func TestTriggeringExplainsAnInstallThatCannotRunJobs(t *testing.T) {
	e := newJobEnv(t)
	e.createJob(t, `{"name":"nightly","command":"a"}`)

	rec := jobReq(t, e.srv, e.router, "owner", http.MethodPost,
		"/api/v1/apps/"+e.app.ID+"/job-runs", `{"job":"nightly"}`)

	// 422, not 400: the request is well-formed; what is wrong is a decision.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
	}
	// The app surface's code for the same condition, never the envelope's
	// generic `invalid` — which is what a malformed request gets.
	if !strings.Contains(rec.Body.String(), `"code":"jobs_unavailable"`) {
		t.Errorf("the refusal does not carry jobs_unavailable: %s", rec.Body.String())
	}
	// On this VM install the blocker is the missing cluster, not a settings
	// toggle — pointing at a switch the operator cannot usefully throw would
	// send them somewhere pointless.
	body := rec.Body.String()
	for _, want := range []string{"cluster", "single machine"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal should mention %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Admin → Settings") {
		t.Errorf("a VM install has no switch to throw; the message should not suggest one: %s", body)
	}
	// And it must not tell the developer to promote their app, which would now
	// be advice that changes nothing.
	if strings.Contains(body, "Promote") {
		t.Errorf("the app's mode is not why this failed: %s", body)
	}
}

func TestTriggeringAnUnknownJobIs404(t *testing.T) {
	e := newJobEnv(t)
	rec := jobReq(t, e.srv, e.router, "owner", http.MethodPost,
		"/api/v1/apps/"+e.app.ID+"/job-runs", `{"job":"no-such-job"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// Anything taking an ID from the URL must scope its query by the app it
// authorized against. This is the IDOR shape, and it keeps recurring.
func TestRunLookupsAreScopedToTheirApp(t *testing.T) {
	e := newJobEnv(t)

	other := &database.App{
		Slug: "theirs", Name: "Theirs", OwnerID: "stranger",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := e.srv.db.CreateAppTx(other, "", "stranger", "", "", ""); err != nil {
		t.Fatal(err)
	}
	secret := &database.JobRun{AppID: other.ID, JobName: "secret-report"}
	if err := e.srv.db.EnqueueJobRun(secret, 0); err != nil {
		t.Fatal(err)
	}

	// Reaching the other app's run *through this app's path* must 404, even
	// though the caller legitimately owns this app.
	for _, path := range []string{
		"/api/v1/apps/" + e.app.ID + "/job-runs/" + secret.ID,
		"/api/v1/apps/" + e.app.ID + "/job-runs/" + secret.ID + "/logs",
		"/api/v1/apps/" + e.app.ID + "/job-runs/" + secret.ID[:8],
		"/api/v1/apps/" + e.app.ID + "/job-runs/" + secret.ID + "/output",
	} {
		rec := jobReq(t, e.srv, e.router, "owner", http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 — another app's run leaked", path, rec.Code)
		}
	}

	rec := jobReq(t, e.srv, e.router, "owner", http.MethodPost,
		"/api/v1/apps/"+e.app.ID+"/job-runs/"+secret.ID+"/cancel", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cancel = %d, want 404 — another app's run was cancellable", rec.Code)
	}
	// And it really was not cancelled.
	if got, _ := e.srv.db.GetJobRun(secret.ID); got.Status == database.JobRunCancelled {
		t.Error("another app's run was cancelled through this app")
	}
}

// Run history, params, actors and logs are workload output: the log audience
// (editors), not the read audience.
func TestViewersCannotReadRunHistory(t *testing.T) {
	e := newJobEnv(t)
	run := &database.JobRun{AppID: e.app.ID, JobName: "nightly"}
	if err := e.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/job-runs", "/job-runs/" + run.ID, "/job-runs/" + run.ID + "/logs", "/jobs"} {
		rec := jobReq(t, e.srv, e.router, "viewer", http.MethodGet, "/api/v1/apps/"+e.app.ID+path, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer GET %s = %d, want 403", path, rec.Code)
		}
	}
	rec := jobReq(t, e.srv, e.router, "owner", http.MethodGet, "/api/v1/apps/"+e.app.ID+"/job-runs", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), run.ID) {
		t.Errorf("owner history = %d, want 200 with the run", rec.Code)
	}
}

// Cancelling is an edit-content action even though reading is not.
func TestViewersCannotCancelRuns(t *testing.T) {
	e := newJobEnv(t)
	run := &database.JobRun{AppID: e.app.ID, JobName: "nightly"}
	e.srv.db.EnqueueJobRun(run, 0)

	rec := jobReq(t, e.srv, e.router, "viewer", http.MethodPost,
		"/api/v1/apps/"+e.app.ID+"/job-runs/"+run.ID+"/cancel", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The listing carries why jobs are unavailable, so the UI explains rather than
// showing an empty page that looks broken.
func TestTheJobListSaysWhyJobsAreUnavailable(t *testing.T) {
	e := newJobEnv(t)

	rec := jobReq(t, e.srv, e.router, "owner", http.MethodGet, "/api/v1/apps/"+e.app.ID+"/jobs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		JobsAvailable bool   `json:"jobs_available"`
		Why           string `json:"unavailable_bec"`
		ExecutionMode string `json:"execution_mode"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	if body.JobsAvailable {
		t.Error("jobs should be unavailable on a VM install")
	}
	if body.Why == "" {
		t.Error("the list should carry the reason")
	}
	if body.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode = %q", body.ExecutionMode)
	}
}

// Every job schedule appears on the estate-wide list, and only scheduled jobs.
func TestTheScheduledOverviewListsJobSchedules(t *testing.T) {
	e := newJobEnv(t)
	e.createJob(t, `{"name":"nightly","command":"a","schedule":"0 3 * * *"}`)
	e.createJob(t, `{"name":"on-demand","command":"b"}`)

	rec := jobReq(t, e.srv, e.router, "root", http.MethodGet, "/api/v1/system/scheduled", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Jobs []database.ScheduledJob `json:"jobs"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	// Only the scheduled one: a job that is triggered and never fires by itself
	// has no answer to "when", so it belongs to the run list.
	if len(body.Jobs) != 1 || body.Jobs[0].Name != "nightly" {
		t.Errorf("jobs = %+v, want only the scheduled one", body.Jobs)
	}
	if body.Jobs[0].AppName != e.app.Name {
		t.Errorf("the row should name its app: %+v", body.Jobs[0])
	}
}

// A re-render is a schedule on the install too, so the list carries it.
func TestTheScheduledOverviewListsReRenders(t *testing.T) {
	e := newJobEnv(t)
	next, _ := schedule.Next("0 6 * * 1", time.Now())
	if _, err := e.srv.db.ReconcileManifestSchedules(e.app.ID, nil,
		&database.ManifestSchedule{Schedule: "0 6 * * 1", NextRunAt: &next}); err != nil {
		t.Fatal(err)
	}
	rec := jobReq(t, e.srv, e.router, "root", http.MethodGet, "/api/v1/system/scheduled", "")
	var body struct {
		Renders []database.EstateSchedule `json:"renders"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Renders) != 1 || body.Renders[0].AppName != e.app.Name || body.Renders[0].Schedule != "0 6 * * 1" {
		t.Fatalf("renders = %+v", body.Renders)
	}
	if body.Renders[0].LastRunAt != nil {
		t.Errorf("a schedule that never fired has no last run: %+v", body.Renders[0])
	}
}

// A cancel answers with the run as it now stands and whether this call changed
// it. A run already finished is not reported as cancelled.
func TestCancelSaysWhetherItChangedAnything(t *testing.T) {
	e := newJobEnv(t)
	run := &database.JobRun{AppID: e.app.ID, JobName: "nightly"}
	e.srv.db.EnqueueJobRun(run, 0)
	path := "/api/v1/apps/" + e.app.ID + "/job-runs/" + run.ID + "/cancel"

	var body struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	rec := jobReq(t, e.srv, e.router, "owner", http.MethodPost, path, "")
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body.Status != database.JobRunCancelled || !body.Changed {
		t.Fatalf("first cancel = %d %s", rec.Code, rec.Body)
	}

	body.Changed = true
	rec = jobReq(t, e.srv, e.router, "owner", http.MethodPost, path, "")
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body.Changed {
		t.Errorf("cancelling a finished run = %d %s, want 200 and changed:false", rec.Code, rec.Body)
	}
}

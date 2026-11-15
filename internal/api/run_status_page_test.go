package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The status page answers a script with JSON and a browser with a page, and
// shows a run started in someone's name to that person and the app's editors
// only — anyone else gets the 404 a run that does not exist gets.
func TestARunsStatusPageIsForTheRightPeople(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "priya", database.SystemRoleViewer)
	seedUser(t, srv.db, "sam", database.SystemRoleViewer)
	app := &database.App{Slug: "reports", Name: "Reports", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mine := &database.JobRun{AppID: app.ID, JobName: "nightly", StartedBy: database.JobStartedByViewer, ActorID: "priya"}
	apps := &database.JobRun{AppID: app.ID, JobName: "nightly", StartedBy: database.JobStartedByApp}
	for _, r := range []*database.JobRun{mine, apps} {
		if err := srv.db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
	}
	principal := func(id string) *auth.Principal {
		p, err := srv.db.PrincipalForUser(id)
		if err != nil || p == nil {
			t.Fatalf("principal %s: %v", id, err)
		}
		return p
	}
	get := func(who, runID, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, runStatusPath(app, runID), nil)
		req.Header.Set("Accept", accept)
		rec := httptest.NewRecorder()
		srv.serveRunStatus(rec, req, app, principal(who), runID)
		return rec
	}

	for who, want := range map[string]int{"priya": 200, "owner": 200, "sam": 404} {
		if rec := get(who, mine.ID, "application/json"); rec.Code != want {
			t.Errorf("%s reading priya's run: %d, want %d", who, rec.Code, want)
		}
	}
	// The platform's answer says so, found or not (BB-014, second
	// 2026-11-03 bash).
	for _, id := range []string{apps.ID, "00000000-0000-0000-0000-000000000000"} {
		if rec := get("sam", id, "application/json"); rec.Header().Get(interstitialHeader) != "run_page" {
			t.Errorf("run %s: X-Toft-Interstitial %q", id, rec.Header().Get(interstitialHeader))
		}
	}
	if rec := get("sam", apps.ID, "application/json"); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Errorf("an app-started run for a reader of the app: %d %s", rec.Code, rec.Body)
	}
	page := get("priya", mine.ID, "text/html,application/xhtml+xml")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "<title>nightly · Reports</title>") ||
		!strings.Contains(page.Header().Get("Content-Type"), "text/html") {
		t.Errorf("the page: %d %q", page.Code, page.Body.String()[:120])
	}
	// Its own policy, kept; the two headers every gateway page carries, added.
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("the page's own policy is gone: %q", csp)
	}
	if got := page.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := page.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q", got)
	}

	// The "Back to" link follows the prefix the request arrived on: the root
	// on the app's own hostname, /apps/{slug}/ on the shared origin (HD-8).
	if !strings.Contains(page.Body.String(), `href="/apps/reports/"`) {
		t.Errorf("on the shared origin the back link is not /apps/reports/")
	}
	for prefix, want := range map[string]string{"": `href="/"`, "/apps/reports": `href="/apps/reports/"`} {
		req := httptest.NewRequest(http.MethodGet, prefix+runStatusSegment+mine.ID, nil)
		req.Header.Set("Accept", "text/html")
		req = req.WithContext(context.WithValue(req.Context(), urlPrefixKey{}, prefix))
		rec := httptest.NewRecorder()
		srv.serveRunStatus(rec, req, app, principal("priya"), mine.ID)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("arrived on prefix %q: the back link is not %s", prefix, want)
		}
	}

	if id, ok := runStatusRequest("/apps/reports/__toft/runs/"+mine.ID, "/apps/reports"); !ok || id != mine.ID {
		t.Errorf("runStatusRequest = %q %v", id, ok)
	}
	if _, ok := runStatusRequest("/apps/reports/__toft/runs/x/y", "/apps/reports"); ok {
		t.Error("a nested path was taken for a run")
	}
}

// BB-110: only an app-started run is open to the app's readers. A schedule's
// run and a deploy key's (cli, no actor) are the editors' — not every viewer's,
// and never a public app's anonymous visitor's.
func TestARunNoPersonStartedIsTheEditorsUnlessTheAppStartedIt(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "sam", database.SystemRoleViewer)
	app := &database.App{Slug: "public-runs", Name: "Public", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	runs := map[string]*database.JobRun{}
	for _, by := range []string{database.JobStartedByApp, database.JobStartedBySchedule, database.JobStartedByCLI} {
		r := &database.JobRun{AppID: app.ID, JobName: "nightly", StartedBy: by}
		if err := srv.db.EnqueueJobRun(r, 0); err != nil {
			t.Fatal(err)
		}
		runs[by] = r
	}
	get := func(who string, runID string) int {
		var p *auth.Principal
		if who != "" {
			var err error
			if p, err = srv.db.PrincipalForUser(who); err != nil || p == nil {
				t.Fatalf("principal %s: %v", who, err)
			}
		}
		req := httptest.NewRequest(http.MethodGet, runStatusPath(app, runID), nil)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		srv.serveRunStatus(rec, req, app, p, runID)
		return rec.Code
	}
	for by, run := range runs {
		open := by == database.JobStartedByApp
		for _, who := range []string{"sam", ""} {
			want := http.StatusNotFound
			if open {
				want = http.StatusOK
			}
			if got := get(who, run.ID); got != want {
				t.Errorf("started_by %s, reader %q: %d, want %d", by, who, got, want)
			}
		}
		if got := get("owner", run.ID); got != http.StatusOK {
			t.Errorf("started_by %s, owner: %d, want 200", by, got)
		}
	}
}

// The gateway answers the status page on every credential path, never the
// app: a personal key and an app key are admitted to the app exactly as a
// session is, and before this only the session path was intercepted, so
// `toft fetch` and a script following respond's 303 got the app's own 404.
func TestTheGatewayServesTheStatusPageToKeysToo(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	enablePersonalKeys(t, srv, true)
	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{ID: "app-runs", Slug: "runs", Name: "Runs", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	run := &database.JobRun{AppID: app.ID, JobName: "nightly", StartedBy: database.JobStartedByApp}
	if err := srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	personal := mintKey(t, srv, "owner", "ci", nil, false)
	router := srv.AppsRouter()

	for name, secret := range map[string]string{"personal key": personal} {
		req := httptest.NewRequest(http.MethodGet, runStatusPath(app, run.ID), nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"job":"nightly"`) {
			t.Errorf("%s: %d %s, want the gateway's JSON for the run", name, rec.Code, rec.Body)
		}
	}
}

// A batch app's address is not a dead end: it lists each job with its
// schedule, next run and last outcome, and no controls.
func TestABatchAppsCardListsItsJobs(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.renderInterstitialWith(rec, "Extract", "app-1", "", stateBatch, "This app runs jobs and serves no pages.",
		false, []batchJobRow{{Name: "nightly", Schedule: "0 3 * * * (Europe/London)", Next: "Fri 23 Oct 03:00 BST",
			Last: "failed · 22 Oct 03:12 BST", LastTone: "danger"}})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the card is the address's answer, not a stand-in (BB-6)", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"nightly", "0 3 * * * (Europe/London)", "Fri 23 Oct 03:00 BST",
		`class="last tone-danger"`, "Open in Toft"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card does not carry %q", want)
		}
	}
	if strings.Contains(body, "<button") || strings.Contains(body, "<form") {
		t.Error("the card offers a control; it is read-only")
	}
}

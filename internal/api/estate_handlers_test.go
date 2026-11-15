package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
)

// The census route, and the fold underneath it.
//
// The database's own tests prove the join; these prove the vocabulary — that an
// app with no recorded language still gets one, that a patch lands in its
// series, that the two wordless states stay apart, and that nobody but an
// administrator sees any of it.

type estateFixture struct {
	srv    *Server
	router http.Handler
}

func newEstateFixture(t *testing.T) *estateFixture {
	t.Helper()
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)
	return &estateFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
}

// estateApp is one deployed app: a framework, a running mode, and what its
// serving release resolved.
func (f *estateFixture) estateApp(t *testing.T, slug, framework, mode, language, version string) *database.App {
	t.Helper()
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "owner",
		Framework: framework, RunningMode: mode,
		Status:     database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if language == "" && version == "" && framework == "static" {
		return app
	}
	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusPending}
	if err := f.srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now().UTC()
	d.Status, d.FinishedAt = database.DeployStatusSuccess, &finished
	if err := f.srv.db.UpdateDeployment(d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if version != "" || language != "" {
		if err := f.srv.db.SetDeploymentRuntime(d.ID, language, version); err != nil {
			t.Fatalf("SetDeploymentRuntime: %v", err)
		}
	}
	return app
}

func (f *estateFixture) get(t *testing.T, who string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/estate/languages", nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, f.srv, who))
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *estateFixture) census(t *testing.T, who string) estateResponse {
	t.Helper()
	rec := f.get(t, who)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out estateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return out
}

func languageRow(t *testing.T, out estateResponse, language string) estateLanguage {
	t.Helper()
	for _, l := range out.Languages {
		if l.Language == language {
			return l
		}
	}
	t.Fatalf("no %q row in %+v", language, out.Languages)
	return estateLanguage{}
}

// Patch never matters: 3.13.2 and 3.13.4 are one series and one row, because a
// census split three ways is not the count an end-of-life decision needs.
func TestEstateGroupsBySeriesAndNeverByPatch(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "reports", "flask", "shared", "python", "3.13.2")
	f.estateApp(t, "api", "fastapi", "isolated", "python", "3.13.4")
	f.estateApp(t, "legacy", "flask", "shared", "python", "3.12.7")

	out := f.census(t, "admin1")
	py := languageRow(t, out, "python")
	if py.Apps != 3 {
		t.Errorf("python apps = %d, want 3", py.Apps)
	}
	if len(py.Series) != 2 {
		t.Fatalf("got %d series rows, want 2 (3.13 and 3.12): %+v", len(py.Series), py.Series)
	}
	// Newest first.
	if py.Series[0].Series != "3.13" || py.Series[0].Apps != 2 {
		t.Errorf("first series = %+v, want 3.13 with 2 apps", py.Series[0])
	}
	if py.Series[1].Series != "3.12" || py.Series[1].Apps != 1 {
		t.Errorf("second series = %+v, want 3.12 with 1 app", py.Series[1])
	}
	// The framework axis is kept, one level down.
	if len(py.Series[0].Frameworks) != 2 {
		t.Errorf("3.13 frameworks = %+v, want flask and fastapi", py.Series[0].Frameworks)
	}
}

// Three causes of an empty version, and the census may not merge them into a
// fourth meaning. A static app is not applicable; a Go app the go command
// settled is unresolved; neither is ever counted as versioned.
func TestEstateTellsNotApplicableFromUnresolved(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "brochure", "static", "shared", "", "")
	f.estateApp(t, "gosvc", "go", "shared", "", "")
	f.estateApp(t, "reports", "flask", "shared", "python", "3.13.4")

	out := f.census(t, "admin1")

	static := languageRow(t, out, "static")
	if len(static.Series) != 1 || static.Series[0].State != estateNotApplicable {
		t.Errorf("static row = %+v, want one not-applicable series", static.Series)
	}
	if static.Label != "Static" {
		t.Errorf("static label = %q", static.Label)
	}

	// A Go app whose build left the version to the go command records no
	// language either, so the language comes from the framework.
	goRow := languageRow(t, out, "go")
	if len(goRow.Series) != 1 || goRow.Series[0].State != estateUnresolved {
		t.Errorf("go row = %+v, want one unresolved series", goRow.Series)
	}
	if goRow.Series[0].Series != "" {
		t.Errorf("an unresolved row carries a series %q; the word is the answer", goRow.Series[0].Series)
	}

	if out.AppsTotal != 3 {
		t.Errorf("apps_total = %d, want 3 — a static app is part of the estate", out.AppsTotal)
	}
	if out.AppsVersioned != 1 {
		t.Errorf("apps_versioned = %d, want 1 — neither empty state is a version", out.AppsVersioned)
	}
}

// An app the detector could not place has no language, and the census says so
// rather than filing it under one.
func TestEstateNamesAnUnknownLanguageRatherThanGuessing(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "mystery", string(builder.FrameworkUnsupported), "shared", "", "")

	out := f.census(t, "admin1")
	unknown := languageRow(t, out, "")
	if unknown.Label != "Unknown" {
		t.Errorf("label = %q, want Unknown", unknown.Label)
	}
	if unknown.Apps != 1 {
		t.Errorf("apps = %d, want 1", unknown.Apps)
	}
}

// The second axis: what is still shared, and what is it written in.
func TestEstateCrossesLanguageWithExecutionMode(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "reports", "flask", "shared", "python", "3.13.4")
	f.estateApp(t, "api", "fastapi", "isolated", "python", "3.13.9")
	f.estateApp(t, "worker", "python", "isolated", "python", "3.13.1")

	out := f.census(t, "admin1")
	py := languageRow(t, out, "python")
	if py.Shared != 1 || py.Isolated != 2 {
		t.Errorf("python shared/isolated = %d/%d, want 1/2", py.Shared, py.Isolated)
	}
	if !out.ShowModes {
		t.Error("show_modes is false on an install with isolated workloads running")
	}
}

// On an install that offers one mode and runs one mode, the columns are absent
// rather than a column of zeroes.
func TestEstateHidesModesWhereTheInstallOffersOne(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "reports", "flask", "shared", "python", "3.13.4")

	out := f.census(t, "admin1")
	if out.ShowModes {
		t.Error("show_modes is true on a shared-only install")
	}
}

// Administrator only, and 404 rather than 403: an estate-wide map of which
// internal tool runs which interpreter version is a target map.
func TestEstateIsAdminOnly(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "reports", "flask", "shared", "python", "3.13.4")

	if rec := f.get(t, "owner"); rec.Code != http.StatusNotFound {
		t.Errorf("a publisher got %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, ""); rec.Code == http.StatusOK {
		t.Errorf("an anonymous caller got the census: %s", rec.Body.String())
	}
	if rec := f.get(t, "admin1"); rec.Code != http.StatusOK {
		t.Errorf("an administrator got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// Nothing deployed is an empty census, not a table of zeroes.
func TestEstateEmptyInstallReturnsNoRows(t *testing.T) {
	f := newEstateFixture(t)
	out := f.census(t, "admin1")
	if len(out.Languages) != 0 {
		t.Errorf("languages = %+v, want none", out.Languages)
	}
	if out.AppsTotal != 0 || out.AppsVersioned != 0 {
		t.Errorf("coverage = %d/%d, want 0/0", out.AppsVersioned, out.AppsTotal)
	}
}

// Every app unversioned is the output the coverage line exists for, and it must
// not look like a broken page: rows render, and the coverage says zero.
func TestEstateRendersWithNoVersionsAtAll(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "gosvc", "go", "shared", "", "")
	f.estateApp(t, "brochure", "static", "shared", "", "")

	out := f.census(t, "admin1")
	if len(out.Languages) != 2 {
		t.Fatalf("languages = %+v, want two rows", out.Languages)
	}
	if out.AppsTotal != 2 || out.AppsVersioned != 0 {
		t.Errorf("coverage = %d of %d, want 0 of 2", out.AppsVersioned, out.AppsTotal)
	}
}

// The apps list carries the release line each app is on, folded by the same
// functions the census uses — and only for an administrator.
//
// A viewer can see more apps than they can edit, so a version on every visible
// row would hand out a partial copy of exactly what the census is gated to
// withhold.
func TestAppsListCarriesTheServingRuntimeForAdminsOnly(t *testing.T) {
	f := newEstateFixture(t)
	f.estateApp(t, "reports", "flask", "shared", "python", "3.13.4")

	type listed struct {
		Slug    string      `json:"slug"`
		Runtime *appRuntime `json:"runtime"`
	}
	list := func(who string) []listed {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
		req.AddCookie(sessionCookieFor(t, f.srv, who))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("listing as %s: status %d: %s", who, rec.Code, rec.Body.String())
		}
		var out []listed
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return out
	}

	admin := list("admin1")
	if len(admin) != 1 || admin[0].Runtime == nil {
		t.Fatalf("an administrator's listing carries no runtime: %+v", admin)
	}
	if admin[0].Runtime.Series != "3.13" || admin[0].Runtime.State != estateResolved {
		t.Errorf("runtime = %+v, want the 3.13 series", admin[0].Runtime)
	}
	if admin[0].Runtime.Label != "Python" {
		t.Errorf("label = %q, want Python", admin[0].Runtime.Label)
	}

	owner := list("owner")
	if len(owner) != 1 {
		t.Fatalf("the owner's listing = %+v", owner)
	}
	if owner[0].Runtime != nil {
		t.Errorf("a publisher's listing carries a runtime version: %+v", owner[0].Runtime)
	}
}

// The third reader: an app's own Settings page, where the editor who owns the
// app is told which interpreter is running their code.
//
// It carries the version in full rather than the series — the census folds
// patch away because a count must, and an owner comparing two of their own
// releases is asking the question that fold erases.
func TestAppDetailCarriesTheServingRuntimeForEditorsOnly(t *testing.T) {
	f := newEstateFixture(t)
	seedUser(t, f.srv.db, "reader", database.SystemRoleViewer)
	app := f.estateApp(t, "reports", "flask", "shared", "python", "3.13.4")

	detail := func(who string) *appRuntime {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID, nil)
		req.AddCookie(sessionCookieFor(t, f.srv, who))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("reading the app as %s: status %d: %s", who, rec.Code, rec.Body.String())
		}
		var out struct {
			Runtime *appRuntime `json:"runtime"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return out.Runtime
	}

	got := detail("owner")
	if got == nil {
		t.Fatal("the owner's own app carries no runtime")
	}
	if got.Version != "3.13.4" {
		t.Errorf("version = %q, want the recorded 3.13.4 rather than a rounded one", got.Version)
	}
	if got.Series != "3.13" || got.State != estateResolved || got.Label != "Python" {
		t.Errorf("runtime = %+v, want the resolved 3.13 series labelled Python", got)
	}

	// A viewer can read this app — it is `authenticated` — and still may not
	// read its version, on the rule the census follows.
	if r := detail("reader"); r != nil {
		t.Errorf("a viewer's read of the app carries a runtime version: %+v", r)
	}
}

// An app that has never been built has no serving release, so there is nothing
// to report — and the words for that are the state's, never a version invented
// from the framework.
func TestAppDetailRuntimeOnANeverDeployedApp(t *testing.T) {
	f := newEstateFixture(t)
	app := &database.App{
		Slug: "fresh", Name: "fresh", OwnerID: "owner",
		Framework: "flask", RunningMode: "shared",
		Status: database.AppStatusStopped, Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID, nil)
	req.AddCookie(sessionCookieFor(t, f.srv, "owner"))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Runtime *appRuntime `json:"runtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Runtime == nil {
		t.Fatal("no runtime at all — the page cannot tell an unread fact from an unbuilt app")
	}
	if out.Runtime.State != estateUnresolved || out.Runtime.Version != "" || out.Runtime.Series != "" {
		t.Errorf("runtime = %+v, want an unresolved row carrying no version", out.Runtime)
	}
}

// The census counts the estate by kind — serving, rendered, batch — and, beside
// them, how many apps of any kind declare a job. The administrator's app list
// carries has_jobs so the census can link to those apps; nobody else's does.
func TestEstateCountsKindsAndAppsWithJobs(t *testing.T) {
	f := newEstateFixture(t)
	web := f.estateApp(t, "web", "flask", "shared", "python", "3.13.4")
	f.estateApp(t, "dash", "shiny", "shared", "r", "4.5.1")
	f.estateApp(t, "site", "static", "shared", "", "")
	f.estateApp(t, "report", "quarto", "shared", "r", "4.5.1")
	nightly := f.estateApp(t, "nightly", "batch-python", "shared", "python", "3.13.4")
	declareJob(t, f.srv.db, web.ID, database.JobDefinition{Name: "refresh", Command: "python refresh.py"})
	declareJob(t, f.srv.db, nightly.ID, database.JobDefinition{Name: "extract", Command: "python main.py"})

	out := f.census(t, "admin1")
	if (out.Kinds != estateKinds{Serving: 2, Rendered: 2, Batch: 1}) {
		t.Errorf("kinds = %+v, want 2 serving, 2 rendered, 1 batch", out.Kinds)
	}
	if out.AppsWithJobs != 2 {
		t.Errorf("apps_with_jobs = %d, want 2", out.AppsWithJobs)
	}

	type listed struct {
		Slug    string `json:"slug"`
		HasJobs bool   `json:"has_jobs"`
	}
	list := func(who string) map[string]bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
		req.AddCookie(sessionCookieFor(t, f.srv, who))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("listing as %s: status %d: %s", who, rec.Code, rec.Body.String())
		}
		var rows []listed
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		m := map[string]bool{}
		for _, r := range rows {
			m[r.Slug] = r.HasJobs
		}
		return m
	}
	admin := list("admin1")
	if !admin["web"] || !admin["nightly"] || admin["dash"] || admin["site"] {
		t.Errorf("admin has_jobs = %+v, want web and nightly only", admin)
	}
	for slug, has := range list("owner") {
		if has {
			t.Errorf("a publisher's listing says %s has jobs", slug)
		}
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The estate's scan targets.
//
// Three properties, and the first is the one an AppSec team's whole run rests
// on: the URL is the install's own answer, not a composition. The second is
// the gate, which is wider than every other admin read here by exactly one
// principal and no more. The third is that the reader never omits an app.

type targetsFixture struct {
	srv    *Server
	router http.Handler
}

func newTargetsFixture(t *testing.T) *targetsFixture {
	t.Helper()
	srv := newTestServerWithConfig(t, dbtest.New(t), &config.Config{
		BaseURL: "https://toft.example",
		// A split-origin install, which is the shape a composed URL would get
		// wrong: apps are not served from BaseURL here.
		AppsBaseURL: "https://apps.example",
		Port:        8080,
		AppsPort:    8081,
	})
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	return &targetsFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
}

// deployed is an app with a serving release, and therefore with an address.
func (f *targetsFixture) deployed(t *testing.T, slug, visibility string) *database.App {
	t.Helper()
	return f.deployedAs(t, slug, visibility, "flask")
}

// deployedAs is deployed with the framework named.
func (f *targetsFixture) deployedAs(t *testing.T, slug, visibility, framework string) *database.App {
	t.Helper()
	app := f.undeployedAs(t, slug, visibility, framework)
	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusPending}
	if err := f.srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now().UTC()
	d.Status, d.FinishedAt = database.DeployStatusSuccess, &finished
	if err := f.srv.db.UpdateDeployment(d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	return app
}

// undeployed is an app row and nothing else — created, never built.
func (f *targetsFixture) undeployed(t *testing.T, slug, visibility string) *database.App {
	t.Helper()
	return f.undeployedAs(t, slug, visibility, "flask")
}

func (f *targetsFixture) undeployedAs(t *testing.T, slug, visibility, framework string) *database.App {
	t.Helper()
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "owner", Framework: framework,
		Status: database.AppStatusRunning, Visibility: visibility,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp %s: %v", slug, err)
	}
	return app
}

func (f *targetsFixture) read(t *testing.T, who string) estateTargetsResponse {
	t.Helper()
	rec := sendAs(t, f.srv, f.router, who, http.MethodGet, "/api/v1/admin/estate/targets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	return decodeTargets(t, rec)
}

func decodeTargets(t *testing.T, rec *httptest.ResponseRecorder) estateTargetsResponse {
	t.Helper()
	var out estateTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v — body %s", err, rec.Body.String())
	}
	return out
}

func findTarget(rows []estateTarget, slug string) *estateTarget {
	for i := range rows {
		if rows[i].Slug == slug {
			return &rows[i]
		}
	}
	return nil
}

// The URL is the apps origin's, never the platform's. On a split-origin
// install a composed `BaseURL + /apps/{slug}` points at a host that does not
// serve apps — the scan would be of a 404, reported clean.
func TestEstateTargetsUsesTheAppsOrigin(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "payroll", database.VisibilityAuthenticated)

	out := f.read(t, "root")

	row := findTarget(out.Targets, "payroll")
	if row == nil {
		t.Fatalf("payroll is missing from %+v", out.Targets)
	}
	if row.URL != "https://apps.example/apps/payroll" {
		t.Errorf("url = %q, want the apps origin's — a scanner pointed at a URL this handler "+
			"composed by hand would scan a 404 and report it clean", row.URL)
	}
	if out.AppsOrigin != "https://apps.example" {
		t.Errorf("apps_origin = %q, want the origin said once", out.AppsOrigin)
	}
}

// An app that has never deployed is a row with no URL, never an omission. A
// scanner needs to know the slug exists and is not scannable; leaving it out
// is the reader answering less than it knows.
func TestEstateTargetsListsAnAppWithNoRelease(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "live", database.VisibilityPublic)
	f.undeployed(t, "never-built", database.VisibilityPrivate)

	out := f.read(t, "root")

	row := findTarget(out.Targets, "never-built")
	if row == nil {
		t.Fatalf("an app with no release was omitted: %+v", out.Targets)
	}
	if row.URL != "" {
		t.Errorf("url = %q, want empty — there is nothing at that address to scan", row.URL)
	}
	if row.Status == "" {
		t.Error("status is empty — the row exists to say the app is there and not scannable")
	}
	if out.AppsTotal != 2 || out.AppsAddressable != 1 {
		t.Errorf("coverage = %d apps, %d addressable; want 2 and 1 — the coverage is what stops "+
			"the list being read as the whole estate", out.AppsTotal, out.AppsAddressable)
	}
}

// A batch app runs jobs and serves no pages, so it is a row with no URL and
// is not counted as addressable — a scanner sent to it would scan a 404.
func TestEstateTargetsGivesABatchAppNoAddress(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "live", database.VisibilityPublic)
	f.deployedAs(t, "nightly", database.VisibilityPrivate, "batch-python")

	out := f.read(t, "root")

	row := findTarget(out.Targets, "nightly")
	if row == nil {
		t.Fatalf("the batch app was omitted: %+v", out.Targets)
	}
	if row.URL != "" {
		t.Errorf("url = %q, want empty — a batch app serves no pages", row.URL)
	}
	if out.AppsTotal != 2 || out.AppsAddressable != 1 {
		t.Errorf("coverage = %d apps, %d addressable; want 2 and 1", out.AppsTotal, out.AppsAddressable)
	}
}

// A locked app is listed with its lock state. The reader's job is to be
// complete; the scanner's job is to skip what it is told to skip.
func TestEstateTargetsListsLockedApps(t *testing.T) {
	f := newTargetsFixture(t)
	app := f.deployed(t, "retired", database.VisibilityAuthenticated)
	if err := f.srv.db.SetAppLock(app.ID, "root", "retired", time.Now().UTC(), app.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("locking: %v", err)
	}

	out := f.read(t, "root")

	row := findTarget(out.Targets, "retired")
	if row == nil || !row.Locked {
		t.Fatalf("a locked app is missing or unmarked: %+v", out.Targets)
	}
	if out.AppsLocked != 1 {
		t.Errorf("apps_locked = %d, want 1 — said once here rather than counted off the rows",
			out.AppsLocked)
	}
}

// The install's anonymous-access switch reaches this reader through the one
// function that owns the substitution. A row that said `public` about an app
// the install has withheld would send a scanner to test access that cannot
// work, and report the refusal as a finding.
func TestEstateTargetsReportsEffectiveVisibility(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "open", database.VisibilityPublic)

	if row := findTarget(f.read(t, "root").Targets, "open"); row == nil ||
		row.Visibility != database.VisibilityPublic || row.VisibilityWithheld {
		t.Fatalf("before the switch, want public and not withheld: %+v", row)
	}

	settings, err := f.srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.PublicAppsEnabled = false
	if err := f.srv.db.SetPlatformSettings(&settings, "root", "req", "ip", "ua"); err != nil {
		t.Fatalf("writing settings: %v", err)
	}

	row := findTarget(f.read(t, "root").Targets, "open")
	if row == nil {
		t.Fatal("the app went missing")
	}
	if row.Visibility != database.VisibilityAuthenticated {
		t.Errorf("visibility = %q, want %q — the substitution is authenticated and never private",
			row.Visibility, database.VisibilityAuthenticated)
	}
	if !row.VisibilityWithheld {
		t.Error("visibility_withheld is false — the difference between an owner who never " +
			"published and an install that withdrew it has been lost")
	}
}

// The gate. 404 to everyone who is not an administrator, like every other
// admin read here — never 403.
func TestEstateTargetsIsAdministratorOnly(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "payroll", database.VisibilityAuthenticated)

	for _, who := range []string{"", "owner"} {
		rec := sendAs(t, f.srv, f.router, who, http.MethodGet, "/api/v1/admin/estate/targets", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("as %q = %d, want 404 — an estate-wide address list is a target map", who, rec.Code)
		}
	}
}

// A deploy key may read it, and both scopes may: an estate read is a
// pipeline's request, and a pipeline holds an toftd_. It is the only route under
// /admin either scope reaches.
func TestEstateTargetsIsReadableByADeployKey(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "payroll", database.VisibilityAuthenticated)

	for _, scope := range []string{database.DeployKeyScopeRead, database.DeployKeyScopeDeploy} {
		t.Run(scope, func(t *testing.T) {
			secret := mintScopedDeployKey(t, f.srv, f.router, "ci-"+scope, scope)
			rec := asDeployKey(t, f.router, http.MethodGet, "/api/v1/admin/estate/targets", secret, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			out := decodeTargets(t, rec)
			if findTarget(out.Targets, "payroll") == nil {
				t.Errorf("a deploy key got an empty list — it acts as owner on every app, and a "+
					"permitted route that answers nothing is worse than a refusal: %+v", out.Targets)
			}
		})
	}
}

// A removed app key reads nothing here: it is refused by name before any
// route, and an old scan job holding one is told what replaces it.
func TestEstateTargetsIsClosedToAnAppKey(t *testing.T) {
	f := newTargetsFixture(t)
	f.deployed(t, "payroll", database.VisibilityAuthenticated)
	secret := "toft_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/estate/targets", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("an toft_ key read the estate's address list — a key on one app may not enumerate "+
			"the install (status %d)", rec.Code)
	}
}

// An install with no apps answers an empty list with its coverage, never a
// 404: "nothing to scan" is an answer and "this route does not exist" is not.
func TestEstateTargetsOnAnEmptyInstall(t *testing.T) {
	f := newTargetsFixture(t)

	out := f.read(t, "root")

	if out.Targets == nil {
		t.Error("targets is null rather than [] — a client looping over it would break on an " +
			"install that simply has no apps yet")
	}
	if len(out.Targets) != 0 || out.AppsTotal != 0 {
		t.Errorf("want an empty list over an empty estate, got %+v", out)
	}
	if out.AppsOrigin == "" {
		t.Error("apps_origin is empty — with no rows it is the only way to tell what the URLs " +
			"would have looked like")
	}
}

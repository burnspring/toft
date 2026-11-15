package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// estateKey identifies a census bucket the way the assertions want to talk
// about one: what was recorded, for which framework, under which mode.
type estateKey struct {
	language  string
	version   string
	framework string
	mode      string
}

func estateCounts(groups []database.EstateGroup) map[estateKey]int {
	out := map[estateKey]int{}
	for _, g := range groups {
		out[estateKey{g.Language, g.Version, g.Framework, g.RunningMode}] += g.Count
	}
	return out
}

// mkEstateApp is one app with a framework and a running mode, which is the
// whole of what the census reads off the app row.
func mkEstateApp(t *testing.T, db *database.DB, slug, framework, mode string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		ID:          "app-" + slug,
		Slug:        slug,
		Name:        slug,
		OwnerID:     owner.ID,
		RepoURL:     "https://github.com/example/" + slug,
		Branch:      "main",
		Status:      database.AppStatusRunning,
		Framework:   framework,
		RunningMode: mode,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// mkEstateDeploy writes one finished deployment with a resolved runtime, the
// way a build does: UpdateDeployment for the outcome, SetDeploymentRuntime for
// what the interpreter turned out to be.
//
// `finished` is passed rather than taken from the clock so a test can say which
// of two deployments is the newer one without sleeping.
func mkEstateDeploy(t *testing.T, db *database.DB, appID, status, language, version string, finished time.Time) *database.Deployment {
	t.Helper()
	d := &database.Deployment{AppID: appID, Branch: "main", Status: database.DeployStatusPending}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	d.Status = status
	fin := finished.UTC()
	d.FinishedAt = &fin
	if err := db.UpdateDeployment(d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if version != "" || language != "" {
		if err := db.SetDeploymentRuntime(d.ID, language, version); err != nil {
			t.Fatalf("SetDeploymentRuntime: %v", err)
		}
	}
	return d
}

// The census reads the release that is *serving*, which is the newest
// successful deployment and never simply the newest one.
//
// This is the assertion the whole feature rests on. An app whose last build
// failed is running what its last successful build resolved, and a census that
// reported the failed build's version would be reporting a version no process
// on this box is executing.
func TestEstateByLanguageCountsTheServingRelease(t *testing.T) {
	db := dbtest.New(t)
	base := time.Now().UTC().Add(-time.Hour)

	// A static app resolves no interpreter, and is still part of the estate.
	mkEstateApp(t, db, "brochure", "static", "shared")

	// Go, left to the go command: `record` declines to write, so the row keeps
	// both columns empty — indistinguishable in the data from a release that
	// predates the columns, which is why neither may be rendered as a version.
	goApp := mkEstateApp(t, db, "gosvc", "go", "isolated")
	mkEstateDeploy(t, db, goApp.ID, database.DeployStatusSuccess, "", "", base)

	older := mkEstateApp(t, db, "reports", "flask", "shared")
	mkEstateDeploy(t, db, older.ID, database.DeployStatusSuccess, "python", "3.12.7", base)

	newer := mkEstateApp(t, db, "api", "fastapi", "isolated")
	mkEstateDeploy(t, db, newer.ID, database.DeployStatusSuccess, "python", "3.13.4", base)

	// The one that matters: serving 3.13.9, with a *newer failed* build that
	// resolved something else entirely.
	rolled := mkEstateApp(t, db, "dash", "flask", "shared")
	mkEstateDeploy(t, db, rolled.ID, database.DeployStatusSuccess, "python", "3.13.9", base)
	mkEstateDeploy(t, db, rolled.ID, database.DeployStatusFailed, "python", "3.14.0", base.Add(30*time.Minute))

	census, err := db.EstateByLanguage(database.AppScope{All: true})
	if err != nil {
		t.Fatalf("EstateByLanguage: %v", err)
	}
	got := estateCounts(census.Groups)

	want := map[estateKey]int{
		{"", "", "static", "shared"}:                1,
		{"", "", "go", "isolated"}:                  1,
		{"python", "3.12.7", "flask", "shared"}:     1,
		{"python", "3.13.4", "fastapi", "isolated"}: 1,
		{"python", "3.13.9", "flask", "shared"}:     1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("bucket %+v: got %d, want %d", k, got[k], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d buckets, want %d: %+v", len(got), len(want), got)
	}
	if n := got[estateKey{"python", "3.14.0", "flask", "shared"}]; n != 0 {
		t.Errorf("the failed build's version was counted (%d apps on 3.14.0); the serving release is the answer", n)
	}
}

// Coverage rides on the same rows the table renders, and a static app is in the
// total and never in the versioned count.
func TestEstateCoverageCountsStaticAppsAndNotTheirVersions(t *testing.T) {
	db := dbtest.New(t)
	base := time.Now().UTC().Add(-time.Hour)

	mkEstateApp(t, db, "brochure", "static", "shared")
	goApp := mkEstateApp(t, db, "gosvc", "go", "shared")
	mkEstateDeploy(t, db, goApp.ID, database.DeployStatusSuccess, "", "", base)
	py := mkEstateApp(t, db, "reports", "flask", "shared")
	mkEstateDeploy(t, db, py.ID, database.DeployStatusSuccess, "python", "3.13.4", base)

	// Never deployed at all: in the estate, with nothing to say about it.
	mkEstateApp(t, db, "fresh", "node", "shared")

	locked := mkEstateApp(t, db, "retired", "flask", "shared")
	mkEstateDeploy(t, db, locked.ID, database.DeployStatusSuccess, "python", "3.11.9", base)
	if err := db.SetAppLock(locked.ID, database.BuiltinAdminID, "Retired.", time.Now(), locked.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("SetAppLock: %v", err)
	}

	census, err := db.EstateByLanguage(database.AppScope{All: true})
	if err != nil {
		t.Fatalf("EstateByLanguage: %v", err)
	}
	if census.AppsTotal != 5 {
		t.Errorf("AppsTotal = %d, want 5 (the static app and the never-deployed one are part of the estate)", census.AppsTotal)
	}
	if census.AppsVersioned != 2 {
		t.Errorf("AppsVersioned = %d, want 2 (only the two with a recorded version)", census.AppsVersioned)
	}
	if census.AppsLocked != 1 {
		t.Errorf("AppsLocked = %d, want 1 (a locked app is counted and marked, never dropped)", census.AppsLocked)
	}

	// The coverage is summed from the rows, so it cannot disagree with them.
	total := 0
	for _, g := range census.Groups {
		total += g.Count
	}
	if total != census.AppsTotal {
		t.Errorf("the rows add up to %d and the coverage says %d", total, census.AppsTotal)
	}
}

// The set of versions is whatever is present. A series nobody runs is not a
// zero row; it is not a row.
func TestEstateVersionSetIsDiscoveredNotDeclared(t *testing.T) {
	db := dbtest.New(t)
	base := time.Now().UTC().Add(-time.Hour)

	only := mkEstateApp(t, db, "legacy", "flask", "shared")
	mkEstateDeploy(t, db, only.ID, database.DeployStatusSuccess, "python", "3.11.9", base)
	other := mkEstateApp(t, db, "current", "flask", "shared")
	mkEstateDeploy(t, db, other.ID, database.DeployStatusSuccess, "python", "3.13.4", base)

	if err := db.DeleteApp(only.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	census, err := db.EstateByLanguage(database.AppScope{All: true})
	if err != nil {
		t.Fatalf("EstateByLanguage: %v", err)
	}
	for _, g := range census.Groups {
		if g.Version == "3.11.9" {
			t.Fatalf("3.11.9 still has a row after its only app was removed: %+v", g)
		}
	}
	if len(census.Groups) != 1 {
		t.Errorf("got %d groups, want 1: %+v", len(census.Groups), census.Groups)
	}
}

// The census obeys the same readable-apps predicate as everything else. It is
// administrator-only at the route, and the scope is not decoration.
func TestEstateByLanguageIsScoped(t *testing.T) {
	db := dbtest.New(t)
	base := time.Now().UTC().Add(-time.Hour)

	mine := mkEstateApp(t, db, "mine", "flask", "shared")
	mkEstateDeploy(t, db, mine.ID, database.DeployStatusSuccess, "python", "3.13.4", base)
	theirs := mkEstateApp(t, db, "theirs", "go", "shared")
	mkEstateDeploy(t, db, theirs.ID, database.DeployStatusSuccess, "go", "1.25.3", base)

	census, err := db.EstateByLanguage(database.AppScope{
		PrincipalID: "owner-mine", PrincipalType: "user",
	})
	if err != nil {
		t.Fatalf("EstateByLanguage: %v", err)
	}
	if census.AppsTotal != 1 {
		t.Fatalf("a scoped census saw %d apps, want 1: %+v", census.AppsTotal, census.Groups)
	}
	if census.Groups[0].Framework != "flask" {
		t.Errorf("scoped census returned %q, want the caller's own app", census.Groups[0].Framework)
	}
}

// D4 follow-up: after a rollback the census, the per-row runtime, the app's
// "last deployed" and the sweep's serving release all named the release rolled
// *away* from — each picked the newest success, while a rollback leaves no
// successful row of its own. Each now reads servingDeploymentSQL, the
// definition ServingDeploymentID uses: what the app row serves.
func TestEstateReadersFollowARollback(t *testing.T) {
	db := dbtest.New(t)
	app := mkEstateApp(t, db, "rolled", "flask", "shared")
	builtBack := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	back := mkEstateDeploy(t, db, app.ID, database.DeployStatusSuccess, "python", "3.12.9", builtBack)
	away := mkEstateDeploy(t, db, app.ID, database.DeployStatusSuccess, "python", "3.13.7", time.Now().UTC())
	app.ReleaseRef = "artifact:releases/" + app.ID + "/" + back.ID + ".tar.gz"
	if err := db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}

	census, err := db.EstateByLanguage(database.AppScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	counts := estateCounts(census.Groups)
	if counts[estateKey{"python", "3.12.9", "flask", "shared"}] != 1 || counts[estateKey{"python", "3.13.7", "flask", "shared"}] != 0 {
		t.Errorf("census = %v, want the rolled-back-to 3.12.9, not %s's 3.13.7", counts, away.ID)
	}

	rt, err := db.ServingRuntimeForApps([]string{app.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rt[app.ID].Version != "3.12.9" {
		t.Errorf("ServingRuntimeForApps = %+v, want 3.12.9", rt[app.ID])
	}

	got, err := db.GetApp(app.ID)
	if err != nil || got == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.LastDeployedAt == nil || !got.LastDeployedAt.Equal(builtBack) {
		t.Errorf("LastDeployedAt = %v, want the serving release's %v", got.LastDeployedAt, builtBack)
	}

	releases, err := db.ListServingReleases()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range releases {
		if r.AppID == app.ID && r.DeploymentID != back.ID {
			t.Errorf("ListServingReleases = %s, want the rolled-back-to %s", r.DeploymentID, back.ID)
		}
	}
}

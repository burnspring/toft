package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// AppActivity.Quiet is unit-tested against no database at all — it is pure.
// What needs a real Postgres is the query that feeds it, and specifically the
// two things that would be wrong in a way no type checks: the scalar
// subqueries, and whether SUM(requests) survives the presence of deployment
// rows. Joining app_usage_daily and deployments in one statement multiplies
// them against each other, and the lifetime request count is what silently
// comes out N times too large when it does.

func mkQuietApp(t *testing.T, db *database.DB, id, name, ownerID string, created time.Time) {
	t.Helper()
	_, err := db.Conn().Exec(`
		INSERT INTO apps (id, slug, name, repo_url, status, created_at, updated_at, owner_user_id)
		VALUES (?, ?, ?, '', 'running', ?, ?, ?)`,
		id, id, name, created, created, ownerID)
	if err != nil {
		t.Fatalf("creating app %s: %v", id, err)
	}
}

func mkUsage(t *testing.T, db *database.DB, appID string, day time.Time, user string, requests int64) {
	t.Helper()
	_, err := db.Conn().Exec(`
		INSERT INTO app_usage_daily (app_id, day, user_id, requests) VALUES (?, ?, ?, ?)`,
		appID, day, user, requests)
	if err != nil {
		t.Fatalf("usage row for %s: %v", appID, err)
	}
}

func mkQuietDeploy(t *testing.T, db *database.DB, id, appID, status string, finished time.Time) {
	t.Helper()
	_, err := db.Conn().Exec(`
		INSERT INTO deployments (id, app_id, status, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?)`, id, appID, status, finished, finished)
	if err != nil {
		t.Fatalf("deployment %s: %v", id, err)
	}
}

func TestListAppActivity(t *testing.T) {
	db := dbtest.New(t)

	owner := mkUser(t, db, "quietowner", database.SystemRolePublisher,
		time.Now().UTC().Add(-400*24*time.Hour), nil, nil, false)

	created := time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)

	// Traffic across three days and two viewers, plus three deployments. If the
	// query joined instead of using subqueries, the request total would come
	// back multiplied by the deployment count.
	mkQuietApp(t, db, "app-busy", "Busy", owner, created)
	mkUsage(t, db, "app-busy", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "u1", 10)
	mkUsage(t, db, "app-busy", time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), "u1", 20)
	mkUsage(t, db, "app-busy", time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), "u2", 5)
	mkQuietDeploy(t, db, "d1", "app-busy", database.DeployStatusSuccess, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	mkQuietDeploy(t, db, "d2", "app-busy", database.DeployStatusSuccess, time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC))
	mkQuietDeploy(t, db, "d3", "app-busy", database.DeployStatusFailed, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	// Never opened, never successfully deployed. Both zero-value times, which is
	// what lets Quiet fall back to creation.
	mkQuietApp(t, db, "app-fresh", "Fresh", owner, created)

	got, err := db.ListAppActivity()
	if err != nil {
		t.Fatalf("ListAppActivity: %v", err)
	}

	byID := map[string]database.AppActivity{}
	for _, a := range got {
		byID[a.AppID] = a
	}

	busy, ok := byID["app-busy"]
	if !ok {
		t.Fatal("app-busy missing from activity")
	}
	if busy.Requests != 35 {
		t.Errorf("lifetime requests = %d, want 35 — a join would have multiplied this by the deployment count", busy.Requests)
	}
	if want := "2026-03-02"; busy.LastDay.UTC().Format("2006-01-02") != want {
		t.Errorf("LastDay = %s, want %s", busy.LastDay.UTC().Format("2006-01-02"), want)
	}
	// The failed deploy in June is more recent than either success and must not
	// be what the clock reads: it is evidence somebody tried, not evidence the
	// app is wanted running.
	if want := "2026-02-10"; busy.LastDeploy.UTC().Format("2006-01-02") != want {
		t.Errorf("LastDeploy = %s, want %s (the failed June deploy must not count)",
			busy.LastDeploy.UTC().Format("2006-01-02"), want)
	}
	if busy.OwnerLogin != "quietowner" {
		t.Errorf("OwnerLogin = %q, want %q", busy.OwnerLogin, "quietowner")
	}

	fresh, ok := byID["app-fresh"]
	if !ok {
		t.Fatal("app-fresh missing from activity")
	}
	if !fresh.LastDay.IsZero() || !fresh.LastDeploy.IsZero() || fresh.Requests != 0 {
		t.Errorf("an app with no traffic and no deploy came back as %+v", fresh)
	}
	// Thirty days after the creation date above, so the fallback is measured
	// from the only fact this app has.
	if q := fresh.Quiet(time.Date(2025, 2, 4, 0, 0, 0, 0, time.UTC)); q.QuietBasis != database.QuietBasisCreated || q.QuietDays != 30 {
		t.Errorf("fresh app verdict = %s/%d days, want created/30", q.QuietBasis, q.QuietDays)
	}
}

// The owner's line and the administrator's list read the same evidence, so the
// query that feeds one app has to agree with the query that feeds all of them.
// Two definitions of "quiet" would eventually tell the two people most likely
// to compare notes different numbers.
func TestAppActivityForAgreesWithTheList(t *testing.T) {
	db := dbtest.New(t)
	now := time.Now().UTC()
	owner := mkUser(t, db, "agreeowner", database.SystemRolePublisher,
		now.Add(-500*24*time.Hour), nil, nil, false)

	mkQuietApp(t, db, "visited", "Visited", owner, now.Add(-400*24*time.Hour))
	mkUsage(t, db, "visited", now.Add(-30*24*time.Hour), "u1", 12)
	mkQuietDeploy(t, db, "d1", "visited", database.DeployStatusSuccess, now.Add(-90*24*time.Hour))

	mkQuietApp(t, db, "deployed", "Deployed", owner, now.Add(-400*24*time.Hour))
	mkQuietDeploy(t, db, "d2", "deployed", database.DeployStatusSuccess, now.Add(-10*24*time.Hour))

	mkQuietApp(t, db, "never", "Never", owner, now.Add(-200*24*time.Hour))

	list, err := db.ListAppActivity()
	if err != nil {
		t.Fatalf("ListAppActivity: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 apps, got %d", len(list))
	}

	for _, want := range list {
		got, err := db.AppActivityFor(want.AppID)
		if err != nil {
			t.Fatalf("AppActivityFor(%s): %v", want.AppID, err)
		}
		if got == nil {
			t.Fatalf("AppActivityFor(%s) returned nothing", want.AppID)
		}
		if *got != want {
			t.Errorf("%s: one-app evidence differs from the list's:\n one  %+v\n list %+v",
				want.AppID, *got, want)
		}
		// And therefore the verdict, which is what each surface renders.
		a, b := got.Quiet(now), want.Quiet(now)
		if a.QuietDays != b.QuietDays || a.QuietBasis != b.QuietBasis {
			t.Errorf("%s: verdicts differ: %+v vs %+v", want.AppID, a, b)
		}
	}

	// The three bases, since the tile's copy branches on them.
	bases := map[string]string{}
	for _, a := range list {
		bases[a.AppID] = a.Quiet(now).QuietBasis
	}
	if bases["visited"] != database.QuietBasisVisit ||
		bases["deployed"] != database.QuietBasisDeploy ||
		bases["never"] != database.QuietBasisCreated {
		t.Errorf("bases = %+v", bases)
	}
}

// An app that does not exist is not an error, so the caller keeps its own 404.
func TestAppActivityForUnknownApp(t *testing.T) {
	db := dbtest.New(t)
	got, err := db.AppActivityFor("no-such-app")
	if err != nil {
		t.Fatalf("AppActivityFor: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for an app that does not exist, got %+v", got)
	}
}

// The Overview tile reads the summary, so the verdict has to arrive on it.
func TestAppSummaryCarriesTheQuietVerdict(t *testing.T) {
	db := dbtest.New(t)
	now := time.Now().UTC()
	owner := mkUser(t, db, "staleowner", database.SystemRolePublisher,
		now.Add(-600*24*time.Hour), nil, nil, false)

	mkQuietApp(t, db, "stale", "Stale", owner, now.Add(-500*24*time.Hour))
	mkUsage(t, db, "stale", now.Add(-214*24*time.Hour), "u1", 9)

	s, err := db.GetAppSummary("stale")
	if err != nil {
		t.Fatalf("GetAppSummary: %v", err)
	}
	// The windowed count says nothing past its window, which is exactly why the
	// unwindowed verdict rides along.
	if s.Requests30d != 0 {
		t.Errorf("requests_30d = %d, want 0", s.Requests30d)
	}
	if s.QuietBasis != database.QuietBasisVisit {
		t.Errorf("quiet_basis = %q", s.QuietBasis)
	}
	if s.QuietDays < 213 || s.QuietDays > 215 {
		t.Errorf("quiet_days = %d, want about 214", s.QuietDays)
	}
}

package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func TestFailOrphanedDeployments(t *testing.T) {
	db := dbtest.New(t)

	// An app mid-build with nothing to fall back to → failed.
	fresh := mkApp(t, db, "orphan-fresh")
	db.UpdateAppStatus(fresh.ID, database.AppStatusBuilding)

	// An app rebuilding over a live release → still running.
	live := mkApp(t, db, "orphan-live")
	live.CurrentRelease = "orphan-live-old"
	if err := db.UpdateApp(live); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	db.UpdateAppStatus(live.ID, database.AppStatusBuilding)

	mkDeploy := func(appID, status string) *database.Deployment {
		d := &database.Deployment{AppID: appID, Status: status, BuildLog: "==> Cloning..."}
		if err := db.CreateDeployment(d, nil); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}
		return d
	}
	orphanPending := mkDeploy(fresh.ID, database.DeployStatusPending)
	orphanBuilding := mkDeploy(live.ID, database.DeployStatusBuilding)
	done := mkDeploy(live.ID, database.DeployStatusSuccess)

	n, err := db.FailOrphanedDeployments()
	if err != nil {
		t.Fatalf("FailOrphanedDeployments: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept %d deployments, want 2", n)
	}

	for _, id := range []string{orphanPending.ID, orphanBuilding.ID} {
		d, err := db.GetDeployment(id)
		if err != nil {
			t.Fatalf("GetDeployment: %v", err)
		}
		if d.Status != database.DeployStatusFailed {
			t.Errorf("deployment %s status = %q, want failed", id, d.Status)
		}
		if d.FinishedAt == nil {
			t.Errorf("deployment %s has no finished_at", id)
		}
		if !strings.Contains(d.BuildLog, "interrupted by a platform restart") {
			t.Errorf("deployment %s build log does not explain the failure: %q", id, d.BuildLog)
		}
	}

	// A finished row is history, not an orphan.
	if d, _ := db.GetDeployment(done.ID); d.Status != database.DeployStatusSuccess {
		t.Errorf("successful deployment rewritten to %q", d.Status)
	}

	if a, _ := db.GetApp(fresh.ID); a.Status != database.AppStatusFailed {
		t.Errorf("app without a release settled to %q, want failed", a.Status)
	}
	if a, _ := db.GetApp(live.ID); a.Status != database.AppStatusRunning {
		t.Errorf("app with a live release settled to %q, want running", a.Status)
	}

	// Second sweep finds nothing — and must not disturb settled state.
	if n, err := db.FailOrphanedDeployments(); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v, want 0", n, err)
	}
}

// An interrupted build puts back the status the app held before it when that
// was a decision — stopped or asleep — rather than starting it at boot; a
// second build over the first keeps the first's before-state (DB-5).
func TestFailOrphanedDeploymentsRestoresAStopOrASleep(t *testing.T) {
	db := dbtest.New(t)
	cases := map[string]string{
		"was-stopped":  database.AppStatusStopped,
		"was-sleeping": database.AppStatusSleeping,
		"was-running":  database.AppStatusRunning,
		"was-crashed":  database.AppStatusCrashed,
	}
	want := map[string]string{
		"was-stopped":  database.AppStatusStopped,
		"was-sleeping": database.AppStatusSleeping,
		"was-running":  database.AppStatusRunning,
		"was-crashed":  database.AppStatusRunning,
	}
	ids := map[string]string{}
	for slug, before := range cases {
		a := mkApp(t, db, slug)
		a.CurrentRelease = slug + "-r1"
		if err := db.UpdateApp(a); err != nil {
			t.Fatal(err)
		}
		db.UpdateAppStatus(a.ID, before)
		got, err := db.MarkAppBuilding(a.ID)
		if err != nil || got != before {
			t.Fatalf("%s: MarkAppBuilding = %q, %v; want %q", slug, got, err, before)
		}
		// A second, overlapping build keeps the first one's before-state.
		if again, _ := db.MarkAppBuilding(a.ID); again != before {
			t.Errorf("%s: a second build read %q as before, want %q", slug, again, before)
		}
		if err := db.CreateDeployment(&database.Deployment{AppID: a.ID, Status: database.DeployStatusBuilding}, nil); err != nil {
			t.Fatal(err)
		}
		ids[slug] = a.ID
	}
	if _, err := db.FailOrphanedDeployments(); err != nil {
		t.Fatal(err)
	}
	for slug, id := range ids {
		if a, _ := db.GetApp(id); a.Status != want[slug] {
			t.Errorf("%s settled to %q, want %q", slug, a.Status, want[slug])
		}
	}
}

package autodeploy

import (
	"testing"

	"toft/internal/database"
)

// DG-13 (security review 2026-11-14): a tip a building deploy already cloned
// is claimed as seen and not enqueued again — push, then Deploy, built the
// commit twice.
func TestATipABuildingDeployAlreadyClonedIsClaimedAndNotEnqueued(t *testing.T) {
	p, db, app := folderPoller(t, "")
	r := remote{tip: "c2"}
	r.install(t)

	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusBuilding}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	// Not cloned yet: it will take the tip, so nothing is claimed.
	p.check(app)
	if seen, n := outcome(t, db, app.ID); seen != "c1" || n != 1 {
		t.Fatalf("before the clone: seen=%q deployments=%d, want c1 and the one row", seen, n)
	}
	if err := db.SetDeploymentCommit(d.ID, "c2"); err != nil {
		t.Fatal(err)
	}
	p.check(app)
	if seen, n := outcome(t, db, app.ID); seen != "c2" || n != 1 {
		t.Errorf("after the clone: seen=%q deployments=%d, want c2 claimed and no second build", seen, n)
	}
}

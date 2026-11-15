package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A rollback row says which release it restored for good: restored_from is
// its source at queue, and survives ClearDeploymentSource — what a rollback
// rebuilt for a new base path does to source_deploy_id (BB-005).
func TestARollbackKeepsWhatItRestored(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "restored")
	src := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
	rb := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusPending,
		TriggeredBy: "rollback", SourceDeployID: src.ID}
	if err := db.CreateDeployment(rb, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearDeploymentSource(rb.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetDeployment(rb.ID)
	if err != nil || got == nil || got.SourceDeployID != "" || got.RestoredFrom != src.ID {
		t.Fatalf("get = %+v %v, want restored_from %s and no source", got, err, src.ID)
	}
	all, _ := db.ListDeployments(app.ID)
	for _, d := range all {
		if d.ID == rb.ID && d.RestoredFrom != src.ID {
			t.Errorf("list restored_from = %q", d.RestoredFrom)
		}
		if d.ID == src.ID && d.RestoredFrom != "" {
			t.Errorf("an ordinary release restored %q", d.RestoredFrom)
		}
	}
}

package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// mkEndpointApp creates an app the way a first deploy leaves one: no release,
// so no endpoint yet.
func mkEndpointApp(t *testing.T, db *database.DB, slug string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: owner.ID,
		RepoURL: "https://github.com/example/" + slug, Branch: "main",
		Status: database.AppStatusStopped,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// Two isolated workloads answer on the same port — 8080 in every pod — and are
// told apart by the service they are reached at. `port` alone was UNIQUE, which
// is only true of shared mode, so the second isolated app on an install could
// not record its endpoint: the deploy failed with a constraint violation one
// line after its log said `Ready.`, with the workload already live.
func TestTwoIsolatedAppsShareTheContainerPort(t *testing.T) {
	db := dbtest.New(t)

	for _, slug := range []string{"alpha", "bravo"} {
		app := mkEndpointApp(t, db, slug)
		app.EndpointHost = slug + "-r1.toft-apps.svc.cluster.local"
		app.Port = 8080
		app.Status = database.AppStatusRunning
		if err := db.UpdateApp(app); err != nil {
			t.Fatalf("recording the endpoint for %s: %v — every isolated workload "+
				"listens on the same port and is addressed by host", slug, err)
		}
	}
}

// And the half the old constraint was right about: in shared mode the port *is*
// the address, so two apps on the loopback host may not hold one.
func TestTwoSharedAppsCannotHoldOneLoopbackPort(t *testing.T) {
	db := dbtest.New(t)

	first := mkEndpointApp(t, db, "alpha")
	first.EndpointHost = "127.0.0.1"
	first.Port = 3100
	if err := db.UpdateApp(first); err != nil {
		t.Fatalf("recording the first endpoint: %v", err)
	}

	second := mkEndpointApp(t, db, "bravo")
	second.EndpointHost = "127.0.0.1"
	second.Port = 3100
	if err := db.UpdateApp(second); err == nil {
		t.Error("two apps hold 127.0.0.1:3100 — the backstop under the port " +
			"allocator is gone, and one of them is unreachable")
	}
}

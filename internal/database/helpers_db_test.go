package database_test

import (
	"testing"

	"toft/internal/database"
)

func mkApp(t *testing.T, db *database.DB, slug string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		ID:      "app-" + slug,
		Slug:    slug,
		Name:    slug,
		OwnerID: owner.ID,
		RepoURL: "https://github.com/example/" + slug,
		Branch:  "main",
		Status:  database.AppStatusStopped,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// mkActor creates a real user to attribute a run to. A run attributed to
// somebody who was never an account is exactly the state the actor foreign key
// exists to refuse.
func mkActor(t *testing.T, db *database.DB, login string) string {
	t.Helper()
	u := &database.User{ID: "user-" + login, Login: login}
	if err := db.UpsertUser(u); err != nil {
		t.Fatalf("UpsertUser(%s): %v", login, err)
	}
	return u.ID
}

// upsertJobDef writes one definition the way a deploy would, without replacing
// the app's others — what a test that builds its jobs one at a time needs.
func upsertJobDef(db *database.DB, d *database.JobDefinition) error {
	return database.UpsertJobDefinitionForTest(db, d)
}

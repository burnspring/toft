package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// DB2-3: the delivered owner/name went into LIKE unescaped and
// case-sensitively, so `acme/data_api` also woke `acme/dataXapi`, and a
// delivery for `Acme/Data-API` missed an app stored in lower case.
func TestAWebhookMatchesItsRepositoryExactlyAndIgnoringCase(t *testing.T) {
	db := dbtest.New(t)
	conn := &database.GitConnection{ID: "gc-hook", Kind: "github", DisplayName: "hook", BaseURL: "https://github.com", AuthMethod: "app", Enabled: true}
	if err := db.CreateGitConnection(conn, database.AuditEntry{}); err != nil {
		t.Fatalf("CreateGitConnection: %v", err)
	}
	wire := func(slug, repo string) string {
		app := mkApp(t, db, slug)
		if _, err := db.Conn().Exec(`UPDATE apps SET repo_url=?, auto_deploy=true, git_connection_id=? WHERE id=?`,
			repo, conn.ID, app.ID); err != nil {
			t.Fatal(err)
		}
		return app.ID
	}
	exact := wire("hook-exact", "https://github.com/acme/data_api.git")
	wire("hook-lookalike", "https://github.com/acme/dataXapi")
	lower := wire("hook-lower", "https://github.com/acme/data-api")

	got, err := db.AppsOnConnectionForRepo(conn.ID, "acme/data_api")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != exact {
		t.Errorf("acme/data_api woke %v, want only %s", got, exact)
	}
	got, err = db.AppsOnConnectionForRepo(conn.ID, "Acme/Data-API")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != lower {
		t.Errorf("Acme/Data-API woke %v, want %s", got, lower)
	}
}

package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"toft/internal/artifact"
	"toft/internal/builder"
	"toft/internal/database"
)

// Deleting the row alone leaves the app's tree and its artifacts behind, and
// nothing afterwards names them — the app id was the only handle. So the
// delete has to take both.
func TestDeleteApp_RemovesFilesAndArtifacts(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.AppsDir = t.TempDir()

	store, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	srv.artifacts = store
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "doomed", Name: "Doomed", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusStopped,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Two releases on disk and one artifact in the store, as a deployed app
	// would have.
	for _, deployID := range []string{"deploy-1", "deploy-2"} {
		dir := builder.ReleaseDir(srv.cfg.AppsDir, app.ID, deployID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir release: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('hi')\n"), 0o644); err != nil {
			t.Fatalf("write release file: %v", err)
		}
	}
	src := builder.ReleaseDir(srv.cfg.AppsDir, app.ID, "deploy-2")
	key := artifact.ReleaseKey(app.ID, "deploy-2")
	if _, err := store.Put(context.Background(), key, src); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	appDir := builder.AppDir(srv.cfg.AppsDir, app.ID)
	if _, err := os.Stat(appDir); err != nil {
		t.Fatalf("app dir missing before the delete: %v", err)
	}

	rec := deleteAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d body=%s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Fatalf("app directory survived the delete (err=%v)", err)
	}
	if _, err := store.Stat(context.Background(), key); err == nil {
		t.Fatal("the artifact survived the delete")
	}
	if a, _ := srv.db.GetApp(app.ID); a != nil {
		t.Fatal("the app row survived the delete")
	}
}

// An app that never built has no directory and no artifacts; deleting it must
// still work, because deletion has to be idempotent to be retryable.
func TestDeleteApp_WithNothingOnDisk(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.AppsDir = t.TempDir()
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "never-built", Name: "Never Built", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusStopped,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	if rec := deleteAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d body=%s", rec.Code, rec.Body.String())
	}
	if a, _ := srv.db.GetApp(app.ID); a != nil {
		t.Fatal("the app row survived the delete")
	}
}

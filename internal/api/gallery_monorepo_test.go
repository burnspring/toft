package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
)

// A catalog whose items share one repository: the extension and the example
// live in folders of it, and the example's archive is the whole repository.
const sharedRepoCatalog = `{
  "schema_version": 1,
  "items": [
    {"id":"ext-1","kind":"extension","name":"Audit Explorer","version":"1.5.0",
     "suggested_slug":"audit-explorer",
     "repo_url":"https://github.com/acme/gallery","ref":"audit-explorer/v1.5.0",
     "path":"extensions/audit-explorer"},
    {"id":"ex-1","kind":"example","name":"Sales Dashboard","version":"2.2.0",
     "suggested_slug":"sales-dashboard","zip_url":"%ZIP%",
     "path":"examples/sales-dashboard"}
  ]
}`

// sharedRepoZip is what GitHub serves for a tag of a shared repository: one
// top-level folder, and every item's folder under it.
func sharedRepoZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"gallery-sales-dashboard-v2.2.0/catalog.json":                        "{}",
		"gallery-sales-dashboard-v2.2.0/examples/sales-dashboard/app.py":     "print('sales')\n",
		"gallery-sales-dashboard-v2.2.0/examples/other/app.py":               "print('other')\n",
		"gallery-sales-dashboard-v2.2.0/extensions/audit-explorer/toft.toml": "[app]\n",
	} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, body)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *galleryEnv) publish(t *testing.T, catalog string) {
	t.Helper()
	e.catalog = catalog
	if err := e.svc.Sync(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestAnExtensionFromASharedRepositoryIsBuiltFromItsFolder(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	e.publish(t, sharedRepoCatalog)

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	app, _ := e.srv.db.GetApp(body.App.ID)
	if app.RepoURL != "https://github.com/acme/gallery" || app.Branch != "audit-explorer/v1.5.0" ||
		app.RepoSubdir != "extensions/audit-explorer" {
		t.Errorf("source = %s @ %s : %q, want the catalog's repository, tag and folder",
			app.RepoURL, app.Branch, app.RepoSubdir)
	}
}

// The case that would otherwise fail only at build time: an extension
// installed from a repository of its own, whose catalog then moves it into the
// shared one. Moving the ref alone would clone the old repository at a tag only
// the new one has.
func TestAnUpdateMovesTheRepositoryAndTheFolderWithTheRef(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.4.0")
	if err := e.srv.db.SetAutoDeploy(app.ID, true, "abc123"); err != nil {
		t.Fatal(err)
	}
	e.publish(t, sharedRepoCatalog)

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	after, _ := e.srv.db.GetApp(app.ID)
	if after.RepoURL != "https://github.com/acme/gallery" || after.Branch != "audit-explorer/v1.5.0" ||
		after.RepoSubdir != "extensions/audit-explorer" {
		t.Errorf("after the update: %s @ %s : %q", after.RepoURL, after.Branch, after.RepoSubdir)
	}
	if after.LastSeenSHA != "" {
		t.Errorf("last_seen_sha = %q survived a move to another repository", after.LastSeenSHA)
	}
	if after.GalleryItemID != "ext-1" {
		t.Error("an update detached the app from the gallery")
	}
}

func TestARedeployThatFollowsTheCatalogMovesTheSourceToo(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.4.0")
	e.publish(t, sharedRepoCatalog)

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
	}
	after, _ := e.srv.db.GetApp(app.ID)
	if after.RepoURL != "https://github.com/acme/gallery" || after.RepoSubdir != "extensions/audit-explorer" {
		t.Errorf("after the redeploy: %s : %q", after.RepoURL, after.RepoSubdir)
	}
}

func TestAnExampleFromASharedRepositoryStagesOnlyItsFolder(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	e.archive = sharedRepoZip(t)
	e.publish(t, sharedRepoCatalog)

	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	staged := builder.UploadStagingDir(e.srv.cfg.AppsDir, body.App.ID)
	if _, err := os.Stat(filepath.Join(staged, "app.py")); err != nil {
		t.Fatalf("the example's folder is not the staged root: %v", err)
	}
	for _, absent := range []string{"catalog.json", "examples", "extensions"} {
		if _, err := os.Stat(filepath.Join(staged, absent)); err == nil {
			t.Errorf("%s from outside the example's folder was staged", absent)
		}
	}
	if _, err := os.Stat(staged + ".whole"); err == nil {
		t.Error("the whole unpacked repository was left beside the staging directory")
	}
}

func TestAnExampleWhosePathIsNotInItsArchiveIsRefused(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	e.archive = sharedRepoZip(t)
	e.publish(t, strings.Replace(sharedRepoCatalog, `"path":"examples/sales-dashboard"`, `"path":"examples/missing"`, 1))

	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "examples/missing is not in the repository") {
		t.Fatalf("deploy = %d: %s, want 400 naming the missing folder", rec.Code, rec.Body.String())
	}
	// The repair the upload route makes: no app is left pointing at a staging
	// directory that was never written.
	apps, err := e.srv.db.ListAllApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 0 {
		t.Errorf("%d apps left behind by a refused example", len(apps))
	}
}

func TestCreatingAnAppFromAFolderOfARepository(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps",
		`{"repo_url":"https://github.com/acme/gallery","repo_subdir":" /extensions/audit-explorer/ "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	// Found by the slug the folder should have given it, which is the second
	// thing this test asserts.
	got, _ := e.srv.db.GetAppBySlug("audit-explorer")
	if got == nil {
		t.Fatalf("no app named for its folder: %s", rec.Body.String())
	}
	if got.RepoSubdir != "extensions/audit-explorer" {
		t.Errorf("repo_subdir = %q, want the normalized folder", got.RepoSubdir)
	}
	// Every app of a shared repository has the same repo basename; the folder
	// is what names it.
	if got.Slug != "audit-explorer" {
		t.Errorf("slug = %q, want it taken from the folder", got.Slug)
	}

	for _, bad := range []string{"../etc", "a/.git", "a//b"} {
		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps",
			`{"repo_url":"https://github.com/acme/gallery","repo_subdir":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create with repo_subdir %q = %d, want 400", bad, rec.Code)
		}
	}

	// PATCH moves it, and clears the poller's last commit with it.
	if err := e.srv.db.SetAutoDeploy(got.ID, true, "abc123"); err != nil {
		t.Fatal(err)
	}
	rec = e.do(t, "publisher", http.MethodPatch, "/api/v1/apps/"+got.ID, `{"repo_subdir":"examples/other/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ = e.srv.db.GetApp(got.ID)
	if got.RepoSubdir != "examples/other" || got.LastSeenSHA != "" {
		t.Errorf("after PATCH: %q, last_seen_sha %q", got.RepoSubdir, got.LastSeenSHA)
	}
	rec = e.do(t, "publisher", http.MethodPatch, "/api/v1/apps/"+got.ID, `{"repo_subdir":"../x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with a bad folder = %d", rec.Code)
	}
}

func TestAnUploadedAppHasNoFolderToMove(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	rec = e.do(t, "publisher", http.MethodPatch, "/api/v1/apps/"+body.App.ID, `{"repo_subdir":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH repo_subdir on an upload = %d: %s", rec.Code, rec.Body.String())
	}
}

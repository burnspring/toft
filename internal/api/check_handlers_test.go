package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/builder"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/gitconn"
	"toft/internal/runtime"
)

func (e *createModeEnv) check(t *testing.T, who, appID string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return e.checkAt(t, who, "/api/v1/apps/"+appID+"/check", files)
}

func (e *createModeEnv) checkAt(t *testing.T, who, path string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "app.zip")
	io.Copy(part, zipOf(t, files))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// A check answers with the deploy's decisions and leaves nothing behind: no
// deployment row, and no directory under the app's own.
func TestCheckBundleRecordsNothing(t *testing.T) {
	e := newCreateModeEnv(t)
	app := seedAppOwnedBy(t, e.srv, "checked", "pub")

	rec := e.check(t, "pub", app.ID, map[string]string{"index.html": "<p>hi</p>", "toft.toml": "[reports]\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != true || body["framework"] != "static" || body["warnings"] == nil {
		t.Errorf("body = %v", body)
	}
	if deps, _ := e.srv.db.ListDeployments(app.ID); len(deps) != 0 {
		t.Errorf("a check left %d deployment rows", len(deps))
	}
	entries, _ := os.ReadDir(filepath.Join(e.srv.cfg.AppsDir, app.ID))
	if len(entries) != 0 {
		t.Errorf("a check left %v behind", entries)
	}
}

// A problem is a 200 with ok false — the check worked, the bundle would not.
func TestCheckBundleReportsTheDeploysReason(t *testing.T) {
	e := newCreateModeEnv(t)
	app := seedAppOwnedBy(t, e.srv, "nothing", "pub")
	rec := e.check(t, "pub", app.ID, map[string]string{"notes.txt": "x"})
	var body struct {
		OK       bool `json:"ok"`
		Problems []struct {
			Reason string `json:"reason"`
		} `json:"problems"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body.OK || len(body.Problems) == 0 ||
		body.Problems[0].Reason != "unsupported_framework" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// The upload's doors: the lock refuses a check exactly as it refuses a deploy.
func TestCheckBundleHonoursTheLock(t *testing.T) {
	e := newCreateModeEnv(t)
	app := seedAppOwnedBy(t, e.srv, "locked", "pub")
	if err := e.srv.db.SetAppLock(app.ID, "root", "held", time.Now().UTC(), app.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("locking: %v", err)
	}
	rec := e.check(t, "pub", app.ID, map[string]string{"index.html": "x"})
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body["code"] != "app_locked" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// AP-8: an app at its version cap, with cleanup off so the cap blocks, gets
// the upload's 409 from the check — not an ok the deploy then refuses.
func TestCheckBundleHonoursTheVersionCap(t *testing.T) {
	e := newCreateModeEnv(t)
	enc, err := crypto.NewEncryptor(strings.Repeat("d0", 32))
	if err != nil {
		t.Fatal(err)
	}
	e.srv.builder = builder.New(e.srv.db, e.srv.rts, enc, gitconn.NewResolver(e.srv.db, enc), nil,
		builder.Options{AppsDir: e.srv.cfg.AppsDir})
	settings, err := e.srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxVersions = 1
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	app := seedAppOwnedBy(t, e.srv, "capped", "pub")
	if _, err := e.srv.db.Conn().Exec(`UPDATE apps SET auto_delete_releases = false WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	d := &database.Deployment{AppID: app.ID, Status: database.DeployStatusSuccess}
	if err := e.srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(builder.ReleaseDir(e.srv.cfg.AppsDir, app.ID, d.ID), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := e.check(t, "pub", app.ID, map[string]string{"index.html": "x"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "platform maximum") {
		t.Fatalf("check at the cap = %d %s, want the upload's 409", rec.Code, rec.Body.String())
	}
}

// A lock the dependency policy placed is lifted by a deploy of the fixed tree,
// so checking that tree is admitted as the deploy is -- the PATCH rule
// refused it (code review 2026-11-11, P1-6).
func TestCheckBundleAdmitsADependencyLockedApp(t *testing.T) {
	e := newCreateModeEnv(t)
	app := seedAppOwnedBy(t, e.srv, "deplocked", "pub")
	if _, err := e.srv.db.LockAppForDependency(app.ID, app.Slug, "Locked for MAL-1.",
		map[string]any{"advisory_id": "MAL-1"}, time.Now().UTC()); err != nil {
		t.Fatalf("locking: %v", err)
	}
	rec := e.check(t, "pub", app.ID, map[string]string{"index.html": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("check of a dependency-locked app = %d %s, want 200 as its deploy is admitted", rec.Code, rec.Body.String())
	}
}

// Before an app exists: the create route's doors, nothing recorded, and the
// mode a create by this caller would be born in.
func TestCheckNewBundle(t *testing.T) {
	e := newCreateModeEnv(t)
	rec := e.checkAt(t, "pub", "/api/v1/apps/check", map[string]string{"index.html": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != true || body["execution_mode"] != "shared" {
		t.Errorf("body = %v", body)
	}

	seedUser(t, e.srv.db, "watcher", database.SystemRoleViewer)
	if rec := e.checkAt(t, "watcher", "/api/v1/apps/check", map[string]string{"index.html": "x"}); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer checking a new app = %d, want the create route's 403", rec.Code)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.srv.cfg.AppsDir, ".preflight")); len(entries) != 0 {
		t.Errorf("a check left %v behind", entries)
	}
}

// `toft check` carries the storage plan the deploy would make, judged for the
// caller and the owner — for an existing app and for one not yet created
// (PRD D9).
func TestCheckCarriesTheStoragePlan(t *testing.T) {
	e := newCreateModeEnv(t)
	e.srv.storage = &fakeProvisioner{}
	app := seedAppOwnedBy(t, e.srv, "planned", "pub")
	res := &database.StorageResource{Name: "reports", Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports"}
	if err := e.srv.db.CreateStorageResource(res, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	if err := e.srv.db.PutStorageManager(&database.StorageManager{ResourceID: res.ID,
		SubjectType: database.StoreSubjectUser, SubjectID: "pub", MaxAccess: "full"},
		"root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageManager: %v", err)
	}
	flask := map[string]string{
		"app.py":           "from flask import Flask\napp = Flask(__name__)\n",
		"requirements.txt": "flask\n",
		"toft.toml":        "[[storage]]\nname = \"reports\"\n",
	}
	type answer struct {
		Storage *struct {
			Add []struct {
				Name, Access, Path string
			} `json:"add"`
		} `json:"storage"`
		Problems []struct{ Reason, Code string } `json:"problems"`
	}
	read := func(rec *httptest.ResponseRecorder) answer {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var a answer
		json.Unmarshal(rec.Body.Bytes(), &a)
		return a
	}

	got := read(e.check(t, "pub", app.ID, flask))
	if got.Storage == nil || len(got.Storage.Add) != 1 || got.Storage.Add[0].Path != "/storage/reports" {
		t.Fatalf("existing app: storage = %+v problems = %+v, want one add at /storage/reports", got.Storage, got.Problems)
	}
	got = read(e.checkAt(t, "pub", "/api/v1/apps/check", flask))
	if got.Storage == nil || len(got.Storage.Add) != 1 {
		t.Fatalf("new app owned by its creator: storage = %+v problems = %+v, want one add", got.Storage, got.Problems)
	}

	flask["toft.toml"] = "[[storage]]\nname = \"payroll\"\n"
	got = read(e.check(t, "pub", app.ID, flask))
	if len(got.Problems) != 1 || got.Problems[0].Reason != "storage_refused" || got.Problems[0].Code != "storage_unknown" {
		t.Fatalf("an unknown name: problems = %+v, want storage_refused/storage_unknown", got.Problems)
	}
}

// A create whose [[storage]] check refuses is refused at the upload with
// check's code, and leaves no app holding the slug (drills on trytoft
// 2026-11-02: it got past the upload and failed at detect, code null).
func TestCreateRefusesAStorageTableAtTheUpload(t *testing.T) {
	e := newCreateModeEnv(t)
	e.srv.storage = &fakeProvisioner{}
	flask := map[string]string{
		"app.py":           "from flask import Flask\napp = Flask(__name__)\n",
		"requirements.txt": "flask\n",
		"toft.toml":        "[[storage]]\nname = \"payroll\"\n",
	}
	archive, _ := io.ReadAll(zipOf(t, flask))
	rec := e.uploadArchive(t, "pub", "wants-payroll", "", string(archive))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s, want 422", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "storage_unknown" || body["reason"] != "storage_refused" {
		t.Errorf("body = %s, want code storage_unknown, reason storage_refused", rec.Body.String())
	}
	if app := e.appBySlug(t, "wants-payroll"); app != nil {
		t.Error("a refused create left an app holding the slug")
	}
}

// toft check plans an entry with no level at the shared root's level, and
// refuses one the root cannot serve with storage_level_unsupported — before
// the upload, where it used to fail only at the app's start (docs/bugs
// 2026-11-03 #1).
func TestCheckPlansAVolumeAtItsRootsLevel(t *testing.T) {
	e := newCreateModeEnv(t)
	e.srv.storage = runtime.NewSharedStorage([]runtime.StorageRoot{{Name: "rw", Path: t.TempDir()}}, t.TempDir(), "")
	app := seedAppOwnedBy(t, e.srv, "rooted", "pub")
	res := &database.StorageResource{Name: "sales-data", Slug: "sales-data", Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: "rw"}
	if err := e.srv.db.CreateStorageResource(res, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	if err := e.srv.db.PutStorageManager(&database.StorageManager{ResourceID: res.ID,
		SubjectType: database.StoreSubjectUser, SubjectID: "pub", MaxAccess: "full"},
		"root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageManager: %v", err)
	}
	tree := map[string]string{
		"app.py":           "from flask import Flask\napp = Flask(__name__)\n",
		"requirements.txt": "flask\n",
		"toft.toml":        "[[storage]]\nname = \"sales-data\"\n",
	}
	var got struct {
		Storage *struct {
			Add []struct{ Name, Access string } `json:"add"`
		} `json:"storage"`
		Problems []struct{ Reason, Code string } `json:"problems"`
	}
	rec := e.check(t, "pub", app.ID, tree)
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Storage == nil || len(got.Storage.Add) != 1 || got.Storage.Add[0].Access != "full" {
		t.Fatalf("no level: %d %s, want one add at full", rec.Code, rec.Body.String())
	}

	tree["toft.toml"] = "[[storage]]\nname = \"sales-data\"\naccess = \"read\"\n"
	got.Storage, got.Problems = nil, nil
	rec = e.check(t, "pub", app.ID, tree)
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Problems) != 1 || got.Problems[0].Reason != "storage_refused" || got.Problems[0].Code != "storage_level_unsupported" {
		t.Fatalf("read on a read-write root: %s, want storage_refused/storage_level_unsupported", rec.Body.String())
	}
}

// A push to a dependency-locked app is its way out, but the slug or visibility
// it carries is a setting, refused under the lock as PATCH refuses it -- the
// push renamed the app before any build (code review 2026-11-11, P1-3).
func TestAPushCarryingASlugIsRefusedUnderADependencyLock(t *testing.T) {
	e := newCreateModeEnv(t)
	app := seedAppOwnedBy(t, e.srv, "pushlocked", "pub")
	if _, err := e.srv.db.LockAppForDependency(app.ID, app.Slug, "Locked for MAL-1.",
		map[string]any{"advisory_id": "MAL-1"}, time.Now().UTC()); err != nil {
		t.Fatalf("locking: %v", err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("slug", "pushlocked-renamed")
	part, _ := mw.CreateFormFile("file", "app.zip")
	io.Copy(part, zipOf(t, map[string]string{"index.html": "x"}))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, "pub"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body["code"] != "app_locked" {
		t.Fatalf("a renaming push under a dependency lock = %d %s, want 409 app_locked", rec.Code, rec.Body.String())
	}
	if fresh, _ := e.srv.db.GetApp(app.ID); fresh == nil || fresh.Slug != "pushlocked" {
		t.Errorf("the slug moved under the lock: %+v", fresh)
	}
}

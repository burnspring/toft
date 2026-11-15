package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/builder"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/gallery"
	"toft/internal/sources"
)

// A catalog with one of each kind. The example's archive is served by the same
// test server, so the whole example path — fetch, unpack, stage — runs for
// real rather than being mocked at the seam that matters.
const testCatalog = `{
  "schema_version": 1,
  "items": [
    {"id":"ext-1","kind":"extension","name":"Audit Explorer","version":"1.4.0",
     "suggested_slug":"audit-explorer",
     "repo_url":"https://github.com/acme/audit-explorer","ref":"v1.4.0",
     "env":[{"name":"AUDIT_DAYS","default":"30"}]},
    {"id":"ex-1","kind":"example","name":"Sales Dashboard","version":"2.1.0",
     "suggested_slug":"sales-dashboard","zip_url":"%ZIP%"}
  ]
}`

type galleryEnv struct {
	srv    *Server
	router http.Handler
	svc    *gallery.Service
	zipHit int
	// catalog is what the test server serves, so a test can republish the
	// catalog — retiring an item is a thing that happens to installs.
	catalog string
	// archive is what the test server serves for any .zip, so a test can
	// serve an archive of a whole shared repository instead.
	archive []byte
}

// exampleZip is a minimal archive with a common root directory, which is the
// shape extractArchive strips — plus a dependency tree, because a catalog archive
// is unpacked by the same function a human upload is and gets the same filter
// (ADR §60).
func exampleZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"sales-dashboard/app.py":           "print('hello')\n",
		"sales-dashboard/requirements.txt": "streamlit\n",
		"sales-dashboard/venv/pyvenv.cfg":  "home = /somebody/else\n",
		"sales-dashboard/.git/config":      "[remote \"origin\"]\n",
	} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := io.WriteString(f, body); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func newGalleryEnv(t *testing.T, mode string) *galleryEnv {
	t.Helper()

	db := dbtest.New(t)
	env := &galleryEnv{catalog: testCatalog}

	env.archive = exampleZip(t)
	srvTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			env.zipHit++
			w.Write(env.archive)
			return
		}
		io.WriteString(w, strings.Replace(env.catalog, "%ZIP%", "https://"+r.Host+"/sales.zip", 1))
	}))
	t.Cleanup(srvTLS.Close)

	cfg := &config.Config{BaseURL: "http://localhost:8080", AppsDir: t.TempDir()}
	svc := gallery.NewForTesting(db, srvTLS.URL, "built-in", "1.0.0", srvTLS.Client().Transport)

	srv := newTestServerWithConfig(t, db, cfg, WithGallery(svc))
	env.srv, env.router, env.svc = srv, srv.Router(http.NotFoundHandler()), svc

	seedUser(t, db, "admin-user", database.SystemRoleAdmin)
	seedUser(t, db, "publisher", database.SystemRolePublisher)
	seedUser(t, db, "viewer", database.SystemRoleViewer)

	setGalleryMode(t, db, mode)
	if mode != database.GalleryOff {
		if err := svc.Sync(t.Context()); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	return env
}

func setGalleryMode(t *testing.T, db *database.DB, mode string) {
	t.Helper()
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	s.GalleryMode = mode
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
}

func (e *galleryEnv) do(t *testing.T, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// auditRows reads the merged activity feed for one action, which is the only
// exported way into audit_log — and the same path the Activity page uses.
func auditRows(t *testing.T, db *database.DB, action string) []database.ActivityEntry {
	t.Helper()
	rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Actions: []string{action}, Limit: 50})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	return rows
}

func decodeGallery(t *testing.T, rec *httptest.ResponseRecorder) galleryResponse {
	t.Helper()
	var resp galleryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding the gallery response: %v (%s)", err, rec.Body.String())
	}
	return resp
}

// Off means absent, not disabled: the routes 404 rather than answering with an
// empty list, exactly as storage and local sign-in do.
func TestGalleryRoutes404WhileItIsOff(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryOff)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/gallery"},
		{http.MethodPost, "/api/v1/gallery/sync"},
		{http.MethodPost, "/api/v1/gallery/ext-1/deploy"},
	} {
		rec := e.do(t, "admin-user", tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestGalleryListingFiltersByRoleAndMode(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	admin := decodeGallery(t, e.do(t, "admin-user", http.MethodGet, "/api/v1/gallery", ""))
	if len(admin.Items) != 2 {
		t.Fatalf("an administrator sees %d items, want both", len(admin.Items))
	}

	// A publisher may deploy, so they see the gallery — but an extension is an
	// administrator's business, and the filtering is the API's, not the UI's.
	pub := decodeGallery(t, e.do(t, "publisher", http.MethodGet, "/api/v1/gallery", ""))
	if len(pub.Items) != 1 || pub.Items[0].Kind != gallery.KindExample {
		t.Fatalf("a publisher sees %+v, want the example alone", pub.Items)
	}

	// A viewer cannot publish at all, so a catalogue of things to deploy is
	// not theirs to read.
	if rec := e.do(t, "viewer", http.MethodGet, "/api/v1/gallery", ""); rec.Code == http.StatusOK {
		t.Errorf("a viewer got %d on the gallery listing", rec.Code)
	}

	setGalleryMode(t, e.srv.db, database.GalleryExtensionsOnly)
	only := decodeGallery(t, e.do(t, "admin-user", http.MethodGet, "/api/v1/gallery", ""))
	if len(only.Items) != 1 || only.Items[0].Kind != gallery.KindExtension {
		t.Fatalf("extensions_only shows %+v, want the extension alone", only.Items)
	}
}

func TestDeployExtensionIsAdminOnlyAndPinsToTheCatalogRef(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	// A publisher must not even be able to confirm the item exists.
	if rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ext-1/deploy", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("publisher deploying an extension = %d, want 404", rec.Code)
	}

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy",
		`{"env_vars":{"AUDIT_DAYS":"90"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		App        database.App        `json:"app"`
		Deployment database.Deployment `json:"deployment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	app, err := e.srv.db.GetApp(body.App.ID)
	if err != nil || app == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.RepoURL != "https://github.com/acme/audit-explorer" || app.Branch != "v1.4.0" {
		t.Errorf("source = %s@%s, want the catalog's repo and ref", app.RepoURL, app.Branch)
	}
	if app.AutoDeploy {
		t.Error("a gallery app must not track a branch")
	}
	if app.GalleryItemID != "ext-1" || app.GalleryKind != gallery.KindExtension {
		t.Errorf("provenance = %q/%q", app.GalleryItemID, app.GalleryKind)
	}
	// Nothing claims to be running a version until a build has succeeded.
	if app.GalleryVersion != "" {
		t.Errorf("gallery_version = %q before the build finished", app.GalleryVersion)
	}
	if body.Deployment.GalleryVersion != "1.4.0" {
		t.Errorf("the deployment carries %q, want the target version", body.Deployment.GalleryVersion)
	}
	if app.OwnerID != "admin-user" {
		t.Errorf("owner = %q", app.OwnerID)
	}

	// The catalog asked for an env var; the deployer supplied it. Nothing about
	// the entry granted the app anything.
	vars, err := e.srv.db.GetEnvVars(app.ID)
	if err != nil || len(vars) != 1 || vars[0].Key != "AUDIT_DAYS" {
		t.Fatalf("env vars = %+v (%v)", vars, err)
	}

	// The deploy is audited with where the code came from, which under this
	// design is a ref rather than a commit.
	entries := auditRows(t, e.srv.db, "gallery.deploy")
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d", len(entries))
	}
	if !strings.Contains(entries[0].Detail, "audit-explorer@v1.4.0") {
		t.Errorf("audit row does not name the source: %s", entries[0].Detail)
	}
}

func TestDeployExampleFetchesAndStagesTheArchive(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		App database.App `json:"app"`
	}
	deployBody := rec.Body.Bytes()
	json.Unmarshal(deployBody, &body)

	app, _ := e.srv.db.GetApp(body.App.ID)
	// The example lands as an upload, which is what makes every existing
	// "this app has no remote" guard apply to it with no new code.
	if !builder.IsUploadedSource(app.RepoURL) {
		t.Fatalf("repo_url = %q, want an upload:// source", app.RepoURL)
	}
	if app.Branch != "upload" {
		t.Errorf("branch = %q", app.Branch)
	}
	// Provenance is recorded; the version is not, because nothing is running
	// yet. Both kinds behave the same way here — the builder writes the version
	// when the build succeeds — so an example whose build failed never claims
	// to be running one.
	if app.GalleryItemID != "ex-1" || app.GalleryKind != gallery.KindExample {
		t.Errorf("provenance = %q/%q", app.GalleryItemID, app.GalleryKind)
	}
	if app.GalleryVersion != "" {
		t.Errorf("gallery_version = %q before the build finished", app.GalleryVersion)
	}
	if e.zipHit != 1 {
		t.Errorf("the archive was fetched %d times", e.zipHit)
	}

	// The archive is unpacked where a human upload would have landed, with its
	// common root stripped.
	staged := builder.UploadStagingDir(e.srv.cfg.AppsDir, app.ID)
	if _, err := os.Stat(filepath.Join(staged, "app.py")); err != nil {
		t.Fatalf("app.py is not staged: %v", err)
	}
	// The strip is a property of the unpacker, not of the door: the catalog's
	// archive loses its venv and its .git exactly as an upload would, and the
	// response says so.
	for _, gone := range []string{"venv", ".git"} {
		if _, err := os.Stat(filepath.Join(staged, gone)); err == nil {
			t.Errorf("%s survived the filter on the gallery path", gone)
		}
	}
	var deployed struct {
		Removed []builder.RemovedEntry `json:"removed"`
	}
	if err := json.Unmarshal(deployBody, &deployed); err != nil {
		t.Fatalf("decoding the deploy response: %v", err)
	}
	if len(deployed.Removed) != 2 {
		t.Errorf("removed = %+v, want venv and .git reported", deployed.Removed)
	}

	// And the app is an ordinary uploaded app from here on: the existing guard
	// refuses git settings on it, provenance or no provenance.
	rec = e.do(t, "publisher", http.MethodPatch, "/api/v1/apps/"+app.ID, `{"auto_deploy":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "upload") {
		t.Errorf("PATCH auto_deploy on an example = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExamplesAreAbsentUnderExtensionsOnly(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryExtensionsOnly)

	// 404 rather than 403: under this mode the item is not listed, and a
	// refusal that confirms it exists would be a disclosure of the catalog the
	// listing deliberately withholds.
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deploying an example under extensions_only = %d, want 404", rec.Code)
	}
	if e.zipHit != 0 {
		t.Error("a refused deploy must not fetch anything")
	}
}

func TestInstalledIsDerivedAndKindDecidesWhoseQuestionItIs(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	// One extension installed by an administrator, one example taken by a
	// publisher.
	if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", ""); rec.Code != http.StatusCreated {
		t.Fatalf("extension deploy: %s", rec.Body.String())
	}
	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("example deploy: %s", rec.Body.String())
	}
	var deployed struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &deployed)

	admin := decodeGallery(t, e.do(t, "admin-user", http.MethodGet, "/api/v1/gallery", ""))
	byID := map[string]galleryItemResponse{}
	for _, it := range admin.Items {
		byID[it.ID] = it
	}
	if got := byID["ext-1"].Installed; len(got) != 1 || got[0].AppID == "" || !got[0].Mine {
		t.Fatalf("the administrator's own extension install = %+v", got)
	}
	// An example is a personal question: somebody else's copy is not an answer
	// to "have I already taken this one".
	if got := byID["ex-1"].Installed; len(got) != 0 {
		t.Fatalf("the administrator sees %+v as their own example installs", got)
	}

	pub := decodeGallery(t, e.do(t, "publisher", http.MethodGet, "/api/v1/gallery", ""))
	if got := pub.Items[0].Installed; len(got) != 1 || !got[0].Mine {
		t.Fatalf("the publisher's own example install = %+v", got)
	}

	// Deleting the app ends the installation. Nothing stored says otherwise,
	// because nothing stored says it at all.
	if err := e.srv.db.DeleteApp(deployed.App.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	pub = decodeGallery(t, e.do(t, "publisher", http.MethodGet, "/api/v1/gallery", ""))
	if got := pub.Items[0].Installed; len(got) != 0 {
		t.Fatalf("a deleted app is still reported as installed: %+v", got)
	}
}

// Installs are not capped: two administrators may each hold their own copy,
// deliberately, and the second deploy is offered rather than refused.
func TestASecondCopyIsAllowed(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	seedUser(t, e.srv.db, "other-admin", database.SystemRoleAdmin)

	for _, who := range []string{"other-admin", "admin-user"} {
		if rec := e.do(t, who, http.MethodPost, "/api/v1/gallery/ext-1/deploy", ""); rec.Code != http.StatusCreated {
			t.Fatalf("deploy as %s: %s", who, rec.Body.String())
		}
	}

	resp := decodeGallery(t, e.do(t, "admin-user", http.MethodGet, "/api/v1/gallery", ""))
	var ext galleryItemResponse
	for _, it := range resp.Items {
		if it.ID == "ext-1" {
			ext = it
		}
	}
	// An extension is an install-wide fact, so both copies are reported — and
	// an administrator reads every app, so both are linkable.
	if len(ext.Installed) != 2 {
		t.Fatalf("installed = %+v, want both copies", ext.Installed)
	}
	mine := 0
	for _, inst := range ext.Installed {
		if inst.AppID == "" || inst.Slug == "" {
			t.Errorf("an instance came back without a link: %+v", inst)
		}
		if inst.Mine {
			mine++
		}
	}
	if mine != 1 {
		t.Errorf("%d instances reported as mine, want 1", mine)
	}
	if !ext.Deployable {
		t.Error("a further copy must still be deployable")
	}
	// The slugs cannot collide.
	if ext.Installed[0].Slug == ext.Installed[1].Slug {
		t.Errorf("both copies share the slug %q", ext.Installed[0].Slug)
	}
}

func TestAutoDeployIsRefusedOnAnAttachedExtension(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", "")
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+body.App.ID, `{"auto_deploy":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH auto_deploy = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gallery") {
		t.Errorf("the refusal does not name the update mechanism: %s", rec.Body.String())
	}
}

func TestChangingTheBranchDetachesAndSaysSo(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", "")
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+body.App.ID, `{"branch":"main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH branch = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gallery_detached") {
		t.Errorf("the response does not say the app was detached: %s", rec.Body.String())
	}
	app, _ := e.srv.db.GetApp(body.App.ID)
	if app.GalleryItemID != "" {
		t.Errorf("still attached after pointing the app elsewhere: %q", app.GalleryItemID)
	}

	// And auto-deploy is available again, because the app now tracks a branch
	// like any other.
	if rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID, `{"auto_deploy":true}`); rec.Code != http.StatusOK {
		t.Errorf("auto_deploy after detaching = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDetachIsAudited(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", "")
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	if rec := e.do(t, "admin-user", http.MethodDelete, "/api/v1/apps/"+body.App.ID+"/gallery", ""); rec.Code != http.StatusOK {
		t.Fatalf("detach = %d: %s", rec.Code, rec.Body.String())
	}
	app, _ := e.srv.db.GetApp(body.App.ID)
	if app.GalleryItemID != "" || app.GalleryVersion != "" {
		t.Errorf("detach left provenance behind: %+v", app)
	}
	if entries := auditRows(t, e.srv.db, "gallery.detach"); len(entries) != 1 {
		t.Fatalf("audit entries = %d", len(entries))
	}

	// Detaching twice is a refusal, not a silent success: the second caller is
	// asking about a link that does not exist.
	if rec := e.do(t, "admin-user", http.MethodDelete, "/api/v1/apps/"+body.App.ID+"/gallery", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("second detach = %d, want 400", rec.Code)
	}
}

func TestSyncIsAdminOnly(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	if rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/sync", ""); rec.Code != http.StatusNotFound {
		t.Errorf("publisher sync = %d, want 404", rec.Code)
	}
	// The first sync ran during setup, so this one is inside the window.
	if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/sync", ""); rec.Code != http.StatusTooManyRequests {
		t.Errorf("immediate re-sync = %d, want 429", rec.Code)
	}
}

func TestTheCatalogURLIsNotASetting(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings",
		`{"gallery_catalog_url":"https://evil.example.com/catalog.json"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCHing the catalog URL = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GALLERY_CATALOG_URL") {
		t.Errorf("the refusal does not say where the value comes from: %s", rec.Body.String())
	}

	// And the mode is refused by name rather than clamped.
	rec = e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings", `{"gallery_deploy_mode":"everything"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "extensions_only") {
		t.Fatalf("a bad mode = %d: %s", rec.Code, rec.Body.String())
	}
}

// --- updates ---

// installExtension deploys ext-1 and marks the release live at `version`, which
// is what a successful build does.
func (e *galleryEnv) installExtension(t *testing.T, who, version string) *database.App {
	t.Helper()
	rec := e.do(t, who, http.MethodPost, "/api/v1/gallery/ext-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy: %s", rec.Body.String())
	}
	var body struct {
		App database.App `json:"app"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	// Stand in for the build: the deployment finishes, and the builder writes
	// the version the deployment was carrying onto the app.
	deploys, err := e.srv.db.ListDeployments(body.App.ID)
	if err != nil || len(deploys) != 1 {
		t.Fatalf("deployments = %+v (%v)", deploys, err)
	}
	d := deploys[0]
	d.Status = database.DeployStatusSuccess
	if err := e.srv.db.UpdateDeployment(&d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if version != "" {
		if err := e.srv.db.SetAppGalleryVersion(body.App.ID, version); err != nil {
			t.Fatalf("SetAppGalleryVersion: %v", err)
		}
	}
	app, _ := e.srv.db.GetApp(body.App.ID)
	return app
}

func (e *galleryEnv) policyOf(t *testing.T, who, appID string) appGalleryPolicy {
	t.Helper()
	rec := e.do(t, who, http.MethodGet, "/api/v1/apps/"+appID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET app = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Policy struct {
			Gallery *appGalleryPolicy `json:"gallery"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the app: %v", err)
	}
	if body.Policy.Gallery == nil {
		t.Fatalf("no gallery block on the app's policy")
	}
	return *body.Policy.Gallery
}

func TestUpdateDeploysOntoTheSameAppAndOnlyAdvancesOnSuccess(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.3.0")

	if pol := e.policyOf(t, "admin-user", app.ID); !pol.UpdateAvailable ||
		pol.InstalledVersion != "1.3.0" || pol.CatalogVersion != "1.4.0" {
		t.Fatalf("policy = %+v, want an update offered", pol)
	}

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	var deploy database.Deployment
	json.Unmarshal(rec.Body.Bytes(), &deploy)

	// The same app, not a new one.
	if deploy.AppID != app.ID {
		t.Errorf("the update created a deployment on %s, not on the app", deploy.AppID)
	}
	if deploy.GalleryVersion != "1.4.0" {
		t.Errorf("the deployment carries %q", deploy.GalleryVersion)
	}
	after, _ := e.srv.db.GetApp(app.ID)
	if after.Branch != "v1.4.0" {
		t.Errorf("branch = %q, want the new ref", after.Branch)
	}
	// Nothing has been built, so what the app reports as running has not moved.
	if after.GalleryVersion != "1.3.0" {
		t.Errorf("gallery_version = %q before the build finished — a queued update is not a running one", after.GalleryVersion)
	}
	if rows := auditRows(t, e.srv.db, "gallery.update"); len(rows) != 1 {
		t.Errorf("audit rows = %d", len(rows))
	}
}

// A locked extension is not updated: an update is a new release, so it takes
// the deploy rule, and refusing it in the builder came after the ref had moved
// and a build had run (RO-4).
func TestGalleryUpdateIsRefusedUnderALock(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.3.0")
	before, _ := e.srv.db.GetApp(app.ID)
	if _, err := e.srv.db.Conn().Exec(`UPDATE apps SET locked_at = now() WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	var deploys int
	countDeploys := func() int {
		t.Helper()
		var n int
		if err := e.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM deployments WHERE app_id = ?`, app.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deploys = countDeploys()

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "app_locked") {
		t.Fatalf("update of a locked extension = %d: %s, want 409 app_locked", rec.Code, rec.Body.String())
	}
	after, _ := e.srv.db.GetApp(app.ID)
	if after.Branch != before.Branch {
		t.Errorf("branch moved to %q under a lock", after.Branch)
	}
	if n := countDeploys(); n != deploys {
		t.Errorf("%d deployments after the refusal, want %d", n, deploys)
	}
}

// "Version unknown" was one sentence for two different facts, and the commoner
// of the two is not a failure to determine anything: nothing was written down
// at install. Telling the owner of a working app that its version "cannot be
// compared" describes a fault that is not there.
func TestNothingRecordedIsNotTheSameAsCannotBeCompared(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	// Serving — the flip wrote a release — with no version recorded. This is
	// the app installed before the version was recorded at all.
	served := e.installExtension(t, "admin-user", "")
	served.CurrentRelease = "reports-dep-1"
	if err := e.srv.db.UpdateApp(served); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	pol := e.policyOf(t, "admin-user", served.ID)
	if !pol.VersionUnknown || pol.UpdateAvailable {
		t.Errorf("policy = %+v — 'cannot tell' must still not render as an update", pol)
	}
	if !pol.VersionNotRecorded {
		t.Error("an app with no recorded version is told it cannot be compared, which reads as a fault")
	}

	// The same empty version with no release is an app whose first build has
	// not landed, and the copy for that has always been its own.
	building := e.installExtension(t, "admin-user", "")
	if p := e.policyOf(t, "admin-user", building.ID); p.VersionNotRecorded {
		t.Error("an app whose first build has not landed was told its version was not recorded")
	}

	// And a version that *is* recorded and will not parse keeps the other
	// sentence, because for that one the platform really cannot tell.
	nightly := e.installExtension(t, "admin-user", "nightly")
	if p := e.policyOf(t, "admin-user", nightly.ID); p.VersionNotRecorded {
		t.Error("a recorded-but-unparseable version was reported as not recorded")
	}
}

func TestUpdateRefusalsEachNameTheirReason(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	t.Run("already current", func(t *testing.T) {
		app := e.installExtension(t, "admin-user", "1.4.0")
		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already running") {
			t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
		}
		if pol := e.policyOf(t, "admin-user", app.ID); pol.UpdateAvailable {
			t.Error("an app on the catalog's version must not be offered an update")
		}
	})

	t.Run("newer than the catalog", func(t *testing.T) {
		app := e.installExtension(t, "admin-user", "2.0.0")
		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("= %d: %s — a catalog rollback is not an upgrade", rec.Code, rec.Body.String())
		}
	})

	t.Run("versions cannot be compared", func(t *testing.T) {
		app := e.installExtension(t, "admin-user", "nightly")
		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "cannot be compared") {
			t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
		}
		pol := e.policyOf(t, "admin-user", app.ID)
		if !pol.VersionUnknown || pol.UpdateAvailable {
			t.Errorf("policy = %+v — 'cannot tell' must not render as an update", pol)
		}
	})

	t.Run("an example has no update channel", func(t *testing.T) {
		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
		var body struct {
			App database.App `json:"app"`
		}
		json.Unmarshal(rec.Body.Bytes(), &body)

		rec = e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+body.App.ID+"/gallery/update", "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "snapshot") {
			t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
		}
		if pol := e.policyOf(t, "publisher", body.App.ID); pol.Updatable {
			t.Error("an example must not report an update channel")
		}
	})

	t.Run("a publisher may not update an extension", func(t *testing.T) {
		app := e.installExtension(t, "admin-user", "1.3.0")
		// The publisher cannot edit somebody else's app at all, which is the
		// first refusal; the admin-only rule is the second. Either is a
		// refusal, and neither may be a 202.
		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
		if rec.Code == http.StatusAccepted {
			t.Fatalf("a publisher updated an extension: %s", rec.Body.String())
		}
	})
}

// An item that leaves the catalog must not leave its installs looking current.
func TestARetiredItemIsSaidToBeRetired(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.4.0")

	// The catalog is republished without the extension.
	e.catalog = `{"schema_version":1,"items":[]}`
	if err := e.svc.Sync(t.Context()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	pol := e.policyOf(t, "admin-user", app.ID)
	if !pol.Retired || pol.UpdateAvailable {
		t.Fatalf("policy = %+v, want retired and no update", pol)
	}
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/gallery/update", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no longer listed") {
		t.Fatalf("update of a retired item = %d: %s", rec.Code, rec.Body.String())
	}
}

// Off means the gallery is not part of this install, including on the page of
// an app that came from it.
func TestThePolicyBlockDisappearsWhenTheGalleryIsOff(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.3.0")

	setGalleryMode(t, e.srv.db, database.GalleryOff)

	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/apps/"+app.ID, "")
	if strings.Contains(rec.Body.String(), "\"gallery\"") {
		t.Errorf("the app page still describes the gallery: %s", rec.Body.String())
	}
	// And the provenance is still on the row — switching the gallery off
	// changes what is offered, never what is recorded.
	after, _ := e.srv.db.GetApp(app.ID)
	if after.GalleryItemID == "" {
		t.Error("switching the gallery off erased an app's provenance")
	}
}

// A rollback carries the version back with the code, or an app that rolled out
// of a bad update would go on claiming the version it had just abandoned.
func TestRollbackCarriesTheGalleryVersion(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.3.0")

	source := &database.Deployment{
		AppID: app.ID, Branch: "v1.3.0", Status: database.DeployStatusSuccess,
		GalleryVersion: "1.3.0",
	}
	if err := e.srv.db.CreateDeployment(source, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got, err := e.srv.db.GetDeployment(source.ID)
	if err != nil || got.GalleryVersion != "1.3.0" {
		t.Fatalf("the deployment did not keep its version: %+v (%v)", got, err)
	}
}

// The gallery and the source policy, which is where the two designs meet.
//
// The catalog is fetched from a repository the Toft team controls and its
// extensions point at public GitHub. Exempting the gallery from a restricted
// source policy would therefore leave a working one-click deploy from
// github.com on an install that had just blocked github.com — the single most
// embarrassing possible gap in this control.
func TestGalleryDeployIsOffWhileTheSourcePolicyIsRestricted(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	if err := e.srv.db.SetSourcePolicy(sources.ModeAllowlist, true,
		[]database.AllowedGitSource{{Pattern: "gitlab.internal/*"}}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	// The catalog stays browsable — it has reference value — behind one banner
	// rather than the same sentence repeated on every card.
	listing := decodeGallery(t, e.do(t, "admin-user", http.MethodGet, "/api/v1/gallery", ""))
	if listing.SourceBlocked == "" {
		t.Fatal("the listing carries no banner")
	}
	if len(listing.Items) != 2 {
		t.Fatalf("the catalog stopped being browsable: %d items", len(listing.Items))
	}
	for _, it := range listing.Items {
		if it.Deployable {
			t.Errorf("%s is still deployable", it.ID)
		}
	}

	// An extension deploy never passes through POST /apps, and an example
	// deploy never passes through POST /apps/upload — so neither creation point
	// can catch them, and each route has to refuse on its own.
	for _, id := range []string{"ext-1", "ex-1"} {
		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/"+id+"/deploy", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("deploying %s = %d, want 403 (%s)", id, rec.Code, rec.Body.String())
		}
	}
}

// An extension update is a re-clone at a new ref on an app that already exists.
// Only the gallery route and the builder can catch it.
func TestGalleryUpdateIsRefusedUnderARestrictedPolicy(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first deploy = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		App database.App `json:"app"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// A newer version, so the update would otherwise be offered.
	e.catalog = strings.Replace(testCatalog, `"version":"1.4.0"`, `"version":"1.5.0"`, 1)
	e.catalog = strings.Replace(e.catalog, `"ref":"v1.4.0"`, `"ref":"v1.5.0"`, 1)
	if err := e.svc.Sync(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if err := e.srv.db.SetSourcePolicy(sources.ModeNone, true, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
	rec = e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+created.App.ID+"/gallery/update", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("gallery update = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

// An example is a ZIP by design, so it reaches an install through the upload
// switch. Without this, the one path that fetches an archive on the platform's
// own initiative would be the one path that ignored the switch turning
// archives off.
func TestGalleryExampleFollowsTheUploadSwitch(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	if err := e.srv.db.SetSourcePolicy(sources.ModeAny, false, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ex-1/deploy", ""); rec.Code != http.StatusForbidden {
		t.Errorf("deploying an example with uploads off = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// The git policy is untouched, so an extension is fine.
	if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", ""); rec.Code != http.StatusCreated {
		t.Errorf("an extension was blocked by the upload switch: %d (%s)", rec.Code, rec.Body.String())
	}
}

// moveCatalogTo republishes the catalog with the extension at a new version and
// a matching ref, then syncs — an extension's update arriving the way one
// really arrives, by the vendor's repository moving under an install that has
// already deployed it.
func (e *galleryEnv) moveCatalogTo(t *testing.T, version, ref string) {
	t.Helper()
	e.catalog = strings.Replace(testCatalog, `"version":"1.4.0"`, `"version":"`+version+`"`, 1)
	e.catalog = strings.Replace(e.catalog, `"ref":"v1.4.0"`, `"ref":"`+ref+`"`, 1)
	if err := e.svc.Sync(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// lastDeploy is the row a deploy route just created.
func (e *galleryEnv) lastDeploy(t *testing.T, appID string) database.Deployment {
	t.Helper()
	deploys, err := e.srv.db.ListDeployments(appID)
	if err != nil || len(deploys) == 0 {
		t.Fatalf("deployments = %+v (%v)", deploys, err)
	}
	return deploys[0]
}

// Redeploy on an extension follows the catalog.
//
// The ref an extension carries is the one the catalog named when it was
// installed, so before this a redeploy rebuilt the version the app was already
// running — which made Redeploy the one button on an extension that quietly
// meant "put the old version back", on an install that had been told for weeks
// that a newer version existed.
func TestRedeployingAnExtensionTakesTheCatalogsVersion(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.4.0")
	e.moveCatalogTo(t, "1.5.0", "v1.5.0")

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
	}

	deploy := e.lastDeploy(t, app.ID)
	if deploy.Branch != "v1.5.0" || deploy.GalleryVersion != "1.5.0" {
		t.Errorf("the redeploy builds %q carrying %q, want the catalog's ref and version",
			deploy.Branch, deploy.GalleryVersion)
	}
	after, _ := e.srv.db.GetApp(app.ID)
	if after.Branch != "v1.5.0" {
		t.Errorf("branch = %q, want the ref that will be cloned", after.Branch)
	}
	// The same rule as the Update button: the recorded version is what is
	// *running*, and nothing has been built yet.
	if after.GalleryVersion != "1.4.0" {
		t.Errorf("gallery_version = %q before the build finished", after.GalleryVersion)
	}
	// One answer to "who took this extension to 1.5.0", whichever button was
	// pressed.
	rows := auditRows(t, e.srv.db, "gallery.update")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want the redeploy audited as the update it is", len(rows))
	}
	if !strings.Contains(rows[0].Detail, "1.5.0") || !strings.Contains(rows[0].Detail, "redeploy") {
		t.Errorf("audit detail = %s", rows[0].Detail)
	}
}

// The follow happens only where taking an update is already allowed — and where
// it is not, the answer is the app's own ref rather than a refusal, because a
// redeploy has to keep working.
func TestRedeployFallsBackToTheAppsOwnRef(t *testing.T) {
	t.Run("an editor who is not an administrator", func(t *testing.T) {
		e := newGalleryEnv(t, database.GalleryAll)
		app := e.installExtension(t, "admin-user", "1.4.0")
		if err := e.srv.db.UpsertGrant(t.Context(), app.ID, "user", "publisher",
			database.ObjectRoleEditor, "admin-user", database.RequestMeta{}); err != nil {
			t.Fatalf("UpsertGrant: %v", err)
		}
		e.moveCatalogTo(t, "1.5.0", "v1.5.0")

		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("redeploy = %d: %s — an editor may still redeploy", rec.Code, rec.Body.String())
		}
		deploy := e.lastDeploy(t, app.ID)
		if deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "" {
			t.Errorf("an editor's redeploy took %q/%q — extensions are updated by administrators",
				deploy.Branch, deploy.GalleryVersion)
		}
		if rows := auditRows(t, e.srv.db, "gallery.update"); len(rows) != 0 {
			t.Errorf("audit rows = %d", len(rows))
		}
	})

	t.Run("an item that has left the catalog", func(t *testing.T) {
		e := newGalleryEnv(t, database.GalleryAll)
		app := e.installExtension(t, "admin-user", "1.4.0")
		e.catalog = `{"schema_version":1,"items":[]}`
		if err := e.svc.Sync(t.Context()); err != nil {
			t.Fatalf("sync: %v", err)
		}

		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("redeploy = %d: %s — a retired item must not strand its installs", rec.Code, rec.Body.String())
		}
		if deploy := e.lastDeploy(t, app.ID); deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "" {
			t.Errorf("a retired item's redeploy = %q/%q", deploy.Branch, deploy.GalleryVersion)
		}
	})

	t.Run("a version this build is too old for", func(t *testing.T) {
		e := newGalleryEnv(t, database.GalleryAll)
		app := e.installExtension(t, "admin-user", "1.4.0")
		e.catalog = strings.Replace(testCatalog, `"version":"1.4.0"`,
			`"version":"1.5.0","min_platform_version":"9.0.0"`, 1)
		e.catalog = strings.Replace(e.catalog, `"ref":"v1.4.0"`, `"ref":"v1.5.0"`, 1)
		if err := e.svc.Sync(t.Context()); err != nil {
			t.Fatalf("sync: %v", err)
		}

		if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", ""); rec.Code != http.StatusAccepted {
			t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
		}
		if deploy := e.lastDeploy(t, app.ID); deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "" {
			t.Errorf("redeploy took a version this build is too old for: %q/%q",
				deploy.Branch, deploy.GalleryVersion)
		}
	})

	t.Run("versions that cannot be compared", func(t *testing.T) {
		e := newGalleryEnv(t, database.GalleryAll)
		app := e.installExtension(t, "admin-user", "nightly")
		e.moveCatalogTo(t, "1.5.0", "v1.5.0")

		if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", ""); rec.Code != http.StatusAccepted {
			t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
		}
		if deploy := e.lastDeploy(t, app.ID); deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "" {
			t.Errorf("redeploy = %q/%q — 'cannot tell' is not 'newer'", deploy.Branch, deploy.GalleryVersion)
		}
	})

	t.Run("the gallery is off", func(t *testing.T) {
		e := newGalleryEnv(t, database.GalleryAll)
		app := e.installExtension(t, "admin-user", "1.4.0")
		e.moveCatalogTo(t, "1.5.0", "v1.5.0")
		setGalleryMode(t, e.srv.db, database.GalleryOff)

		if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", ""); rec.Code != http.StatusAccepted {
			t.Fatalf("redeploy = %d: %s — an app whose gallery was switched off keeps deploying",
				rec.Code, rec.Body.String())
		}
		if deploy := e.lastDeploy(t, app.ID); deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "" {
			t.Errorf("a redeploy reached the catalog with the gallery off: %q/%q",
				deploy.Branch, deploy.GalleryVersion)
		}
	})

	t.Run("an example", func(t *testing.T) {
		e := newGalleryEnv(t, database.GalleryAll)
		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy", "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			App database.App `json:"app"`
		}
		json.Unmarshal(rec.Body.Bytes(), &body)
		// Stand in for the build, or the redeploy coalesces onto the install's
		// own queued deployment.
		first := e.lastDeploy(t, body.App.ID)
		first.Status = database.DeployStatusSuccess
		if err := e.srv.db.UpdateDeployment(&first); err != nil {
			t.Fatalf("UpdateDeployment: %v", err)
		}

		if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+body.App.ID+"/deploy", ""); rec.Code != http.StatusAccepted {
			t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
		}
		if deploy := e.lastDeploy(t, body.App.ID); deploy.GalleryVersion != "" {
			t.Errorf("an example's redeploy carried %q — an example is a snapshot", deploy.GalleryVersion)
		}
	})
}

// One-click deploy from the catalog is off while the source policy is
// restricted, and arriving through a different button does not change that. The
// redeploy itself is not refused — the app's own repository is allowed — it
// simply builds the code it is running.
func TestRedeployDoesNotFollowTheCatalogUnderARestrictedPolicy(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.4.0")
	e.moveCatalogTo(t, "1.5.0", "v1.5.0")

	if err := e.srv.db.SetSourcePolicy(sources.ModeAllowlist, true,
		[]database.AllowedGitSource{{Pattern: "github.com/acme/*"}},
		database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("redeploy = %d: %s — this app's repository is on the allowlist", rec.Code, rec.Body.String())
	}
	if deploy := e.lastDeploy(t, app.ID); deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "" {
		t.Errorf("a redeploy took a catalog version on a restricted install: %q/%q",
			deploy.Branch, deploy.GalleryVersion)
	}
}

// A first install whose build failed records no version, and the app then
// claims nothing while running the catalog's code. A redeploy of the ref the
// catalog names carries that version, so the record catches up without anybody
// having to detach and reinstall.
func TestRedeployRecordsTheVersionAFailedInstallNeverWrote(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "")
	if app.GalleryVersion != "" {
		t.Fatalf("gallery_version = %q, want the shape a failed first build leaves", app.GalleryVersion)
	}

	if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
	}
	deploy := e.lastDeploy(t, app.ID)
	if deploy.Branch != "v1.4.0" || deploy.GalleryVersion != "1.4.0" {
		t.Errorf("the retry builds %q carrying %q, want the catalog's ref and version",
			deploy.Branch, deploy.GalleryVersion)
	}
	// Not an update: nothing moved, so nothing is claimed to have.
	if rows := auditRows(t, e.srv.db, "gallery.update"); len(rows) != 0 {
		t.Errorf("audit rows = %d — recording a version nobody moved is not an update", len(rows))
	}
}

// **The catalog asks for a born state, and the install translates it into
// exactly two columns — never into anonymous reach.**
//
// The translation is the whole of the feature's security surface: a catalog is
// a document fetched over the network, and the one thing it must never be able
// to reach is `public`, which is the install's decision, gated on a transition
// and withdrawable install-wide. So the assertion is not only that each word
// produces the right pair — it is that no word produces `public`, including one
// that literally says so.
func TestGalleryBornVisibility(t *testing.T) {
	for _, tc := range []struct {
		word       string
		visibility string
		listable   bool
	}{
		// The default, and what every entry written before the field existed
		// still gets: private, and listed.
		{"", database.VisibilityPrivate, true},
		{"listable", database.VisibilityPrivate, true},
		// Everyone signed in may open it. Never anonymous.
		{"viewable", database.VisibilityAuthenticated, true},
		// Nobody but the owner and an administrator learns it exists.
		{"hidden", database.VisibilityPrivate, false},
		// A catalog asking for anonymous reach gets the default instead.
		{"public", database.VisibilityPrivate, true},
	} {
		t.Run("word="+tc.word, func(t *testing.T) {
			env := newGalleryEnv(t, database.GalleryAll)

			vis := ""
			if tc.word != "" {
				vis = `"visibility":"` + tc.word + `",`
			}
			env.catalog = `{"schema_version":1,"items":[
			  {"id":"ext-1","kind":"extension","name":"Born","version":"1.0.0",
			   "suggested_slug":"born",` + vis + `
			   "repo_url":"https://github.com/acme/born","ref":"v1.0.0"}
			]}`
			if err := env.svc.Sync(t.Context()); err != nil {
				t.Fatalf("sync: %v", err)
			}

			app := env.installExtension(t, "admin-user", "1.0.0")

			var visibility string
			var listable bool
			if err := env.srv.db.Conn().QueryRow(
				`SELECT visibility, listable FROM apps WHERE id = ?`, app.ID,
			).Scan(&visibility, &listable); err != nil {
				t.Fatalf("reading the row: %v", err)
			}
			if visibility != tc.visibility {
				t.Errorf("visibility = %q, want %q", visibility, tc.visibility)
			}
			if listable != tc.listable {
				t.Errorf("listable = %v, want %v", listable, tc.listable)
			}
			if visibility == database.VisibilityPublic {
				t.Fatalf("a catalog reached `public` with %q — anonymous reach is the install's "+
					"decision and no document fetched over the network may take it", tc.word)
			}
		})
	}
}

// Switching the gallery on fetches the catalog then, as the save says it does.
// It waited for the six-hourly timer, so a publisher opening Gallery straight
// after saw nothing (docs/bugs 2026-10-22-e2e-apptypes #1).
func TestSwitchingTheGalleryOnFetchesTheCatalog(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryOff)
	rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings",
		`{"gallery_deploy_mode":"`+database.GalleryAll+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switching the gallery on = %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := decodeGallery(t, e.do(t, "publisher", http.MethodGet, "/api/v1/gallery", ""))
		if len(resp.Items) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("five seconds after the gallery was switched on, a publisher still sees no catalog")
}

// A key the settings write does not read is refused by name, not answered 200
// over a write that changed nothing (drills 2026-10-29) — and the platform
// log's retention, which the Settings page has always sent, is one it reads.
func TestASettingThisInstallDoesNotReadIsRefused(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)

	for body, want := range map[string]string{
		`{"no_such_setting":1}`:      "no_such_setting",
		`{"max_job_memory_mb":2048}`: "job_memory_limit_mb",
	} {
		rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) ||
			!strings.Contains(rec.Body.String(), `"unknown_setting"`) {
			t.Errorf("%s = %d %s, want 400 unknown_setting naming %q", body, rec.Code, rec.Body.String(), want)
		}
	}

	rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings", `{"platform_log_retention_days":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform_log_retention_days = %d: %s", rec.Code, rec.Body.String())
	}
	if got, err := e.srv.db.GetPlatformSettings(); err != nil || got.PlatformLogRetentionDays != 30 {
		t.Errorf("platform_log_retention_days stored = %+v, %v; want 30", got.PlatformLogRetentionDays, err)
	}
	rec = e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings", `{"platform_log_retention_days":3}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a retention under the floor = %d, want 400", rec.Code)
	}
}

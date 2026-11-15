package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"toft/internal/database"
)

// connectEnv is an install with personal keys on, a publisher who owns nothing
// yet, and a second publisher to be refused.
type connectEnv struct {
	srv      *Server
	router   http.Handler
	alice    string // full-scope key
	reader   string // alice, read-scoped
	stranger string
}

func newConnectEnv(t *testing.T) *connectEnv {
	t.Helper()
	srv, router := keyEnv(t)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)
	return &connectEnv{
		srv:      srv,
		router:   router,
		alice:    mintKey(t, srv, "alice", "rstudio", nil, false),
		reader:   mintScopedKey(t, srv, "alice", "rstudio-read", database.UserKeyScopeRead, nil, false),
		stranger: mintKey(t, srv, "bob", "bobs", nil, false),
	}
}

// do sends what rsconnect sends: the /__api__ path and `Authorization: Key`.
func (e *connectEnv) do(t *testing.T, key, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/__api__"+path, body)
	if key != "" {
		req.Header.Set("Authorization", "Key "+key)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *connectEnv) doJSON(t *testing.T, key, method, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(v)
	return e.do(t, key, method, path, bytes.NewReader(b))
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
}

// shinyBundle is what rsconnect::deployApp() uploads for the smallest Shiny
// app: the source and a manifest at the top level of a gzipped tar.
func shinyBundle(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"app.R":         "library(shiny)\nshinyApp(fluidPage('hi'), function(input, output) {})\n",
		"manifest.json": `{"version":1,"metadata":{"appmode":"shiny"},"platform":"4.4.3","packages":{}}`,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// The probe rsconnect makes when a server is added takes no credential and
// must come back as JSON, or the server is refused as "not Connect".
func TestConnectServerSettingsAnswersUnauthenticated(t *testing.T) {
	e := newConnectEnv(t)
	rec := e.do(t, "", http.MethodGet, "/server_settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q, rsconnect requires JSON", ct)
	}
	var body map[string]any
	decodeInto(t, rec, &body)
	if body["version"] != connectCompatVersion {
		t.Fatalf("version = %v", body["version"])
	}
	// Both ways in are personal keys, and this install has them on.
	if body["tokens_enabled"] != true || body["api_key_auth"] != true {
		t.Fatalf("settings = %v, want both ways in reported on", body)
	}
}

// `Key` is translated for this prefix and no other: the platform API proper
// still answers it as no credential at all.
func TestConnectKeyHeaderIsReadOnlyUnderTheConnectPrefix(t *testing.T) {
	e := newConnectEnv(t)

	rec := e.do(t, e.alice, http.MethodGet, "/users/current", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("users/current = %d: %s", rec.Code, rec.Body.String())
	}
	var me map[string]any
	decodeInto(t, rec, &me)
	if me["username"] != "alice" || me["id"] != "alice" || me["user_role"] != "publisher" {
		t.Fatalf("users/current = %v", me)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Key "+e.alice)
	direct := httptest.NewRecorder()
	e.router.ServeHTTP(direct, req)
	if direct.Code == http.StatusOK {
		t.Fatalf("/api/v1/auth/me accepted the Key spelling: %s", direct.Body.String())
	}

	if rec := e.do(t, "", http.MethodGet, "/users/current", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous users/current = %d, want 401", rec.Code)
	}
	if rec := e.do(t, "toftu_nonsense", http.MethodGet, "/users/current", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d, want 401", rec.Code)
	}
}

// The whole publish, as rsconnect makes it: create, upload, deploy, poll.
func TestConnectPublishCreatesAnAppAndQueuesItsDeploy(t *testing.T) {
	e := newConnectEnv(t)

	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "my-shiny", "title": "My Shiny"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var content connectContent
	decodeInto(t, rec, &content)
	if content.GUID == "" || content.Name != "my-shiny" || content.Title != "My Shiny" || content.ContentURL == "" {
		t.Fatalf("content = %+v", content)
	}
	app, err := e.srv.db.GetApp(content.GUID)
	if err != nil || app == nil {
		t.Fatalf("app not created: %v", err)
	}
	if app.OwnerID != "alice" || !builder.IsUploadedSource(app.RepoURL) || app.Status != database.AppStatusStopped {
		t.Fatalf("app = owner %q, repo %q, status %q", app.OwnerID, app.RepoURL, app.Status)
	}

	rec = e.do(t, e.alice, http.MethodPost, "/v1/content/"+app.ID+"/bundles", bytes.NewReader(shinyBundle(t)))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle = %d: %s", rec.Code, rec.Body.String())
	}
	var bundle struct{ ID string }
	decodeInto(t, rec, &bundle)
	if !validBundleID(bundle.ID) {
		t.Fatalf("bundle id = %q", bundle.ID)
	}

	rec = e.doJSON(t, e.alice, http.MethodPost, "/v1/content/"+app.ID+"/deploy", map[string]string{"bundle_id": bundle.ID})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	var task struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, rec, &task)
	deploy, err := e.srv.db.GetDeployment(task.TaskID)
	if err != nil || deploy == nil || deploy.AppID != app.ID {
		t.Fatalf("task %q is not a deployment of the app: %+v %v", task.TaskID, deploy, err)
	}
	if _, err := os.Stat(filepath.Join(builder.UploadStagingDir(e.srv.cfg.AppsDir, app.ID), "app.R")); err != nil {
		t.Fatalf("bundle was not unpacked into staging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(connectBundleDir(e.srv.cfg.AppsDir, app.ID), bundle.ID+".tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("deployed bundle was left on disk: %v", err)
	}

	// No builder in a test, so the deploy sits queued: not finished, nothing to say.
	rec = e.do(t, e.alice, http.MethodGet, "/v1/tasks/"+task.TaskID+"?first=0&wait=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("task = %d: %s", rec.Code, rec.Body.String())
	}
	var tb map[string]any
	decodeInto(t, rec, &tb)
	if tb["finished"] != false {
		t.Fatalf("queued task reported finished: %v", tb)
	}

	// A second deploy of the same bundle has nothing to deploy.
	rec = e.doJSON(t, e.alice, http.MethodPost, "/v1/content/"+app.ID+"/deploy", map[string]string{"bundle_id": bundle.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("redeploying a consumed bundle = %d, want 404", rec.Code)
	}
}

// A taken name is refused, never suffixed: rsconnect finds content again by the
// name it sent.
func TestConnectCreateRefusesATakenName(t *testing.T) {
	e := newConnectEnv(t)
	if err := e.srv.db.CreateApp(&database.App{Slug: "taken", Name: "Taken", OwnerID: "bob",
		RepoURL: "upload://taken.zip", Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "taken"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("create over a taken name = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// AP-7: the Connect create applies the slug length rule the other creates do,
// refusing rather than cutting, in Connect's error shape.
func TestConnectCreateRefusesASlugTooLong(t *testing.T) {
	e := newConnectEnv(t)
	long := strings.Repeat("a", maxSlugLen+1)
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": long})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a %d-character name = %d, want 400: %s", len(long), rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeInto(t, rec, &body)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "at most") {
		t.Errorf("body = %v, want Connect's error naming the limit", body)
	}
	if taken, _ := e.srv.db.SlugExists(long); taken {
		t.Error("an app row was created for the refused name")
	}

	fits := strings.Repeat("b", maxSlugLen)
	if rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": fits}); rec.Code != http.StatusOK {
		t.Fatalf("create with a %d-character name = %d: %s", len(fits), rec.Code, rec.Body.String())
	}
}

// Somebody else's app is not found, and cannot be uploaded or deployed into.
func TestConnectRefusesAnotherPublishersContent(t *testing.T) {
	e := newConnectEnv(t)
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "alices"})
	var content connectContent
	decodeInto(t, rec, &content)

	for _, path := range []string{"/applications/" + content.ID, "/v1/content/" + content.GUID} {
		if rec := e.do(t, e.stranger, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("stranger GET %s = %d, want 404", path, rec.Code)
		}
	}
	rec = e.do(t, e.stranger, http.MethodPost, "/v1/content/"+content.GUID+"/bundles", bytes.NewReader(shinyBundle(t)))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("stranger upload = %d", rec.Code)
	}
	rec = e.do(t, e.stranger, http.MethodGet, "/applications?filter=name:alices", nil)
	var list struct {
		Applications []connectContent
		Total        int
	}
	decodeInto(t, rec, &list)
	if list.Total != 0 {
		t.Fatalf("stranger's listing found %d", list.Total)
	}
}

// A read-scoped key reads and does not publish — the scope guard applies here
// because this is the platform API.
func TestConnectReadScopedKeyCannotPublish(t *testing.T) {
	e := newConnectEnv(t)
	if rec := e.do(t, e.reader, http.MethodGet, "/users/current", nil); rec.Code != http.StatusOK {
		t.Fatalf("read-scoped users/current = %d", rec.Code)
	}
	rec := e.doJSON(t, e.reader, http.MethodPost, "/v1/content", map[string]string{"name": "nope"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-scoped create = %d, want 403", rec.Code)
	}
}

// Lookup by name, the question rsconnect asks before it creates anything.
func TestConnectListApplicationsByName(t *testing.T) {
	e := newConnectEnv(t)
	e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "one"})
	e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "two"})

	rec := e.do(t, e.alice, http.MethodGet, "/applications?filter=account_id:alice&filter=name:two&count=100&start=0&cont=", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Applications []connectContent
		Count, Total int
	}
	decodeInto(t, rec, &list)
	if list.Total != 1 || list.Count != 1 || list.Applications[0].Name != "two" || list.Applications[0].URL == "" {
		t.Fatalf("list = %+v", list)
	}
}

// Variables set through this API are secrets, and a null removes one.
func TestConnectEnvironmentIsWriteOnly(t *testing.T) {
	e := newConnectEnv(t)
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "envy"})
	var content connectContent
	decodeInto(t, rec, &content)

	rec = e.doJSON(t, e.alice, http.MethodPatch, "/v1/content/"+content.GUID+"/environment",
		[]map[string]any{{"name": "TOKEN", "value": "s3cret"}, {"name": "OTHER", "value": "x"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	vars, _ := e.srv.db.GetEnvVars(content.GUID)
	if len(vars) != 2 || !vars[0].Sensitive || !vars[1].Sensitive {
		t.Fatalf("vars = %+v", vars)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("a value came back: %s", rec.Body.String())
	}

	rec = e.doJSON(t, e.alice, http.MethodPatch, "/v1/content/"+content.GUID+"/environment",
		[]map[string]any{{"name": "OTHER", "value": nil}})
	var names []string
	decodeInto(t, rec, &names)
	if len(names) != 1 || names[0] != "TOKEN" {
		t.Fatalf("names = %v", names)
	}

	// Audited as the platform's own route audits it, and frozen by a lock
	// (security review 2026-11-10, B-2).
	var audited int
	e.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'app.env_change' AND target_id = ?`,
		content.GUID).Scan(&audited)
	if audited != 2 {
		t.Errorf("%d app.env_change rows, want 2", audited)
	}
	if _, err := e.srv.db.Conn().Exec(`UPDATE apps SET locked_at = now() WHERE id = ?`, content.GUID); err != nil {
		t.Fatal(err)
	}
	rec = e.doJSON(t, e.alice, http.MethodPatch, "/v1/content/"+content.GUID+"/environment",
		[]map[string]any{{"name": "TOKEN", "value": nil}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a locked app's environment changed: %d %s", rec.Code, rec.Body.String())
	}
}

// The Connect app reads answer nothing without a credential (security review
// 2026-11-10, B-1).
func TestConnectAppReadsRefuseAnonymousCallers(t *testing.T) {
	e := newConnectEnv(t)
	for _, path := range []string{"/applications", "/applications/x", "/v1/content/x"} {
		req := httptest.NewRequest(http.MethodGet, "/__api__"+path, nil)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, rec.Code)
		}
	}
}

// A finished deployment reports its log and, when it failed, why — the code
// is what rsconnect decides success on.
func TestConnectTaskReportsAFailedDeploy(t *testing.T) {
	e := newConnectEnv(t)
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "fails"})
	var content connectContent
	decodeInto(t, rec, &content)

	now := time.Now().UTC()
	d := &database.Deployment{AppID: content.GUID, Status: database.DeployStatusFailed, Branch: "upload",
		BuildLog: "==> one\n==> two\nERROR: it broke\n", FinishedAt: &now}
	if err := e.srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.db.UpdateDeployment(d); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.db.SetDeploymentFailure(d.ID, "build_failed", "", "it broke"); err != nil {
		t.Fatal(err)
	}

	rec = e.do(t, e.alice, http.MethodGet, "/v1/tasks/"+d.ID+"?first=1&wait=1", nil)
	var tb struct {
		Output   []string
		Finished bool
		Code     int
		Error    string
		Last     int
	}
	decodeInto(t, rec, &tb)
	if !tb.Finished || tb.Code != 1 || tb.Error != "it broke" || tb.Last != 3 ||
		len(tb.Output) != 2 || tb.Output[1] != "ERROR: it broke" {
		t.Fatalf("task = %+v", tb)
	}

	if rec := e.do(t, e.stranger, http.MethodGet, "/v1/tasks/"+d.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger task = %d, want 404", rec.Code)
	}
}

// A dependency policy's lock is lifted by a release that drops the package, so
// the bundle upload and the deploy admit it as POST /apps/{id}/upload does; an
// administrator's lock still refuses both (RO-3).
func TestConnectDeployAdmitsADependencyLockOnly(t *testing.T) {
	e := newConnectEnv(t)
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "locked-shiny", "title": "Locked"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var content connectContent
	decodeInto(t, rec, &content)

	lock := func(source string) {
		t.Helper()
		if _, err := e.srv.db.Conn().Exec(`UPDATE apps SET locked_at = now(), locked_source = ? WHERE id = ?`,
			source, content.GUID); err != nil {
			t.Fatal(err)
		}
	}

	lock(database.AppLockSourceDependency)
	rec = e.do(t, e.alice, http.MethodPost, "/v1/content/"+content.GUID+"/bundles", bytes.NewReader(shinyBundle(t)))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle under a dependency lock = %d: %s", rec.Code, rec.Body.String())
	}
	var bundle struct{ ID string }
	decodeInto(t, rec, &bundle)
	rec = e.doJSON(t, e.alice, http.MethodPost, "/v1/content/"+content.GUID+"/deploy", map[string]string{"bundle_id": bundle.ID})
	if rec.Code == http.StatusConflict {
		t.Fatalf("deploy under a dependency lock = 409: %s", rec.Body.String())
	}

	lock(database.AppLockSourceAdmin)
	rec = e.do(t, e.alice, http.MethodPost, "/v1/content/"+content.GUID+"/bundles", bytes.NewReader(shinyBundle(t)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("bundle under an administrator's lock = %d, want 409", rec.Code)
	}
	rec = e.doJSON(t, e.alice, http.MethodPost, "/v1/content/"+content.GUID+"/deploy", map[string]string{"bundle_id": bundle.ID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("deploy under an administrator's lock = %d, want 409", rec.Code)
	}
}

// AP-6: uploaded bundles waiting to be deployed are bounded per content item,
// and deploying one makes room for the next.
func TestConnectBundleUploadsAreBoundedPerContent(t *testing.T) {
	e := newConnectEnv(t)
	rec := e.doJSON(t, e.alice, http.MethodPost, "/v1/content", map[string]string{"name": "many-bundles"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var content connectContent
	decodeInto(t, rec, &content)
	upload := func() *httptest.ResponseRecorder {
		return e.do(t, e.alice, http.MethodPost, "/v1/content/"+content.GUID+"/bundles", bytes.NewReader(shinyBundle(t)))
	}

	var first struct{ ID string }
	for i := 0; i < maxWaitingConnectBundles; i++ {
		rec := upload()
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d = %d: %s", i+1, rec.Code, rec.Body.String())
		}
		if i == 0 {
			decodeInto(t, rec, &first)
		}
	}
	if rec := upload(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("upload past the limit = %d, want 429: %s", rec.Code, rec.Body.String())
	}

	rec = e.doJSON(t, e.alice, http.MethodPost, "/v1/content/"+content.GUID+"/deploy", map[string]string{"bundle_id": first.ID})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := upload(); rec.Code != http.StatusOK {
		t.Fatalf("upload after a deploy = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

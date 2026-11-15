package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The mode a *new* app is born in.
//
// Three create paths — a git URL, a ZIP upload, a gallery item — and one
// decision shared between them. These tests exist mostly to hold that "one":
// a fourth path, or a copy of the ladder that drifts, is the failure this
// whole file is arranged against.

func principal(role string) *auth.Principal {
	return &auth.Principal{Type: database.PrincipalTypeUser, SystemRole: role}
}

// The ladder itself, without a router in the way. Every branch, in the order
// resolveCreateMode takes them.
func TestResolveCreateMode(t *testing.T) {
	available := runtime.Capabilities{
		Topology: runtime.TopologyKubernetes, IsolatedPossible: true, IsolatedEnabled: true,
	}
	notEnabled := runtime.Capabilities{Topology: runtime.TopologyKubernetes, IsolatedPossible: true}
	noCluster := runtime.Capabilities{Topology: runtime.TopologyKubernetes}
	onAVM := runtime.VMCapabilities()

	admin := principal(database.SystemRoleAdmin)
	publisher := principal(database.SystemRolePublisher)

	cases := []struct {
		name           string
		who            *auth.Principal
		caps           runtime.Capabilities
		requested      string
		installDefault string
		wantMode       runtime.Mode
		wantCode       int
		wantWords      []string
	}{
		// Saying nothing is never a refusal, whoever is asking. This is the
		// case every existing client is in.
		{name: "absent, publisher", who: publisher, caps: available, requested: "", wantMode: runtime.ModeShared},
		{name: "absent, admin", who: admin, caps: available, requested: "", wantMode: runtime.ModeShared},
		{name: "whitespace is absent", who: publisher, caps: available, requested: "  ", wantMode: runtime.ModeShared},

		// The install's default reaches the people who may not choose. This is
		// the whole point of D3: the per-request field is administrator-only,
		// and most apps are created by publishers.
		{name: "default isolated, publisher", who: publisher, caps: available,
			installDefault: "isolated", wantMode: runtime.ModeIsolated},
		{name: "default isolated, admin", who: admin, caps: available,
			installDefault: "isolated", wantMode: runtime.ModeIsolated},
		{name: "default shared", who: publisher, caps: available,
			installDefault: "shared", wantMode: runtime.ModeShared},

		// Absent, not dormant. A default whose capability has gone away
		// creates shared apps rather than apps that cannot build — and it is
		// never a refusal, because the person creating the app asked for
		// nothing and has nothing to fix.
		{name: "default isolated, switch off", who: publisher, caps: notEnabled,
			installDefault: "isolated", wantMode: runtime.ModeShared},
		{name: "default isolated, no cluster", who: publisher, caps: noCluster,
			installDefault: "isolated", wantMode: runtime.ModeShared},
		{name: "default isolated, on a VM", who: admin, caps: onAVM,
			installDefault: "isolated", wantMode: runtime.ModeShared},
		{name: "an unreadable default is shared", who: admin, caps: available,
			installDefault: "fargate", wantMode: runtime.ModeShared},

		// Naming a mode is still administrator-only on an isolated-by-default
		// install: a publisher gets the default, they do not get a choice.
		{name: "publisher names the default", who: publisher, caps: available,
			requested: "isolated", installDefault: "isolated",
			wantCode: http.StatusForbidden, wantWords: []string{"only an administrator"}},

		// Naming one is administrator-only — including naming the mode you
		// would have been given anyway. An API that ignores a field the caller
		// may not set teaches people that a setting took effect when it did not.
		{name: "publisher names isolated", who: publisher, caps: available, requested: "isolated",
			wantCode: http.StatusForbidden, wantWords: []string{"only an administrator"}},
		{name: "publisher names shared", who: publisher, caps: available, requested: "shared",
			wantCode: http.StatusForbidden, wantWords: []string{"only an administrator"}},
		{name: "anonymous names a mode", who: nil, caps: available, requested: "shared",
			wantCode: http.StatusForbidden},

		{name: "admin, isolated, available", who: admin, caps: available, requested: "isolated",
			wantMode: runtime.ModeIsolated},
		{name: "admin, shared", who: admin, caps: available, requested: "shared", wantMode: runtime.ModeShared},

		// The API's vocabulary is modes, never backends.
		{name: "a backend name is not a mode", who: admin, caps: available, requested: "fargate",
			wantCode: http.StatusBadRequest, wantWords: []string{"unknown execution mode"}},

		// 422, and the sentence differs by topology on purpose: one is a switch
		// an administrator can throw, one is configuration only an operator can
		// supply, and one is a dead end.
		{name: "isolated, not enabled", who: admin, caps: notEnabled, requested: "isolated",
			wantCode: http.StatusUnprocessableEntity, wantWords: []string{"turned off", "administrator can enable"}},
		{name: "isolated, no cluster", who: admin, caps: noCluster, requested: "isolated",
			wantCode: http.StatusUnprocessableEntity, wantWords: []string{"no isolated backend configured", "operator"}},
		{name: "isolated, on a VM", who: admin, caps: onAVM, requested: "isolated",
			wantCode: http.StatusUnprocessableEntity, wantWords: []string{"single machine", "no upgrade path"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, refusal := resolveCreateMode(tc.who, tc.caps, tc.requested, tc.installDefault)

			if tc.wantCode == 0 {
				if refusal != nil {
					t.Fatalf("refused with %d %q, want mode %q", refusal.status, refusal.message, tc.wantMode)
				}
				if mode != tc.wantMode {
					t.Errorf("mode = %q, want %q", mode, tc.wantMode)
				}
				return
			}

			if refusal == nil {
				t.Fatalf("resolved to %q, want a %d refusal", mode, tc.wantCode)
			}
			if refusal.status != tc.wantCode {
				t.Errorf("status = %d, want %d (message: %q)", refusal.status, tc.wantCode, refusal.message)
			}
			// A refusal resolves no mode: a caller that ignored the error must
			// not find a usable value sitting beside it.
			if mode != "" {
				t.Errorf("a refusal also returned mode %q", mode)
			}
			for _, want := range tc.wantWords {
				if !strings.Contains(refusal.message, want) {
					t.Errorf("message should mention %q, got: %q", want, refusal.message)
				}
			}
		})
	}
}

// The three create paths, driven through the real router.

type createModeEnv struct {
	srv    *Server
	router http.Handler
}

// newCreateModeEnv builds an install that *can* offer isolated mode, so that
// every refusal these tests see is the one they are about.
func newCreateModeEnv(t *testing.T) *createModeEnv {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), runtime.NewMockRuntime(nil))

	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.IsolatedEnabled = true
	if err := srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("enable isolated: %v", err)
	}

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)

	return &createModeEnv{srv: srv, router: srv.Router(http.NotFoundHandler())}
}

func (e *createModeEnv) create(t *testing.T, who, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// upload posts the multipart form the ZIP path reads, with execution_mode
// beside name and slug exactly as the handler expects it. The archive holds a
// page, not nothing: a create refuses a tree it cannot recognise.
func (e *createModeEnv) upload(t *testing.T, who, slug, mode string) *httptest.ResponseRecorder {
	t.Helper()
	page, _ := io.ReadAll(zipOf(t, map[string]string{"index.html": "<p>hi</p>\n"}))
	return e.uploadArchive(t, who, slug, mode, string(page))
}

// uploadArchive is upload with the archive's bytes given.
func (e *createModeEnv) uploadArchive(t *testing.T, who, slug, mode, archive string) *httptest.ResponseRecorder {
	t.Helper()

	var buf strings.Builder
	const boundary = "----tofttest"
	field := func(name, value string) {
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString(`Content-Disposition: form-data; name="` + name + `"` + "\r\n\r\n")
		buf.WriteString(value + "\r\n")
	}
	field("name", slug)
	field("slug", slug)
	if mode != "" {
		field("execution_mode", mode)
	}
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="file"; filename="app.zip"` + "\r\n")
	buf.WriteString("Content-Type: application/zip\r\n\r\n")
	buf.WriteString(archive + "\r\n")
	buf.WriteString("--" + boundary + "--\r\n")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/upload", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *createModeEnv) appBySlug(t *testing.T, slug string) *database.App {
	t.Helper()
	app, err := e.srv.db.GetAppBySlug(slug)
	if err != nil {
		return nil
	}
	return app
}

func createdID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		App struct {
			ID            string `json:"id"`
			ExecutionMode string `json:"execution_mode"`
		} `json:"app"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create response: %v (body: %s)", err, rec.Body.String())
	}
	return body.App.ID
}

// The point of the whole feature: an app that is isolated from its first build,
// with no shared release ever having existed.
func TestCreateAppInIsolatedMode(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"tool","name":"Tool","execution_mode":"isolated"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}

	app := e.appBySlug(t, "tool")
	if app == nil {
		t.Fatal("no app was created")
	}
	if app.ExecutionMode != string(runtime.ModeIsolated) {
		t.Errorf("execution_mode = %q, want %q", app.ExecutionMode, runtime.ModeIsolated)
	}
	// It has never run anywhere, which is the state the feature exists to
	// reach: the first release will be built for a runner image.
	if app.RunningMode != "" {
		t.Errorf("running_mode = %q on an app that has never deployed", app.RunningMode)
	}
	if id := createdID(t, rec); id != app.ID {
		t.Errorf("response app id = %q, row id = %q", id, app.ID)
	}
}

// Absent means the default, and the default is shared. Every client that
// existed before this field keeps working unchanged.
func TestCreateAppWithoutAModeIsShared(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/quiet","slug":"quiet","name":"Quiet"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "quiet"); app == nil || app.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode = %v, want shared", app)
	}
}

// A refused create leaves nothing behind — not an app row, and not a slug
// somebody now has to work around.
func TestCreateAppRefusedModeWritesNothing(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/nope","slug":"nope","name":"Nope","execution_mode":"isolated"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}

	if app := e.appBySlug(t, "nope"); app != nil {
		t.Fatalf("a refused create left an app row: %s in %q", app.ID, app.ExecutionMode)
	}
	// And the slug is still free, so the same person can create the app they
	// were entitled to create all along.
	if rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/nope","slug":"nope","name":"Nope"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the slug was taken by a refused create: %d %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "nope"); app == nil || app.Slug != "nope" {
		t.Error("the second create did not get the slug it asked for")
	}
}

// Sending the mode you would have been given anyway is still sending it.
func TestCreateAppRefusesTheHarmlessMode(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/same","slug":"same","name":"Same","execution_mode":"shared"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "same"); app != nil {
		t.Error("a refused create left an app row")
	}
}

// The install that cannot offer it says so in the body, with the sentence that
// names who can change it.
func TestCreateAppIsolatedIsRefusedWhenUnavailable(t *testing.T) {
	e := newCreateModeEnv(t)

	settings, _ := e.srv.db.GetPlatformSettings()
	settings.IsolatedEnabled = false
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("disable isolated: %v", err)
	}

	rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/off","slug":"off","name":"Off","execution_mode":"isolated"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body["error"], "turned off") {
		t.Errorf("the body should say the switch is off, got: %q", body["error"])
	}
	if app := e.appBySlug(t, "off"); app != nil {
		t.Error("a refused create left an app row")
	}
}

// The ZIP path reads the same field out of its multipart form.
func TestUploadAppInIsolatedMode(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.upload(t, "root", "zipped", "isolated")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "zipped"); app == nil || app.ExecutionMode != string(runtime.ModeIsolated) {
		t.Errorf("execution_mode = %v, want isolated", app)
	}
}

func TestUploadAppRefusedModeWritesNothing(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.upload(t, "pub", "zipnope", "isolated")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "zipnope"); app != nil {
		t.Error("a refused upload left an app row")
	}
}

func TestUploadAppWithoutAModeIsShared(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.upload(t, "pub", "zipquiet", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "zipquiet"); app == nil || app.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode = %v, want shared", app)
	}
}

// A toft.toml the deploy would refuse is refused by the create, with the
// code toft check gives it, and the create is undone: before, the app was
// created, the build failed it as manifest_invalid, and the slug stayed taken.
func TestUploadAppRefusesARemovedKeyAndKeepsNoSlug(t *testing.T) {
	e := newCreateModeEnv(t)

	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for name, body := range map[string]string{
		"app.py":    "print('hi')\n",
		"toft.toml": "[jobs.work]\ncommand = \"python app.py\"\nretries = 2\n",
	} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	rec := e.uploadArchive(t, "pub", "zipretries", "", zipped.String())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "removed_key" || body["hint"] == nil {
		t.Errorf("the refusal should carry removed_key and its hint: %s", rec.Body.String())
	}
	if app := e.appBySlug(t, "zipretries"); app != nil {
		t.Error("a refused create left an app row, and with it the slug")
	}
}

// A tree toft check refuses for something past the manifest's parse — a
// static app that declares a job, a Python pin this install has no build of —
// is refused by the create with check's reason, and leaves no app: before, the
// slug was claimed and the build failed it at detect (docs/bugs 2026-10-28 D6).
func TestUploadAppRefusesWhatCheckRefusesAndKeepsNoSlug(t *testing.T) {
	for _, tc := range []struct {
		slug, reason string
		files        map[string]string
	}{
		{"zipstaticjob", "manifest_invalid", map[string]string{
			"index.html": "<p>hi</p>\n",
			"toft.toml":  "[jobs.x]\ncommand = \"echo hi\"\n",
		}},
		{"zipbadpin", "runtime_version", map[string]string{
			"app.py":           "print('hi')\n",
			"requirements.txt": "\n",
			"toft.toml":        "[runtime]\npython = \"2.1\"\n",
		}},
		// The wrong directory: nothing Toft recognises. check said no, and a
		// create used to upload anyway and leave a failed app holding the slug
		// (docs/bugs 2026-10-28 #1).
		{"zipunrec", "unsupported_framework", map[string]string{
			"readme.txt": "notes\n",
		}},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			e := newCreateModeEnv(t)
			var zipped bytes.Buffer
			zw := zip.NewWriter(&zipped)
			for name, body := range tc.files {
				f, _ := zw.Create(name)
				_, _ = f.Write([]byte(body))
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			rec := e.uploadArchive(t, "pub", tc.slug, "", zipped.String())
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
			}
			var body map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["code"] != tc.reason || body["error"] == nil {
				t.Errorf("the refusal should carry %s and its sentence: %s", tc.reason, rec.Body.String())
			}
			if app := e.appBySlug(t, tc.slug); app != nil {
				t.Error("a refused create left an app row, and with it the slug")
			}
		})
	}
}

// The gallery path takes the field from its caller on the same terms — and
// takes it from nowhere else.

// isolatedGallery is a gallery install that can also offer isolated mode.
func isolatedGallery(t *testing.T) *galleryEnv {
	t.Helper()

	e := newGalleryEnv(t, database.GalleryAll)
	e.srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), runtime.NewMockRuntime(nil))

	settings, err := e.srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.IsolatedEnabled = true
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("enable isolated: %v", err)
	}
	return e
}

func TestGalleryDeployInIsolatedMode(t *testing.T) {
	e := isolatedGallery(t)

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy",
		`{"slug":"audit","execution_mode":"isolated"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}

	app, err := e.srv.db.GetAppBySlug("audit")
	if err != nil {
		t.Fatalf("no app created: %v", err)
	}
	if app.ExecutionMode != string(runtime.ModeIsolated) {
		t.Errorf("execution_mode = %q, want isolated", app.ExecutionMode)
	}
}

func TestGalleryDeployRefusedModeWritesNothing(t *testing.T) {
	e := isolatedGallery(t)

	// The *example*, because an extension is admin-only at an earlier gate:
	// a publisher is told the item does not exist before the mode is ever
	// read, and 404-before-403 is the right order for a disclosure. The
	// example is the item a publisher may genuinely deploy, so it is the one
	// that can reach this refusal.
	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy",
		`{"slug":"nope","execution_mode":"isolated"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
	if app, err := e.srv.db.GetAppBySlug("nope"); err == nil && app != nil {
		t.Errorf("a refused gallery deploy left an app row in %q", app.ExecutionMode)
	}
	// Nothing was fetched either. The refusal is upstream of the archive
	// download, so a refused deploy costs the install no outbound request.
	if e.zipHit != 0 {
		t.Errorf("a refused deploy fetched the example archive %d times", e.zipHit)
	}
}

// D8, and the reason this test is worth its length: the catalog is data, never
// authority. A JSON file the platform fetched from somewhere else does not get
// to decide that an install spends elastic capacity — so an item that names a
// mode is deployed shared, exactly as an item that does not.
func TestGalleryCatalogCannotChooseTheMode(t *testing.T) {
	e := isolatedGallery(t)

	e.catalog = strings.Replace(testCatalog,
		`"repo_url":"https://github.com/acme/audit-explorer"`,
		`"execution_mode":"isolated","repo_url":"https://github.com/acme/audit-explorer"`, 1)
	if e.catalog == testCatalog {
		t.Fatal("the catalog fixture changed shape; this test edits it by hand")
	}
	if err := e.svc.Sync(t.Context()); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	// Deployed by an administrator, who *could* have asked for isolated and
	// did not. The item asked instead, and the item does not get a vote.
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ext-1/deploy", `{"slug":"catalog-says"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}

	app, err := e.srv.db.GetAppBySlug("catalog-says")
	if err != nil {
		t.Fatalf("no app created: %v", err)
	}
	if app.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("the catalog chose the mode: execution_mode = %q, want shared", app.ExecutionMode)
	}
}

// D4: the audit says which mode a new app was born in, once.

// modeChangeRows returns the app.execution_mode.change rows written for an app,
// oldest first. Straight at the table: audit_log deliberately has very few
// readers, and a test that went through one of them would be testing that
// reader instead.
func modeChangeRows(t *testing.T, srv *Server, appID string) [][2]string {
	t.Helper()

	rows, err := srv.db.Conn().Query(
		`SELECT COALESCE(before::text,''), COALESCE(after::text,'') FROM audit_log
		 WHERE action = 'app.execution_mode.change' AND target_type = 'app' AND target_id = ?
		 ORDER BY occurred_at`, appID)
	if err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var before, after string
		if err := rows.Scan(&before, &after); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, [2]string{before, after})
	}
	return out
}

func TestIsolatedCreateIsAudited(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/born","slug":"born","name":"Born","execution_mode":"isolated"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	app := e.appBySlug(t, "born")

	got := modeChangeRows(t, e.srv, app.ID)
	if len(got) != 1 {
		t.Fatalf("wrote %d app.execution_mode.change rows, want exactly 1: %v", len(got), got)
	}
	// `before` names the mode the app would otherwise have had, so the row is
	// readable on its own — a reader does not have to know what a create
	// defaults to in order to know what changed.
	if !strings.Contains(got[0][0], string(runtime.ModeShared)) {
		t.Errorf("before = %q, want it to name shared", got[0][0])
	}
	if !strings.Contains(got[0][1], string(runtime.ModeIsolated)) {
		t.Errorf("after = %q, want it to name isolated", got[0][1])
	}
}

// An app that takes the default records nothing, for the same reason a create
// does not audit every column that took one.
func TestSharedCreateIsNotAudited(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/plain","slug":"plain","name":"Plain"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	app := e.appBySlug(t, "plain")

	if got := modeChangeRows(t, e.srv, app.ID); len(got) != 0 {
		t.Errorf("a shared create wrote %d mode-change rows: %v", len(got), got)
	}
}

// The upload path writes the same one row, because it goes through the same
// CreateAppTx — which is the point of putting it there rather than in each
// handler.
func TestIsolatedUploadIsAudited(t *testing.T) {
	e := newCreateModeEnv(t)

	if rec := e.upload(t, "root", "zipborn", "isolated"); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	app := e.appBySlug(t, "zipborn")

	if got := modeChangeRows(t, e.srv, app.ID); len(got) != 1 {
		t.Errorf("wrote %d mode-change rows, want exactly 1: %v", len(got), got)
	}
}

// D3: the install-wide default, end to end.

// setDefaultMode writes the setting straight to the database, for tests about
// what a *create* then does with it. The route that sets it has its own tests
// below.
func setDefaultMode(t *testing.T, srv *Server, mode string) {
	t.Helper()
	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.DefaultExecutionMode = mode
	if err := srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("set default mode: %v", err)
	}
}

func (e *createModeEnv) putSettings(t *testing.T, who, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/system/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// The default is what reaches a developer who may not choose — the reason D3
// exists at all.
func TestPublisherGetsTheInstallDefault(t *testing.T) {
	e := newCreateModeEnv(t)
	setDefaultMode(t, e.srv, string(runtime.ModeIsolated))

	rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/dev","slug":"dev","name":"Dev"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "dev"); app == nil || app.ExecutionMode != string(runtime.ModeIsolated) {
		t.Fatalf("execution_mode = %v, want isolated", app)
	}

	// A seed is still a decision worth recording: the audit says the app was
	// born isolated, whoever chose it.
	app := e.appBySlug(t, "dev")
	if got := modeChangeRows(t, e.srv, app.ID); len(got) != 1 {
		t.Errorf("wrote %d mode-change rows, want 1: %v", len(got), got)
	}
}

// Absent, not dormant. The capability goes away and new apps are shared again —
// with no refusal, because the person creating the app asked for nothing.
func TestDefaultDegradesWhenIsolatedIsWithdrawn(t *testing.T) {
	e := newCreateModeEnv(t)
	setDefaultMode(t, e.srv, string(runtime.ModeIsolated))

	settings, _ := e.srv.db.GetPlatformSettings()
	settings.IsolatedEnabled = false
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("disable isolated: %v", err)
	}

	rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/after","slug":"after","name":"After"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a withdrawn capability must not refuse a create that asked for nothing. body: %s",
			rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "after"); app == nil || app.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode = %v, want shared", app)
	}
}

// The default seeds; it never re-reads. An app that already exists keeps the
// mode it has, in either direction.
func TestTheDefaultNeverTouchesAnExistingApp(t *testing.T) {
	e := newCreateModeEnv(t)

	if rec := e.create(t, "pub", `{"repo_url":"https://github.com/acme/early","slug":"early","name":"Early"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	setDefaultMode(t, e.srv, string(runtime.ModeIsolated))

	if app := e.appBySlug(t, "early"); app == nil || app.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("changing the default moved an existing app to %v", app)
	}
}

// An administrator cannot make the default a mode this install does not offer:
// a stored default every create silently ignores is a setting that lies.
func TestDefaultIsolatedIsRefusedWhenUnavailable(t *testing.T) {
	e := newCreateModeEnv(t)

	settings, _ := e.srv.db.GetPlatformSettings()
	settings.IsolatedEnabled = false
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("disable isolated: %v", err)
	}

	rec := e.putSettings(t, "root", `{"default_execution_mode":"isolated"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
	}

	got, _ := e.srv.db.GetPlatformSettings()
	if got.DefaultExecutionMode != string(runtime.ModeShared) {
		t.Errorf("a refused write stored %q", got.DefaultExecutionMode)
	}
}

// One request may turn isolated execution on and make it the default, because
// the check reads the settings as the request leaves them.
func TestIsolatedCanBeEnabledAndMadeTheDefaultTogether(t *testing.T) {
	e := newCreateModeEnv(t)

	settings, _ := e.srv.db.GetPlatformSettings()
	settings.IsolatedEnabled = false
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("disable isolated: %v", err)
	}

	rec := e.putSettings(t, "root", `{"isolated_execution_enabled":true,"default_execution_mode":"isolated"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got, _ := e.srv.db.GetPlatformSettings()
	if !got.IsolatedEnabled || got.DefaultExecutionMode != string(runtime.ModeIsolated) {
		t.Errorf("stored isolated=%v default=%q", got.IsolatedEnabled, got.DefaultExecutionMode)
	}
}

// A word that is not a mode is a malformed request, not an unavailable one.
func TestDefaultModeRejectsANonMode(t *testing.T) {
	e := newCreateModeEnv(t)

	rec := e.putSettings(t, "root", `{"default_execution_mode":"fargate"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
	}
}

// What the wizard reads to know what it is about to make.

func (e *createModeEnv) capabilities(t *testing.T, who string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities = %d. body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// A developer cannot read the install's settings — 404 — and still has to be
// told what mode their app is about to be created in, because they are about
// to wait tens of seconds longer for a deploy that behaves differently. This
// is the field that carries it.
func TestCapabilitiesSayWhatANewAppIsBornAs(t *testing.T) {
	e := newCreateModeEnv(t)

	if got := e.capabilities(t, "pub")["new_app_execution_mode"]; got != string(runtime.ModeShared) {
		t.Errorf("new_app_execution_mode = %v, want shared", got)
	}

	setDefaultMode(t, e.srv, string(runtime.ModeIsolated))
	if got := e.capabilities(t, "pub")["new_app_execution_mode"]; got != string(runtime.ModeIsolated) {
		t.Errorf("new_app_execution_mode = %v, want isolated", got)
	}

	// And it degrades exactly as a create would, so the screen cannot promise
	// a mode the create would not produce.
	settings, _ := e.srv.db.GetPlatformSettings()
	settings.IsolatedEnabled = false
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("disable isolated: %v", err)
	}
	if got := e.capabilities(t, "pub")["new_app_execution_mode"]; got != string(runtime.ModeShared) {
		t.Errorf("after the capability was withdrawn, new_app_execution_mode = %v, want shared", got)
	}

	// The settings route itself stays shut to them, which is why this field
	// exists at all.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/settings", nil)
	req.AddCookie(sessionCookieFor(t, e.srv, "pub"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a publisher reading the settings got %d, want 404", rec.Code)
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/sources"
)

// The seven enforcement points, each tested on its own so that none of them can
// come to rely on an earlier one having run. An eighth path that becomes a
// running app without consulting the resolver is a bug of the same class as a
// handler that forgets to authorize, and only per-point tests catch that.
//
// The two ZIP routes are tested here too and are *not* among the seven: they
// answer the upload switch, which is a different control.

type sourceEnv struct {
	db     *database.DB
	srv    *Server
	router http.Handler
}

func newSourceEnv(t *testing.T) *sourceEnv {
	t.Helper()
	db := dbtest.New(t)
	cfg := &config.Config{BaseURL: "http://localhost:8080", AppsDir: t.TempDir()}
	srv := newTestServerWithConfig(t, db, cfg)
	seedUser(t, db, "admin-user", database.SystemRoleAdmin)
	seedUser(t, db, "publisher", database.SystemRolePublisher)
	return &sourceEnv{db: db, srv: srv, router: srv.Router(http.NotFoundHandler())}
}

func (e *sourceEnv) do(t *testing.T, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	var req *http.Request
	if r != nil {
		req = httptest.NewRequest(method, path, r)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *sourceEnv) restrict(t *testing.T, patterns ...string) {
	t.Helper()
	entries := make([]database.AllowedGitSource, 0, len(patterns))
	for _, p := range patterns {
		entries = append(entries, database.AllowedGitSource{Pattern: p})
	}
	if err := e.db.SetSourcePolicy(sources.ModeAllowlist, true, entries, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
}

func (e *sourceEnv) app(t *testing.T, slug, repoURL string) *database.App {
	t.Helper()
	app := &database.App{
		ID: "app-" + slug, Slug: slug, Name: slug, OwnerID: "admin-user",
		RepoURL: repoURL, Branch: "main", Status: database.AppStatusRunning,
	}
	if err := e.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// Point 1: creating an app from a repository nobody approved.
func TestCreateRefusesABlockedSource(t *testing.T) {
	e := newSourceEnv(t)
	e.restrict(t, "github.com/acme/*")

	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps",
		`{"name":"Tool","repo_url":"https://github.com/randomguy/tool"}`)

	// 403, not 400: the URL is perfectly well-formed and the install simply
	// refuses it. 400 would invite a client to retry with a "corrected" URL.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /apps = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "github.com/randomguy/tool") || !strings.Contains(body, "github.com/acme/*") {
		t.Errorf("the refusal does not name what was refused and what would work: %s", body)
	}
	// And it is recorded — the number that answers "is anybody hitting this
	// fence" is the one that decides whether an exception workflow is needed.
	if rows := auditRows(t, e.db, "deploy_source_refused"); len(rows) != 1 {
		t.Errorf("deploy_source_refused written %d times, want 1", len(rows))
	}

	// The same request with an allowed source goes through, so the refusal is
	// the policy and not the route being broken.
	if rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps",
		`{"name":"Fine","repo_url":"https://github.com/acme/fine"}`); rec.Code != http.StatusCreated {
		t.Fatalf("an allowed source got %d (%s)", rec.Code, rec.Body.String())
	}
}

// An address or branch the builder would refuse is refused by the create
// itself, under an open policy, so it takes no slug and leaves no row.
func TestCreateRefusesWhatTheBuilderWouldRefuse(t *testing.T) {
	e := newSourceEnv(t)
	for _, body := range []string{
		`{"name":"A","slug":"bad-scheme","repo_url":"http://example.com/x/y.git"}`,
		`{"name":"B","slug":"bad-dash","repo_url":"-oProxyCommand=touch"}`,
		`{"name":"C","slug":"bad-branch","repo_url":"https://github.com/acme/c","branch":"--upload-pack=x"}`,
	} {
		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	for _, slug := range []string{"bad-scheme", "bad-dash", "bad-branch"} {
		if app, _ := e.db.GetAppBySlug(slug); app != nil {
			t.Errorf("a refused create left app %q behind", slug)
		}
	}
}

// Point 2: uploads have their own switch, because upload-only is a legitimate
// install and is the exact inverse of restricting git.
func TestUploadRefusedWhenUploadsAreOff(t *testing.T) {
	e := newSourceEnv(t)
	if err := e.db.SetSourcePolicy(sources.ModeAny, false, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps/upload", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /apps/upload = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// Git is untouched: the two switches are separate.
	if rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps",
		`{"name":"Git","repo_url":"https://github.com/anyone/thing"}`); rec.Code != http.StatusCreated {
		t.Fatalf("turning uploads off blocked a git deploy: %d (%s)", rec.Code, rec.Body.String())
	}
}

// A settings change never takes down running code, and never blocks the one
// operation an app with a bad release depends on.
func TestNonCompliantAppKeepsServingAndCanRollBack(t *testing.T) {
	e := newSourceEnv(t)
	app := e.app(t, "stranded", "https://github.com/randomguy/tool")
	e.restrict(t, "github.com/acme/*")

	// Still running, and its own page says why it cannot deploy.
	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/apps/"+app.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps/{id} = %d", rec.Code)
	}
	var detail struct {
		Policy appPolicyResponse `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding the app: %v", err)
	}
	if detail.Policy.SourceAllowed {
		t.Error("policy.source_allowed is true for an app on no pattern")
	}
	if !strings.Contains(detail.Policy.SourceReason, "github.com/acme/*") {
		t.Errorf("policy.source_reason does not say what would work: %q", detail.Policy.SourceReason)
	}

	// A deploy re-clones, so it is refused.
	if rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", ""); rec.Code != http.StatusForbidden {
		t.Errorf("deploy of a non-compliant app = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	// A rollback fetches nothing — the release is already on disk and was
	// vetted under the policy in force when it was built — so it is not
	// refused *by the source policy*. It fails here for want of a release,
	// which is a different answer entirely.
	rec = e.do(t, "admin-user", http.MethodPost, "/api/v1/apps/"+app.ID+"/deployments/nope/rollback", "")
	if rec.Code == http.StatusForbidden {
		t.Errorf("rollback was refused by the source policy: %s", rec.Body.String())
	}
}

// An uploaded app has no remote and is never non-compliant on the git policy.
// The upload switch governs it, and only at creation — a new ZIP for an app
// that already exists is not a new app.
func TestUploadedAppIsUnaffectedByTheGitPolicy(t *testing.T) {
	e := newSourceEnv(t)
	app := e.app(t, "zipped", "upload://thing.zip")
	if err := e.db.SetSourcePolicy(sources.ModeNone, false, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/apps/"+app.ID, "")
	var detail struct {
		Policy appPolicyResponse `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding the app: %v", err)
	}
	if !detail.Policy.SourceAllowed {
		t.Errorf("an uploaded app is reported non-compliant: %q", detail.Policy.SourceReason)
	}
}

// Re-pointing (D10) is what makes the switch adoptable on an install that
// already has apps: the alternative is deleting and recreating, which throws
// away env vars, storage, schedules, grants and history.
func TestRepointing(t *testing.T) {
	e := newSourceEnv(t)
	app := e.app(t, "moving", "https://github.com/randomguy/tool")
	e.restrict(t, "github.com/acme/*")

	// An owner cannot do it, however much they own the app: substituting the
	// codebase of an app other people trust is not content editing, and on a
	// restricted install it would be the way around the allowlist that an
	// exact-repository entry deliberately is.
	theirs := &database.App{
		ID: "app-theirs", Slug: "theirs", Name: "theirs", OwnerID: "publisher",
		RepoURL: "https://github.com/randomguy/theirs", Branch: "main", Status: database.AppStatusRunning,
	}
	if err := e.db.CreateApp(theirs); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if rec := e.do(t, "publisher", http.MethodPatch, "/api/v1/apps/"+theirs.ID,
		`{"repo_url":"https://github.com/acme/tool"}`); rec.Code != http.StatusForbidden {
		t.Errorf("an owner re-pointed their own app: %d (%s)", rec.Code, rec.Body.String())
	}

	// Nor may an administrator re-point at something the policy also refuses.
	if rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID,
		`{"repo_url":"https://github.com/elsewhere/tool"}`); rec.Code != http.StatusForbidden {
		t.Errorf("re-point to a blocked target = %d, want 403", rec.Code)
	}

	// Onto an approved mirror, though, is the whole point.
	rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID,
		`{"repo_url":"https://github.com/acme/tool"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-point = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	moved, err := e.db.GetApp(app.ID)
	if err != nil || moved == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if moved.RepoURL != "https://github.com/acme/tool" {
		t.Errorf("repo_url = %q", moved.RepoURL)
	}
	// The next deploy must be a full clone of a repository this platform has
	// never seen; a carried-over commit would have the poller believe it had
	// already deployed it.
	if moved.LastSeenSHA != "" {
		t.Errorf("last_seen_sha survived the re-point: %q", moved.LastSeenSHA)
	}
	if rows := auditRows(t, e.db, "app_source_changed"); len(rows) != 1 {
		t.Errorf("app_source_changed written %d times, want 1", len(rows))
	}
}

// The narrow view every signed-in user may read. Without it the New App form
// refuses blindly, and a refusal that does not say what would work sends the
// developer to Slack.
func TestEffectivePolicyIsReadableByANonAdministrator(t *testing.T) {
	e := newSourceEnv(t)
	e.restrict(t, "gitlab.internal/*")

	rec := e.do(t, "publisher", http.MethodGet, "/api/v1/system/source-policy/effective", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET effective = %d, want 200", rec.Code)
	}
	var eff effectiveSourcePolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &eff); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if eff.Mode != sources.ModeAllowlist || len(eff.Patterns) != 1 || eff.Patterns[0] != "gitlab.internal/*" {
		t.Fatalf("effective policy = %+v", eff)
	}

	// The administrator's view is not: it carries provenance, notes and usage
	// counts, which is a map of the fence rather than a fact a developer needs.
	if rec := e.do(t, "publisher", http.MethodGet, "/api/v1/system/source-policy", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a publisher read the full source policy: %d", rec.Code)
	}
}

// The preview is what an install with forty apps runs before saving. It must
// mutate nothing at all — including the pattern list it is asked about.
func TestPreviewMutatesNothing(t *testing.T) {
	e := newSourceEnv(t)
	e.app(t, "one", "https://github.com/acme/one")
	e.app(t, "two", "https://gitlab.internal/dept/two")

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/system/source-policy/preview",
		`{"mode":"allowlist","patterns":[{"pattern":"GitHub.com/ACME/*"},{"pattern":"bad.*/x"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", rec.Code, rec.Body.String())
	}
	var preview sourcePolicyPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decoding the preview: %v", err)
	}
	if preview.Count != 1 || preview.NonCompliant[0].Slug != "two" {
		t.Errorf("preview names %+v, want just the gitlab app", preview.NonCompliant)
	}
	// A pattern that changes under normalization is shown as it will be saved,
	// before saving, rather than discovered afterwards.
	if len(preview.Canonicalized) != 1 || preview.Canonicalized[0].Stored != "github.com/acme/*" {
		t.Errorf("canonicalization not reported: %+v", preview.Canonicalized)
	}
	// A bad entry is reported beside the answer rather than replacing it: the
	// administrator is mid-edit.
	if len(preview.Invalid) != 1 {
		t.Errorf("an unparseable pattern was swallowed: %+v", preview.Invalid)
	}

	// Nothing was stored.
	stored, err := e.db.ResolveSourcePolicy()
	if err != nil {
		t.Fatalf("ResolveSourcePolicy: %v", err)
	}
	if stored.Mode != sources.ModeAny || len(stored.Patterns) != 0 {
		t.Fatalf("the preview changed the stored policy: %+v", stored.Policy)
	}
}

// Saving warns rather than refuses. Every one of these is a real consequence
// the administrator may well have intended, and refusing a deliberate choice is
// how a control becomes something people work around.
func TestSavingWarnsAboutWhatItBreaks(t *testing.T) {
	e := newSourceEnv(t)
	e.app(t, "stranded", "https://github.com/randomguy/tool")

	rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/source-policy",
		`{"mode":"allowlist","uploads_enabled":true,"patterns":[{"pattern":"github.com/acme/*","note":"ours"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Policy  sourcePolicyResponse `json:"policy"`
		Warning string               `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Policy.NonCompliant != 1 || !strings.Contains(out.Warning, "running and serving") {
		t.Errorf("saving did not warn about the app it strands: %+v", out)
	}

	// The fence with a gate next to it, said out loud.
	rec = e.do(t, "admin-user", http.MethodPut, "/api/v1/system/source-policy",
		`{"mode":"none"}`)
	var second struct {
		Warning string `json:"warning"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if !strings.Contains(second.Warning, "running and serving") && !strings.Contains(second.Warning, "ZIP") {
		t.Errorf("mode none warned about nothing: %q", second.Warning)
	}

	// A mode nobody defined is refused by name rather than clamped: an
	// administrator who typed something meant something.
	if rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/source-policy",
		`{"mode":"whatever"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown mode = %d, want 400", rec.Code)
	}
	// And so is a pattern that cannot mean what it says.
	if rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/source-policy",
		`{"patterns":[{"pattern":"github.*/acme"}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a wildcard host = %d, want 400", rec.Code)
	}
}

// Seeding: the install offers what it is already deploying from, so a starting
// list is a few clicks rather than transcription.
func TestObservedSourcesEndpoint(t *testing.T) {
	e := newSourceEnv(t)
	e.app(t, "one", "https://github.com/acme/one")
	e.app(t, "two", "https://github.com/acme/two")

	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/system/source-policy/observed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("observed = %d", rec.Code)
	}
	var out struct {
		Sources []database.ObservedSource `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	found := false
	for _, o := range out.Sources {
		if o.Pattern == "github.com/acme/*" && o.Apps == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("the organization everything is under was not offered: %+v", out.Sources)
	}
	// Admin-only, like the rest of the administrator's half.
	if rec := e.do(t, "publisher", http.MethodGet, "/api/v1/system/source-policy/observed", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a publisher read the observed sources: %d", rec.Code)
	}
}

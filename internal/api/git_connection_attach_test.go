package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

// Attaching an app to a connected git host: who may, and to what.

// Test 5. **Coverage refuses an uncovered repository on both write paths**, and
// gives the same answer whether the URL was picked or typed.
//
// This is the rule that makes it safe for any developer to attach an app —
// there is no role gating it, deliberately, because a role would take the
// repository choice away from the person who knows which repository they want.
func TestCoverageRefusesAnUncoveredRepositoryOnBothWritePaths(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, tokenConnectionBody) // covers acme/*

	// Create.
	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps", `{
		"name":"Payroll","slug":"payroll","repo_url":"https://github.com/elsewhere/payroll",
		"branch":"main","git_connection_id":"`+conn.ID+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create outside coverage = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Both, because neither alone tells them whether to change the URL or to
	// ask an administrator.
	if !strings.Contains(body, "elsewhere/payroll") || !strings.Contains(body, "Acme GitHub") {
		t.Errorf("the refusal does not name the repository and the connection: %s", body)
	}
	// **Nothing is written.** A refused create leaves no app row and no slug
	// taken to explain away.
	if app, _ := e.db.GetAppBySlug("payroll"); app != nil {
		t.Error("a refused create left an app row behind")
	}

	// Create, inside coverage.
	rec = e.do(t, "publisher", http.MethodPost, "/api/v1/apps", `{
		"name":"Reports","slug":"reports","repo_url":"https://github.com/acme/reports",
		"branch":"main","git_connection_id":"`+conn.ID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create inside coverage = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	app, err := e.db.GetAppBySlug("reports")
	if err != nil || app == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if app.GitConnectionID != conn.ID {
		t.Errorf("git_connection_id = %q, want %q", app.GitConnectionID, conn.ID)
	}

	// Re-point, to somewhere the connection does not cover. Refused at the
	// moment of the mistake rather than at the next deploy with a hint —
	// **even though this request never mentions the connection**.
	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID,
		`{"repo_url":"https://github.com/elsewhere/reports"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("re-point outside coverage = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	moved, _ := e.db.GetApp(app.ID)
	if moved.RepoURL != "https://github.com/acme/reports" {
		t.Errorf("a refused re-point moved the app: %q", moved.RepoURL)
	}

	// Attaching an existing app to a connection that does not cover it.
	other := e.app(t, "elsewhere", "https://github.com/elsewhere/tool")
	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+other.ID,
		`{"git_connection_id":"`+conn.ID+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("attach outside coverage = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	unattached, _ := e.db.GetApp(other.ID)
	if unattached.GitConnectionID != "" {
		t.Error("a refused attach wrote the connection anyway")
	}
}

// A repository typed by hand meets exactly the check a picked one does, which
// is what lets the picker filter without being an enforcement point.
func TestATypedRepositoryMeetsTheSameCheck(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, tokenConnectionBody)

	// Owner compares whole. `acme/*` must not admit `acme-evil/*`, which is the
	// same class of mistake `HasSuffix` on a host is.
	for _, repo := range []string{
		"https://github.com/acme-evil/reports",
		"https://github.com/notacme/reports",
		"https://github.com/acme", // no repository at all
	} {
		rec := e.do(t, "publisher", http.MethodPost, "/api/v1/apps", `{
			"name":"X","slug":"x-`+strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(repo)+`",
			"repo_url":"`+repo+`","branch":"main","git_connection_id":"`+conn.ID+`"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 (%s)", repo, rec.Code, rec.Body.String())
		}
	}
}

// Test 11. `git_connection_id` from a key is refused on both routes.
//
// A deploy key belongs to nobody and is owner on every app. One that could set
// this field could grant itself the organization's git access — so the *field*
// is refused, not the route, because taking POST /apps off the deploy-key map
// would break every pipeline that creates its own app to fix a field those
// pipelines do not send.
func TestAKeyCannotAttachAnAppToAConnection(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, tokenConnectionBody)
	enablePersonalKeys(t, e.srv, true)
	key := mintKey(t, e.srv, "publisher", "ci", nil, false)
	// Owned by the key's own user, so a 404 for unreadability cannot stand in
	// for the refusal being tested.
	app := &database.App{
		ID: "app-keyed", Slug: "keyed", Name: "keyed", OwnerID: "publisher",
		RepoURL: "https://github.com/acme/keyed", Branch: "main", Status: database.AppStatusRunning,
	}
	if err := e.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/apps", `{"name":"K","slug":"k","repo_url":"https://github.com/acme/k",
			"branch":"main","git_connection_id":"` + conn.ID + `"}`},
		{http.MethodPatch, "/api/v1/apps/" + app.ID, `{"git_connection_id":"` + conn.ID + `"}`},
	} {
		rec := e.withKey(t, key, c.method, c.path, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a key = %d, want 403 (%s)", c.method, c.path, rec.Code, rec.Body.String())
			continue
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["code"] != "session_required" {
			t.Errorf("%s %s: code = %q, want session_required", c.method, c.path, body["code"])
		}
		if !strings.Contains(body["error"], "git_connection_id") {
			t.Errorf("%s %s: the refusal does not name the field: %q", c.method, c.path, body["error"])
		}
	}

	// The same key editing the same app *without* the field still works — the
	// field is what was taken away, not the route. A key that could no longer
	// touch its own app would be this rule overreaching into every pipeline.
	rec := e.withKey(t, key, http.MethodPatch, "/api/v1/apps/"+app.ID, `{"branch":"release"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("edit without the field = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// Test 4. `GET /git/connections` **never carries the auth method**, and reports
// `picker` false for a token-backed connection.
//
// The leak this guards is a field somebody adds for the admin page without
// noticing the same DTO is on a developer's route.
func TestTheDeveloperRouteNeverCarriesTheAuthMethod(t *testing.T) {
	e := newSourceEnv(t)
	createConnection(t, e, tokenConnectionBody)

	rec := e.do(t, "publisher", http.MethodGet, "/api/v1/git/connections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, forbidden := range []string{
		"auth_method", "clone_secret", "client_secret", "webhook_secret",
		"credential_kind", "credential_owner", "base_url", "app_id", "token",
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the developer's route carries %q: %s", forbidden, raw)
		}
	}

	var body struct {
		Connections []developerGitConnectionDTO `json:"connections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Connections) != 1 {
		t.Fatalf("connections = %d, want 1", len(body.Connections))
	}
	if body.Connections[0].Picker {
		t.Error("a token-backed connection reports a picker on the developer's route")
	}
	if body.Connections[0].DisplayName != "Acme GitHub" {
		t.Errorf("display_name = %q", body.Connections[0].DisplayName)
	}
	// Coverage is an administrator's declaration rather than a credential, and
	// a developer who can see it opens fewer tickets.
	if len(body.Connections[0].Coverage) != 1 {
		t.Errorf("coverage = %v", body.Connections[0].Coverage)
	}
}

// A disabled connection is absent from the developer's list: offering somebody
// a connection whose next deploy would fail is worse than offering none.
func TestADisabledConnectionIsNotOffered(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, tokenConnectionBody)

	rec := e.do(t, "admin-user", http.MethodPatch,
		"/api/v1/admin/git-connections/"+conn.ID, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d (%s)", rec.Code, rec.Body.String())
	}

	rec = e.do(t, "publisher", http.MethodGet, "/api/v1/git/connections", "")
	var body struct {
		Connections []developerGitConnectionDTO `json:"connections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Connections) != 0 {
		t.Errorf("a disabled connection is offered to developers: %+v", body.Connections)
	}
}

// The re-point note is a sentence about a token one person issued for one
// repository. It is false of a connection, which covers both — so it is emitted
// only when a pasted token is what is in force.
func TestTheRepointNoteIsOnlyAboutAPastedToken(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, `{
		"kind":"github","display_name":"Acme GitHub","auth_method":"token",
		"clone_secret":"github_pat_1","coverage":["acme/*"]}`)

	app := e.app(t, "moving", "https://github.com/acme/one")
	if err := e.db.SetGitToken(app.ID, "ciphertext"); err != nil {
		t.Fatalf("SetGitToken: %v", err)
	}
	if err := e.db.SetAppGitConnection(app.ID, conn.ID); err != nil {
		t.Fatalf("SetAppGitConnection: %v", err)
	}

	rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID,
		`{"repo_url":"https://github.com/acme/two"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-point = %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	message, _ := body["message"].(string)
	if strings.Contains(message, "app's settings") {
		t.Errorf("a connection-backed app is sent to a token field it does not use: %q", message)
	}

	// The same app with no connection gets the sentence, because for it the
	// sentence is true.
	if err := e.db.SetAppGitConnection(app.ID, ""); err != nil {
		t.Fatalf("SetAppGitConnection: %v", err)
	}
	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID,
		`{"repo_url":"https://github.com/acme/three"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	message, _ = body["message"].(string)
	if !strings.Contains(message, "app's settings") {
		t.Errorf("a token-backed app is not told its token is probably wrong: %q", message)
	}
}

// An app's own page says which credential is in force, so Settings does not
// show an empty token field beside a working app.
func TestTheAppSaysWhichCredentialIsInForce(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, tokenConnectionBody)
	app := e.app(t, "connected", "https://github.com/acme/connected")
	if err := e.db.SetAppGitConnection(app.ID, conn.ID); err != nil {
		t.Fatalf("SetAppGitConnection: %v", err)
	}

	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/apps/"+app.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	var detail struct {
		GitConnection *developerGitConnectionDTO `json:"git_connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if detail.GitConnection == nil || detail.GitConnection.DisplayName != "Acme GitHub" {
		t.Fatalf("git_connection = %+v", detail.GitConnection)
	}

	// Nil rather than an empty object for an app with none, so the install that
	// never adopted this feature renders the token field it always had.
	plain := e.app(t, "plain", "https://github.com/acme/plain")
	rec = e.do(t, "admin-user", http.MethodGet, "/api/v1/apps/"+plain.ID, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if detail.GitConnection != nil {
		t.Errorf("an app with no connection reports one: %+v", detail.GitConnection)
	}
}

// withKey issues a request carrying a bearer token and no session cookie.
func (e *sourceEnv) withKey(t *testing.T, secret, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// A deploy key belongs to nobody and is owner on every app, so it is a machine
// credential for this rule exactly as a personal key is.
func TestADeployKeyCannotSetAConnection(t *testing.T) {
	for name, p := range map[string]*auth.Principal{
		"personal key": {APIKeyID: "k1"},
		"deploy key":   {DeployKeyID: "dk1", DeployKeyScope: "deploy"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil)
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		if !refusedConnectionByKey(w, r, "git_connection_id") || w.Code != http.StatusForbidden {
			t.Errorf("%s: refused=%v status=%d, want a 403", name, w.Code == http.StatusForbidden, w.Code)
		}
	}
}

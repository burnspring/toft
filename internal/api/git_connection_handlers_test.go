package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/gitconn"
)

// Admin → Sources, the connections half.
//
// The connection is admin-only for the reason the sign-in providers are:
// whoever can create one has pointed this platform's builds at a credential.
// What is tested here is the surface — who may reach it, what comes back out,
// and the two verbs, which exist separately on purpose.

func createConnection(t *testing.T, e *sourceEnv, body string) adminGitConnectionDTO {
	t.Helper()
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/admin/git-connections", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var dto adminGitConnectionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decoding the connection: %v", err)
	}
	return dto
}

const tokenConnectionBody = `{
	"kind": "github",
	"display_name": "Acme GitHub",
	"auth_method": "token",
	"clone_secret": "github_pat_11ABCDEFG",
	"credential_owner": "platform-bot",
	"coverage": ["acme/*"]
}`

// A credential goes in and never comes out, and the page is told what it can
// see rather than being asked.
func TestConnectionSecretsAreWriteOnly(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, tokenConnectionBody)

	if !dto.CloneSecretSet {
		t.Error("clone_secret_set is false after storing one")
	}
	if strings.Contains(strings.ToLower(dto.Reach), "github_pat_11abcdefg") {
		t.Error("the reach sentence quotes the token")
	}

	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/admin/git-connections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "github_pat_11ABCDEFG") {
		t.Error("the stored credential came back on the list route")
	}
	// A fine-grained token, said from its own prefix and not from a field
	// anybody had to fill in.
	if dto.CredentialKind != database.GitCredFineGrained {
		t.Errorf("credential_kind = %q, want %q", dto.CredentialKind, database.GitCredFineGrained)
	}
	if dto.Reach == "" {
		t.Error("the page is told nothing about what the credential reaches")
	}
	// A token connection has no picker, and nothing about the method is a
	// developer's business — but the administrator's own page states it.
	if dto.Picker {
		t.Error("a token-backed connection reports a picker")
	}
	if dto.AuthMethod != database.GitAuthToken {
		t.Errorf("auth_method = %q", dto.AuthMethod)
	}
}

// A classic token is described rather than refused (Q10): it is the only kind
// some GHES versions can issue, and coverage bounds it either way.
func TestAClassicTokenIsDescribedRatherThanRefused(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, `{
		"kind": "github", "display_name": "Legacy", "auth_method": "token",
		"clone_secret": "ghp_0123456789", "coverage": ["acme/*"]}`)

	if dto.CredentialKind != database.GitCredClassic {
		t.Errorf("credential_kind = %q, want %q", dto.CredentialKind, database.GitCredClassic)
	}
	if !strings.Contains(dto.Reach, "Everything the account") {
		t.Errorf("the page does not state a classic token's reach: %q", dto.Reach)
	}
}

// Test 8. Remove is refused while apps depend on it, and the refusal names
// every one — "something is using this" is not a refusal anybody can act on.
func TestRemovingAConnectionWithAppsIsRefusedAndNamesThem(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, tokenConnectionBody)

	app := e.app(t, "reports", "https://github.com/acme/reports")
	if err := e.db.SetAppGitConnection(app.ID, dto.ID); err != nil {
		t.Fatalf("SetAppGitConnection: %v", err)
	}

	// The page can see the dependants before an administrator tries.
	rec := e.do(t, "admin-user", http.MethodGet,
		"/api/v1/admin/git-connections/"+dto.ID+"/apps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("listing dependants = %d (%s)", rec.Code, rec.Body.String())
	}
	var listed struct {
		Apps []database.GitConnectionApp `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(listed.Apps) != 1 || listed.Apps[0].Slug != "reports" {
		t.Fatalf("dependants = %+v", listed.Apps)
	}

	rec = e.do(t, "admin-user", http.MethodDelete, "/api/v1/admin/git-connections/"+dto.ID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reports") {
		t.Errorf("the refusal does not name the app: %s", rec.Body.String())
	}

	// **Disable is never refused**, which is what makes separating the verbs
	// worth its cost: the credential can always be turned off immediately, and
	// the row cannot be deleted out from under six teams by accident.
	rec = e.do(t, "admin-user", http.MethodPatch,
		"/api/v1/admin/git-connections/"+dto.ID, `{"enabled": false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	after, err := e.db.GetGitConnection(dto.ID)
	if err != nil {
		t.Fatalf("GetGitConnection: %v", err)
	}
	if after.Enabled {
		t.Error("the connection is still enabled after a disable")
	}
	// Disabling did not touch the credential.
	if !after.CloneSecretSet {
		t.Error("disabling cleared the stored credential")
	}
	if rows := auditRows(t, e.db, "git_connection.disabled"); len(rows) != 1 {
		t.Errorf("git_connection.disabled written %d times, want 1", len(rows))
	}

	// With the app moved off, removal goes through.
	if err := e.db.SetAppGitConnection(app.ID, ""); err != nil {
		t.Fatalf("SetAppGitConnection: %v", err)
	}
	rec = e.do(t, "admin-user", http.MethodDelete, "/api/v1/admin/git-connections/"+dto.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete after moving the app = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

// Admin-only, and 403 rather than a quiet empty list: whoever can create a
// connection has pointed this platform's builds at a credential.
func TestConnectionRoutesAreAdminOnly(t *testing.T) {
	e := newSourceEnv(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/git-connections", ""},
		{http.MethodPost, "/api/v1/admin/git-connections", tokenConnectionBody},
		{http.MethodPatch, "/api/v1/admin/git-connections/whatever", `{"enabled":false}`},
		{http.MethodDelete, "/api/v1/admin/git-connections/whatever", ""},
		{http.MethodGet, "/api/v1/admin/git-connections/whatever/apps", ""},
		{http.MethodPost, "/api/v1/admin/git-connections/whatever/test", ""},
		{http.MethodPost, "/api/v1/admin/git-connections/whatever/webhook-secret", ""},
		{http.MethodDelete, "/api/v1/admin/git-connections/whatever/webhook-secret", ""},
	} {
		if rec := e.do(t, "publisher", c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a publisher = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

// A connection has to be usable and bounded, or it is a credential with no
// boundary at all. Coverage is the boundary on a token connection, and there is
// no host to ask for one.
func TestAConnectionIsRefusedWithoutWhatMakesItSafe(t *testing.T) {
	e := newSourceEnv(t)
	for _, c := range []struct{ name, body, wants string }{
		{"no coverage", `{"kind":"github","display_name":"X","auth_method":"token",
			"clone_secret":"ghp_1"}`, "covers"},
		{"no credential", `{"kind":"github","display_name":"X","auth_method":"token",
			"coverage":["acme/*"]}`, "access token"},
		{"http base url", `{"kind":"github","display_name":"X","auth_method":"token",
			"base_url":"http://git.internal","clone_secret":"ghp_1","coverage":["acme/*"]}`, "https"},
		{"unknown kind", `{"kind":"bitbucket","display_name":"X","auth_method":"token",
			"clone_secret":"x","coverage":["acme/*"]}`, "github"},
		{"app method on gitlab", `{"kind":"gitlab","display_name":"X","auth_method":"app",
			"clone_secret":"key","app_id":"1"}`, "GitHub"},
	} {
		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/admin/git-connections", c.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422 (%s)", c.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), c.wants) {
			t.Errorf("%s: the refusal does not say what would work: %s", c.name, rec.Body.String())
		}
	}
}

// The base URL is a value in the row and never a constant — a clone is not a
// login, and a self-managed host is the ordinary case for the organizations
// this feature is for.
func TestBaseURLDefaultsByKindAndIsStored(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, tokenConnectionBody)
	if dto.BaseURL != "https://github.com" {
		t.Errorf("base_url = %q, want the public default", dto.BaseURL)
	}

	enterprise := createConnection(t, e, `{
		"kind":"github","display_name":"Acme GHES","auth_method":"token",
		"base_url":"https://git.acme.internal/","clone_secret":"ghp_2","coverage":["acme/*"]}`)
	if enterprise.BaseURL != "https://git.acme.internal" {
		t.Errorf("base_url = %q, want the stored host with no trailing slash", enterprise.BaseURL)
	}
}

// Step 5's whole claim: a GitLab connection is the `hold` path with a different
// host and no picker. If this needed anything the GitHub token connection did
// not, the shape D6 asserts would not be a shape.
func TestAGitLabConnectionIsTheSameFeature(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, `{
		"kind":"gitlab","display_name":"Acme GitLab","auth_method":"token",
		"base_url":"https://gitlab.acme.internal",
		"clone_secret":"glpat-abcdefghij","credential_owner":"platform-bot",
		"coverage":["acme/team/*","acme/payroll"]}`)

	// A group token belongs to the group rather than to a person, which is the
	// one thing it does not give up that a GitHub PAT does.
	if dto.CredentialKind != database.GitCredGroup {
		t.Errorf("credential_kind = %q, want %q", dto.CredentialKind, database.GitCredGroup)
	}
	if !strings.Contains(dto.Reach, "does not stop working when somebody leaves") {
		t.Errorf("the page does not state what a group token buys: %q", dto.Reach)
	}
	// No picker: a group token is not an OAuth client, and registering one
	// beside it would be a second artifact on the kind chosen because creating
	// things was the obstacle.
	if dto.Picker {
		t.Error("a GitLab connection reports a picker")
	}

	// GitLab nests subgroups, and coverage reads that with the same rule.
	app := e.app(t, "payroll", "https://gitlab.acme.internal/acme/team/payroll")
	rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+app.ID,
		`{"git_connection_id":"`+dto.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("attaching a covered subgroup repository = %d (%s)", rec.Code, rec.Body.String())
	}

	outside := e.app(t, "other", "https://gitlab.acme.internal/other/team/thing")
	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/apps/"+outside.ID,
		`{"git_connection_id":"`+dto.ID+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("attaching an uncovered repository = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

// The expiry is a merge-patch field: absent keeps it, an explicit null clears
// it. A null read as absent kept the old date, and the row went on to say
// Expired for a credential that does not expire.
func TestAConnectionsExpiryClearsOnNullAndStaysWhenAbsent(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, tokenConnectionBody)
	patch := func(body string) *database.GitConnection {
		t.Helper()
		rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/admin/git-connections/"+dto.ID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d (%s)", body, rec.Code, rec.Body.String())
		}
		c, err := e.db.GetGitConnection(dto.ID)
		if err != nil {
			t.Fatalf("GetGitConnection: %v", err)
		}
		return c
	}

	if c := patch(`{"credential_expires_at":"2030-01-02T00:00:00Z"}`); c.CredentialExpiresAt == nil {
		t.Fatal("setting the expiry stored nothing")
	}
	if c := patch(`{"display_name":"Renamed"}`); c.CredentialExpiresAt == nil {
		t.Error("a PATCH without the field cleared the expiry")
	}
	if c := patch(`{"credential_expires_at":null}`); c.CredentialExpiresAt != nil {
		t.Errorf("an explicit null kept the expiry: %v", c.CredentialExpiresAt)
	}
}

// Without a resolver there is nothing to test with. Said, rather than panicked
// through a nil receiver — the rule the mailer's own routes follow.
func TestTestingAConnectionWithoutAResolverExplainsItself(t *testing.T) {
	e := newSourceEnv(t)
	e.srv.gitConn = gitconn.NewResolver(e.db, e.srv.encryptor)
	dto := createConnection(t, e, tokenConnectionBody)
	e.srv.gitConn = nil

	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/admin/git-connections/"+dto.ID+"/test", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("test with no resolver = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}

// The webhook is opt-in: most organizations let no git host call in, so a
// connection starts with no secret and polls. POST …/webhook-secret turns it on
// and is the one response that carries the secret; again replaces it; DELETE
// turns it off. Each is audited as what it was.
func TestTheWebhookIsOffUntilSetUpAndItsSecretIsShownOnce(t *testing.T) {
	e := newSourceEnv(t)
	rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/admin/git-connections", tokenConnectionBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	var created gitConnectionWithSecret
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding the create: %v", err)
	}
	if created.WebhookSecretSet || strings.Contains(rec.Body.String(), `"webhook_secret"`) {
		t.Fatalf("a new connection has a webhook (set=%v): %s", created.WebhookSecretSet, rec.Body.String())
	}
	hook := func(secret string) bool {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(pushBody))
		req.Header.Set("X-Hub-Signature-256", sign(secret, pushBody))
		conn, err := e.db.GetGitConnection(created.ID)
		if err != nil {
			t.Fatalf("GetGitConnection: %v", err)
		}
		if conn.EncryptedWebhookSecret() == "" {
			return false
		}
		stored, err := e.srv.encryptor.Decrypt(conn.EncryptedWebhookSecret())
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		return hookSignatureValid(req, []byte(pushBody), stored)
	}
	mint := func() gitConnectionWithSecret {
		t.Helper()
		rec := e.do(t, "admin-user", http.MethodPost,
			"/api/v1/admin/git-connections/"+created.ID+"/webhook-secret", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook-secret = %d (%s)", rec.Code, rec.Body.String())
		}
		var got gitConnectionWithSecret
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return got
	}
	lastAction := func() string {
		t.Helper()
		var action string
		if err := e.db.Conn().QueryRow(`SELECT action FROM audit_log WHERE target_id = ?
			ORDER BY occurred_at DESC, id DESC LIMIT 1`, created.ID).Scan(&action); err != nil {
			t.Fatalf("reading the audit row: %v", err)
		}
		return action
	}

	on := mint()
	if len(on.WebhookSecret) != 64 || !on.WebhookSecretSet {
		t.Fatalf("set up returned %q (set=%v), want the 64-hex minted value", on.WebhookSecret, on.WebhookSecretSet)
	}
	if !hook(on.WebhookSecret) {
		t.Fatal("the secret set up returned does not verify a delivery signed with it")
	}
	if a := lastAction(); a != "git_connection.webhook_enabled" {
		t.Errorf("set up audited as %q", a)
	}

	// Never again on a read or an update.
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/git-connections", ""},
		{http.MethodPatch, "/api/v1/admin/git-connections/" + created.ID, `{"display_name":"Renamed"}`},
	} {
		rec := e.do(t, "admin-user", call.method, call.path, call.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d (%s)", call.method, call.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), on.WebhookSecret) ||
			strings.Contains(rec.Body.String(), `"webhook_secret"`) {
			t.Errorf("%s %s carries the webhook secret", call.method, call.path)
		}
	}

	// Again replaces it, once, and the old one stops verifying.
	rotated := mint()
	if rotated.WebhookSecret == "" || rotated.WebhookSecret == on.WebhookSecret {
		t.Fatalf("rotate returned %q, want a new secret", rotated.WebhookSecret)
	}
	if hook(on.WebhookSecret) {
		t.Error("the old secret still verifies after a rotate")
	}
	if !hook(rotated.WebhookSecret) {
		t.Error("the rotated secret does not verify")
	}
	if rotated.DisplayName != "Renamed" {
		t.Errorf("the rotate's connection reads %q, want the current row", rotated.DisplayName)
	}
	if a := lastAction(); a != "git_connection.webhook_secret_rotated" {
		t.Errorf("replace audited as %q", a)
	}

	// Off: nothing verifies, the poller is all there is.
	rec = e.do(t, "admin-user", http.MethodDelete,
		"/api/v1/admin/git-connections/"+created.ID+"/webhook-secret", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"webhook_secret_set":true`) {
		t.Fatalf("turn off = %d (%s)", rec.Code, rec.Body.String())
	}
	if hook(rotated.WebhookSecret) {
		t.Error("a delivery still verifies after the webhook was turned off")
	}
	if a := lastAction(); a != "git_connection.webhook_disabled" {
		t.Errorf("turn off audited as %q", a)
	}

	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		if rec := e.do(t, "admin-user", m,
			"/api/v1/admin/git-connections/no-such/webhook-secret", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s on a missing connection = %d, want 404", m, rec.Code)
		}
	}
}

// DG-9 (security review 2026-11-14): a base URL moved to another host without
// the credentials re-entered is refused, so the stored ones never reach it;
// with them, the move lands and every person's picker token goes with the old
// host.
func TestMovingAConnectionToAnotherHostNeedsItsCredentialsAgain(t *testing.T) {
	e := newSourceEnv(t)
	dto := createConnection(t, e, tokenConnectionBody)
	before, err := e.db.GetGitConnection(dto.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := e.srv.encryptor.Encrypt("a-persons-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.PutGitConnectionIdentity(dto.ID, "admin-user", token, nil); err != nil {
		t.Fatalf("PutGitConnectionIdentity: %v", err)
	}

	rec := e.do(t, "admin-user", http.MethodPatch, "/api/v1/admin/git-connections/"+dto.ID,
		`{"base_url":"https://git.elsewhere.example"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("base_url alone = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if c, _ := e.db.GetGitConnection(dto.ID); c.BaseURL != before.BaseURL {
		t.Errorf("a refused move changed the row: %q", c.BaseURL)
	}
	if id, _ := e.db.GetGitConnectionIdentity(dto.ID, "admin-user"); id == nil {
		t.Error("a refused move dropped the picker token")
	}

	// Same host, another path: not a move, nothing re-entered.
	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/admin/git-connections/"+dto.ID,
		`{"display_name":"Renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d (%s)", rec.Code, rec.Body.String())
	}

	rec = e.do(t, "admin-user", http.MethodPatch, "/api/v1/admin/git-connections/"+dto.ID,
		`{"base_url":"https://git.elsewhere.example","clone_secret":"github_pat_11NEWNEWNEW"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("base_url with clone_secret = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if id, _ := e.db.GetGitConnectionIdentity(dto.ID, "admin-user"); id != nil {
		t.Error("the old host's picker token survived the move")
	}
}

package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

// Who owns an app the platform was asked to create.
//
// The bug these exist for: all three creation paths derived the owner from the
// *session's* JWT claims, which a personal API key does not set — so every one
// of them created an app with no owner at all when an `toftu_` key made the
// request, and `apps_one_owner` refused the insert with a 500. All three routes
// are on the personal-key allow-list, so all three were meant to work.

// A personal key is a person. The principal *is* the user — the key is only
// which credential they spoke through — so an app created with one is owned by
// the key's owner, exactly as if they had used a browser.
func TestAKeyCreatedAppIsOwnedByTheKeysUser(t *testing.T) {
	e := newSourceEnv(t)
	enablePersonalKeys(t, e.srv, true)
	key := mintKey(t, e.srv, "publisher", "ci", nil, false)

	rec := e.withKey(t, key, http.MethodPost, "/api/v1/apps",
		`{"name":"Reports","slug":"reports","repo_url":"https://github.com/acme/reports","branch":"main"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a personal key = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	app, err := e.db.GetAppBySlug("reports")
	if err != nil || app == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if app.OwnerID != "publisher" {
		t.Errorf("owner_user_id = %q, want the key's own user", app.OwnerID)
	}
	if app.OwnerGroupID != "" {
		t.Errorf("owner_group_id = %q, want empty — exactly one owner, always", app.OwnerGroupID)
	}

	// The owner grant goes in with the row, in one transaction — so an app
	// created with no owner would have had no grant either, and nobody could
	// administer it. This is the half that makes the column mean something.
	if rows := auditRows(t, e.db, "object_grant.create"); len(rows) == 0 {
		t.Error("no owner grant was written with the app")
	}
	// And it is really theirs: the same person, through the same key, can edit
	// what they just created.
	rec = e.withKey(t, key, http.MethodPatch, "/api/v1/apps/"+app.ID, `{"branch":"release"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("the key's owner cannot edit the app they just created: %d (%s)",
			rec.Code, rec.Body.String())
	}
}

// The same bug on the other creation route that takes an archive.
func TestAKeyUploadedAppIsOwnedByTheKeysUser(t *testing.T) {
	e := newSourceEnv(t)
	enablePersonalKeys(t, e.srv, true)
	key := mintKey(t, e.srv, "publisher", "ci", nil, false)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "Bundle")
	_ = mw.WriteField("slug", "bundle")
	part, err := mw.CreateFormFile("file", "bundle.zip")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(minimalZip(t))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload with a personal key = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	app, err := e.db.GetAppBySlug("bundle")
	if err != nil || app == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if app.OwnerID != "publisher" {
		t.Errorf("owner_user_id = %q, want the key's own user", app.OwnerID)
	}
}

// The three principal classes, and each is a different answer rather than a
// special case. The gallery's one-click deploy is the third creation path and
// calls this same function, which is what makes one test here cover it.
func TestAppOwnerIDByPrincipalClass(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *auth.Principal
		want string
	}{
		{"a cookie session", &auth.Principal{
			Type: database.PrincipalTypeUser, ID: "alice"}, "alice"},
		// A personal key carries no identity of its own: the principal is the
		// user, and APIKeyID only records which credential they spoke through.
		{"a personal key", &auth.Principal{
			Type: database.PrincipalTypeUser, ID: "alice", APIKeyID: "key-1"}, "alice"},
		// A service account acts on behalf of somebody. **Its own ID is not a
		// person's** and must never land in owner_user_id.
		{"a service account", &auth.Principal{
			Type: database.PrincipalTypeServiceAccount, ID: "sa-1", OwningUserID: "alice"}, "alice"},
		{"a service account with no owner", &auth.Principal{
			Type: database.PrincipalTypeServiceAccount, ID: "sa-1"}, ""},
		// A deploy key is nobody. "" is the right answer rather than a failure:
		// POST /apps turns it into the refusal that names owner_group_id.
		{"a deploy key", &auth.Principal{
			Type: auth.PrincipalTypeDeployKey, DeployKeyID: "dk-1"}, ""},
		{"nothing at all", nil, ""},
	} {
		if got := appOwnerID(c.p); got != c.want {
			t.Errorf("%s: appOwnerID = %q, want %q", c.name, got, c.want)
		}
	}
}

// A deploy key still has to name a group, and the refusal still names the
// field — the fix must not have turned "nobody" into "somebody".
func TestADeployKeyStillHasToNameAGroup(t *testing.T) {
	e := newSourceEnv(t)
	seedUser(t, e.db, "root", database.SystemRoleAdmin)
	secret := mintDeployKey(t, e.srv, e.router, "pipeline")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(
		`{"name":"Pipeline","slug":"pipeline","repo_url":"https://github.com/acme/p","branch":"main"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("deploy-key create with no group = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "owner_group_required" {
		t.Errorf("code = %q, want owner_group_required", body["code"])
	}
	if app, _ := e.db.GetAppBySlug("pipeline"); app != nil {
		t.Error("a refused create left an app row behind")
	}
}

// **The principal is the source, and the session's claims never were.**
//
// This is the bug class rather than the bug: a handler that reaches for
// `auth.GetClaims` to find out who is acting gets nothing on every credential
// that is not a cookie session, and a principal is sitting in the same context.
// The one legitimate reader needs the claims *themselves* — for session
// renewal — rather than as a stand-in for a user id.
func TestClaimsAreReadOnlyWhereTheClaimsThemselvesAreTheAnswer(t *testing.T) {
	allowed := map[string]string{
		"proxy_handler.go": "the gateway renews the session cookie, which needs the token's own " +
			"claims rather than the user behind them",
		"app_handshake.go": "the grant carries the platform session's own start, so the " +
			"apps-origin cookie inherits the sign-in's clock; the principal knows who, " +
			"and only the claims know when",
		"session.go": "keepOwnSession re-issues the caller's own session after a membership " +
			"change, keeping its jti and auth_time — the token itself is what is carried across",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package: %v", err)
	}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if allowed[name] != "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if strings.Contains(string(body), "auth.GetClaims(") {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("auth.GetClaims is read in %s.\n"+
			"Claims exist only for a cookie session, so this answers \"\" for a personal API key "+
			"and for a deploy key — which is how all three app-creation paths came to write an "+
			"app with no owner. For who is acting, use auth.GetPrincipal(r.Context()); for who "+
			"owns what they are creating, use appOwnerID. If the claims themselves really are "+
			"the answer, add the file to this test's allow-list with the reason.",
			strings.Join(offenders, ", "))
	}
}

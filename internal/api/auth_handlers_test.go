package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/entitlements"
	"toft/internal/password"
	"toft/internal/runtime"
)

// getThroughRouter drives a GET through the real router. Provider sign-in
// handlers read the provider id from the URL, so calling them directly would
// test a handler no route can produce.
func getThroughRouter(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:34567"
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	return rr
}

// **Turns the install-wide `list` rung off, for a test whose subject is not the
// rung.**
//
// It is on by default from 2026-08-31, which means every signed-in person holds
// `list` on every app and an app route answers 403 `deny_list_only` where it
// used to answer 404. That is the rung working — the refusal is a door — but it
// is not what a test about logs, jobs, releases, storage or tags is asking
// about, and those tests all carried a "somebody unrelated gets 404" line as
// ordinary IDOR hygiene.
//
// So they say the precondition out loud rather than inheriting it from a
// default that has now moved once and could move again. The rung's own tests in
// list_rung_test.go assert the on-case, which is where that assertion belongs.
func rungOff(t *testing.T, srv *Server) {
	t.Helper()
	if _, err := srv.db.PutSetting(database.SettingAppsListableToAuthenticated, "false", ""); err != nil {
		t.Fatalf("turning the install-wide list rung off: %v", err)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	// The published default, asked for by name.
	//
	// A fresh install generates its own first password (ADR §35), which is
	// right in production and useless in a test that has to sign in as the
	// built-in administrator to exercise the must_change flow at all. Supplying
	// it here is the same thing scripts/dev.sh does for the local loop: the
	// default is opt-in now, not automatic.
	db := dbtest.New(t, database.WithBootstrapAdminPassword(password.Default))

	cfg := &config.Config{
		BaseURL: "http://localhost:8080",
		// An environment-configured GitHub provider, which is what most of
		// these tests need to exist in order to exercise a sign-in at all.
		GitHubClientID:     "client-id",
		GitHubClientSecret: "client-secret",
	}
	return newTestServerWithConfig(t, db, cfg)
}

// newTestServerWithConfig is the variant for tests that care about which
// providers exist — an install with no provider configured behaves differently
// from one with GitHub set, and both cases have to be reachable.
func newTestServerWithConfig(t *testing.T, db *database.DB, cfg *config.Config, opts ...ServerOption) *Server {
	t.Helper()

	// An apps directory of its own, because an empty one is not "no apps
	// directory" — it is the *working* directory. config.Load resolves AppsDir
	// to an absolute path and defaults it to ./apps, so production never has an
	// empty one; a fixture that builds a config.Config by hand skips both, and
	// the first upload handler to run then extracts into
	// internal/api/<app-id>/upload-staging. Ninety-eight of those accumulated in
	// the package directory before anybody noticed, which is the shape of this
	// bug: it does not fail a test, it litters the repository.
	if cfg.AppsDir == "" {
		cfg.AppsDir = t.TempDir()
	}

	jwtSvc := auth.NewJWTService("test-secret-key")
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	// **A licensed install, asked for by name.**
	//
	// The permissive default is now the demo tier, which caps app creation and
	// locks the six configuration surfaces a license unlocks. That is right in
	// production and useless in a fixture whose subject is groups, or storage,
	// or the eleventh app of a test that is not about licensing — so the
	// fixture states what it is rather than inheriting it. Prepended, so a test
	// that *is* about licensing overrides it with its own source.
	opts = append([]ServerOption{WithEntitlements(entitlements.AtTier(entitlements.TierPaid))}, opts...)

	// A real runtime, mocked: handlers that enrich responses with process
	// metrics (GET /apps, GET /apps/{id}) dereference it unconditionally, as
	// they legitimately may in production.
	return NewServer(cfg, db, jwtSvc, runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil), nil, enc, opts...)
}

func TestGitHubLogin_FlagOff_RedirectsToGitHub(t *testing.T) {
	srv := newTestServer(t)
	rr := getThroughRouter(t, srv, "/api/v1/auth/github/login")

	if rr.Code < 300 || rr.Code >= 400 {
		t.Fatalf("expected 3xx, got %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize") {
		t.Fatalf("expected GitHub oauth URL, got %s", loc)
	}
}

// A GET on the local provider's login URL has nowhere to redirect to — the form
// lives on the login page — so it lands there rather than erroring about a flow
// that does not apply to it.
func TestProviderLogin_LocalProvider_RedirectsToLoginPage(t *testing.T) {
	srv := newTestServer(t)
	rr := getThroughRouter(t, srv, "/api/v1/auth/local/login")

	if rr.Code < 300 || rr.Code >= 400 {
		t.Fatalf("expected 3xx, got %d body=%s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "http://localhost:8080/login" {
		t.Fatalf("Location = %q, want the login page", loc)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "oauth_state" {
			t.Fatal("an oauth_state cookie was set for a provider with no OAuth flow")
		}
	}
}

// Password sign-in has no callback: there is no authorization code to redeem, so
// the URL is not part of anything and must not look like it is.
func TestProviderCallback_LocalProvider_Returns404(t *testing.T) {
	srv := newTestServer(t)
	rr := getThroughRouter(t, srv, "/api/v1/auth/local/callback?code=x&state=y")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("callback on the local provider = %d, want 404", rr.Code)
	}
}

// Disabled means absent. An administrator who switches local sign-in off must
// leave nothing behind that hints a password form is one setting away.
func TestPasswordRoutes_ProviderDisabled_Return404(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.Router(http.NotFoundHandler())

	if err := srv.db.SetAuthProviderEnabled(database.LocalProviderID, false, database.AuditEntry{
		ActorType: database.PrincipalTypeOperator, ActorID: "test",
	}); err != nil {
		t.Fatalf("disabling local sign-in: %v", err)
	}

	for _, path := range []string{"/api/v1/auth/local/login", "/api/v1/auth/local/password"} {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password.Default})
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("POST %s with local sign-in off = %d, want 404", path, rr.Code)
		}
	}

	// And it is gone from what the login page is told to render.
	rr := getThroughRouter(t, srv, "/api/v1/auth/providers")
	var opts loginOptionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &opts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, p := range opts.Providers {
		if p.ID == database.LocalProviderID {
			t.Error("a disabled local provider is still offered on the login page")
		}
	}
}

// The callback is the other half of the login's door. Switching a provider off
// is the one thing an administrator can do during an IdP incident, and a
// person who builds the authorize URL by hand — the client id is public —
// arrives at the callback with a good code and a state cookie their own
// browser set. Today the login redirect refuses them; the callback must too,
// and it must refuse before anything is exchanged, so a disabled provider
// mints nothing.
func TestProviderCallback_DisabledProvider_Returns403AndMintsNothing(t *testing.T) {
	srv := newTestServer(t)
	p := &database.AuthProvider{
		ID: "okta", Kind: database.ProviderKindOIDC, DisplayName: "Okta",
		Enabled: false, ClientID: "client-id", Issuer: "https://okta.example.invalid",
		DefaultRole: database.SystemRoleViewer, AutoProvision: true,
	}
	p.SetEncryptedSecret(mustEncrypt(t, srv, "secret"))
	if err := srv.db.CreateAuthProvider(p, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin-1",
	}); err != nil {
		t.Fatalf("CreateAuthProvider: %v", err)
	}

	// A state cookie that would pass the CSRF check, so the refusal is the
	// provider's and not the state's.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/okta/callback?code=good&state=nonce", nil)
	req.RemoteAddr = "127.0.0.1:34567"
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: "okta|nonce"})
	req.AddCookie(&http.Cookie{Name: oauthVerifierCookie, Value: "verifier"})
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("callback on a disabled provider = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "that sign-in provider is disabled") {
		t.Errorf("body = %q, want the login's own sentence", rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			t.Fatal("a session cookie was minted through a disabled provider")
		}
	}
}

func mustEncrypt(t *testing.T, srv *Server, plaintext string) string {
	t.Helper()
	enc, err := srv.encryptor.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return enc
}

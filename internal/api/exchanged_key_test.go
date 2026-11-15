package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"toft/internal/authprovider"
	"toft/internal/database"
)

// The short-lived key a trusted issuer's identity token buys (ADR §157).

// exchangeEnv is an install with personal keys on, an administrator and
// alice, a sign-in provider `sso` alice has an identity on, and an OIDC issuer
// on loopback that can sign tokens for her.
type exchangeEnv struct {
	srv    *Server
	router http.Handler
	idp    *httptest.Server
	key    *rsa.PrivateKey
}

func newExchangeEnv(t *testing.T) *exchangeEnv {
	t.Helper()
	srv, router := keyEnv(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := &exchangeEnv{srv: srv, router: router, key: key}
	mux := http.NewServeMux()
	e.idp = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": e.idp.URL, "jwks_uri": e.idp.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	t.Cleanup(func() {
		e.idp.Close()
		authprovider.ForgetDiscovery(e.idp.URL)
		authprovider.ForgetJWKS(e.idp.URL)
	})
	db := srv.db.Conn()
	if _, err := db.Exec(`INSERT INTO auth_providers (id, kind, display_name) VALUES ('sso', 'oidc', 'SSO')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO user_identities (provider_id, subject, user_id) VALUES ('sso', 'alice-sub', 'alice')`); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *exchangeEnv) token(t *testing.T, aud, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": e.idp.URL, "aud": aud, "sub": sub, "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(e.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *exchangeEnv) send(t *testing.T, method, path, who, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	switch b := body.(type) {
	case url.Values:
		req = httptest.NewRequest(method, path, strings.NewReader(b.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case nil:
		req = httptest.NewRequest(method, path, nil)
	default:
		buf, _ := json.Marshal(b)
		req = httptest.NewRequest(method, path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// trust names the loopback issuer for an audience, as an administrator does.
func (e *exchangeEnv) trust(t *testing.T, aud string) map[string]any {
	t.Helper()
	rec := e.send(t, http.MethodPost, "/api/v1/admin/trusted-issuers", "root", "", map[string]any{
		"display_name": "Garth", "issuer": e.idp.URL, "audience": aud,
		"subject_claim": "sub", "provider_id": "sso"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("trust: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func exchangeForm(token string) url.Values {
	return url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token_type": {tokenTypeIDToken},
		"subject_token":      {token},
	}
}

func (e *exchangeEnv) exchange(t *testing.T, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	rec := e.send(t, http.MethodPost, path, "", "", form)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestATrustedIssuersTokenBuysAShortLivedDeployKey(t *testing.T) {
	e := newExchangeEnv(t)
	e.trust(t, "garth")

	code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub")))
	if code != http.StatusOK {
		t.Fatalf("exchange: %d %v", code, out)
	}
	secret, _ := out["access_token"].(string)
	if !strings.HasPrefix(secret, exchangedKeySecretPrefix) {
		t.Fatalf("access_token = %q, want a toftx_ key", secret)
	}
	if out["expires_in"].(float64) != float64(database.ExchangedKeyDefaultMinutes*60) {
		t.Errorf("expires_in = %v", out["expires_in"])
	}

	// It is alice, on the deploy surface.
	rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", secret, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a deploy route: %d %s", rec.Code, rec.Body.String())
	}
	var caps map[string]any
	json.Unmarshal(rec.Body.Bytes(), &caps)
	if cred, _ := caps["credential"].(map[string]any); cred != nil && cred["kind"] != "exchanged_key" {
		t.Errorf("credential = %v, want kind exchanged_key", cred)
	}
	// A personal key's route that is not deploying is refused by name.
	rec = e.send(t, http.MethodGet, "/api/v1/directory/users", "", secret, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_deploy_scope") {
		t.Fatalf("a non-deploy route: %d %s", rec.Code, rec.Body.String())
	}
	// And a session's route is refused as one.
	rec = e.send(t, http.MethodGet, "/api/v1/admin/trusted-issuers", "", secret, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an admin route: %d %s", rec.Code, rec.Body.String())
	}

	// The exchange is audited as alice, naming the issuer.
	var n int
	if err := e.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE action = 'exchanged_key.create' AND actor_id = 'alice' AND after::text LIKE '%Garth%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d, %v", n, err)
	}
}

// rsconnect's own route answers in the type it asked for.
func TestRsconnectsFederationRouteIsTheSameExchange(t *testing.T) {
	e := newExchangeEnv(t)
	e.trust(t, "workbench")
	form := exchangeForm(e.token(t, "workbench", "alice-sub"))
	form.Set("requested_token_type", tokenTypeConnectAPIKey)
	code, out := e.exchange(t, "/__api__/v1/oauth/integrations/credentials", form)
	if code != http.StatusOK || out["issued_token_type"] != tokenTypeConnectAPIKey {
		t.Fatalf("rsconnect exchange: %d %v", code, out)
	}
	// rsconnect presents it as `Authorization: Key`.
	req := httptest.NewRequest(http.MethodGet, "/__api__/v1/user", nil)
	req.Header.Set("Authorization", "Key "+out["access_token"].(string))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rsconnect's current user with the key: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnExchangeIsRefused(t *testing.T) {
	e := newExchangeEnv(t)
	e.trust(t, "garth")

	cases := map[string]struct {
		form url.Values
		code int
		want string
	}{
		"another audience":   {exchangeForm(e.token(t, "someone-else", "alice-sub")), http.StatusUnauthorized, "issuer_not_trusted"},
		"nobody by that sub": {exchangeForm(e.token(t, "garth", "stranger")), http.StatusForbidden, "no_account"},
		"not a token":        {exchangeForm("nonsense"), http.StatusUnauthorized, "issuer_not_trusted"},
		"another grant":      {url.Values{"grant_type": {"password"}}, http.StatusBadRequest, "unsupported_grant_type"},
	}
	for name, c := range cases {
		code, out := e.exchange(t, "/api/v1/auth/token-exchange", c.form)
		if code != c.code || out["code"] != c.want {
			t.Errorf("%s: %d %v, want %d %s", name, code, out, c.code, c.want)
		}
	}

	// Nor does one awaiting activation (security review 2026-11-10, A-1): the
	// first browser sign-in wrote it and refused the session.
	if _, err := e.srv.db.Conn().Exec(`UPDATE users SET pending_activation_at = now() WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub"))); code != http.StatusForbidden || out["code"] != "owner_pending" {
		t.Errorf("pending: %d %v", code, out)
	}
	if _, err := e.srv.db.Conn().Exec(`UPDATE users SET pending_activation_at = NULL WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}

	// A locked account buys nothing.
	if _, err := e.srv.db.Conn().Exec(`UPDATE users SET locked_at = now() WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub"))); code != http.StatusForbidden || out["code"] != "owner_locked" {
		t.Errorf("locked: %d %v", code, out)
	}
}

// Nothing is trusted by default.
func TestWithNoTrustedIssuerNothingIsExchanged(t *testing.T) {
	e := newExchangeEnv(t)
	if code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub"))); code != http.StatusUnauthorized {
		t.Fatalf("untrusted: %d %v", code, out)
	}
}

// One token exchanged again and again holds at most the cap of live keys: the
// oldest is revoked, never the exchange refused (security review 2026-11-14,
// SI-8).
func TestTokenExchangeKeepsAtMostTheCapOfLiveKeys(t *testing.T) {
	e := newExchangeEnv(t)
	e.trust(t, "garth")
	token := e.token(t, "garth", "alice-sub")
	var keys []string
	for i := 0; i <= maxExchangedKeysPerIssuer; i++ {
		code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(token))
		if code != http.StatusOK {
			t.Fatalf("exchange %d: %d %v", i+1, code, out)
		}
		keys = append(keys, out["access_token"].(string))
	}
	var live int
	if err := e.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM exchanged_keys
		WHERE user_id = 'alice' AND revoked_at IS NULL AND expires_at > now()`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != maxExchangedKeysPerIssuer {
		t.Errorf("%d live keys after %d exchanges, want %d", live, len(keys), maxExchangedKeysPerIssuer)
	}
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", keys[0], nil); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "key_revoked") {
		t.Errorf("the oldest key: %d %s, want 401 key_revoked", rec.Code, rec.Body.String())
	}
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", keys[len(keys)-1], nil); rec.Code != http.StatusOK {
		t.Errorf("the newest key: %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// The holder revokes it; switching the issuer off ends every key it vouched
// for, on the next request.
func TestAnExchangedKeyEnds(t *testing.T) {
	e := newExchangeEnv(t)
	issuer := e.trust(t, "garth")
	mint := func() string {
		_, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub")))
		return out["access_token"].(string)
	}

	held := mint()
	if rec := e.send(t, http.MethodPost, "/api/v1/auth/token-exchange/revoke", "", "", url.Values{"token": {held}}); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", held, nil); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "key_revoked") {
		t.Fatalf("a revoked key: %d %s", rec.Code, rec.Body.String())
	}

	live := mint()
	// A key in hand is refused per request once its account awaits activation.
	e.srv.db.Conn().Exec(`UPDATE users SET pending_activation_at = now() WHERE id = 'alice'`)
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", live, nil); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "owner_pending") {
		t.Fatalf("a pending owner's key: %d %s", rec.Code, rec.Body.String())
	}
	e.srv.db.Conn().Exec(`UPDATE users SET pending_activation_at = NULL WHERE id = 'alice'`)
	rec := e.send(t, http.MethodGet, "/api/v1/admin/exchanged-keys", "root", "", nil)
	var keys []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &keys)
	if len(keys) != 1 || keys[0]["user_login"] != "alice" {
		t.Fatalf("live keys: %s", rec.Body.String())
	}
	if rec := e.send(t, http.MethodPatch, "/api/v1/admin/trusted-issuers/"+issuer["id"].(string), "root", "",
		map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", live, nil); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "issuer_disabled") {
		t.Fatalf("a disabled issuer's key: %d %s", rec.Code, rec.Body.String())
	}

	// An administrator revokes one.
	if rec := e.send(t, http.MethodDelete, "/api/v1/admin/exchanged-keys/"+keys[0]["id"].(string), "root", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke: %d %s", rec.Code, rec.Body.String())
	}
}

// A personal key ends itself on the holder's route: only the key presented,
// whatever state it is in, and the answer is the same 200 for a string that
// was never a key — so a program holding a key (Garth, for a session) can
// revoke it though no key may list or delete keys.
func TestAPersonalKeyEndsItself(t *testing.T) {
	e := newExchangeEnv(t)
	revoke := func(token string) {
		t.Helper()
		rec := e.send(t, http.MethodPost, "/api/v1/auth/token-exchange/revoke", "", "", url.Values{"token": {token}})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked":true`) {
			t.Fatalf("revoke %.10s…: %d %s", token, rec.Code, rec.Body.String())
		}
	}
	held := mintKey(t, e.srv, "alice", "toft on garth session s1", nil, false)
	other := mintKey(t, e.srv, "alice", "ci", nil, false)
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", held, nil); rec.Code != http.StatusOK {
		t.Fatalf("before: %d %s", rec.Code, rec.Body.String())
	}

	revoke(held)
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", held, nil); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "invalid_key") {
		t.Fatalf("a revoked personal key: %d %s", rec.Code, rec.Body.String())
	}
	// The owner's other key is untouched: there is nothing to aim with.
	if rec := e.send(t, http.MethodGet, "/api/v1/me/capabilities", "", other, nil); rec.Code != http.StatusOK {
		t.Fatalf("the other key: %d %s", rec.Code, rec.Body.String())
	}

	// Gone already, never a key, nothing at all: the same answer.
	revoke(held)
	revoke("toftu_" + strings.Repeat("0", 40))
	revoke("")

	// A disabled key, which can't authenticate, still ends.
	disabled := mintKey(t, e.srv, "alice", "old", nil, true)
	revoke(disabled)
	var n int
	if err := e.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM user_api_keys WHERE user_id = 'alice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("alice has %d keys, want 1 (ci)", n)
	}

	// Recorded as the owner's revocation, by the holder.
	if err := e.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'user.api_key_revoke'
		AND actor_id = 'alice' AND after::text LIKE '%holder%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d holder revocations audited, want 2", n)
	}
}

// An issuer row is validated before it is written.
func TestATrustedIssuerIsValidated(t *testing.T) {
	e := newExchangeEnv(t)
	for name, body := range map[string]map[string]any{
		"no audience":      {"display_name": "x", "issuer": e.idp.URL, "provider_id": "sso"},
		"plain http":       {"display_name": "x", "issuer": "http://idp.example", "audience": "a", "provider_id": "sso"},
		"no provider":      {"display_name": "x", "issuer": e.idp.URL, "audience": "a"},
		"unknown provider": {"display_name": "x", "issuer": e.idp.URL, "audience": "a", "provider_id": "nope"},
		"a week-long key":  {"display_name": "x", "issuer": e.idp.URL, "audience": "a", "provider_id": "sso", "key_lifetime_minutes": 10080},
		"an unknown claim": {"display_name": "x", "issuer": e.idp.URL, "audience": "a", "subject_claim": "oid"},
	} {
		if rec := e.send(t, http.MethodPost, "/api/v1/admin/trusted-issuers", "root", "", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Only an administrator names one.
	if rec := e.send(t, http.MethodPost, "/api/v1/admin/trusted-issuers", "alice", "", map[string]any{
		"display_name": "x", "issuer": e.idp.URL, "audience": "a", "provider_id": "sso"}); rec.Code != http.StatusForbidden {
		t.Errorf("a publisher: %d", rec.Code)
	}
}

// The key's routes are the deploy key's deploy scope and the Connect surface,
// and every one of them is already a personal key's.
func TestExchangedKeyRoutesAreDeployScoped(t *testing.T) {
	for route := range exchangedKeyAllowedRoutes {
		if !apiKeyAllowedRoutes[route] {
			t.Errorf("%s is an exchanged key's but not a personal key's", route)
		}
	}
	for route := range deployKeyAllowedRoutes {
		if !exchangedKeyAllowedRoutes[route] {
			t.Errorf("%s is a deploy key's but not an exchanged key's", route)
		}
	}
	for _, route := range []string{"PUT /api/v1/apps/{appID}/storage/{resourceID}", "GET /api/v1/me/storage",
		"POST /api/v1/store/{slug}/objects", "GET /api/v1/store/{slug}/objects/*"} {
		if !exchangedKeyAllowedRoutes[route] {
			t.Errorf("%s must be an exchanged key's: a session deploys apps that declare storage", route)
		}
	}
	for _, route := range []string{"POST /api/v1/connect/tokens", "POST /api/v1/connect/v1/oauth/integrations/credentials",
		"GET /api/v1/me/api-keys", "DELETE /api/v1/apps/{appID}",
		"PUT /api/v1/admin/storage/{id}/grants", "PUT /api/v1/admin/storage/{id}/managers"} {
		if exchangedKeyAllowedRoutes[route] {
			t.Errorf("%s must not be an exchanged key's", route)
		}
	}
}

// At the apps gateway an exchanged key is refused by name, never passed
// through to the app as an unrecognised Authorization header.
func TestAnExchangedKeyDoesNotReachAnApp(t *testing.T) {
	srv, _ := keyEnv(t)
	app := &database.App{Name: "gw-x", Slug: "gw-x", OwnerID: "alice", Framework: "flask", Status: database.AppStatusRunning, Visibility: database.VisibilityPublic}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/gw-x/", nil)
	req.Header.Set("Authorization", "Bearer "+exchangedKeySecretPrefix+"0123456789abcdef")
	rec := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "toftx_") {
		t.Fatalf("gateway: %d %s", rec.Code, rec.Body.String())
	}
}

// A `sub` issuer may name a provider the environment configured: the merged
// registry is what GET /admin/auth-providers lists, and what the table alone
// refused (code review 2026-11-11, A-3). keyEnv's server has GitHub from the
// environment.
func TestATrustedIssuerMayNameAnEnvironmentProvider(t *testing.T) {
	e := newExchangeEnv(t)
	rec := e.send(t, http.MethodPost, "/api/v1/admin/trusted-issuers", "root", "", map[string]any{
		"display_name": "x", "issuer": e.idp.URL, "audience": "a", "subject_claim": "sub", "provider_id": "github"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("an environment provider was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// A disabled provider is closed to the exchange as it is to sign-in and SCIM,
// and enabling it again reopens it (security review 2026-11-14, SI-4).
func TestTokenExchangeRefusesADisabledProvider(t *testing.T) {
	e := newExchangeEnv(t)
	e.trust(t, "garth")
	if _, err := e.srv.db.Conn().Exec(`UPDATE auth_providers SET enabled = FALSE WHERE id = 'sso'`); err != nil {
		t.Fatal(err)
	}
	code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub")))
	if code != http.StatusForbidden || out["code"] != "provider_disabled" {
		t.Fatalf("exchange with the provider disabled: %d %v", code, out)
	}
	if _, err := e.srv.db.Conn().Exec(`UPDATE auth_providers SET enabled = TRUE WHERE id = 'sso'`); err != nil {
		t.Fatal(err)
	}
	if code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub"))); code != http.StatusOK {
		t.Fatalf("exchange with the provider enabled again: %d %v", code, out)
	}
}

// A provider deleted after an issuer named it leaves the issuer inert and
// switch-off-able, as the schema promises: the exchange refuses, and
// `{"enabled": false}` is not refused for the very reason it was sent (code
// review 2026-11-11, A-4).
func TestADeletedProviderLeavesItsIssuerInertAndDisableable(t *testing.T) {
	e := newExchangeEnv(t)
	issuer := e.trust(t, "garth")
	if _, err := e.srv.db.Conn().Exec(`DELETE FROM auth_providers WHERE id = 'sso'`); err != nil {
		t.Fatal(err)
	}
	code, out := e.exchange(t, "/api/v1/auth/token-exchange", exchangeForm(e.token(t, "garth", "alice-sub")))
	if code != http.StatusForbidden || out["code"] != "provider_gone" {
		t.Fatalf("exchange with the provider gone: %d %v", code, out)
	}
	id, _ := issuer["id"].(string)
	rec := e.send(t, http.MethodPatch, "/api/v1/admin/trusted-issuers/"+id, "root", "", map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling: %d %s", rec.Code, rec.Body.String())
	}
	// Re-pointing it at a provider that is not there is still refused.
	rec = e.send(t, http.MethodPatch, "/api/v1/admin/trusted-issuers/"+id, "root", "", map[string]any{"provider_id": "nope"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("naming a missing provider: %d %s", rec.Code, rec.Body.String())
	}
}

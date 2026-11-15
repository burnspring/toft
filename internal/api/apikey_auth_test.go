package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"toft/internal/database"
)

// enablePersonalKeys flips the install-wide capability on. Off is the default,
// so almost every test here has to turn it on first — which is itself the
// "absent, not dormant" rule showing through.
func enablePersonalKeys(t *testing.T, srv *Server, on bool) {
	t.Helper()
	s, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	s.APIKeysEnabled = on
	if err := srv.db.SetPlatformSettings(&s, database.BuiltinAdminID, "req", "ip", "ua"); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// mintKey inserts a key directly, returning the secret. Going through the
// database rather than the API keeps these tests about *authentication* — the
// minting route has tests of its own.
func mintKey(t *testing.T, srv *Server, owner, name string, expires *time.Time, disabled bool) string {
	t.Helper()
	return mintScopedKey(t, srv, owner, name, database.UserKeyScopeFull, expires, disabled)
}

// mintScopedKey is the same, for the tests that care which scope the key has.
// The unscoped helper above stays the common case and says `full`, because that
// is what every key on every install had before the column existed.
func mintScopedKey(t *testing.T, srv *Server, owner, name, scope string, expires *time.Time, disabled bool) string {
	t.Helper()
	secret, err := generateUserAPIKeySecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	k := database.UserAPIKey{
		ID:        name + "-" + owner,
		UserID:    owner,
		Name:      name,
		KeyHash:   hashAPIKey(secret),
		KeyPrefix: keyDisplayPrefix(secret),
		Scope:     scope,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expires,
	}
	if err := srv.db.CreateUserAPIKeyTx(&k, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: owner, Action: "user.api_key_create",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if disabled {
		on := true
		if err := srv.db.UpdateUserAPIKeyTx(k.ID, nil, &on, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: owner, Action: "user.api_key_disable",
		}); err != nil {
			t.Fatalf("disable key: %v", err)
		}
	}
	return secret
}

// getWithKey issues a GET carrying a bearer token and nothing else.
func getWithKey(t *testing.T, router http.Handler, secret, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func keyEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	enablePersonalKeys(t, srv, true)
	return srv, router
}

func TestAPIKeyAuthenticatesOnPlatformAPI(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	rec := getWithKey(t, router, secret, "/api/v1/me/capabilities")
	if rec.Code != http.StatusOK {
		t.Fatalf("keyed request: %d body=%s", rec.Code, rec.Body.String())
	}
}

// Both header forms, because a client that already knows how to reach a
// deployed app should not have to learn a second convention.
func TestAPIKeyAcceptsXAPIKeyHeader(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.Header.Set("X-API-Key", secret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-API-Key request: %d body=%s", rec.Code, rec.Body.String())
	}
}

// The property that makes revocation mean anything: a presented-but-unusable
// key must never quietly succeed as whoever is signed in on the same browser.
func TestAPIKeyNeverFallsBackToCookie(t *testing.T) {
	srv, router := keyEnv(t)
	mintKey(t, srv, "alice", "ci", nil, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+userKeySecretPrefix+"deadbeefdeadbeef")
	req.AddCookie(sessionCookieFor(t, srv, "alice")) // a perfectly good session
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key alongside a valid cookie: %d, want 401 — falling back to the "+
			"cookie would keep a revoked token working from a developer's browser", rec.Code)
	}
}

// A refusal a caller can act on says why. Each of these presented a hash that
// matched, so naming the reason leaks nothing they do not already hold.
func TestAPIKeyRefusalsSayWhy(t *testing.T) {
	srv, router := keyEnv(t)
	seedUser(t, srv.db, "locked-user", database.SystemRoleViewer)

	past := time.Now().UTC().Add(-time.Hour)
	cases := []struct {
		name   string
		secret string
		code   string
	}{
		{"disabled", mintKey(t, srv, "alice", "off", nil, true), "key_disabled"},
		{"expired", mintKey(t, srv, "alice", "old", &past, false), "key_expired"},
		{"locked owner", mintKey(t, srv, "locked-user", "lk", nil, false), "owner_locked"},
	}
	// Locked through the API, because that is where the lock transaction lives.
	if rec := postAs(t, srv, router, "root", "/api/v1/users/locked-user/lock"); rec.Code != http.StatusOK {
		t.Fatalf("lock user: %d body=%s", rec.Code, rec.Body.String())
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getWithKey(t, router, tc.secret, "/api/v1/me/capabilities")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
			var body refusalBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["code"] != tc.code {
				t.Errorf("code %q, want %q — a caller holding this key is owed the reason",
					body["code"], tc.code)
			}
		})
	}
}

// A key that resolves to nothing is the one case that can be an attack, and is
// the one case that says nothing.
func TestUnknownAPIKeyIsOpaque(t *testing.T) {
	_, router := keyEnv(t)
	rec := getWithKey(t, router, userKeySecretPrefix+"0123456789abcdef", "/api/v1/me/capabilities")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "invalid_key" {
		t.Errorf("code %q, want invalid_key", body["code"])
	}
}

// Off means absent, and it means live keys stop working — not merely that no
// new ones can be minted.
func TestDisablingCapabilityKillsLiveKeys(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	if rec := getWithKey(t, router, secret, "/api/v1/me/capabilities"); rec.Code != http.StatusOK {
		t.Fatalf("before switch-off: %d", rec.Code)
	}
	enablePersonalKeys(t, srv, false)

	rec := getWithKey(t, router, secret, "/api/v1/me/capabilities")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after switch-off: %d, want 401 — an administrator flipping this switch "+
			"means the tokens stop working now", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "api_keys_disabled" {
		t.Errorf("code %q, want api_keys_disabled", body["code"])
	}

	// And the management routes are absent rather than refused.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/api-keys", nil)
	req.AddCookie(sessionCookieFor(t, srv, "alice"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /me/api-keys with capability off: %d, want 404", rec.Code)
	}
}

// A removed app key at the platform API is named — a bare 401 would read as
// "your key is wrong" when the class it belongs to no longer exists.
func TestAppKeyAtPlatformAPIIsNamed(t *testing.T) {
	_, router := keyEnv(t)
	rec := getWithKey(t, router, retiredAppKeyPrefix+"abc123", "/api/v1/me/capabilities")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "app_key_removed" {
		t.Errorf("code %q, want app_key_removed", body["code"])
	}
}

// The allow-list, exercised end to end rather than only as a map.
func TestAPIKeyCannotReachDeniedRoute(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	// Listing your own keys is denied to a key: a token must not be able to
	// enumerate or mint tokens.
	rec := getWithKey(t, router, secret, "/api/v1/me/api-keys")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("keyed request to a denied route: %d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "session_required" {
		t.Errorf("code %q, want session_required", body["code"])
	}

	// The same route with a cookie is fine — this limits the credential, not
	// the person.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/api-keys", nil)
	req.AddCookie(sessionCookieFor(t, srv, "alice"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same route with a session: %d, want 200", rec.Code)
	}
}

// A key cannot mint its own successor. Stated separately from the route map
// because it is the property the map exists to produce.
func TestAPIKeyCannotMintAnotherKey(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	body, _ := json.Marshal(map[string]string{"name": "escalated"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/api-keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("minting a key with a key: %d, want 403", rec.Code)
	}
	n, err := srv.db.CountUserAPIKeys("alice")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("alice has %d keys, want 1 — the mint must not have happened", n)
	}
}

// The role is read live, so an admin's key is an admin key and a demotion
// applies on the next call rather than whenever a token would have expired.
func TestKeyCarriesLiveRole(t *testing.T) {
	srv, router := keyEnv(t)
	seedUser(t, srv.db, "boss", database.SystemRoleAdmin)
	secret := mintKey(t, srv, "boss", "ops", nil, false)

	// /admin/audit is admin-only and 404s to everyone else.
	if rec := getWithKey(t, router, secret, "/api/v1/admin/audit"); rec.Code != http.StatusOK {
		t.Fatalf("admin key on an admin route: %d, want 200", rec.Code)
	}

	if err := srv.db.UpdateUserRole("boss", database.SystemRoleViewer); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if rec := getWithKey(t, router, secret, "/api/v1/admin/audit"); rec.Code != http.StatusNotFound {
		t.Fatalf("after demotion: %d, want 404 — nothing is baked into the credential", rec.Code)
	}
}

// Presence answers "who is online now". A cron job holding a token is traffic,
// not attendance.
func TestKeyTrafficIsNotPresence(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	if rec := getWithKey(t, router, secret, "/api/v1/me/capabilities"); rec.Code != http.StatusOK {
		t.Fatalf("keyed request: %d", rec.Code)
	}
	if n := srv.presence.Count(time.Minute); n != 0 {
		t.Errorf("%d principals online after keyed traffic, want 0", n)
	}
}

// A read-scoped personal key is refused every write, the create included, so
// its capabilities say it creates nothing — before, they answered from the
// owner's role alone and whoami promised a create the guard refused
// key_read_scope (docs/bugs 2026-10-28 D7).
func TestCapabilitiesOfAReadScopedKeyCreateNothing(t *testing.T) {
	srv, router := keyEnv(t)
	for _, tc := range []struct {
		scope string
		want  bool
	}{{database.UserKeyScopeRead, false}, {database.UserKeyScopeFull, true}} {
		secret := mintScopedKey(t, srv, "alice", "k-"+tc.scope, tc.scope, nil, false)
		rec := getWithKey(t, router, secret, "/api/v1/me/capabilities")
		var body struct {
			CanCreate   bool `json:"can_create_objects"`
			CanCreateSA bool `json:"can_create_service_account"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v %s", err, rec.Body.String())
		}
		if body.CanCreate != tc.want || body.CanCreateSA != tc.want {
			t.Errorf("%s key: can_create_objects=%v can_create_service_account=%v, want %v",
				tc.scope, body.CanCreate, body.CanCreateSA, tc.want)
		}
	}
}

// `toft whoami` reads this: which credential spoke, what it may reach and when
// it stops, and what the install itself will do — so an agent learns its
// limits by asking rather than by its first refusal.
func TestCapabilitiesNameTheCredentialAndTheInstall(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	rec := getWithKey(t, router, secret, "/api/v1/me/capabilities")
	var body struct {
		Credential map[string]any `json:"credential"`
		Install    struct {
			Version  string              `json:"version"`
			Uploads  *bool               `json:"uploads_allowed"`
			Public   *bool               `json:"public_apps_allowed"`
			Jobs     *bool               `json:"jobs_available"`
			Storage  *bool               `json:"storage_available"`
			Stores   *bool               `json:"store_uploads_available"`
			Runtimes map[string][]string `json:"runtimes"`
		} `json:"install"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	if body.Credential["kind"] != "api_key" || body.Credential["name"] != "ci" || body.Credential["scope"] != "full" {
		t.Errorf("credential = %v", body.Credential)
	}
	if body.Install.Version == "" || body.Install.Uploads == nil || body.Install.Public == nil ||
		body.Install.Jobs == nil || body.Install.Storage == nil || body.Install.Runtimes == nil {
		t.Errorf("install = %+v", body.Install)
	}
	// It is the storage routes' own gate, so the two cannot disagree: this
	// environment configures no storage, and /admin/storage is a 404 here.
	if body.Install.Storage != nil && *body.Install.Storage != srv.storageOffered() {
		t.Errorf("storage_available = %v, storageOffered = %v", *body.Install.Storage, srv.storageOffered())
	}
	// And the store data plane, by the same rule: the gate's own test.
	if body.Install.Stores == nil || *body.Install.Stores != srv.storeUploadsOffered() {
		t.Errorf("store_uploads_available = %v, storeUploadsOffered = %v", body.Install.Stores, srv.storeUploadsOffered())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.AddCookie(sessionCookieFor(t, srv, "alice"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Credential["kind"] != "session" {
		t.Errorf("a browser session reads as %v", body.Credential)
	}
}

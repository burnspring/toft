package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/runtime"
)

// doJSONAs issues a request with a body as the named user.
func doJSONAs(t *testing.T, srv *Server, router http.Handler, who, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The secret is shown once and never again — there is no path, at any
// privilege, that recovers it, because only its SHA-256 was stored.
func TestMintReturnsSecretExactlyOnce(t *testing.T) {
	srv, router := keyEnv(t)

	rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "ci"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d body=%s", rec.Code, rec.Body.String())
	}
	var minted struct {
		Secret string `json:"secret"`
		Key    struct {
			ID        string `json:"id"`
			KeyPrefix string `json:"key_prefix"`
			State     string `json:"state"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if minted.Secret == "" {
		t.Fatal("mint returned no secret")
	}
	if minted.Key.State != database.KeyStateActive {
		t.Errorf("state %q, want active", minted.Key.State)
	}
	// The prefix identifies the key in a CI config without being usable.
	if minted.Key.KeyPrefix != minted.Secret[:12] {
		t.Errorf("prefix %q is not the secret's leading fragment", minted.Key.KeyPrefix)
	}

	// Nothing in any subsequent read carries the secret.
	rec = doJSONAs(t, srv, router, "alice", http.MethodGet, "/api/v1/me/api-keys", nil)
	if bytes.Contains(rec.Body.Bytes(), []byte(minted.Secret)) {
		t.Error("the list endpoint returned the secret — it must exist only in the mint response")
	}
}

// Expiry defaults rather than being absent: a key that never expires is
// offered, not assumed.
func TestMintExpiryDefaultAndNever(t *testing.T) {
	srv, router := keyEnv(t)

	rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "default-expiry"})
	var withDefault struct {
		Key struct {
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &withDefault)
	if withDefault.Key.ExpiresAt == nil {
		t.Fatal("a key minted without an expiry should get the 90-day default")
	}
	days := int(time.Until(*withDefault.Key.ExpiresAt).Hours() / 24)
	if days < 88 || days > 90 {
		t.Errorf("default expiry is %d days out, want ~90", days)
	}

	never := 0
	rec = doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]interface{}{"name": "forever", "expires_in_days": &never})
	var noExpiry struct {
		Key struct {
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &noExpiry)
	if noExpiry.Key.ExpiresAt != nil {
		t.Error("expires_in_days=0 must mean never")
	}
}

// The mint's vocabulary, end to end through a real column.
//
// Absent is the load-bearing case: every client that predates this field sends
// no scope and must keep getting the key it always got. The API is unchanged
// for all of them or the feature is a breaking change wearing an optional
// field.
func TestMintScopeDefaultsToFullAndRoundTrips(t *testing.T) {
	srv, router := keyEnv(t)

	rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "no-scope-named"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d body=%s", rec.Code, rec.Body.String())
	}
	var minted struct {
		Key struct {
			ID    string `json:"id"`
			Scope string `json:"scope"`
		} `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)
	if minted.Key.Scope != database.UserKeyScopeFull {
		t.Errorf("a mint that named no scope produced %q, want full — every existing client "+
			"sends exactly this body", minted.Key.Scope)
	}

	rec = doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "narrow", "scope": database.UserKeyScopeRead})
	if rec.Code != http.StatusCreated {
		t.Fatalf("read-scoped mint: %d body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)
	if minted.Key.Scope != database.UserKeyScopeRead {
		t.Errorf("minted scope %q, want read", minted.Key.Scope)
	}

	// And the listing agrees, because the column travels on every read path
	// rather than only on the mint's response.
	rec = doJSONAs(t, srv, router, "alice", http.MethodGet, "/api/v1/me/api-keys", nil)
	var listed struct {
		Keys []struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	seen := map[string]string{}
	for _, k := range listed.Keys {
		seen[k.Name] = k.Scope
	}
	if seen["narrow"] != database.UserKeyScopeRead || seen["no-scope-named"] != database.UserKeyScopeFull {
		t.Errorf("the listing reports %v — a scope the owner cannot see is a boundary they "+
			"cannot audit", seen)
	}
}

// A word nobody implemented is refused at the door, naming both values.
//
// The alternative is the database's CHECK answering with a 500, which tells the
// caller their request was accepted and something broke. It is also why the
// predicate lives beside the constants rather than in this handler: the guard,
// the mint and the column cannot disagree about the vocabulary if there is only
// one of it.
func TestMintRejectsAnUnknownScope(t *testing.T) {
	srv, router := keyEnv(t)

	for _, scope := range []string{"readonly", "READ", "deploy", "admin"} {
		rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
			map[string]string{"name": "bad-" + scope, "scope": scope})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("scope %q: %d, want 400 — an unimplemented word must never reach the "+
				"column", scope, rec.Code)
		}
	}
}

// The scope joins the mint's audit blob and creates no second action. What
// happened is a mint; splitting it would put "which keys were minted" behind
// two queries forever.
func TestMintAuditCarriesTheScope(t *testing.T) {
	srv, router := keyEnv(t)

	rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "audited", "scope": database.UserKeyScopeRead})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d body=%s", rec.Code, rec.Body.String())
	}

	var action, after string
	err := srv.db.Conn().QueryRow(
		`SELECT action, COALESCE(after::text, '') FROM audit_log
		  WHERE actor_id = 'alice' AND action LIKE 'user.api_key%'
		  ORDER BY occurred_at DESC LIMIT 1`).Scan(&action, &after)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if action != "user.api_key_create" {
		t.Errorf("action %q — a mint is a mint whatever its scope", action)
	}
	// Decoded rather than matched as a substring: jsonb round-trips with its own
	// spacing, and a test that greps the rendering asserts Postgres's formatting
	// instead of the field.
	var blob struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(after), &blob); err != nil {
		t.Fatalf("decode audit blob %s: %v", after, err)
	}
	if blob.Scope != database.UserKeyScopeRead {
		t.Errorf("audit blob %s carries scope %q — the row cannot answer how wide the "+
			"credential was", after, blob.Scope)
	}
}

// The scope is chosen at mint and never edited, and the PATCH is where that
// would leak in. An unknown field decodes and does nothing, which is the
// existing behaviour for every other unknown field and is left alone rather
// than special-cased for one word.
func TestPatchCannotWidenAScope(t *testing.T) {
	srv, router := keyEnv(t)

	rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "fixed", "scope": database.UserKeyScopeRead})
	var minted struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)

	rec = doJSONAs(t, srv, router, "alice", http.MethodPatch, "/api/v1/me/api-keys/"+minted.Key.ID,
		map[string]string{"name": "fixed", "scope": database.UserKeyScopeFull})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d body=%s", rec.Code, rec.Body.String())
	}

	key, err := srv.db.GetUserAPIKey("alice", minted.Key.ID)
	if err != nil || key == nil {
		t.Fatalf("read back: %v", err)
	}
	if key.Scope != database.UserKeyScopeRead {
		t.Errorf("the key is now %q — a scope that can be widened in place is a label rather "+
			"than a boundary", key.Scope)
	}
}

func TestKeyCapAndDuplicateNames(t *testing.T) {
	srv, router := keyEnv(t)

	for i := 0; i < maxAPIKeysPerUser; i++ {
		rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
			map[string]string{"name": fmt.Sprintf("key-%d", i)})
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint %d: %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "one-too-many"})
	if rec.Code != http.StatusConflict {
		t.Errorf("11th key: %d, want 409", rec.Code)
	}
	// The same code the CLI sign-in's approval answers with, and a hint that
	// says an expired key is still holding a slot — the case that reads as a
	// bug when the list shows nothing usable.
	var refusal map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	if refusal["code"] != "key_limit_reached" || refusal["hint"] == nil {
		t.Errorf("11th key refusal = %v, want code key_limit_reached with a hint", refusal)
	}

	rec = doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
		map[string]string{"name": "key-0"})
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate name: %d, want 409", rec.Code)
	}
}

// D-106: a personal key's name collision is name_taken, as a deploy key's is —
// not the envelope's generic `conflict`.
func TestADuplicateKeyNameIsNameTaken(t *testing.T) {
	srv, router := keyEnv(t)
	for i, want := range []int{http.StatusCreated, http.StatusConflict} {
		rec := doJSONAs(t, srv, router, "alice", http.MethodPost, "/api/v1/me/api-keys",
			map[string]string{"name": "twice"})
		if rec.Code != want {
			t.Fatalf("mint %d: %d %s, want %d", i, rec.Code, rec.Body.String(), want)
		}
		if want == http.StatusConflict {
			var refusal map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
			if refusal["code"] != "name_taken" || refusal["hint"] == nil {
				t.Errorf("duplicate name refusal = %v, want code name_taken with a hint", refusal)
			}
		}
	}
}

// Disable is reversible and revocation is not — the two are different tools and
// the API keeps them different.
func TestDisableThenEnable(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)
	keyID := "ci-alice"

	on := true
	rec := doJSONAs(t, srv, router, "alice", http.MethodPatch, "/api/v1/me/api-keys/"+keyID,
		map[string]interface{}{"disabled": &on})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := getWithKey(t, router, secret, "/api/v1/me/capabilities"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key still works: %d", rec.Code)
	}

	off := false
	rec = doJSONAs(t, srv, router, "alice", http.MethodPatch, "/api/v1/me/api-keys/"+keyID,
		map[string]interface{}{"disabled": &off})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := getWithKey(t, router, secret, "/api/v1/me/capabilities"); rec.Code != http.StatusOK {
		t.Fatalf("re-enabled key does not work: %d", rec.Code)
	}
}

// A key id from a URL never names a key on its own.
func TestCannotTouchAnotherUsersKey(t *testing.T) {
	srv, router := keyEnv(t)
	seedUser(t, srv.db, "mallory", database.SystemRolePublisher)
	mintKey(t, srv, "alice", "ci", nil, false)

	rec := doJSONAs(t, srv, router, "mallory", http.MethodDelete, "/api/v1/me/api-keys/ci-alice", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoking someone else's key: %d, want 404", rec.Code)
	}
	if n, _ := srv.db.CountUserAPIKeys("alice"); n != 1 {
		t.Error("alice's key was removed by another user")
	}
}

// The install-wide inventory exists so a leaked credential can be killed from
// a screen rather than from psql — and it stays reachable when the capability
// switch is off, which is exactly when it is needed.
func TestAdminInventoryAndRevocation(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "leaked", nil, false)

	rec := doJSONAs(t, srv, router, "alice", http.MethodGet, "/api/v1/admin/api-keys", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin reading the inventory: %d, want 404", rec.Code)
	}

	rec = doJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/admin/api-keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin inventory: %d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Keys []struct {
			ID         string `json:"id"`
			OwnerLogin string `json:"owner_login"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 1 || list.Keys[0].OwnerLogin != "alice" {
		t.Fatalf("inventory = %+v, want one key owned by alice", list.Keys)
	}

	rec = doJSONAs(t, srv, router, "root", http.MethodDelete, "/api/v1/admin/api-keys/"+list.Keys[0].ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := getWithKey(t, router, secret, "/api/v1/me/capabilities"); rec.Code != http.StatusUnauthorized {
		t.Error("revoked key still authenticates")
	}
}

// A personal key belongs to the person, so deleting them takes it with them.
// An app key belongs to the app and does not work this way — see the ADR.
func TestDeletingUserCascadesTheirKeys(t *testing.T) {
	srv, _ := keyEnv(t)
	seedUser(t, srv.db, "temp", database.SystemRoleViewer)
	mintKey(t, srv, "temp", "ci", nil, false)

	if err := srv.db.DeleteUser("temp", database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "root", Action: "user.delete",
	}); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n, err := srv.db.CountUserAPIKeys("temp"); err != nil || n != 0 {
		t.Errorf("keys after user deletion = %d (err %v), want 0", n, err)
	}
}

// Usage is stored daily and rolled up on read, so changing the bucket can
// never change the total.
func TestKeyUsageBucketsRollUpTheSameRows(t *testing.T) {
	srv, router := keyEnv(t)
	mintKey(t, srv, "alice", "ci", nil, false)

	day := time.Now().UTC().AddDate(0, 0, -3)
	rows := []database.APIKeyUsageBump{
		{KeyID: "ci-alice", KeyKind: database.KeyKindUser, Day: day, StatusClass: 2, Requests: 7},
		{KeyID: "ci-alice", KeyKind: database.KeyKindUser, Day: day, StatusClass: 5, Requests: 3},
		{KeyID: "ci-alice", KeyKind: database.KeyKindUser, Day: day.AddDate(0, 0, 1), StatusClass: 2, Requests: 5},
	}
	if err := srv.db.BumpAPIKeyUsage(rows); err != nil {
		t.Fatalf("bump: %v", err)
	}

	totals := map[string]int64{}
	for _, bucket := range []string{"day", "week", "month"} {
		rec := doJSONAs(t, srv, router, "alice", http.MethodGet,
			"/api/v1/me/api-keys/ci-alice/usage?bucket="+bucket+"&days=30", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("usage (%s): %d body=%s", bucket, rec.Code, rec.Body.String())
		}
		var body struct {
			Series []database.APIKeyUsagePoint `json:"series"`
			Totals database.APIKeyUsagePoint   `json:"totals"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		totals[bucket] = body.Totals.Requests
		var summed int64
		for _, p := range body.Series {
			summed += p.Requests
		}
		if summed != body.Totals.Requests {
			t.Errorf("%s: series sums to %d but totals says %d", bucket, summed, body.Totals.Requests)
		}
		if body.Totals.ServerError != 3 {
			t.Errorf("%s: server errors = %d, want 3 — the outcome split is the point",
				bucket, body.Totals.ServerError)
		}
	}
	if totals["day"] != 15 || totals["week"] != 15 || totals["month"] != 15 {
		t.Errorf("totals differ by bucket: %+v — week and month are rollups of the same rows", totals)
	}
}

// The window is bounded, and the ceiling is a year so month-over-month is
// answerable at all.
func TestKeyUsageWindowIsBounded(t *testing.T) {
	srv, router := keyEnv(t)
	mintKey(t, srv, "alice", "ci", nil, false)

	if rec := doJSONAs(t, srv, router, "alice", http.MethodGet,
		"/api/v1/me/api-keys/ci-alice/usage?days=365", nil); rec.Code != http.StatusOK {
		t.Errorf("days=365: %d, want 200", rec.Code)
	}
	if rec := doJSONAs(t, srv, router, "alice", http.MethodGet,
		"/api/v1/me/api-keys/ci-alice/usage?days=366", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("days=366: %d, want 400", rec.Code)
	}
	if rec := doJSONAs(t, srv, router, "alice", http.MethodGet,
		"/api/v1/me/api-keys/ci-alice/usage?bucket=fortnight", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bucket=fortnight: %d, want 400", rec.Code)
	}
}

// An audited mutation names the credential that made it, not just the person
// it was made as.
func TestAuditNamesTheKey(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	app := &database.App{
		ID: "app-audit", Slug: "audit-app", Name: "Audit App", OwnerID: "alice",
		Framework: "go", Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
	}
	// CreateApp mints the id itself, so the fixture's is discarded.
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// A change that is actually audited. Renaming an app is not — see the
	// audited-actions list in handleUpdateApp.
	off := false
	body, _ := json.Marshal(map[string]interface{}{"notify_failures": &off})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyed update: %d body=%s", rec.Code, rec.Body.String())
	}

	var keyID string
	err := srv.db.Conn().QueryRow(
		`SELECT COALESCE(api_key_id, '') FROM audit_log
		 WHERE target_id = ? AND actor_id = 'alice' ORDER BY occurred_at DESC LIMIT 1`,
		app.ID).Scan(&keyID)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if keyID != "ci-alice" {
		t.Errorf("audit api_key_id = %q, want ci-alice — otherwise a leaked token and its "+
			"owner leave identical rows", keyID)
	}
}

// A personal key at the gateway is authorized normally: possession names a
// person, it does not stand in for a grant.
func TestPersonalKeyAtGatewayObeysRBAC(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	enablePersonalKeys(t, srv, true)

	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "outsider", database.SystemRoleViewer)

	app := &database.App{
		ID: "app-gw", Slug: "gw", Name: "Gateway App", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	ownerSecret := mintKey(t, srv, "owner", "ci", nil, false)
	outsiderSecret := mintKey(t, srv, "outsider", "ci", nil, false)
	router := srv.AppsRouter()

	get := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/apps/gw/data", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(ownerSecret); rec.Code != http.StatusOK {
		t.Errorf("owner's key: %d, want 200", rec.Code)
	}
	// With the install-wide list rung on — the default — the outsider has been
	// told the app exists, so the refusal says so: 403 deny_list_only, never the
	// app's own response (auth_helpers.go, writeAuthzError).
	rec := get(outsiderSecret)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "deny_list_only") {
		t.Errorf("outsider's key with the list rung on: %d %s, want 403 deny_list_only — "+
			"possession is not authorization for a personal key", rec.Code, rec.Body.String())
	}
	// With it off, 404, not 403: an unreadable object does not admit it exists.
	rungOff(t, srv)
	if rec := get(outsiderSecret); rec.Code != http.StatusNotFound {
		t.Errorf("outsider's key on a private app with the rung off: %d, want 404 — possession is not "+
			"authorization for a personal key", rec.Code)
	}
}

// The app never sees the credential, whichever header carried it.
func TestPersonalKeyStrippedBeforeForwarding(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	enablePersonalKeys(t, srv, true)

	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-strip", Slug: "strip", Name: "Strip", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	secret := mintKey(t, srv, "owner", "ci", nil, false)
	router := srv.AppsRouter()

	for _, header := range []string{"Authorization", "X-API-Key"} {
		req := httptest.NewRequest(http.MethodGet, "/apps/strip/data", nil)
		if header == "Authorization" {
			req.Header.Set(header, "Bearer "+secret)
		} else {
			req.Header.Set(header, secret)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", header, rec.Code)
		}
		sawAuth := be.lastReq.Header.Get("Authorization")
		sawAPIKey := be.lastReq.Header.Get("X-API-Key")
		if sawAuth != "" || sawAPIKey != "" {
			t.Errorf("%s: app saw Authorization=%q X-API-Key=%q — the credential must never "+
				"reach app code", header, sawAuth, sawAPIKey)
		}
	}
}

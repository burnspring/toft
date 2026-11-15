package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The endpoint's tests, and the first three lead because their failure would be
// **silent**: no error anywhere, an IdP reporting success, and an install whose
// authorization is quietly wrong.

// scimFixture gives a server, a provider with a token, and one account that
// provider knows about.
func scimFixture(t *testing.T) (*Server, string, *database.User) {
	t.Helper()
	srv := newTestServer(t)

	row := &database.AuthProvider{
		ID: "sso", Kind: "oidc", DisplayName: "SSO", Enabled: true,
		ClientID: "cid", Issuer: "https://idp.example.com", AutoProvision: true,
		DefaultRole: database.SystemRoleViewer,
	}
	if err := srv.db.CreateAuthProvider(row, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin",
	}); err != nil {
		t.Fatalf("CreateAuthProvider: %v", err)
	}

	secret := "test-scim-token"
	sum := sha256.Sum256([]byte(secret))
	if err := srv.db.SetSCIMToken(context.Background(), "sso", hex.EncodeToString(sum[:]),
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "admin"}); err != nil {
		t.Fatalf("SetSCIMToken: %v", err)
	}

	user, _, err := srv.db.ResolveIdentity("sso", database.IdentityProfile{
		Subject: "sub-alice", Login: "alice", Name: "Alice", Email: "alice@example.com",
	}, database.SignInPolicy{DefaultRole: database.SystemRoleViewer, AutoProvision: true},
		database.AuditEntry{ActorType: database.PrincipalTypeUser})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	return srv, secret, user
}

func scimReq(t *testing.T, srv *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/scim+json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, r)
	return rr
}

// SILENT #1. IdPs retry generously and forever, and a lock bumps token_version.
// If a repeat wrote anyway, every sync cycle would end the person's other
// sessions — which reads as a session bug, on a schedule nobody here controls.
func TestARepeatedDeactivationWritesNothing(t *testing.T) {
	srv, token, user := scimFixture(t)
	body := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],` +
		`"Operations":[{"op":"replace","value":{"active":false}}]}`

	if rr := scimReq(t, srv, "PATCH", "/scim/v2/Users/"+user.ID, token, body); rr.Code != http.StatusOK {
		t.Fatalf("first deactivate = %d: %s", rr.Code, rr.Body.String())
	}
	first, err := srv.db.GetTokenVersion(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	if rr := scimReq(t, srv, "PATCH", "/scim/v2/Users/"+user.ID, token, body); rr.Code != http.StatusOK {
		t.Fatalf("repeat deactivate = %d: %s", rr.Code, rr.Body.String())
	}
	second, err := srv.db.GetTokenVersion(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("token version moved from %d to %d on a repeated deactivation — every sync "+
			"cycle would end this person's other sessions", first, second)
	}
}

// SILENT #2. Provider isolation is structural — every lookup joins
// user_identities on (provider_id, login) — and the failure is one directory
// quietly deactivating accounts it was never given authority over, through a
// filter that looks like it worked.
func TestATokenCannotReachAnotherProvidersPeople(t *testing.T) {
	srv, _, user := scimFixture(t)

	other := &database.AuthProvider{
		ID: "other", Kind: "oidc", DisplayName: "Other", Enabled: true,
		ClientID: "cid2", Issuer: "https://other.example.com", AutoProvision: true,
		DefaultRole: database.SystemRoleViewer,
	}
	if err := srv.db.CreateAuthProvider(other, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	otherSecret := "other-scim-token"
	sum := sha256.Sum256([]byte(otherSecret))
	if err := srv.db.SetSCIMToken(context.Background(), "other", hex.EncodeToString(sum[:]),
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "admin"}); err != nil {
		t.Fatal(err)
	}

	// The other directory looks for a person only the first one knows.
	rr := scimReq(t, srv, "GET", `/scim/v2/Users?filter=userName%20eq%20%22alice%22`, otherSecret, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("filter = %d: %s", rr.Code, rr.Body.String())
	}
	var list struct {
		TotalResults int `json:"totalResults"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.TotalResults != 0 {
		t.Errorf("totalResults = %d, want 0 — one directory resolved another's person", list.TotalResults)
	}

	// And it cannot reach them by id either.
	rr = scimReq(t, srv, "GET", "/scim/v2/Users/"+user.ID, otherSecret, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET by id from the wrong provider = %d, want 404", rr.Code)
	}
}

// SILENT #3. A filter matching nothing must be an empty list, not a 404: IdPs
// branch on the difference to decide whether to create an account, and a 404
// reads as a broken endpoint. An unknown *id*, by contrast, is a 404.
func TestAnEmptyFilterResultIsNotAFourOhFour(t *testing.T) {
	srv, token, _ := scimFixture(t)

	rr := scimReq(t, srv, "GET", `/scim/v2/Users?filter=userName%20eq%20%22nobody%22`, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("filter matching nothing = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"totalResults":0`) {
		t.Errorf("body = %s, want totalResults 0", rr.Body.String())
	}

	if rr := scimReq(t, srv, "GET", "/scim/v2/Users/nope", token, ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rr.Code)
	}
}

// The one refusal this endpoint makes, and it is refused whoever asks: an
// install whose only administrator who can sign in is locked cannot be
// administered at all, including to undo it.
func TestTheLastAdministratorIsRefusedInSCIMsOwnErrorShape(t *testing.T) {
	srv, token, user := scimFixture(t)
	if err := srv.db.UpdateUserRole(user.ID, database.SystemRoleAdmin); err != nil {
		t.Fatal(err)
	}
	// The built-in administrator exists but signs in without a provider; to
	// make this account the last *unlocked* one, lock that.
	if _, err := srv.db.LockUser(context.Background(), database.BuiltinAdminID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: user.ID,
	}); err != nil {
		t.Fatal(err)
	}

	rr := scimReq(t, srv, "PATCH", "/scim/v2/Users/"+user.ID, token,
		`{"Operations":[{"op":"replace","value":{"active":false}}]}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("deactivating the last administrator = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "mutability") {
		t.Errorf("body carries no scimType: %s", rr.Body.String())
	}

	// And the account is still usable, which is the point of the refusal.
	after, err := srv.db.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Locked() {
		t.Error("the last administrator was locked despite the refusal")
	}
}

// An ordinary administrator is not protected. §48 refuses to let the *timer*
// lock one because a sweep has no name; a directory administrator has one.
func TestAnOrdinaryAdministratorCanBeDeactivated(t *testing.T) {
	srv, token, user := scimFixture(t)
	if err := srv.db.UpdateUserRole(user.ID, database.SystemRoleAdmin); err != nil {
		t.Fatal(err)
	}
	// The built-in administrator is unlocked, so this is not the last one.

	rr := scimReq(t, srv, "PATCH", "/scim/v2/Users/"+user.ID, token,
		`{"Operations":[{"op":"replace","value":{"active":false}}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("deactivating an administrator = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	after, err := srv.db.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Locked() {
		t.Error("an ordinary administrator survived a SCIM deactivation")
	}
}

// Creation and deletion are refused with a reason, and they are routes on
// purpose: an IdP that receives a clear 501 configures around it, and one that
// receives a 404 concludes the base URL is wrong and stops.
func TestCreateAndDeleteAreRefusedWithAReason(t *testing.T) {
	srv, token, user := scimFixture(t)

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/scim/v2/Users", `{"userName":"bob"}`},
		{"DELETE", "/scim/v2/Users/" + user.ID, ""},
		{"POST", "/scim/v2/Groups", `{"displayName":"x"}`},
	} {
		rr := scimReq(t, srv, tc.method, tc.path, token, tc.body)
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501", tc.method, tc.path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "detail") {
			t.Errorf("%s %s carried no reason: %s", tc.method, tc.path, rr.Body.String())
		}
	}

	// The account is untouched by the attempt.
	after, err := srv.db.GetUser(user.ID)
	if err != nil || after == nil {
		t.Fatal("the refused DELETE removed the account")
	}
}

// No token, no feed. 401 rather than 404, because an IdP's diagnostics
// distinguish them and a 404 sends whoever is configuring it to re-check a base
// URL they pasted correctly.
func TestSCIMRefusesWithoutAToken(t *testing.T) {
	srv, _, _ := scimFixture(t)

	for _, token := range []string{"", "wrong-token"} {
		rr := scimReq(t, srv, "GET", "/scim/v2/ServiceProviderConfig", token, "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", token, rr.Code)
		}
	}
	if rr := scimReq(t, srv, "GET", "/scim/v2/ServiceProviderConfig", "test-scim-token", ""); rr.Code != http.StatusOK {
		t.Errorf("the right token = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// Disabling a provider is the reversible half of removing it. Its directory
// must not go on unlocking accounts and moving group membership meanwhile.
func TestSCIMRefusesADisabledProvidersToken(t *testing.T) {
	srv, token, _ := scimFixture(t)
	if err := srv.db.SetAuthProviderEnabled("sso", false,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "admin"}); err != nil {
		t.Fatalf("SetAuthProviderEnabled: %v", err)
	}
	if rr := scimReq(t, srv, "GET", "/scim/v2/ServiceProviderConfig", token, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("a disabled provider's token = %d, want 401", rr.Code)
	}
}

// An unfiltered list is refused. This endpoint will not enumerate an install's
// people for a credential living in a third-party console, and no IdP needs it
// to.
func TestSCIMWillNotEnumerateAccounts(t *testing.T) {
	srv, token, _ := scimFixture(t)
	rr := scimReq(t, srv, "GET", "/scim/v2/Users", token, "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unfiltered list = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

// Every shape the invariant promises a 501 with a reason for is routed, so a
// directory's pre-check — Okta's `GET /Groups?filter=displayName eq …` — gets
// a SCIM error body and not chi's text/plain 404 or a bare 405. The table is
// the specification: an unregistered promised shape fails here.
func TestEverySCIMShapeTheInvariantPromisesIsRouted(t *testing.T) {
	srv, token, user := scimFixture(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/scim/v2/Users"},
		{http.MethodPut, "/scim/v2/Users/" + user.ID},
		{http.MethodDelete, "/scim/v2/Users/" + user.ID},
		{http.MethodGet, "/scim/v2/Groups"},
		{http.MethodGet, "/scim/v2/Groups?filter=displayName%20eq%20%22Finance%22"},
		{http.MethodGet, "/scim/v2/Groups/g1"},
		{http.MethodPut, "/scim/v2/Groups/g1"},
		{http.MethodPost, "/scim/v2/Groups"},
		{http.MethodDelete, "/scim/v2/Groups/g1"},
	} {
		rec := scimReq(t, srv, tc.method, tc.path, token, `{}`)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501 with a reason: %s", tc.method, tc.path, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "scim+json") || !strings.Contains(rec.Body.String(), "detail") {
			t.Errorf("%s %s did not answer in SCIM's error shape: %s %s", tc.method, tc.path, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
}

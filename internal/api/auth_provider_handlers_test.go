package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

func providerTestEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)
	return srv, srv.Router(http.NotFoundHandler())
}

// disableLocalSignIn switches the built-in password provider off.
//
// Every fresh database has it enabled — that is the bootstrap — so any test about
// "what does the login page offer" has to say which world it is testing. Done
// through the database rather than the API because the API deliberately refuses
// to switch it off while no administrator can sign in another way, which is the
// situation in most of these tests.
func disableLocalSignIn(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.db.SetAuthProviderEnabled(database.LocalProviderID, false, database.AuditEntry{
		ActorType: database.PrincipalTypeOperator, ActorID: "test",
	}); err != nil {
		t.Fatalf("disabling local sign-in: %v", err)
	}
}

const oktaBody = `{"id":"okta","kind":"oidc","display_name":"Okta",
	"issuer":"https://acme.okta.com","client_id":"client","client_secret":"shh-secret"}`

// A provider is a door into the platform, so only an administrator may add one.
func TestAuthProviderAdminSurfaceIsAdminOnly(t *testing.T) {
	srv, router := providerTestEnv(t)

	for _, tc := range []struct {
		who      string
		wantList int
		wantPost int
	}{
		{who: "", wantList: http.StatusUnauthorized, wantPost: http.StatusUnauthorized},
		{who: "pub", wantList: http.StatusForbidden, wantPost: http.StatusForbidden},
		{who: "root", wantList: http.StatusOK, wantPost: http.StatusCreated},
	} {
		name := tc.who
		if name == "" {
			name = "anonymous"
		}
		t.Run(name, func(t *testing.T) {
			if rec := getAs(t, srv, router, tc.who, "/api/v1/admin/auth-providers"); rec.Code != tc.wantList {
				t.Errorf("GET = %d, want %d", rec.Code, tc.wantList)
			}
			rec := sendAs(t, srv, router, tc.who, http.MethodPost, "/api/v1/admin/auth-providers", oktaBody)
			if rec.Code != tc.wantPost {
				t.Errorf("POST = %d, want %d (body: %s)", rec.Code, tc.wantPost, rec.Body.String())
			}
		})
	}
}

// The secret goes in and never comes out — not in the create response, not in
// the listing. Otherwise a stolen admin session yields the credential that lets
// someone stand up a look-alike IdP.
func TestClientSecretIsNeverReturned(t *testing.T) {
	srv, router := providerTestEnv(t)

	created := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers", oktaBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "shh-secret") {
		t.Error("the create response echoed the client secret")
	}

	listing := getAs(t, srv, router, "root", "/api/v1/admin/auth-providers").Body.String()
	if strings.Contains(listing, "shh-secret") {
		t.Error("the listing exposed the client secret")
	}

	// Stored encrypted, and decryptable by the registry that needs it.
	row, err := srv.db.GetAuthProvider("okta")
	if err != nil {
		t.Fatalf("GetAuthProvider: %v", err)
	}
	if row.EncryptedSecret() == "" || row.EncryptedSecret() == "shh-secret" {
		t.Fatalf("secret at rest = %q, want ciphertext", row.EncryptedSecret())
	}
	provider, err := srv.authProvider("okta")
	if err != nil {
		t.Fatalf("authProvider: %v", err)
	}
	if provider.ClientSecret != "shh-secret" {
		t.Fatalf("resolved secret = %q, want the plaintext", provider.ClientSecret)
	}
}

func TestAuthProviderValidation(t *testing.T) {
	srv, router := providerTestEnv(t)

	for _, tc := range []struct {
		name, body, wantSubstr string
		wantCode               int
	}{
		{
			name:       "an OIDC provider with no issuer",
			body:       `{"id":"sso","kind":"oidc","display_name":"SSO","client_id":"c","client_secret":"s"}`,
			wantCode:   http.StatusUnprocessableEntity,
			wantSubstr: "issuer URL is required",
		},
		{
			name:       "no client secret",
			body:       `{"id":"sso","kind":"oidc","display_name":"SSO","client_id":"c","issuer":"https://idp.example.com"}`,
			wantCode:   http.StatusUnprocessableEntity,
			wantSubstr: "client secret is required",
		},
		{
			// Otherwise adding a provider means handing the platform to whoever
			// its IdP authenticates.
			name:       "system_admin as the default role",
			body:       `{"id":"sso","kind":"oidc","display_name":"SSO","client_id":"c","client_secret":"s","issuer":"https://idp.example.com","default_role":"system_admin"}`,
			wantCode:   http.StatusUnprocessableEntity,
			wantSubstr: "cannot be system_admin",
		},
		{
			name:       "an id that would break the redirect URI",
			body:       `{"id":"Acme SSO","kind":"oidc","display_name":"SSO","client_id":"c","client_secret":"s","issuer":"https://idp.example.com"}`,
			wantCode:   http.StatusUnprocessableEntity,
			wantSubstr: "lowercase letters",
		},
		{
			// The environment owns `github` here (see newTestServer), and a row
			// that validates but is never used is indistinguishable from a bug.
			name:       "colliding with an environment-configured provider",
			body:       `{"id":"github","kind":"github","display_name":"GitHub","client_id":"c","client_secret":"s"}`,
			wantCode:   http.StatusUnprocessableEntity,
			wantSubstr: "configured by this install's environment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("POST = %d, want %d (body: %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantSubstr) {
				t.Errorf("body %q does not explain the refusal (%q)", rec.Body.String(), tc.wantSubstr)
			}
		})
	}
}

// An environment-configured provider is the operator's, so the admin API says
// so rather than 404ing on an id the page is displaying.
func TestEnvProviderIsNotEditable(t *testing.T) {
	srv, router := providerTestEnv(t)

	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/github", `{"display_name":"Hijacked"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}

	listing := struct {
		Providers []adminProviderDTO `json:"providers"`
	}{}
	if err := json.Unmarshal(getAs(t, srv, router, "root", "/api/v1/admin/auth-providers").Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, p := range listing.Providers {
		if p.ID == "github" {
			found = true
			if p.Managed {
				t.Error("an environment-configured provider must not be reported as managed")
			}
			if p.RedirectURI != "http://localhost:8080/api/v1/auth/github/callback" {
				t.Errorf("RedirectURI = %q", p.RedirectURI)
			}
		}
	}
	if !found {
		t.Error("the environment-configured provider is missing from the admin listing, so its presence on the login page is unexplainable")
	}
}

// Three doors on one object have to give one answer.
//
// The create path refuses the collision with 422, the edit with 403 and a
// sentence naming the environment, and the delete used to fall through to the
// row lookup and answer a bare 404 — `not found` for a provider the same API
// had just listed, which teaches whoever reads it that the list is unreliable.
//
// 403 and not 404: *404 over 403* protects the existence of something the
// caller may not see, and there is nothing to conceal about a provider on the
// page they are looking at.
func TestEnvProviderIsNotDeletable(t *testing.T) {
	srv, router := providerTestEnv(t)

	before, err := srv.db.ListAuthProviders()
	if err != nil {
		t.Fatalf("listing providers: %v", err)
	}

	rec := sendAs(t, srv, router, "root", http.MethodDelete,
		"/api/v1/admin/auth-providers/github", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	// The same sentence the edit answers with. One refusal, one wording.
	if !strings.Contains(rec.Body.String(), "configured by this install's environment") {
		t.Errorf("the refusal does not say why: %s", rec.Body.String())
	}

	after, err := srv.db.ListAuthProviders()
	if err != nil {
		t.Fatalf("listing providers: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("auth_providers went from %d rows to %d; the refusal was protecting exactly this",
			len(before), len(after))
	}

	// And it is still on the list it was refused from.
	listing := struct {
		Providers []adminProviderDTO `json:"providers"`
	}{}
	if err := json.Unmarshal(getAs(t, srv, router, "root", "/api/v1/admin/auth-providers").Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, p := range listing.Providers {
		found = found || p.ID == "github"
	}
	if !found {
		t.Error("the environment-configured provider left the listing after a refused delete")
	}

	// An id that is neither a row nor the environment's is still 404 — the
	// guard says "this one is the environment's", not "everything is".
	if rec := sendAs(t, srv, router, "root", http.MethodDelete,
		"/api/v1/admin/auth-providers/no-such-provider", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown id = %d, want 404", rec.Code)
	}
}

// Auto-provisioning is on unless the form says otherwise, and off is a state
// the admin API can both write and read back — a switch that silently reverts
// to "create accounts for anyone this IdP authenticates" is the failure that
// matters here.
func TestAutoProvisionRoundTrips(t *testing.T) {
	srv, router := providerTestEnv(t)

	read := func(t *testing.T, id string) adminProviderDTO {
		t.Helper()
		listing := struct {
			Providers []adminProviderDTO `json:"providers"`
		}{}
		if err := json.Unmarshal(getAs(t, srv, router, "root", "/api/v1/admin/auth-providers").Body.Bytes(), &listing); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, p := range listing.Providers {
			if p.ID == id {
				return p
			}
		}
		t.Fatalf("provider %s missing from the listing", id)
		return adminProviderDTO{}
	}

	// Omitted on create means on: the column default, and what every provider
	// did before the switch existed.
	if rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers", oktaBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", rec.Code, rec.Body.String())
	}
	if p := read(t, "okta"); !p.AutoProvision {
		t.Error("a provider created without the field must auto-provision")
	}

	if rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"auto_provision":false}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d body=%s", rec.Code, rec.Body.String())
	}
	if p := read(t, "okta"); p.AutoProvision {
		t.Error("auto_provision:false did not stick")
	}

	// An unrelated edit must not turn it back on — the PATCH shape is partial,
	// and a policy that re-enables itself on a display-name change is worse
	// than one that was never offered.
	if rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"display_name":"Okta (SSO)"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d body=%s", rec.Code, rec.Body.String())
	}
	if p := read(t, "okta"); p.AutoProvision {
		t.Error("an unrelated edit re-enabled auto-provisioning")
	}

	// The environment-configured provider always provisions: it exists to get
	// the first administrator in.
	if p := read(t, "github"); !p.AutoProvision {
		t.Error("an environment-configured provider must auto-provision")
	}
}

// The login page redirects when there is exactly one way in, and only then.
func TestLoginOptionsAutoSelect(t *testing.T) {
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) loginOptionsResponse {
		t.Helper()
		var out loginOptionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	t.Run("one provider forwards", func(t *testing.T) {
		srv, router := providerTestEnv(t)
		disableLocalSignIn(t, srv)
		got := decode(t, getAs(t, srv, router, "", "/api/v1/auth/providers"))
		if len(got.Providers) != 1 || got.AutoSelect != "github" {
			t.Fatalf("got %+v, want the single provider auto-selected", got)
		}
	})

	t.Run("two providers do not", func(t *testing.T) {
		srv, router := providerTestEnv(t)
		disableLocalSignIn(t, srv)
		if rec := sendAs(t, srv, router, "root", http.MethodPost,
			"/api/v1/admin/auth-providers", oktaBody); rec.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
		}
		got := decode(t, getAs(t, srv, router, "", "/api/v1/auth/providers"))
		if len(got.Providers) != 2 || got.AutoSelect != "" {
			t.Fatalf("got %+v, want two providers and no auto-select", got)
		}
	})

	t.Run("a disabled provider is not offered", func(t *testing.T) {
		srv, router := providerTestEnv(t)
		disableLocalSignIn(t, srv)
		if rec := sendAs(t, srv, router, "root", http.MethodPost,
			"/api/v1/admin/auth-providers", oktaBody); rec.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
		}
		// The subject here is what /auth/providers offers, but switching a
		// provider off now needs another way in to survive it — and
		// disableLocalSignIn went round the API, so this install has none that
		// an administrator has ever used. Giving root an identity on the
		// environment provider is that way in, and it is what the same
		// condition on local sign-in has always been tested with.
		linkIdentity(t, srv, "github", "root")
		if rec := sendAs(t, srv, router, "root", http.MethodPatch,
			"/api/v1/admin/auth-providers/okta", `{"enabled":false}`); rec.Code != http.StatusOK {
			t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
		}
		got := decode(t, getAs(t, srv, router, "", "/api/v1/auth/providers"))
		if len(got.Providers) != 1 || got.Providers[0].ID != "github" {
			t.Fatalf("got %+v, want the disabled provider left out", got)
		}
	})

	t.Run("local sign-in suppresses the forward", func(t *testing.T) {
		// A fresh install offers the IdP *and* the built-in password form, which
		// is two ways in — forwarding straight to the IdP would hide the one an
		// operator needs before SSO is proven to work. The password provider is
		// also never itself a forward target: the form is on this page.
		srv, router := providerTestEnv(t)
		got := decode(t, getAs(t, srv, router, "", "/api/v1/auth/providers"))
		if got.AutoSelect != "" {
			t.Errorf("AutoSelect = %q, want none while local sign-in is also offered", got.AutoSelect)
		}
		var kinds []string
		for _, p := range got.Providers {
			kinds = append(kinds, p.ID+":"+p.Kind)
		}
		if len(got.Providers) != 2 {
			t.Fatalf("providers = %v, want github and the built-in local provider", kinds)
		}
	})
}

// A disabled provider must refuse to start a flow, not merely be absent from
// the login page — the URL is guessable and bookmarkable.
func TestDisabledProviderRefusesSignIn(t *testing.T) {
	srv, router := providerTestEnv(t)
	if rec := sendAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/auth-providers", oktaBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}

	rec := getThroughRouter(t, srv, "/api/v1/auth/okta/login")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("login = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}

// An unknown provider is a 404, not a redirect to somewhere surprising — with
// local auth off there is no picker to fall back to.
func TestUnknownProviderIs404(t *testing.T) {
	srv, _ := providerTestEnv(t)
	rec := getThroughRouter(t, srv, "/api/v1/auth/nope/login")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("login = %d, want 404", rec.Code)
	}
}

// State binds a callback to one provider. Without that, state minted at a
// trusted provider could be replayed at a hostile one's callback.
func TestCallbackRejectsAnotherProvidersState(t *testing.T) {
	srv, router := providerTestEnv(t)
	if rec := sendAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/auth-providers", oktaBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	// Start a flow at github and take its state cookie.
	start := getThroughRouter(t, srv, "/api/v1/auth/github/login")
	var state string
	for _, c := range start.Result().Cookies() {
		if c.Name == oauthStateCookie {
			state = c.Value
		}
	}
	if state == "" {
		t.Fatal("no state cookie was set")
	}
	nonce := state[strings.Index(state, "|")+1:]

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/okta/callback?code=stolen&state="+nonce, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: state})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-provider callback = %d, want 400", rec.Code)
	}
}

// The provider's own refusal is not our error: it returns the user to the login
// page with the reason instead of a 500.
func TestCallbackWithProviderErrorReturnsToLogin(t *testing.T) {
	_, router := providerTestEnv(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/github/callback?error=access_denied&error_description=user+refused", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code < 300 || rec.Code >= 400 {
		t.Fatalf("code = %d, want a redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/login?error=access_denied") {
		t.Fatalf("Location = %q, want the login page with the reason", loc)
	}
}

// A secret pasted out of an IdP's console arrives with a newline on it more
// often than not, and the provider's answer to a secret with a newline on it is
// `incorrect_client_credentials` — which names the credential, not the
// whitespace, and only at the end of a browser sign-in nobody can debug from the
// form they typed it into. So it is trimmed on the way in, on both writes.
func TestClientSecretIsTrimmed(t *testing.T) {
	srv, router := providerTestEnv(t)

	created := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers",
		`{"id":"okta","kind":"oidc","display_name":"Okta","issuer":"https://acme.okta.com",
		  "client_id":" client ","client_secret":"  shh-secret\n"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	provider, err := srv.authProvider("okta")
	if err != nil {
		t.Fatalf("authProvider: %v", err)
	}
	if provider.ClientSecret != "shh-secret" {
		t.Errorf("secret after create = %q, want it trimmed", provider.ClientSecret)
	}

	rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/auth-providers/okta",
		`{"client_secret":"\n rotated \t"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	if provider, err = srv.authProvider("okta"); err != nil {
		t.Fatalf("authProvider: %v", err)
	}
	if provider.ClientSecret != "rotated" {
		t.Errorf("secret after update = %q, want it trimmed", provider.ClientSecret)
	}

	// Whitespace alone is the empty secret, which on an edit means "keep the
	// stored one" — the same as the field the form leaves untouched.
	if rec = sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/auth-providers/okta",
		`{"display_name":"Okta SSO","client_secret":"   "}`); rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	if provider, err = srv.authProvider("okta"); err != nil {
		t.Fatalf("authProvider: %v", err)
	}
	if provider.ClientSecret != "rotated" {
		t.Errorf("secret after a whitespace-only edit = %q, want the stored one kept", provider.ClientSecret)
	}
}

// A provider's kind is what it is. PATCH took `kind` unfiltered, and the
// validator accepts `password`, so an OIDC row could become a second
// password-kind provider: a second login form, a row no route could edit
// back, and one the lockout guard would treat as the guaranteed way in.
func TestAProvidersKindCannotBeChanged(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	row := &database.AuthProvider{
		ID: "okta", Kind: database.ProviderKindOIDC, DisplayName: "Okta", Enabled: true,
		ClientID: "cid", Issuer: "https://okta.example.invalid", DefaultRole: database.SystemRoleViewer,
	}
	row.SetEncryptedSecret(mustEncrypt(t, srv, "secret"))
	if err := srv.db.CreateAuthProvider(row, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "root",
	}); err != nil {
		t.Fatal(err)
	}

	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"kind":"password"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("changing a provider's kind = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	after, err := srv.db.GetAuthProvider("okta")
	if err != nil {
		t.Fatal(err)
	}
	if after.Kind != database.ProviderKindOIDC {
		t.Fatalf("kind = %q after a refused change", after.Kind)
	}
	// Re-sending the kind it already has is not a change.
	if rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"kind":"oidc","display_name":"Okta SSO"}`); rec.Code != http.StatusOK {
		t.Fatalf("re-sending the same kind = %d: %s", rec.Code, rec.Body.String())
	}
}

// The stored secret is never carried to a host it was not entered for: moving
// the issuer or an endpoint without a client_secret is refused, or two requests
// sent the decrypted secret to an attacker's token endpoint (code review
// 2026-11-11, R1-1).
func TestMovingAProvidersEndpointsNeedsTheSecretAgain(t *testing.T) {
	srv, router := providerTestEnv(t)
	created := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers",
		`{"id":"okta","kind":"oidc","display_name":"Okta","issuer":"https://acme.okta.com",
		  "client_id":"client","client_secret":"shh-secret"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/auth-providers/okta",
		`{"token_url":"https://attacker.example/token"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("moving the token endpoint without a secret = %d: %s", rec.Code, rec.Body.String())
	}
	if provider, _ := srv.authProvider("okta"); provider.ClientSecret != "shh-secret" || provider.TokenURL != "" {
		t.Fatalf("the refusal changed the row: %+v", provider)
	}
	// With the secret re-entered, the move is the administrator's to make.
	rec = sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/auth-providers/okta",
		`{"token_url":"https://other.okta.com/token","auth_url":"https://other.okta.com/auth","client_secret":"new-secret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("moving with a new secret = %d: %s", rec.Code, rec.Body.String())
	}
	// An edit that moves nothing keeps the stored secret, as before.
	rec = sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/auth-providers/okta",
		`{"display_name":"Okta SSO"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a rename = %d: %s", rec.Code, rec.Body.String())
	}
}

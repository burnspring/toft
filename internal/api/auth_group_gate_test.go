package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// --- A stub IdP, which is what makes the gate's ordering testable ---
//
// The properties this file is about — that a refused person creates no row, and
// that the domain answer wins over the group answer — are only observable
// through the whole callback: the exchange, the userinfo read, the claim, and
// then the ladder. Asserting them against `GroupsPermitted` alone would be
// asserting the thing that was never in doubt.

// stubIdP serves the two endpoints a confidential client actually calls. No
// discovery document: the provider row carries explicit endpoints, which is the
// escape hatch a provider without one already uses.
type stubIdP struct {
	*httptest.Server
	// claims is the userinfo body. A map rather than a struct so a test can
	// omit `groups` entirely — absent and present-and-empty are different
	// facts here and the fixture has to be able to express both.
	claims map[string]any
}

func newStubIdP(t *testing.T, claims map[string]any) *stubIdP {
	t.Helper()
	idp := &stubIdP{claims: claims}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "stub-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.claims)
	})
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	return idp
}

// gatedProviderBody is a provider row pointed at the stub, with whatever gate
// the test is exercising.
func gatedProviderBody(idp *stubIdP, extra map[string]any) string {
	body := map[string]any{
		"id": "stub", "kind": "oidc", "display_name": "Stub",
		"client_id": "cid", "client_secret": "csecret",
		"auth_url":     idp.URL + "/authorize",
		"token_url":    idp.URL + "/token",
		"userinfo_url": idp.URL + "/userinfo",
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// signInThroughStub drives a whole sign-in and returns the callback's response.
func signInThroughStub(t *testing.T, srv *Server, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	start := getThroughRouter(t, srv, "/api/v1/auth/stub/login")
	var state, verifier string
	for _, c := range start.Result().Cookies() {
		switch c.Name {
		case oauthStateCookie:
			state = c.Value
		case oauthVerifierCookie:
			verifier = c.Value
		}
	}
	if state == "" {
		t.Fatalf("no state cookie from /login (got %d: %s)", start.Code, start.Body.String())
	}
	nonce := state[strings.Index(state, "|")+1:]

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/stub/callback?code=good&state="+nonce, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: state})
	req.AddCookie(&http.Cookie{Name: oauthVerifierCookie, Value: verifier})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func createProvider(t *testing.T, srv *Server, router http.Handler, body string) {
	t.Helper()
	if rec := sendAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/auth-providers", body); rec.Code != http.StatusCreated {
		t.Fatalf("create provider = %d: %s", rec.Code, rec.Body.String())
	}
}

// deniedReason reads the `?denied=` the callback sent the browser back with.
func deniedReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rec.Header().Get("Location")
	if i := strings.Index(loc, "denied="); i >= 0 {
		return loc[i+len("denied="):]
	}
	return ""
}

func identityCount(t *testing.T, srv *Server) int {
	t.Helper()
	var n int
	if err := srv.db.Conn().QueryRow(`SELECT count(*) FROM user_identities`).Scan(&n); err != nil {
		t.Fatalf("counting identities: %v", err)
	}
	return n
}

// --- The gate ---

func TestPermittedLoginGroups(t *testing.T) {
	cases := []struct {
		name       string
		claims     map[string]any
		permitted  string
		wantSignIn bool
		wantReason string
	}{
		{
			name:       "a member is admitted",
			claims:     map[string]any{"sub": "u1", "email": "kim@acme.com", "groups": []string{"data-team"}},
			permitted:  "data-team",
			wantSignIn: true,
		},
		{
			name:       "a non-member is refused",
			claims:     map[string]any{"sub": "u1", "email": "kim@acme.com", "groups": []string{"marketing"}},
			permitted:  "data-team",
			wantReason: deniedGroupRefused,
		},
		{
			// The upgrade path: every provider that existed before this feature
			// reads as an empty list and must be unaffected.
			name:       "an empty policy admits anybody",
			claims:     map[string]any{"sub": "u1", "email": "kim@acme.com", "groups": []string{"marketing"}},
			permitted:  "",
			wantSignIn: true,
		},
		{
			// The decision the whole feature turns on.
			name:       "an absent claim is refused",
			claims:     map[string]any{"sub": "u1", "email": "kim@acme.com"},
			permitted:  "data-team",
			wantReason: deniedGroupRefused,
		},
		{
			name:       "an absent claim is admitted when nothing gates",
			claims:     map[string]any{"sub": "u1", "email": "kim@acme.com"},
			permitted:  "",
			wantSignIn: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := providerTestEnv(t)
			idp := newStubIdP(t, tc.claims)
			createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
				"permitted_login_groups": tc.permitted,
			}))

			// Counted rather than compared to one: the fixture already seeds
			// an administrator, so the assertion is that this sign-in added an
			// identity and not that it is the only one.
			before := identityCount(t, srv)
			rec := signInThroughStub(t, srv, router)
			reason := deniedReason(t, rec)
			if tc.wantSignIn {
				if reason != "" {
					t.Fatalf("refused with %q, want a sign-in", reason)
				}
				if after := identityCount(t, srv); after != before+1 {
					t.Fatalf("identities went from %d to %d, want the sign-in to have provisioned one", before, after)
				}
				return
			}
			if reason != tc.wantReason {
				t.Fatalf("denied = %q, want %q (status %d, location %q)",
					reason, tc.wantReason, rec.Code, rec.Header().Get("Location"))
			}
		})
	}
}

// A refused person creates nothing.
//
// The gate sits before ResolveIdentity for exactly this reason: a gate that
// provisioned the people it rejected would fill the People page with everyone
// in the tenant, and leave an activation queue nobody can work through.
func TestARefusedGroupSignInProvisionsNothing(t *testing.T) {
	srv, router := providerTestEnv(t)
	idp := newStubIdP(t, map[string]any{
		"sub": "u1", "email": "kim@acme.com", "groups": []string{"marketing"},
	})
	createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
		"permitted_login_groups": "data-team",
	}))

	before := identityCount(t, srv)
	var users int
	if err := srv.db.Conn().QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("counting users: %v", err)
	}

	if got := deniedReason(t, signInThroughStub(t, srv, router)); got != deniedGroupRefused {
		t.Fatalf("denied = %q, want %q", got, deniedGroupRefused)
	}

	if after := identityCount(t, srv); after != before {
		t.Errorf("identities went from %d to %d — a refused sign-in wrote one", before, after)
	}
	var usersAfter int
	if err := srv.db.Conn().QueryRow(`SELECT count(*) FROM users`).Scan(&usersAfter); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if usersAfter != users {
		t.Errorf("users went from %d to %d — a refused sign-in provisioned an account", users, usersAfter)
	}
}

// The ladder's order is a decision, so it is a test.
//
// Somebody failing both rules is told about the domain, which is the cheaper
// and more specific answer and the one whose remedy is not "ask your directory
// administrator".
func TestTheDomainRefusalWinsOverTheGroupRefusal(t *testing.T) {
	srv, router := providerTestEnv(t)
	idp := newStubIdP(t, map[string]any{
		"sub": "u1", "email": "kim@elsewhere.com", "groups": []string{"marketing"},
	})
	createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
		"allowed_domains":        "acme.com",
		"permitted_login_groups": "data-team",
	}))

	rec := signInThroughStub(t, srv, router)
	// The domain refusal is a 403 with its own copy rather than a redirect, so
	// "not a group denial" is the assertion that survives either shape.
	if got := deniedReason(t, rec); got == deniedGroupRefused {
		t.Fatalf("refused on groups, want the domain refusal to have come first")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the domain gate", rec.Code)
	}
}

// The two refusals are one word to the person and two to the administrator.
func TestTheGatesTwoRefusalsAreDistinctInTheAudit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{
			name:   "a claim that matched nothing",
			claims: map[string]any{"sub": "u1", "email": "kim@acme.com", "groups": []string{"marketing"}},
			want:   signinGroupRefused,
		},
		{
			name:   "a claim that never arrived",
			claims: map[string]any{"sub": "u1", "email": "kim@acme.com"},
			want:   signinGroupClaimAbsent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := providerTestEnv(t)
			idp := newStubIdP(t, tc.claims)
			createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
				"permitted_login_groups": "data-team",
			}))
			before := len(signinRows(t, srv))
			signInThroughStub(t, srv, router)

			rows := signinRows(t, srv)
			if len(rows) != before+1 {
				t.Fatalf("ledger went from %d to %d rows, want one more", before, len(rows))
			}
			got := rows[0]
			if got.Action != ActionSigninFailed {
				t.Errorf("action = %q, want %q", got.Action, ActionSigninFailed)
			}
			// No account was resolved, so there is nothing to attribute this to
			// but the address the provider vouched for.
			if got.TargetID != "" {
				t.Errorf("target = %q, want empty: no account was identified", got.TargetID)
			}
			d := detailOf(t, got)
			if d["reason"] != tc.want {
				t.Errorf("reason = %q, want %q", d["reason"], tc.want)
			}
			if d["attempted"] != "kim@acme.com" {
				t.Errorf("attempted = %q, want the address the provider vouched for", d["attempted"])
			}
		})
	}
}

// The separator, end to end: a claim delivered as one delimited string has to
// reach the gate as a list, or a correctly-typed policy refuses everybody.
func TestAGroupClaimIsSplitOnTheConfiguredSeparator(t *testing.T) {
	claims := map[string]any{"sub": "u1", "email": "kim@acme.com", "groups": "marketing,data-team"}

	t.Run("without a separator it matches nothing", func(t *testing.T) {
		srv, router := providerTestEnv(t)
		idp := newStubIdP(t, claims)
		createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
			"permitted_login_groups": "data-team",
		}))
		if got := deniedReason(t, signInThroughStub(t, srv, router)); got != deniedGroupRefused {
			t.Fatalf("denied = %q, want %q — an unsplit claim is one group whose name has a comma in it",
				got, deniedGroupRefused)
		}
	})

	t.Run("with one it admits", func(t *testing.T) {
		srv, router := providerTestEnv(t)
		idp := newStubIdP(t, claims)
		createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
			"permitted_login_groups": "data-team",
			"groups_separator":       ",",
		}))
		if got := deniedReason(t, signInThroughStub(t, srv, router)); got != "" {
			t.Fatalf("denied = %q, want a sign-in", got)
		}
	})
}

// Individual addresses, in union with the domains.
func TestAllowedEmailsAreAUnionWithTheDomains(t *testing.T) {
	for _, tc := range []struct {
		name    string
		addr    string
		domains string
		emails  string
		wantIn  bool
	}{
		{"the domain admits", "kim@acme.com", "acme.com", "sam@other.com", true},
		{"the address admits outside the domain", "sam@other.com", "acme.com", "sam@other.com", true},
		{"neither refuses", "nobody@elsewhere.com", "acme.com", "sam@other.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := providerTestEnv(t)
			idp := newStubIdP(t, map[string]any{"sub": "u1", "email": tc.addr})
			createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
				"allowed_domains": tc.domains,
				"allowed_emails":  tc.emails,
			}))
			rec := signInThroughStub(t, srv, router)
			if tc.wantIn && rec.Code == http.StatusForbidden {
				t.Fatalf("refused %s, want it admitted: %s", tc.addr, rec.Body.String())
			}
			if !tc.wantIn && rec.Code != http.StatusForbidden {
				t.Fatalf("admitted %s (status %d), want it refused", tc.addr, rec.Code)
			}
		})
	}
}

// --- The form ---

// The one lockout the platform can be certain about, refused where it is typed.
func TestAGroupGateOnGitHubIsRefusedAtTheForm(t *testing.T) {
	srv, router := providerTestEnv(t)
	body := `{"id":"gh2","kind":"github","display_name":"GitHub","client_id":"cid",` +
		`"client_secret":"csecret","permitted_login_groups":"data-team"}`
	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422: GitHub asserts no group claim, so the gate would refuse everyone", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "group") {
		t.Errorf("refusal does not say why: %s", rec.Body.String())
	}
}

func TestTheGateRoundTripsThroughTheAdminForm(t *testing.T) {
	srv, router := providerTestEnv(t)
	idp := newStubIdP(t, map[string]any{"sub": "u1"})
	createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{
		"permitted_login_groups": " Data-Team , platform ",
		"allowed_emails":         "sam@other.com",
		"groups_separator":       ",",
	}))

	rec := sendAs(t, srv, router, "root", http.MethodGet, "/api/v1/admin/auth-providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var resp adminProvidersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	var found *adminProviderDTO
	for i := range resp.Providers {
		if resp.Providers[i].ID == "stub" {
			found = &resp.Providers[i]
		}
	}
	if found == nil {
		t.Fatal("the provider is not in the listing")
	}
	// Normalized on the way in, so what comes back is what the reader parses
	// rather than what somebody typed.
	if found.PermittedLoginGroups != "data-team, platform" {
		t.Errorf("permitted_login_groups = %q, want the normalized form", found.PermittedLoginGroups)
	}
	if found.AllowedEmails != "sam@other.com" {
		t.Errorf("allowed_emails = %q", found.AllowedEmails)
	}
	// Not trimmed: a separator may legitimately be a space.
	if found.GroupsSeparator != "," {
		t.Errorf("groups_separator = %q", found.GroupsSeparator)
	}
}

// --- The checks page ---

func TestCheckLoginGroupGate(t *testing.T) {
	t.Run("skips when no provider gates", func(t *testing.T) {
		srv := newBareServer(t)
		row, found := rowsFor(t, srv, (*Server).checkLoginGroupGate)["login_group_gate"]
		if !found {
			t.Fatal("no row")
		}
		if row.Status != checkSkip {
			t.Fatalf("status = %s, want skip — absent, not off: %s", row.Status, row.Detail)
		}
	})

	t.Run("passes and names the risk when one does", func(t *testing.T) {
		srv := newBareServer(t)
		if _, err := srv.db.Conn().Exec(
			`INSERT INTO auth_providers (id, kind, display_name, client_id, permitted_login_groups)
			 VALUES ('gated-idp', 'oidc', 'Gated', 'cid', 'data-team')`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		row := rowsFor(t, srv, (*Server).checkLoginGroupGate)["login_group_gate"]
		// A gate somebody configured on purpose is not a fault, and a warning
		// nobody can clear is how this page decays.
		if row.Status != checkPass {
			t.Fatalf("status = %s, want pass: %s", row.Status, row.Detail)
		}
		if !strings.Contains(row.Detail, "gated-idp") {
			t.Errorf("detail does not name the provider: %s", row.Detail)
		}
		for _, want := range []string{"locked out", "administrator"} {
			if !strings.Contains(row.Detail, want) {
				t.Errorf("detail does not carry %q — the recovery path is the point: %s", want, row.Detail)
			}
		}
	})
}

var _ = fmt.Sprintf
var _ = database.SystemRoleViewer

// An allowed-domain gate is evidence about an address, and an address the
// provider itself calls unverified is a claim. The provider that says nothing
// either way is not penalised.
func TestAnAddressTheProviderCallsUnverifiedDoesNotPassTheDomainGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claim  map[string]any
		wantOK bool
	}{
		{"said unverified", map[string]any{"sub": "u1", "email": "ceo@acme.com", "email_verified": false}, false},
		{"said verified", map[string]any{"sub": "u2", "email": "kim@acme.com", "email_verified": true}, true},
		{"said nothing", map[string]any{"sub": "u3", "email": "lee@acme.com"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := providerTestEnv(t)
			idp := newStubIdP(t, tc.claim)
			createProvider(t, srv, router, gatedProviderBody(idp, map[string]any{"allowed_domains": "acme.com"}))
			rec := signInThroughStub(t, srv, router)
			if refused := rec.Code == http.StatusForbidden; refused == tc.wantOK {
				t.Errorf("status = %d, wantOK=%v", rec.Code, tc.wantOK)
			}
		})
	}
}

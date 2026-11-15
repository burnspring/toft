package authprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       Provider
		wantErr string
	}{
		{
			name: "oidc with an issuer",
			p:    Provider{ID: "okta", Kind: KindOIDC, DisplayName: "Okta", ClientID: "c", Issuer: "https://acme.okta.com"},
		},
		{
			name: "github needs no issuer",
			p:    Provider{ID: "github", Kind: KindGitHub, DisplayName: "GitHub", ClientID: "c"},
		},
		{
			name:    "oidc with neither issuer nor endpoints",
			p:       Provider{ID: "sso", Kind: KindOIDC, DisplayName: "SSO", ClientID: "c"},
			wantErr: "issuer URL is required",
		},
		{
			// A plain-http issuer on a real network exposes the authorization
			// code and the client secret.
			name:    "oidc over plain http off-box",
			p:       Provider{ID: "sso", Kind: KindOIDC, DisplayName: "SSO", ClientID: "c", Issuer: "http://idp.example.com"},
			wantErr: "must use https",
		},
		{
			name: "oidc over plain http on loopback is allowed for development",
			p:    Provider{ID: "dex", Kind: KindOIDC, DisplayName: "Dex", ClientID: "c", Issuer: "http://localhost:5556/dex"},
		},
		{
			// The id lands in a URL path and in the redirect URI registered at
			// the provider.
			name:    "id with a slash",
			p:       Provider{ID: "acme/sso", Kind: KindOIDC, DisplayName: "SSO", ClientID: "c", Issuer: "https://idp.example.com"},
			wantErr: "only lowercase letters",
		},
		{
			name:    "unknown kind",
			p:       Provider{ID: "saml", Kind: "saml", DisplayName: "SAML", ClientID: "c"},
			wantErr: "unsupported provider kind",
		},
		{
			name:    "no client id",
			p:       Provider{ID: "sso", Kind: KindOIDC, DisplayName: "SSO", Issuer: "https://idp.example.com"},
			wantErr: "client id is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// An empty domain list means "anyone this provider authenticates", which is only
// safe for a single-tenant IdP. A non-empty one must be exact-matched, and an
// address the provider did not supply must not slip through.
func TestEmailAllowed(t *testing.T) {
	open := Provider{}
	restricted := Provider{AllowedDomains: []string{"acme.com", "acme.co.uk"}}

	for _, tc := range []struct {
		p     Provider
		email string
		want  bool
	}{
		{open, "anyone@example.org", true},
		{open, "", true},
		{restricted, "person@acme.com", true},
		{restricted, "person@ACME.com", true},
		{restricted, "person@acme.co.uk", true},
		{restricted, "person@notacme.com", false},
		// Suffix matching would accept this; the check is exact.
		{restricted, "person@evil-acme.com", false},
		{restricted, "person@sub.acme.com", false},
		// A domain policy with no address to check fails closed.
		{restricted, "", false},
		{restricted, "nodomain", false},
	} {
		if got := tc.p.EmailAllowed(tc.email); got != tc.want {
			t.Errorf("EmailAllowed(%q) with %v = %v, want %v", tc.email, tc.p.AllowedDomains, got, tc.want)
		}
	}
}

// PKCE applies to the OIDC path only: GitHub's OAuth2 ignores a code challenge.
func TestUsesPKCE(t *testing.T) {
	if !(&Provider{Kind: KindOIDC}).UsesPKCE() {
		t.Error("an OIDC provider should use PKCE")
	}
	if (&Provider{Kind: KindGitHub}).UsesPKCE() {
		t.Error("GitHub does not support PKCE")
	}
}

func TestResolveDiscoversEndpoints(t *testing.T) {
	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(DiscoveryDoc{
			Issuer:                issuer,
			AuthorizationEndpoint: issuer + "/authorize",
			TokenEndpoint:         issuer + "/token",
			UserinfoEndpoint:      issuer + "/userinfo",
		})
	}))
	defer srv.Close()
	issuer = srv.URL

	p := Provider{ID: "sso", Kind: KindOIDC, ClientID: "c", Issuer: issuer}
	ForgetDiscovery(issuer)
	ep, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ep.AuthURL != issuer+"/authorize" || ep.TokenURL != issuer+"/token" || ep.UserinfoURL != issuer+"/userinfo" {
		t.Fatalf("discovered endpoints = %+v", ep)
	}

	// An explicit override beats the document, so a provider with one wrong
	// entry can be worked around.
	p.TokenURL = "https://override.example/token"
	ForgetDiscovery(issuer)
	ep, err = p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve with override: %v", err)
	}
	if ep.TokenURL != "https://override.example/token" {
		t.Fatalf("override ignored: %+v", ep)
	}
}

// A document whose issuer disagrees with the configured URL means the tokens
// belong to a different authority than the administrator thinks.
func TestDiscoveryRefusesAnIssuerMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DiscoveryDoc{
			Issuer:                "https://someone-else.example",
			AuthorizationEndpoint: "https://someone-else.example/authorize",
			TokenEndpoint:         "https://someone-else.example/token",
			UserinfoEndpoint:      "https://someone-else.example/userinfo",
		})
	}))
	defer srv.Close()

	ForgetDiscovery(srv.URL)
	_, err := Discover(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("Discover() = %v, want an issuer mismatch error", err)
	}
}

func TestDiscoveryIsCached(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": "https://idp.example/authorize",
			"token_endpoint":         "https://idp.example/token",
			"userinfo_endpoint":      "https://idp.example/userinfo",
		})
	}))
	defer srv.Close()

	ForgetDiscovery(srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := Discover(context.Background(), srv.URL); err != nil {
			t.Fatalf("Discover: %v", err)
		}
	}
	if hits != 1 {
		t.Fatalf("issuer was fetched %d times, want 1 — a sign-in must not depend on a fresh IdP round trip", hits)
	}

	// An admin edit must invalidate it, or a corrected issuer appears broken
	// for another half hour.
	ForgetDiscovery(srv.URL)
	if _, err := Discover(context.Background(), srv.URL); err != nil {
		t.Fatalf("Discover after forget: %v", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2 — ForgetDiscovery did not drop the entry", hits)
	}
}

// email_verified gates account linking, so anything that is not affirmatively
// true has to read as false — including the string form real providers ship.
func TestUserinfoNormalization(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		wantLogin     string
		wantVerified  bool
		wantSubject   string
		wantErrSubstr string
	}{
		{
			name:         "preferred_username wins",
			body:         `{"sub":"abc","preferred_username":"jdoe","name":"Jane Doe","email":"jane@acme.com","email_verified":true}`,
			wantLogin:    "jdoe",
			wantVerified: true,
			wantSubject:  "abc",
		},
		{
			name:         "email local part when no username is offered",
			body:         `{"sub":"abc","email":"jane@acme.com","email_verified":"true"}`,
			wantLogin:    "jane",
			wantVerified: true,
			wantSubject:  "abc",
		},
		{
			name:         "absent email_verified is unverified",
			body:         `{"sub":"abc","email":"jane@acme.com"}`,
			wantLogin:    "jane",
			wantVerified: false,
			wantSubject:  "abc",
		},
		{
			name:         "string false is unverified",
			body:         `{"sub":"abc","email":"jane@acme.com","email_verified":"false"}`,
			wantLogin:    "jane",
			wantVerified: false,
			wantSubject:  "abc",
		},
		{
			name:         "subject is the fallback login",
			body:         `{"sub":"abc"}`,
			wantLogin:    "abc",
			wantVerified: false,
			wantSubject:  "abc",
		},
		{
			name:          "no subject is not an identity",
			body:          `{"email":"jane@acme.com"}`,
			wantErrSubstr: "no subject",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			profile, err := oidcProfile(context.Background(), srv.Client(), srv.URL, "", "")
			if tc.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("oidcProfile: %v", err)
			}
			if profile.Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", profile.Subject, tc.wantSubject)
			}
			if profile.Login != tc.wantLogin {
				t.Errorf("Login = %q, want %q", profile.Login, tc.wantLogin)
			}
			if profile.EmailVerified != tc.wantVerified {
				t.Errorf("EmailVerified = %v, want %v", profile.EmailVerified, tc.wantVerified)
			}
		})
	}
}

// The preset catalog is a form filler, so a broken entry surfaces as a provider
// an admin cannot save. Every preset must therefore describe something valid
// once credentials are supplied.
func TestEveryPresetIsAValidProvider(t *testing.T) {
	for _, preset := range Presets() {
		t.Run(preset.ID, func(t *testing.T) {
			if preset.Category != CategoryEnterprise && preset.Category != CategorySocial {
				t.Fatalf("category = %q, want enterprise or social", preset.Category)
			}
			p := Provider{
				ID: preset.ID, Kind: preset.Kind, DisplayName: preset.Name,
				ClientID: "client-id", Issuer: preset.Issuer,
			}
			// A preset with a templated issuer has nothing to validate until the
			// admin fills it in; check the shape it will produce.
			if p.Issuer == "" && preset.Kind == KindOIDC {
				if preset.IssuerTemplate == "" {
					t.Fatal("an OIDC preset needs either an issuer or an issuer template")
				}
				p.Issuer = "https://placeholder.example"
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			// A provider that authenticates outside accounts must be flagged, or
			// the UI cannot warn about an empty domain list.
			if preset.Category == CategorySocial && !preset.MultiTenant {
				t.Error("a social preset must be marked multi_tenant")
			}
		})
	}
}

// Absent and empty are different assertions, and the difference is the one
// thing in this feature that could quietly destroy an install's authorization.
func TestGroupClaimAbsentAndEmptyAreDifferent(t *testing.T) {
	cases := []struct {
		name     string
		claims   map[string]any
		want     []string
		asserted bool
	}{
		{"absent", map[string]any{"sub": "x"}, nil, false},
		{"null", map[string]any{"groups": nil}, nil, false},
		{"empty list", map[string]any{"groups": []any{}}, []string{}, true},
		{"one", map[string]any{"groups": []any{"payments"}}, []string{"payments"}, true},
		{"several", map[string]any{"groups": []any{"a", "b"}}, []string{"a", "b"}, true},
		{"bare string", map[string]any{"groups": "payments"}, []string{"payments"}, true},
		// A shape this product does not understand is a provider it does not
		// understand, not an assertion that somebody is in nothing.
		{"a number", map[string]any{"groups": 42.0}, nil, false},
		{"a list of numbers", map[string]any{"groups": []any{1.0}}, nil, false},
		{"an object", map[string]any{"groups": map[string]any{"a": 1}}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, asserted := claimStrings(tc.claims, "groups", "")
			if asserted != tc.asserted {
				t.Fatalf("asserted = %v, want %v — absent and empty must not collapse", asserted, tc.asserted)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("groups[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The claim's name is an administrator's setting, and empty means `groups`
// rather than the column storing that literal.
func TestTheGroupsClaimNameIsConfigurable(t *testing.T) {
	claims := map[string]any{"roles": []any{"payments"}}
	if _, ok := claimStrings(claims, GroupsClaimOr(""), ""); ok {
		t.Error("the default claim name found something in a token that only carries `roles`")
	}
	got, ok := claimStrings(claims, GroupsClaimOr("roles"), "")
	if !ok || len(got) != 1 || got[0] != "payments" {
		t.Errorf("claimStrings(roles) = %v, %v", got, ok)
	}
	if GroupsClaimOr("  ") != "groups" {
		t.Errorf("GroupsClaimOr(blank) = %q, want groups", GroupsClaimOr("  "))
	}
}

// The id_token is read for claims and never for identity, so a malformed one
// costs nothing: absent, never empty, and never a subject.
func TestIDTokenClaimsAreReadWithoutVerificationAndFailClosed(t *testing.T) {
	// {"groups":["payments"],"sub":"nobody"} — a real payload, unsigned.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"groups":["payments"],"sub":"nobody"}`))
	token := "header." + payload + ".signature"

	claims := claimsFromIDToken(token)
	if claims == nil {
		t.Fatal("a well-formed id_token yielded no claims")
	}
	got, ok := claimStrings(claims, "groups", "")
	if !ok || len(got) != 1 || got[0] != "payments" {
		t.Errorf("groups from id_token = %v, %v", got, ok)
	}

	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.!!!.c", "a.b.c"} {
		if c := claimsFromIDToken(bad); c != nil {
			t.Errorf("claimsFromIDToken(%q) = %v, want nil", bad, c)
		}
	}
}

// --- The permitted-groups gate ---

func TestGroupsPermitted(t *testing.T) {
	asserted := func(groups ...string) *Profile {
		return &Profile{Groups: groups, GroupsAsserted: true}
	}
	cases := []struct {
		name      string
		permitted []string
		profile   *Profile
		want      GroupDecision
	}{
		{"no policy admits a member", nil, asserted("data-team"), GroupsAdmitted},
		{"no policy admits a non-member", nil, asserted("anything"), GroupsAdmitted},
		// The upgrade path, asserted rather than assumed: every provider that
		// existed before this feature reads as an empty list.
		{"no policy admits an absent claim", nil, &Profile{}, GroupsAdmitted},
		{"a match admits", []string{"data-team"}, asserted("data-team"), GroupsAdmitted},
		{"one match out of several admits", []string{"data-team"}, asserted("everyone", "data-team"), GroupsAdmitted},
		{"any of the permitted values admits", []string{"a", "b"}, asserted("b"), GroupsAdmitted},
		{"no match refuses", []string{"data-team"}, asserted("marketing"), GroupsNotPermitted},
		// Present-and-empty is an assertion and is honoured, exactly as the
		// membership reconciler honours it.
		{"an empty claim refuses", []string{"data-team"}, asserted(), GroupsNotPermitted},
		// The decision the whole feature turns on.
		{"an absent claim refuses", []string{"data-team"}, &Profile{}, GroupsClaimAbsent},
		{"a nil profile refuses", []string{"data-team"}, nil, GroupsClaimAbsent},
		// Entra emits object GUIDs whose case is not stable across surfaces.
		{"case does not decide", []string{"Data-Team"}, asserted("dATA-tEAM"), GroupsAdmitted},
		{"surrounding space does not decide", []string{" data-team "}, asserted("data-team "), GroupsAdmitted},
		// A list of nothing but blanks is not a policy, and must not read as a
		// lockout nobody typed.
		{"a blank list is not a policy", []string{"", "  "}, asserted("anything"), GroupsAdmitted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{PermittedLoginGroups: tc.permitted}
			if got := p.GroupsPermitted(tc.profile); got != tc.want {
				t.Fatalf("GroupsPermitted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGroupsPermittedDistinguishesItsTwoRefusals is the reason GroupDecision is
// not a bool: the audit log has to be able to say which, because a claim that
// arrived and matched nothing is fixed in the admin form and a claim that never
// arrived is fixed at the IdP.
func TestGroupsPermittedDistinguishesItsTwoRefusals(t *testing.T) {
	p := &Provider{PermittedLoginGroups: []string{"data-team"}}
	if got := p.GroupsPermitted(&Profile{Groups: []string{"marketing"}, GroupsAsserted: true}); got != GroupsNotPermitted {
		t.Fatalf("a claim that matched nothing = %v, want GroupsNotPermitted", got)
	}
	if got := p.GroupsPermitted(&Profile{}); got != GroupsClaimAbsent {
		t.Fatalf("a claim that never arrived = %v, want GroupsClaimAbsent", got)
	}
}

func TestEmailAllowedUnion(t *testing.T) {
	cases := []struct {
		name    string
		domains []string
		emails  []string
		addr    string
		want    bool
	}{
		{"both empty admits anything", nil, nil, "anyone@anywhere.test", true},
		{"both empty admits a malformed address", nil, nil, "not-an-address", true},
		{"a domain match admits", []string{"acme.com"}, nil, "kim@acme.com", true},
		{"a domain miss refuses", []string{"acme.com"}, nil, "kim@other.com", false},
		// The union, which is the whole point: the domain, plus two contractors.
		{"an address match admits outside the domains", []string{"acme.com"}, []string{"kim@other.com"}, "kim@other.com", true},
		{"a domain match admits with addresses listed", []string{"acme.com"}, []string{"kim@other.com"}, "sam@acme.com", true},
		{"neither refuses", []string{"acme.com"}, []string{"kim@other.com"}, "nobody@elsewhere.com", false},
		{"addresses alone admit a match", nil, []string{"kim@other.com"}, "kim@other.com", true},
		{"addresses alone refuse a miss", nil, []string{"kim@other.com"}, "sam@other.com", false},
		{"case does not decide", []string{"acme.com"}, []string{"Kim@Other.com"}, "KIM@other.COM", true},
		// A policy exists precisely to keep unknown accounts out.
		{"a domain policy fails closed on a malformed address", []string{"acme.com"}, nil, "not-an-address", false},
		{"an address policy fails closed on a malformed address", nil, []string{"kim@other.com"}, "not-an-address", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{AllowedDomains: tc.domains, AllowedEmails: tc.emails}
			if got := p.EmailAllowed(tc.addr); got != tc.want {
				t.Fatalf("EmailAllowed(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestClaimStringsSeparator covers the bug the column exists to fix: a claim
// delivered as one delimited string read as a single group whose name contains
// the delimiter, and matched nothing.
func TestClaimStringsSeparator(t *testing.T) {
	cases := []struct {
		name      string
		claim     any
		separator string
		want      []string
		asserted  bool
	}{
		{"no separator leaves a delimited string whole", "a,b", "", []string{"a,b"}, true},
		{"a separator splits it", "a,b", ",", []string{"a", "b"}, true},
		{"a separator trims the parts", " a , b ", ",", []string{"a", "b"}, true},
		{"empty parts are dropped", "a,,b", ",", []string{"a", "b"}, true},
		{"a semicolon separator", "a;b;c", ";", []string{"a", "b", "c"}, true},
		{"a separator applies inside a list too", []any{"a,b", "c"}, ",", []string{"a", "b", "c"}, true},
		{"a list is unaffected without one", []any{"a", "b"}, "", []string{"a", "b"}, true},
		// Absent must survive the separator: it is what keeps a misconfigured
		// provider from reading as "in no groups".
		{"absent stays absent", nil, ",", nil, false},
		{"present and empty stays empty", "", ",", nil, true},
		{"a wrong-shaped claim still reads as absent", 42.0, ",", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{}
			if tc.claim != nil {
				claims["groups"] = tc.claim
			}
			got, asserted := claimStrings(claims, "groups", tc.separator)
			if asserted != tc.asserted {
				t.Fatalf("asserted = %v, want %v", asserted, tc.asserted)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("groups = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestValidateRefusesGroupGateOnGitHub covers the one lockout this package can
// be *certain* about: GitHub asserts no group claim at all, so the gate would
// refuse every sign-in rather than narrow anything.
func TestValidateRefusesGroupGateOnGitHub(t *testing.T) {
	p := &Provider{
		ID: "gh", Kind: KindGitHub, DisplayName: "GitHub", ClientID: "abc",
		PermittedLoginGroups: []string{"data-team"},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("a permitted login group on a GitHub provider must be refused — it can never be satisfied")
	}
	p.PermittedLoginGroups = nil
	if err := p.Validate(); err != nil {
		t.Fatalf("an unGated GitHub provider must still validate: %v", err)
	}
}

// The exchange is the one call that turns a code into an account, so it is
// where "disabled" has to hold even for a caller that never went through the
// login handlers. It refuses before touching the network: no discovery, no
// token request.
func TestExchangeRefusesADisabledProvider(t *testing.T) {
	p := &Provider{ID: "okta", Kind: KindOIDC, Enabled: false,
		ClientID: "id", ClientSecret: "secret", Issuer: "http://127.0.0.1:1"}
	_, err := p.Exchange(context.Background(), "code", "http://localhost/cb", "verifier")
	if err != ErrProviderDisabled {
		t.Fatalf("Exchange on a disabled provider = %v, want ErrProviderDisabled", err)
	}
}

// A token endpoint that accepts the connection and never answers fails the
// sign-in at the package's bound rather than holding the callback open: the
// exchange runs on the bounded client, not http.DefaultClient.
func TestExchangeIsBoundedWhenTheTokenEndpointHangs(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	saved := httpClient
	httpClient = &http.Client{Timeout: 200 * time.Millisecond}
	defer func() { httpClient = saved }()

	p := &Provider{ID: "sso", Kind: KindOIDC, Enabled: true, ClientID: "c", ClientSecret: "s",
		AuthURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", UserinfoURL: srv.URL + "/userinfo"}
	done := make(chan error, 1)
	go func() {
		_, err := p.Exchange(context.Background(), "code", "http://localhost/cb", "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Exchange against a hanging token endpoint succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exchange against a hanging token endpoint did not return: the token POST is unbounded")
	}
}

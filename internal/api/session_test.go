package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/database"
)

func TestStripPlatformCookiesRemovesOnlyPlatformCookies(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"session removed", "toft_session=jwt", ""},
		{"app session removed", "toft_app_session=jwt", ""},
		{"app cookie kept", "app_pref=dark", "app_pref=dark"},
		{"mixed", "toft_session=jwt; app_pref=dark; toft_app_session=jwt", "app_pref=dark"},
		{"spacing preserved", "  toft_session=jwt ;  csrf=abc  ", "csrf=abc"},
		{"lookalike name kept", "session_id=abc", "session_id=abc"},
		{"dev auth removed", "toft_dev_auth=hmac; keep=1", "keep=1"},
		{"oauth removed", "toft_oauth_state=x; toft_oauth_redirect=y; keep=1", "keep=1"},
		{"empty value cookie", "toft_session=; keep=1", "keep=1"},
		// The prefix is the rule, so a name nobody has written yet is already
		// covered — which is the property a list of names does not have.
		{"anything under the prefix removed", "toft_whatever=x; keep=1", "keep=1"},
		// The pre-prefix names, still in browsers for up to a session TTL after
		// an upgrade. `session` is not among them, and has its own test below.
		{"legacy app session removed", "app_session=jwt; keep=1", "keep=1"},
		{"legacy dev auth removed", "dev_auth=hmac; keep=1", "keep=1"},
		{"legacy oauth removed", "oauth_state=x; oauth_redirect=y; keep=1", "keep=1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/apps/demo/", nil)
			req.Header.Set("Cookie", tc.in)

			stripPlatformCookies(req)

			if got := req.Header.Get("Cookie"); got != tc.want {
				t.Errorf("Cookie = %q, want %q", got, tc.want)
			}
		})
	}
}

// The name `session` belongs to the app now, and this is the test that says so.
//
// Flask's default SESSION_COOKIE_NAME is `session`. Stripping is by name —
// a Cookie header carries no paths — so while the platform held that name,
// every deployed Flask app using `flask.session` had its own cookie deleted
// from every request it received and its session silently reset on each one.
//
// Giving it back is safe because the platform's cookie is `Path=/api/v1` and
// an app is served under `/apps/{slug}`: a browser never sends it here. What
// arrives under this name is the app's own, and it must survive.
func TestAnAppsOwnSessionCookieReachesTheApp(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/", nil)
	req.Header.Set("Cookie", "session=flasks-own; toft_session=platform-jwt; keep=1")

	stripPlatformCookies(req)

	if got, want := req.Header.Get("Cookie"), "session=flasks-own; keep=1"; got != want {
		t.Errorf("Cookie = %q, want %q", got, want)
	}
}

func TestStripPlatformCookiesNoHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apps/demo/", nil)
	stripPlatformCookies(req)

	if _, ok := req.Header["Cookie"]; ok {
		t.Error("Cookie header should not be created when absent")
	}
}

func TestSessionCookiesArePathScoped(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example"}}
	rec := httptest.NewRecorder()

	s.setSessionCookies(rec, auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(time.Hour)})

	byName := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		byName[c.Name] = c
	}

	session, ok := byName[sessionCookie]
	if !ok {
		t.Fatalf("no %s cookie set", sessionCookie)
	}
	if session.Path != sessionCookiePath {
		t.Errorf("session Path = %q, want %q", session.Path, sessionCookiePath)
	}
	if !session.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	// Secure is the default — inferring it from BASE_URL shipped cleartext
	// cookies behind TLS-terminating load balancers (task 1.8).
	if !session.Secure {
		t.Error("session cookie must be Secure by default")
	}

	appSession, ok := byName[appSessionCookie]
	if !ok {
		t.Fatalf("no %s cookie set", appSessionCookie)
	}
	if appSession.Path != appSessionCookiePath {
		t.Errorf("app_session Path = %q, want %q", appSession.Path, appSessionCookiePath)
	}
	if appSession.Value != session.Value {
		t.Error("both cookies should carry the same token")
	}
}

// A plain-http BASE_URL no longer drops the Secure flag — only the explicit
// dev opt-out does. Behind a TLS-terminating proxy BASE_URL is often left at
// its http://localhost default, and inferring from it shipped both session
// cookies in the clear.
func TestSessionCookiesSecureByDefaultEvenOverPlainHTTP(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "http://localhost:8080"}}
	rec := httptest.NewRecorder()

	s.setSessionCookies(rec, auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(time.Hour)})

	for _, c := range rec.Result().Cookies() {
		if !c.Secure {
			t.Errorf("%s must be Secure unless DEV_INSECURE_COOKIES is set", c.Name)
		}
	}
}

func TestDevInsecureCookiesOptOut(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "http://intranet.example:8080", DevInsecureCookies: true}}
	rec := httptest.NewRecorder()

	s.setSessionCookies(rec, auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(time.Hour)})

	for _, c := range rec.Result().Cookies() {
		if c.Secure {
			t.Errorf("%s must not be Secure with DEV_INSECURE_COOKIES set, or it will never be sent", c.Name)
		}
	}
}

func TestClearSessionCookiesExpiresBoth(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example"}}
	rec := httptest.NewRecorder()

	s.clearSessionCookies(rec)

	// Four: the current pair and the pre-prefix pair. Clearing only the current
	// names would leave a browser upgraded mid-session holding a legacy cookie
	// that still names a session — a sign-out that does not sign anybody out.
	cookies := rec.Result().Cookies()
	if len(cookies) != 4 {
		t.Fatalf("got %d cookies, want 4", len(cookies))
	}
	cleared := map[string]bool{}
	for _, c := range cookies {
		cleared[c.Name] = true
	}
	for _, name := range []string{sessionCookie, appSessionCookie, auth.LegacySessionCookie, "app_session"} {
		if !cleared[name] {
			t.Errorf("logout left %s alive", name)
		}
	}
	for _, c := range cookies {
		if c.MaxAge >= 0 {
			t.Errorf("%s MaxAge = %d, want negative", c.Name, c.MaxAge)
		}
		if c.Value != "" {
			t.Errorf("%s value = %q, want empty", c.Name, c.Value)
		}
	}
}

// The upgrade window has closed. Neither pre-prefix name authenticates
// anybody: both are names an app page can set from script, so reading them was
// a way for a deployed app to choose who the platform thought the viewer was.
func TestTheLegacySessionCookiesAreNoLongerRead(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	req.AddCookie(&http.Cookie{Name: auth.LegacySessionCookie, Value: "planted-jwt"})
	if got := auth.SessionToken(req); got != "" {
		t.Errorf("platform token = %q, want none from the legacy cookie", got)
	}

	for _, name := range []string{"app_session", auth.LegacySessionCookie} {
		req := httptest.NewRequest(http.MethodGet, "/apps/demo/", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: "planted-jwt"})
		if got := (&Server{}).appSessionToken(req); got != "" {
			t.Errorf("gateway token from %s = %q, want none", name, got)
		}
	}
}

// Signing in again is what retires a legacy cookie early, so the window is
// measured in one person's habits rather than in the full TTL.
func TestSigningInExpiresTheLegacyPair(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example"}}
	rec := httptest.NewRecorder()

	s.setSessionCookies(rec, auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(time.Hour)})

	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case auth.LegacySessionCookie, "app_session":
			if c.MaxAge >= 0 || c.Value != "" {
				t.Errorf("%s should be expired by a sign-in, got value %q MaxAge %d",
					c.Name, c.Value, c.MaxAge)
			}
		case sessionCookie, appSessionCookie:
			if c.Value != "the-jwt" {
				t.Errorf("%s value = %q, want the new token", c.Name, c.Value)
			}
		default:
			t.Errorf("unexpected cookie %q", c.Name)
		}
	}
}

// The apps gateway reads two names: its own, and the platform's for a
// single-port install where the browser attaches both.
func TestTheAppsGatewayAcceptsItsOwnAndThePlatformCookie(t *testing.T) {
	for _, name := range []string{appSessionCookie, auth.SessionCookie} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/apps/demo/", nil)
			req.AddCookie(&http.Cookie{Name: name, Value: "the-jwt"})
			if got := (&Server{}).appSessionToken(req); got != "the-jwt" {
				t.Errorf("token = %q, want the-jwt", got)
			}
		})
	}
}

// The DEV_PASSWORD gate's cookie is Secure where the install's cookies are, and
// carries a signed expiry, so a captured one stops opening the gate after its
// thirty days rather than living as long as the password (API-AUTH-5).
func TestTheDevGateCookieIsSecureAndExpires(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example", DevPassword: "pw"}}
	var reached bool
	h := s.devBasicAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var gate *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == devAuthCookie {
			gate = c
		}
	}
	if !reached || gate == nil {
		t.Fatalf("basic auth: reached=%v cookie=%v", reached, gate)
	}
	if !gate.Secure {
		t.Error("the gate cookie is not Secure on an https install")
	}

	now := time.Now()
	if !s.validDevToken(gate.Value, now) {
		t.Error("the cookie just issued does not open the gate")
	}
	if s.validDevToken(gate.Value, now.Add(31*24*time.Hour)) {
		t.Error("the cookie still opens the gate after its thirty days")
	}
	if s.validDevToken(s.devToken(now.Add(-time.Minute)), now) {
		t.Error("an expired token opens the gate")
	}
	forged := s.devToken(now.Add(time.Hour))
	if s.validDevToken("9999999999"+forged[strings.IndexByte(forged, '.'):], now) {
		t.Error("a token whose expiry was rewritten opens the gate")
	}
}

// An app page can plant `toft_app_session` at a longer path, which the browser
// then sends first. Two copies resolve to no session — not the first, not the
// platform cookie beside them — so app A cannot choose whose session app B's
// viewers carry (API-PROXY-1).
func TestTwoCopiesOfTheAppSessionCookieAreNoSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apps/b/", nil)
	req.AddCookie(&http.Cookie{Name: appSessionCookie, Value: "planted-jwt"})
	req.AddCookie(&http.Cookie{Name: appSessionCookie, Value: "the-jwt"})
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "the-jwt"})
	if got := (&Server{}).appSessionToken(req); got != "" {
		t.Errorf("token = %q, want none from two gateway cookies", got)
	}
}

// A deployed app's Set-Cookie is confined to the app's own path prefix, and
// **every** Path attribute is rewritten rather than the first.
//
// RFC 6265 gives the last Path the win, so rewriting only the first let an app
// emit `Path=/x; Path=/api/v1` and plant a cookie on the platform's own path —
// where a name it chose would overwrite the platform's cookie of that name in
// the browser's jar. That confinement is the argument for handing `session`
// back to apps, so it is tested rather than assumed.
func TestEveryCookiePathIsConfinedToTheApp(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"root", "a=b; Path=/", "a=b; Path=/apps/demo/"},
		{"subpath", "a=b; Path=/foo", "a=b; Path=/apps/demo/foo"},
		{"absent", "a=b; HttpOnly", "a=b; HttpOnly; Path=/apps/demo/"},
		{"lowercase", "a=b; path=/foo", "a=b; Path=/apps/demo/foo"},
		{"attributes after", "a=b; Path=/; HttpOnly; SameSite=Lax",
			"a=b; Path=/apps/demo/; HttpOnly; SameSite=Lax"},
		// The escape: the second Path is the one the browser would honour.
		{"two paths", "a=b; Path=/x; Path=/api/v1",
			"a=b; Path=/apps/demo/x; Path=/apps/demo/api/v1"},
		// A value that merely looks like an attribute is not one.
		{"value is not an attribute", "a=path=/api/v1; Path=/",
			"a=path=/api/v1; Path=/apps/demo/"},
		// A prefix-aware app that set the path itself is left alone; a path
		// that merely starts with the same letters is not the prefix.
		{"already prefixed", "a=b; Path=/apps/demo", "a=b; Path=/apps/demo"},
		{"already under the prefix", "a=b; Path=/apps/demo/sub", "a=b; Path=/apps/demo/sub"},
		{"a sibling slug is not the prefix", "a=b; Path=/apps/demo2", "a=b; Path=/apps/demo/apps/demo2"},
		// A browser trims blanks around the attribute name, so this is a Path
		// and, being last, the one it honours (PB-6).
		{"blank before the equals", "a=b; Path=/x; path =/apps/payroll",
			"a=b; Path=/apps/demo/x; Path=/apps/demo/apps/payroll"},
		// Without a leading slash the value is not concatenated onto the
		// prefix: /apps/demo2 is a sibling app's path (PB-6).
		{"no leading slash", "a=b; Path=2", "a=b; Path=/apps/demo/"},
		{"no value", "a=b; Path", "a=b; Path=/apps/demo/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteCookiePath(tc.in, "/apps/demo"); got != tc.want {
				t.Errorf("rewriteCookiePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The cookie's MaxAge is the token's own remaining life, on both origins.
//
// These were two constants that happened to agree — `tokenExpiry` in
// internal/auth and `sessionTTL` here, both eight hours, with nothing in code
// tying them together. A policy applied to one and not the other is a browser
// holding a cookie the server refuses, or discarding one it would have taken.
func TestCookieMaxAgeIsTheTokensOwnDeadline(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example"}}
	sess := auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(30 * time.Minute)}

	platform := httptest.NewRecorder()
	s.setSessionCookies(platform, sess)
	appsOrigin := httptest.NewRecorder()
	s.setAppSessionCookie(appsOrigin, nil, sess)

	check := func(rec *httptest.ResponseRecorder, where string) {
		t.Helper()
		for _, c := range rec.Result().Cookies() {
			if c.Value == "" {
				continue // the legacy pair, being expired
			}
			// A minute of slack: the assertion is "half an hour, not eight
			// hours", not a stopwatch.
			if c.MaxAge < 29*60 || c.MaxAge > 30*60 {
				t.Errorf("%s %s MaxAge = %d, want the token's ~1800s", where, c.Name, c.MaxAge)
			}
		}
	}
	check(platform, "platform")
	check(appsOrigin, "apps origin")
}

// A lapsed token must not produce a cookie with no Max-Age at all — which is
// what MaxAge 0 renders as, and which a browser keeps for the whole browsing
// session.
func TestALapsedTokenStillExpiresItsCookie(t *testing.T) {
	if got := cookieMaxAge(auth.Session{ExpiresAt: time.Now().Add(-time.Hour)}); got < 1 {
		t.Errorf("cookieMaxAge = %d, want a positive floor", got)
	}
}

// The act-for blob is bound to the ABSOLUTE deadline, never to the sliding
// `exp`. Passing `exp` through would make the archetypal Streamlit app's
// act-for button fail thirty minutes into every session.
func TestActForIsBoundToTheAbsoluteDeadline(t *testing.T) {
	policy := auth.SessionPolicy{Absolute: 8 * time.Hour, Idle: 30 * time.Minute}
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)

	j := auth.NewJWTService("test-secret-key-32-bytes-long!!!")
	minted, err := j.Generate(auth.GenerateParams{UserID: "priya", TokenVersion: 1, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := j.Validate(minted.Token)
	if err != nil {
		t.Fatal(err)
	}
	// As it would arrive two hours into a session with a 30-minute idle window.
	claims.AuthTime = jwt.NewNumericDate(start)
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().UTC().Add(30 * time.Minute))

	deadline, ok := actForDeadline(claims, policy)
	if !ok {
		t.Fatal("no act-for deadline for a live session")
	}
	if want := start.Add(8 * time.Hour); !deadline.Equal(want) {
		t.Errorf("deadline = %v, want the absolute ceiling %v", deadline, want)
	}
	if !deadline.After(claims.ExpiresAt.Time) {
		t.Error("the blob was bound to the sliding exp, which is the failure this test exists for")
	}
}

// No session behind the request, or claims carrying no clock, means no blob.
func TestActForNeedsASessionWithAClock(t *testing.T) {
	policy := auth.SessionPolicy{Absolute: 8 * time.Hour}
	if _, ok := actForDeadline(nil, policy); ok {
		t.Error("minted an act-for deadline for an anonymous request")
	}
	if _, ok := actForDeadline(&auth.Claims{UserID: "priya"}, policy); ok {
		t.Error("minted an act-for deadline from claims with no clock at all")
	}
}

// A two-host install serving secure cookies gives browsers the __Host- copy —
// Path=/, Secure, no Domain — beside the plain pair a non-browser client reads,
// and a sign-out expires all of them.
func TestATwoHostInstallSetsTheHostCookie(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example", AppsBaseURL: "https://apps.example"}}
	rec := httptest.NewRecorder()
	s.setSessionCookies(rec, auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(time.Hour)})

	var host *http.Cookie
	plain := false
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case auth.HostSessionCookie:
			host = c
		case auth.SessionCookie:
			plain = c.Value == "the-jwt"
		}
	}
	if host == nil || host.Value != "the-jwt" || host.Path != "/" || !host.Secure || !host.HttpOnly || host.Domain != "" {
		t.Fatalf("__Host- cookie = %+v", host)
	}
	if !plain {
		t.Error("the plain cookie a non-browser client reads was not set")
	}

	rec = httptest.NewRecorder()
	s.clearSessionCookies(rec)
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.HostSessionCookie && c.MaxAge < 0 && c.Path == "/" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("sign-out left the __Host- cookie alive")
	}

	// One host: nothing changes.
	s = &Server{cfg: &config.Config{BaseURL: "https://toft.example", AppsBaseURL: "https://toft.example:8081"}}
	rec = httptest.NewRecorder()
	s.setSessionCookies(rec, auth.Session{Token: "the-jwt", ExpiresAt: time.Now().Add(time.Hour)})
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.HostSessionCookie {
			t.Error("a one-host install set the __Host- cookie")
		}
	}
}

// An app cannot spell its way past the strip with a browser cookie prefix.
func TestHostPrefixedPlatformCookiesAreStillThePlatforms(t *testing.T) {
	for _, name := range []string{auth.HostSessionCookie, "__Secure-toft_session", "toft_session"} {
		if !isPlatformCookie(name) {
			t.Errorf("%s is not treated as the platform's", name)
		}
	}
	if isPlatformCookie("__Host-session") {
		t.Error("an app's own __Host- cookie was taken for the platform's")
	}
}

// A session is minted from the row as it stands, not from a User the caller
// read before a demotion: the role claim follows the row (security review
// 2026-11-14, SI-6).
func TestMintSessionTakesTheRoleFromTheRow(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "riley", database.SystemRoleAdmin)
	stale, err := srv.db.GetUser("riley")
	if err != nil || stale == nil {
		t.Fatal(err)
	}
	if err := srv.db.UpdateUserRole("riley", database.SystemRoleViewer); err != nil {
		t.Fatal(err)
	}
	sess, err := srv.mintSessionFrom(stale, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := srv.jwt.Validate(sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SystemRole != database.SystemRoleViewer {
		t.Errorf("minted role = %q, want the row's %q", claims.SystemRole, database.SystemRoleViewer)
	}
}

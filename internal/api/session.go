package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"toft/internal/auth"
)

// Session cookies are deliberately split by path so that a deployed app never
// receives the platform API credential:
//
//	toft_session      Path=/api/v1  — the platform API and the SPA
//	toft_app_session  Path=/apps    — the apps gateway, to authorize proxying
//	toft_apps_seen    Path=/apps    — the two-host handshake's marker
//	toft_visitor      Path=/apps    — the anonymous audience's id, when switched on
//
// Both carry the same JWT. Cookies are not port-scoped, so a hostile app served
// from the apps gateway can still cause the browser to attach the platform
// cookie to a cross-origin request at the platform port — that is what
// originCheck blocks.
//
// Every name is under `auth.CookiePrefix`, and that prefix — not this list — is
// what the strip rule below is written against. The reasoning for the prefix,
// and for the window in which the pre-prefix names are still honoured, is in
// `internal/auth/cookies.go`.
const (
	sessionCookie    = auth.SessionCookie
	appSessionCookie = auth.CookiePrefix + "app_session"

	sessionCookiePath    = "/api/v1"
	appSessionCookiePath = "/apps"
	oauthCookiePath      = "/api/v1/auth"
)

// sessionPolicy is the install's answer to how long a session lives and how
// long it may sit unused. Read live rather than held as a constant or baked
// into the token at mint: an administrator who shortens the lifetime means
// *now*, including for the sessions already outstanding. `database.DB` caches
// it and drops the cache on write, because this is read on every authenticated
// request.
func (s *Server) sessionPolicy() auth.SessionPolicy {
	return s.db.SessionPolicy()
}

// sessionOptions is what every mount of auth.Middleware is given, so that all
// of them renew on the same rule and read the same policy. A mount that built
// its own would be the sixth enforcement point nobody wrote down.
func (s *Server) sessionOptions() auth.Options {
	return auth.Options{
		Policy:     s.sessionPolicy,
		Renew:      s.setSessionCookies,
		HostCookie: s.hostSessionCookie,
	}
}

// hostSessionCookie is whether browsers on this install carry the session as
// auth.HostSessionCookie: secure cookies, which `__Host-` requires, on an
// install where another host can toss a plain cookie onto the platform's —
// two hostnames, where the apps host is a sibling, or **any app with a
// hostname of its own**. An app on reports.example.com beside a platform on
// toft.example.com can set `Domain=example.com; toft_session=…`, and a
// signed-out viewer would then be signed in as the attacker. A name that is
// merely recorded counts, because switching it on is one click away.
//
// Turning it on signs browsers holding only the plain cookie out once; that
// is the cost of the boundary, paid on the first hostname rather than by
// every install.
func (s *Server) hostSessionCookie() bool {
	if !s.secureCookies() {
		return false
	}
	return s.cfg.TwoHostMode() || s.appHostnamesRecorded()
}

// appHostnamesRecorded reports whether any app has a hostname recorded, read
// at most every appHostnamesTTL: it is asked on every authenticated request,
// and a hostname takes effect only after a rebuild anyway. The setter drops
// the cache, so the replica that saved one answers at once.
func (s *Server) appHostnamesRecorded() bool {
	if s.db == nil {
		return false // a Server built by hand for a cookie test has no apps
	}
	if c := s.hostnamesSeen.Load(); c != nil && time.Since(c.at) < appHostnamesTTL {
		return c.any
	}
	n, err := s.db.CountRecordedHostnames()
	if err != nil {
		// Fail toward the stricter cookie: a browser re-signs in, nobody is tossed.
		return true
	}
	s.hostnamesSeen.Store(&hostnamesCache{any: n > 0, at: time.Now()})
	return n > 0
}

const appHostnamesTTL = 30 * time.Second

type hostnamesCache struct {
	any bool
	at  time.Time
}

// sessionToken is auth.SessionTokenFor with this install's mode.
func (s *Server) sessionToken(r *http.Request) string {
	return auth.SessionTokenFor(r, s.hostSessionCookie())
}

// legacyPlatformCookieNames are the pre-prefix names. They are still stripped
// from a proxied request, because a browser upgraded mid-session still holds
// them and none is a name an app would have chosen.
//
// `session` is deliberately **not** among them. That is the whole point of the
// prefix: it is Flask's default `SESSION_COOKIE_NAME`, stripping is by name
// because a Cookie header carries no paths, and so every deployed Flask app
// using `flask.session` lost its own session on every request. It never needed
// stripping either — `Path=/api/v1` means a browser does not send it to an app
// under `/apps/{slug}` at all.
var legacyPlatformCookieNames = []string{
	"app_session", "dev_auth", "oauth_state", "oauth_verifier", "oauth_redirect",
}

// secureCookies defaults to true: behind a TLS-terminating load balancer with
// BASE_URL left at its http://localhost default, inferring from the URL
// silently shipped both session cookies recoverable from any cleartext
// request. Plain-http deployments must opt out explicitly
// (DEV_INSECURE_COOKIES=1); http://localhost keeps working either way, since
// browsers treat localhost as a secure context.
func (s *Server) secureCookies() bool {
	return !s.cfg.DevInsecureCookies
}

// sessionCookiePaths is the pair this install issues, and legacySessionCookiePaths
// the pair it may still have to expire. Same paths, older names.
var (
	sessionCookiePaths = map[string]string{
		sessionCookie:    sessionCookiePath,
		appSessionCookie: appSessionCookiePath,
	}
	legacySessionCookiePaths = map[string]string{
		auth.LegacySessionCookie: sessionCookiePath,
		"app_session":            appSessionCookiePath,
	}
)

// setSessionCookies issues the path-scoped session cookie pair. It is both the
// sign-in path and the renewal path — a renewed session is the same session
// under the same names, so there is nothing here for it to do differently.
//
// It also expires the pre-prefix pair, which is what keeps the migration
// window short: signing in again is the common event, and after it a browser
// holds exactly one session under exactly one set of names. Without this a
// browser would carry both for up to a full lifetime, and `auth.SessionToken`'s
// preference for the current name would be doing real work rather than
// standing by.
func (s *Server) setSessionCookies(w http.ResponseWriter, sess auth.Session) {
	maxAge := cookieMaxAge(sess)
	for name, path := range sessionCookiePaths {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    sess.Token,
			Path:     path,
			HttpOnly: true,
			Secure:   s.secureCookies(),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   maxAge,
		})
	}
	s.expireCookies(w, legacySessionCookiePaths)
	// The browser's copy on a two-host install. The plain pair above is still
	// set beside it, for clients that are not browsers (SessionTokenFor).
	if s.hostSessionCookie() {
		http.SetCookie(w, &http.Cookie{
			Name:     auth.HostSessionCookie,
			Value:    sess.Token,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   maxAge,
		})
	}
}

// cookieMaxAge is the token's own remaining life, and that relationship is the
// point of it being a function.
//
// These were two constants that happened to agree — `tokenExpiry` in
// `internal/auth` and `sessionTTL` here, both eight hours, with nothing in code
// tying them together. A session policy applied to one and not the other is a
// browser holding a cookie the server refuses, or discarding one it would have
// accepted; the honest answer to "how long should the browser keep this" is
// "exactly as long as it is worth anything".
//
// Floored at one second: `MaxAge: 0` means *no* Max-Age attribute, which Go
// would render as a cookie that outlives the browser session rather than one
// that has already lapsed.
func cookieMaxAge(sess auth.Session) int {
	seconds := int(time.Until(sess.ExpiresAt).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

// setAppSessionCookie issues the apps origin's single host-only cookie — the
// handshake's last step, and the apps gateway's renewal.
//
// Beside setSessionCookies rather than in app_handshake.go because the two are
// the same decision made for two origins, and a difference between them is the
// kind of thing that stays invisible until an idle timeout applies on one host
// and not the other.
//
// **On an app's own hostname it is a different cookie**: `Path=/`, because
// `/apps` exists to keep the gateway's cookie off platform paths on a shared
// host and an app host has none, and `__Host-` whenever cookies are secure, so
// no sibling host can plant one (appHostCookie).
func (s *Server) setAppSessionCookie(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name, path := s.appCookie(r, appSessionCookie, appSessionCookiePath)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    sess.Token,
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   cookieMaxAge(sess),
	})
}

// appsSeenCookie is the marker the two-host handshake leaves on the apps
// origin once it has run for this browser, signed in or not. A public app
// there cannot see the platform's cookie, so without a hop through the
// platform origin a signed-in colleague is anonymous to every public app; the
// marker says the hop has run recently enough not to ask again. Under the
// reserved prefix, so the proxy strips it from what an app receives.
const appsSeenCookie = auth.CookiePrefix + "apps_seen"

// The marker's life depends on what the hop found (BB-69). It used to be 400
// days whatever the answer, and the marker suppresses the next hop — so a
// browser that once opened a public app before signing in, and a browser
// whose app session had since expired, were anonymous to every public app
// until they happened to open a private one.
//
//   - "Nobody is signed in" is true until someone signs in, which is a matter
//     of minutes: appsSeenAnonymousMaxAge, so a genuinely anonymous visitor
//     pays the two redirects at most once a minute and a colleague who signs
//     in is recognised within one.
//   - A real grant lasts exactly as long as the app session it minted
//     (cookieMaxAge), so the hop runs again once that session is gone.
const appsSeenAnonymousMaxAge = 60

func (s *Server) setAppsSeenCookie(w http.ResponseWriter, r *http.Request, maxAge int) {
	name, path := s.appCookie(r, appsSeenCookie, appSessionCookiePath)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "1",
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

func (s *Server) hasAppsSeenCookie(r *http.Request) bool {
	name, _ := s.appCookie(r, appsSeenCookie, appSessionCookiePath)
	c, err := r.Cookie(name)
	return err == nil && c.Value != ""
}

// visitorCookie is the anonymous audience's identifier (ADR § The anonymous
// audience): 16 random bytes, issued on an anonymous page load while
// audience_measurement_enabled is on, and counted into a sketch when it comes
// back. Install-wide across apps, because it takes appCookie's name and path
// like every gateway cookie; a different jar on an app with a host of its own.
//
// Under the reserved prefix, so stripPlatformCookies removes it before a
// request reaches an app: a stable per-browser id handed to code the platform
// does not control would be a tracking capability, and that is the privacy
// argument, tested rather than assumed (proxy_visitor_test.go).
//
// **Never refreshed.** A presented cookie is not reissued, because the
// audience-measurement exemption some regulators grant forbids extending the
// lifetime automatically: thirteen months from issue, then a new id.
const visitorCookie = auth.CookiePrefix + "visitor"

// visitorMaxAge is thirteen months, in seconds.
const visitorMaxAge = 395 * 24 * 60 * 60

// visitorIDBytes is the identifier's entropy before encoding.
const visitorIDBytes = 16

// visitorID is the request's visitor id, and whether a cookie by that name was
// presented at all (valid or not, which decides whether an off switch has
// anything to expire). A value that is not 16 bytes of base64url is malformed
// and reads as absent: it is reissued, never counted.
func (s *Server) visitorID(r *http.Request) (id string, presented bool) {
	name, _ := s.appCookie(r, visitorCookie, appSessionCookiePath)
	c, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != visitorIDBytes {
		return "", true
	}
	return c.Value, true
}

// setVisitorCookie issues a fresh id on this response.
func (s *Server) setVisitorCookie(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, visitorIDBytes)
	if _, err := rand.Read(b); err != nil {
		return // no id is a browser not counted, never a failed request
	}
	name, path := s.appCookie(r, visitorCookie, appSessionCookiePath)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    base64.RawURLEncoding.EncodeToString(b),
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   visitorMaxAge,
	})
}

// expireVisitorCookie answers a presented cookie with Max-Age=0: the switch is
// off, and off stops issuing and counting at once and leaves nothing behind in
// a browser that comes back.
func (s *Server) expireVisitorCookie(w http.ResponseWriter, r *http.Request) {
	name, path := s.appCookie(r, visitorCookie, appSessionCookiePath)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// appCookie is the name and path a gateway cookie has on the origin this
// request is on: as given on the apps origin, and on an app's own hostname at
// Path=/ under `__Host-` when cookies are secure. The prefix makes the browser
// refuse a copy set by any other host or with any Domain, which is what lets
// one name stand for one origin's session.
func (s *Server) appCookie(r *http.Request, name, path string) (string, string) {
	if r == nil || !onAppHost(r) {
		return name, path
	}
	if s.secureCookies() {
		return auth.HostCookiePrefix + name, "/"
	}
	return name, "/"
}

// setGatewaySession is how the apps gateway renews, and the one place the
// two-origin split changes the answer.
//
// Two hosts: only the /apps cookie, because the platform pair belongs to
// another origin and writing it from here would be both futile and wrong.
//
// One host: the whole pair. The platform cookie is scoped `Path=/api/v1`, so a
// browser never sends it to `/apps/{slug}` — nothing on this path would keep it
// alive, and somebody who spent an hour inside an app would come back to the
// dashboard signed out. Setting both is what makes "time in an app is time the
// person was present" true on the install where both live on one origin.
//
// An app host is the first case whatever the install's shape: it is never the
// platform's host, so only its own cookie means anything there.
func (s *Server) setGatewaySession(r *http.Request) func(http.ResponseWriter, auth.Session) {
	return func(w http.ResponseWriter, sess auth.Session) {
		if s.cfg.TwoHostMode() || onAppHost(r) {
			s.setAppSessionCookie(w, r, sess)
			return
		}
		s.setSessionCookies(w, sess)
	}
}

// clearSessionCookies expires both session cookies — and the legacy pair with
// them. Clearing only the current names would leave a browser upgraded
// mid-session still holding a valid legacy cookie, so a sign-out would not sign
// anybody out: the token-version bump is what actually revokes, but the cookie
// would still name a session and the UI would still look signed in.
func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	s.expireCookies(w, sessionCookiePaths)
	s.expireCookies(w, legacySessionCookiePaths)
	if s.hostSessionCookie() {
		s.expireCookies(w, map[string]string{auth.HostSessionCookie: "/"})
	}
}

func (s *Server) expireCookies(w http.ResponseWriter, namePaths map[string]string) {
	for name, path := range namePaths {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     path,
			HttpOnly: true,
			Secure:   s.secureCookies(),
			MaxAge:   -1,
		})
	}
}

// appSessionToken returns the JWT the apps gateway should authorize against:
// the /apps-scoped cookie, then the platform cookie, for single-port
// deployments where the browser may attach it too.
//
// The pre-prefix names (`app_session`, `session`) are no longer read. Their
// window closed long ago (ADR §47), and on this origin they were worse than
// stale: both are names an app page can set from script — `session` is Flask's
// own — so reading them let a deployed app plant a JWT the gateway would then
// authorize every other app against.
//
// **Two copies of the gateway cookie is no session**, as two of the platform's
// is (auth.SessionTokenFor). A page on the apps origin can set
// `toft_app_session` from script at a longer path — `Path=/apps/b` is a
// different cookie from the HttpOnly one at `/apps`, so nothing blocks it — and
// a browser sends the longer path first. Taking the first copy let app A choose
// whose session app B's viewers carried; taking neither costs the victim their
// session on that app, not their identity. Nor does it fall back to the platform cookie:
// a planted copy must not decide whose request this is, either way.
//
// On an app host the name is that host's (appCookie), by the same two-copies
// rule, and there is no platform cookie to fall back to: the host is never the
// platform's, so a session there is the handshake's or nothing.
func (s *Server) appSessionToken(r *http.Request) string {
	name, _ := s.appCookie(r, appSessionCookie, appSessionCookiePath)
	var v string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			v = c.Value
			n++
		}
	}
	switch {
	case n == 1:
		return v
	case n > 1:
		return ""
	}
	if onAppHost(r) {
		return ""
	}
	return auth.SessionToken(r)
}

// stripPlatformCookies removes Toft's own cookies from an outbound proxied
// request, leaving any cookies the deployed app set for itself intact.
//
// Only the first `Cookie:` header line is rewritten, and one is enough: a
// browser sends exactly one, and a client that sends two is not a browser
// carrying a session it was given — the platform cookies are set HttpOnly on
// the platform's own paths, so a second line cannot carry a JWT to an app the
// browser did not already choose to visit with the first.
func stripPlatformCookies(req *http.Request) {
	raw := req.Header.Get("Cookie")
	if raw == "" {
		return
	}

	kept := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ";") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		name := trimmed
		if eq := strings.IndexByte(trimmed, '='); eq >= 0 {
			name = trimmed[:eq]
		}
		if isPlatformCookie(name) {
			continue
		}
		kept = append(kept, trimmed)
	}

	if len(kept) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header.Set("Cookie", strings.Join(kept, "; "))
}

// isPlatformCookie is the prefix rule, plus the legacy names for as long as a
// browser may still hold one. A name test rather than a list membership test is
// what keeps a cookie added later from having to remember to join a list — the
// failure mode there is a platform credential handed to an app, which is the
// one direction this must never fail in.
func isPlatformCookie(name string) bool {
	// The prefix after a browser's own `__Host-`/`__Secure-`, which is how
	// auth.HostSessionCookie is spelled and how an app could try to spell one.
	name = strings.TrimPrefix(strings.TrimPrefix(name, "__Host-"), "__Secure-")
	if strings.HasPrefix(name, auth.CookiePrefix) {
		return true
	}
	for _, n := range legacyPlatformCookieNames {
		if name == n {
			return true
		}
	}
	return false
}

// keepOwnSession re-issues the caller's own session after a change that bumped
// their token_version from inside this very request — adding themselves to a
// group, removing themselves from one, deleting a group they are in.
//
// The bump is right: membership is authorization, and every session carrying
// the old groups must die (invariants § Identity and access). But the session
// that *made* the change is the one place nothing is stale — the person has
// just acted, the change is theirs — and revoking it signs an administrator out
// mid-click for having added themselves to a team. So this one session is
// carried across: same `jti`, same `auth_time` and therefore the same absolute
// deadline (Renew), with the groups, role and token_version read afresh. Every
// other session the person holds still dies on its next request.
//
// The version is read before the row, so a demotion racing in between can only
// leave the re-issued session one version behind — dead, which is the safe
// direction — never carrying a role the row no longer holds. Nothing for a
// request authenticated by an API key, which token_version does not revoke, or
// by anything but the caller's own browser session.
func (s *Server) keepOwnSession(w http.ResponseWriter, r *http.Request, userID string) {
	claims := auth.GetClaims(r.Context())
	if claims == nil || claims.UserID != userID {
		return
	}
	version, err := s.db.GetTokenVersion(userID)
	if err != nil {
		return
	}
	user, err := s.db.GetUser(userID)
	if err != nil || user == nil || user.LockedAt != nil {
		return
	}
	groups, err := s.db.GetUserGroupIDs(userID)
	if err != nil {
		return
	}
	next := *claims
	next.SystemRole = user.SystemRole
	next.Groups = groups
	next.TokenVersion = version
	sess, err := s.jwt.Renew(&next, s.sessionPolicy())
	if err != nil {
		return
	}
	s.setSessionCookies(w, sess)
}

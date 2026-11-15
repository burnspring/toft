package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"toft/internal/auth"
)

// The two-host handshake: giving the apps origin a session of its own.
//
// Read internal/auth/appgrant.go first — it explains why this exists and why the
// alternative (a Domain cookie spanning both hosts) is refused.
//
// The flow, and every hop is a browser redirect because a cookie can only be set
// by the origin it belongs to:
//
//	1. GET https://apps.example.com/apps/reports/     no app_session on this host
//	2. -> 302 https://lp.example.com/api/v1/auth/app-grant?redirect=<1>
//	3. platform reads ITS session (the cookie is on this host); signs in if not
//	4. -> 302 https://apps.example.com/__toft/session?grant=<blob>&redirect=<1>
//	5. apps origin verifies the grant, sets app_session for ITSELF
//	6. -> 302 back to <1>, now with a cookie
//
// Three redirects, once per session per origin. Every later request goes
// straight in.
//
// Step 1 also leaves a random nonce in a host-only cookie on the redeeming
// origin and passes it along; step 3 signs it into the grant and step 5
// compares the two. That is what ties a grant to the browser that asked for
// it: without it, anybody signed in could mint a grant for themselves and
// navigate someone else's browser to the redeem URL, which would replace that
// browser's app session with theirs.

// appHandshakePath is reserved on the apps origin.
//
// It has to be somewhere an app can never serve, because the apps origin is
// otherwise entirely the app's. The double underscore is deliberately ugly: this
// is a path nobody types and no framework routes by default.
const appHandshakePath = "/__toft/session"

// appsOrigin is the one string both halves of the handshake agree on.
//
// Deliberately the CONFIGURED value rather than the per-request one, which is
// the opposite of platformBaseFor and worth saying why. platformBaseFor exists
// so a sign-in through a tunnel returns to the origin holding the new cookie —
// it adapts to the browser. This must not adapt: the grant is minted on one
// listener and verified on another, and if the two derive the origin
// differently — a proxy presenting a name the config does not use — every grant
// fails verification and the gateway redirects forever. A loop is a worse
// failure than a refusal, and configuration is the thing both listeners share.
//
// So an install behind a proxy that renames the apps host must say so in
// APPS_BASE_URL. That is the same rule as BASE_URL and it is already how the
// origin allowlist behaves.
func (s *Server) appsOrigin() string {
	return normalizeOrigin(s.cfg.AppsBaseURL)
}

// handleAppGrant runs on the PLATFORM origin, where the viewer's session cookie
// lives, and hands out a grant redeemable at the apps origin.
//
// It is deliberately not an API endpoint: it answers browsers with redirects,
// never JSON, because the only caller is a navigation the gateway started.
func (s *Server) handleAppGrant(w http.ResponseWriter, r *http.Request) {
	// Where the viewer was going. Validated against the origin allowlist, which
	// already admits the apps origin — an unvalidated value here would make this
	// an open redirect with a credential attached.
	target := s.safeRedirect(r, r.URL.Query().Get("redirect"))
	if target == "" {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}

	// Which origin the grant is for, and where it is redeemed: an app's own
	// hostname when the target is on one, the shared apps origin otherwise.
	// The signature binds the origin, so a grant for one app host is dead at
	// every other and at the apps origin.
	origin := s.grantOriginFor(target)

	// The starting browser's nonce, signed into the grant below. A request
	// without one was not started by the gateway, and a grant cut for it
	// could never be redeemed.
	nonce := r.URL.Query().Get("nonce")
	if !auth.ValidAppGrantNonce(nonce) {
		http.Error(w, "invalid app session request", http.StatusBadRequest)
		return
	}

	p := auth.GetPrincipal(r.Context())
	if p == nil || p.ID == "" {
		// A public app asked whether this browser has a platform session, and
		// it does not. Back to the apps origin with the answer — no grant,
		// just the marker — rather than to a login page for an app that
		// needs none.
		if r.URL.Query().Get("public") == "1" {
			back := origin + appHandshakePath +
				"?anonymous=1&redirect=" + url.QueryEscape(target)
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		// Not signed in yet. Send them through the normal login and back to this
		// same endpoint, so the handshake resumes rather than dropping them on a
		// dashboard with no idea what they were trying to open.
		here := s.platformBaseFor(r) + r.URL.Path + "?redirect=" + url.QueryEscape(target) +
			"&nonce=" + url.QueryEscape(nonce)
		http.Redirect(w, r, s.platformBaseFor(r)+"/login?redirect="+url.QueryEscape(here), http.StatusFound)
		return
	}

	// The token version goes INTO the grant so the redeemer can tell that the
	// session it was cut from is still the one in force. Reading it here rather
	// than trusting the claim keeps a stale JWT from minting a fresh session.
	tokenVersion, err := s.db.GetTokenVersion(p.ID)
	if err != nil {
		http.Error(w, "could not issue an app session", http.StatusInternalServerError)
		return
	}

	// The platform session's own start goes into the grant, so the session
	// minted on the other origin inherits the sign-in's clock rather than
	// starting a new one. A principal with no claims — there is none on this
	// route, which is cookie-only — would fail closed at the redeem.
	var sessionStart time.Time
	if claims := auth.GetClaims(r.Context()); claims != nil {
		sessionStart = claims.SessionStart()
	}
	grant := s.jwt.MintAppGrant(p.ID, origin, tokenVersion, sessionStart, nonce, time.Now().UTC())

	redeem := origin + appHandshakePath +
		"?grant=" + url.QueryEscape(grant) + "&redirect=" + url.QueryEscape(target)
	// The grant is about to travel in a URL. Nothing downstream should learn it
	// from a Referer header.
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, redeem, http.StatusFound)
}

// handleAppSessionRedeem runs on the APPS origin and is the only thing there
// that is not an app.
func (s *Server) handleAppSessionRedeem(w http.ResponseWriter, r *http.Request) {
	// Set before any exit: an error page reached with a grant in the URL must
	// not leak it onwards either.
	w.Header().Set("Referrer-Policy", "no-referrer")

	// No handshake runs on a one-host install's shared origin — the platform
	// cookie reaches the gateway by itself — so nothing here may set a cookie
	// there either: a forged navigation would only overwrite the gateway's
	// session with a token it refuses.
	if !s.cfg.TwoHostMode() && !onAppHost(r) {
		http.NotFound(w, r)
		return
	}

	target := s.safeRedirect(r, r.URL.Query().Get("redirect"))
	if target == "" {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}

	// The platform answered "nobody is signed in" for a public app. The
	// marker is set so this browser is not asked again, and the visitor goes
	// on as the anonymous person they are. Nothing is verified because
	// nothing is minted: the worst a forged arrival here can do is set the
	// marker on its own browser.
	if r.URL.Query().Get("anonymous") == "1" {
		// Briefly: the answer stops being true the moment they sign in.
		s.setAppsSeenCookie(w, r, appsSeenAnonymousMaxAge)
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	grant, err := s.jwt.VerifyAppGrant(r.URL.Query().Get("grant"), normalizeOrigin(s.gatewayOrigin(r)), time.Now().UTC())
	if err != nil {
		// An expired grant is the one failure a legitimate browser hits — a slow
		// hop, a laptop asleep between redirects — so it restarts the handshake
		// instead of dead-ending. Anything else is refused outright rather than
		// bounced, because a redirect loop is what a forged grant would produce.
		if err == auth.ErrAppGrantExpired {
			http.Redirect(w, r, s.startHandshake(w, r, target), http.StatusFound)
			return
		}
		http.Error(w, "invalid app session grant", http.StatusForbidden)
		return
	}

	// The browser presenting the grant must be the one the handshake started
	// in. A mismatch is somebody else's grant navigated into this browser.
	held := s.grantNonceCookie(r)
	if held == "" || subtle.ConstantTimeCompare([]byte(held), []byte(grant.Nonce)) != 1 {
		http.Error(w, "invalid app session grant", http.StatusForbidden)
		return
	}

	// Both halves of verification, exactly as the act-for path does: the
	// signature proved what was minted, and this proves it is still true. A
	// sign-out, demotion, group change or lock between mint and redeem lands
	// here.
	user, err := s.db.GetUser(grant.UserID)
	if err != nil || user == nil || user.LockedAt != nil {
		http.Error(w, "invalid app session grant", http.StatusForbidden)
		return
	}
	current, err := s.db.GetTokenVersion(grant.UserID)
	if err != nil || current != grant.TokenVersion {
		http.Error(w, "invalid app session grant", http.StatusForbidden)
		return
	}

	// Not a login: nothing was proved here that the platform session had not
	// already proved, so the clock is the grant's and last_login_at is not
	// stamped. A platform session past its absolute ceiling never reaches
	// this point — the grant route refused it — and a grant that somehow
	// carries a start past the ceiling mints a session already expired.
	// For this origin only (auth.AppsAudience): the grant crossed it in a URL
	// an app's own code can see, so what it buys must not be a platform
	// session, nor a session at any other app host.
	sess, err := s.mintSessionFor(user, grant.SessionStart, auth.AppsAudience(normalizeOrigin(s.gatewayOrigin(r))))
	if err != nil {
		http.Error(w, "could not issue an app session", http.StatusInternalServerError)
		return
	}

	// ONE cookie, host-only, on this origin. Not setSessionCookies: that pair
	// belongs to the platform origin, and the platform-API cookie has no
	// business existing here — the apps origin never serves /api/v1, and a
	// session cookie that is never sent cannot be stolen.
	//
	// Same policy either way, though — the token was minted under it and the
	// MaxAge is read off that token. A separate, longer floor for this origin
	// was considered and refused: one policy is what an assessor can be shown.
	s.setAppSessionCookie(w, r, sess)
	// And the marker: the handshake has run for this browser, whichever door
	// brought it here — for as long as the session it minted, and no longer,
	// so an expired app session is asked for again (BB-69).
	s.setAppsSeenCookie(w, r, cookieMaxAge(sess))
	// The nonce has done its work.
	nonceName, noncePath := s.appCookie(r, appGrantNonceCookie, appHandshakePath)
	s.expireCookies(w, map[string]string{nonceName: noncePath})

	// Straight back out, which is also what retires the grant-bearing URL from
	// the address bar and from history's most recent entry.
	http.Redirect(w, r, target, http.StatusFound)
}

// appGrantNonceCookie holds the nonce that ties a handshake to the browser
// that started it. Under the reserved prefix, so the proxy strips it from what
// an app receives; at the redeem path on the shared apps origin, at `/` under
// `__Host-` on an app host (appCookie).
const appGrantNonceCookie = auth.CookiePrefix + "grant_nonce"

// appGrantNonceMaxAge outlasts a sign-in detour: the handshake may stop at
// the platform's login page before the grant is minted.
const appGrantNonceMaxAge = 15 * 60

// startHandshake leaves a fresh nonce in this browser and returns the
// platform-origin URL that starts a handshake for target, carrying it.
func (s *Server) startHandshake(w http.ResponseWriter, r *http.Request, target string) string {
	var b [24]byte
	rand.Read(b[:])
	nonce := base64.RawURLEncoding.EncodeToString(b[:])
	name, path := s.appCookie(r, appGrantNonceCookie, appHandshakePath)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    nonce,
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   appGrantNonceMaxAge,
	})
	return strings.TrimSuffix(s.cfg.BaseURL, "/") +
		"/api/v1/auth/app-grant?redirect=" + url.QueryEscape(target) + "&nonce=" + nonce
}

// grantNonceCookie is the nonce this browser holds, by appSessionToken's rule:
// two copies of the name — an app page on the shared origin can plant one at
// a longer path — count as none.
func (s *Server) grantNonceCookie(r *http.Request) string {
	name, _ := s.appCookie(r, appGrantNonceCookie, appHandshakePath)
	var v string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			v = c.Value
			n++
		}
	}
	if n != 1 {
		return ""
	}
	return v
}

// grantOriginFor is the origin a handshake for target runs on: the canonical
// origin of the app host target is on, or the apps origin. Only an app host
// that is served now counts — safeRedirect has already refused any other.
func (s *Server) grantOriginFor(target string) string {
	if o := normalizeOrigin(target); o != "" && s.appHostOrigins()[o] {
		return o
	}
	return s.appsOrigin()
}

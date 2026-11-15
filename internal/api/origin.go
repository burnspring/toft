package api

import (
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/hoststore"
)

// originCheck rejects API requests that carry a browser Origin belonging to
// anything other than the platform itself.
//
// Cookies are scoped by site, not by port, so a page served from the apps
// gateway can still cause the browser to attach the session cookie to a request
// aimed at the platform API. The same-origin policy stops such a page reading
// the response, but not from issuing the request — this is what stops the write.
//
// Requests with no Origin header (top-level navigations, curl, server-to-server
// calls) are left alone: browsers always send Origin on the cross-origin fetches
// this defends against, and page script cannot suppress it.
//
// `Origin: null` is not "no Origin". It is what a browser sends when it will not
// say where a request came from — a page served with `Referrer-Policy:
// no-referrer`, a sandboxed frame, a redirect across origins — and an app page
// can arrange any of those. The apps gateway is same-site with the platform, so
// the Lax session cookie rides a top-level form POST from it; admitting `null`
// let a deployed app file a promotion request as one viewer and approve it as
// the next administrator to open the page. So on a state-changing method `null`
// is refused like any foreign origin. Nothing the platform serves posts a form,
// so nothing legitimate arrives that way.
//
// Two further refusals stand behind that, for a request carrying the session
// cookie and a state-changing method. Where Origin is absent but the browser
// sent `Sec-Fetch-Site`, a value of `cross-site` or `same-site` is the same
// forgery with the Origin header lost on the way. And a cookie request whose
// Content-Type is one an HTML form can produce without a preflight —
// `text/plain` or `application/x-www-form-urlencoded` — is refused outright,
// because the SPA never sends either. Multipart stays allowed: it is how the SPA
// uploads, and it carries an Origin like everything else.
//
// The host data plane (`/store-data/`) is exempt from that backstop, and only
// from it: the store browser PUTs a File by XHR with no Content-Type of its
// own, so a `.txt` arrives as `text/plain` carrying the `Path=/` cookie. The
// data plane reads neither cookie nor Authorization and accepts only the
// signed token's own method, which a form (GET or POST) cannot spend, so the
// exemption opens nothing; the Origin allow-list below still applies (security
// review 2026-11-15, HD-3).
//
// The allowlist is rebuilt per request rather than captured once at mount
// time, because one of its entries — the origin a trusted proxy says the
// browser used — is a fact about the request rather than about the process.
func (s *Server) originCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !onHostDataPlane(r) && refuseBrowserForm(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" || (origin == "null" && safeMethod(r.Method)) {
			next.ServeHTTP(w, r)
			return
		}
		if origin == "null" {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}

		if !s.allowedOrigins(r)[normalizeOrigin(origin)] {
			s.rejectOrigin(w, r, origin)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rootOriginCheck is the origin check the platform router installs at its
// root. On a two-listener install it is originCheck, whole. On a single-origin
// install (APPS_PORT equal to PORT) the same router also serves the deployed
// apps inline under /apps/{slug}, and those requests take the apps gateway's
// rule instead — appsOriginCheck, which admits an app's own `Origin: null`
// form under `Referrer-Policy: no-referrer` and has no form backstop. Held to
// the platform's rule, an app form post that works behind the gateway got a
// bare 403 on the other install shape (security review 2026-11-14, RO-1).
//
// The check stays at the root rather than moving onto platform subtrees, so a
// route nobody remembered is never left unguarded; only the inline proxy's
// prefix is carved out, and only on both spellings of the path, as
// onCallbackSurface tests them, since chi routes on RawPath whenever it is set.
func (s *Server) rootOriginCheck(next http.Handler) http.Handler {
	platform := s.originCheck(next)
	if s.cfg.SeparateAppsListener() {
		return platform
	}
	apps := s.appsOriginCheck(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onInlineAppPath(r) {
			apps.ServeHTTP(w, r)
			return
		}
		platform.ServeHTTP(w, r)
	})
}

// onInlineAppPath is whether a request is under /apps/ in both its decoded
// path and, when set, its raw one.
func onInlineAppPath(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/apps/") {
		return false
	}
	if p := r.URL.RawPath; p != "" {
		return strings.HasPrefix(p, "/apps/")
	}
	return true
}

// onHostDataPlane is whether a request is under the host store's data plane in
// both its decoded path and, when set, its raw one.
func onHostDataPlane(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, hoststore.Route) {
		return false
	}
	if p := r.URL.RawPath; p != "" {
		return strings.HasPrefix(p, hoststore.Route)
	}
	return true
}

// safeMethod reports whether a method is one that must not change state, and
// so one a forged request gains nothing by sending.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// refuseBrowserForm is originCheck's second line: the forgeries that reach
// here without a usable Origin. Only a request carrying the platform session
// cookie and no Authorization header is considered — a script or a server
// presents a key and never needs the browser's cookie, and is not what a
// forged form can be.
func refuseBrowserForm(r *http.Request) bool {
	if safeMethod(r.Method) || !carriesSessionCookie(r) || r.Header.Get("Authorization") != "" {
		return false
	}
	if r.Header.Get("Origin") == "" {
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			return true
		}
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "text/plain" || ct == "application/x-www-form-urlencoded"
}

// carriesSessionCookie is whether a request presents the platform session under
// either of its names — whether a forged form could be riding it, which is a
// question about the cookie being there rather than about which one is read.
func carriesSessionCookie(r *http.Request) bool {
	for _, c := range r.Cookies() {
		if (c.Name == auth.SessionCookie || c.Name == auth.HostSessionCookie) && c.Value != "" {
			return true
		}
	}
	return false
}

// rejectOrigin refuses a request and, when the refusal is a misconfiguration
// rather than an attack, says so.
//
// The two cases are distinguishable, and the difference is worth everything to
// whoever is reading the result. A cross-site request forgery carries an Origin
// belonging to *another* site: Origin and the address the request was sent to
// disagree. A misconfigured install carries an Origin equal to its own public
// address — the browser is on the very page this install served — and is
// rejected only because BASE_URL names some other address, or because the
// proxy is not forwarding enough for us to tell.
//
// That second case used to present as a bare 403 on the login form of a
// brand-new install: the page renders (browsers omit Origin on same-origin
// GETs), the password POST carries one, and nothing in the response or the log
// named the setting at fault. It is the single most likely way to be locked out
// of a fresh deployment, so it gets a sentence that names the cause.
func (s *Server) rejectOrigin(w http.ResponseWriter, r *http.Request, origin string) {
	if !sameHost(origin, requestHost(r)) {
		// Origin belongs to another site. This is the case the check exists
		// for; say nothing that helps.
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	log.Printf("origin check: rejected %s %s from origin %s, which is this install's own address — "+
		"BASE_URL is %s and the request %s. Set BASE_URL to the address people use to reach this "+
		"install, add it to EXTRA_ORIGINS, or have the reverse proxy forward X-Forwarded-Proto and "+
		"X-Forwarded-Host from a peer inside TRUSTED_PROXY_CIDRS.",
		r.Method, r.URL.Path, origin, s.cfg.BaseURL, proxyDiagnosis(r))

	http.Error(w, "This request came from "+origin+", which this install does not recognise as its "+
		"own address — so it was rejected as cross-origin. Nothing is wrong with your account or "+
		"password. The install is configured to answer to a different address; an administrator can "+
		"fix it with BASE_URL, or by having the reverse proxy forward X-Forwarded-Proto and "+
		"X-Forwarded-Host. See the server log for the specifics.", http.StatusForbidden)
}

// proxyDiagnosis describes, for the log line, how much this request told us
// about where it really came from — which is the difference between "set
// BASE_URL" and "fix the proxy".
func proxyDiagnosis(r *http.Request) string {
	if !viaTrustedProxy(r) {
		return "did not arrive through a peer inside TRUSTED_PROXY_CIDRS, so no X-Forwarded-* header was believed"
	}
	if r.Header.Get("X-Forwarded-Proto") == "" {
		return "arrived through a trusted proxy that did not set X-Forwarded-Proto, so its scheme had to be guessed"
	}
	return "arrived through a trusted proxy declaring " + forwardedOrigin(r)
}

// appsOriginCheck is originCheck for the apps gateway listener, which had no
// origin checking at all.
//
// The allowlist differs from the platform's, and it has to. The apps gateway
// serves the app pages themselves, and a browser sends Origin on same-origin
// requests with a non-safelisted method — so an app POSTing to itself carries
// the gateway's own origin. Installing the platform allowlist here would 403
// every such request and break the apps.
//
// What this does stop is an unrelated site driving a deployed app with the
// viewer's app_session cookie attached. It does *not* separate apps from each
// other: they share one origin, so app A's page carries the same Origin as app
// B's and passes. That is an accepted risk, reviewed on 2026-08-04 on the
// basis that apps are internally authored by trusted teams — the scope note in
// docs/dev-reference.md § Origin isolation states it and the conditions that
// would reverse it.
func (s *Server) appsOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || (origin == "null" && nullOriginAdmitted(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if origin == "null" {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}

		if !s.allowedAppsOrigins(r)[normalizeOrigin(origin)] {
			s.rejectOrigin(w, r, origin)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// appHostOriginCheck is appsOriginCheck for a request on an app's own
// hostname, and its allowlist is the narrowest on the install: that host's
// canonical origin and the platform's, and nothing else — not the apps
// origin, not another app's host, not EXTRA_ORIGINS. A page on the shared
// apps origin, or on a neighbour's host, cannot drive this app with the
// viewer's session attached; that is the isolation a hostname buys.
//
// A WebSocket upgrade carries Origin too, so the proxied socket is held to the
// same list. The apps origin's own allowlist is unchanged for the gateway.
func (s *Server) appHostOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || (origin == "null" && nullOriginAdmitted(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if origin == "null" {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		host, _ := r.Context().Value(appHostKey{}).(string)
		got := normalizeOrigin(origin)
		allowed := got != "" && (got == normalizeOrigin(s.cfg.AppHostOrigin(host)) ||
			got == normalizeOrigin(s.cfg.BaseURL))
		// The address a trusted proxy says this request was sent to, when it
		// is this host: the same request-derived entry the other two checks
		// believe, and for the same reason it adds no trust.
		if !allowed {
			if fo := forwardedOrigin(r); fo != "" && fo == got {
				if u, err := url.Parse(fo); err == nil && strings.EqualFold(u.Hostname(), host) {
					allowed = true
				}
			}
		}
		if !allowed {
			s.rejectOrigin(w, r, origin)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// nullOriginAdmitted is whether an app-side request with `Origin: null` may
// pass. On a safe method it may. On a state-changing one only when the
// browser says the request is its own page's (`Sec-Fetch-Site: same-origin`),
// or says nothing — which is not a browser, and so not a forgery riding the
// viewer's cookie. A sandboxed iframe on a neighbour's host or any other site
// sends `null` with `same-site` or `cross-site`, and admitting `null` whole let
// it drive an app with the viewer's session (security review 2026-11-10, D-3).
// The app's own form under `Referrer-Policy: no-referrer` also sends `null`,
// and with `same-origin`, which is why this is not originCheck's flat refusal.
//
// A WebSocket upgrade counts as a write although it is a GET: a socket has no
// same-origin protection on what it returns, so the Origin check is all that
// stands between a sandboxed frame and a socket carrying the viewer's session.
// WebKit sends a same-site frame's Lax cookie with such a handshake (security
// review 2026-11-15, HD-1). A null-origin upgrade carrying any session cookie
// is refused outright — the half that holds where a browser sends no
// Sec-Fetch-Site on a handshake — and one without is held to the same
// Sec-Fetch-Site rule as a POST.
func nullOriginAdmitted(r *http.Request) bool {
	upgrade := isUpgrade(r.Header)
	if safeMethod(r.Method) && !upgrade {
		return true
	}
	if upgrade && carriesAnySessionCookie(r) {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

// carriesAnySessionCookie is whether the request holds a non-empty platform or
// app session cookie under any of their names.
func carriesAnySessionCookie(r *http.Request) bool {
	for _, c := range r.Cookies() {
		if c.Value == "" {
			continue
		}
		switch c.Name {
		case appSessionCookie, auth.HostCookiePrefix + appSessionCookie, auth.SessionCookie, auth.HostSessionCookie:
			return true
		}
	}
	return false
}

// allowedAppsOrigins is the apps gateway's own origin, plus the platform's so
// that a link or form on the platform UI can still reach an app.
func (s *Server) allowedAppsOrigins(r *http.Request) map[string]bool {
	origins := map[string]bool{
		normalizeOrigin(s.cfg.AppsBaseURL): true,
		normalizeOrigin(s.cfg.BaseURL):     true,
	}
	if o := forwardedOrigin(r); o != "" {
		origins[o] = true
	}
	for _, o := range s.cfg.ExtraOrigins {
		origins[o] = true
	}
	if s.cfg.IsLocalDevelopment() || s.cfg.DevPassword != "" {
		origins[normalizeOrigin("http://localhost:5173")] = true
		origins[normalizeOrigin("http://127.0.0.1:5173")] = true
	}
	delete(origins, "")
	return origins
}

// allowedOrigins is the platform's own origin, anything an operator added
// through EXTRA_ORIGINS, and the Vite dev server. The apps gateway is
// deliberately absent — that is the origin being defended against, and
// EXTRA_ORIGINS must never be used to put it back.
func (s *Server) allowedOrigins(r *http.Request) map[string]bool {
	origins := map[string]bool{}
	for _, o := range s.trustedPlatformOrigins(r) {
		origins[o] = true
	}
	delete(origins, "")
	return origins
}

// trustedPlatformOrigins is the same set as an ordered list, most authoritative
// first: the origin a trusted proxy says this request arrived at, then
// BASE_URL, then the operator's EXTRA_ORIGINS, then the Vite dev server.
//
// Order matters to callers that have to *choose* one rather than test
// membership — redirectURIFor picks the first whose host matches the request —
// so this is a slice and allowedOrigins is derived from it, rather than the two
// drifting as separate lists of the same thing.
//
// The forwarded origin leads because it is the only entry derived from the
// request in hand rather than from configuration, and it is what makes an
// install behind a reverse proxy work without being told its own address.
// Believing it introduces no new trust: it is admitted only when the socket
// peer was inside TRUSTED_PROXY_CIDRS, and it can only ever equal the address
// the request was *sent to*. A cross-site page fetching this API sends its own
// Origin while the request still travels to us — so the two disagree and the
// check still bites. Making them agree would require the attacker's page to be
// served from this install's own hostname, which is not forgery, and a browser
// would not attach our cookies to any other one.
func (s *Server) trustedPlatformOrigins(r *http.Request) []string {
	out := make([]string, 0, 4+len(s.cfg.ExtraOrigins))
	if o := forwardedOrigin(r); o != "" {
		out = append(out, o)
	}
	out = append(out, normalizeOrigin(s.cfg.BaseURL))
	out = append(out, s.cfg.ExtraOrigins...)
	if s.cfg.IsLocalDevelopment() || s.cfg.DevPassword != "" {
		out = append(out, "http://localhost:5173", "http://127.0.0.1:5173")
	}
	return out
}

// forwardedOrigin is the public origin a trusted proxy says this request
// arrived at: X-Forwarded-Proto and X-Forwarded-Host, falling back to the
// Host header a proxy passes through unchanged.
//
// Empty for a request that did not come through a trusted proxy, which is the
// whole safety property — an arbitrary client cannot nominate its own origin,
// because nothing it sends is read here unless the socket peer vouched for it.
//
// Also empty when nothing was actually forwarded. Trusting loopback by default
// means the peer test alone cannot tell Caddy on this box from curl on this
// box, or from an SSH tunnel — and a direct local request that nominated
// itself as the install's public address would be believed, shown to an
// administrator as the address they had reached, and offered as the one to put
// in everybody's email. So the declaration has to exist, not merely be
// permitted: a proxy that is forwarding at all sets one of these two.
func forwardedOrigin(r *http.Request) string {
	if r == nil || !viaTrustedProxy(r) {
		return ""
	}

	declaredHost := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if declaredHost == "" && r.Header.Get("X-Forwarded-Proto") == "" {
		return ""
	}

	host := declaredHost
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}

	scheme := strings.ToLower(firstHeaderValue(r.Header.Get("X-Forwarded-Proto")))
	if scheme != "http" && scheme != "https" {
		// No usable declaration. r.TLS is set only when this process
		// terminated TLS itself, which behind a proxy it did not — so the
		// honest answer for an unadorned proxy is http, and an operator who
		// terminates TLS upstream has to say so.
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}

	return normalizeOrigin(scheme + "://" + host)
}

// publicObservedOrigin is forwardedOrigin narrowed to addresses worth showing
// an administrator as "where you reached this install" — which is to say, ones
// that could sensibly appear in an email to somebody else.
//
// A loopback address never can. It is right for the inbound checks, which is
// why forwardedOrigin still returns it: a request through an SSH tunnel or the
// Vite dev server has to pass the origin check. It is wrong as a suggestion,
// because a link to localhost works only for the person who is already on the
// machine, and this is a mistake that has already been made once — the dev
// server preserves the browser's Host, so the settings page cheerfully offered
// `http://localhost:5173` as the address to put in everybody's mail, and it was
// taken. Two independent guards now stand between that and a stored value:
// nothing is observed without an actual forwarded declaration, and nothing
// loopback is ever offered.
func publicObservedOrigin(r *http.Request) string {
	origin := forwardedOrigin(r)
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || config.IsLoopbackHost(u.Hostname()) {
		return ""
	}
	return origin
}

// firstHeaderValue takes the leftmost entry of a comma-separated forwarded
// header. Proxies append, so the leftmost is the value the outermost hop saw —
// the one the browser actually used.
func firstHeaderValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// requestHost is the public host this request was addressed to, as best we can
// tell: what a trusted proxy declared, else the Host header.
func requestHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); h != "" && viaTrustedProxy(r) {
		return h
	}
	return r.Host
}

// sameHost reports whether an Origin names the same host as the address the
// request was sent to — the test that separates a misconfigured install from a
// genuine cross-site request. Compared on host and port, ignoring scheme: an
// install reached over https whose BASE_URL says http is misconfigured, not
// under attack.
func sameHost(origin, host string) bool {
	if host == "" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

// safeRedirect validates a post-login redirect target, returning "" when it is
// not one Toft is willing to send a user to.
//
// The `redirect` query parameter was previously stored and used verbatim, so
// /api/v1/auth/github/login?redirect=https://evil.example/ sent the user
// off-site *after a genuine, successful GitHub login* — a high-credibility
// phishing primitive. originCheck does not apply: a top-level navigation
// carries no Origin.
//
// Two forms are accepted: a site-relative path, and an absolute URL on an
// origin already trusted for browser traffic — the platform's own, the Vite
// dev server on :5173, and the apps gateway (so signing in from a protected
// app's login redirect can land the user back on that app).
func (s *Server) safeRedirect(r *http.Request, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// A control character can split the Location header, and a tab or newline
	// inside the value is stripped by browsers' URL parsers, which turns
	// "/<TAB>/host" into "//host".
	for _, c := range raw {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}

	if strings.HasPrefix(raw, "/") {
		// "//evil.example" is a scheme-relative URL — a different site, not a
		// path. So is "/\evil.example", which some browsers normalise to "//".
		if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
			return ""
		}
		return raw
	}

	target := normalizeOrigin(raw)
	if target == "" {
		return ""
	}
	if s.allowedOrigins(r)[target] || target == normalizeOrigin(s.cfg.AppsBaseURL) {
		return raw
	}
	// An app's own hostname is somewhere a viewer may be sent back to after
	// signing in — and nothing more. It is never merged into allowedOrigins:
	// that set is who may write to /api/v1 as the viewer, and an app host in
	// it would let every page of that app act on the platform.
	if s.appHostOrigins()[target] {
		return raw
	}
	return ""
}

// appHostOrigins is the canonical origin of every app host served now. A
// read, so it is consulted only on the sign-in paths and never on the data
// path; a set of its own, so it cannot leak into allowedOrigins.
func (s *Server) appHostOrigins() map[string]bool {
	hosts, err := s.db.ListServedHostnames()
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		out[normalizeOrigin(s.cfg.AppHostOrigin(h))] = true
	}
	return out
}

// normalizeOrigin reduces a URL to scheme://host[:port] for comparison.
func normalizeOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

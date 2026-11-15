package auth

import "net/http"

// The platform's cookies all live under one prefix, and **the prefix is the
// contract**: a cookie whose name starts with `toft_` belongs to the
// platform and is stripped before any proxied request reaches a deployed app;
// every other name in the browser's jar belongs to the app.
//
// It was not always so, and the reason for the change is worth keeping. The
// session cookie used to be named `session` — which is Flask's default
// `SESSION_COOKIE_NAME`. The proxy strips platform cookies *by name*, and a
// name is all the Cookie header carries: no path comes with it. So a deployed
// Flask app calling `flask.session` had its own cookie removed from every
// request it ever received, and its session silently reset on each one. The
// platform had reached into the app's namespace and taken the most obvious
// name in it.
//
// Path scoping is why freeing that name is safe rather than merely convenient.
// The platform's session cookie is `Path=/api/v1` and a deployed app is served
// under `/apps/{slug}`, so a browser never sends it to an app in the first
// place — stripping it was defence in depth, and the depth is preserved by
// moving the platform to a name no framework will ever pick.
//
// It was a window, not a flag day: the old name was read as a fallback, never
// written, for long enough that every session issued under it has expired. The
// fallback is gone now, and it had to go rather than merely could: `session` is
// a name a deployed app's page can set from script, so reading it handed the
// platform's authentication to whatever an app planted. Signing in still
// expires the old cookie, which is housekeeping and nothing more.
const (
	// CookiePrefix is reserved by the platform. Anything under it is stripped
	// from a proxied request; anything outside it is the app's.
	CookiePrefix = "toft_"

	// SessionCookie carries the platform JWT on the platform API path.
	SessionCookie = CookiePrefix + "session"

	// HostSessionCookie is the session as a two-host install's browsers carry
	// it. A `__Host-` cookie cannot have a Domain attribute, so the apps host —
	// a sibling under the same parent domain — cannot toss one onto the
	// platform's host the way it can a plain cookie. See SessionTokenFor.
	HostSessionCookie = HostCookiePrefix + SessionCookie

	// HostCookiePrefix is the browser's own: a cookie under it must be Secure,
	// at Path=/, with no Domain, so only the host that set it can have.
	HostCookiePrefix = "__Host-"

	// LegacySessionCookie is what SessionCookie was called before the prefix
	// existed. Never read and never written; only expired. See the note above.
	LegacySessionCookie = "session"
)

// SessionToken is SessionTokenFor on a one-host install.
func SessionToken(r *http.Request) string { return SessionTokenFor(r, false) }

// SessionTokenFor returns the platform session JWT a request carries, or "". A
// caller still validates what it gets — an unparseable value here means an
// anonymous request, never an error.
//
// **Two copies of the cookie is no session.** A page on the apps origin can set
// a cookie the platform's host receives — a different port of the same host,
// or `Domain=` the parent of a sibling — and a browser then sends both, the
// planted one first when its path is longer. Choosing either is choosing whose
// session a person gets, so neither is chosen.
//
// hostCookie is a two-host install serving secure cookies. There the browser's
// session is HostSessionCookie and only that: a request carrying fetch
// metadata is a browser, and the plain name is the one a sibling host can
// toss. A client that is not a browser — the CLI, a script — still presents
// the plain name, and still may.
func SessionTokenFor(r *http.Request, hostCookie bool) string {
	if hostCookie {
		if v, n := cookieValue(r, HostSessionCookie); n == 1 {
			return v
		} else if n > 1 {
			return ""
		}
		if r.Header.Get("Sec-Fetch-Site") != "" {
			return ""
		}
	}
	if v, n := cookieValue(r, SessionCookie); n == 1 {
		return v
	}
	return ""
}

// cookieValue is the value of the named cookie and how many non-empty copies of
// it the request carries.
func cookieValue(r *http.Request, name string) (string, int) {
	var v string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			v = c.Value
			n++
		}
	}
	return v, n
}

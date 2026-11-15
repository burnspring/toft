package api

import (
	"net/http"
	"strings"

	"toft/internal/artifact"
	"toft/internal/config"
)

// The response headers a browser needs in order to defend the platform's own
// pages, set in one place because a header set on most responses is a header
// somebody can find a page without.
//
// **Not on deployed apps** — and the exemption is not a path, which it was
// until a scan asked for `/apps` and got the app list back with no policy on it
// at all. `/apps/{slug}` is three different things wearing one prefix: the
// proxy on a single-origin install, a redirect on a two-origin one, and the
// SPA's own `/apps/new`, `/apps/{id}` and `/apps/{id}/{tab}` on both. Nothing
// in the path tells them apart — a slug and an app id occupy the same segment —
// so a prefix wide enough to exempt the first exempted all three, and left
// every app-management page in this application framable and without a CSP.
// The scanner reached one of them signed out; the rest were worse and quieter.
//
// So they are set here on everything this router serves, and taken off again by
// `appProxyGuard`, at the one point where a real app has been resolved and the
// SPA fall-through ruled out. The rule has not changed — the platform writes no
// policy over somebody else's code — but it is decided now by resolving the app
// rather than by spelling its path.
//
// The reason for the rule has not changed either. An app is somebody else's
// code: a Content-Security-Policy written for this SPA would forbid the inline
// scripts Shiny, Streamlit and Dash all emit, and would do it on a page the
// platform does not own. The platform's part in an app is the routing; the
// app's part is all the rest, which includes deciding what its own pages may
// load. `frame-ancestors 'none'` would also break the platform's own preview
// iframe, which embeds exactly that path.
//
// A deployed app that wants these headers sets them itself, and a static app
// gets `nosniff` from `internal/staticserve` — the one case where the platform
// *is* the server.

// securityHeaderPolicy is the built Content-Security-Policy and the headers
// beside it, resolved once from configuration rather than per request.
type securityHeaderPolicy struct {
	csp string
}

// newSecurityHeaderPolicy builds the policy for this install.
//
// The two directives that are not simply 'self' are the two the product needs:
//
//   - `frame-src` carries the apps origin, because the app page embeds a live
//     preview of the app in an iframe. On a single-origin install that origin
//     *is* 'self' and the extra source is absent rather than redundant.
//   - `img-src` allows any https source, because a person's avatar comes from
//     whichever directory or git host signed them in and the set of those is
//     not knowable here. It is images only: a foreign image can be a tracking
//     pixel and cannot be a script.
//
// `style-src` keeps 'unsafe-inline' and that is a real weakening, said out
// loud: the component library positions menus, popovers and tooltips by writing
// a `style` attribute, so a policy without it renders the application unusable
// rather than merely plainer. `script-src` does not have it, which is the half
// that matters for XSS.
//
// `connect-src` also carries the object store's origins when store uploads are
// on: the store browser PUTs straight to the bucket, and without them the
// install's own page refused every upload as a network error the transfer
// tray then blamed on the bucket's CORS. And `script-src` has
// 'wasm-unsafe-eval' — never 'unsafe-eval' — because the digests the platform
// signs into an upload URL are computed by a WebAssembly module, and a script
// policy without it refuses to compile one (docs/bugs 2026-10-22-e2e-storage #1).
func newSecurityHeaderPolicy(appsBaseURL string, separateAppsOrigin bool, storeOrigins []string) securityHeaderPolicy {
	frameSrc := "'self'"
	connectSrc := "'self'"
	if separateAppsOrigin && appsBaseURL != "" {
		frameSrc += " " + appsBaseURL
		connectSrc += " " + appsBaseURL
	}
	for _, o := range storeOrigins {
		connectSrc += " " + o
	}

	directives := []string{
		"default-src 'self'",
		"base-uri 'self'",
		"object-src 'none'",
		// The anti-clickjacking half. X-Frame-Options below says the same thing
		// for anything that does not read this.
		"frame-ancestors 'none'",
		"form-action 'self'",
		"script-src 'self' 'wasm-unsafe-eval'",
		// 'unsafe-inline', and it was narrowed to style-src-attr once and put
		// back. The attribute half is what the component library needs to
		// position menus and popovers, but something in the SPA's dependencies
		// also *injects* inline `<style>` elements at runtime — five refusals
		// across five pages in a headless browser, one of them for an empty
		// one. Their hashes would have to be listed, and would change with the
		// next dependency bump, breaking the application's styling in a way no
		// test here would catch. So the exception stays wide and stays visible:
		// the scan reports it every run rather than a comment claiming it was
		// considered.
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: https:",
		"font-src 'self' data:",
		// 'self' covers the log stream's WebSocket: it is opened against
		// window.location.host, and CSP matches ws:// and wss:// on the
		// document's own host.
		"connect-src " + connectSrc,
		"frame-src " + frameSrc,
	}
	return securityHeaderPolicy{csp: strings.Join(directives, "; ")}
}

// securityHeaders sets them on everything this router serves. Which responses
// turn out to belong to an app is not a question a middleware can answer, so it
// does not try: appProxyGuard takes them off again once it knows.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	var storeOrigins []string
	if o, ok := s.storeSigner.(artifact.Origins); ok && s.storeSigner != nil {
		storeOrigins = o.ConnectOrigins()
	}
	policy := newSecurityHeaderPolicy(s.cfg.AppsBaseURL, s.cfg.SeparateAppsListener(), storeOrigins)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy.csp)
		// The same refusal for a browser that does not implement
		// frame-ancestors. DENY rather than SAMEORIGIN: nothing in this
		// application frames another of its own pages.
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// Send the origin to other sites, the full path only to this one, and
		// nothing at all when leaving https for http.
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// Strict-Transport-Security is the host's, and so it is the install's.
//
// It is not one more header in the list above. A browser keys HSTS by host, not
// by origin and not by path, and every shape this product supports gives the
// host to the install: one origin, two hosts, or two ports on one host. So it is
// set by middleware on **both** routers, which reaches the gateway's own cards,
// the redeem's redirects, security.txt and every refusal as well as every
// proxied response. It is never taken off by appProxyGuard. An app's own copy is
// deleted in handleAppProxy's ModifyResponse, because one app sending
// `max-age=0` would switch HSTS off for every app on the host.
//
// No includeSubDomains, because whether every name under the organization's
// domain is https is the organization's DNS policy rather than this host's. No
// preload, because it cannot be undone from the product.
const hstsValue = "max-age=31536000"

// isHTTPSURL is whether a configured address names https.
func isHTTPSURL(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "https://")
}

// hstsWithheld is the one shape on which the install can tell from its own
// configuration that the header is wrong: apps on another port of the
// platform's own host, under the other scheme. HSTS ignores the port, so the
// https listener's header would rewrite every link to the http one into an
// https URL that nothing serves, for a year, in every browser that once opened
// the https side. Withheld rather than refused, because refusing to start
// would stop an install that works today on the morning of its upgrade.
func hstsWithheld(cfg *config.Config) bool {
	return cfg.SeparateAppsListener() && !cfg.TwoHostMode() &&
		isHTTPSURL(cfg.BaseURL) != isHTTPSURL(cfg.AppsBaseURL)
}

// HSTSWithheld is hstsWithheld for main.go's startup line, so the sentence an
// operator reads and the decision the routers make are one function.
func HSTSWithheld(cfg *config.Config) bool { return hstsWithheld(cfg) }

// hsts sets the header for the listener whose configured address is
// listenerURL: BASE_URL on the platform router, APPS_BASE_URL on the gateway.
//
// **The request's scheme or the configured address, either one.** A browser
// ignores HSTS received over plain http (RFC 6797 §8.1), so sending it there
// costs nothing. The configured half is what reaches a chart install whose
// trusted-proxy list is still the loopback default, where every request reads
// as http and a request-only rule would send it to nobody without anything
// saying so. The request half covers an install that never stated an https
// address and is reached over one anyway.
//
// Mount after realIPMiddleware: schemeFromRequest believes X-Forwarded-Proto
// only on the trusted-proxy verdict that middleware records.
func (s *Server) hsts(listenerURL string) func(http.Handler) http.Handler {
	if hstsWithheld(s.cfg) {
		return func(next http.Handler) http.Handler { return next }
	}
	configured := isHTTPSURL(listenerURL)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if configured || s.schemeFromRequest(r) == "https" {
				w.Header().Set("Strict-Transport-Security", hstsValue)
			}
			next.ServeHTTP(w, r)
		})
	}
}

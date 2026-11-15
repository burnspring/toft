package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/notify"
	"toft/internal/runtime"
	"toft/internal/schedule"
	"toft/internal/telemetry"
)

// retiredAppKeyPrefix is what an app key's secret began with. App keys were
// removed (migration 00065); the prefix is kept, and never given to another
// class, so a leaked old key can only ever be refused — by name, at both
// doors, rather than falling through to anonymous or to a cookie.
const retiredAppKeyPrefix = "toft_"

// sessionHeader carries the viewer's opaque per-login session id (the JWT's
// jti) to the app. It is a correlation id, not identity: apps use it to keep
// two browser logins' scratch files apart (see the SDKs' sessionScratchDir),
// and it never says who the viewer is.
const sessionHeader = "X-Toft-Session"

// scanHeader is a caller saying this request is a scan and not a use of the
// app.
//
// **It suppresses telling, never doing.** What it changes is one thing: the
// request is not written to the app's usage counters. Authorization is
// unchanged, no refusal is different because of it, nothing is switched off,
// and no authorization path reads it — it has exactly the standing the role
// word has, a description the platform acts on for its own bookkeeping. An
// install that wants scan traffic *refused* somewhere is asking for a
// different feature, and it cannot be built on this header.
//
// **Believed from the scanner account and nobody else** — an account an
// administrator marked (users.scanner_since), on its session or its personal
// key. From anybody else it is stripped and ignored — a viewer who could make their own traffic invisible is the
// failure this rule exists to prevent, and it would be invisible by
// construction. The mark is set by an administrator and never by its holder,
// which is the whole difference.
//
// It is deleted from every request on the inbound path whatever the
// credential, like `traceparent`: what an app receives must never include a
// platform header a visitor could have set.
const scanHeader = "X-Toft-Scan"

// isScannerAccount reads the scanner mark, and only after the header claimed a
// scan — so the read costs nothing on ordinary traffic. A failed read is
// counted as usage: the safe error for a subtraction is not to subtract.
func (s *Server) isScannerAccount(userID string) bool {
	ok, err := s.db.IsScannerAccount(userID)
	if err != nil {
		log.Printf("gateway: reading the scanner mark for %s: %v", userID, err)
		return false
	}
	return ok
}

// isScanValue reads the header's one true value.
//
// `1` and `true` and nothing else: a header whose mere presence counted would
// make `X-Toft-Scan: 0` mean the opposite of what anyone sending it
// intended, and this is a control whose failure is a silently wrong number on
// somebody else's page.
func isScanValue(v string) bool {
	switch strings.TrimSpace(v) {
	case "1", "true":
		return true
	}
	return false
}

// retiredAppKeyFromRequest reports a presented app key, which is now only ever
// refused. Both header forms, as the class accepted.
func retiredAppKeyFromRequest(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("X-API-Key"), retiredAppKeyPrefix) {
		return true
	}
	return strings.HasPrefix(bearerToken(r), retiredAppKeyPrefix)
}

// retiredAppKeyMessage is the one sentence a holder of an old key needs.
const retiredAppKeyMessage = "app API keys (toft_…) were removed from Toft. Use the personal " +
	"API key (toftu_…) of an account that has been granted access to this app."

func hashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// gatewayPrincipalFromKey resolves a personal key presented at the apps
// gateway into its owner's principal, writing the refusal itself when the key
// cannot be used. The refusal vocabulary is the platform API's, for the same
// reason: the holder of a matching hash learns nothing from being told why.
//
// **The key's scope is deliberately not read here.** A read-scoped key reaches
// every app its owner can read, with any method, exactly as a full-scope one
// does. The scope bounds Toft's own routes; it says nothing about a
// deployed app's, because Toft has no view into them — a bound claimed
// here would be asserting HTTP semantics on somebody else's code, and it would
// be false the first time an app put a side effect behind a GET. Refusing the
// gateway outright was the other candidate and costs the read case its most
// obvious use. TestAReadScopedKeyStillWritesThroughTheGateway is the decision
// stated as behaviour, so a later tightening fails a test rather than a
// customer's automation.
func (s *Server) gatewayPrincipalFromKey(w http.ResponseWriter, r *http.Request, secret string) (*auth.Principal, bool) {
	key, err := s.db.ResolveUserAPIKey(hashAPIKey(secret))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	if why := gatewayKeyRefusal(key); why != "" {
		http.Error(w, why, http.StatusUnauthorized)
		return nil, false
	}

	p, err := s.gatewayKeyPrincipal(key)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	return p, true
}

// gatewayKeyRefusal is why a resolved personal key may not open an app, or ""
// when it may. Writes no response, so the socket recheck can ask it too.
func gatewayKeyRefusal(key *database.APIKeyPrincipal) string {
	switch {
	case key == nil:
		return "invalid API key"
	case !key.Enabled:
		return "personal API keys are switched off on this install"
	case key.DisabledAt != nil:
		return "this API key is disabled"
	case key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now().UTC()):
		return "this API key expired on " + key.ExpiresAt.Format("2006-01-02")
	case key.Locked:
		return "the account this API key belongs to is locked"
	case key.Pending:
		return "the account this API key belongs to is awaiting activation"
	}
	return ""
}

// gatewayKeyPrincipal is the account a usable personal key names, with its
// groups read now.
func (s *Server) gatewayKeyPrincipal(key *database.APIKeyPrincipal) (*auth.Principal, error) {
	groups, err := s.db.GetUserGroupIDs(key.UserID)
	if err != nil {
		return nil, err
	}
	return &auth.Principal{
		Type:       database.PrincipalTypeUser,
		ID:         key.UserID,
		SystemRole: key.SystemRole,
		Groups:     groups,
		APIKeyID:   key.KeyID,
		APIKeyName: key.KeyName,
	}, nil
}

// socketRecheckKey carries, on a WebSocket upgrade, the question the guard
// asked at the handshake in a form that can be asked again: whether this
// caller may still read this app. handleAppProxy asks it every
// socketRecheckInterval for as long as the socket is open and ends the tunnel
// on the first no — a socket is one request, and without this a viewer locked,
// signed out, de-granted or past the session's absolute ceiling kept a live
// app session until it dropped (security review 2026-11-15, HD-4). The idle
// clock is the one the recheck does not apply: a socket makes no requests to
// renew it (docs/invariants.md).
type socketRecheckKey struct{}

// socketRecheckInterval is how often an open socket is re-authorized. A
// variable so tests can shorten it.
var socketRecheckInterval = time.Minute

// withSocketRecheck attaches recheck to an upgrade request; any other request
// is returned unchanged.
func withSocketRecheck(r *http.Request, recheck func() bool) *http.Request {
	if !isUpgrade(r.Header) {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), socketRecheckKey{}, recheck))
}

// appStillReadable re-reads the app and asks whether p may still read it and
// whether it is unlocked. A failed read keeps the socket: only a definite
// answer ends it, so a database blip does not drop every open session.
func (s *Server) appStillReadable(appID string, p *auth.Principal) bool {
	app, err := s.db.GetApp(appID)
	if err != nil {
		return true
	}
	if app == nil || app.Locked() {
		return false
	}
	return auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db).Allow
}

// serveRechecked runs serve with a context that is cancelled the first time
// the request's socket recheck says no. Without a recheck it is serve(r).
// ReverseProxy ends an upgraded tunnel when the request context ends.
func serveRechecked(r *http.Request, serve func(*http.Request)) {
	recheck, _ := r.Context().Value(socketRecheckKey{}).(func() bool)
	if recheck == nil {
		serve(r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		t := time.NewTicker(socketRecheckInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !recheck() {
					cancel()
					return
				}
			}
		}
	}()
	serve(r.WithContext(ctx))
}

// serveCountingKey runs the rest of the chain and records the outcome against
// the key. The status is the point — a token quietly 404ing every night looks
// identical to a healthy one in a plain request count.
func (s *Server) serveCountingKey(w http.ResponseWriter, r *http.Request, next http.Handler, keyID, kind, appID string) {
	// The common exit of both gateway credential paths, and therefore the one
	// place the access log has to be told which credential was spent.
	setAccessKey(r.Context(), keyID, kind)
	ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
	next.ServeHTTP(ww, r)
	status := ww.Status()
	if status == 0 {
		status = http.StatusOK
	}
	s.usage.RecordKey(keyID, kind, appID, status)
}

// appProxyGuard checks if the slug matches a deployed app.
// If not, it returns 404 so Chi falls through to the SPA NotFound handler.
// countVisitor is the anonymous audience at the record site (ADR § The
// anonymous audience): after authorization, so a refused request is never a
// visitor, and only for a request the usage counter also counts.
//
//   - Switch off: nothing is issued or counted, and a presented cookie is
//     expired — off leaves nothing behind in a browser that comes back.
//   - Signed in: nothing. A person is counted by name among the people; a
//     cookie picked up before signing in stays and is ignored.
//   - Sec-GPC: 1: nothing. The page load still counts; the browser does not.
//   - A valid cookie: added to today's sketch for the app. Any request counts
//     — the stylesheet the document pulled is the return.
//   - No valid cookie, on a page load: issued one, not counted. Issuing only on
//     page loads keeps a page's parallel asset requests from racing to mint
//     different ids; counting only on return makes a client with no cookie
//     jar — a crawler, curl, a monitor — count zero.
func (s *Server) countVisitor(w http.ResponseWriter, r *http.Request, p *auth.Principal, appID string) {
	if !s.db.AudienceMeasurement() {
		if _, presented := s.visitorID(r); presented {
			s.expireVisitorCookie(w, r)
		}
		return
	}
	if p != nil || r.Header.Get("Sec-GPC") == "1" {
		return
	}
	if id, _ := s.visitorID(r); id != "" {
		s.usage.RecordVisitor(appID, id)
		return
	}
	if isPageLoad(r) {
		s.setVisitorCookie(w, r)
	}
}

// unresolved is what a request under the apps prefix gets when the slug names
// no app. It is a property of the *origin*, not of the path, which is why the
// guard is handed one rather than deciding for itself:
//
//   - On the platform origin the answer is the SPA. `/apps/new` and
//     `/apps/{id}/{tab}` are as much the application as `/settings` is, and a
//     single-origin install serves them from the same router that proxies apps.
//   - On the apps gateway the answer is 404 and nothing else. That origin
//     serves deployed apps and nothing else, so that code running on it has no
//     same-origin path to Toft itself — and the platform's own document
//     appearing there is exactly the shape origin isolation exists to prevent.
//     It also made a deleted app look alive: the slug stopped resolving, the
//     SPA answered 200, and anything checking whether the app was gone was
//     told yes by the wrong server.
func (s *Server) gatewayUnresolved(w http.ResponseWriter, r *http.Request) {
	s.serveGatewayNotFound(w, r)
}

// gatewayFrameCSS is the frame both cards on the apps origin wear: the one
// the SPA's three pages outside its `Layout` share (`PublicFrame`, the
// landing page, `/login`, `/cli`). One white surface, a header carrying the
// wordmark over a hairline, and a left-aligned display heading — so an app
// that is asleep or building reads as the same product its sign-in did.
//
// Nothing here can load from the SPA, so the tokens are a transcription of
// `web/src/index.css`'s `:root`, never a second opinion, and the type scale is
// the SPA's written out in pixels: the heading is `text-4xl`/`sm:text-5xl` at
// `tracking-tighter`, the lede `text-lg`, the container `max-w-5xl` under a
// `max-w-6xl` header with the 16px/24px gutter.
const gatewayFrameCSS = `
    :root {
      --background: oklch(0.98 0.002 250);
      --card: oklch(1 0 0);
      --foreground: oklch(0.145 0 0);
      --muted-foreground: oklch(0.50 0 0);
      --border: oklch(0.912 0.005 264);
      --primary: oklch(0.488 0.180 264);
    }
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { min-height: 100vh; display: flex; flex-direction: column;
           background: var(--card); color: var(--foreground);
           font-family: 'Geist Variable', ui-sans-serif, system-ui, -apple-system,
                        'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
           font-size: 14px; line-height: 1.5; -webkit-font-smoothing: antialiased; }
    header { border-bottom: 1px solid var(--border); }
    .bar { max-width: 72rem; margin: 0 auto; height: 56px; padding: 0 16px;
           display: flex; align-items: center; }
    .wordmark { display: flex; align-items: center; gap: 8px;
                font-weight: 600; letter-spacing: -0.025em; }
    .wordmark svg { width: 18px; height: 18px; color: var(--primary); }
    main { flex: 1; display: flex; align-items: center; }
    .content { width: 100%; max-width: 64rem; margin: 0 auto; padding: 56px 16px; }
    .inner { max-width: 36rem; }
    h1 { font-size: 36px; line-height: 1.05; font-weight: 600; letter-spacing: -0.05em;
         text-wrap: balance; }
    .lede { margin-top: 16px; font-size: 18px; line-height: 1.625;
            color: var(--muted-foreground); text-wrap: pretty; }
    .foot { margin-top: 40px; padding-top: 20px; border-top: 1px solid var(--border);
            display: flex; flex-wrap: wrap; align-items: center; gap: 12px; }
    a { color: var(--primary); font-weight: 500; text-decoration: none; }
    a:hover { text-decoration: underline; }
    @media (min-width: 640px) {
      .bar { padding: 0 24px; }
      .content { padding: 80px 24px; }
      h1 { font-size: 48px; }
    }`

// gatewayFrameHeader is the wordmark, unlinked: each card's one way back is
// the link at its foot, and a second to the same place is noise. The glyph is
// Toft's mark, transcribed from web/src/components/toft-mark.tsx.
const gatewayFrameHeader = `  <header>
    <div class="bar">
      <span class="wordmark">
        <svg viewBox="0 0 32 32" aria-hidden="true">
          <path d="M4.5 14.5 16 5l11.5 9.5" fill="none" stroke="currentColor" stroke-width="3.6"
                stroke-linecap="round" stroke-linejoin="round"/>
          <rect x="8" y="17.5" width="16" height="10" rx="2.2" fill="currentColor"/>
        </svg>
        Toft
      </span>
    </div>
  </header>
`

// notFoundTmpl is the card a person gets on the apps origin when the address
// resolves to nothing they can open.
//
// It says the minimum, and that is the whole design. **404 over 403** means
// this response has to be the same for a slug that names no app and for one
// that names an app this account may not read — so it names neither the app
// nor its owner, and the two paths render the identical page. What it adds
// over the bare `not found` it replaces is the one thing that leaks nothing:
// a route back to Toft, which is where a person who was sent a link they
// cannot open finds out what they can.
var notFoundTmpl = template.Must(template.New("gateway404").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Not found</title>
  <style>` + gatewayFrameCSS + `
  </style>
</head>
<body>
` + gatewayFrameHeader + `  <main>
    <div class="content">
      <div class="inner">
        <h1>Not found</h1>
        <p class="lede">There is nothing at this address for you — either it names no app on
           this install, or it names one this account cannot open. If somebody sent you
           the link, ask them for access.</p>
{{if .PlatformLink}}        <div class="foot"><a href="{{.PlatformLink}}">Go to Toft →</a></div>
{{end}}      </div>
    </div>
  </main>
</body>
</html>`))

// listOnlyTmpl is the card for the one caller 404-over-403 does not cover: a
// person holding the `list` rung, whose app list already showed them this app
// (docs/invariants.md § Authorization, the deny_list_only exception). Telling
// them "names no app" contradicted their own list and hid the way in — the
// app's page in Toft, where Request access is (BB-95, BB-107). It names the
// app, which the caller has been shown, and nothing beyond it.
var listOnlyTmpl = template.Must(template.New("gateway403").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{.Name}} · No access yet</title>
  <style>` + gatewayFrameCSS + `
  </style>
</head>
<body>
` + gatewayFrameHeader + `  <main>
    <div class="content">
      <div class="inner">
        <h1>You can't open {{.Name}} yet</h1>
        <p class="lede">This app is listed for you, but this account doesn't have access to
           open it. Ask its owner for access on the app's page in Toft.</p>
        <div class="foot"><a href="{{.AppPage}}">Request access →</a></div>
      </div>
    </div>
  </main>
</body>
</html>`))

// serveGatewayListOnly answers a list-rung holder: 403, a card linking to the
// app's page for a browser, and the API's own deny_list_only wording for
// anything else.
func (s *Server) serveGatewayListOnly(w http.ResponseWriter, r *http.Request, app *database.App) {
	if !isBrowserNavigation(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "forbidden", "code": "deny_list_only", "hint": errorHints["deny_list_only"],
		})
		return
	}
	name := app.Name
	if name == "" {
		name = app.Slug
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	gatewayPageHeaders(w, withFrame(listOnlyCardCSP, ""))
	w.WriteHeader(http.StatusForbidden)
	err := listOnlyTmpl.Execute(w, struct{ Name, AppPage string }{
		name, strings.TrimRight(s.platformURL(), "/") + "/apps/" + url.PathEscape(app.ID),
	})
	if err != nil {
		log.Printf("gateway 403 render: %v", err)
	}
}

// serveGatewayNotFound is the one 404 the apps origin gives a person.
//
// Only for a browser navigation: an app's own fetch for a file that is not
// there, a curl, an XHR, all keep the plain `not found` they have always had,
// because a page of HTML in place of a missing asset is worse than a sentence.
func (s *Server) serveGatewayNotFound(w http.ResponseWriter, r *http.Request) {
	if !isBrowserNavigation(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	gatewayPageHeaders(w, withFrame(notFoundCardCSP, ""))
	w.WriteHeader(http.StatusNotFound)
	err := notFoundTmpl.Execute(w, struct{ PlatformLink string }{
		strings.TrimRight(s.platformURL(), "/"),
	})
	if err != nil {
		log.Printf("gateway 404 render: %v", err)
	}
}

func (s *Server) appProxyGuard(unresolved http.HandlerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return s.guardedAppProxy(next, unresolved)
	}
}

func (s *Server) guardedAppProxy(next http.Handler, unresolved http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, urlPrefix, err := s.requestedApp(r)
		if err != nil {
			// A failed read is not "no such app": answered as one, a live app
			// served the SPA or a 404 card that a browser and a monitor kept.
			log.Printf("proxy: loading the app for %s: %v", r.URL.Path, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if app == nil {
			// Not a deployed app slug. What that means belongs to the origin
			// this router serves — see gatewayUnresolved.
			unresolved(w, r)
			return
		}
		// An app whose release now lives at the root of its own hostname is
		// not served at /apps/{slug} any more: the old address answers 308 to
		// the new one, before authorization — the app host authorizes for
		// itself, and a redirect says nothing a visitor could not learn from
		// DNS. Keyed on the release, not the switch: mid-move the old path
		// keeps serving the old release, and the redirect begins at the flip.
		if urlPrefix != "" && builder.ServingBasePath(app) == "" && app.Hostname != "" {
			http.Redirect(w, r, s.appHostURL(app, strings.TrimPrefix(r.URL.EscapedPath(), urlPrefix), r.URL.RawQuery),
				http.StatusPermanentRedirect)
			return
		}
		// The move back off a cleared hostname is handleAppProxy's card, past
		// authorization: here it named a private app to anyone (security
		// review 2026-11-15, HD-6).

		// Past that fall-through the request belongs to an app, which is where
		// the platform's *response* headers come off. securityHeaders set them
		// on everything — see there for why the decision cannot be made from
		// the path — and this is the one place that knows better.
		//
		// Deleted from the response writer before the proxy runs, so an
		// upstream header of the same name still lands: a static app's own
		// nosniff from internal/staticserve survives this, and so does the
		// frame-ancestors policy handleAppProxy adds afterwards.
		//
		// nosniff is on the list rather than quietly left behind, and that is a
		// decision rather than an oversight. Setting it on proxied responses
		// would be a real improvement on a single-origin install, where content
		// a browser sniffs as HTML runs on the platform's own origin — and it
		// would also stop an app serving any file whose Content-Type it gets
		// wrong. That trade belongs to whoever chooses to make it, not to a fix
		// for a prefix.
		//
		// nosniff and Referrer-Policy come off here and are put back by
		// handleAppProxy's ModifyResponse where the app sent neither, while
		// the install's app_security_headers switch is on — the trade above,
		// made by an administrator rather than by a fix for a prefix.
		//
		// Strict-Transport-Security is never on the list. It is the host's, so
		// the install's value stays on the writer for the copy, and
		// ModifyResponse deletes the app's own (security_headers.go).
		hdr := w.Header()
		hdr.Del("Content-Security-Policy")
		hdr.Del("X-Frame-Options")
		hdr.Del("X-Content-Type-Options")
		hdr.Del("Referrer-Policy")

		// Inbound headers are otherwise forwarded verbatim, so both platform
		// headers are deleted unconditionally — before any path can return
		// early, anonymous and API-key included — and set again below only
		// from a validated session. Merely not setting them on the other paths
		// would let a client-supplied value sail through to the app.
		//
		// The act-for header would be forged rather than merely spoofed, so it
		// is signed and a supplied one would fail verification anyway. Deleting
		// it is still unconditional: an app that received a *valid* blob from
		// somewhere else — another app's page, a copied header — must not have
		// it arrive looking like this request's viewer.
		//
		// The role header is the third member of that rule rather than a new
		// rule, and it is *also* signed — for a reason stripping cannot cover.
		// Deployed apps bind 127.0.0.1 in shared mode and app-to-app isolation
		// there is an accepted risk, so another app on the box can open a socket
		// to this app's port and send whatever headers it likes without the
		// gateway ever seeing the request. Stripping here answers the visitor;
		// the signature answers the neighbour. A stripped-but-unsigned design
		// and a signed-but-unstripped one fail differently and both fail, which
		// is why there is a test for each half.
		r.Header.Del(sessionHeader)
		r.Header.Del(auth.ActForHeader)
		r.Header.Del(auth.RoleHeader)

		// The W3C trace headers join that list, in the same place and for the
		// same reason (ADR §31 D6). An app is not part of the platform's trace,
		// and a traceparent arriving from a visitor's browser is a free channel
		// into somebody else's tooling. Deleted unconditionally here; put back
		// by the Director in handleAppProxy, and only when
		// PROPAGATE_TRACE_TO_APPS says the operator instruments their apps.
		//
		// This runs *after* the telemetry middleware, so a header a trusted
		// proxy sent has already been honoured by the time it is removed.
		r.Header.Del("traceparent")
		r.Header.Del("tracestate")

		// Read once and deleted unconditionally, in that order and in that
		// place: the value is consulted below for a marked scanner account
		// alone, and no branch can forward it to a workload. A platform header a
		// visitor could set must not survive the gateway.
		scanClaimed := isScanValue(r.Header.Get(scanHeader))
		r.Header.Del(scanHeader)
		// The request deadline is the gateway's statement, never a visitor's:
		// an app that waited on a job until a forged deadline would be cut off
		// with a bare 504 the SDK exists to prevent.
		r.Header.Del(RequestDeadlineHeader)
		// And any spelling of those that is the same name once `_` is read as
		// `-`: Go keeps a header with an underscore exactly as sent, and a
		// framework or SDK that folds `_` to `-` (CGI's HTTP_X_TOFT_ROLE, the R
		// SDK's header_key) reads `X_Toft_Session` as the platform's header
		// (security review 2026-11-10, G-2).
		dropUnderscoreSpellings(r.Header, sessionHeader, auth.ActForHeader, auth.RoleHeader,
			scanHeader, RequestDeadlineHeader, "traceparent", "tracestate",
			// And the forwarding headers the Director owns: a client's
			// X_Forwarded_Host survived beside the platform's X-Forwarded-Host,
			// and a server that folds `_` to `-` read the client's (D-8).
			"X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
			"X-Forwarded-Prefix", "X-Script-Name", "True-Client-IP", "Forwarded")
		// And the client's Connection header is applied *here*, before the
		// platform sets anything on this request. The Director settles it too,
		// but by then the session and act-for headers were already set, so
		// `Connection: X-Toft-Act-For, X-Toft-Session` removed the platform's
		// own blob and the app read "no viewer" instead of "nobody in
		// particular" (code review 2026-11-11, D-1).
		settleConnectionHeader(r.Header)

		// Which app served this request, for the span and the metric the
		// middleware writes when the handler returns. The slug was resolved here
		// anyway; doing it a second time to satisfy a metric is exactly the cost
		// this package refuses to add.
		telemetry.SetRequestApp(r.Context(), app.ID)
		setAccessApp(r.Context(), app.ID)
		r = r.WithContext(context.WithValue(r.Context(), guardedAppKey{}, app.ID))
		r = r.WithContext(context.WithValue(r.Context(), urlPrefixKey{}, urlPrefix))

		// An app key is refused by name, and never falls back to a cookie or
		// to anonymous: a presented credential that silently became no
		// credential is the failure this branch always refused. The class was
		// removed (migration 00065); its holder is told what replaces it.
		if retiredAppKeyFromRequest(r) {
			http.Error(w, retiredAppKeyMessage, http.StatusUnauthorized)
			return
		}

		// An exchanged key (ADR §157) deploys and opens nothing. Refused by
		// name rather than passed through, because an unrecognised
		// Authorization header goes to the app, and this one must never reach
		// an app's code.
		if exchangedKeyFromRequest(r) != "" {
			http.Error(w, "a short-lived deploy key (toftx_) cannot open an app — use a personal key or sign in",
				http.StatusUnauthorized)
			return
		}

		// A deploy key deploys and never opens: the request goes on as
		// anonymous — which is what `toft deploy --verify` under TOFT_TOKEN
		// relies on, and what the CLI tells its holder — but the credential
		// itself is taken off first. It is owner on every app of the install,
		// and it used to be forwarded verbatim to a public app's code (code
		// review 2026-11-11, A-1).
		if deployKeyFromRequest(r) != "" {
			stripPresentedKey(r, deployKeySecretPrefix)
		}

		// A personal key reaching a deployed app. Possession is *not*
		// authorization here: the credential names an account, and that
		// account is then authorized exactly as it would be in a browser. It
		// is the one machine credential the gateway takes — an integration is
		// an account of its own, granted on the app it serves.
		if secret := userAPIKeyFromRequest(r); secret != "" {
			p, ok := s.gatewayPrincipalFromKey(w, r, secret)
			if !ok {
				return
			}
			stripPresentedKey(r, userKeySecretPrefix)
			if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
				// No sign-in redirect: whatever holds a token is not a browser
				// that can complete one. A list holder was shown the app, so
				// is told it exists (BB-107); everyone else, the 404.
				if d.Reason == "deny_list_only" {
					s.serveGatewayListOnly(w, r, app)
					return
				}
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			s.touchUserAPIKey(p.APIKeyID, clientIP(r))
			// Recorded as the key, not as the person — deliberately. It keeps
			// /system/active-users honest: a nightly cron is
			// traffic, not a daily active human. The breakdown resolves the
			// row back to its owner at read time.
			// **A scan is never usage**: the scanner account's key, scanning,
			// is not written to the app's usage, and recognition only ever
			// subtracts — the key's own traffic below (serveCountingKey) is
			// still recorded, which is where a scanner's volume belongs. Any
			// other personal key is counted.
			if !(scanClaimed && s.isScannerAccount(p.ID)) {
				s.usage.Record(app.ID, database.UsageKeyPrefix+p.APIKeyID, p.ID, isPageLoad(r))
			}
			s.idle.Touch(app.ID)
			// A personal key names a person, and that person is authorized here
			// exactly as they would be in a browser — so the role is theirs.
			//
			// The error is the list rung arriving at the gateway, which means
			// something above let through somebody who cannot read this app. The
			// request is refused rather than forwarded with a softened word.
			role, err := auth.CallerRole(p, objectFromApp(app), s.db)
			if err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask the app's owner for access: POST /api/v1/apps/{id}/access-requests"})
				return
			}
			r = withCallerRole(r, role)
			keyHash, appID := hashAPIKey(secret), app.ID
			r = withSocketRecheck(r, func() bool {
				key, err := s.db.ResolveUserAPIKey(keyHash)
				if err != nil {
					return true
				}
				if gatewayKeyRefusal(key) != "" {
					return false
				}
				fresh, err := s.gatewayKeyPrincipal(key)
				if err != nil {
					return true
				}
				return s.appStillReadable(appID, fresh)
			})
			s.serveCountingKey(w, r, s.withRunStatus(next, app, p), p.APIKeyID, database.KeyKindUser, app.ID)
			return
		}

		// Enforce visibility: run the JWT parser so we get a principal, then authorize.
		// The claims ride along on whichever path produced the principal:
		// auth.Middleware on the single-port path, the app-session cookie
		// otherwise. Anonymous viewers have none.
		p := auth.GetPrincipal(r.Context())
		claims := auth.GetClaims(r.Context())
		// Whether this request's session came from the gateway's own cookie, and
		// so whether *this* is the middleware that has to renew it. When
		// auth.Middleware produced the principal it has already renewed, and
		// re-minting a second time would set the cookie twice for one request.
		gatewaySession := false
		if p == nil {
			p, claims = s.principalFromAppSession(r)
			gatewaySession = claims != nil
		}
		// The viewer, for the access log. Nil stays nil: an anonymous request
		// to a public app is logged as anonymous rather than as a blank.
		setAccessPrincipal(r.Context(), p)
		// The session id is the jti. Pre-upgrade tokens have none, which is why
		// this is read rather than assumed present.
		sessionID := ""
		if claims != nil {
			sessionID = claims.ID
		}
		if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
			if p != nil && d.Reason == "deny_list_only" {
				s.serveGatewayListOnly(w, r, app)
				return
			}
			if p == nil {
				// A person in a browser gets sent to sign in and back; anything
				// programmatic (fetch, curl, an app's own XHR) keeps the 401.
				// The platform's answer, not the app's: named so a client
				// (toft fetch) need not read the body to say who answered
				// (BB-014, second 2026-11-03 bash).
				w.Header().Set(interstitialHeader, "sign_in_required")
				if isBrowserNavigation(r) {
					returnTo := s.gatewayOrigin(r) + r.URL.RequestURI()
					// Two hostnames: the session cookie is host-only and will
					// never arrive here however many times we send somebody to
					// sign in. Start the handshake instead, which mints a
					// session for THIS origin (app_handshake.go). Sending them
					// to /login here is what produced a login page that could
					// not succeed — it signs them in on the platform host and
					// returns them to a gateway that still sees nobody.
					//
					// And on an app host whatever the install's shape: it is
					// never the platform's host (ParseAppHostname), so a plain
					// sign-in could never set a cookie here either.
					if s.cfg.TwoHostMode() || onAppHost(r) {
						http.Redirect(w, r, s.startHandshake(w, r, returnTo), http.StatusFound)
						return
					}
					// One host: the cookie reaches this listener by itself, so a
					// plain sign-in is all that is missing.
					http.Redirect(w, r, s.cfg.BaseURL+"/login?redirect="+url.QueryEscape(returnTo), http.StatusFound)
					return
				}
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			// The same bytes a slug naming no app gets. That is the point:
			// 404 over 403 is only true if the two are indistinguishable.
			s.serveGatewayNotFound(w, r)
			return
		}

		// A public app on two hosts identifies a signed-in visitor through a
		// hop. Nobody is signed in *here* — the platform's cookie is host-only
		// and never arrives — so a browser navigation with no gateway session
		// and no marker goes through the handshake, which mints a session for
		// this origin if the platform has one and leaves the marker either
		// way. The marker lasts as long as the answer it records: the session
		// minted, or a minute for "nobody is signed in" (BB-69, session.go),
		// so a genuinely anonymous visitor hops at most once a minute and a
		// person who signs in is recognised without opening a private app.
		//
		// **Only a browser's own navigation starts it** (isHandshakeNavigation):
		// the marker is a cookie, and a client that keeps none — a crawler, a
		// link unfurler, an uptime probe asking for HTML — came back without it
		// and went round forever (BB-41). Such a client is served as the
		// anonymous caller it is.
		//
		// **A cookie that is present but dead counts as no session.** Sign-out
		// bumps the token version and cannot clear this host's cookie, so a
		// browser that signed out and in again carried a revoked session here
		// and was served as anonymous until it lapsed (code review 2026-11-11,
		// D-3). The dead cookie is expired on the way to the handshake, so the
		// marker is the loop guard on the next navigation as before.
		if p == nil && (s.cfg.TwoHostMode() || onAppHost(r)) && isHandshakeNavigation(r) {
			dead := s.appSessionToken(r) != ""
			if dead || !s.hasAppsSeenCookie(r) {
				if dead {
					name, path := s.appCookie(r, appSessionCookie, appSessionCookiePath)
					s.expireCookies(w, map[string]string{name: path})
				}
				returnTo := s.gatewayOrigin(r) + r.URL.RequestURI()
				http.Redirect(w, r, s.startHandshake(w, r, returnTo)+"&public=1", http.StatusFound)
				return
			}
		}

		// Usage accounting happens only after authorization, so probes and
		// denied requests never count. Anonymous viewers record as "".
		viewerID := ""
		if p != nil {
			viewerID = p.ID
		}
		// The scanner account in a browser — ZAP's SPA mode signs in as it —
		// is subtracted from the usage counter only: presence and idle below
		// are untouched, as they are for the key. Recognising it excuses the
		// request from counting and never admits one.
		scan := scanClaimed && p != nil && s.isScannerAccount(p.ID)
		if !scan {
			s.usage.Record(app.ID, viewerID, "", isPageLoad(r))
		}
		// The anonymous audience, at the same site and under the same
		// subtraction: whatever is not usage is not an audience either.
		if !scan {
			s.countVisitor(w, r, p, app.ID)
		}
		// Same request, same rule: memory only. Anonymous viewers are usage but
		// not presence, and Touch ignores the empty id.
		s.presence.Touch(viewerID, app.ID)
		// Idle activity counts every authorized request, anonymous included —
		// an app used only by people who never sign in is still in use.
		s.idle.Touch(app.ID)

		// The same idea one layer up: time spent inside a deployed app is time
		// the person was present, so it slides their session forward too. An
		// idle timeout the platform origin applied and the gateway did not
		// would sign somebody out of Toft while they were using it.
		//
		// After authorization, like the counters above — a probe or a refused
		// request is not evidence anybody is there.
		if gatewaySession {
			auth.RenewIfDue(s.jwt, s.sessionPolicy(), s.setGatewaySession(r), w, claims)
		}

		if sessionID != "" {
			r.Header.Set(sessionHeader, sessionID)
		}

		// The act-for blob, so this app can start background work in this
		// person's name without ever learning who they are. Minted here because
		// this is the only place all four facts coincide: a browser request in
		// flight, a validated session, an identified app, and the platform doing
		// the identifying.
		//
		// Session-backed requests only. A personal API key is a person too, but
		// it is not a session — it is not revoked by a token_version bump, so
		// binding a blob's life to that counter would either kill it
		// arbitrarily or, worse, keep it alive after the key was disabled. A
		// script holding an toftu_ key can call the platform's own job routes
		// directly, as itself, which is the better answer for it anyway.
		policy := s.sessionPolicy()
		if deadline, ok := actForDeadline(claims, policy); ok {
			r.Header.Set(auth.ActForHeader, s.jwt.MintActFor(
				p.ID, app.ID, claims.TokenVersion, deadline))
		} else if p == nil {
			// A visitor with no session gets a blob too, so that *nobody in
			// particular* is a thing the app can relay and the platform can
			// refuse by name.
			//
			// Before this, an anonymous visitor pressing a button relayed
			// nothing, and relaying nothing means *the app acting as itself* —
			// the most authority the app has. That was never a decision anybody
			// made; it was the only shape available. Now the two are different
			// values instead of the same absence, and an app can refuse a
			// stranger and still run its own scheduled work.
			//
			// **This is a behaviour change and it is deliberate.** A page that
			// started a job "as the viewer where possible" used to fall back to
			// the app's authority for a stranger; now the start is refused,
			// naming the anonymous act-for. That is the correct behaviour, and
			// making it visible is the whole reason the blob is minted rather
			// than the fallback being left in place.
			r.Header.Set(auth.ActForHeader, s.jwt.MintAnonymousActFor(
				app.ID, time.Now().UTC().Add(policy.Absolute)))
		}

		// What the caller may do to this app, in one word, on every forwarded
		// request. Computed from the principal this request already resolved and
		// gone when the response is written — a lifetime of one request, which
		// is strictly fresher than toft.ViewerFor's sixty-second identity cache
		// that apps use for the same purpose today.
		role, err := auth.CallerRole(p, objectFromApp(app), s.db)
		if err != nil {
			// See the personal-key path above: a caller the ladder says may only
			// list this app has no business reaching its workload, and the
			// gateway refuses rather than describing them as a viewer.
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask the app's owner for access: POST /api/v1/apps/{id}/access-requests"})
			return
		}
		r = withCallerRole(r, role)
		appID := app.ID
		r = withSocketRecheck(r, func() bool {
			if claims != nil && !auth.CheckSession(s.db, claims, s.sessionPolicy()) {
				return false
			}
			return s.appStillReadable(appID, p)
		})
		s.withRunStatus(next, app, p).ServeHTTP(w, r)
	})
}

// withRunStatus answers a run's live status page, on the app's own origin, by
// the gateway and never forwarded — and hands everything else to next. Every
// credential path ends here, after its own authorization, so the page is
// reachable exactly by whoever can reach the app: a session or a personal key
// (the person, as in a browser).
func (s *Server) withRunStatus(next http.Handler, app *database.App, p *auth.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, _ := r.Context().Value(urlPrefixKey{}).(string)
		if runID, ok := runStatusRequest(r.URL.Path, prefix); ok {
			s.serveRunStatus(w, r, app, p, runID)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerRoleKey carries the word chosen here to the Director that mints it.
//
// The role is *decided* here, where the principal was resolved and authorized,
// and *signed* there, where the app row that chose the endpoint is in hand. The
// split is not tidiness: the key is derived per workload, this middleware and
// the handler each read the app row once, and a release flipping between the
// two reads would otherwise sign for the workload this request is no longer
// going to. That window is a deploy, it is milliseconds wide, and what it would
// produce is an app refusing a caller it should have admitted — quiet, brief,
// and on every deploy, which is exactly the kind of wrongness worth ten lines.
type callerRoleKey struct{}

// guardedAppKey carries the id of the app appProxyGuard authorized, so the
// handler's own read of the slug can be held to it.
type guardedAppKey struct{}

func withCallerRole(r *http.Request, role string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), callerRoleKey{}, role))
}

func callerRoleFrom(ctx context.Context) string {
	role, _ := ctx.Value(callerRoleKey{}).(string)
	return role
}

// setRoleHeader signs the role this request already resolved, against the
// workload it is about to be forwarded to.
//
// Silent when there is no role on the context — nothing reaches an app without
// passing appProxyGuard, so that is a programming error rather than a state —
// and silent when the app has no current workload to derive a key for. The far
// end reads an absent header as `unknown`, and `unknown` refuses.
func (s *Server) setRoleHeader(req *http.Request, app *database.App) {
	role := callerRoleFrom(req.Context())
	if role == "" {
		return
	}
	blob := s.jwt.MintRole(role, app.ID, app.CurrentRelease, time.Now().UTC())
	if blob == "" {
		return
	}
	req.Header.Set(auth.RoleHeader, blob)
}

// actForDeadline is how long an act-for blob may live: the session's ABSOLUTE
// deadline, never its sliding `exp`. The distinction is load-bearing enough to
// be its own function with its own test.
//
// MintActFor's comment says why a short TTL is wrong: Streamlit talks over a
// WebSocket, so headers arrive at the handshake and the app holds them for the
// life of the browser session, which is hours. **A sliding `exp` is a short
// TTL.** Passing it through would make the archetypal app's act-for button fail
// thirty minutes into every session — the exact failure that comment forbids,
// reintroduced through a door it was not watching.
//
// What that leaves is a residual, accepted rather than discovered: an app
// holding a blob can act for a viewer whose *platform* session has gone
// idle-expired, until the absolute cap. It is narrow — CheckActForSession
// re-checks token_version live, so a logout, a demotion, a group change or a
// lock kills the blob everywhere immediately — and a blob is only ever minted
// for somebody who was present in that app with a live connection. No sentence
// of settings copy may imply the idle timeout reaches it.
//
// False for a request with no session behind it, and for claims carrying no
// clock at all: a blob with a zero deadline is one nothing would accept.
func actForDeadline(claims *auth.Claims, policy auth.SessionPolicy) (time.Time, bool) {
	if claims == nil {
		return time.Time{}, false
	}
	start := claims.SessionStart()
	if start.IsZero() {
		return time.Time{}, false
	}
	return start.Add(policy.Absolute), true
}

// isBrowserNavigation reports whether a request looks like a top-level page
// load — the only case where redirecting to the login page helps rather than
// corrupting a client that expected app content.
func isBrowserNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// isHandshakeNavigation is isBrowserNavigation that the browser itself vouches
// for: Sec-Fetch-Mode "navigate", which every current browser sends on a
// navigation (top level or framed, so the Preview tab is still identified) and
// no script can forge. Crawlers, unfurlers and probes send none, and they keep
// no cookies either — the marker that ends the public handshake could never
// end it for them. A browser too old to send it is served anonymously, which
// is what it was before the handshake existed.
func isHandshakeNavigation(r *http.Request) bool {
	return isBrowserNavigation(r) && r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// principalFromAppSession parses the apps-gateway session cookie. Proxy routes
// live outside the /api/v1 group where auth.Middleware runs, and on the apps
// gateway port only the /apps-scoped cookie is sent by the browser.
//
// The claims come back with the principal because two headers are derived from
// them: the session id (the JWT's jti, absent on tokens minted before the claim
// existed) and the act-for blob, whose expiry and token version must be the
// session's own rather than a clock of their own.
func (s *Server) principalFromAppSession(r *http.Request) (*auth.Principal, *auth.Claims) {
	token := s.appSessionToken(r)
	if token == "" {
		return nil, nil
	}

	claims, err := s.validateGatewaySession(r, token)
	if err != nil {
		return nil, nil
	}
	// Same revocation rule as auth.Middleware: a stale token_version means
	// the session was revoked (logout, demotion, group change), and a locked
	// user's sessions are dead at the gateway too.
	if !auth.CheckSession(s.db, claims, s.sessionPolicy()) {
		return nil, nil
	}

	p := &auth.Principal{
		Type:         claims.PrincipalType,
		ID:           claims.UserID,
		SystemRole:   claims.SystemRole,
		Groups:       claims.Groups,
		OwningUserID: claims.OwningUserID,
	}
	if p.Type == "" {
		p.Type = "user"
	}
	return p, claims
}

// validateGatewaySession validates the session the gateway was handed for the
// origin this request is on. Where the app handshake runs — two hosts, or an
// app's own hostname — that is a session minted for this origin alone
// (auth.AppsAudience), never a platform session nor another origin's. On one
// host the gateway shares the platform's session by design.
func (s *Server) validateGatewaySession(r *http.Request, token string) (*auth.Claims, error) {
	if s.cfg.TwoHostMode() || onAppHost(r) {
		return s.jwt.ValidateFor(token, auth.AppsAudience(normalizeOrigin(s.gatewayOrigin(r))))
	}
	return s.jwt.Validate(token)
}

func (s *Server) handleAppProxy(w http.ResponseWriter, r *http.Request) {
	app, urlPrefix, err := s.requestedApp(r)
	if err != nil {
		log.Printf("proxy: loading the app for %s: %v", r.URL.Path, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}
	// The guard authorized the app it read under this slug; this read is a
	// second one, kept because it carries the freshest endpoint across a
	// deploy's flip. A rename between the two would hand a request authorized
	// for one app to whichever now holds its slug, so the two must agree.
	if guarded, ok := r.Context().Value(guardedAppKey{}).(string); ok && guarded != app.ID {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	// The lock, before the status switch and not inside it.
	//
	// It has to come first because a lock is orthogonal to status: a locked app
	// may be `stopped` (the usual case — locking stops it), but it may also be
	// `crashed`, or `sleeping` because the reaper got there first, or still
	// `running` if the stop failed. Reading it as one more case in the switch
	// would show "Crashed — this page will refresh automatically" to a visitor
	// whose app is never coming back, and would hand a sleeping locked app to
	// wakeApp, which starts it. This is also therefore the answer to "never wake
	// a locked app": the wake path is below and unreachable from here.
	//
	// No countdown — serveInterstitial only starts one for the statuses whose
	// copy promises it, and a locked app is not going to change on its own.
	if app.Locked() {
		settings, _ := s.db.GetPlatformSettings()
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateLocked, database.ResolveLockMessage(app, settings))
		return
	}

	// An app host whose app's release is still the one built for its gateway
	// path: the move has not flipped, and serving that release here would hand
	// a tree built for /apps/{slug} requests at /. The old address keeps
	// serving until the flip; this one says so and refreshes.
	if onAppHost(r) && builder.ServingBasePath(app) != "" {
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateMoving, "This app is moving to this address.")
		return
	}
	// A release built for the root of a hostname the app no longer has — the
	// name was cleared and the move back has not flipped — is not served here
	// with an empty prefix: its cookies landed at the gateway's root for every
	// other app and its redirects left the app (code review 2026-11-11, D-4).
	// The moving card, until the flip — after the guard has authorized the
	// caller, since it names the app (security review 2026-11-15, HD-6).
	if urlPrefix != "" && builder.ServingBasePath(app) == "" && app.Hostname == "" {
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateMoving,
			"This app is moving back to this address.")
		return
	}

	switch app.Status {
	case database.AppStatusBuilding:
		// `building` is the deploy's state, not the gateway's answer. A redeploy
		// is blue/green — the new release starts beside the old one and the flip
		// moves traffic — so while a previous release is serving, it keeps
		// serving, and the card here would turn every deploy into downtime.
		// Whether the old workload is actually up is the dial's question: a
		// stopped, sleeping or crashed app being redeployed refuses the
		// connection, and the ErrorHandler below answers that with this same
		// card rather than "not responding".
		if servesDuringBuild(app) {
			break
		}
		// "This app", not "your app". Every visitor sees this card, including
		// anonymous ones on a public app; the owner is the one person who never
		// does, because they are watching the deploy console.
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateBuilding, "This app is being built.")
		return
	case database.AppStatusFailed:
		// Written for the reader this surface actually has. "Check the build
		// logs for details" instructs somebody whose access to build logs is
		// gated by `can_edit` — which most visitors do not have, and none of
		// the anonymous ones do.
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateDeployFailed,
			"The most recent deploy failed, so there is nothing new to serve. Its owner has the build log.")
		return
	case database.AppStatusStopped:
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateStopped, "This app has been stopped by its owner.")
		return
	case database.AppStatusReady:
		// Not a dead end: what the app does, for anyone who can see it — each
		// job's schedule, next run and last outcome. No params, logs or
		// controls: those are on the app's page in Toft, linked below.
		s.renderInterstitialWith(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateBatch,
			"This app runs jobs and serves no pages.", false, s.batchJobRows(app))
		return
	case database.AppStatusCrashed:
		// Two different truths wearing one status. While the supervisor is
		// still restarting this app the promise below is worth making; once it
		// has run out of attempts, nothing is going to happen without a person,
		// and a page that promises to refresh into a working app is a lie that
		// keeps a visitor waiting for it. The settled variant drops the promise
		// and the Refresh header together.
		// The memory ending is checked first because it is the one the
		// supervisor cannot answer: it is the health sweep that stopped this
		// workload, in either mode, and `Retrying` says yes for every shared
		// app. It also has the more useful sentence — an owner told "it ran out
		// of memory" has somewhere to go, and one told "it crashed" does not.
		if s.health.OutOfMemory(app) {
			s.serveSettledInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateOutOfMemory,
				"This app kept running out of memory, so it was stopped. Its owner will need to look at it.")
			return
		}
		if s.crashedAppRestarting(app) {
			s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateRestarting, "This app has crashed and is restarting.")
			return
		}
		s.serveSettledInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateCrashed,
			"This app has crashed and is not being restarted. Its owner will need to look at it.")
		return
	case database.AppStatusSleeping:
		// Unlike every other branch here, sleeping is recoverable without a
		// person: this is the request that wakes it. wakeApp returns the
		// refreshed row on success and has already answered otherwise.
		woken := s.wakeApp(w, r, app)
		if woken == nil {
			return
		}
		app = woken
	}

	// The address comes from the endpoint the backend reported when it started
	// this release — the proxy never constructs one, which is what lets a
	// workload live somewhere other than a port on this machine.
	endpoint := app.Endpoint()
	if endpoint.Port == 0 {
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateUnavailable, "This app has no running workload. It may be starting up.")
		return
	}
	// Two prefixes, and every app not in the middle of a move has them equal.
	// urlPrefix is the one the request arrived on — /apps/{slug} on the
	// gateway, "" on an app host — and is what the Director strips. releasePrefix is the one the
	// serving release was *built* for, which is what a keep-prefix framework
	// expects to receive and what the app's own links, redirects and cookies
	// are written on. A renamed app is served at its new path by a release
	// told its old one until its move flips; reading the slug for both is how
	// a rename used to leave a Next.js app answering 404.
	prefix := builder.ServingBasePath(app)

	// The trailing slash is not cosmetic for a framework whose pages carry
	// relative URLs and whose author has no code in which to honour BASE_PATH.
	// 308 rather than 301: permanent, and method-preserving, so nothing about
	// this depends on the request being a GET.
	if r.URL.Path == urlPrefix && builder.NeedsTrailingSlash(app.Framework) {
		withSlash := urlPrefix + "/"
		if r.URL.RawQuery != "" {
			withSlash += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, withSlash, http.StatusPermanentRedirect)
		return
	}

	target, _ := url.Parse(endpoint.URL())

	// Frameworks that were told their mount point at build/start time expect to
	// receive the full prefixed path; the rest are mounted at / and the proxy
	// strips the prefix, announcing it via X-Forwarded-Prefix.
	keepPrefix := builder.KeepsPrefix(app.Framework)
	legacy := app.LegacyPrefixRewrite
	frameAncestorsPolicy := s.frameAncestorsPolicy(app)
	// The install's header baseline, read once per request from the settings
	// cache — an atomic load, never a database read on this path.
	fillHeaders := s.db.AppSecurityHeaders()

	proxy := &httputil.ReverseProxy{
		// Stream responses straight through — buffering breaks SSE and any
		// long-lived response.
		FlushInterval: -1,
		Director: func(req *http.Request) {
			// First, before anything is set: ReverseProxy removes the headers
			// a Connection header names *after* the Director returns, so a
			// client's `Connection: X-Toft-Act-For, X-Real-IP` stripped what
			// the platform set (security review 2026-11-10, D-5).
			settleConnectionHeader(req.Header)

			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host

			// The escaped path travels as the client sent it: trimming the
			// decoded Path alone and clearing RawPath re-encoded the request
			// line, so /files/a%2Fb reached the app as /files/a/b (code
			// review 2026-11-11, D-6).
			escaped := strings.TrimPrefix(req.URL.EscapedPath(), urlPrefix)
			req.URL.Path = strings.TrimPrefix(req.URL.Path, urlPrefix)
			if keepPrefix {
				req.URL.Path = prefix + req.URL.Path
				escaped = prefix + escaped
			}
			if req.URL.Path == "" {
				req.URL.Path, escaped = "/", "/"
			}
			req.URL.RawPath = ""
			if escaped != req.URL.Path {
				req.URL.RawPath = escaped
			}
			req.Host = target.Host

			// A deployed app must never see the viewer's platform credentials.
			stripPlatformCookies(req)

			// Who the client is, once and as this platform resolved it. The
			// inbound headers that claim to say so are the client's own words
			// unless a trusted proxy vouched — and realIP has already read
			// them under exactly that rule — so none is passed on, and apps,
			// which commonly believe X-Real-IP or the leftmost X-Forwarded-For,
			// see the address the audit log does. With X-Forwarded-For
			// deleted, ReverseProxy sets it to that one address itself — but
			// only when RemoteAddr is host:port, and realIP leaves a believed
			// forwarded address bare, so then it is set here (security review
			// 2026-11-15, HD-7). Never unconditionally: on a direct connection
			// ReverseProxy would append the address a second time.
			for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "True-Client-IP", "Forwarded"} {
				req.Header.Del(h)
			}
			if _, _, err := net.SplitHostPort(r.RemoteAddr); err != nil {
				req.Header.Set("X-Forwarded-For", clientIP(r))
			}
			req.Header.Set("X-Real-IP", clientIP(r))

			// The host a trusted proxy declared, as the app-host routing and
			// X-Forwarded-Proto already believe it (D-7).
			req.Header.Set("X-Forwarded-Host", requestHost(r))
			req.Header.Set("X-Forwarded-Proto", s.schemeFromRequest(r))
			req.Header.Set("X-Forwarded-Prefix", prefix)
			// WSGI convention (werkzeug ProxyFix, and the SCRIPT_NAME
			// middleware Flask apps commonly hand-roll).
			req.Header.Set("X-Script-Name", prefix)

			// When this request will be cut off, as far as the gateway knows: the
			// SDK refuses to wait on a job past it (would_outlive_request) and
			// points at respond(run) instead.
			setRequestDeadline(req, time.Now(), s.cfg.AppRequestDeadline)

			// The role, signed here rather than in the guard: this is the app
			// row that chose the endpoint above, so the key the assertion is
			// minted against and the workload it is delivered to are the same
			// read.
			s.setRoleHeader(req, app)

			// Off by default. On, the app receives *this platform's* trace
			// context — never the visitor's, which appProxyGuard deleted — so an
			// operator who has instrumented their own app code gets one
			// connected waterfall instead of two unrelated ones.
			if s.cfg.Telemetry.PropagateToApps {
				telemetry.InjectInto(req.Context(), req.Header)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// An app's own HSTS never reaches the browser. The install's is
			// already on the writer (the hsts middleware) and the guard never
			// strips it, so with this deleted the copy has nothing to add
			// beside it. Without the delete an app's `max-age=0` would follow
			// the install's header, and only header order would keep HSTS on
			// for every other app on the host.
			resp.Header.Del("Strict-Transport-Security")

			// Who may put this app in an iframe, and it is the platform's
			// answer rather than the app's.
			//
			// **Added, never replaced.** A response that already carries a
			// Content-Security-Policy keeps it: browsers enforce every policy
			// they are given, so the app's own directives still apply and this
			// one narrows the framing question on top of them. That also means
			// an app cannot widen what the install allows by sending its own
			// frame-ancestors — the intersection is what runs, and this side
			// of it is the setting.
			resp.Header.Add("Content-Security-Policy", frameAncestorsPolicy)

			// The baseline fills silence, and only here: this is the one place
			// that can see whether the app spoke. Whether it did is decided by
			// the header's *name* — an app that sends either one, with any
			// value, has said something about its own pages and keeps it, which
			// is also the way out for an app whose Content-Type is wrong.
			// appProxyGuard took the platform's own values off the writer
			// before the proxy, or the copy would have put an app's value next
			// to them and the browser would have chosen by order.
			if fillHeaders {
				if _, said := resp.Header["X-Content-Type-Options"]; !said {
					resp.Header.Set("X-Content-Type-Options", "nosniff")
				}
				if _, said := resp.Header["Referrer-Policy"]; !said {
					resp.Header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
				}
			}

			// Cookie confinement is **not** behind the keepPrefix guard, and the
			// bug it fixes is why the ordering here is worth a comment. A
			// framework that emits its own prefixed URLs has said something about
			// its *links*; it has said nothing about where its cookies belong,
			// and the two are orthogonal. While this sat below the early return,
			// Next.js and Streamlit — the two most likely frameworks on any
			// install — had every `Set-Cookie: …; Path=/` forwarded verbatim to
			// the origin root, where on a single-origin install an app-chosen
			// `toft_session` at `Path=/api/v1` overwrites the platform's own
			// cookie in the browser's jar and signs the viewer out. Inbound
			// stripping cannot help: the jar is keyed by name, domain and path,
			// and the collision happens in the browser before any request.
			rewriteCookiePaths(resp, prefix)

			// Above the keepPrefix return, for the reason cookies are: Next.js
			// prefixes its links and not a route handler's redirect, so
			// `Location: /about` went to the install's root — and the toft-sdk
			// skill tells every author a redirect is written without the base
			// path. One that already carries it is left alone on every
			// framework: an app that built it from BASE_PATH or url_for meant
			// it, and no app has routes under its own prefix.
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteLocation(loc, target, prefix))
			}

			if keepPrefix {
				return nil // app emits correctly-prefixed URLs itself
			}

			// Legacy escape hatch: string-rewrite absolute paths in HTML for
			// apps that cannot be told their mount point. Off by default —
			// it corrupts embedded JSON and misses client-rendered URLs.
			if legacy && prefix != "" && strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				rewriteHTMLBody(resp, prefix)
			}

			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A deploy over a release that was not running (stopped, asleep,
			// crashed): there was nothing to keep serving, so the honest card is
			// the build's, exactly as on a first deploy.
			if app.Status == database.AppStatusBuilding {
				s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateBuilding, "This app is being built.")
				return
			}
			s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateUnavailable, "This app is not responding. It may be starting up.")
		},
		// Timing the transport rather than ServeHTTP, so the figure is the app's
		// own latency and not the time a large download or an SSE subscription
		// spent streaming back to the visitor. Nil (and therefore
		// http.DefaultTransport) whenever telemetry is off.
		Transport: telemetry.UpstreamTransport(nil, app.ID),
	}

	// Held for the whole life of the response, not just its arrival. The proxy
	// streams (FlushInterval above), so an SSE subscription or a long download
	// is activity for as long as it lasts — and the reaper must not stop the
	// process underneath one.
	done := s.idle.Begin(app.ID)
	defer done()
	serveRechecked(r, func(r *http.Request) { proxy.ServeHTTP(w, r) })
}

// servesDuringBuild reports whether an app in `building` has a previous release
// the gateway should keep proxying to: a published release with an endpoint,
// on an app that serves pages at all. A first deploy has no release, and a
// batch app (ADR §107) has no service, so both get the building card.
//
// The endpoint cannot belong to another app: a stopped app's port stays
// claimed by its row (internal/ports), and the flip is the only writer that
// moves it.
func servesDuringBuild(app *database.App) bool {
	return app.CurrentRelease != "" &&
		app.Endpoint().Port != 0 &&
		database.LiveStatus(app.Framework) == database.AppStatusRunning
}

// wakeHoldTimeout bounds how long the first visitor to a sleeping app waits
// before getting an interstitial instead of the page. Placing the workload
// through the agent is a second or two and readiness for a slow framework a
// few more; an isolated wake can be minutes, and is not waited out here
// either — the wake carries on, bounded by the backend's own start budget,
// the same one a deploy has. Past this the honest
// answer is a refreshing page, not a request that looks hung.
const wakeHoldTimeout = 25 * time.Second

// wakeApp restarts a sleeping app and returns the refreshed row to proxy to.
// It returns nil when it has already written a response.
//
// The wake itself is shared: concurrent requests for the same app wait on one
// call, and a visitor who gives up does not cancel it.
func (s *Server) wakeApp(w http.ResponseWriter, r *http.Request, app *database.App) *database.App {
	if s.builder == nil {
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateSleeping, "This app is asleep and cannot be woken here.")
		return nil
	}

	isolated := app.RunningModeOrShared() == runtime.ModeIsolated
	wakingCopy := "This app was asleep and is starting up."
	if isolated {
		// A pod's wake is a schedule, perhaps a pull, and the app's own start:
		// the visitor is owed the fact that this may be minutes, not seconds.
		wakingCopy = "This app was asleep and is starting up. This can take a few minutes."
	}

	// **A slow wake is answered by the page that refreshes, not by a request
	// that hangs.** The first visitor to an isolated app holds for the wake as
	// a shared one's does, and the wake goes on past the hold for whoever asks
	// next. Every reload after that used to join it and hold again — a tab
	// spinning for 25 s, then the Waking page, then five seconds, then 25 s
	// more, for as long as the pod took. A wake already in flight is now
	// answered at once, so a reader sees the countdown throughout and reaches
	// the app within one refresh of it serving. Shared apps keep the join: a
	// shared wake is a second or two, and a burst of requests that each got
	// the interstitial instead would be worse than one short wait.
	if isolated && s.wakes.InFlight(app.ID) {
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateWaking, wakingCopy)
		return nil
	}

	// Bound this waiter, not the wake: a client that hangs up stops us waiting,
	// while the restart it triggered runs to completion for whoever asks next.
	ctx, cancel := context.WithTimeout(r.Context(), s.wakeHold)
	defer cancel()

	// How long the first visitor to a slept app waits is the cost of idle sleep
	// made visible (ADR §16). Without it, "sleeping saves money" is a claim with
	// no counter-claim attached.
	wakeStart := time.Now()
	// The group only merges callers that arrive while a wake is in flight. A
	// request that read `sleeping` before the wake finished and reaches here
	// after it would start a second one — a burst straddling the end of a wake
	// restarted the app twice — so the wake re-reads the row and does nothing
	// when someone else has already brought the app back.
	alreadyAwake := false
	if err := s.wakes.Do(ctx, app.ID, func() error {
		if now, err := s.db.GetApp(app.ID); err == nil && now != nil && now.Status == database.AppStatusRunning {
			alreadyAwake = true
			return nil
		}
		return s.builder.Wake(app)
	}); err != nil {
		telemetry.RecordWake(ctx, app.ID, telemetry.OutcomeFailed, time.Since(wakeStart))
		// A timeout means the wake is still running and a cancel means the
		// visitor left; neither is the app's fault, and logging them would bury
		// the wakes that actually failed.
		if ctx.Err() == nil {
			log.Printf("wake: %s: %v", app.Slug, err)
		}
		// Either still waking or failed outright. Both refresh: a wake that is
		// merely slow finishes and the next load serves the app, and one that
		// failed has already written `crashed`, whose interstitial says so.
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateWaking, wakingCopy)
		return nil
	}
	if !alreadyAwake {
		telemetry.RecordWake(ctx, app.ID, telemetry.OutcomeSucceeded, time.Since(wakeStart))
		telemetry.RecordAppTransition(ctx,
			database.AppStatusSleeping, database.AppStatusRunning, telemetry.ReasonWake)
	}

	// Re-read rather than trusting the row this request arrived with: a wake
	// that had to rebuild the workload may have landed on a different address,
	// and proxying to the remembered one would 502 on the very request that
	// paid for the wake.
	fresh, err := s.db.GetApp(app.ID)
	if err != nil || fresh == nil {
		s.serveInterstitial(w, app.Name, app.ID, s.frameAncestorsPolicy(app), stateUnavailable, "This app is not responding. It may be starting up.")
		return nil
	}
	return fresh
}

// rewriteLocation rewrites a backend Location URL to include the proxy prefix.
// "http://127.0.0.1:3101/scores/?x=1" → "/apps/my-app/scores/?x=1"
//
// Only when there is something to prefix: an absolute URL on the backend
// host, or an absolute path. A relative reference — `new/`, `?page=2`, `.`,
// all legal and what Django's HttpResponseRedirect("new/") emits verbatim —
// is returned untouched, because the browser resolves it against the
// prefixed URL it requested and gets the right answer by itself; gluing the
// prefix on made `/apps/desk/tickets/` + `new/` into `/apps/desknew/`.
// rewriteLocation points a redirect the app sent at its place under the
// gateway. A root-relative path, or an absolute URL on the backend's own host
// (the proxy sets Host to it, so an app building URLs from Host names its
// loopback address), loses the host and gains the prefix — once: a path already
// under the prefix, which a prefix-emitting framework's redirects usually are,
// is not given it twice. Anything else is left as it is.
func rewriteLocation(loc string, target *url.URL, prefix string) string {
	parsed, err := url.Parse(loc)
	if err != nil {
		return loc
	}

	absoluteOnBackend := parsed.Host != "" && parsed.Host == target.Host
	absolutePath := parsed.Host == "" && parsed.Scheme == "" && strings.HasPrefix(parsed.Path, "/")
	if !absoluteOnBackend && !absolutePath {
		return loc
	}
	// The escaped forms, not the decoded ones: rebuilt from parsed.Path, an
	// app's `Location: /files/a%3Fb%23c` became /apps/x/files/a?b#c — a
	// different URL (code review 2026-11-11, D-5).
	result := parsed.EscapedPath()
	if !(parsed.Path == prefix || strings.HasPrefix(parsed.Path, prefix+"/")) {
		result = prefix + result
	}
	if parsed.RawQuery != "" {
		result += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		result += "#" + parsed.EscapedFragment()
	}
	return result
}

// rewriteHTMLBody rewrites absolute paths in HTML so they include the proxy prefix.
// href="/foo" → href="/apps/slug/foo", src="/static/x" → src="/apps/slug/static/x", etc.
//
// It decodes gzip and nothing else. A body under an encoding it did not decode
// — brotli, deflate — is returned untouched with its header intact, because
// the alternative was rewriting nothing and deleting the header, which
// delivered brotli bytes labelled as plain HTML. This hatch is opt-in and
// deprecated; it should do less, not more.
func rewriteHTMLBody(resp *http.Response, prefix string) {
	var reader io.ReadCloser
	var wasGzipped bool

	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return
		}
		reader = gr
		wasGzipped = true
	case "", "identity":
		reader = resp.Body
	default:
		return
	}

	body, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		return
	}

	// Rewrite common HTML path patterns
	replacements := []struct{ old, new string }{
		{`href="/`, `href="` + prefix + `/`},
		{`src="/`, `src="` + prefix + `/`},
		{`action="/`, `action="` + prefix + `/`},
		{`hx-get="/`, `hx-get="` + prefix + `/`},
		{`hx-post="/`, `hx-post="` + prefix + `/`},
		{`hx-put="/`, `hx-put="` + prefix + `/`},
		{`hx-delete="/`, `hx-delete="` + prefix + `/`},
		{`hx-patch="/`, `hx-patch="` + prefix + `/`},
		// Single-quoted variants
		{`href='/`, `href='` + prefix + `/`},
		{`src='/`, `src='` + prefix + `/`},
		{`action='/`, `action='` + prefix + `/`},
		{`hx-get='/`, `hx-get='` + prefix + `/`},
		{`hx-post='/`, `hx-post='` + prefix + `/`},
	}

	for _, r := range replacements {
		body = bytes.ReplaceAll(body, []byte(r.old), []byte(r.new))
	}

	if wasGzipped {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		gw.Write(body)
		gw.Close()
		body = buf.Bytes()
		resp.Header.Set("Content-Encoding", "gzip")
	} else {
		resp.Header.Del("Content-Encoding")
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// rewriteCookiePaths fixes Set-Cookie headers so cookies work under the proxy prefix.
// A cookie with Path=/ becomes Path=/apps/{slug}/
//
// It also enforces the reserved prefix in the outbound direction: a cookie the
// app named `toft_…` is dropped, not rewritten. The prefix is documented as
// the platform's and `stripPlatformCookies` enforces it inbound, so enforcing it
// in one direction only left the claim half-true. Path confinement above already
// makes such a cookie harmless in the jar; this is the belt to that pair of
// braces, and the risk it carries is an app that had genuinely chosen a reserved
// name — which the contract already forbids.
func rewriteCookiePaths(resp *http.Response, prefix string) {
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	resp.Header.Del("Set-Cookie")
	for _, cookie := range cookies {
		if isPlatformCookie(cookieName(cookie)) {
			continue
		}
		// Replace Path=/ with Path={prefix}/ but not Path=/something
		cookie = rewriteCookiePath(cookie, prefix)
		resp.Header.Add("Set-Cookie", cookie)
	}
}

// cookieName is the name a Set-Cookie header sets, or "" if it sets nothing.
//
// Deliberately not http.ParseSetCookie: a malformed header this cannot read is
// one whose name is unknown, and the safe reading of an unknown name on this
// path is that it is *not* the platform's — dropping unparseable headers would
// silently eat an app's cookie, which is the failure the whole `session` episode
// was about. The reserved prefix only has to be recognised in the well-formed
// case, because the well-formed case is the only one a browser will store.
func cookieName(cookie string) string {
	name, _, ok := strings.Cut(cookie, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// rewriteCookiePath confines one Set-Cookie to the app's own prefix:
// Path=/ becomes Path=/apps/{slug}/, Path=/foo becomes Path=/apps/{slug}/foo,
// and a cookie with no Path at all is given one.
//
// **Every** Path attribute is rewritten, not the first. RFC 6265 says the last
// one wins, so rewriting only the first left `Path=/x; Path=/api/v1` escaping
// to the platform's own path — where a cookie the app chose the name of would
// overwrite the platform's cookie of that name in the browser's jar, and sign
// the viewer out or worse. That confinement is also the argument for handing
// the name `session` back to apps (`session.go`), which is why it is checked
// here rather than assumed.
//
// A Path already at or under the prefix is left alone: a prefix-aware app —
// one reading X-Forwarded-Prefix, or a framework configured with its base
// path — sets `Path=/apps/desk` itself, and prefixing it again produced
// `/apps/desk/apps/desk`, under which its login never stuck.
func rewriteCookiePath(cookie, prefix string) string {
	parts := strings.Split(cookie, ";")
	found := false
	// From 1: part 0 is name=value and is never an attribute.
	for i, part := range parts[1:] {
		// The name as a browser reads it (RFC 6265 §5.2): cut at the first
		// `=`, blanks trimmed either side — `path =/x` is a Path too, and
		// matching only the `path=` spelling forwarded it verbatim (PB-6).
		name, value, _ := strings.Cut(part, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "path") {
			continue
		}
		found = true
		value = strings.TrimSpace(value)
		if value == prefix || strings.HasPrefix(value, prefix+"/") {
			parts[i+1] = " Path=" + value
			continue
		}
		// A value without a leading slash is one the browser replaces with
		// the default path; concatenated, `Path=2` under /apps/pay became
		// /apps/pay2, a sibling app's path (PB-6).
		if !strings.HasPrefix(value, "/") {
			parts[i+1] = " Path=" + prefix + "/"
			continue
		}
		parts[i+1] = " Path=" + prefix + value
	}
	if !found {
		return cookie + "; Path=" + prefix + "/"
	}
	return strings.Join(parts, ";")
}

// schemeFromRequest reports the scheme the *browser* used, which is not always
// the scheme this process was reached by. It is what the proxy forwards to a
// deployed app as X-Forwarded-Proto.
//
// The distinction is load-bearing for WebSockets. Caddy terminates TLS here and
// speaks plain HTTP to Toft, so r.TLS is nil on a request the browser made
// over https. An app that builds its socket URL from X-Forwarded-Proto — which
// is what every framework's "am I behind a proxy" helper does, and what the
// Toft SDKs tell apps to use — then emits `ws://` on an `https://` page.
// Browsers block that as mixed content, so the socket never opens and the page
// looks merely broken: the handshake is refused before a byte reaches the app,
// and nothing appears in its log.
//
// The inbound header is only believed from a peer inside TRUSTED_PROXY_CIDRS,
// on the same reasoning as realIPMiddleware: X-Forwarded-Proto is trivially
// forgeable, and an app that trusts it decides what its own absolute URLs say.
// With no trusted proxies configured the socket is the only evidence there is.
//
// The question is asked of viaTrustedProxy and never of RemoteAddr, which by
// now is the *client's* address on exactly the installs this matters to: a
// proxy that sets X-Forwarded-For — Caddy does by default — has already had
// realIPMiddleware rewrite the peer out from under this check, so re-testing it
// would answer "untrusted" for every forwarded request and hand the app `http`
// on an https page. That is the bug this comment used to describe as prevented.
func (s *Server) schemeFromRequest(r *http.Request) string {
	if viaTrustedProxy(r) {
		// Caddy sends one value; a chain of proxies appends, and the leftmost
		// is what the browser used. Anything but the two real schemes is
		// discarded rather than forwarded — this value lands in an app's
		// absolute URLs.
		if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
			switch scheme := strings.ToLower(strings.TrimSpace(strings.Split(xfp, ",")[0])); scheme {
			case "https", "http":
				return scheme
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// interstitialTmpl renders the app-status page — the most-seen surface in this
// product, and for a long time the only one that was not on the product's
// identity.
//
// It renders on the apps gateway origin, where the SPA's stylesheet, its fonts
// and its tokens are all unreachable, so it wears `gatewayFrameCSS` — the
// transcribed frame the gateway 404 shares — and adds only the tone *solids*
// the dot needs, transcribed from `web/src/index.css`'s `:root` the same way. Geist is named first and cannot load here, so
// the fallback stack after it is the one that actually renders and is written
// out in full rather than left at `sans-serif`.
//
// app.Name is user-controlled; html/template contextually escapes it, unlike
// the fmt.Fprintf this replaces (which was a stored XSS on the apps-gateway
// origin).
var interstitialTmpl = template.Must(template.New("interstitial").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{.AppName}} — {{.Status}}</title>
  <style>` + gatewayFrameCSS + `
    :root {
      --tone-ok-solid: oklch(0.696 0.17 162.48);
      --tone-attention-solid: oklch(0.769 0.188 70.08);
      --tone-danger-solid: oklch(0.637 0.237 25.331);
      --tone-neutral-solid: oklch(0.552 0.016 285.938);
      --tone-info-solid: oklch(0.488 0.180 264);
    }
    /* The app, named above the state it is in — the eyebrow the SPA's pages
       set in the brand colour, here carrying the dot instead. */
    .status { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
    /* The base carries a colour. The class used to be derived from the status
       word, so an unmapped one — and there was one — rendered a 10px
       transparent hole where the most severe state on this card should be.
       Neutral grey is the fallback: an unknown state is not an emergency, and
       *invisible* is the one answer a status dot may not give. */
    .dot { width: 10px; height: 10px; border-radius: 50%; flex: none;
           background: var(--tone-neutral-solid);
           animation: pulse 2s ease-in-out infinite; }
    /* Named for the tone, not for the status: the tone is chosen where the
       card is, beside the words, and there are five of them however many
       statuses the product grows. */
    .dot.tone-attention { background: var(--tone-attention-solid); }
    .dot.tone-danger { background: var(--tone-danger-solid); }
    .dot.tone-neutral { background: var(--tone-neutral-solid); }
    .dot.tone-ok { background: var(--tone-ok-solid); }
    .dot.tone-info { background: var(--tone-info-solid); }
    /* Whatever the status, a page that is not refreshing itself does not
       pulse: the animation is the promise, in the same way the countdown is.
       Stopped, sleeping and locked are all settled — a locked app is not
       coming back on its own, and a pulsing dot would promise that it might. */
    .dot.settled { animation: none; }
    .app-name { font-family: 'Geist Mono Variable', ui-monospace, SFMono-Regular,
                             Menlo, Consolas, monospace;
                font-size: 13px; color: var(--muted-foreground); word-break: break-all; }
    button { font: inherit; font-weight: 500; color: var(--primary); background: none;
             border: 0; padding: 0; cursor: pointer; }
    button:hover { text-decoration: underline; }
    .refresh { margin-top: 16px; font-size: 13px; color: var(--muted-foreground); }
    /* A batch app's jobs: what it does, for a reader with nothing to open. */
    table.jobs { width: 100%; border-collapse: collapse; margin-top: 16px; font-size: 13px; }
    table.jobs th { text-align: left; font-weight: 500; color: var(--muted-foreground);
                    padding: 6px 8px 6px 0; border-bottom: 1px solid var(--border); }
    table.jobs td { padding: 8px 8px 8px 0; border-bottom: 1px solid var(--border); vertical-align: top; }
    table.jobs .name { font-family: 'Geist Mono Variable', ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
    .last { display: inline-flex; align-items: center; gap: 6px; }
    .last::before { content: ""; width: 8px; height: 8px; border-radius: 50%; flex: none;
                    background: var(--tone-neutral-solid); }
    .last.tone-ok::before { background: var(--tone-ok-solid); }
    .last.tone-danger::before { background: var(--tone-danger-solid); }
    .last.tone-info::before { background: var(--tone-info-solid); }
    @media (max-width: 560px) { table.jobs .wide { display: none; } }
    @media (prefers-reduced-motion: reduce) { .dot { animation: none; } }
    @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
  </style>
</head>
<body>
` + gatewayFrameHeader + `  <main>
    <div class="content">
      <div class="inner">
        <div class="status">
          <span class="dot {{.DotClass}}"></span>
          <span class="app-name">{{.AppName}}</span>
        </div>
        <h1>{{.Status}}</h1>
        <p class="lede">{{.Message}}</p>
{{if .Jobs}}        <table class="jobs">
          <thead><tr><th>Job</th><th class="wide">Schedule</th><th>Next run</th><th>Last run</th></tr></thead>
          <tbody>
{{range .Jobs}}            <tr><td class="name">{{.Name}}</td><td class="wide">{{.Schedule}}</td><td>{{.Next}}</td><td><span class="last tone-{{.LastTone}}">{{.Last}}</span></td></tr>
{{end}}          </tbody>
        </table>
{{end}}{{if .Refresh}}        <p class="refresh" id="refresh">
          <noscript>This page does not refresh itself. <a href="">Check again</a>.</noscript>
        </p>
{{end}}{{if .AppLink}}        <div class="foot"><a href="{{.AppLink}}">Open in Toft →</a></div>
{{end}}      </div>
    </div>
  </main>
{{if .Refresh}}  <script>
    // The refresh, stoppable — WCAG 2.2.1. It was a Refresh: 5 response
    // header, which no reader can pause, extend or turn off, on a page some
    // people sit and watch through a five-minute build. The countdown says
    // what is about to happen and the button ends it; with scripting off the
    // noscript line above offers the same reload by hand.
    (function () {
      var el = document.getElementById("refresh");
      var left = 5, timer;
      var stop = document.createElement("button");
      stop.type = "button";
      stop.textContent = "Stop";
      var text = document.createTextNode("");
      el.appendChild(text);
      el.appendChild(document.createTextNode(" "));
      el.appendChild(stop);
      stop.addEventListener("click", function () {
        clearInterval(timer);
        el.textContent = "";
        var again = document.createElement("button");
        again.type = "button";
        again.textContent = "Check again";
        again.addEventListener("click", function () { location.reload(); });
        el.appendChild(document.createTextNode("Not refreshing. "));
        el.appendChild(again);
      });
      function tick() {
        text.nodeValue = "Checking again in " + left + "s.";
        if (left <= 0) { clearInterval(timer); location.reload(); return; }
        left--;
      }
      tick();
      timer = setInterval(tick, 1000);
    })();
  </script>
{{end}}</body>
</html>`))

// interstitialState is one card's identity: the word on it, the tone its dot
// takes, and whether the page promises to refresh itself.
//
// **The tone is chosen here and passed, never derived from the word.** The dot
// class used to be `strings.ToLower(status)`, so "Out of Memory" became the
// four classes `out of memory settled` — none of which the stylesheet has, and
// the base `.dot` rule declared no background. The most severe state this card
// can show was the one state that rendered no dot at all, and nothing could
// have reported it: a class that matches nothing is silent.
//
// Refresh belongs here too. A page that promises to reload and does not is
// worse than one that never promised, and the promise used to be a list of
// status words compared in a second place — so a status added anywhere else
// silently got no countdown whatever its copy said.
type interstitialState struct {
	Status  string
	Tone    string
	Refresh bool
	// Key is the state as a machine word, sent as X-Toft-Interstitial on the
	// card so a client fetching an app URL can tell the platform's page from
	// the app's own without reading HTML. Distinct per state even where the
	// words on the card are not — restarting and crashed both say "Crashed".
	Key string
}

// interstitialHeader names the card's state on the response. Present on every
// interstitial, on the gateway's own 401 (sign_in_required) and on the run
// status page (run_page) — every answer the platform gives in the app's place
// — and on nothing the app serves, so its presence is the answer to "is this
// the app".
const interstitialHeader = "X-Toft-Interstitial"

// The twelve states this card has, each one named where it is used. `interstitialStates`
// below is the same set as a list, and `interstitial_test.go` walks it: every
// tone must be one the stylesheet has a rule for.
var (
	stateLocked       = interstitialState{"Locked", "neutral", false, "locked"}
	stateBuilding     = interstitialState{"Building", "attention", true, "building"}
	stateDeployFailed = interstitialState{"Deploy failed", "danger", false, "deploy_failed"}
	stateStopped      = interstitialState{"Stopped", "neutral", false, "stopped"}
	stateOutOfMemory  = interstitialState{"Out of memory", "danger", false, "out_of_memory"}
	stateRestarting   = interstitialState{"Crashed", "danger", true, "restarting"}
	stateCrashed      = interstitialState{"Crashed", "danger", false, "crashed"}
	stateSleeping     = interstitialState{"Sleeping", "neutral", false, "sleeping"}
	stateWaking       = interstitialState{"Waking", "attention", true, "waking"}
	stateUnavailable  = interstitialState{"Unavailable", "attention", true, "unavailable"}
	// A batch app (ADR §107) is working as intended when it serves nothing:
	// neutral, and no refresh, because no wait would change the answer.
	stateBatch = interstitialState{"Runs jobs", "neutral", false, "batch"}
	// An app host whose app is still being rebuilt for it: the old address
	// keeps serving the old release until the flip, and this one waits.
	stateMoving = interstitialState{"Moving", "attention", true, "moving"}
)

// interstitialStates is every state the two serve functions are called with.
// Adding a card means adding it here, which is what the table test walks.
var interstitialStates = []interstitialState{
	stateLocked, stateBuilding, stateDeployFailed, stateStopped, stateOutOfMemory,
	stateRestarting, stateCrashed, stateSleeping, stateWaking, stateUnavailable, stateBatch,
	stateMoving,
}

// interstitialTones is the set the stylesheet above has a `.dot.tone-…` rule
// for. Named here so the test compares against something a person can read
// beside the CSS rather than parsing it.
var interstitialTones = map[string]bool{
	"ok": true, "attention": true, "danger": true, "neutral": true, "info": true,
}

func (s *Server) serveInterstitial(
	w http.ResponseWriter, appName, appID, frame string, state interstitialState, message string,
) {
	s.renderInterstitial(w, appName, appID, frame, state, message, state.Refresh)
}

// serveSettledInterstitial is the same card with the promise removed, for a
// state whose *word* can carry both readings: a crashed app is being restarted
// right up until the supervisor stops trying. `stateCrashed` and
// `stateRestarting` are that pair, and this is what the caller reaches for
// where a state that normally refreshes has stopped.
func (s *Server) serveSettledInterstitial(
	w http.ResponseWriter, appName, appID, frame string, state interstitialState, message string,
) {
	s.renderInterstitial(w, appName, appID, frame, state, message, false)
}

// platformURL is where a viewer would reach this install, or "" when nothing
// has said. Nil-safe: the interstitial renders from a bare Server in tests, and
// a card with no link is a supported shape rather than a panic.
func (s *Server) platformURL() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.db.ResolvePublicBaseURL(s.cfg.BaseURL)
}

// frame is the app's frame-ancestors line (frameAncestorsPolicy): a card is
// exactly as framable as the app it stands in for, because the app page's
// preview iframe shows the card before it shows the app. Empty is 'none'.
func (s *Server) renderInterstitial(
	w http.ResponseWriter, appName, appID, frame string, state interstitialState,
	message string, refresh bool,
) {
	s.renderInterstitialWith(w, appName, appID, frame, state, message, refresh, nil)
}

// batchJobRow is one job on a batch app's address card.
type batchJobRow struct {
	Name, Schedule, Next, Last string
	LastTone                   string
}

// batchJobRows reads a batch app's jobs for its address card: when each runs
// and how its last run went, in the schedule's own zone.
func (s *Server) batchJobRows(app *database.App) []batchJobRow {
	defs, err := s.db.ListJobDefinitions(app.ID)
	if err != nil {
		return nil
	}
	rows := make([]batchJobRow, 0, len(defs))
	for _, d := range defs {
		row := batchJobRow{Name: d.Name, Schedule: "when started", Next: "—", Last: "never run", LastTone: "neutral"}
		zone, expr := schedule.Split(d.Schedule)
		loc := time.UTC
		if zone != "" {
			if l, err := time.LoadLocation(zone); err == nil {
				loc = l
			}
		}
		if expr != "" {
			row.Schedule = expr
			if zone != "" {
				row.Schedule += " (" + zone + ")"
			}
			switch {
			case !d.Enabled:
				row.Next = "paused"
			case d.NextRunAt != nil:
				row.Next = d.NextRunAt.In(loc).Format("Mon 2 Jan 15:04 MST")
			}
		}
		if runs, err := s.db.ListJobRuns(app.ID, d.Name, "", 1); err == nil && len(runs) > 0 {
			r := runs[0]
			row.Last = strings.ReplaceAll(r.ShownStatus(), "_", " ")
			when := r.FinishedAt
			if when == nil {
				when = &r.QueuedAt
			}
			row.Last += " · " + when.In(loc).Format("2 Jan 15:04 MST")
			switch r.Status {
			case database.JobRunSucceeded:
				row.LastTone = "ok"
			case database.JobRunFailed, database.JobRunTimedOut, database.JobRunOutOfMemory:
				row.LastTone = "danger"
			case database.JobRunRunning, database.JobRunQueued, database.JobRunClaimed:
				row.LastTone = "info"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// interstitialData is what the status card renders from.
type interstitialData struct {
	AppName, Status, Message, DotClass, AppLink string
	Refresh                                     bool
	Jobs                                        []batchJobRow
}

// renderInterstitialWith is renderInterstitial with a batch app's jobs.
func (s *Server) renderInterstitialWith(
	w http.ResponseWriter, appName, appID, frame string, state interstitialState,
	message string, refresh bool, jobs []batchJobRow,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The script runs only on a card that refreshes, so only that card's
	// policy names its hash.
	policy := statusCardCSP
	if refresh {
		policy = statusCardLiveCSP
	}
	gatewayPageHeaders(w, withFrame(policy, frame))
	key := state.Key
	if key == "" {
		key = "unavailable"
	}
	w.Header().Set(interstitialHeader, key)
	tone := state.Tone
	if !interstitialTones[tone] {
		// Belt and braces beside the CSS fallback: an unnamed tone is grey
		// rather than a class the stylesheet does not have.
		tone = "neutral"
	}
	dot := "tone-" + tone
	if !refresh {
		dot += " settled"
	}
	// The way back. A viewer whose app is asleep, building or locked had no
	// anchor on this page at all, so the app's own page — its owner, its
	// status, the person to ask — was unreachable from the one screen that
	// told them something was wrong.
	//
	// Keyed by the **id**, and this is the one link where getting that wrong
	// was a loop rather than a 404: the slug it used to carry is exactly what
	// the platform origin recognises as a deployed app and redirects to the
	// apps origin, which is the page the reader is already looking at.
	link := notify.AppPageURL(s.platformURL(), appID)
	// A batch app's card is the answer its address exists to give — each
	// job's schedule, next run and last outcome — so it is a 200; every other
	// card stands in for an app that could not answer (BB-6, 2026-11-04).
	code := http.StatusServiceUnavailable
	if state.Key == stateBatch.Key {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	err := interstitialTmpl.Execute(w, interstitialData{
		AppName: appName, Status: state.Status, Message: message, DotClass: dot,
		AppLink: link, Refresh: refresh, Jobs: jobs,
	})
	if err != nil {
		log.Printf("interstitial render: %v", err)
	}
}

// crashedAppRestarting is whether anything is going to bring a crashed app
// back without a person, which is what the restarting card promises.
//
// An isolated app is the supervisor's to answer. A shared one is the agent's:
// it restarts a dead process in place on a backoff and never gives up, but
// only while it still holds the workload. A start that never became ready
// deletes the workload — at boot, on a wake, from the Restart button — and
// then nothing retries it, while the card went on promising a refresh
// (TOFT-BB-003, task-mailer on trytoft). So a shared app is restarting exactly
// when the agent still has its release and has not stopped it.
func (s *Server) crashedAppRestarting(app *database.App) bool {
	if app.RunningModeOrShared() != runtime.ModeShared || s.rts == nil {
		return s.supervisor.Retrying(app)
	}
	rt, err := s.runtimeFor(app)
	if err != nil || rt == nil {
		return s.supervisor.Retrying(app)
	}
	m, err := rt.Metrics(app.CurrentRelease)
	if err != nil || m == nil {
		return false
	}
	return m.Status != runtime.StatusStopped
}

// settleConnectionHeader applies a client's Connection header to the inbound
// headers now — every name it lists is removed, as a hop-by-hop header — and
// leaves it naming nothing but an upgrade. ReverseProxy applies Connection
// after its Director, which would let the client strip a header the Director
// set; with only `Upgrade` left, what it strips is the standard hop-by-hop set
// it re-adds for an upgrade itself.
func settleConnectionHeader(h http.Header) {
	upgrade := false
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			name = textproto.TrimString(name)
			if name == "" {
				continue
			}
			if strings.EqualFold(name, "upgrade") {
				upgrade = true
				continue
			}
			h.Del(name)
		}
	}
	h.Del("Connection")
	if upgrade {
		h.Set("Connection", "Upgrade")
	}
}

// dropUnderscoreSpellings deletes every header whose name, with `_` read as
// `-` and case ignored, is one of names. The canonical spelling is the
// caller's to delete; this catches the ones Header.Del cannot name.
func dropUnderscoreSpellings(h http.Header, names ...string) {
	for k := range h {
		if !strings.Contains(k, "_") {
			continue
		}
		folded := strings.ReplaceAll(k, "_", "-")
		for _, n := range names {
			if strings.EqualFold(folded, n) {
				delete(h, k)
				break
			}
		}
	}
}

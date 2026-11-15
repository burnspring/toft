package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/staticserve"
)

// backend stands in for a deployed app and records what the proxy forwarded.
type backend struct {
	server  *httptest.Server
	port    int
	lastReq *http.Request
	body    string
	headers http.Header
}

func newBackend(t *testing.T, handler http.HandlerFunc) *backend {
	t.Helper()

	b := &backend{body: "ok", headers: http.Header{}}
	h := handler
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			for k, vs := range b.headers {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.Write([]byte(b.body))
		}
	}

	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.lastReq = r.Clone(r.Context())
		h(w, r)
	}))
	t.Cleanup(b.server.Close)

	parsed, err := url.Parse(b.server.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("backend port: %v", err)
	}
	b.port = port

	return b
}

// proxyTo wires an app row pointing at the backend and serves one request
// through the real proxy handler.
func proxyTo(t *testing.T, srv *Server, app *database.App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	// apps.owner_id is a real foreign key under Postgres, so the owner has to
	// exist before the app does.
	if err := srv.db.UpsertUser(&database.User{
		ID: app.OwnerID, Login: app.OwnerID, SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	return proxyServe(t, srv, app, req)
}

// proxyServe is the second and later requests to an app proxyTo already
// created: same handler, same routing context, no second CreateApp.
func proxyServe(t *testing.T, srv *Server, app *database.App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", app.Slug)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	srv.handleAppProxy(rec, req)
	return rec
}

func TestProxyStripsPlatformCookiesFromForwardedRequest(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)

	app := &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/echo/", nil)
	// `session=` is Flask's own, and this app is a Flask app. It must arrive:
	// the platform holding that name is what silently reset a Flask session on
	// every request until the `toft_` prefix took over (ADR §47).
	req.Header.Set("Cookie",
		"toft_session=platform-jwt; toft_app_session=platform-jwt; "+
			"session=flasks-own; app_pref=dark")

	proxyTo(t, srv, app, req)

	if be.lastReq == nil {
		t.Fatal("backend was not called")
	}
	got := be.lastReq.Header.Get("Cookie")
	if strings.Contains(got, "platform-jwt") {
		t.Errorf("platform session leaked to the app: %q", got)
	}
	if !strings.Contains(got, "app_pref=dark") {
		t.Errorf("the app's own cookie was dropped: %q", got)
	}
	if !strings.Contains(got, "session=flasks-own") {
		t.Errorf("the Flask app's own session cookie was dropped: %q", got)
	}
}

// An app is told who the client is once, as the platform resolved it. A
// client's own X-Real-IP, True-Client-IP and Forwarded never reach it, and
// X-Forwarded-For is the one address rather than whatever chain the client
// started — apps commonly believe the leftmost entry.
func TestProxyNamesTheClientOnceAndScrubsItsClaims(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)

	app := &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/echo/", nil)
	req.RemoteAddr = "198.51.100.9:5000"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Real-IP", "10.0.0.2")
	req.Header.Set("True-Client-IP", "10.0.0.3")
	req.Header.Set("Forwarded", "for=10.0.0.4")
	proxyTo(t, srv, app, req)

	if be.lastReq == nil {
		t.Fatal("backend was not called")
	}
	h := be.lastReq.Header
	if got := h.Get("X-Forwarded-For"); got != "198.51.100.9" {
		t.Errorf("X-Forwarded-For = %q, want the one resolved address", got)
	}
	if got := h.Get("X-Real-IP"); got != "198.51.100.9" {
		t.Errorf("X-Real-IP = %q, want the resolved address", got)
	}
	if h.Get("True-Client-IP") != "" || h.Get("Forwarded") != "" {
		t.Errorf("the client's own claims reached the app: %q / %q", h.Get("True-Client-IP"), h.Get("Forwarded"))
	}
}

// Behind a trusted proxy realIP leaves RemoteAddr a bare address, and
// ReverseProxy sets X-Forwarded-For only from host:port — so the app was sent
// none (security review 2026-11-15, HD-7).
func TestProxyNamesTheClientBehindATrustedProxy(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	app := &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/echo/", nil)
	req.RemoteAddr = "198.51.100.9"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	proxyTo(t, srv, app, req)
	if be.lastReq == nil {
		t.Fatal("backend was not called")
	}
	if got := be.lastReq.Header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "198.51.100.9" {
		t.Errorf("X-Forwarded-For = %q, want the one resolved address", got)
	}
}

// The guard authorized one app and the handler reads the slug again. If the
// slug has moved to another app in between, the request is not served.
func TestProxyRefusesWhenTheSlugChangedHandsAfterTheGuard(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	app := &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/echo/", nil)
	req = req.WithContext(context.WithValue(req.Context(), guardedAppKey{}, "some-other-app"))
	rr := proxyTo(t, srv, app, req)

	if rr.Code != http.StatusNotFound || be.lastReq != nil {
		t.Errorf("status = %d, backend called = %v; want 404 and no call", rr.Code, be.lastReq != nil)
	}
}

func TestProxyStripsPrefixForMountedAtRootFrameworks(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)

	app := &database.App{
		Slug: "flasky", Name: "Flasky", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/flasky/reports/42", nil)
	proxyTo(t, srv, app, req)

	if got := be.lastReq.URL.Path; got != "/reports/42" {
		t.Errorf("forwarded path = %q, want /reports/42", got)
	}
	// The app still needs to know where it is mounted in order to build links.
	if got := be.lastReq.Header.Get("X-Forwarded-Prefix"); got != "/apps/flasky" {
		t.Errorf("X-Forwarded-Prefix = %q, want /apps/flasky", got)
	}
	if got := be.lastReq.Header.Get("X-Script-Name"); got != "/apps/flasky" {
		t.Errorf("X-Script-Name = %q, want /apps/flasky", got)
	}
}

func TestProxyKeepsPrefixForFrameworksBuiltWithBasePath(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)

	// Next.js resolves basePath at build time and expects the full path.
	app := &database.App{
		Slug: "nexty", Name: "Nexty", OwnerID: "u1",
		Framework: "nextjs", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/nexty/_next/static/chunk.js", nil)
	proxyTo(t, srv, app, req)

	if got := be.lastReq.URL.Path; got != "/apps/nexty/_next/static/chunk.js" {
		t.Errorf("forwarded path = %q, want the prefix preserved", got)
	}
}

func TestProxyDoesNotRewriteHTMLByDefault(t *testing.T) {
	srv := newTestServer(t)

	const page = `<a href="/reports">r</a><script>var data = {"href":"/api"}</script>`
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	})

	app := &database.App{
		Slug: "plain", Name: "Plain", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/plain/", nil))

	if got := rec.Body.String(); got != page {
		t.Errorf("body was rewritten:\n got: %s\nwant: %s", got, page)
	}
}

func TestProxyRewritesHTMLWhenLegacyOptInIsSet(t *testing.T) {
	srv := newTestServer(t)

	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="/reports">r</a>`))
	})

	app := &database.App{
		Slug: "legacy", Name: "Legacy", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
		LegacyPrefixRewrite: true,
	}

	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/legacy/", nil))

	if got := rec.Body.String(); !strings.Contains(got, `href="/apps/legacy/reports"`) {
		t.Errorf("legacy rewrite did not apply: %s", got)
	}
}

func TestProxyRewritesCookiePathsForPrefixedApps(t *testing.T) {
	srv := newTestServer(t)

	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "app_sess", Value: "x", Path: "/"})
		w.Write([]byte("ok"))
	})

	app := &database.App{
		Slug: "cookies", Name: "Cookies", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/cookies/", nil))

	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Path=/apps/cookies/") {
		t.Errorf("Set-Cookie = %q, want the app prefix in Path", setCookie)
	}
}

// The bug this pins: cookie confinement used to sit below ModifyResponse's
// keepPrefix early return, so the two frameworks most likely to be on an install
// were the two whose cookies escaped to the origin root. Both classes are walked
// here, because the fix is only correct if it is *unconditional* — a test on one
// framework would pass with the guard back in place.
func TestProxyConfinesCookiesForEveryFrameworkClass(t *testing.T) {
	for _, framework := range []string{"nextjs", "streamlit", "flask", "static"} {
		t.Run(framework, func(t *testing.T) {
			srv := newTestServer(t)

			be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "prefs", Value: "dark", Path: "/"})
				w.Write([]byte("ok"))
			})

			app := &database.App{
				Slug: "conf", Name: "Conf", OwnerID: "u1",
				Framework: framework, Port: database.Port(be.port),
				Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
			}

			rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/conf/", nil))

			setCookie := rec.Header().Get("Set-Cookie")
			if !strings.Contains(setCookie, "Path=/apps/conf/") {
				t.Errorf("Set-Cookie = %q, want Path confined to the app's prefix", setCookie)
			}
		})
	}
}

// A redirect is written without the base path on every framework, as the
// toft-sdk skill says. Next.js prefixes its links but not a route handler's
// `Location: /about`, which went to the install's root; and one that already
// carries the prefix — Next's own, or Flask's url_for — is not given it twice.
func TestProxyPrefixesARedirectOnEveryFrameworkClass(t *testing.T) {
	for _, framework := range []string{"nextjs", "streamlit", "flask"} {
		for loc, want := range map[string]string{
			"/about":             "/apps/redir/about",
			"/apps/redir/about":  "/apps/redir/about",
			"https://x.test/out": "https://x.test/out",
		} {
			t.Run(framework+loc, func(t *testing.T) {
				srv := newTestServer(t)
				be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", loc)
					w.WriteHeader(http.StatusFound)
				})
				app := &database.App{
					Slug: "redir", Name: "Redir", OwnerID: "u1",
					Framework: framework, Port: database.Port(be.port),
					Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
				}

				rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/redir/go", nil))

				if got := rec.Header().Get("Location"); got != want {
					t.Errorf("Location = %q, want %q", got, want)
				}
			})
		}
	}
}

// A cookie with no Path at all is given one, on every framework class. Without
// it the browser scopes the cookie to the request's own directory, which on a
// keepPrefix app is already /apps/{slug}/… — right by accident, and wrong the
// moment the app sets one from its root.
func TestProxyAddsAPathToACookieThatCarriesNone(t *testing.T) {
	srv := newTestServer(t)

	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "bare=1; HttpOnly")
		w.Write([]byte("ok"))
	})

	app := &database.App{
		Slug: "bare", Name: "Bare", OwnerID: "u1",
		Framework: "nextjs", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/bare/", nil))

	if got := rec.Header().Get("Set-Cookie"); !strings.Contains(got, "Path=/apps/bare/") {
		t.Errorf("Set-Cookie = %q, want a Path added", got)
	}
}

// RFC 6265 says the last Path wins, which is why rewriteCookiePath rewrites
// every one of them. The case is in the code's own comment; it was untested.
func TestProxyRewritesEveryPathAttributeNotJustTheFirst(t *testing.T) {
	srv := newTestServer(t)

	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "sneaky=1; Path=/x; Path=/api/v1")
		w.Write([]byte("ok"))
	})

	app := &database.App{
		Slug: "multi", Name: "Multi", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/multi/", nil))

	got := rec.Header().Get("Set-Cookie")
	if strings.Contains(got, "Path=/api/v1") {
		t.Errorf("Set-Cookie = %q, want no Path escaping to the platform's own", got)
	}
	if strings.Count(got, "Path=/apps/multi") != 2 {
		t.Errorf("Set-Cookie = %q, want both Path attributes confined", got)
	}
}

// The reserved prefix, enforced outbound. Path confinement already makes this
// harmless in the jar; the point is that `toft_` is claimed by the platform
// in both directions or it is not claimed at all.
func TestProxyDropsAppCookiesUnderTheReservedPrefix(t *testing.T) {
	srv := newTestServer(t)

	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "toft_session=forged; Path=/api/v1")
		w.Header().Add("Set-Cookie", "session=flask-keeps-this; Path=/")
		w.Write([]byte("ok"))
	})

	app := &database.App{
		Slug: "reserved", Name: "Reserved", OwnerID: "u1",
		Framework: "streamlit", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/reserved/", nil))

	all := strings.Join(rec.Header().Values("Set-Cookie"), "\n")
	if strings.Contains(all, "toft_session") {
		t.Errorf("Set-Cookie = %q, want the reserved-prefix cookie dropped", all)
	}
	// `session` was handed back to apps on purpose — it is Flask's default, and
	// stripping it was resetting every Flask app's session on every request.
	if !strings.Contains(all, "session=flask-keeps-this") {
		t.Errorf("Set-Cookie = %q, want the app's own `session` cookie kept", all)
	}
	if !strings.Contains(all, "Path=/apps/reserved/") {
		t.Errorf("Set-Cookie = %q, want the kept cookie confined", all)
	}
}

func TestProxyServesInterstitialForNonRunningApps(t *testing.T) {
	srv := newTestServer(t)

	for _, status := range []string{
		database.AppStatusBuilding,
		database.AppStatusFailed,
		database.AppStatusStopped,
		database.AppStatusCrashed,
	} {
		t.Run(status, func(t *testing.T) {
			app := &database.App{
				Slug: "app-" + status, Name: "App", OwnerID: "u1",
				Framework: "flask", Status: status,
				Visibility: database.VisibilityPublic,
			}

			rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/x/", nil))

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
		})
	}
}

// A redeploy is blue/green: while the new release builds, the one already
// serving keeps serving. `building` on the row is the deploy's state, and the
// gateway answering it with the building card turned every deploy into seconds
// of 503s. The card is for when there is nothing to serve — a first deploy, or
// a redeploy over a release that is not running.
func TestProxyKeepsServingThePreviousReleaseWhileBuilding(t *testing.T) {
	srv := newTestServer(t)

	t.Run("previous release serving", func(t *testing.T) {
		be := newBackend(t, nil)
		be.body = "old release"
		app := &database.App{
			Slug: "redeploying", Name: "Redeploying", OwnerID: "u1",
			Framework: "flask", Port: database.Port(be.port),
			CurrentRelease: "app-redeploying-d1",
			Status:         database.AppStatusBuilding, Visibility: database.VisibilityPublic,
		}
		rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/redeploying/", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "old release" {
			t.Errorf("got %d %q, want the previous release's 200", rec.Code, rec.Body.String())
		}
		if h := rec.Header().Get(interstitialHeader); h != "" {
			t.Errorf("%s = %q on a proxied response", interstitialHeader, h)
		}
	})

	t.Run("first deploy", func(t *testing.T) {
		app := &database.App{
			Slug: "first-build", Name: "First", OwnerID: "u1", Framework: "flask",
			Status: database.AppStatusBuilding, Visibility: database.VisibilityPublic,
		}
		rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/first-build/", nil))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(interstitialHeader) != stateBuilding.Key {
			t.Errorf("got %d %q, want 503 building", rec.Code, rec.Header().Get(interstitialHeader))
		}
	})

	t.Run("previous release not running", func(t *testing.T) {
		// A stopped app being redeployed: the row still names its release and
		// port, but nothing listens there. The build's card, not "not responding".
		be := newBackend(t, nil)
		port := be.port
		be.server.Close()
		app := &database.App{
			Slug: "was-stopped", Name: "Was stopped", OwnerID: "u1",
			Framework: "flask", Port: database.Port(port),
			CurrentRelease: "app-was-stopped-d1",
			Status:         database.AppStatusBuilding, Visibility: database.VisibilityPublic,
		}
		rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/was-stopped/", nil))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(interstitialHeader) != stateBuilding.Key {
			t.Errorf("got %d %q, want 503 building", rec.Code, rec.Header().Get(interstitialHeader))
		}
	})
}

// A static app's mount path must end in a slash, and the file server's own
// directory redirects must come back through the proxy with the app's prefix on
// them.
//
// The whole chain is exercised on purpose — the real internal/staticserve
// handler behind the real proxy handler — because the two halves are what make
// relative URLs work, and each is correct on its own while being useless
// without the other. Without the first, a browser at /apps/{slug} resolves
// href="styles.css" to /apps/styles.css, which is the platform's origin and not
// this app. Without the second, /apps/{slug}/guide redirects to /guide/, which
// is off the app entirely.
func TestStaticMountPathRedirectsAndKeepsItsPrefix(t *testing.T) {
	srv := newTestServer(t)

	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":       `<link href="styles.css">home`,
		"guide/index.html": "the guide",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := staticserve.Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	handler, err := staticserve.NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() { handler.Close() })

	be := newBackend(t, handler.ServeHTTP)
	app := &database.App{
		Slug: "site", Name: "Site", OwnerID: "u1",
		Framework: "static", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}

	// The mount path itself. 308 rather than 301 so the redirect is
	// method-preserving as well as permanent.
	rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/site", nil))
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("GET /apps/site = %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/apps/site/" {
		t.Errorf("Location = %q, want /apps/site/", loc)
	}

	// With the slash, the entry document is served at the directory path — not
	// redirected to its file name, which would put that name in every bookmark.
	rec = proxyServe(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/site/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "home") {
		t.Fatalf("GET /apps/site/ = %d, body %q", rec.Code, rec.Body.String())
	}

	// A subdirectory's redirect comes from the file server as "/guide/" and the
	// proxy's rewriteLocation is what puts the prefix back on.
	rec = proxyServe(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/site/guide", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /apps/site/guide = %d, want the file server's redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/apps/site/guide/" {
		t.Errorf("Location = %q, want /apps/site/guide/ — rewriteLocation is what adds the prefix", loc)
	}
}

// A Location is rewritten only when there is something to prefix: an absolute
// URL on the backend host, or an absolute path. A relative reference is what
// Django's HttpResponseRedirect("new/") emits verbatim, and the browser
// resolves it correctly against the prefixed URL it asked for; gluing the
// prefix on turned `/apps/desk/tickets/` + `new/` into `/apps/desknew/`.
func TestRewriteLocationLeavesRelativeReferencesAlone(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:3101")
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:3101/scores/?x=1", "/apps/desk/scores/?x=1"},
		{"/tickets/", "/apps/desk/tickets/"},
		{"/tickets/?page=2#top", "/apps/desk/tickets/?page=2#top"},
		{"new/", "new/"},
		{"?page=2", "?page=2"},
		{".", "."},
		{"../", "../"},
		{"#section", "#section"},
		{"https://elsewhere.example/x", "https://elsewhere.example/x"},
		// On the backend's host and already under the prefix — an app that
		// builds URLs from Host, which the proxy sets to the backend's — loses
		// the loopback host and is not prefixed twice (review #5).
		{"http://127.0.0.1:3101/apps/desk/done", "/apps/desk/done"},
		{"/apps/desk/done", "/apps/desk/done"},
		{"/apps/desk", "/apps/desk"},
		{"/apps/desktop", "/apps/desk/apps/desktop"},
		// Rebuilt from the escaped forms: a reserved character the app encoded
		// stays encoded, or the browser follows a different URL (code review
		// 2026-11-11, D-5).
		{"/files/a%3Fb%23c", "/apps/desk/files/a%3Fb%23c"},
		{"/files/a%2Fb?x=%2F#s%20t", "/apps/desk/files/a%2Fb?x=%2F#s%20t"},
		{"/files/a%2520b", "/apps/desk/files/a%2520b"},
	} {
		if got := rewriteLocation(tc.in, target, "/apps/desk"); got != tc.want {
			t.Errorf("rewriteLocation(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The legacy HTML rewrite decodes gzip and nothing else. A body under an
// encoding it did not decode goes back untouched, header intact — before this
// the header was deleted regardless, and brotli bytes were delivered labelled
// as plain HTML.
func TestLegacyRewriteLeavesAnEncodingItDidNotDecode(t *testing.T) {
	body := []byte("\x1b\x00brotli bytes href=\"/x\"")
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": {"br"}, "Content-Type": {"text/html"}},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
	rewriteHTMLBody(resp, "/apps/desk")
	if resp.Header.Get("Content-Encoding") != "br" {
		t.Errorf("Content-Encoding = %q, want br kept", resp.Header.Get("Content-Encoding"))
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body was rewritten under an encoding the rewrite cannot read: %q", got)
	}

	// And a plain body is still rewritten, so the hatch still works.
	plain := &http.Response{
		Header: http.Header{"Content-Type": {"text/html"}},
		Body:   io.NopCloser(strings.NewReader(`<a href="/x">`)),
	}
	rewriteHTMLBody(plain, "/apps/desk")
	got, _ = io.ReadAll(plain.Body)
	if string(got) != `<a href="/apps/desk/x">` {
		t.Errorf("plain body = %q", got)
	}
}

// A crashed shared app is restarting only while the agent still holds its
// workload. A failed start deletes it, and the card then said "restarting"
// over an app nothing would ever start again (TOFT-BB-003).
func TestCrashedSharedAppPromisesARestartOnlyWhileTheAgentHoldsIt(t *testing.T) {
	srv := newTestServer(t)
	mock := runtime.NewMockRuntime(nil)
	srv.rts = runtime.NewRuntimes(mock, nil)

	crashed := func(slug string) *database.App {
		return &database.App{
			Slug: slug, Name: "App", OwnerID: "u1", Framework: "flask",
			CurrentRelease: slug + "-r1", RunningMode: string(runtime.ModeShared),
			Status: database.AppStatusCrashed, Visibility: database.VisibilityPublic,
		}
	}

	t.Run("workload deleted by a failed start", func(t *testing.T) {
		app := crashed("gone")
		rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/gone/", nil))
		if got := rec.Header().Get(interstitialHeader); got != stateCrashed.Key {
			t.Errorf("card = %q, want %q", got, stateCrashed.Key)
		}
		if rec.Header().Get("Refresh") != "" {
			t.Error("a settled card must not refresh itself")
		}
	})

	t.Run("workload still held, the agent restarting it", func(t *testing.T) {
		app := crashed("looping")
		if _, err := mock.Deploy(runtime.WorkloadSpec{AppID: "x", Slug: app.Slug, Name: app.CurrentRelease}); err != nil {
			t.Fatal(err)
		}
		rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/looping/", nil))
		if got := rec.Header().Get(interstitialHeader); got != stateRestarting.Key {
			t.Errorf("card = %q, want %q", got, stateRestarting.Key)
		}
	})

	t.Run("workload held but stopped", func(t *testing.T) {
		app := crashed("held-stopped")
		if _, err := mock.Deploy(runtime.WorkloadSpec{AppID: "y", Slug: app.Slug, Name: app.CurrentRelease}); err != nil {
			t.Fatal(err)
		}
		if err := mock.Stop(app.CurrentRelease); err != nil {
			t.Fatal(err)
		}
		rec := proxyTo(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/held-stopped/", nil))
		if got := rec.Header().Get(interstitialHeader); got != stateCrashed.Key {
			t.Errorf("card = %q, want %q", got, stateCrashed.Key)
		}
	})
}

// A client's Connection header cannot name the platform's own headers away:
// the act-for blob and the session id are set after it is applied, so the app
// still receives "nobody in particular" for a stranger rather than nothing
// (code review 2026-11-11, D-1).
func TestAConnectionHeaderCannotStripTheActForBlob(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	app := &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.UpsertUser(&database.User{ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher}); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/echo/", nil)
	req.Header.Set("Connection", "X-Toft-Act-For, X-Toft-Session, X-Toft-Role")
	rec := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if be.lastReq == nil {
		t.Fatal("backend was not called")
	}
	if be.lastReq.Header.Get(auth.ActForHeader) == "" {
		t.Error("the anonymous act-for blob was stripped by the client's Connection header")
	}
}

// An encoded slash in an app's path reaches the app encoded: trimming the
// decoded path and clearing RawPath re-encoded the request line, so
// /files/a%2Fb arrived as /files/a/b and routed elsewhere (code review
// 2026-11-11, D-6).
func TestProxyKeepsAnEncodedSlashInThePath(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	app := &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/echo/files/a%2Fb%3Bc", nil)
	proxyTo(t, srv, app, req)
	if be.lastReq == nil {
		t.Fatal("backend was not called")
	}
	if got := be.lastReq.URL.RequestURI(); got != "/files/a%2Fb%3Bc" {
		t.Errorf("the app received %q, want /files/a%%2Fb%%3Bc", got)
	}
}

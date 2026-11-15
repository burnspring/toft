package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"toft/internal/config"
	"toft/internal/database"
)

// trustedCIDRs parses the CIDR list a test wants believed, in the same form
// TRUSTED_PROXY_CIDRS takes.
func trustedCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parse %q: %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

// X-Forwarded-Proto is what a deployed app builds its own absolute URLs from,
// and a WebSocket URL is the case where getting it wrong is fatal rather than
// cosmetic: `ws://` on an `https://` page is blocked by the browser as mixed
// content, before the handshake reaches the app.
//
// Caddy terminates TLS on this box and speaks plain HTTP to Toft, so the
// socket says http on a request the browser made over https. The forwarded
// value has to come from the header in that case — but only from a peer inside
// TRUSTED_PROXY_CIDRS, because otherwise any client could dictate what an app
// thinks its own URLs are.
// It runs through realIPMiddleware rather than straight at the handler, because
// that middleware rewrites the very field the scheme decision used to be taken
// from — see TestForwardedSchemeSurvivesRealIPRewrite, which is the case a
// handler-only test could not see.
func TestProxyForwardsBrowserSchemeFromTrustedProxyOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted []string
		xff     string
		header  string
		want    string
	}{
		{"trusted proxy reporting https", []string{"192.0.2.0/24"}, "", "https", "https"},
		{"trusted proxy reporting http", []string{"192.0.2.0/24"}, "", "http", "http"},
		// The same proxy, forwarding the client it is vouching for — which is
		// what a real one does, and what moves the peer address.
		{"trusted proxy declaring its client", []string{"192.0.2.0/24"}, "203.0.113.9", "https", "https"},
		// The peer is not vouched for, so its claim is worth nothing and the
		// socket is the only evidence there is.
		{"untrusted peer claiming https", []string{"10.0.0.0/8"}, "", "https", "http"},
		// ...and it cannot buy itself trust by naming a hop inside the trusted
		// range. Nothing about this request was believed, X-Forwarded-For least
		// of all.
		{"untrusted peer forging a trusted hop", []string{"10.0.0.0/8"}, "10.0.0.5", "https", "http"},
		// The default: no proxy configured at all, direct-to-the-internet.
		{"no trusted proxies configured", nil, "", "https", "http"},
		{"trusted proxy sending nothing", []string{"192.0.2.0/24"}, "", "", "http"},
		// A chain of proxies appends; the browser's scheme is leftmost.
		{"proxy chain", []string{"192.0.2.0/24"}, "", "https, http", "https"},
		// Never forward a value that is not a scheme — it lands in an app's URLs.
		{"junk value", []string{"192.0.2.0/24"}, "", "javascript:alert(1)", "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.cfg.TrustedProxyCIDRs = trustedCIDRs(t, tc.trusted...)
			be := newBackend(t, nil)

			app := &database.App{
				Slug: "wsapp", Name: "WS App", OwnerID: "u1",
				Framework: "flask", Port: database.Port(be.port),
				Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
			}

			// httptest.NewRequest uses 192.0.2.1:1234 as the peer.
			req := httptest.NewRequest(http.MethodGet, "/apps/wsapp/", nil)
			if tc.header != "" {
				req.Header.Set("X-Forwarded-Proto", tc.header)
			}
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			proxyThroughRealIP(t, srv, app, req)

			if be.lastReq == nil {
				t.Fatal("backend was not called")
			}
			if got := be.lastReq.Header.Get("X-Forwarded-Proto"); got != tc.want {
				t.Errorf("X-Forwarded-Proto forwarded to the app = %q, want %q", got, tc.want)
			}
		})
	}
}

// proxyThroughRealIP serves a request the way the router does: realIPMiddleware
// first — installed only when there are trusted networks, as router.go does it —
// and the proxy handler behind it.
//
// Handing a request straight to the handler is not the same thing, and the
// difference is not cosmetic: the middleware rewrites RemoteAddr and records
// what it found in the request context, so anything downstream that asks "did
// this arrive through our proxy" is answering a question about a field that has
// already moved.
func proxyThroughRealIP(t *testing.T, srv *Server, app *database.App, req *http.Request) {
	t.Helper()

	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(t, srv, app, r)
	})
	if len(srv.cfg.TrustedProxyCIDRs) > 0 {
		h = realIPMiddleware(srv.cfg.TrustedProxyCIDRs)(h)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// A trusted proxy that forwards its client — which is every real one; Caddy
// sets X-Forwarded-For by default — makes realIPMiddleware replace RemoteAddr
// with the *browser's* address before any handler runs. A scheme decision taken
// by re-testing RemoteAddr against TRUSTED_PROXY_CIDRS therefore answered
// "untrusted" for exactly the installs the header exists to serve, and handed
// every app `http` on an https page: `ws://`, mixed content, a blank Streamlit
// and nothing in the app's log.
//
// The two requests below differ only by X-Forwarded-For. A regression here
// makes them disagree, and the one carrying more information is the one that
// gets it wrong — which is why this is worth its own test rather than a row in
// the table above.
//
// It builds its own Server rather than calling newTestServer: the decision
// under test reads nothing but the config, and a guard that skips wherever
// TEST_DATABASE_URL is unset is not much of a guard.
func TestForwardedSchemeSurvivesRealIPRewrite(t *testing.T) {
	srv := &Server{cfg: &config.Config{
		TrustedProxyCIDRs: trustedCIDRs(t, "192.0.2.0/24"),
	}}

	scheme := func(xff string) string {
		var got string
		h := realIPMiddleware(srv.cfg.TrustedProxyCIDRs)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = srv.schemeFromRequest(r)
			}))

		req := httptest.NewRequest(http.MethodGet, "/apps/wsapp/", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	bare := scheme("")
	forwarded := scheme("203.0.113.9")
	if bare != "https" || forwarded != "https" {
		t.Errorf("scheme without X-Forwarded-For = %q, with = %q; want https for both",
			bare, forwarded)
	}
}

// The whole handshake, over a real socket: httptest.NewRecorder cannot be
// hijacked, so a recorder-based test proves nothing about upgrades. This one
// dials through the proxy and echoes a frame.
//
// It is here because the proxy does several things to a request that an upgrade
// does not survive by default — rewriting the path, stripping cookies, holding
// an idle-tracker lease for the life of the response — and because a WebSocket
// is exactly the traffic that must not be buffered (FlushInterval: -1) or
// counted as idle while it is open.
func TestProxyUpgradesWebSocketConnections(t *testing.T) {
	srv := newTestServer(t)

	upgrader := websocket.Upgrader{
		// The app is the origin's own server here; Toft's origin check for
		// its *own* streams is TestCheckWebSocketOriginRejectsForeignOrigins.
		CheckOrigin: func(*http.Request) bool { return true },
	}

	var (
		gotPath   string
		gotProto  string
		gotPrefix string
	)
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotPrefix = r.Header.Get("X-Forwarded-Prefix")

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("app failed to upgrade: %v", err)
			return
		}
		defer conn.Close()
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("app read: %v", err)
			return
		}
		if err := conn.WriteMessage(mt, append([]byte("echo:"), msg...)); err != nil {
			t.Errorf("app write: %v", err)
		}
	})

	// Loopback is the trusted proxy, standing in for Caddy: the dialer below
	// connects from 127.0.0.1, and the point is that the app is told the
	// browser was on https even though this hop is plaintext.
	srv.cfg.TrustedProxyCIDRs = trustedCIDRs(t, "127.0.0.1/32")

	app := &database.App{
		Slug: "socket", Name: "Socket", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	front := proxyServer(t, srv, app)

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/apps/socket/live"
	conn, resp, err := dialer.Dial(wsURL, http.Header{"X-Forwarded-Proto": {"https"}})
	if err != nil {
		status := "no response"
		if resp != nil {
			status = strconv.Itoa(resp.StatusCode)
		}
		t.Fatalf("dial through the proxy: %v (%s)", err, status)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != "echo:hello" {
		t.Errorf("round trip = %q, want %q", msg, "echo:hello")
	}

	// The upgrade went through the same rewriting as an ordinary request: a
	// Flask app is mounted at / and told its prefix, so the socket path it
	// registered is the unprefixed one.
	if gotPath != "/live" {
		t.Errorf("app saw path %q, want /live", gotPath)
	}
	if gotPrefix != "/apps/socket" {
		t.Errorf("app saw X-Forwarded-Prefix %q, want /apps/socket", gotPrefix)
	}
	// The reason this test carries the scheme assertion too: an app that builds
	// its socket URL from this header is the one that breaks when it is wrong,
	// and it breaks in the browser rather than here.
	if gotProto != "https" {
		t.Errorf("app saw X-Forwarded-Proto %q, want https — a ws:// URL on an https:// page is blocked as mixed content", gotProto)
	}
}

// An open socket holding the app awake is not tested here: it is the same
// s.idle.Begin lease around the same proxy.ServeHTTP that
// TestProxy_StreamingResponseHoldsTheAppAwake already covers, and an upgrade
// reaches it by the identical path.

// proxyServer puts the app proxy behind a real listener, which is what an
// upgrade needs: httptest.NewRecorder does not implement http.Hijacker, so a
// recorder-based test cannot tell a working upgrade from a broken one.
func proxyServer(t *testing.T, srv *Server, app *database.App) *httptest.Server {
	t.Helper()

	if err := srv.db.UpsertUser(&database.User{
		ID: app.OwnerID, Login: app.OwnerID, SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// realIPMiddleware in front, on the same condition router.go installs it —
	// the gateway's stack, not the handler on its own. What the proxy tells an
	// app about the browser's scheme is decided from what this middleware
	// recorded, so a front door without it would be testing a different program.
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("slug", app.Slug)
		srv.handleAppProxy(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
	})
	if len(srv.cfg.TrustedProxyCIDRs) > 0 {
		h = realIPMiddleware(srv.cfg.TrustedProxyCIDRs)(h)
	}

	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front
}

// A proxied socket is one request, authorized once at the handshake. It is
// re-authorized every socketRecheckInterval and ended on the first no: a viewer
// whose session is revoked loses the open socket, and an anonymous socket to a
// public app is left alone (security review 2026-11-15, HD-4).
func TestProxyRechecksAnOpenWebSocket(t *testing.T) {
	old := socketRecheckInterval
	socketRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() { socketRecheckInterval = old })

	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "viewer", database.SystemRoleViewer)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	})
	app := &database.App{
		Slug: "live", Name: "Live", OwnerID: "owner",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(srv.AppsRouter())
	t.Cleanup(front.Close)
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/apps/live/ws"

	dial := func(h http.Header) *websocket.Conn {
		t.Helper()
		conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial(wsURL, h)
		if err != nil {
			status := "no response"
			if resp != nil {
				status = strconv.Itoa(resp.StatusCode)
			}
			t.Fatalf("dial: %v (%s)", err, status)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	// alive is a round trip through the app after a few recheck intervals.
	// gorilla keeps a read error for good, so a timed-out read cannot be
	// followed by another: alive writes and reads its echo instead.
	alive := func(conn *websocket.Conn) bool {
		time.Sleep(4 * socketRecheckInterval)
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
			return false
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		return err == nil && string(msg) == "ping"
	}
	// ended is whether the tunnel closes within the window, read once.
	ended := func(conn *websocket.Conn, within time.Duration) bool {
		conn.SetReadDeadline(time.Now().Add(within))
		_, _, err := conn.ReadMessage()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false
		}
		return err != nil
	}

	conn := dial(http.Header{"Cookie": {appSessionFor(t, srv, "viewer")}})
	if !alive(conn) {
		t.Fatal("the viewer's socket ended with nothing revoked")
	}
	if err := srv.db.BumpTokenVersion("viewer"); err != nil {
		t.Fatal(err)
	}
	if !ended(conn, 2*time.Second) {
		t.Error("the viewer's session was revoked and the open socket kept serving")
	}

	// Anonymous on a public app: nothing to revoke, and the socket stays.
	if err := srv.db.SetAppVisibility(app.ID, database.VisibilityAuthenticated, database.VisibilityPublic, "system", "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	anon := dial(nil)
	if !alive(anon) {
		t.Error("an anonymous socket to a public app was ended")
	}
}

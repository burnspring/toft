package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The upgrader shipped with CheckOrigin returning true unconditionally. Since
// the deployed-apps gateway is same-site, a hostile app's page could open a log
// stream for any app the viewer could reach. originCheck does not cover the
// upgrade path. Finding 0.6.
func TestCheckWebSocketOriginRejectsForeignOrigins(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.BaseURL = "https://toft.example.com"

	for _, tc := range []struct {
		name, origin string
		want         bool
	}{
		{"the platform itself", "https://toft.example.com", true},
		{"platform with a trailing path", "https://toft.example.com/", true},
		{"case-insensitive host", "https://TOFT.example.com", true},
		{"the apps gateway", "https://toft.example.com:8081", false},
		{"an unrelated site", "https://evil.example", false},
		{"null origin", "null", false},
		// A browser always sends Origin on a WebSocket handshake, so its absence
		// means this is not the browser traffic the endpoint exists for.
		{"absent origin", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/x/logs/stream", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			if got := srv.checkWebSocketOrigin(req); got != tc.want {
				t.Errorf("checkWebSocketOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

// The Vite dev server is allowed only in the dev configurations that already
// allowlist it for ordinary requests.
func TestCheckWebSocketOriginAllowsViteOnlyInDev(t *testing.T) {
	prod := newTestServer(t)
	prod.cfg.BaseURL = "https://toft.example.com"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://localhost:5173")

	if prod.checkWebSocketOrigin(req) {
		t.Error("the Vite dev origin is accepted in a production configuration")
	}

	dev := newTestServer(t)
	dev.cfg = &config.Config{BaseURL: "http://localhost:8080"}
	if !dev.checkWebSocketOrigin(req) {
		t.Error("the Vite dev origin is rejected on a loopback install")
	}
}

// End to end through a real handshake: an authorized caller with a hostile
// Origin must be refused the upgrade.
func TestLogStreamHandshakeRejectsForeignOrigin(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "streamy", Name: "Streamy", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := srv.rts.Shared().Deploy(runtime.WorkloadSpec{
		AppID: app.ID, Slug: app.Slug, Name: app.ProcessName(), Framework: app.Framework,
	}); err != nil {
		t.Fatalf("register mock process: %v", err)
	}

	// Fix the platform origin before building the router: originCheck snapshots
	// the allowlist at construction. The Origin header a client sends is
	// independent of the host it dials, so this needs no real DNS.
	const platformOrigin = "http://platform.test"
	srv.cfg.BaseURL = platformOrigin

	ts := httptest.NewServer(srv.Router(http.NotFoundHandler()))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/apps/" + app.ID + "/logs/stream"
	cookie := sessionCookieFor(t, srv, "owner")

	dial := func(origin string) (*http.Response, error) {
		h := http.Header{}
		h.Set("Cookie", cookie.Name+"="+cookie.Value)
		if origin != "" {
			h.Set("Origin", origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, h)
		if conn != nil {
			conn.Close()
		}
		return resp, err
	}

	// The app gateway's origin — same site, different port — is the one this
	// defends against.
	resp, err := dial("http://apps.example.com")
	if err == nil {
		t.Fatal("handshake from a foreign origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}

	// The platform's own origin still works, or log streaming is broken.
	if _, err := dial(platformOrigin); err != nil {
		t.Errorf("handshake from the platform origin failed: %v", err)
	}
}

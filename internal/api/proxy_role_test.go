package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
)

// roleAt reads what the app was actually told, verifying it the way an SDK
// would: with the key this workload holds, against this app's id.
//
// Verifying rather than string-matching is the point. A test that asserted on
// the header's text would pass for a header the app could not verify, which is
// the failure mode that matters — an unverifiable assertion is `unknown` at the
// far end, and `unknown` refuses.
func roleAt(t *testing.T, srv *Server, be *backend, app *database.App) string {
	t.Helper()
	if be.lastReq == nil {
		t.Fatal("backend was not called")
	}
	key := srv.jwt.RoleKey(app.ID, app.CurrentRelease)
	return auth.VerifyRole(key, be.lastReq.Header.Get(auth.RoleHeader), app.ID, time.Now().UTC())
}

func roleTestApp(t *testing.T, srv *Server, be *backend, visibility string) *database.App {
	t.Helper()
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-role", Slug: "roles", Name: "Roles", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port), CurrentRelease: "roles-r1",
		Status: database.AppStatusRunning, Visibility: visibility,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return app
}

// A visitor cannot forge the header, and the reason it is *also* signed is that
// stripping only answers the visitor.
//
// Deployed apps bind 127.0.0.1 in shared mode and app-to-app isolation there is
// an accepted risk, so another app on the box can send whatever headers it likes
// straight to this app's port. A stripped-but-unsigned design and a
// signed-but-unstripped one fail differently and both fail, which is why this
// asserts both halves.
func TestAnInboundRoleHeaderIsStrippedOnEveryPath(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	be := newBackend(t, nil)
	app := roleTestApp(t, srv, be, database.VisibilityPublic)
	router := srv.AppsRouter()

	// Correctly signed for this app, by this platform — the strongest thing a
	// visitor could ever hold, because it is what the platform itself would
	// mint. It must still not survive: a valid blob obtained from somewhere
	// else must not arrive looking like *this* request's caller.
	stolen := srv.jwt.MintRole(auth.RoleOwner, app.ID, app.CurrentRelease, time.Now().UTC())

	paths := map[string]func(*http.Request){
		"anonymous":     func(r *http.Request) {},
		"bad app key":   func(r *http.Request) { r.Header.Set("X-API-Key", "toft_deadbeef") },
		"bad session":   func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "toft_app_session", Value: "nonsense"}) },
		"websocket ask": func(r *http.Request) { r.Header.Set("Connection", "Upgrade"); r.Header.Set("Upgrade", "websocket") },
		"browser navigate": func(r *http.Request) {
			r.Header.Set("Accept", "text/html")
		},
	}
	for name, decorate := range paths {
		t.Run(name, func(t *testing.T) {
			be.lastReq = nil
			req := httptest.NewRequest(http.MethodGet, "/apps/roles/data", nil)
			req.Header.Set(auth.RoleHeader, stolen)
			decorate(req)
			router.ServeHTTP(httptest.NewRecorder(), req)

			if be.lastReq == nil {
				// A path that refused before forwarding is a pass: nothing
				// reached the app, so nothing carried the stolen header.
				return
			}
			if got := be.lastReq.Header.Get(auth.RoleHeader); got == stolen {
				t.Errorf("the caller's own %s header reached the app", auth.RoleHeader)
			}
		})
	}
}

// Every forwarded request carries a role, and an anonymous visitor of a public
// app is told so — which is the bit the whole feature exists to give an app: it
// is what "refuse anonymous visitors" *is*.
func TestAnAnonymousVisitorArrivesAsAnonymous(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	be := newBackend(t, nil)
	app := roleTestApp(t, srv, be, database.VisibilityPublic)

	req := httptest.NewRequest(http.MethodGet, "/apps/roles/data", nil)
	srv.AppsRouter().ServeHTTP(httptest.NewRecorder(), req)

	if got := roleAt(t, srv, be, app); got != auth.RoleAnonymous {
		t.Errorf("role = %q, want %q", got, auth.RoleAnonymous)
	}
	// And the app can act for nobody-in-particular rather than acting as
	// itself, which is the other half of the same sentence.
	blob := be.lastReq.Header.Get(auth.ActForHeader)
	if blob == "" {
		t.Fatal("no act-for blob was minted for an anonymous visitor")
	}
	viewer, err := srv.jwt.VerifyActFor(blob, app.ID)
	if err != nil || !viewer.Anonymous {
		t.Errorf("the anonymous visitor's blob = %+v, %v — want an anonymous one", viewer, err)
	}
}

// The word travels with the request and dies with it, and that is the answer to
// the staleness objection the ADR reversal had to overturn: the lifetime is one
// request, which is strictly fresher than the sixty-second identity cache apps
// use for this today.
//
// A demotion between two requests reaches the app on the second one. Nothing is
// baked into a credential, and nothing is held between requests.
func TestARoleChangeReachesTheAppOnTheNextRequest(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	be := newBackend(t, nil)
	app := roleTestApp(t, srv, be, database.VisibilityPrivate)
	router := srv.AppsRouter()

	// A personal key is a person, authorized exactly as they would be in a
	// browser — the cheapest way to hold one identity across two requests.
	seedUser(t, srv.db, "reader", database.SystemRolePublisher)
	enablePersonalKeys(t, srv, true)
	secret := mintKey(t, srv, "reader", "cli", nil, false)

	ask := func() string {
		be.lastReq = nil
		req := httptest.NewRequest(http.MethodGet, "/apps/roles/data", nil)
		req.Header.Set("X-API-Key", secret)
		router.ServeHTTP(httptest.NewRecorder(), req)
		return roleAt(t, srv, be, app)
	}
	grant := func(role string) {
		t.Helper()
		if err := srv.db.UpsertGrant(context.Background(), app.ID,
			database.PrincipalTypeUser, "reader", role, "owner", database.RequestMeta{}); err != nil {
			t.Fatalf("granting %s: %v", role, err)
		}
	}

	grant(auth.RoleViewer)
	if got := ask(); got != auth.RoleViewer {
		t.Fatalf("role = %q, want %q", got, auth.RoleViewer)
	}

	grant(auth.RoleEditor)
	if got := ask(); got != auth.RoleEditor {
		t.Errorf("after a promotion the app was told %q, want %q — the role is read "+
			"live per request, not carried from the last one", got, auth.RoleEditor)
	}

	grant(auth.RoleViewer)
	if got := ask(); got != auth.RoleViewer {
		t.Errorf("after a demotion the app was told %q, want %q — a stale role is the "+
			"whole objection this design had to answer", got, auth.RoleViewer)
	}
}

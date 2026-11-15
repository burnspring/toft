package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// AppsRouter had no origin checking at all, so any site could POST to a deployed
// app and the browser would attach the viewer's app_session. Finding 0.7a — the
// one piece of 0.7 worth taking while app↔app separation stays an accepted risk.
func TestAppsRouterRejectsForeignOrigins(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	srv.cfg.BaseURL = "https://toft.example.com"
	srv.cfg.AppsBaseURL = "https://apps.example.com"

	be := newBackend(t, nil)
	app := &database.App{
		Slug: "target", Name: "Target", OwnerID: "owner",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	router := srv.AppsRouter()

	for _, tc := range []struct {
		name, origin string
		wantBlocked  bool
	}{
		{"an unrelated site", "https://evil.example", true},
		{"the platform origin", "https://toft.example.com", false},
		// The apps gateway's own origin must pass: browsers send Origin on
		// same-origin POSTs, so blocking it would break every app that posts to
		// itself. Note this also lets one app reach another — that is finding
		// 0.7, an accepted risk.
		{"the apps gateway itself", "https://apps.example.com", false},
		{"no origin at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/apps/target/submit", strings.NewReader("x=1"))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			blocked := rec.Code == http.StatusForbidden &&
				strings.Contains(rec.Body.String(), "cross-origin request rejected")
			if blocked != tc.wantBlocked {
				t.Errorf("origin %q: blocked = %v (status %d), want blocked = %v",
					tc.origin, blocked, rec.Code, tc.wantBlocked)
			}
		})
	}
}

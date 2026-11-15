package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The apps gateway answers a slug that resolves to nothing with 404, never with
// the platform's own document.
//
// It served the SPA for as long as both routers shared one guard, and that is
// two failures rather than one. The first is origin isolation: Toft's own
// HTML on the apps origin is exactly the shape the split exists to prevent. The
// second is worse because it looks like success — a deleted app's slug stops
// resolving, so `GET /apps/{gone}/health` answered 200 with a page of SPA, and
// anything asking whether the app was still being served was told yes by the
// wrong server.
//
// The platform origin keeps the other answer, and that is not an inconsistency:
// `/apps/new` and `/apps/{id}/{tab}` are SPA routes there.
func TestAppsGatewayDoesNotServeTheSPAForAnUnknownSlug(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body><div id="root"></div></body></html>`))
	})

	srv := newTestServer(t)
	srv.cfg.BaseURL = "https://toft.example.com"
	srv.cfg.AppsBaseURL = "https://apps.example.com"
	platform := srv.Router(spa)
	apps := srv.AppsRouter()

	for _, path := range []string{
		"/apps/never-existed-xyz/health",
		"/apps/never-existed-xyz/",
		"/apps/never-existed-xyz",
	} {
		rec := httptest.NewRecorder()
		apps.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("gateway %s: status = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("gateway %s served the SPA document: %q", path, rec.Body.String())
		}
	}

	// And the platform origin still falls through to the SPA, because an SPA
	// route is what `/apps/...` means there.
	rec := httptest.NewRecorder()
	platform.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apps/new", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("platform /apps/new: status %d, body %q; want the SPA",
			rec.Code, rec.Body.String())
	}
}

// A deleted app is the case the e2e suite caught (APP.9): the row goes, the
// slug stops resolving, and the gateway must stop serving it.
func TestAppsGatewayStopsServingADeletedApp(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.AppsBaseURL = "https://apps.example.com"

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "doomed", Name: "Doomed", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := srv.db.DeleteApp(app.ID); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/apps/doomed/health", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a deleted app is not served", rec.Code)
	}
}

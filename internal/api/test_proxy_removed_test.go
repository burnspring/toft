package api

import (
	"fmt"
	"net/http"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// GET /apps/{appID}/test fetched an arbitrary URL built by string concatenation
// (`http://127.0.0.1:%d%s`) and returned the status, headers, and full body to
// the caller. A path of `@169.254.169.254/latest/meta-data/...` made the
// intended host parse as *userinfo* and the attacker's host as the real target,
// giving a fully readable SSRF into cloud metadata and any internal service the
// host could reach. Verified live before removal.
//
// The route existed for debugging and was registered unconditionally, so it was
// deleted rather than hardened. This test keeps it deleted.
func TestTestProxyRouteIsGone(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	app := &database.App{
		Slug: "any", Name: "Any", OwnerID: "root", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// As an admin, who would pass every authorization check the handler had.
	for _, path := range []string{
		fmt.Sprintf("/api/v1/apps/%s/test", app.ID),
		fmt.Sprintf("/api/v1/apps/%s/test?path=@169.254.169.254/latest/meta-data/", app.ID),
	} {
		rec := getAs(t, srv, router, "root", path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 — the SSRF route is back", path, rec.Code)
		}
	}
}

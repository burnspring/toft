package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// BB-96: "visibility" on a JSON create was dropped without a word — 201 and a
// private app. It now goes through the upload create's gates, and a value
// that is not an audience is refused rather than ignored.
func TestAJSONCreateHonoursVisibility(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)

	rec := sendAs(t, srv, router, "pub", http.MethodPost, "/api/v1/apps",
		`{"name":"Vis","slug":"vis-json","slug_exact":true,"repo_url":"https://github.com/acme/vis","visibility":"authenticated"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body)
	}
	var body struct {
		App struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"app"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.App.Visibility != database.VisibilityAuthenticated {
		t.Errorf("answered visibility = %q", body.App.Visibility)
	}
	if got, _ := srv.db.GetApp(body.App.ID); got == nil || got.Visibility != database.VisibilityAuthenticated {
		t.Errorf("stored visibility = %+v", got)
	}

	rec = sendAs(t, srv, router, "pub", http.MethodPost, "/api/v1/apps",
		`{"name":"Bad","slug":"vis-bad","slug_exact":true,"repo_url":"https://github.com/acme/vis","visibility":"everyone"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown audience = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if a, _ := srv.db.GetAppBySlug("vis-bad"); a != nil {
		t.Error("a refused create left an app behind")
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The failure this guards is silent: with the hiding left in, a guide an
// earlier version installed is invisible in its owner's app list, so they
// cannot find it to delete it — and nothing anywhere fails.
func TestAnAppOnTheReservedNamespaceIsListedLikeAnyOther(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	app := e.installExtension(t, "admin-user", "1.4.0")
	app.Slug = "_user-guide" // through the database: no creation path reaches `_`
	if err := e.srv.db.UpdateApp(app); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}

	rec := e.do(t, "admin-user", http.MethodGet, "/api/v1/apps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps = %d", rec.Code)
	}
	var listed []struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		var wrapped struct {
			Apps []struct {
				Slug string `json:"slug"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wrapped); err != nil {
			t.Fatalf("decoding /apps: %v", err)
		}
		listed = wrapped.Apps
	}
	for _, a := range listed {
		if a.Slug == "_user-guide" {
			return
		}
	}
	t.Errorf("an app on _user-guide is missing from its owner's list: %s", rec.Body.String())
}

// Also silent: a first-party branch half-removed would still mint `_user-guide`
// from a catalog document naming the old guide's repository. Now nothing does —
// it is an ordinary extension on an ordinary slug.
func TestACatalogItemNamingAnOldGuideIsAnOrdinaryExtension(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	e.publish(t, `{"schema_version":1,"items":[
	  {"id":"guide-1","kind":"extension","name":"Toft User Guide","version":"2.1.0",
	   "suggested_slug":"_user-guide",
	   "repo_url":"https://github.com/burnspring/toft-ext-user-guide","ref":"v2.1.0"}]}`)

	for i := 0; i < 2; i++ {
		rec := e.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/guide-1/deploy", "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("deploy %d = %d: %s, want an ordinary install, never already_installed", i+1, rec.Code, rec.Body.String())
		}
		var body struct {
			App database.App `json:"app"`
		}
		json.Unmarshal(rec.Body.Bytes(), &body)
		if strings.HasPrefix(body.App.Slug, "_") {
			t.Fatalf("deploy %d landed on the reserved namespace: %q", i+1, body.App.Slug)
		}
	}
}

// Where documentation links point is configuration, and the settings API says
// so in its own words rather than sending the reader to the gallery's variable.
func TestTheDocsURLIsNotASetting(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	rec := e.do(t, "admin-user", http.MethodPut, "/api/v1/system/settings",
		`{"docs_url":"https://evil.example.com/"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT docs_url = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DOCS_URL") || strings.Contains(body, "GALLERY_CATALOG_URL") {
		t.Errorf("the refusal should name DOCS_URL and only it: %s", body)
	}
}

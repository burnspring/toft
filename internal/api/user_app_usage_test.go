package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// What a person has been opening: admins only, 404 to anyone else, only apps
// that still exist, only the person's own traffic, most recent first, and the
// window honoured.
func TestUserAppUsage(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)
	seedUser(t, srv.db, "other", database.SystemRolePublisher)

	mk := func(slug string) *database.App {
		a := &database.App{
			Slug: slug, Name: slug, OwnerID: "other",
			Framework: "flask", Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
		}
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		return a
	}
	recent, older, gone, ancient := mk("recent"), mk("older"), mk("gone"), mk("ancient")

	seedUsage(t, srv.db, recent.ID, "pub", 1, 3)
	seedUsage(t, srv.db, recent.ID, "pub", 0, 2)
	seedUsage(t, srv.db, older.ID, "pub", 10, 50)
	seedUsage(t, srv.db, gone.ID, "pub", 0, 9)
	seedUsage(t, srv.db, ancient.ID, "pub", 200, 1)
	seedUsage(t, srv.db, older.ID, "other", 0, 99) // somebody else's
	seedUsage(t, srv.db, older.ID, database.UsageKeyPrefix+"k1", 0, 7)
	if err := srv.db.DeleteApp(gone.ID); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	if rec := getAs(t, srv, router, "pub", "/api/v1/users/other/app-usage"); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin: %d, want 404", rec.Code)
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/users/no-such/app-usage"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing user: %d, want 404", rec.Code)
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/users/pub/app-usage?days=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("days=0: %d, want 400", rec.Code)
	}

	read := func(q string) []userAppUsageEntry {
		t.Helper()
		rec := getAs(t, srv, router, "root", "/api/v1/users/pub/app-usage"+q)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin%s: %d %s", q, rec.Code, rec.Body.String())
		}
		var body struct {
			Apps []userAppUsageEntry `json:"apps"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Apps
	}

	got := read("")
	if len(got) != 2 || got[0].Slug != "recent" || got[1].Slug != "older" {
		t.Fatalf("default window: %+v, want recent then older — the deleted app and the 200-day-old one absent", got)
	}
	if got[0].Requests != 5 || got[0].Days != 2 || got[1].Requests != 50 {
		t.Fatalf("counts are this person's alone: %+v", got)
	}
	if got := read("?days=365"); len(got) != 3 || got[2].Slug != "ancient" {
		t.Fatalf("a year: %+v, want the ancient app last", got)
	}
}

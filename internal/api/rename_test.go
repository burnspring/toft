package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// Renaming a group and renaming a storage resource, which are the same feature
// wearing two shapes and fail in different places.
//
// A group's name is `UNIQUE`, so its interesting case is the collision — and it
// has to come back as a refusal somebody can act on rather than as a 500, which
// is what an opaque constraint error had always produced on the create beside
// it. A resource's name is not unique and its slug is not editable, so its
// interesting case is a client that tries anyway: the slug is the stem every
// mounted binding is named after, and quietly dropping the field would let an
// administrator believe they had moved an address that a live mount still
// resolves by the old one.

func patchAs(t *testing.T, srv *Server, router http.Handler, who, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRenameGroup(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	rec := patchAs(t, srv, router, "root", "/api/v1/groups/"+groupID, `{"name":"Platform"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got, err := srv.db.GetGroup(groupID)
	if err != nil || got == nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if got.Name != "Platform" {
		t.Errorf("name = %q, want %q", got.Name, "Platform")
	}

	// The id is what every grant and owned app points at, and it does not move.
	if got.ID != groupID {
		t.Errorf("the id changed with the name: %q", got.ID)
	}

	// Trimmed, so the two doors into the column cannot disagree about what
	// "  Platform " is.
	if rec := patchAs(t, srv, router, "root", "/api/v1/groups/"+groupID,
		`{"name":"  Spaced  "}`); rec.Code != http.StatusOK {
		t.Fatalf("rename with padding = %d (%s)", rec.Code, rec.Body.String())
	}
	if got, _ := srv.db.GetGroup(groupID); got == nil || got.Name != "Spaced" {
		t.Errorf("name = %q, want %q", got.Name, "Spaced")
	}
}

func TestRenameGroupRefusals(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	other, err := srv.db.CreateGroup(t.Context(), "finance", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	// The collision, and it must be a 409 carrying words — not the 500 an
	// unwrapped constraint error produced.
	rec := patchAs(t, srv, router, "root", "/api/v1/groups/"+groupID, `{"name":"finance"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("renaming onto a taken name = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	var body refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Errorf("no message on the refusal: %s", rec.Body.String())
	}
	if got, _ := srv.db.GetGroup(groupID); got == nil || got.Name != "platform-team" {
		t.Errorf("the refused rename was written anyway: %+v", got)
	}
	if got, _ := srv.db.GetGroup(other.ID); got == nil || got.Name != "finance" {
		t.Errorf("the other group moved: %+v", got)
	}

	// A group with no name cannot be picked out of a list or told from another
	// one — the id is not a name — so blank is refused rather than stored.
	if rec := patchAs(t, srv, router, "root", "/api/v1/groups/"+groupID,
		`{"name":"   "}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("renaming to blank = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	// Not an administrator.
	if rec := patchAs(t, srv, router, "insider", "/api/v1/groups/"+groupID,
		`{"name":"mine"}`); rec.Code != http.StatusForbidden {
		t.Errorf("a publisher renamed a group: %d (%s)", rec.Code, rec.Body.String())
	}

	// Creating onto a taken name is the same answer, from the door beside it.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groups", strings.NewReader(`{"name":"finance"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, srv, "root"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("creating a duplicate group = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// storageRenameEnv is storageTestEnv with a resource already provisioned. The
// rename touches no cluster, so the fake provisioner is here only to make the
// routes exist at all.
func storageRenameEnv(t *testing.T) (*Server, http.Handler, *database.StorageResource) {
	t.Helper()

	srv, router, _ := storageTestEnv(t, &fakeProvisioner{})
	res := &database.StorageResource{
		Slug: "reports", Name: "reports", Kind: database.StorageKindVolume,
		Description: "the first thing anybody wrote here",
	}
	if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return srv, router, res
}

func TestRenameStorageResourceRefusesTheName(t *testing.T) {
	srv, router, res := storageRenameEnv(t)

	rec := patchAs(t, srv, router, "root", "/api/v1/admin/storage/"+res.ID,
		`{"name":"reports","slug":"somewhere-else"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sending a slug = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	// Refused by name rather than dropped: the whole point is that the
	// administrator finds out, and a 200 with the name changed and the slug
	// silently ignored is the outcome this exists to prevent.
	if !strings.Contains(rec.Body.String(), "cannot be changed") {
		t.Errorf("the refusal does not say what was wrong: %s", rec.Body.String())
	}
	after, err := srv.db.GetStorageResource(res.ID)
	if err != nil || after == nil {
		t.Fatalf("GetStorageResource: %v", err)
	}
	if after.Slug != res.Slug {
		t.Errorf("slug = %q, want %q", after.Slug, res.Slug)
	}
	if after.Name != res.Name {
		t.Errorf("the name was written by a request that was refused: %q", after.Name)
	}
}

func TestRenameStorageResource(t *testing.T) {
	srv, router, res := storageRenameEnv(t)

	rec := patchAs(t, srv, router, "root", "/api/v1/admin/storage/"+res.ID,
		`{"description":"Finance owns this. Ask before deleting."}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	after, err := srv.db.GetStorageResource(res.ID)
	if err != nil || after == nil {
		t.Fatalf("GetStorageResource: %v", err)
	}
	// The name is the identifier and does not move (ADR §71).
	if after.Name != "reports" {
		t.Errorf("name = %q", after.Name)
	}
	if after.Description != "Finance owns this. Ask before deleting." {
		t.Errorf("description = %q", after.Description)
	}
	// Nothing about where it lives moved — this is a label and the claim is the
	// identity.
	if after.Slug != res.Slug || after.ClaimName != res.ClaimName {
		t.Errorf("the binding moved: slug %q claim %q", after.Slug, after.ClaimName)
	}

	// Clearing the description is a real edit and there has to be a way to make
	// it, which is why the field is a pointer.
	if rec := patchAs(t, srv, router, "root", "/api/v1/admin/storage/"+res.ID,
		`{"description":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing the description = %d (%s)", rec.Code, rec.Body.String())
	}
	after, _ = srv.db.GetStorageResource(res.ID)
	if after.Description != "" {
		t.Errorf("description = %q, want empty", after.Description)
	}
	if after.Name != "reports" {
		t.Errorf("clearing the description moved the name: %q", after.Name)
	}

	// A blank name is a change of name like any other, and an empty body has
	// nothing to do.
	if rec := patchAs(t, srv, router, "root", "/api/v1/admin/storage/"+res.ID,
		`{"name":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("renaming to blank = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if rec := patchAs(t, srv, router, "root", "/api/v1/admin/storage/"+res.ID,
		`{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty patch = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

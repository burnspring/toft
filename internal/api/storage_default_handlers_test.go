package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

func newResource(t *testing.T, srv *Server, slug string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{Name: slug, Slug: slug, Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/" + slug}
	if err := srv.db.CreateStorageResource(res, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return res
}

func managersOf(t *testing.T, srv *Server, res *database.StorageResource) map[string]database.StorageManager {
	t.Helper()
	ms, err := srv.db.ListStorageManagers(res.ID)
	if err != nil {
		t.Fatalf("ListStorageManagers: %v", err)
	}
	out := map[string]database.StorageManager{}
	for _, m := range ms {
		out[m.SubjectID] = m
	}
	return out
}

// A resource created with defaults present is delegated to each, as ordinary
// rows of origin default — revocable on that resource alone (D15).
func TestACreatedResourceIsDelegatedToEachDefault(t *testing.T) {
	srv, router, _, _ := delegateEnv(t)
	g, err := srv.db.CreateGroup(t.Context(), "deployers", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	// A claim-mapped group may hold a default: a default is a delegation.
	if _, err := srv.db.Conn().Exec(`INSERT INTO group_claim_mappings (group_id, provider_id, claim_value) VALUES (?, 'sso', 'deployers')`, g.ID); err != nil {
		t.Fatalf("mapping the group: %v", err)
	}
	for _, body := range []string{
		`{"subject_type":"group","subject_id":"` + g.ID + `","max_access":"full"}`,
		`{"subject_type":"user","subject_id":"delegate","max_access":"read"}`,
	} {
		if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "root", body); rec.Code != 200 {
			t.Fatalf("PUT default = %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "root",
		`{"subject_type":"user","subject_id":"delegate","max_access":"write"}`); rec.Code != 400 {
		t.Errorf("a default at write = %d, want 400 — read and full are the levels every kind has", rec.Code)
	}
	if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "owner",
		`{"subject_type":"user","subject_id":"owner","max_access":"full"}`); rec.Code == 200 {
		t.Errorf("a non-administrator set a default")
	}

	res := newResource(t, srv, "fresh")
	ms := managersOf(t, srv, res)
	if ms[g.ID].Origin != database.DelegationOriginDefault || ms[g.ID].MaxAccess != "full" ||
		ms["delegate"].Origin != database.DelegationOriginDefault || ms["delegate"].MaxAccess != "read" {
		t.Fatalf("managers on a new resource = %+v, want both defaults, origin default", ms)
	}
}

// Adding a default offers the existing resources, and leaves alone a ceiling
// an administrator chose per resource (D17).
func TestApplyExistingSkipsAndReportsADifferingCeiling(t *testing.T) {
	srv, router, _, reports := delegateEnv(t)
	other := newResource(t, srv, "other")
	delegate(t, srv, other.ID, "delegate", "read")

	rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "root",
		`{"subject_type":"user","subject_id":"delegate","max_access":"full","apply_existing":true}`)
	if rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Applied   []string                    `json:"applied"`
		Differing []database.DefaultDiffering `json:"differing"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Applied) != 1 || body.Applied[0] != reports.Slug {
		t.Errorf("applied = %v, want [%s]", body.Applied, reports.Slug)
	}
	if len(body.Differing) != 1 || body.Differing[0].Resource != "other" || body.Differing[0].MaxAccess != "read" {
		t.Errorf("differing = %+v, want other at read", body.Differing)
	}
	if m := managersOf(t, srv, other)["delegate"]; m.MaxAccess != "read" || m.Origin != database.DelegationOriginAdmin {
		t.Errorf("the per-resource choice moved: %+v", m)
	}
}

// Removing a default revokes nothing by itself; revoke_existing collects only
// the rows a default wrote, and names the apps it restarts (D17).
func TestRemovingADefaultRevokesOnlyWhenAskedAndOnlyItsOwn(t *testing.T) {
	srv, router, app, reports := delegateEnv(t)
	if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "root",
		`{"subject_type":"user","subject_id":"delegate","max_access":"full"}`); rec.Code != 200 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	defaulted := newResource(t, srv, "defaulted")
	delegate(t, srv, reports.ID, "delegate", "full") // the administrator's own, origin admin
	m := &database.StorageMapping{ResourceID: defaulted.ID, AppID: app.ID, Path: "/data/d", Access: "read",
		Authority: database.AuthorityKey(database.StoreSubjectUser, "delegate")}
	if err := srv.db.PutStorageMapping(m, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	// Spelled as the SPA sends it, percent-encoded (code review 2026-11-11,
	// N-1): the colon arrives as %3A and still cuts.
	if rec := storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/defaults/user%3Adelegate", "root", ""); rec.Code != 200 {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body.String())
	}
	if len(managersOf(t, srv, defaulted)) != 1 {
		t.Fatal("removing a default revoked its delegations without being asked")
	}

	if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "root",
		`{"subject_type":"user","subject_id":"delegate","max_access":"full"}`); rec.Code != 200 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	rec := storageRequest(t, srv, router, http.MethodDelete,
		"/api/v1/admin/storage/defaults/user:delegate?revoke_existing=true", "root", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"slug":"demo"`) {
		t.Fatalf("DELETE revoke_existing = %d %s, want the app named", rec.Code, rec.Body.String())
	}
	if len(managersOf(t, srv, defaulted)) != 0 {
		t.Error("revoke_existing left the default's delegation")
	}
	if m := managersOf(t, srv, reports)["delegate"]; m.Origin != database.DelegationOriginAdmin {
		t.Error("revoke_existing took an administrator's own delegation")
	}
}

// The create answers with the row as committed: the default delegations its
// own transaction wrote are in the counts, not the zero values of the struct
// built before the insert (BB-47).
func TestTheCreateAnswerCountsTheDefaultDelegations(t *testing.T) {
	srv, router, _, _ := delegateEnv(t)
	if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/defaults", "root",
		`{"subject_type":"user","subject_id":"delegate","max_access":"full"}`); rec.Code != 200 {
		t.Fatalf("PUT default = %d %s", rec.Code, rec.Body.String())
	}
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"xteam","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.ManagerUsers != 1 || res.ManagerGroups != 0 {
		t.Errorf("create answered manager_users=%d manager_groups=%d, want 1 and 0", res.ManagerUsers, res.ManagerGroups)
	}
}

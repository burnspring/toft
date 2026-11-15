package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// BB-048, fifth 2026-11-03 black-box list: a volume attached whole and then by
// folder to one app was accepted, and every start after it failed. The second
// attach is refused by code, with a hint, and nothing is stored.
func TestANestedMappingIsRefusedAtAttach(t *testing.T) {
	srv, router, app := storageTestEnv(t, &fakeProvisioner{})
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"vol-a","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)
	url := "/api/v1/admin/storage/" + res.ID + "/mappings/" + app.ID

	if rec = storageRequest(t, srv, router, http.MethodPut, url, "root", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("whole = %d: %s", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodPut, url, "root", `{"folder":"f"}`)
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body["code"] != "storage_path_taken" || body["hint"] == "" ||
		!strings.Contains(body["error"], "nests with") {
		t.Fatalf("folder over whole = %d: %s, want 409 storage_path_taken with a hint", rec.Code, rec.Body.String())
	}
	if got, _ := srv.db.ListStorageMappingsForApp(app.ID); len(got) != 1 {
		t.Errorf("mappings = %d, want the whole one alone", len(got))
	}
	// Re-putting the same mapping is not a conflict with itself.
	if rec = storageRequest(t, srv, router, http.MethodPut, url, "root", `{}`); rec.Code != http.StatusOK {
		t.Errorf("re-put whole = %d: %s", rec.Code, rec.Body.String())
	}
	// A folder at a path of its own sits beside it.
	if rec = storageRequest(t, srv, router, http.MethodPut, url, "root", `{"folder":"f","path":"/other/f"}`); rec.Code != http.StatusOK {
		t.Errorf("folder beside = %d: %s", rec.Code, rec.Body.String())
	}
}

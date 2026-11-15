package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The three refusals an agent probed its way round (porting feedback
// 2026-11-03): a resource's name where its id belongs, a field the route does
// not take, and a create missing more than one field.
func TestStorageRoutesSayWhatWasWrongWithTheRequest(t *testing.T) {
	srv, router, app := storageTestEnv(t, &fakeProvisioner{})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root", `{}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "name and kind") {
		t.Errorf("an empty create = %d %s, want both fields named", rec.Code, rec.Body)
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"frs","kind":"volume","size":"10Gi"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `\"size\" is not a field`) ||
		!strings.Contains(rec.Body.String(), "storage_root") {
		t.Errorf("an unknown field = %d %s, want it and the accepted ones named", rec.Code, rec.Body)
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"frs","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/frs/mappings/"+app.ID, "root",
		`{"path":"/data/frs","access":"read"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), res.ID) ||
		!strings.Contains(rec.Body.String(), "storage_not_found") {
		t.Errorf("a name for the id = %d %s, want the id named", rec.Code, rec.Body)
	}
	rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"mount":"/data/frs","access":"read"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "path, access, folder") {
		t.Errorf("a wrong body = %d %s, want the fields named", rec.Code, rec.Body)
	}
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

func partitionedEnv(t *testing.T, perApp bool) (*Server, http.Handler, *database.App) {
	t.Helper()
	srv, router, app := storageTestEnv(t, &rootProvisioner{
		backingProvisioner: backingProvisioner{
			fakeProvisioner: fakeProvisioner{kinds: []string{runtime.MountVolume}},
			backings:        map[string][]string{runtime.MountVolume: {runtime.BackingHost}},
		},
		roots: []runtime.StorageRoot{{Name: "data", Path: "/srv/data"}},
	})
	srv.cfg.AppUserPerApp = perApp
	return srv, router, app
}

// A partitioned resource is offered only where a folder's group can keep an app
// out, which is an install whose apps run under accounts of their own. Where
// they do not, it is refused by name rather than created and not enforced.
func TestAPartitionedResourceNeedsAccountsPerApp(t *testing.T) {
	srv, router, _ := partitionedEnv(t, false)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "APP_USER_PER_APP") {
		t.Errorf("create = %d %s, want 422 naming APP_USER_PER_APP", rec.Code, rec.Body.String())
	}
	// Whole is unaffected.
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"whole","kind":"volume","backing":"host","storage_root":"data"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("a whole resource = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// Fixed at creation: a rename naming the flag is refused by name, the way the
// slug is, rather than having the field silently dropped.
func TestPartitionedIsFixedAtCreate(t *testing.T) {
	srv, router, _ := partitionedEnv(t, true)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.Partitioned {
		t.Fatal("the created resource is not partitioned")
	}
	rec = storageRequest(t, srv, router, http.MethodPatch, "/api/v1/admin/storage/"+res.ID, "root",
		`{"name":"share","partitioned":false}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "partitioned is fixed") {
		t.Errorf("PATCH partitioned = %d %s, want 400 naming the field", rec.Code, rec.Body.String())
	}
}

// The two folder rules answer with what is in the way, on the administrator's
// attach route; the delegate's goes through the same helper.
func TestAPartitionedMappingNamesAFolderThatOverlapsNothing(t *testing.T) {
	srv, router, app := partitionedEnv(t, true)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`)
	var res database.StorageResource
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	other := &database.App{Slug: "other", Name: "Other", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateAppTx(other, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	put := func(appID, body string) *httptest.ResponseRecorder {
		return storageRequest(t, srv, router, http.MethodPut,
			"/api/v1/admin/storage/"+res.ID+"/mappings/"+appID, "root", body)
	}

	if rec := put(app.ID, `{"path":"/data/share","access":"full"}`); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "mapped by folder") {
		t.Errorf("whole mapping = %d %s, want 422 asking for a folder", rec.Code, rec.Body.String())
	}
	if rec := put(app.ID, `{"path":"/data/a","access":"full","folder":"teams/a"}`); rec.Code != http.StatusOK {
		t.Fatalf("folder mapping = %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(other.ID, `{"path":"/data/a","access":"full","folder":"teams/a"}`); rec.Code != http.StatusOK {
		t.Errorf("sharing a folder = %d %s, want 200", rec.Code, rec.Body.String())
	}
	rec = put(other.ID, `{"path":"/data/t","access":"full","folder":"teams"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `overlaps \"teams/a\"`) ||
		!strings.Contains(rec.Body.String(), "Demo") {
		t.Errorf("overlap = %d %s, want 422 naming teams/a and the app on it", rec.Code, rec.Body.String())
	}

	// A delegate is told the folder in the way and not the apps on it, which
	// they may not be able to read.
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/apps/"+other.ID+"/storage/"+res.ID, "owner",
		`{"path":"/data/t","access":"full","folder":"teams"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `overlaps \"teams/a\"`) ||
		strings.Contains(rec.Body.String(), "Demo") {
		t.Errorf("delegate overlap = %d %s, want 422 naming teams/a and no app", rec.Code, rec.Body.String())
	}
}

// Both caps answer 422 with the sentence, on the create and on the attach,
// rather than a 500 from a database refusal.
func TestTheStorageCapsAnswerWithTheCap(t *testing.T) {
	srv, router, app := partitionedEnv(t, true)
	var share database.StorageResource
	for i := 0; i < database.MaxStorageResources; i++ {
		rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
			fmt.Sprintf(`{"name":"share-%d","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d = %d %s", i, rec.Code, rec.Body.String())
		}
		if i == 0 {
			_ = json.Unmarshal(rec.Body.Bytes(), &share)
		}
	}
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"one-more","kind":"volume","backing":"host","storage_root":"data"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at most 25 storage resources") {
		t.Errorf("26th create = %d %s, want 422 naming the cap", rec.Code, rec.Body.String())
	}

	for i := 0; i <= database.MaxAppStorageMappings; i++ {
		rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+share.ID+"/mappings/"+app.ID, "root",
			fmt.Sprintf(`{"path":"/data/f%d","access":"full","folder":"f%d"}`, i, i))
	}
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at most 5 storage mappings") {
		t.Errorf("6th mapping = %d %s, want 422 naming the cap", rec.Code, rec.Body.String())
	}
}

// API-PROXY-3: the binding was made before the mapping cap was checked, and a
// refused row never collected it — one cluster object per refused call. The
// attach past the cap now creates nothing.
func TestAnAttachPastTheMappingCapBindsNothing(t *testing.T) {
	srv, router, app := partitionedEnv(t, true)
	prov := srv.storage.(*rootProvisioner)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`)
	var share database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &share); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < database.MaxAppStorageMappings; i++ {
		rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+share.ID+"/mappings/"+app.ID, "root",
			fmt.Sprintf(`{"path":"/data/f%d","access":"full","folder":"f%d"}`, i, i))
		if rec.Code != http.StatusOK {
			t.Fatalf("mapping %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	prov.mu.Lock()
	before := len(prov.ensured)
	prov.mu.Unlock()
	for i := 0; i < 3; i++ {
		rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+share.ID+"/mappings/"+app.ID, "root",
			fmt.Sprintf(`{"path":"/data/x%d","access":"full","folder":"x%d"}`, i, i))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attach past the cap = %d %s, want 422", rec.Code, rec.Body.String())
		}
	}
	prov.mu.Lock()
	after := len(prov.ensured)
	prov.mu.Unlock()
	if after != before {
		t.Fatalf("refused attaches ensured %d binding(s)", after-before)
	}
	// An existing mapping is not new, so it may be re-put at the cap — here
	// moved to another path (a read-write root serves full and nothing else,
	// so the level cannot be the thing that changes).
	rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+share.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/f0-moved","access":"full","folder":"f0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-put at the cap = %d %s", rec.Code, rec.Body.String())
	}
}

// The inventory carries the caps and whether partitioning is offered under a
// host mount, so the page's "N of 25" and its create form read the server's
// numbers rather than copies of them.
func TestTheInventoryCarriesTheCapsAndThePartitionOffer(t *testing.T) {
	for _, perApp := range []bool{true, false} {
		srv, router, _ := partitionedEnv(t, perApp)
		rec := storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "root", "")
		var got struct {
			MaxResources     int  `json:"max_resources"`
			MaxAppMappings   int  `json:"max_app_mappings"`
			PartitionedRoots bool `json:"partitioned_roots"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		if got.MaxResources != database.MaxStorageResources || got.MaxAppMappings != database.MaxAppStorageMappings ||
			got.PartitionedRoots != perApp {
			t.Errorf("perApp=%v: list carried %+v", perApp, got)
		}
	}
}

// A directory already at the path is refused before anything is written: no
// row, no gate, no audit entry, and the name stays free to use once the
// directory is moved aside. Failing, the resource takes over a directory Toft
// did not make, and its delete removes it.
func TestACreateOverAnExistingDirectoryIsRefused(t *testing.T) {
	srv, router, _ := partitionedEnv(t, true)
	prov := srv.storage.(*rootProvisioner)
	prov.existing = map[string]bool{"taken": true}

	for _, body := range []string{
		`{"name":"taken","kind":"volume","backing":"host","storage_root":"data"}`,
		`{"name":"taken","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`,
	} {
		rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root", body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "directory_exists") ||
			!strings.Contains(rec.Body.String(), "/srv/data/taken") {
			t.Errorf("create %s = %d %s, want 409 directory_exists naming the path", body, rec.Code, rec.Body.String())
		}
	}
	if all, _ := srv.db.ListStorageResources(); len(all) != 0 {
		t.Errorf("a refused create left a resource: %+v", all)
	}
	var gates, audits int
	_ = srv.db.Conn().QueryRow(`SELECT count(*) FROM storage_gates WHERE group_name = ?`, runtime.StorageGroupName("taken")).Scan(&gates)
	_ = srv.db.Conn().QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'storage_resource.create'`).Scan(&audits)
	if gates != 0 || audits != 0 {
		t.Errorf("a refused create left %d gate(s) and %d audit row(s)", gates, audits)
	}

	delete(prov.existing, "taken")
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"taken","kind":"volume","backing":"host","storage_root":"data"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("once the directory is gone, create = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// A name already in use is name_taken, never directory_exists about the
// resource's own directory; and a malformed name is 400, as every invalid_*
// is (BB-019).
func TestACreateUnderATakenNameIsNameTaken(t *testing.T) {
	srv, router, _ := partitionedEnv(t, true)
	body := `{"name":"twice","kind":"volume","backing":"host","storage_root":"data"}`
	if rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d %s", rec.Code, rec.Body.String())
	}
	// Its directory exists now, as it does on a real root.
	srv.storage.(*rootProvisioner).existing = map[string]bool{"twice": true}
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"name_taken"`) ||
		!strings.Contains(rec.Body.String(), `"hint"`) {
		t.Errorf("second create = %d %s, want 409 name_taken", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"Bad Name","kind":"volume","backing":"host","storage_root":"data"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_storage_name") {
		t.Errorf("bad name = %d %s, want 400 invalid_storage_name", rec.Code, rec.Body.String())
	}
}

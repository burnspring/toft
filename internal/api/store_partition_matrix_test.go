package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// A partitioned resource takes a write only inside a folder an app is mapped
// to, and the rule is one rule: a bucket keeps it exactly as a host root does.
// A test drive found the host root refusing every write and the bucket taking
// them anywhere, the store root included, from the same `partitioned: true`.
//
// The matrix is backing × partitioned × mapped, and each cell asks the same
// three writes: a file beside every folder, a file in the mapped folder, and a
// marker for the mapped folder itself.
func TestPartitionIsEnforcedOnEveryBacking(t *testing.T) {
	type cell struct {
		backing           string
		partitioned, mapd bool
	}
	var cells []cell
	for _, b := range []string{"bucket", "host"} {
		for _, p := range []bool{false, true} {
			for _, m := range []bool{false, true} {
				cells = append(cells, cell{b, p, m})
			}
		}
	}
	for _, c := range cells {
		name := c.backing
		if c.partitioned {
			name += "/partitioned"
		} else {
			name += "/whole"
		}
		if c.mapd {
			name += "/mapped"
		} else {
			name += "/unmapped"
		}
		t.Run(name, func(t *testing.T) {
			srv, router, slug := partitionCell(t, c.backing, c.partitioned, c.mapd)

			upload := func(key string) *httptest.ResponseRecorder {
				rec, _ := mintUpload(t, srv, router, slug, key, []byte("x"))
				return rec
			}
			mkdir := func(path string) *httptest.ResponseRecorder {
				return storageRequest(t, srv, router, http.MethodPost, "/api/v1/store/"+slug+"/folders", "root",
					`{"path":"`+path+`"}`)
			}

			// Beside every folder: only a whole resource takes it.
			for what, rec := range map[string]*httptest.ResponseRecorder{
				"a file at the root": upload("loose.txt"), "a folder at the root": mkdir("loose"),
			} {
				if !c.partitioned {
					if rec.Code != http.StatusOK {
						t.Errorf("%s on a whole resource = %d %s, want 200", what, rec.Code, rec.Body.String())
					}
					continue
				}
				var body struct {
					Code    string   `json:"code"`
					Error   string   `json:"error"`
					Folders []string `json:"folders"`
					Hint    string   `json:"hint"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if rec.Code != http.StatusForbidden || body.Code != "outside_mapped_folders" || body.Hint == "" {
					t.Errorf("%s = %d %s, want 403 outside_mapped_folders with a hint", what, rec.Code, rec.Body.String())
				}
				if c.mapd && (len(body.Folders) != 1 || body.Folders[0] != "teams/sales" ||
					!strings.Contains(body.Error, `"teams/sales/"`)) {
					t.Errorf("%s: the refusal does not name the mapped folder: %s", what, rec.Body.String())
				}
				if !c.mapd && !strings.Contains(body.Error, "no folder of it is mapped") {
					t.Errorf("%s: the refusal does not say nothing is mapped: %s", what, rec.Body.String())
				}
			}

			// Inside the mapped folder: refused only where nothing is mapped.
			wantInside := http.StatusOK
			if c.partitioned && !c.mapd {
				wantInside = http.StatusForbidden
			}
			if rec := upload("teams/sales/in.txt"); rec.Code != wantInside {
				t.Errorf("a file in the folder = %d %s, want %d", rec.Code, rec.Body.String(), wantInside)
			}
			if rec := mkdir("teams/sales"); rec.Code != wantInside {
				t.Errorf("the folder's own marker = %d %s, want %d", rec.Code, rec.Body.String(), wantInside)
			}

			// And the listing says so before anything is tried.
			rec := storageRequest(t, srv, router, http.MethodGet, "/api/v1/store", "root", "")
			var list struct {
				Stores []storeSummary `json:"stores"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &list)
			if len(list.Stores) != 1 {
				t.Fatalf("GET /store = %s", rec.Body.String())
			}
			row := list.Stores[0]
			if row.Partitioned != c.partitioned {
				t.Errorf("partitioned = %v, want %v", row.Partitioned, c.partitioned)
			}
			want := ""
			if c.partitioned && c.mapd {
				want = "teams/sales"
			}
			// Present on every partitioned row, the empty list included —
			// "no write lands here" is the answer, not a missing key — and
			// absent on a whole store.
			switch {
			case c.partitioned && row.WritableFolders == nil:
				t.Errorf("a partitioned row has no writable_folders: %s", rec.Body.String())
			case !c.partitioned && row.WritableFolders != nil:
				t.Errorf("a whole store carries writable_folders: %s", rec.Body.String())
			case row.WritableFolders != nil:
				if got := strings.Join(*row.WritableFolders, ","); got != want {
					t.Errorf("writable_folders = %q, want %q", got, want)
				}
			}
			if c.partitioned && !c.mapd && !strings.Contains(rec.Body.String(), `"writable_folders":[]`) {
				t.Errorf("an unmapped partitioned store does not say [] on the wire: %s", rec.Body.String())
			}
		})
	}
}

// partitionCell builds one cell: a resource on the backing, root granted
// `full` on it, and — when mapped — an app on `teams/sales` (or on the whole
// of a whole resource).
func partitionCell(t *testing.T, backing string, partitioned, mapped bool) (*Server, http.Handler, string) {
	t.Helper()
	folder := ""
	if partitioned {
		folder = "teams/sales"
	}
	if backing == "host" {
		srv, router, dir := hostStoreEnv(t)
		body := `{"name":"teams","kind":"volume","backing":"host","storage_root":"data"}`
		if partitioned {
			body = `{"name":"teams","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`
		}
		res := hostResource(t, srv, router, dir, body)
		if mapped {
			app := &database.App{Slug: "sales", Name: "Sales", OwnerID: "owner",
				Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
			if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
				t.Fatal(err)
			}
			mapping := `{"path":"/data/sales","access":"full"}`
			if folder != "" {
				mapping = `{"path":"/data/sales","access":"full","folder":"` + folder + `"}`
			}
			if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID,
				"root", mapping); rec.Code != http.StatusOK {
				t.Fatalf("map = %d %s", rec.Code, rec.Body.String())
			}
			if _, err := srv.db.Conn().Exec(`UPDATE storage_gates SET gid = ? WHERE resource_id = ?`, os.Getgid(), res.ID); err != nil {
				t.Fatal(err)
			}
		}
		grant(t, srv, router, res, "full")
		return srv, router, res.Slug
	}

	srv := storeScopeServer(t, newCountingObjectStore(t))
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	res := &database.StorageResource{Name: "teams", Slug: "teams", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/teams", Partitioned: partitioned}
	if err := srv.db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	if mapped {
		app := &database.App{Slug: "sales", Name: "Sales", OwnerID: "owner", Framework: "flask",
			Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
		if err := srv.db.CreateApp(app); err != nil {
			t.Fatal(err)
		}
		m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID,
			Path: "/data/sales", Access: runtime.AccessWrite, Folder: folder}
		if err := srv.db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
			t.Fatalf("PutStorageMapping: %v", err)
		}
	}
	grantFolder(t, srv.db, res.ID, "root", "", runtime.AccessFull)
	return srv, srv.Router(http.NotFoundHandler()), res.Slug
}

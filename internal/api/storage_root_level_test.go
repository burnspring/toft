package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// On a shared install a volume's level is the root's (checkRootLevel): a
// read-write root serves full, a read-only one read. An attach that names no
// level takes the root's, and one naming the other is refused before anything
// is written — not accepted and then failed at the app's start, which is what
// a two-line `[[storage]]` entry used to do on trytoft (docs/bugs 2026-11-03
// #1). The console and the manifest share validateAttach, so this is both.
func TestAVolumeTakesItsRootsLevelAndRefusesTheOther(t *testing.T) {
	srv, router, app := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "rw", Path: t.TempDir()}, {Name: "ro", Path: t.TempDir(), ReadOnly: true}},
		t.TempDir(), ""))
	for i, tc := range []struct {
		root, body string
		wantCode   int
		wantAccess string
	}{
		{"rw", `{"path":"/data/a"}`, http.StatusOK, runtime.AccessFull},
		{"rw", `{"path":"/data/b","access":"full"}`, http.StatusOK, runtime.AccessFull},
		{"rw", `{"path":"/data/c","access":"read"}`, http.StatusBadRequest, ""},
		{"ro", `{"path":"/data/d"}`, http.StatusOK, runtime.AccessRead},
		{"ro", `{"path":"/data/e","access":"full"}`, http.StatusBadRequest, ""},
	} {
		res := &database.StorageResource{Slug: fmt.Sprintf("vol-%d", i), Name: fmt.Sprintf("vol-%d", i), Kind: database.StorageKindVolume,
			Backing: database.StorageBackingHost, Root: tc.root}
		if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
			t.Fatal(err)
		}
		rec := storageRequest(t, srv, router, http.MethodPut,
			"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", tc.body)
		if rec.Code != tc.wantCode {
			t.Errorf("%s %s = %d, want %d: %s", tc.root, tc.body, rec.Code, tc.wantCode, rec.Body.String())
			continue
		}
		mappings, err := srv.db.ListStorageMappingsForResource(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tc.wantCode != http.StatusOK {
			// The console answers its 400 with the sentence (the manifest's
			// plan carries the code, storage_level_unsupported — check test).
			var body map[string]any
			json.Unmarshal(rec.Body.Bytes(), &body)
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, `storage root "`+tc.root+`"`) || !strings.Contains(msg, "leave the level out") {
				t.Errorf("%s %s: refusal = %q, want it to name the root and the way out", tc.root, tc.body, msg)
			}
			// And its own code with a hint, not the 400's generic one (BB-046).
			if body["code"] != "storage_level_unsupported" || body["hint"] == nil {
				t.Errorf("%s %s: code %v hint %v", tc.root, tc.body, body["code"], body["hint"])
			}
			if len(mappings) != 0 {
				t.Errorf("%s %s: a refused attach wrote %+v", tc.root, tc.body, mappings)
			}
			continue
		}
		if len(mappings) != 1 || mappings[0].Access != tc.wantAccess {
			t.Errorf("%s %s: mappings = %+v, want one at %s", tc.root, tc.body, mappings, tc.wantAccess)
		}
	}
}

// A delegation's ceiling under a read-write root has to reach full, the only
// level the root serves; below it nobody could ever attach (drills 2026-11-03:
// read was accepted, then the delegate met above_ceiling at full and
// level_unsupported at read). A read-only root takes any ceiling at read or
// above, since read is all it serves.
func TestADelegationBelowItsRootsLevelIsRefused(t *testing.T) {
	srv, router, _ := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "rw", Path: t.TempDir()}, {Name: "ro", Path: t.TempDir(), ReadOnly: true}},
		t.TempDir(), ""))
	for i, tc := range []struct {
		root, level string
		want        int
	}{
		{"rw", "read", http.StatusBadRequest},
		{"rw", "full", http.StatusOK},
		{"ro", "read", http.StatusOK},
		{"ro", "full", http.StatusOK},
	} {
		res := &database.StorageResource{Slug: fmt.Sprintf("dvol-%d", i), Name: fmt.Sprintf("dvol-%d", i), Kind: database.StorageKindVolume,
			Backing: database.StorageBackingHost, Root: tc.root}
		if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
			t.Fatal(err)
		}
		rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+res.ID+"/managers", "root",
			`{"subject_type":"user","subject_id":"root","max_access":"`+tc.level+`"}`)
		if rec.Code != tc.want {
			t.Errorf("%s at %s = %d, want %d: %s", tc.root, tc.level, rec.Code, tc.want, rec.Body.String())
			continue
		}
		if tc.want == http.StatusBadRequest && !strings.Contains(rec.Body.String(), "storage_level_unsupported") {
			t.Errorf("%s at %s: refusal carries no code: %s", tc.root, tc.level, rec.Body.String())
		}
	}
}

// BB-046 (fourth 2026-11-03 black-box list): the documented bodies work on an
// install with one root. {"name","kind"} creates a host volume under it; a
// mapping with no path is mounted at /storage/<name>; a partitioned resource
// mapped with no folder is refused with its own code and a hint; a detach
// with no folder says it took every folder and how to name one; and the note
// says a start, not only a deployment, picks a mapping up.
func TestTheDocumentedStorageBodiesWorkOnAOneRootInstall(t *testing.T) {
	srv, router, app := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "ebs", Path: t.TempDir()}}, t.TempDir(), ""))
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"bb-vol","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Backing != database.StorageBackingHost || res.Root != "ebs" {
		t.Errorf("created %+v, want host under ebs", res)
	}
	rec = storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", `{"folder":"a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mapping with no path = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"/storage/bb-vol/a"`) || !strings.Contains(rec.Body.String(), "next start") {
		t.Errorf("mapping answer = %s, want /storage/bb-vol/a and the next start", rec.Body.String())
	}
	if rec := storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", `{"folder":"b"}`); rec.Code != http.StatusOK {
		t.Fatalf("second folder = %d %s", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":2`) || !strings.Contains(rec.Body.String(), "?folder=") {
		t.Errorf("detach with no folder = %d %s", rec.Code, rec.Body.String())
	}

	part := &database.StorageResource{Slug: "bb-part", Name: "bb-part", Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: "ebs", Partitioned: true}
	if err := srv.db.CreateStorageResource(part, "root", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rec = storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+part.ID+"/mappings/"+app.ID, "root", `{"folder":""}`)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusUnprocessableEntity || body["code"] != "storage_folder_required" || body["hint"] == nil {
		t.Errorf("partitioned with no folder = %d %s", rec.Code, rec.Body.String())
	}
}

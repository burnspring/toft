package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// A `blob` store attached at `write` or `overwrite` must be a 400 that writes
// no row, and this is one of the three failures in this work that would
// otherwise be silent.
//
// blobfuse2 has no allow-overwrite and no allow-delete: it mounts read-only or
// read-write, with nothing in between. A refusal that returned 200 and mounted
// read-write instead would be a silent grant of `full` on somebody else's
// dataset — an escalation that every existing test still passes through,
// because from every other angle the mapping looks exactly like the one the
// administrator asked for.
func TestBlobStoreRefusesTheRungItCannotMount(t *testing.T) {
	srv, router, app := blobStoreEnv(t)
	res := createBlobStore(t, srv, router)

	for _, level := range []string{runtime.AccessWrite} {
		t.Run(level, func(t *testing.T) {
			rec := storageRequest(t, srv, router, http.MethodPut,
				"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
				`{"path":"/data","access":"`+level+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("attach at %s = %d, want 400: %s", level, rec.Code, rec.Body.String())
			}

			// The refusal has to say all three things: the driver, what does
			// work, and where the finer level is still available.
			body := rec.Body.String()
			for _, want := range []string{"blobfuse2", "read", "full", "signed URL"} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not mention %q: %s", want, body)
				}
			}

			// And no row. A refusal that wrote the mapping and failed afterwards
			// would leave an app holding a level nothing can mount.
			mappings, err := srv.db.ListStorageMappingsForResource(res.ID)
			if err != nil {
				t.Fatalf("listing mappings: %v", err)
			}
			if len(mappings) != 0 {
				t.Errorf("a refused attach wrote %d mapping(s): %+v", len(mappings), mappings)
			}
		})
	}
}

// The two it *can* mount still work, or the refusal above would have taken the
// backing with it.
func TestBlobStoreAcceptsReadAndFull(t *testing.T) {
	srv, router, app := blobStoreEnv(t)
	res := createBlobStore(t, srv, router)

	for _, level := range []string{runtime.AccessRead, runtime.AccessFull} {
		rec := storageRequest(t, srv, router, http.MethodPut,
			"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
			`{"path":"/data","access":"`+level+`"}`)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Errorf("attach at %s = %d, want success: %s", level, rec.Code, rec.Body.String())
		}
	}
}

// A `mountpoint` store is untouched by any of this: all three rungs.
// The refusal is a property of one backing and not of stores.
func TestMountpointStoreKeepsAllThreeRungs(t *testing.T) {
	srv, router, app := blobStoreEnv(t)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"s3-reports","kind":"store","backing":"mountpoint","bucket":"acme-data","prefix":"reports"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	for _, level := range []string{
		runtime.AccessRead, runtime.AccessWrite, runtime.AccessFull,
	} {
		rec := storageRequest(t, srv, router, http.MethodPut,
			"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
			`{"path":"/data","access":"`+level+`"}`)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Errorf("mountpoint attach at %s = %d: %s", level, rec.Code, rec.Body.String())
		}
	}
}

func blobStoreEnv(t *testing.T) (*Server, http.Handler, *database.App) {
	t.Helper()
	return storageTestEnv(t, &backingProvisioner{
		backings: map[string][]string{
			runtime.MountStore:  {runtime.BackingMountpoint, runtime.BackingBlob},
			runtime.MountVolume: {runtime.BackingEFS, runtime.BackingAzureFiles},
		},
	})
}

func createBlobStore(t *testing.T, srv *Server, router http.Handler) database.StorageResource {
	t.Helper()
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"blob-reports","kind":"store","backing":"blob","bucket":"acme-data","prefix":"reports"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating the blob store = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding the created resource: %v", err)
	}
	if res.Backing != database.StorageBackingBlob {
		t.Fatalf("backing = %q, want %q", res.Backing, database.StorageBackingBlob)
	}
	// **The container and the prefix have to survive the create**, and for a
	// while they did not: the handler's bucket branch named `mountpoint` and
	// `s3files`, so a blob store came back with an empty container — nothing to
	// mount — and an empty prefix, which is the containment itself.
	if res.Bucket != "acme-data" || res.Prefix != "reports" {
		t.Fatalf("created blob store has bucket %q prefix %q, want the container and prefix "+
			"the form asked for — an empty prefix mounts the container root", res.Bucket, res.Prefix)
	}
	return res
}

// Whitespace in a blob folder or prefix is an argv boundary for the blob CSI
// driver, which splits mount options on spaces — so it is refused where it is
// written, while a mountpoint store keeps spaces (security review 2026-11-14,
// K8-2).
func TestBlobStoreRefusesWhitespaceInAFolderOrPrefix(t *testing.T) {
	srv, router, app := blobStoreEnv(t)
	res := createBlobStore(t, srv, router)

	rec := storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data","access":"read","folder":"x --subdirectory=other"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blob attach with a spaced folder = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"blob-spaced","kind":"store","backing":"blob","bucket":"acme-data","prefix":"te am"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blob create with a spaced prefix = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"s3-spaced","kind":"store","backing":"mountpoint","bucket":"acme-data","prefix":"te am"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("mountpoint create with a spaced prefix = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

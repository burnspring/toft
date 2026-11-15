package database_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

func mkResource(t *testing.T, db *database.DB, name, slug, kind string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{Name: name, Slug: slug, Kind: kind}
	if kind == database.StorageKindStore {
		res.Bucket, res.Prefix = "acme-data", "team/"+slug
	}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return res
}

// mkFileStore is the other half of mkResource: a volume backed by S3 Files,
// which is a file system to an app and a bucket to the platform. It names a
// bucket, and that is the whole difference the data plane cares about.
func mkFileStore(t *testing.T, db *database.DB, name, slug string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{
		Name: name, Slug: slug, Kind: database.StorageKindVolume,
		Backing: database.StorageBackingS3Files,
		Bucket:  "acme-files", Prefix: "team/" + slug,
	}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return res
}

// The backing a caller names is the backing that is stored, and a caller that
// names none gets the one its kind has always had.
//
// This is the column arriving without changing what an existing client
// creates, which is the only reason a default is defensible at all.
func TestBackingDefaultsToWhatTheKindAlwaysWas(t *testing.T) {
	db := dbtest.New(t)

	vol := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)
	if got := findResource(t, db, vol.ID).Backing; got != database.StorageBackingEFS {
		t.Errorf("volume backing = %q, want %q", got, database.StorageBackingEFS)
	}
	store := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	if got := findResource(t, db, store.ID).Backing; got != database.StorageBackingMountpoint {
		t.Errorf("store backing = %q, want %q", got, database.StorageBackingMountpoint)
	}
	files := mkFileStore(t, db, "Reports", "reports")
	if got := findResource(t, db, files.ID).Backing; got != database.StorageBackingS3Files {
		t.Errorf("file store backing = %q, want %q", got, database.StorageBackingS3Files)
	}
}

// A backing belongs to a kind, and the database refuses the combination rather
// than recording it.
//
// The CHECK is what makes the default in CreateStorageResource safe: a caller
// that names the wrong backing is refused here, not silently corrected.
func TestABackingBelongsToAKind(t *testing.T) {
	db := dbtest.New(t)

	res := &database.StorageResource{
		Name: "Wrong", Slug: "wrong", Kind: database.StorageKindStore,
		Backing: database.StorageBackingS3Files, Bucket: "acme-data", Prefix: "team/wrong",
	}
	err := db.CreateStorageResource(res, database.BuiltinAdminID, "req-1", "10.0.0.1", "test")
	if err == nil {
		t.Fatal("a store backed by s3files was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "storage_resources_backing_check") {
		t.Errorf("refused by something other than the backing check: %v", err)
	}
}

// A root is a file store's, and a `host` backing is a root's.
//
// The two halves of one mistake: a store with a root is a resource no backend
// can provision, because a store needs a CSI driver and therefore a cluster;
// and a host backing with no root names no directory at all.
func TestARootBelongsToAFileStoreAndAHostBackingNeedsOne(t *testing.T) {
	db := dbtest.New(t)

	// The pairing that works.
	ok := &database.StorageResource{
		Name: "Nightly", Slug: "nightly", Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: "data",
	}
	if err := db.CreateStorageResource(ok, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("a host-backed file store under a root was refused: %v", err)
	}
	got := findResource(t, db, ok.ID)
	if got.Root != "data" {
		t.Errorf("root = %q, want the one it was created with", got.Root)
	}
	// No bucket, and still a data plane: a directory under a host root is one
	// the platform itself can carry the bytes of (migration 00058), and with
	// no bucket there is no other path for them.
	if !got.HasDataPlane() || got.HasBucket() {
		t.Error("a host-backed file store should have a data plane and no bucket")
	}

	for _, c := range []struct {
		name string
		res  *database.StorageResource
	}{
		{"a host backing with no root", &database.StorageResource{
			Name: "Rootless", Slug: "rootless", Kind: database.StorageKindVolume,
			Backing: database.StorageBackingHost}},
		{"a store under a root", &database.StorageResource{
			Name: "Objects", Slug: "objects", Kind: database.StorageKindStore,
			Backing: database.StorageBackingMountpoint, Root: "data",
			Bucket: "acme-data", Prefix: "team/objects"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := db.CreateStorageResource(c.res, database.BuiltinAdminID, "req", "10.0.0.1", "test")
			if err == nil {
				t.Fatal("accepted, want a refusal")
			}
			if !strings.Contains(err.Error(), "storage_resources_root_check") {
				t.Errorf("refused by something other than the root check: %v", err)
			}
		})
	}
}

// A resource that apps still mount cannot be deleted.
//
// The refusal is what stands between an administrator and a live app losing its
// filesystem mid-read. It is a 409 rather than a cascade precisely because the
// mappings are the thing that has to be reconsidered, one app at a time.
func TestStorageResourceInUseCannotBeDeleted(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	res := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)

	mapping := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: "/data/cache", Access: runtime.AccessWrite,
	}
	if err := db.PutStorageMapping(mapping, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	err := db.DeleteStorageResource(res.ID, "admin-1", "req-2", "10.0.0.1", "test")
	if !errors.Is(err, database.ErrStorageInUse) {
		t.Fatalf("err = %v, want ErrStorageInUse", err)
	}
	// The count is in the message so an admin knows how much unmapping is ahead.
	if !strings.Contains(err.Error(), "1 app") {
		t.Errorf("the refusal should say how many, got: %v", err)
	}

	// Unmapped, it deletes.
	if _, err := db.DeleteStorageMapping(res.ID, app.ID, nil, "admin-1", "req-3", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageMapping: %v", err)
	}
	if err := db.DeleteStorageResource(res.ID, "admin-1", "req-4", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageResource: %v", err)
	}
}

// The (resource, app) pair is the mapping's identity, which is what makes the
// PUT an upsert: mapping the same resource again moves it rather than failing
// or leaving two mounts of one thing.
func TestMappingIsIdentifiedByResourceAndApp(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)

	put := func(path, access string) *database.StorageMapping {
		t.Helper()
		m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: path, Access: access}
		if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
			t.Fatalf("PutStorageMapping(%s): %v", path, err)
		}
		return m
	}
	first := put("/data/sets", runtime.AccessRead)
	second := put("/data/moved", runtime.AccessFull)
	// A re-put is answered as the row it is: created when the first put was, by
	// whoever made that one -- not 0001-01-01 and nobody (the trytoft drills).
	if second.CreatedAt.IsZero() || !second.CreatedAt.Equal(first.CreatedAt.Truncate(time.Microsecond)) || second.CreatedBy != first.CreatedBy {
		t.Errorf("the re-put mapping answered created %v by %q, want %v by %q",
			second.CreatedAt, second.CreatedBy, first.CreatedAt, first.CreatedBy)
	}

	mappings, err := db.ListStorageMappingsForApp(app.ID)
	if err != nil {
		t.Fatalf("ListStorageMappingsForApp: %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("got %d mappings, want 1 — the pair is the identity", len(mappings))
	}
	if mappings[0].Path != "/data/moved" || mappings[0].Access != runtime.AccessFull {
		t.Errorf("mapping = %+v, want the moved path and raised access", mappings[0])
	}
}

// Two resources on one path would be a pod the kubelet refuses to create, with
// an error nobody traces back to a storage mapping. Caught at the write.
func TestTwoResourcesCannotShareOnePath(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	first := mkResource(t, db, "One", "one", database.StorageKindVolume)
	second := mkResource(t, db, "Two", "two", database.StorageKindVolume)

	m1 := &database.StorageMapping{ResourceID: first.ID, AppID: app.ID, Path: "/data/x", Access: runtime.AccessRead}
	if err := db.PutStorageMapping(m1, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("first mapping: %v", err)
	}
	m2 := &database.StorageMapping{ResourceID: second.ID, AppID: app.ID, Path: "/data/x", Access: runtime.AccessRead}
	if err := db.PutStorageMapping(m2, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err == nil {
		t.Fatal("expected a conflict for two resources on one path")
	}
}

// Deleting an app removes its mappings and leaves the resource and its data
// alone. That asymmetry is the whole point: an app is disposable, the data an
// administrator provisioned for it is not.
func TestDeletingAnAppKeepsTheResource(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	res := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)

	m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: "/data/cache", Access: runtime.AccessWrite}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	if err := db.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	got, err := db.GetStorageResource(res.ID)
	if err != nil || got == nil {
		t.Fatalf("the resource should survive its app: %v", err)
	}
	if got.MappingCount != 0 {
		t.Errorf("mapping count = %d, want 0", got.MappingCount)
	}
}

// AppMounts is the one function both the deploy path and the job dispatcher
// call, which is what makes a job and its app see identical storage.
func TestAppMountsSpeakTheSeamsVocabulary(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	store := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)

	m := &database.StorageMapping{
		ResourceID: store.ID, AppID: app.ID, Path: "/data/sets", Access: runtime.AccessFull,
	}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	mounts, err := db.AppMounts(app.ID)
	if err != nil {
		t.Fatalf("AppMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	got := mounts[0]
	if got.Name != "datasets" || got.Path != "/data/sets" || got.Kind != runtime.MountStore {
		t.Errorf("mount = %+v", got)
	}
	// A store's flags live on its binding, so the claim has to carry the level.
	if got.Claim != runtime.BindingName("datasets", runtime.MountStore, runtime.AccessFull, "") {
		t.Errorf("claim = %q, want the level-specific binding", got.Claim)
	}
	if got.ReadOnly() {
		t.Error("a full-access mount must not read as read-only")
	}

	// An app with nothing mapped gets an empty slice, not a surprise.
	other := mkApp(t, db, "plain")
	if mounts, err := db.AppMounts(other.ID); err != nil || len(mounts) != 0 {
		t.Errorf("AppMounts on an unmapped app = %v, %v", mounts, err)
	}
}

// Every storage mutation is audited in the same transaction as the change, as
// every authorization-relevant mutation on this platform is.
func TestStorageChangesAreAudited(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")

	before, _ := db.CountAuditEntries()

	res := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)
	m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: "/data/cache", Access: runtime.AccessWrite}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	if _, err := db.DeleteStorageMapping(res.ID, app.ID, nil, "admin-1", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageMapping: %v", err)
	}
	if err := db.DeleteStorageResource(res.ID, "admin-1", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageResource: %v", err)
	}

	after, _ := db.CountAuditEntries()
	if after-before < 4 {
		t.Errorf("wrote %d audit entries for 4 mutations", after-before)
	}
}

// --- Validation ---

// The ladder is not symmetric, and offering a store's rungs on a volume would
// promise something no filesystem flag delivers.
func TestAccessLevelsAreValidatedPerKind(t *testing.T) {
	for _, tc := range []struct {
		kind, access string
		valid        bool
	}{
		{database.StorageKindVolume, runtime.AccessRead, true},
		// A volume mapping cannot be create-only: a filesystem cannot refuse a
		// rename to a process that can write.
		{database.StorageKindVolume, runtime.AccessWrite, false},
		{database.StorageKindVolume, runtime.AccessFull, true},
		{database.StorageKindVolume, "overwrite", false},
		{database.StorageKindStore, runtime.AccessRead, true},
		{database.StorageKindStore, runtime.AccessWrite, true},
		{database.StorageKindStore, runtime.AccessFull, true},
		// Merged into full by migration 00074, and refused rather than aliased.
		{database.StorageKindStore, "overwrite", false},
		{database.StorageKindStore, "sudo", false},
	} {
		if got := database.ValidStorageAccess(tc.kind, tc.access); got != tc.valid {
			t.Errorf("ValidStorageAccess(%q, %q) = %v, want %v", tc.kind, tc.access, got, tc.valid)
		}
	}
}

// A bad path is rejected here rather than at container creation, where it
// surfaces as an unexplained error — or, worse, succeeds and shadows the
// release tree the app is about to exec out of.
func TestMountPathsAreValidated(t *testing.T) {
	bad := []string{
		"data/reports",       // relative
		"/",                  // container root
		"/data/../etc",       // traversal
		"/workspace",         // the runner's own tree
		"/workspace/release", // ditto, deeper
		"/usr/lib",           // the image's own files
		"/data/reports/",     // trailing slash
		"/data//reports",     // an empty segment: the same link as /data/reports (B-6)
		"//etc/x",            // and one that hid a reserved prefix
	}
	for _, path := range bad {
		if err := database.ValidateMountPath(path); err == nil {
			t.Errorf("ValidateMountPath(%q) allowed it", path)
		}
	}

	for _, path := range []string{"/data", "/data/reports", "/mnt/cache", "/srv/shared-data"} {
		if err := database.ValidateMountPath(path); err != nil {
			t.Errorf("ValidateMountPath(%q) = %v, want no error", path, err)
		}
	}

	// The refusal for a reserved path suggests one that works, because the
	// reader has just been told "no" about the most obvious guess.
	err := database.ValidateMountPath("/workspace/data")
	if err == nil || !strings.Contains(err.Error(), "/data/") {
		t.Errorf("the refusal should suggest an alternative, got: %v", err)
	}
}

// A mapping comes back with the fields its own JSON promises.
//
// `PutStorageMapping` is what the two attach routes return to their caller, and
// it used to return a struct carrying only what the caller had already sent —
// no resource slug, no resource name, no kind. Nothing rendered it because both
// UI call sites refetch, which is precisely why this is worth a test: the next
// field added to the mapping shape would have been missing here too, silently.
func TestPutStorageMappingFillsDisplayFields(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	res := mkResource(t, db, "Nightly drop", "nightly-drop", database.StorageKindStore)

	mapping := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: "/data/drop", Access: runtime.AccessWrite,
	}
	if err := db.PutStorageMapping(mapping, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	if mapping.ResourceKind != database.StorageKindStore {
		t.Errorf("resource kind = %q, want %q — a reader with no kind has to guess one",
			mapping.ResourceKind, database.StorageKindStore)
	}
	// One name: the slug, whatever the caller passed as a label.
	if mapping.ResourceSlug != "nightly-drop" || mapping.ResourceName != "nightly-drop" {
		t.Errorf("resource slug/name = %q/%q, want nightly-drop for both",
			mapping.ResourceSlug, mapping.ResourceName)
	}
	if mapping.AppSlug != app.Slug || mapping.AppName != app.Name {
		t.Errorf("app slug/name = %q/%q, want %q/%q",
			mapping.AppSlug, mapping.AppName, app.Slug, app.Name)
	}
	// An administrator's stamp names nobody, and that absence is the fact —
	// `authority_name` is empty on `admin` in the listing too.
	if mapping.AuthorityName != "" {
		t.Errorf("authority name = %q, want empty on an admin stamp", mapping.AuthorityName)
	}

	// The update path returns them too: the same struct, a second write.
	mapping.Path = "/data/drop2"
	mapping.ResourceKind, mapping.ResourceSlug = "", ""
	if err := db.PutStorageMapping(mapping, database.BuiltinAdminID, "req-2", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping (update): %v", err)
	}
	if mapping.ResourceKind != database.StorageKindStore || mapping.ResourceSlug != "nightly-drop" {
		t.Errorf("update returned kind %q slug %q, want them refilled",
			mapping.ResourceKind, mapping.ResourceSlug)
	}
}

// A resource delete that races an attach waits for it and refuses: the
// mapping is not cascaded away with no audit row (security review 2026-11-14,
// DB-7).
func TestDeleteStorageResourceWaitsForAnAttachInFlight(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Scratch", "scratch-race", database.StorageKindStore)
	app := mkApp(t, db, "attaching")

	attach, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer attach.Rollback()
	if _, err := attach.Exec(`
		INSERT INTO storage_mappings (id, resource_id, app_id, path, access, authority, created_at, updated_at)
		VALUES ('sm-del-race', ?, ?, '/data/race', 'read', ?, NOW(), NOW())`,
		res.ID, app.ID, database.AuthorityAdmin); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- db.DeleteStorageResource(res.ID, "admin-1", "req", "10.0.0.1", "test") }()
	time.Sleep(200 * time.Millisecond)
	if err := attach.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, database.ErrStorageInUse) {
		t.Fatalf("delete during an attach = %v, want ErrStorageInUse", err)
	}
	var n int
	db.Conn().QueryRow(`SELECT COUNT(*) FROM storage_mappings WHERE id='sm-del-race'`).Scan(&n)
	if n != 1 {
		t.Error("the attached mapping was cascaded away")
	}
}

// A volume named for a store's level is that store's cluster binding: volume
// reports-read and store reports both mean the claim lpv-reports-read. The
// pair is refused at create in either order, naming the other; a
// level-suffixed name with no such store is not.
func TestAVolumeAndAStoreThatWouldShareABindingAreRefused(t *testing.T) {
	db := dbtest.New(t)
	mkResource(t, db, "reports-read", "reports-read", database.StorageKindVolume)
	store := &database.StorageResource{Name: "reports", Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports"}
	err := db.CreateStorageResource(store, database.BuiltinAdminID, "req", "10.0.0.1", "test")
	var clash *database.StorageBindingClashError
	if !errors.Is(err, database.ErrStorageBindingClash) || !errors.As(err, &clash) || clash.Other != "reports-read" {
		t.Fatalf("store after volume: err = %v, want a clash naming reports-read", err)
	}

	mkResource(t, db, "ledger", "ledger", database.StorageKindStore)
	vol := &database.StorageResource{Name: "ledger-full-0a1b2c3d", Slug: "ledger-full-0a1b2c3d", Kind: database.StorageKindVolume}
	err = db.CreateStorageResource(vol, database.BuiltinAdminID, "req", "10.0.0.1", "test")
	if !errors.As(err, &clash) || clash.Other != "ledger" || clash.Volume != "ledger-full-0a1b2c3d" {
		t.Fatalf("volume after store: err = %v, want a clash naming ledger", err)
	}

	mkResource(t, db, "uploads-full", "uploads-full", database.StorageKindVolume)
	mkResource(t, db, "ledger-fuller", "ledger-fuller", database.StorageKindVolume)
}

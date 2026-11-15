package database_test

import (
	"errors"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

func mkHostResource(t *testing.T, db *database.DB, slug string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{Name: slug, Slug: slug, Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: "data"}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource(%s): %v", slug, err)
	}
	return res
}

// gateOf is what a start is handed for one resource: the gate AppMounts reads.
func gateOf(t *testing.T, db *database.DB, res *database.StorageResource, slug string) runtime.Gate {
	t.Helper()
	app := mkApp(t, db, slug)
	m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: "/data/x", Access: runtime.AccessWrite}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	mounts, err := db.AppMounts(app.ID)
	if err != nil || len(mounts) != 1 {
		t.Fatalf("AppMounts = %v, %v", mounts, err)
	}
	if _, err := db.DeleteStorageMapping(res.ID, app.ID, nil, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageMapping: %v", err)
	}
	return mounts[0].Gate
}

// A host-root resource's gate is numbered by the database, in the create's own
// transaction, and a start is handed that number. The host's /etc/group dies
// with its container; the number on the disk does not, so it has to come from
// somewhere that does not either.
func TestAHostResourceIsGivenItsGateNumberAtCreate(t *testing.T) {
	db := dbtest.New(t)
	a := mkHostResource(t, db, "alpha")
	b := mkHostResource(t, db, "beta")

	ga, gb := gateOf(t, db, a, "app-a"), gateOf(t, db, b, "app-b")
	if ga != (runtime.Gate{Group: "toft-store-alpha", GID: database.DefaultGateGIDLow}) {
		t.Errorf("first gate = %+v, want toft-store-alpha at the bottom of the range", ga)
	}
	if gb.GID != database.DefaultGateGIDLow+1 || gb.Group != "toft-store-beta" {
		t.Errorf("second gate = %+v, want the next number", gb)
	}

	// A resource with no root has no gate: there is no host directory to gate.
	efs := mkResource(t, db, "Cluster cache", "cluster-cache", database.StorageKindVolume)
	if g := gateOf(t, db, efs, "app-c"); g != (runtime.Gate{}) {
		t.Errorf("a cluster resource was given gate %+v", g)
	}
}

// Deleting a resource keeps its directory, so it keeps its number: the files
// there still carry it. A new resource never takes it, and one created onto the
// same directory — the same slug — takes it back.
func TestADeletedResourcesGateNumberIsNeverReused(t *testing.T) {
	db := dbtest.New(t)
	a := mkHostResource(t, db, "alpha")
	first := gateOf(t, db, a, "app-a")
	if err := db.DeleteStorageResource(a.ID, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageResource: %v", err)
	}
	// A host root's destroy is synchronous, so the handler frees the slug at
	// once; this is that step.
	if err := db.FinishStorageDeletion("alpha"); err != nil {
		t.Fatalf("FinishStorageDeletion: %v", err)
	}

	b := mkHostResource(t, db, "beta")
	if g := gateOf(t, db, b, "app-b"); g.GID == first.GID {
		t.Errorf("a new resource took the deleted one's number %d", g.GID)
	}
	again := mkHostResource(t, db, "alpha")
	if g := gateOf(t, db, again, "app-c"); g != first {
		t.Errorf("recreated onto the same directory: gate %+v, want the one its files carry, %+v", g, first)
	}
}

// A range with nothing left refuses the create that needed a number, naming the
// setting, rather than wrapping or reusing.
func TestAnExhaustedGateRangeRefusesTheCreate(t *testing.T) {
	db := dbtest.New(t, database.WithGateGIDRange(63000, 63000))
	mkHostResource(t, db, "alpha")
	res := &database.StorageResource{Name: "beta", Slug: "beta", Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: "data"}
	err := db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test")
	if !errors.Is(err, database.ErrGateRangeExhausted) {
		t.Fatalf("err = %v, want ErrGateRangeExhausted", err)
	}
}

func mkPartitioned(t *testing.T, db *database.DB, slug, root string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{Name: slug, Slug: slug, Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: root, Partitioned: true}
	if root == "" {
		res.Backing = database.StorageBackingEFS
	}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource(%s): %v", slug, err)
	}
	return res
}

func mapFolder(db *database.DB, res *database.StorageResource, app *database.App, path, folder string) error {
	return db.PutStorageMapping(&database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: path,
		Access: runtime.AccessWrite, Folder: folder}, database.BuiltinAdminID, "req", "10.0.0.1", "test")
}

// A partitioned resource is mapped only by folder, and no folder contains
// another — matched on a separator, so `report` and `reports` are neighbours.
// Equal folders are how two apps share one. This is the trigger, so it holds for
// every writer, a delegate included.
func TestAPartitionedResourcesFoldersNeverOverlap(t *testing.T) {
	db := dbtest.New(t)
	res := mkPartitioned(t, db, "share", "data")
	a, b, c := mkApp(t, db, "app-a"), mkApp(t, db, "app-b"), mkApp(t, db, "app-c")

	if err := mapFolder(db, res, a, "/data/r", ""); err == nil {
		t.Error("a whole mapping of a partitioned resource was accepted")
	}
	if err := mapFolder(db, res, a, "/data/r", "teams/a"); err != nil {
		t.Fatalf("first folder: %v", err)
	}
	if err := mapFolder(db, res, b, "/data/r", "teams/a"); err != nil {
		t.Errorf("a second app on the same folder was refused: %v", err)
	}
	for _, f := range []string{"teams", "teams/a/inbox"} {
		if err := mapFolder(db, res, c, "/data/"+f, f); err == nil {
			t.Errorf("%q overlapping teams/a was accepted", f)
		}
	}
	for _, f := range []string{"teams/ab", "teams/b", "team"} {
		if err := mapFolder(db, res, c, "/data/"+f, f); err != nil {
			t.Errorf("%q beside teams/a was refused: %v", f, err)
		}
	}
	other, apps, err := db.PartitionConflict(res.ID, "teams", "")
	if err != nil || other == "" || len(apps) == 0 {
		t.Errorf("PartitionConflict(teams) = %q %v %v, want a folder beneath it and its apps", other, apps, err)
	}

	// The flag never moves.
	if _, err := db.Conn().Exec(`UPDATE storage_resources SET partitioned = false WHERE id = ?`, res.ID); err == nil {
		t.Error("a resource's partitioned flag was changed")
	}
}

// Each folder of a partitioned resource on a host root is a gate with its own
// number, allocated at the first mapping and shared by every app on that
// folder; the resource itself has none. On a cluster no gate is written.
func TestAPartitionedFolderIsAGateOfItsOwn(t *testing.T) {
	db := dbtest.New(t)
	res := mkPartitioned(t, db, "share", "data")
	a, b, c := mkApp(t, db, "app-a"), mkApp(t, db, "app-b"), mkApp(t, db, "app-c")
	for _, m := range []struct {
		app    *database.App
		folder string
	}{{a, "teams/a"}, {b, "teams/a"}, {c, "teams/b"}} {
		if err := mapFolder(db, res, m.app, "/data/r", m.folder); err != nil {
			t.Fatalf("map %s: %v", m.folder, err)
		}
	}
	gate := func(app *database.App) runtime.Gate {
		mounts, err := db.AppMounts(app.ID)
		if err != nil || len(mounts) != 1 || !mounts[0].Partitioned {
			t.Fatalf("AppMounts = %+v, %v", mounts, err)
		}
		return mounts[0].Gate
	}
	ga, gb, gc := gate(a), gate(b), gate(c)
	if ga != gb {
		t.Errorf("two apps on teams/a got gates %+v and %+v, want one", ga, gb)
	}
	if ga.GID == gc.GID || ga.Group != runtime.FolderGateName("share", "teams/a") {
		t.Errorf("gates %+v and %+v, want one per folder, named for it", ga, gc)
	}

	cluster := mkPartitioned(t, db, "cluster-share", "")
	d := mkApp(t, db, "app-d")
	if err := mapFolder(db, cluster, d, "/data/r", "x"); err != nil {
		t.Fatalf("map on a cluster: %v", err)
	}
	if mounts, _ := db.AppMounts(d.ID); mounts[0].Gate != (runtime.Gate{}) {
		t.Errorf("a cluster resource's folder was given gate %+v", mounts[0].Gate)
	}
}

// A write on a host root takes the gate that holds its name: the resource's
// own on a whole resource; on a partitioned one the gate of the mapped folder
// it is inside — the folder itself included — and none beside the folders.
func TestAWriteTakesTheGateThatHoldsItsName(t *testing.T) {
	db := dbtest.New(t)
	whole := mkHostResource(t, db, "whole")
	want := gateOf(t, db, whole, "app-w").GID
	if gid, err := db.GateForKey(whole.ID, "any/where.csv"); err != nil || gid != want {
		t.Errorf("whole: GateForKey = %d, %v; want %d", gid, err, want)
	}

	part := mkPartitioned(t, db, "teams", "data")
	app := mkApp(t, db, "sales")
	if err := mapFolder(db, part, app, "/data/sales", "teams/sales"); err != nil {
		t.Fatal(err)
	}
	mounts, _ := db.AppMounts(app.ID)
	folderGID := mounts[0].Gate.GID
	for name, wantGID := range map[string]int{
		"teams/sales/in.csv": folderGID,
		"teams/sales/":       folderGID,
		"teams/sales/sub/":   folderGID,
	} {
		if gid, err := db.GateForKey(part.ID, name); err != nil || gid != wantGID {
			t.Errorf("GateForKey(%q) = %d, %v; want %d", name, gid, err, wantGID)
		}
	}
	for _, name := range []string{"loose.csv", "teams/", "teams/salesx/f"} {
		if _, err := db.GateForKey(part.ID, name); !errors.Is(err, database.ErrNoGateForKey) {
			t.Errorf("GateForKey(%q) err = %v, want ErrNoGateForKey", name, err)
		}
	}
}

// A folder's gate outlives the mapping that numbered it, and a stale gate still
// covers every key beneath its folder. The write takes the gate of the folder
// that is mapped now, in either direction of a re-split, and a key under no
// mapped folder is refused rather than given a stale group.
func TestAWriteTakesTheGateOfTheFolderMappedNowNotAStaleOne(t *testing.T) {
	for _, tc := range []struct{ stale, mapped, inside, outside string }{
		{"teams", "teams/a", "teams/a/f.csv", "teams/b/f.csv"},
		{"teams/a", "teams", "teams/a/f.csv", "other/f.csv"},
	} {
		db := dbtest.New(t)
		res := mkPartitioned(t, db, "share", "data")
		app := mkApp(t, db, "app-a")
		if err := mapFolder(db, res, app, "/data/r", tc.stale); err != nil {
			t.Fatalf("map %s: %v", tc.stale, err)
		}
		folder := tc.stale
		if _, err := db.DeleteStorageMapping(res.ID, app.ID, &folder, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
			t.Fatalf("unmap %s: %v", tc.stale, err)
		}
		if err := mapFolder(db, res, app, "/data/r", tc.mapped); err != nil {
			t.Fatalf("map %s: %v", tc.mapped, err)
		}
		mounts, err := db.AppMounts(app.ID)
		if err != nil || len(mounts) != 1 {
			t.Fatalf("AppMounts = %+v, %v", mounts, err)
		}
		want := mounts[0].Gate.GID
		if gid, err := db.GateForKey(res.ID, tc.inside); err != nil || gid != want {
			t.Errorf("%s -> %s: GateForKey(%q) = %d, %v; want the mapped folder's %d", tc.stale, tc.mapped, tc.inside, gid, err, want)
		}
		if _, err := db.GateForKey(res.ID, tc.outside); !errors.Is(err, database.ErrNoGateForKey) {
			t.Errorf("%s -> %s: GateForKey(%q) err = %v, want ErrNoGateForKey", tc.stale, tc.mapped, tc.outside, err)
		}
	}
}

// A gate is re-pointed only onto the folder it already names — its own live
// row, or a retired gate of a slug re-created — never off another resource's
// live gate whose name a chosen folder collides with. A collision is a refused
// mapping, and the gate it would have taken stays its folder's.
func TestAFolderWhoseGateNameIsTakenIsRefusedNotHandedTheGate(t *testing.T) {
	db := dbtest.New(t)
	share := mkPartitioned(t, db, "share", "data")
	other := mkPartitioned(t, db, "other", "data")
	a, b := mkApp(t, db, "app-a"), mkApp(t, db, "app-b")
	if err := mapFolder(db, share, a, "/data/r", "teams/a"); err != nil {
		t.Fatalf("map teams/a: %v", err)
	}
	before, err := db.AppMounts(a.ID)
	if err != nil || len(before) != 1 {
		t.Fatalf("AppMounts = %+v, %v", before, err)
	}
	// Stage the 48-bit collision: share/teams/a's gate now carries the name
	// other/x would be given.
	collide := runtime.FolderGateName("other", "x")
	if _, err := db.Conn().Exec(`UPDATE storage_gates SET group_name = ? WHERE resource_id = ? AND folder = 'teams/a'`,
		collide, share.ID); err != nil {
		t.Fatalf("staging the collision: %v", err)
	}
	if err := mapFolder(db, other, b, "/data/r", "x"); !errors.Is(err, database.ErrGateNameTaken) {
		t.Fatalf("mapping a colliding folder: err = %v, want ErrGateNameTaken", err)
	}
	if err := db.EnsureFolderGate(other.ID, "x"); !errors.Is(err, database.ErrGateNameTaken) {
		t.Errorf("EnsureFolderGate on a colliding folder: err = %v, want ErrGateNameTaken", err)
	}
	after, err := db.AppMounts(a.ID)
	if err != nil || len(after) != 1 || after[0].Gate.GID != before[0].Gate.GID {
		t.Errorf("share's teams/a after the refused mapping: %+v, %v; want gid %d still", after, err, before[0].Gate.GID)
	}
}

// A grant on a host-root resource passes the floor now that the platform
// carries its bytes; one on a cluster EFS resource, which nobody here mounts,
// still does not.
func TestAHostRootResourceTakesGrantsAndEFSStillDoesNot(t *testing.T) {
	db := dbtest.New(t)
	host := mkHostResource(t, db, "share")
	if err := db.PutStoreGrant(&database.StoreGrant{ResourceID: host.ID, SubjectType: database.StoreSubjectUser,
		SubjectID: database.BuiltinAdminID, Access: "read"}, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Errorf("a grant on a host-root resource: %v", err)
	}
	efs := mkResource(t, db, "Cluster", "cluster", database.StorageKindVolume)
	if _, err := db.Conn().Exec(`INSERT INTO store_grants (id, resource_id, subject_type, subject_id, access)
		VALUES ('g-efs', ?, 'user', ?, 'read')`, efs.ID, database.BuiltinAdminID); err == nil {
		t.Error("the trigger let a grant onto an EFS resource")
	}
}

// A slug names one store. The reach query's filter has two halves — a bucket,
// or a host root — and both callers append "AND …" to it; written without
// parentheses the slug bound only to the second half, so a caller granted a
// bucket store got that bucket back for every slug they asked about, a host
// volume's included, and their uploads were signed into the wrong resource.
// Silent at every layer above: the request succeeds, at the wrong place.
func TestASlugResolvesToItsOwnStoreAndNoOther(t *testing.T) {
	db := dbtest.New(t)
	bucket := mkResource(t, db, "Drop", "drop", database.StorageKindStore)
	host := mkHostResource(t, db, "share")
	for _, res := range []*database.StorageResource{bucket, host} {
		if err := db.PutStoreGrant(&database.StoreGrant{ResourceID: res.ID, SubjectType: database.StoreSubjectUser,
			SubjectID: database.BuiltinAdminID, Access: "write"}, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []*database.StorageResource{bucket, host} {
		got, err := db.StoreScopesBySlug(database.BuiltinAdminID, want.Slug)
		if err != nil || got == nil || got.Resource.ID != want.ID {
			t.Errorf("StoreScopesBySlug(%q) = %+v, %v; want resource %s", want.Slug, got, err, want.ID)
		}
	}
	if got, err := db.StoreScopesBySlug(database.BuiltinAdminID, "nobody-granted-this"); err != nil || got != nil {
		t.Errorf("an unknown slug resolved to %+v, %v", got, err)
	}
	reach, err := db.ListReachableStores(database.BuiltinAdminID)
	if err != nil || len(reach) != 2 {
		t.Errorf("ListReachableStores = %d stores, %v; want both", len(reach), err)
	}
}

// A folder an app's own manifest maps is no conflict for the same app's next
// manifest: the flip removes it before it writes, so moving `teams` to
// `teams/a` in one deploy is a move, not an overlap. Another app's folder, or
// the app's console mapping, still is (code review 2026-11-11, B-5).
func TestAManifestFolderMayMoveBeneathItself(t *testing.T) {
	db := dbtest.New(t)
	res := mkPartitioned(t, db, "share", "data")
	a, b := mkApp(t, db, "app-a"), mkApp(t, db, "app-b")
	if err := mapFolder(db, res, a, "/data/r", "teams"); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	// What the manifest's reconcile writes; the console route never does.
	if _, err := db.Conn().Exec(`UPDATE storage_mappings SET source = ? WHERE app_id = ?`,
		database.MappingSourceManifest, a.ID); err != nil {
		t.Fatalf("marking the row the manifest's: %v", err)
	}
	if other, _, err := db.PartitionConflict(res.ID, "teams/a", a.ID); err != nil || other != "" {
		t.Errorf("the app's own manifest folder conflicted with its move: %q %v", other, err)
	}
	if other, _, err := db.PartitionConflict(res.ID, "teams/a", b.ID); err != nil || other != "teams" {
		t.Errorf("another app's view = %q %v, want teams in the way", other, err)
	}
	if other, _, err := db.PartitionConflict(res.ID, "teams/a", ""); err != nil || other != "teams" {
		t.Errorf("with no exception = %q %v, want teams in the way", other, err)
	}
}

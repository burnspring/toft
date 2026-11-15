package database_test

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// A deleted resource's slug is held until its volumes are gone, and freed
// exactly then. A slug used again provisions onto the same directory, so a
// create inside the window would have its files removed by its predecessor's
// late delete.
func TestADeletedSlugIsHeldUntilItsDeleteFinishes(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "share", "share", database.StorageKindVolume)
	for _, c := range []string{"lpv-share", "lpv-share-0a1b2c3d"} {
		if err := db.RecordStorageClaim(res.ID, c, ""); err != nil {
			t.Fatalf("RecordStorageClaim: %v", err)
		}
	}

	if err := db.DeleteStorageResource(res.ID, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageResource: %v", err)
	}
	all, err := db.ListStorageDeletions()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListStorageDeletions = %v, %v; want one", all, err)
	}
	if d := all[0]; d.Slug != "share" || d.Report != nil ||
		strings.Join(d.Claims, ",") != "lpv-share,lpv-share-0a1b2c3d" {
		t.Errorf("tombstone = %+v, want the slug, both claims and no report yet", d)
	}

	again := &database.StorageResource{Name: "share", Slug: "share", Kind: database.StorageKindVolume}
	if err := db.CreateStorageResource(again, database.BuiltinAdminID, "req", "10.0.0.1", "test"); !errors.Is(err, database.ErrStorageDeleting) {
		t.Fatalf("create while deleting: err = %v, want ErrStorageDeleting", err)
	}

	if _, err := db.SetStorageDeletionReport("share", []string{"pv-1"}, runtime.DestroyReport{RemovesData: true}); err != nil {
		t.Fatalf("SetStorageDeletionReport: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := db.FinishStorageDeletion("share"); err != nil {
			t.Fatalf("FinishStorageDeletion #%d: %v", i+1, err)
		}
	}
	if err := db.CreateStorageResource(again, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("create once finished: %v", err)
	}

	// Each stage of the delete is on the Activity page once, and all three
	// still say which resource after its row is gone.
	entries, err := db.ListActivity(database.ActivityFilter{
		Actions: []string{"storage_resource.delete", "storage_resource.destroy_started", "storage_resource.destroyed"},
		Limit:   50,
	})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Action]++
		if e.TargetLabel != "share" {
			t.Errorf("%s is labelled %q, want the deleted resource's slug", e.Action, e.TargetLabel)
		}
	}
	for _, a := range []string{"storage_resource.delete", "storage_resource.destroy_started", "storage_resource.destroyed"} {
		if seen[a] != 1 {
			t.Errorf("%s appears %d times, want once", a, seen[a])
		}
	}
}

// A create and a delete of one slug cannot interleave so that the create
// lands while the delete's volumes are still going: the create either meets
// the row (a duplicate) or the tombstone, never a gap between them.
func TestACreateNeverSlipsBetweenADeleteAndItsTombstone(t *testing.T) {
	db := dbtest.New(t)
	for i := 0; i < 15; i++ {
		res := mkResource(t, db, "race", "race", database.StorageKindVolume)
		var wg sync.WaitGroup
		var createErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := db.DeleteStorageResource(res.ID, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
				t.Errorf("delete: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			createErr = db.CreateStorageResource(&database.StorageResource{Name: "race", Slug: "race",
				Kind: database.StorageKindVolume}, database.BuiltinAdminID, "req", "10.0.0.1", "test")
		}()
		wg.Wait()
		if createErr == nil {
			t.Fatalf("round %d: a create of a slug being deleted succeeded", i)
		}
		if err := db.FinishStorageDeletion("race"); err != nil {
			t.Fatal(err)
		}
	}
}

// The migration names every claim it can: each volume's root claim and one per
// folder it is mapped at, under runtime.BindingName's own spelling — the SQL
// digest and the Go digest have to agree, or a recorded claim names nothing.
// The statements are read from the migration file itself, so this tests what
// ran rather than a copy of it.
func TestTheBackfillSpellsClaimsAsTheRuntimeDoes(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	share := mkResource(t, db, "share", "share", database.StorageKindVolume)
	store := mkResource(t, db, "drop", "drop", database.StorageKindStore)
	for _, folder := range []string{"teams", "finance/q1"} {
		if err := db.PutStorageMapping(&database.StorageMapping{ResourceID: share.ID, AppID: app.ID,
			Path: "/data/" + strings.ReplaceAll(folder, "/", "-"), Access: runtime.AccessWrite, Folder: folder},
			database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
			t.Fatalf("mapping %s: %v", folder, err)
		}
	}
	if err := db.PutStorageMapping(&database.StorageMapping{ResourceID: store.ID, AppID: app.ID,
		Path: "/data/drop", Access: runtime.AccessRead}, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("store mapping: %v", err)
	}

	raw, err := os.ReadFile("migrations/00072_storage_claims_and_deletions.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	ran := 0
	for _, stmt := range strings.Split(up, ";") {
		if i := strings.Index(stmt, "INSERT INTO storage_claims"); i >= 0 {
			if _, err := db.Conn().SQL().Exec(stmt[i:]); err != nil {
				t.Fatalf("backfill: %v", err)
			}
			ran++
		}
	}
	if ran != 2 {
		t.Fatalf("found %d backfill statements, want 2", ran)
	}

	got, err := db.ListStorageClaims(share.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		runtime.BindingName("share", runtime.MountVolume, runtime.AccessRead, ""),
		runtime.BindingName("share", runtime.MountVolume, runtime.AccessRead, "teams"),
		runtime.BindingName("share", runtime.MountVolume, runtime.AccessRead, "finance/q1"),
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("claims %v are missing %s", got, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("claims = %v, want exactly %v", got, want)
	}
	if storeClaims, _ := db.ListStorageClaims(store.ID); len(storeClaims) != 0 {
		t.Errorf("a store got claims %v: stores are released, never destroyed, and need no record", storeClaims)
	}
}

// AP-4: a tombstone's volumes are a union across passes, so a second pass
// that could not name a volume the first flipped does not forget it.
func TestATombstonesVolumesAccumulateAcrossPasses(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "pile", "pile", database.StorageKindVolume)
	if err := db.DeleteStorageResource(res.ID, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageResource: %v", err)
	}
	if err := db.AddStorageDeletionVolumes("pile", []string{"pv-1"}); err != nil {
		t.Fatalf("AddStorageDeletionVolumes: %v", err)
	}
	all, _ := db.ListStorageDeletions()
	if len(all) != 1 || all[0].Report != nil || strings.Join(all[0].Volumes, ",") != "pv-1" {
		t.Fatalf("after a cut-short pass = %+v, want pv-1 and no report", all)
	}
	merged, err := db.SetStorageDeletionReport("pile", []string{"pv-2"}, runtime.DestroyReport{})
	if err != nil {
		t.Fatalf("SetStorageDeletionReport: %v", err)
	}
	if strings.Join(merged, ",") != "pv-1,pv-2" {
		t.Errorf("merged = %v, want pv-1,pv-2", merged)
	}
	merged, err = db.SetStorageDeletionReport("pile", nil, runtime.DestroyReport{})
	if err != nil || strings.Join(merged, ",") != "pv-1,pv-2" {
		t.Errorf("a pass with nothing pending = %v, %v; want the earlier volumes kept", merged, err)
	}
}

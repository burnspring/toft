package database_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// The per-app cap is what keeps an app inside NFS on a host root: AUTH_SYS
// carries 16 gids, and toft-apps and the account's own group take two. Raised
// past 14 the refusal at the mapping becomes an EACCES on a file every mode
// bit says the app may open — silent, which is why this is a test and not a
// comment.
func TestThePerAppCapStaysUnderTheNFSGroupCeiling(t *testing.T) {
	const authSysGIDs, ownGroups = 16, 2
	if database.MaxAppStorageMappings > authSysGIDs-ownGroups {
		t.Fatalf("MaxAppStorageMappings = %d; an app with that many gates is past the %d NFS carries",
			database.MaxAppStorageMappings, authSysGIDs-ownGroups)
	}
}

// The 26th resource is refused, whatever its kind, and two creates racing for
// the 25th place cannot both win it.
func TestAnInstallHoldsAtMostTwentyFiveResources(t *testing.T) {
	db := dbtest.New(t)
	for i := 0; i < database.MaxStorageResources-2; i++ {
		mkResource(t, db, fmt.Sprintf("Res %d", i), fmt.Sprintf("res-%d", i), database.StorageKindVolume)
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := &database.StorageResource{Name: fmt.Sprintf("Race %d", i), Slug: fmt.Sprintf("race-%d", i),
				Kind: database.StorageKindVolume}
			errs[i] = db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test")
		}(i)
	}
	wg.Wait()
	won, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, database.ErrStorageResourceCap):
			capped++
		default:
			t.Fatalf("racing create: %v", err)
		}
	}
	if won != 2 || capped != 1 {
		t.Fatalf("three creates for two places: %d won, %d capped", won, capped)
	}
	all, err := db.ListStorageResources()
	if err != nil || len(all) != database.MaxStorageResources {
		t.Fatalf("ListStorageResources = %d, %v; want %d", len(all), err, database.MaxStorageResources)
	}
}

// An app holds at most five mappings, folders and whole resources counted
// alike. Editing a mapping it already holds is not a sixth, and detaching one
// makes room.
func TestAnAppHoldsAtMostFiveMappings(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	share := mkResource(t, db, "Share", "share", database.StorageKindVolume)
	other := mkResource(t, db, "Other", "other", database.StorageKindVolume)

	put := func(res *database.StorageResource, folder, path string) error {
		return db.PutStorageMapping(&database.StorageMapping{ResourceID: res.ID, AppID: app.ID,
			Path: path, Access: runtime.AccessWrite, Folder: folder}, database.BuiltinAdminID, "req", "10.0.0.1", "test")
	}
	if err := put(other, "", "/data/other"); err != nil {
		t.Fatalf("whole mapping: %v", err)
	}
	for i := 1; i < database.MaxAppStorageMappings; i++ {
		if err := put(share, fmt.Sprintf("f%d", i), fmt.Sprintf("/data/f%d", i)); err != nil {
			t.Fatalf("mapping %d: %v", i+1, err)
		}
	}
	if err := put(share, "f9", "/data/f9"); !errors.Is(err, database.ErrAppMappingCap) {
		t.Fatalf("sixth mapping: err = %v, want ErrAppMappingCap", err)
	}
	if err := put(share, "f1", "/data/moved"); err != nil {
		t.Fatalf("editing a held mapping at the cap: %v", err)
	}

	// Another app is counted on its own. The answer carries the source the
	// row has, as every later read does (BB-087).
	second := &database.StorageMapping{ResourceID: share.ID, AppID: mkApp(t, db, "second-app").ID,
		Path: "/data/f9", Access: runtime.AccessWrite, Folder: "f9"}
	if err := db.PutStorageMapping(second, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("a second app's first mapping: %v", err)
	}
	if second.Source != database.MappingSourceConsole {
		t.Errorf("the attach answer's source = %q, want %q", second.Source, database.MappingSourceConsole)
	}

	f2 := "f2"
	if _, err := db.DeleteStorageMapping(share.ID, app.ID, &f2, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageMapping: %v", err)
	}
	if err := put(share, "f9", "/data/f9"); err != nil {
		t.Fatalf("a mapping after detaching one: %v", err)
	}
}

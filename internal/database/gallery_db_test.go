package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// mkGalleryApp creates an app attached to a catalog item, the way a gallery
// deploy does: provenance is written with the row rather than by a second
// update, so an app can never exist without the record of where it came from.
func mkGalleryApp(t *testing.T, db *database.DB, slug, itemID, kind string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: owner.ID,
		RepoURL: "https://github.com/example/" + slug, Branch: "v1.0.0",
		Status:        database.AppStatusStopped,
		GalleryItemID: itemID, GalleryKind: kind,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func TestGalleryProvenanceRoundTrips(t *testing.T) {
	db := dbtest.New(t)
	app := mkGalleryApp(t, db, "audit", "ext-1", "extension")

	got, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.GalleryItemID != "ext-1" || got.GalleryKind != "extension" {
		t.Fatalf("provenance = %q/%q", got.GalleryItemID, got.GalleryKind)
	}
	// Nothing claims a running version until a build has succeeded.
	if got.GalleryVersion != "" {
		t.Fatalf("gallery_version = %q on a freshly created app, want empty", got.GalleryVersion)
	}

	if err := db.SetAppGalleryVersion(app.ID, "1.4.0"); err != nil {
		t.Fatalf("SetAppGalleryVersion: %v", err)
	}
	if got, _ = db.GetApp(app.ID); got.GalleryVersion != "1.4.0" {
		t.Fatalf("gallery_version = %q after a successful deploy", got.GalleryVersion)
	}

	if err := db.ClearAppGallery(app.ID); err != nil {
		t.Fatalf("ClearAppGallery: %v", err)
	}
	got, _ = db.GetApp(app.ID)
	if got.GalleryItemID != "" || got.GalleryKind != "" || got.GalleryVersion != "" {
		t.Fatalf("detaching left %+v", []string{got.GalleryItemID, got.GalleryKind, got.GalleryVersion})
	}
}

// A detached app must not go on accepting version writes: the version belongs
// to the attachment, and one left behind would render as a claim about a
// catalog this app is no longer part of.
func TestSetGalleryVersionIgnoresADetachedApp(t *testing.T) {
	db := dbtest.New(t)
	app := mkGalleryApp(t, db, "detached", "ext-1", "extension")
	if err := db.ClearAppGallery(app.ID); err != nil {
		t.Fatalf("ClearAppGallery: %v", err)
	}
	if err := db.SetAppGalleryVersion(app.ID, "9.9.9"); err != nil {
		t.Fatalf("SetAppGalleryVersion: %v", err)
	}
	got, _ := db.GetApp(app.ID)
	if got.GalleryVersion != "" {
		t.Fatalf("gallery_version = %q on a detached app", got.GalleryVersion)
	}
}

// "Installed" is derived from live rows. Two copies of one item is a supported
// state — two administrators may each hold their own — and a deleted app stops
// counting the moment it is gone.
func TestListAppsByGalleryItemIsDerivedFromLiveRows(t *testing.T) {
	db := dbtest.New(t)
	a := mkGalleryApp(t, db, "copy-one", "ext-1", "extension")
	b := mkGalleryApp(t, db, "copy-two", "ext-1", "extension")
	mkGalleryApp(t, db, "other", "ext-2", "extension")

	apps, err := db.ListAppsByGalleryItem("ext-1")
	if err != nil {
		t.Fatalf("ListAppsByGalleryItem: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want both copies", len(apps))
	}

	if err := db.DeleteApp(b.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	apps, _ = db.ListAppsByGalleryItem("ext-1")
	if len(apps) != 1 || apps[0].ID != a.ID {
		t.Fatalf("after deletion got %+v", apps)
	}

	all, err := db.ListGalleryApps()
	if err != nil {
		t.Fatalf("ListGalleryApps: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListGalleryApps = %d, want the two surviving attached apps", len(all))
	}
}

func TestGalleryModeIsRefusedOutOfRangeAtTheReadPath(t *testing.T) {
	db := dbtest.New(t)

	// A corrupt row can only be read one safe way: the gallery is not offered.
	if _, err := db.PutSetting(database.SettingGalleryMode, "everything", database.BuiltinAdminID); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if s.GalleryMode != database.GalleryOff {
		t.Fatalf("mode = %q, want off for an unreadable row", s.GalleryMode)
	}

	s.GalleryMode = database.GalleryExtensionsOnly
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
	if s2, _ := db.GetPlatformSettings(); s2.GalleryMode != database.GalleryExtensionsOnly {
		t.Fatalf("mode = %q after a write", s2.GalleryMode)
	}
}

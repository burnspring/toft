package builder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

const (
	goneID   = "22222222-2222-4222-8222-222222222222"
	freshID  = "33333333-3333-4333-8333-333333333333"
	runsGone = "44444444-4444-4444-8444-444444444444"
)

// uuidApp creates an app and returns the id the database gave it — CreateApp
// assigns its own, so a directory must be named after what comes back.
func uuidApp(t *testing.T, db *database.DB, slug string) string {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatal(err)
	}
	app := &database.App{Slug: slug, Name: slug, OwnerID: owner.ID, Status: database.AppStatusStopped}
	if err := db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	return app.ID
}

// liveID is a placeholder in the tests that need no live app.
const liveID = "11111111-1111-4111-8111-111111111111"

// mkOld makes a directory with a file in it, last touched two hours ago.
func mkOld(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// The sweep removes what no app owns and nothing else: a live app's
// directories, one an hour has not passed over, and every dot-directory stay.
func TestTheOrphanSweepRemovesOnlyWhatNoAppOwns(t *testing.T) {
	db := dbtest.New(t)
	apps, runs, logs := t.TempDir(), t.TempDir(), t.TempDir()
	liveID := uuidApp(t, db, "live")
	b := &Builder{db: db, opts: Options{AppsDir: apps, JobRunDir: runs, LogsDir: logs}}

	mkOld(t, filepath.Join(apps, liveID))
	mkOld(t, filepath.Join(apps, goneID))
	mkOld(t, filepath.Join(apps, ".staging"))
	if err := os.MkdirAll(filepath.Join(apps, freshID), 0o755); err != nil {
		t.Fatal(err)
	}
	mkOld(t, filepath.Join(runs, liveID))
	mkOld(t, filepath.Join(runs, runsGone))
	mkOld(t, filepath.Join(logs, liveID))
	mkOld(t, filepath.Join(logs, goneID))

	b.sweepOrphans()

	for _, gone := range []string{filepath.Join(apps, goneID), filepath.Join(runs, runsGone), filepath.Join(logs, goneID)} {
		if exists(gone) {
			t.Errorf("%s is still there — no app owns it", gone)
		}
	}
	for _, kept := range []string{filepath.Join(apps, liveID), filepath.Join(runs, liveID), filepath.Join(logs, liveID),
		filepath.Join(apps, ".staging"), filepath.Join(apps, freshID)} {
		if !exists(kept) {
			t.Errorf("%s was removed — a live app's, a dot-directory, or one inside the grace period", kept)
		}
	}
	if sw := b.LastOrphanSweep(); sw.Removed != 3 {
		t.Errorf("the sweep reports %d removed, want 3", sw.Removed)
	}
}

// A database that lists no apps while app directories exist is a
// misconfiguration, never grounds to delete the estate.
func TestTheOrphanSweepTouchesNothingWhenTheDatabaseListsNoApps(t *testing.T) {
	db := dbtest.New(t)
	apps := t.TempDir()
	b := &Builder{db: db, opts: Options{AppsDir: apps}}
	mkOld(t, filepath.Join(apps, goneID))

	b.sweepOrphans()

	if !exists(filepath.Join(apps, goneID)) {
		t.Error("the sweep deleted an app directory on a database that listed no apps at all")
	}
}

// Deleting an app takes its job-run directory too: each finished run removes
// its own, but the app's level was left behind, one per app, forever.
func TestDeletingAnAppRemovesItsJobRunDirectory(t *testing.T) {
	apps, runs, logs := t.TempDir(), t.TempDir(), t.TempDir()
	mkOld(t, filepath.Join(apps, goneID))
	for _, root := range []string{runs, logs} {
		mkOld(t, filepath.Join(root, goneID))
		mkOld(t, filepath.Join(root, liveID))
	}

	if err := DeleteAppStorage(apps, runs, logs, nil, goneID); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{runs, logs} {
		if exists(filepath.Join(root, goneID)) {
			t.Errorf("the deleted app's directory under %s is still there", filepath.Base(root))
		}
		if !exists(filepath.Join(root, liveID)) {
			t.Errorf("another app's directory under %s went with it", filepath.Base(root))
		}
	}
}

// A log or job-run directory that cannot be removed yet does not keep the app:
// on EFS a file still held open is an `.nfs…` entry nobody can unlink, and a
// deploy still starting writes into the log directory as it goes. Refusing the
// delete left an app nobody could remove; the orphan sweep takes the rest.
func TestADirectoryThatWillNotGoDoesNotKeepTheApp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a read-only directory's entries")
	}
	apps, runs, logs := t.TempDir(), t.TempDir(), t.TempDir()
	mkOld(t, filepath.Join(apps, goneID))
	stuck := filepath.Join(logs, goneID, "held")
	mkOld(t, stuck)
	if err := os.WriteFile(filepath.Join(stuck, ".nfs0000"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	if err := DeleteAppStorage(apps, runs, logs, nil, goneID); err != nil {
		t.Fatalf("a log directory that will not go refused the delete: %v", err)
	}
	if exists(filepath.Join(apps, goneID)) {
		t.Error("the app directory is still there")
	}
}

// A build whose app was deleted under it takes the app's files with it; one
// whose app still exists leaves them alone.
func TestABuildThatOutlivesItsAppTakesItsFilesWithIt(t *testing.T) {
	db := dbtest.New(t)
	apps := t.TempDir()
	liveID := uuidApp(t, db, "live")
	b := &Builder{db: db, opts: Options{AppsDir: apps}}
	mkOld(t, filepath.Join(apps, liveID))
	mkOld(t, filepath.Join(apps, goneID))

	b.dropIfDeleted(goneID)
	b.dropIfDeleted(liveID)

	if exists(filepath.Join(apps, goneID)) {
		t.Error("a deleted app's release tree survived the build that wrote it")
	}
	if !exists(filepath.Join(apps, liveID)) {
		t.Error("a live app's files were removed at the end of its own build")
	}
}

package builder

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"toft/internal/artifact"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/events"
)

func at(minute int) *time.Time {
	t := time.Date(2026, 10, 23, 12, minute, 0, 0, time.UTC)
	return &t
}

// The keep set is the rows': the live release, the pinned ones, anything not
// finished, and the newest keep−1 other successful releases by start time.
func TestRetainedReleasesCountsTheRowsNotTheDisk(t *testing.T) {
	deployments := []database.Deployment{
		{ID: "building", Status: database.DeployStatusBuilding, StartedAt: at(9)},
		{ID: "live", Status: database.DeployStatusSuccess, StartedAt: at(8)},
		{ID: "rollback", Status: database.DeployStatusSuccess, SourceDeployID: "v2", StartedAt: at(7)},
		{ID: "v5", Status: database.DeployStatusSuccess, StartedAt: at(6)},
		{ID: "failed", Status: database.DeployStatusFailed, StartedAt: at(5)},
		{ID: "v4", Status: database.DeployStatusSuccess, StartedAt: at(4)},
		{ID: "v3", Status: database.DeployStatusSuccess, StartedAt: at(3)},
		{ID: "v2", Status: database.DeployStatusSuccess, StartedAt: at(2)},
		{ID: "v1", Status: database.DeployStatusSuccess, StartedAt: at(1)},
	}

	kept, retired := retainedReleases(deployments, "live", 3, map[string]bool{"v1": true})

	for _, id := range []string{"live", "v5", "v4", "v1", "building"} {
		if !kept[id] {
			t.Errorf("%s is not kept", id)
		}
	}
	if want := []string{"v3", "v2"}; !slices.Equal(retired, want) {
		t.Errorf("retired = %v, want %v (newest first)", retired, want)
	}
	if kept["rollback"] || slices.Contains(retired, "rollback") {
		t.Error("a rollback row owns no release and is neither kept nor retired")
	}
	if kept["failed"] || slices.Contains(retired, "failed") {
		t.Error("a failed build is neither a version kept nor one retired")
	}
}

// The live release counts toward keep wherever it sits in the history — a
// promote can make an old build live — and a missing start time sorts last.
func TestRetainedReleasesKeepsAnOldLiveRelease(t *testing.T) {
	deployments := []database.Deployment{
		{ID: "new", Status: database.DeployStatusSuccess, StartedAt: at(3)},
		{ID: "unknown", Status: database.DeployStatusSuccess},
		{ID: "mid", Status: database.DeployStatusSuccess, StartedAt: at(2)},
		{ID: "old", Status: database.DeployStatusSuccess, StartedAt: at(1)},
	}
	kept, retired := retainedReleases(deployments, "old", 2, nil)
	if !kept["old"] || !kept["new"] || len(kept) != 2 {
		t.Errorf("kept = %v, want old and new", kept)
	}
	if want := []string{"mid", "unknown"}; !slices.Equal(retired, want) {
		t.Errorf("retired = %v, want %v", retired, want)
	}
}

// The regression the move off EFS made routine: a task replacement empties
// APPS_DIR, the next deploy's sweep finds one directory on disk, and the store
// used to be pruned to match it — deleting every older rollback target while
// their rows stayed in the history. With a store, what the rows retain is what
// the store keeps.
func TestSweepWithAnEmptiedDiskKeepsTheStoresRollbackTargets(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "sweep", "")
	store, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	appsDir := t.TempDir()
	b := &Builder{db: db, artifacts: store, opts: Options{AppsDir: appsDir, Events: events.NewRecorder(db, nil)}}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.py"), []byte("print(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 5; i++ {
		d := &database.Deployment{AppID: app.ID, Status: database.DeployStatusSuccess}
		if err := db.CreateDeployment(d, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(context.Background(), artifact.ReleaseKey(app.ID, d.ID), src); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
		time.Sleep(2 * time.Millisecond) // started_at orders them
	}
	live := ids[4]
	// Only the live release is on disk, as after a restore.
	if err := os.MkdirAll(ReleaseDir(appsDir, app.ID, live), 0o700); err != nil {
		t.Fatal(err)
	}

	app.AutoDeleteReleases = true
	app.ReleasesKept = 3
	b.sweepReleases(app, live)

	for _, id := range ids[2:] {
		if _, err := store.Stat(context.Background(), artifact.ReleaseKey(app.ID, id)); err != nil {
			t.Errorf("retained release %s left the store: %v", id, err)
		}
		if d, _ := db.GetDeployment(id); d == nil {
			t.Errorf("retained release %s lost its row", id)
		}
	}
	for _, id := range ids[:2] {
		if _, err := store.Stat(context.Background(), artifact.ReleaseKey(app.ID, id)); err == nil {
			t.Errorf("retired release %s is still in the store", id)
		}
		if d, _ := db.GetDeployment(id); d != nil {
			t.Errorf("retired release %s kept its row", id)
		}
	}

	// And the retirement is an event, naming what went (BB-009).
	evs, err := db.ListAppEvents(app.ID, database.AppEventFilter{Kind: string(events.KindReleasesPruned)})
	if err != nil || len(evs) != 1 {
		t.Fatalf("want one releases_pruned event, got %v (%v)", evs, err)
	}
	for _, id := range ids[:2] {
		if !strings.Contains(evs[0].Detail, id) {
			t.Errorf("the event does not name retired release %s: %q", id, evs[0].Detail)
		}
	}
}

// The release a queued or running rollback is returning to is kept by the
// sweep, as the delete route keeps it: a deploy that flipped while the
// rollback waited used to retire it (code review 2026-11-11, F-4).
func TestRetainedReleasesKeepsAnInFlightRollbacksSource(t *testing.T) {
	deployments := []database.Deployment{
		{ID: "live", Status: database.DeployStatusSuccess, StartedAt: at(8)},
		{ID: "rb", Status: database.DeployStatusBuilding, SourceDeployID: "v1", StartedAt: at(7)},
		{ID: "v4", Status: database.DeployStatusSuccess, StartedAt: at(4)},
		{ID: "v3", Status: database.DeployStatusSuccess, StartedAt: at(3)},
		{ID: "v2", Status: database.DeployStatusSuccess, StartedAt: at(2)},
		{ID: "v1", Status: database.DeployStatusSuccess, StartedAt: at(1)},
	}
	kept, retired := retainedReleases(deployments, "live", 2, nil)
	if !kept["v1"] || slices.Contains(retired, "v1") {
		t.Errorf("the rollback's source was retired: kept=%v retired=%v", kept, retired)
	}
	if want := []string{"v3", "v2"}; !slices.Equal(retired, want) {
		t.Errorf("retired = %v, want %v", retired, want)
	}
	app := &database.App{}
	if keep := diskKeep(app, deployments, "live", nil); !keep["v1"] {
		t.Errorf("diskKeep = %v, want the rollback's source kept", keep)
	}
	// A finished rollback protects nothing.
	deployments[1].Status = database.DeployStatusSuccess
	if kept, _ := retainedReleases(deployments, "live", 2, nil); kept["v1"] {
		t.Error("a finished rollback still pinned its source")
	}
}

package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// UpdateApp must not write `status`, for the reason it does not write branch,
// auto_deploy, last_seen_sha or git_token: the builder calls it mid-build with
// an App it loaded before the build started.
//
// This is not hypothetical and it is not cheap. The builder sets the app
// `building`, then persists the framework detection had just produced about a
// hundred milliseconds later — through UpdateApp, carrying an App whose Status
// field still said `sleeping`. Every rebuild in the estate therefore went
// sleeping -> building -> sleeping inside a fifth of a second, and the apps
// list's Building tab never showed a rebuild at all. A *first* build looked
// correct only by luck: that row is inserted `building`, so the stale value it
// clobbered with was the same word.
func TestUpdateAppDoesNotClobberStatus(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "status-clobber")

	// What the builder does at the top of a build.
	if err := db.UpdateAppStatus(app.ID, database.AppStatusBuilding); err != nil {
		t.Fatalf("UpdateAppStatus: %v", err)
	}

	// What the builder does a moment later, holding the App it started with —
	// whose Status is whatever the row said before the build.
	app.Status = database.AppStatusSleeping
	app.Framework = "shiny"
	if err := db.UpdateApp(app); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}

	got, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Status != database.AppStatusBuilding {
		t.Errorf("status = %q, want %q — UpdateApp wrote a stale status back over "+
			"the one the builder had just set, which is what made a rebuild "+
			"invisible in the estate list", got.Status, database.AppStatusBuilding)
	}
	// The columns it *is* responsible for still land.
	if got.Framework != "shiny" {
		t.Errorf("framework = %q, want shiny — UpdateApp must still write what it owns", got.Framework)
	}
}

// The two clocks `toft status` reads. serving_since moves when the release
// does and on nothing else; status_changed_at moves when the word does and not
// when the same word is written again — the builder writes `running` twice
// around a flip.
func TestServingSinceAndStatusChangedAt(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "clocks")

	read := func() *database.App {
		t.Helper()
		got, err := db.GetApp(app.ID)
		if err != nil || got == nil {
			t.Fatalf("GetApp: %v", err)
		}
		return got
	}

	if got := read(); got.StatusChangedAt == nil {
		t.Fatal("status_changed_at nil on a new row, want its insert time")
	}

	// A flip.
	app.CurrentRelease = "app-clocks-d1"
	if err := db.UpdateApp(app); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	first := read().ServingSince
	if first == nil {
		t.Fatal("serving_since nil after current_release changed")
	}

	// The same release written again — the builder's mid-build UpdateApp.
	app.Framework = "flask"
	if err := db.UpdateApp(app); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	if again := read().ServingSince; again == nil || !again.Equal(*first) {
		t.Errorf("serving_since moved to %v on a write that kept the release, want %v", again, first)
	}

	if err := db.UpdateAppStatus(app.ID, database.AppStatusRunning); err != nil {
		t.Fatal(err)
	}
	running := read().StatusChangedAt
	if err := db.UpdateAppStatus(app.ID, database.AppStatusRunning); err != nil {
		t.Fatal(err)
	}
	if again := read().StatusChangedAt; !again.Equal(*running) {
		t.Errorf("status_changed_at moved to %v on the same status, want %v", again, running)
	}
	if err := db.UpdateAppStatus(app.ID, database.AppStatusStopped); err != nil {
		t.Fatal(err)
	}
	if stopped := read().StatusChangedAt; !stopped.After(*running) {
		t.Errorf("status_changed_at = %v after a stop, want later than %v", stopped, running)
	}

	// Nothing serving.
	app.CurrentRelease = ""
	if err := db.UpdateApp(app); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	if got := read().ServingSince; got != nil {
		t.Errorf("serving_since = %v with no release, want nil", got)
	}
}

// The builder's two writes leave what somebody changed during the build alone.
//
// It holds the App it loaded when the build started, and used to save its
// detection and its flip through UpdateApp, which writes name, slug and
// visibility too: a rename made while the build ran came back as the old name
// (the eks e2e run's CLI.18 saw it), and a visibility change could be undone
// the same way -- an app made private during its first build, public again.
func TestTheBuildersWritesDoNotRevertAConcurrentEdit(t *testing.T) {
	db := dbtest.New(t)
	stale := mkApp(t, db, "build-race")

	// Somebody edits the app while the build runs, from a fresh read.
	fresh, err := db.GetApp(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Name = "Renamed meanwhile"
	fresh.Visibility = "private"
	if err := db.UpdateApp(fresh); err != nil {
		t.Fatal(err)
	}

	// The builder saves what it detected, then flips, both from its old copy.
	stale.Visibility = "public"
	stale.Name = "the name at the start of the build"
	if err := db.SetAppDetection(stale.ID, "flask", "app.py", ""); err != nil {
		t.Fatal(err)
	}
	stale.Framework, stale.Entrypoint = "flask", "app.py"
	stale.CurrentRelease, stale.ReleaseRef, stale.RunningMode = "build-race-r1", "ref-1", "shared"
	stale.Port = 4100
	if err := db.SetAppRelease(stale); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetApp(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed meanwhile" || got.Visibility != "private" {
		t.Errorf("the build reverted a concurrent edit: name %q visibility %q", got.Name, got.Visibility)
	}
	if got.Framework != "flask" || got.Entrypoint != "app.py" || got.CurrentRelease != "build-race-r1" ||
		got.ReleaseRef != "ref-1" || int(got.Port) != 4100 {
		t.Errorf("the build's own fields were not written: %+v", got)
	}
	if got.ServingSince == nil {
		t.Error("a flip to a new release did not set serving_since")
	}
}

// The compare-and-set the watcher and the reaper write through: it moves the
// row only from the status the caller read, so a person's Stop that landed
// after the read is kept (JOBS-3, JOBS-6).
func TestUpdateAppStatusFromKeepsAStatusThatMoved(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "status-cas")
	if err := db.UpdateAppStatus(app.ID, database.AppStatusRunning); err != nil {
		t.Fatal(err)
	}

	// A person stops it after the sweep read `running`.
	if err := db.UpdateAppStatus(app.ID, database.AppStatusStopped); err != nil {
		t.Fatal(err)
	}
	moved, err := db.UpdateAppStatusFrom(app.ID, database.AppStatusRunning, database.AppStatusCrashed)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Error("moved = true from running on a row that says stopped")
	}
	if got, _ := db.GetApp(app.ID); got.Status != database.AppStatusStopped {
		t.Errorf("status = %q, want stopped — the person's Stop was overwritten", got.Status)
	}

	moved, err = db.UpdateAppStatusFrom(app.ID, database.AppStatusStopped, database.AppStatusRunning)
	if err != nil || !moved {
		t.Fatalf("moved = %v, %v; want true from the status the row holds", moved, err)
	}
	if got, _ := db.GetApp(app.ID); got.Status != database.AppStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

// The deploy flip's write: it moves the row from anything but `stopped` — the
// build's own `building`, or the live status an overlapping flip wrote — and
// leaves a Stop or a lock made during the build standing (AP-2).
func TestUpdateAppStatusUnlessKeepsAStop(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "status-unless")
	for _, from := range []string{database.AppStatusBuilding, database.AppStatusRunning} {
		db.UpdateAppStatus(app.ID, from)
		moved, err := db.UpdateAppStatusUnless(app.ID, database.AppStatusStopped, database.AppStatusReady)
		if err != nil || !moved {
			t.Fatalf("from %s: moved = %v, %v; want true", from, moved, err)
		}
	}
	db.UpdateAppStatus(app.ID, database.AppStatusStopped)
	moved, err := db.UpdateAppStatusUnless(app.ID, database.AppStatusStopped, database.AppStatusRunning)
	if err != nil || moved {
		t.Fatalf("from stopped: moved = %v, %v; want false", moved, err)
	}
	if got, _ := db.GetApp(app.ID); got.Status != database.AppStatusStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}
}

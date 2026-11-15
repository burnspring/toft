package builder

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// Two deploys of one app queued seconds apart both read the app when they
// began, so both named the first release as the one to retire: the second
// release's workload was never deleted, and its log stayed open on EFS
// (trytoft, 2026-10-28, E1b). The flip reads what is serving now.
func TestTheFlipRetiresWhatIsServingNotWhatWasWhenTheDeployBegan(t *testing.T) {
	db := dbtest.New(t)
	b := &Builder{db: db}
	id := uuidApp(t, db, "flip")

	// The deploy's snapshot, taken while release a was serving.
	stale, err := db.GetApp(id)
	if err != nil {
		t.Fatal(err)
	}
	stale.CurrentRelease = "flip-a"

	// Meanwhile a second deploy flipped to b.
	now, err := db.GetApp(id)
	if err != nil {
		t.Fatal(err)
	}
	now.CurrentRelease = "flip-b"
	now.RunningMode = "shared"
	if err := db.SetAppRelease(now); err != nil {
		t.Fatal(err)
	}

	unlock := b.servingBeforeFlip(stale)
	if stale.CurrentRelease != "flip-b" {
		t.Errorf("the flip would retire %q; b is the release serving", stale.CurrentRelease)
	}

	// And a second flip of the same app waits for the first to retire its
	// previous release, so it reads the row after the first has written it.
	second := &database.App{ID: id}
	done := make(chan struct{})
	go func() {
		u := b.servingBeforeFlip(second)
		u()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a second flip of the same app ran while the first held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second flip never ran after the first released the lock")
	}
}

// A second rollback to the release already serving shares its workload name,
// so the builder asks the row before starting it and before a failure deletes
// it (security review 2026-11-14, PB-1).
func TestIsServingAnswersFromTheRowAsItIsNow(t *testing.T) {
	db := dbtest.New(t)
	b := &Builder{db: db}
	id := uuidApp(t, db, "serving")

	if b.isServing(id, "serving-a") {
		t.Fatal("nothing has flipped yet, but serving-a reads as serving")
	}
	app, err := db.GetApp(id)
	if err != nil {
		t.Fatal(err)
	}
	app.CurrentRelease = "serving-a"
	app.RunningMode = "shared"
	if err := db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}
	if !b.isServing(id, "serving-a") {
		t.Error("serving-a was flipped to, but does not read as serving")
	}
	if b.isServing(id, "serving-b") {
		t.Error("serving-b reads as serving; serving-a is")
	}
	if b.isServing(id, "") {
		t.Error("an empty name reads as serving")
	}
}

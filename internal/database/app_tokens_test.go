package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A deploy keeps the incoming workload's token and revokes every other
// workload's — and it is keyed on the workload rather than on a token id
// because in isolated mode the deploy mints none: the pod mints its own when it
// bootstraps, which has already happened by the time Deploy reports ready.
func TestADeployKeepsOnlyTheIncomingWorkloadsTokens(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")

	old := &database.AppToken{
		ID: "tok-old", AppID: app.ID, Workload: "demo-dep1",
		TokenHash: "hash-old", Prefix: "tofta_old",
	}
	incoming := &database.AppToken{
		ID: "tok-new", AppID: app.ID, Workload: "demo-dep2",
		TokenHash: "hash-new", Prefix: "tofta_new",
	}
	for _, tok := range []*database.AppToken{old, incoming} {
		if err := db.CreateAppToken(tok); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.RevokeAppTokensExceptWorkload(app.ID, "demo-dep2"); err != nil {
		t.Fatal(err)
	}

	if got, _ := db.ResolveAppToken("hash-old"); got != nil {
		t.Error("the outgoing release's token is still live after its replacement went up")
	}
	if got, _ := db.ResolveAppToken("hash-new"); got == nil {
		t.Fatal("the incoming workload's token was revoked by its own deploy")
	}
}

// Superseding is the *restart* rule and is narrower: only this workload's own
// earlier starts, because the process holding those is gone. A different
// release serving beside it during a deploy must keep working.
func TestAWorkloadSupersedesOnlyItsOwnEarlierStarts(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")

	for _, tok := range []*database.AppToken{
		{ID: "a1", AppID: app.ID, Workload: "demo-dep1", TokenHash: "h-a1", Prefix: "tofta_a1"},
		{ID: "a2", AppID: app.ID, Workload: "demo-dep1", TokenHash: "h-a2", Prefix: "tofta_a2"},
		{ID: "b1", AppID: app.ID, Workload: "demo-dep2", TokenHash: "h-b1", Prefix: "tofta_b1"},
	} {
		if err := db.CreateAppToken(tok); err != nil {
			t.Fatal(err)
		}
	}

	// The second start of demo-dep1 supersedes the first, and nothing else.
	if err := db.SupersedeWorkloadTokens(app.ID, "demo-dep1", "a2"); err != nil {
		t.Fatal(err)
	}

	if got, _ := db.ResolveAppToken("h-a1"); got != nil {
		t.Error("the previous start's token outlived the process holding it")
	}
	if got, _ := db.ResolveAppToken("h-a2"); got == nil {
		t.Error("the current start's token was revoked")
	}
	if got, _ := db.ResolveAppToken("h-b1"); got == nil {
		t.Error("another release's token was revoked; a rolling deploy would break it")
	}
}

// An empty workload matches nothing, in both directions. Legacy rows carry no
// workload, and a query that read "" as a wildcard would revoke every one of
// them on the first restart after this shipped.
func TestAnEmptyWorkloadRevokesNothing(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")

	legacy := &database.AppToken{
		ID: "legacy", AppID: app.ID, TokenHash: "h-legacy", Prefix: "tofta_leg",
	}
	if err := db.CreateAppToken(legacy); err != nil {
		t.Fatal(err)
	}

	if err := db.SupersedeWorkloadTokens(app.ID, "", "nothing"); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAppTokensExceptWorkload(app.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ResolveAppToken("h-legacy"); got == nil {
		t.Error("a token with no workload was revoked by a call that names none")
	}
}

// A lock stops the app, but a stop that failed leaves its token unrevoked —
// so the resolved token carries the lock itself, and the auth middleware can
// refuse on it without trusting the status to have followed.
func TestAResolvedTokenCarriesTheAppsLock(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	if err := db.CreateAppToken(&database.AppToken{
		ID: "tok", AppID: app.ID, Workload: "demo-dep1", TokenHash: "hash", Prefix: "tofta_x",
	}); err != nil {
		t.Fatal(err)
	}

	if got, _ := db.ResolveAppToken("hash"); got == nil || got.AppLocked {
		t.Fatalf("before the lock: %+v", got)
	}
	if err := db.SetAppLock(app.ID, "", "held for review", time.Now(), app.Slug, database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ResolveAppToken("hash"); got == nil || !got.AppLocked {
		t.Fatalf("after the lock: %+v, want AppLocked", got)
	}
}

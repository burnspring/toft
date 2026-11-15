package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func TestClaimAutoDeploySHA(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "claim")

	// First claim from the empty seed wins.
	won, err := db.ClaimAutoDeploySHA(app.ID, "", "sha-1")
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}

	// A node still holding the stale old value must lose — this is the
	// multi-node guarantee.
	won, err = db.ClaimAutoDeploySHA(app.ID, "", "sha-2")
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	if won {
		t.Fatal("stale claim won; expected the guarded UPDATE to reject it")
	}

	// Advancing from the current value wins again.
	won, err = db.ClaimAutoDeploySHA(app.ID, "sha-1", "sha-2")
	if err != nil || !won {
		t.Fatalf("advancing claim: won=%v err=%v", won, err)
	}

	got, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.LastSeenSHA != "sha-2" {
		t.Fatalf("LastSeenSHA = %q, want sha-2", got.LastSeenSHA)
	}
}

func TestSetAutoDeploySeedsAndKeeps(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "toggle")

	if err := db.SetAutoDeploy(app.ID, true, "seed-sha"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ := db.GetApp(app.ID)
	if !got.AutoDeploy || got.LastSeenSHA != "seed-sha" {
		t.Fatalf("after enable: auto=%v sha=%q", got.AutoDeploy, got.LastSeenSHA)
	}

	// Disabling must not wipe the seed: re-enabling soon after should not
	// re-deploy a commit that is already live.
	if err := db.SetAutoDeploy(app.ID, false, ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ = db.GetApp(app.ID)
	if got.AutoDeploy || got.LastSeenSHA != "seed-sha" {
		t.Fatalf("after disable: auto=%v sha=%q", got.AutoDeploy, got.LastSeenSHA)
	}
}

func TestListAutoDeployApps(t *testing.T) {
	db := dbtest.New(t)

	gitApp := mkApp(t, db, "gitapp")
	if err := db.SetAutoDeploy(gitApp.ID, true, ""); err != nil {
		t.Fatalf("SetAutoDeploy: %v", err)
	}

	// Enabled but sourced from an upload: nothing to poll.
	uploaded := mkApp(t, db, "zipapp")
	if _, err := db.Conn().Exec(`UPDATE apps SET repo_url='upload://app.zip', auto_deploy=TRUE WHERE id=?`, uploaded.ID); err != nil {
		t.Fatalf("marking upload app: %v", err)
	}

	// Git remote but auto-deploy off.
	mkApp(t, db, "offapp")

	apps, err := db.ListAutoDeployApps()
	if err != nil {
		t.Fatalf("ListAutoDeployApps: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != gitApp.ID {
		t.Fatalf("got %d apps, want exactly the git app", len(apps))
	}
}

func TestHasPendingDeployment(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "pending")

	cutoff := time.Now().UTC().Add(-30 * time.Minute)

	if has, err := db.HasPendingDeployment(app.ID, cutoff); err != nil || has {
		t.Fatalf("empty app: has=%v err=%v", has, err)
	}

	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusPending}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	if has, _ := db.HasPendingDeployment(app.ID, cutoff); !has {
		t.Fatal("fresh pending deployment not detected")
	}

	// A pending row older than the cutoff is a restart orphan and must not block.
	if has, _ := db.HasPendingDeployment(app.ID, time.Now().UTC().Add(time.Minute)); has {
		t.Fatal("stale pending deployment should be ignored")
	}

	// Once the build is picked up, the queue slot is free again.
	d.Status = database.DeployStatusBuilding
	if err := db.UpdateDeployment(d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if has, _ := db.HasPendingDeployment(app.ID, cutoff); has {
		t.Fatal("building deployment should not count as pending")
	}
}

func TestLatestSuccessfulSHA(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "seed")

	if sha, err := db.LatestSuccessfulSHA(app.ID); err != nil || sha != "" {
		t.Fatalf("no deployments: sha=%q err=%v", sha, err)
	}

	older := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess, CommitSHA: "old-sha"}
	if err := db.CreateDeployment(older, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := db.Conn().Exec(`UPDATE deployments SET started_at=? WHERE id=?`, past, older.ID); err != nil {
		t.Fatalf("backdating: %v", err)
	}

	newer := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess, CommitSHA: "new-sha"}
	if err := db.CreateDeployment(newer, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	failed := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusFailed, CommitSHA: "bad-sha"}
	if err := db.CreateDeployment(failed, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	sha, err := db.LatestSuccessfulSHA(app.ID)
	if err != nil {
		t.Fatalf("LatestSuccessfulSHA: %v", err)
	}
	if sha != "new-sha" {
		t.Fatalf("sha = %q, want new-sha", sha)
	}
}

func TestCreateDeploymentDefaultsTrigger(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "trigger")

	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusPending}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got, err := db.GetDeployment(d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.TriggeredBy != database.DeployTriggerManual {
		t.Fatalf("TriggeredBy = %q, want manual default", got.TriggeredBy)
	}
}

// last_seen_tree is cleared wherever last_seen_sha is, so a re-pointed app's
// first deploy is full: a tree recorded against another repository, another
// folder or a seed commit says nothing about what the new tip would build.
func TestTheFolderTreeIsClearedWithTheSeenCommit(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "tree")

	tree := func() string {
		t.Helper()
		got, err := db.GetApp(app.ID)
		if err != nil || got == nil {
			t.Fatalf("GetApp: %v", err)
		}
		return got.LastSeenTree
	}
	seen := func() {
		t.Helper()
		if err := db.SetLastSeen(app.ID, "abc123", "tree1"); err != nil {
			t.Fatalf("SetLastSeen: %v", err)
		}
		if got := tree(); got != "tree1" {
			t.Fatalf("last_seen_tree = %q after SetLastSeen, want tree1", got)
		}
	}

	for _, c := range []struct {
		name  string
		clear func() error
	}{
		{"the folder changes", func() error { return db.SetAppRepoSubdir(app.ID, "apps/other") }},
		{"the repository is re-pointed", func() error { return db.SetAppRepoURL(app.ID, "https://github.com/example/moved") }},
		{"auto-deploy is turned on", func() error { return db.SetAutoDeploy(app.ID, true, "abc123") }},
	} {
		seen()
		if err := c.clear(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := tree(); got != "" {
			t.Errorf("%s: last_seen_tree = %q, want it cleared", c.name, got)
		}
	}
}

// DG-13 (security review 2026-11-14): a finishing build never rewinds a claim
// the poller made after it cloned, and the in-flight read sees a building row
// once its commit is recorded.
func TestAdvanceLastSeenDoesNotOverwriteALaterClaim(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "advance")
	if err := db.SetLastSeen(app.ID, "x", ""); err != nil {
		t.Fatal(err)
	}
	// The build of x cloned while last_seen was x; then y was claimed.
	if won, err := db.ClaimAutoDeploySHA(app.ID, "x", "y"); err != nil || !won {
		t.Fatalf("claim y: won=%v err=%v", won, err)
	}
	if err := db.AdvanceLastSeen(app.ID, "x", "", "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetApp(app.ID); got.LastSeenSHA != "y" {
		t.Errorf("last_seen_sha = %q, want the later claim y kept", got.LastSeenSHA)
	}
	// Unmoved since the clone: the build's commit is written.
	if err := db.AdvanceLastSeen(app.ID, "z", "", "y"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetApp(app.ID); got.LastSeenSHA != "z" {
		t.Errorf("last_seen_sha = %q, want z", got.LastSeenSHA)
	}
}

func TestInFlightDeploymentsSeesABuildingCommit(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "inflight")
	cutoff := time.Now().UTC().Add(-30 * time.Minute)
	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusBuilding}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatal(err)
	}
	if uncloned, building, err := db.InFlightDeployments(app.ID, cutoff); err != nil || !uncloned || len(building) != 0 {
		t.Fatalf("building, no commit: uncloned=%v building=%v err=%v", uncloned, building, err)
	}
	if err := db.SetDeploymentCommit(d.ID, "abc"); err != nil {
		t.Fatal(err)
	}
	if uncloned, building, _ := db.InFlightDeployments(app.ID, cutoff); uncloned || len(building) != 1 || building[0] != "abc" {
		t.Errorf("building abc: uncloned=%v building=%v", uncloned, building)
	}
}

// Build output is tool output: one NUL byte or invalid UTF-8 sequence in it
// failed the whole terminal write, so the row stayed `building` with no log
// (security review 2026-11-14, PB-2). Both setters clean what they store.
func TestADeploymentLogWithBadBytesStillLands(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "badbytes")

	d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusBuilding}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	bad := "a\x00b\xffc"
	if err := db.SetDeploymentFailure(d.ID, "build_failed", "", bad); err != nil {
		t.Fatalf("SetDeploymentFailure: %v", err)
	}
	now := time.Now().UTC()
	d.Status = database.DeployStatusFailed
	d.BuildLog = bad
	d.FinishedAt = &now
	if err := db.UpdateDeployment(d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}

	got, err := db.GetDeployment(d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.Status != database.DeployStatusFailed || got.FinishedAt == nil {
		t.Errorf("status = %q, finished_at = %v; want failed and set", got.Status, got.FinishedAt)
	}
	want := "a�b�c"
	if got.BuildLog != want {
		t.Errorf("build_log = %q, want %q", got.BuildLog, want)
	}
	if got.FailureMessage != want {
		t.Errorf("failure_message = %q, want %q", got.FailureMessage, want)
	}
}

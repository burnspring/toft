package builder

import (
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func strp(s string) *string { return &s }

// Serving reads the release, never the wish. With the app renamed and its
// release still built for the old slug, every reader that starts or describes
// the running code must answer the old path — a woken app started at the new
// one would be a Next.js app with a blank page.
func TestServingReadsTheReleaseNotTheWish(t *testing.T) {
	app := &database.App{Slug: "new", CurrentRelease: "old-abc", ReleaseBasePath: strp("/apps/old")}
	if got := ServingBasePath(app); got != "/apps/old" {
		t.Errorf("ServingBasePath = %q, want the release's /apps/old", got)
	}
	if got := BasePathFor(app); got != "/apps/new" {
		t.Errorf("BasePathFor = %q, want /apps/new", got)
	}
	if !MoveNeeded(app) {
		t.Error("a release built for another path needs a move")
	}

	app.Hostname, app.HostnameEnabled = "reports.example.com", true
	if got := BasePathFor(app); got != "" {
		t.Errorf("BasePathFor with a hostname on = %q, want \"\"", got)
	}
	if got := ServingBasePath(app); got != "/apps/old" {
		t.Errorf("switching the hostname on moved the serving path to %q before any flip", got)
	}

	// A recorded name that is switched off is only a record.
	app.HostnameEnabled = false
	if got := BasePathFor(app); got != "/apps/new" {
		t.Errorf("BasePathFor with the hostname off = %q", got)
	}

	// No release: nothing to move, and the gateway path for anyone asking.
	idle := &database.App{Slug: "idle"}
	if MoveNeeded(idle) || ServingBasePath(idle) != "/apps/idle" {
		t.Error("an app with nothing serving has nothing to move and answers its gateway path")
	}
}

// The flip writes the release's base path in the statement that names the
// release, so the two change at one instant.
func TestTheFlipRecordsTheReleaseBasePath(t *testing.T) {
	db := dbtest.New(t)
	id := uuidApp(t, db, "flipped")
	app, _ := db.GetApp(id)
	if app.ReleaseBasePath != nil {
		t.Fatalf("a new app has a serving base path %q", *app.ReleaseBasePath)
	}
	app.CurrentRelease = "flipped-1"
	app.RunningMode = "shared"
	app.ReleaseBasePath = strp("")
	if err := db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetApp(id)
	if got.ReleaseBasePath == nil || *got.ReleaseBasePath != "" {
		t.Errorf("release_base_path after the flip = %v, want \"\"", got.ReleaseBasePath)
	}
}

func queueingBuilder(t *testing.T, db *database.DB) *Builder {
	t.Helper()
	return &Builder{db: db, queue: make(chan *BuildJob, 4), opts: Options{AppsDir: t.TempDir()}}
}

// A rename is a move: the release is rebuilt at the new slug, from the commit
// already running, without advancing the poller.
func TestARenameQueuesAMoveOfTheRunningCommit(t *testing.T) {
	db := dbtest.New(t)
	b := queueingBuilder(t, db)
	id := uuidApp(t, db, "before")

	live := &database.Deployment{AppID: id, CommitSHA: "a91f3c2", Status: database.DeployStatusSuccess}
	if err := db.CreateDeployment(live, nil); err != nil {
		t.Fatal(err)
	}
	app, _ := db.GetApp(id)
	app.CurrentRelease = "before-" + strings.ReplaceAll(live.ID, "-", "")[:8]
	app.ReleaseRef = "local:" + ReleaseDir(b.opts.AppsDir, id, live.ID)
	app.RunningMode = "shared"
	app.ReleaseBasePath = strp("/apps/before")
	if err := db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}

	// Nothing moved yet: no move.
	if queued, err := b.QueueMove(id); err != nil || queued {
		t.Fatalf("QueueMove with nothing to move = %v, %v", queued, err)
	}

	app.Slug = "after"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	queued, err := b.QueueMove(id)
	if err != nil || !queued {
		t.Fatalf("QueueMove after a rename = %v, %v", queued, err)
	}
	job := <-b.queue
	if !job.Reseat {
		t.Error("a move must be a Reseat — a rebuild of the running code, never a fresh clone")
	}
	if job.Deployment.TriggeredBy != database.DeployTriggerMove {
		t.Errorf("triggered_by = %q, want move", job.Deployment.TriggeredBy)
	}
	if job.Deployment.CommitSHA != "a91f3c2" {
		t.Errorf("a move carries commit %q, want the live a91f3c2", job.Deployment.CommitSHA)
	}
	// TOFT-BB-001: the row names the release it rebuilt, on the row as well
	// as the job, or the next rollback reads "previous" off build time.
	if job.Deployment.RestoredFrom != live.ID {
		t.Errorf("a move's restored_from = %q, want the live %s", job.Deployment.RestoredFrom, live.ID)
	}
	if row, _ := db.GetDeployment(job.Deployment.ID); row == nil || row.RestoredFrom != live.ID {
		t.Errorf("the move's row does not record restored_from: %+v", row)
	}
	if job.advancesPoller() {
		t.Error("a move must not tell the poller its commit shipped")
	}
	if job.settleAs != database.AppStatusStopped {
		t.Errorf("a stopped app's move settles as %q; it must stay stopped", job.settleAs)
	}
	if job.stampsCommit() {
		t.Error("a move's tree is a copy; its commit is the live one, not read off the tree")
	}

	// One queued build covers the move: a second one is not stacked.
	if queued, _ := b.QueueMove(id); queued {
		t.Error("a second move was queued behind a pending one")
	}
}

// A locked app is never moved by the floor beneath the handlers.
func TestALockedAppIsNotMoved(t *testing.T) {
	db := dbtest.New(t)
	b := queueingBuilder(t, db)
	id := uuidApp(t, db, "locked")
	app, _ := db.GetApp(id)
	app.CurrentRelease = "locked-1"
	app.RunningMode = "shared"
	app.ReleaseBasePath = strp("/apps/elsewhere")
	if err := db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAppLock(id, database.BuiltinAdminID, "", time.Now().UTC(), "locked", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if queued, _ := b.QueueMove(id); queued {
		t.Error("a move was queued for a locked app")
	}
}

// A rollback whose target was built for another base path — or for one
// nobody recorded — is rebuilt from that release's tree; one built for the
// path the app has now starts exactly as built.
func TestARollbackAcrossBasePathsIsRebuilt(t *testing.T) {
	db := dbtest.New(t)
	b := queueingBuilder(t, db)
	id := uuidApp(t, db, "rolled")
	app, _ := db.GetApp(id)

	mk := func(basePath *string) *database.Deployment {
		d := &database.Deployment{AppID: id, CommitSHA: "c0ffee", Status: database.DeployStatusSuccess}
		if err := db.CreateDeployment(d, nil); err != nil {
			t.Fatal(err)
		}
		if basePath != nil {
			if err := db.SetDeploymentBasePath(d.ID, *basePath); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}

	for _, tc := range []struct {
		name    string
		built   *string
		rebuild bool
	}{
		{"same path", strp("/apps/rolled"), false},
		{"another slug", strp("/apps/older"), true},
		{"a hostname's root", strp(""), true},
		{"unrecorded", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := mk(tc.built)
			job := &BuildJob{App: app, Deployment: &database.Deployment{AppID: id}, SourceDeployID: src.ID}
			var log []string
			b.rebuildRollbackAcrossPaths(job, func(l string) { log = append(log, l) })
			if job.Reseat != tc.rebuild {
				t.Fatalf("Reseat = %v, want %v", job.Reseat, tc.rebuild)
			}
			if !tc.rebuild {
				if job.SourceDeployID != src.ID || len(log) != 0 {
					t.Error("a rollback to a release built for this path must start as built")
				}
				return
			}
			if job.SourceDeployID != "" || job.reseatFrom != src.ID {
				t.Errorf("rebuilt rollback: SourceDeployID %q, reseatFrom %q", job.SourceDeployID, job.reseatFrom)
			}
			if job.Deployment.CommitSHA != "c0ffee" {
				t.Errorf("the rebuilt rollback carries %q, not its target's commit", job.Deployment.CommitSHA)
			}
			if len(log) != 1 || !strings.Contains(log[0], "rebuilding for /apps/rolled") {
				t.Errorf("the log does not say why: %v", log)
			}
		})
	}
}

// A rebuild of a rebuild names the original release, so a chain of moves and
// rebuilt rollbacks still says which release is serving (TOFT-BB-001).
func TestOriginalReleaseFollowsARebuild(t *testing.T) {
	if got := OriginalRelease(&database.Deployment{ID: "v5"}); got != "v5" {
		t.Errorf("an ordinary release = %q, want itself", got)
	}
	if got := OriginalRelease(&database.Deployment{ID: "m1", RestoredFrom: "v5"}); got != "v5" {
		t.Errorf("a move of v5 = %q, want v5", got)
	}
	// A plain rollback's transient row still starts its source's tree.
	if got := OriginalRelease(&database.Deployment{ID: "t", SourceDeployID: "v5", RestoredFrom: "v5"}); got != "t" {
		t.Errorf("a transient rollback row = %q, want itself", got)
	}
}

func TestRollbackRebuildsAcrossBasePaths(t *testing.T) {
	app := &database.App{Slug: "new"}
	if RollbackRebuilds(&database.Deployment{BasePath: strp("/apps/new")}, app) {
		t.Error("a release built for the current path is started, not rebuilt")
	}
	if !RollbackRebuilds(&database.Deployment{BasePath: strp("/apps/old")}, app) {
		t.Error("a release built for the old slug must be rebuilt")
	}
	if !RollbackRebuilds(&database.Deployment{}, app) {
		t.Error("a release with no recorded base path must be rebuilt")
	}
}

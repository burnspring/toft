package builder

import (
	"fmt"
	"log"
	"time"

	"toft/internal/database"
)

// A move is how a running app changes address: a rebuild of the code already
// serving, at the base path the app should live at now. Three things queue
// one — a slug rename, a hostname switched on or off, and a hostname edited
// while on — and the flip queues another when the address moved again while a
// build was already past computing its path. Nothing else coordinates them:
// the build reads the wish when it starts, the flip compares it again under
// the flip lock, and a move with nothing to do is never queued.

// MoveNeeded reports whether an app's serving release was built for an address
// other than the one it should have. An app with nothing serving has nothing
// to move; its next deploy builds for the right path on its own.
func MoveNeeded(app *database.App) bool {
	return app.CurrentRelease != "" && ServingBasePath(app) != BasePathFor(app)
}

// QueueMove queues a move of one app if it needs one, and says whether it did.
//
// Read fresh rather than from a caller's App, because every caller has just
// written something that changes the answer. Not queued — and not an error —
// when the app is locked (the handlers refuse the change before it is saved;
// this is the floor under the flip-time caller), when a deployment is already
// pending (that build computes the path itself, which is the move), or when
// the address did not in fact change.
func (b *Builder) QueueMove(appID string) (bool, error) {
	app, err := b.db.GetApp(appID)
	if err != nil || app == nil {
		return false, err
	}
	if !MoveNeeded(app) || app.Locked() {
		return false, nil
	}
	pending, err := b.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-PendingDeployWindow))
	if err != nil || pending {
		return false, err
	}

	// The commit already running, which is what a move rebuilds. Empty for an
	// uploaded app, which has none.
	//
	// And which release that is, on the row as restored_from: the move's row
	// is the newest of all, and without it "the release before the one
	// serving" was read from build time — so a rollback after a rename went
	// forward to the newest release rather than back from the one rebuilt
	// (TOFT-BB-001). A release that is itself a rebuild names the original.
	var sha, rebuilt string
	if liveID := LiveReleaseID(b.opts.AppsDir, app); liveID != "" {
		rebuilt = liveID
		if live, err := b.db.GetDeployment(liveID); err == nil && live != nil {
			sha = live.CommitSHA
			rebuilt = OriginalRelease(live)
		}
	}
	deploy := &database.Deployment{
		AppID:        app.ID,
		Branch:       app.Branch,
		CommitSHA:    sha,
		Status:       database.DeployStatusPending,
		TriggeredBy:  database.DeployTriggerMove,
		RestoredFrom: rebuilt,
	}
	// No deploy record: a move ships no new code, and whoever changed the
	// address is on the audit row of the change itself.
	if err := b.db.CreateDeployment(deploy, nil); err != nil {
		return false, err
	}
	job := &BuildJob{App: app, Deployment: deploy, Reseat: true}
	if app.Status == database.AppStatusStopped || app.Status == database.AppStatusSleeping {
		job.settleAs = app.Status
	}
	b.Enqueue(job)
	return true, nil
}

// freshApp is the app as the database has it now, for a question whose answer
// may have changed since the build was queued; the build's own App when the
// read fails, which is the answer it would have had anyway.
func (b *Builder) freshApp(app *database.App) *database.App {
	if fresh, err := b.db.GetApp(app.ID); err == nil && fresh != nil {
		return fresh
	}
	return app
}

// describeBasePath is a base path as a log line says it: "/" for the root of a
// hostname rather than an empty string nobody can see.
func describeBasePath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// queueMoveAfterFlip is the flip's reconcile: the release just flipped was
// built for basePath, and if the app should now live somewhere else another
// move is queued. In a goroutine, because the caller is a build worker holding
// the flip lock and the queue may be full of jobs waiting for that worker.
func (b *Builder) queueMoveAfterFlip(appID string) {
	go func() {
		if _, err := b.QueueMove(appID); err != nil {
			log.Printf("Warning: queueing a move of %s after its flip: %v", appID, err)
		}
	}()
}

// rebuildRollbackAcrossPaths turns a rollback into a Reseat of its target's
// tree when that tree was built for a base path other than the one the app
// should have now.
//
// Started as it is, such a release was told an address the app no longer
// has — a slug since renamed, a hostname since switched — and would report
// success while serving 404s (for Next.js, a blank page). Rebuilt, it is slower
// and can fail on a dependency since gone, which is a failure the log names
// rather than one nobody sees. A release from before base paths were recorded
// is rebuilt for the same reason: nothing says what it was built for.
func (b *Builder) rebuildRollbackAcrossPaths(job *BuildJob, appendLog func(string)) {
	if job.SourceDeployID == "" {
		return
	}
	src, err := b.db.GetDeployment(job.SourceDeployID)
	if err != nil || src == nil {
		return // the rollback path reports a missing release in its own words
	}
	app := b.freshApp(job.App)
	if !RollbackRebuilds(src, app) {
		return
	}
	wish := BasePathFor(app)
	built := "an unrecorded base path"
	if src.BasePath != nil {
		built = describeBasePath(*src.BasePath)
	}
	appendLog(fmt.Sprintf("==> Release %s was built for %s; rebuilding for %s", src.ID, built, describeBasePath(wish)))
	job.reseatFrom, job.SourceDeployID, job.Reseat = src.ID, "", true
	job.Deployment.CommitSHA = src.CommitSHA
}

// RollbackRebuilds reports whether making src current again for app is a
// rebuild rather than a start of its tree: the tree was built for a base path
// other than the one the app should have now, or for one nobody recorded.
// The rollback handler asks it to say so in its answer, and the builder to do
// it, so the two cannot disagree.
func RollbackRebuilds(src *database.Deployment, app *database.App) bool {
	return src.BasePath == nil || *src.BasePath != BasePathFor(app)
}

// OriginalRelease is the release a row is: the one it restored or rebuilt,
// when it is a rebuild (a move, or a rollback across base paths), else itself.
// What a new rebuild records as restored_from, so a chain of them names the
// first rather than the rebuild before.
func OriginalRelease(d *database.Deployment) string {
	if d.RestoredFrom != "" && d.SourceDeployID == "" && d.ID != d.RestoredFrom {
		return d.RestoredFrom
	}
	return d.ID
}

// RetireInvalidHostnames switches off every served hostname this install's
// configuration now refuses — a BASE_URL changed so that a stored name is the
// platform's own, or a parent or child of it — and moves each such app back to
// its gateway path. Run at startup, before anything is served: a name the
// validator refuses is never served, and the record of it is kept so the
// settings page can say why it is off.
func (b *Builder) RetireInvalidHostnames(reserved, extra []string) {
	enabled, err := b.db.ListEnabledHostnames()
	if err != nil {
		log.Printf("Warning: reading app hostnames to validate them: %v", err)
		return
	}
	for id, name := range enabled {
		if _, err := database.ParseAppHostname(name, reserved, extra); err == nil {
			continue
		} else {
			log.Printf("App %s: its hostname %s is no longer valid on this install (%v); switching it off", id, name, err)
		}
		if err := b.db.SetAppHostname(id, name, true, name, false, database.PrincipalTypeSystem, "", database.RequestMeta{}); err != nil {
			log.Printf("Warning: switching off the hostname of %s: %v", id, err)
			continue
		}
		if _, err := b.QueueMove(id); err != nil {
			log.Printf("Warning: moving %s back to its gateway path: %v", id, err)
		}
	}
}

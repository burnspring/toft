package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"toft/internal/artifact"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
	"toft/internal/telemetry"
)

// restoreTimeout bounds a single release fetch. Boot cost is proportional to
// app count and artifact size, and this is the accepted price of local-disk
// steady-state reads; the bound is what stops one unreachable artifact holding
// the whole reconcile pass.
const restoreTimeout = 10 * time.Minute

// publishTimeout bounds packing and uploading one release. Longer than a
// restore's, because packing reads the whole tree before the upload starts.
var publishTimeout = 15 * time.Minute

// publishSayEvery is how often a long publish says how far it has got.
var publishSayEvery = 15 * time.Second

// ArtifactResolver turns a release reference into a directory to exec out of,
// fetching and unpacking the artifact when the directory is not already there.
//
// This is where restore-on-boot actually happens, and it is deliberately not
// in the reconciler: the reconciler asks the backend to start a workload from
// a reference, the backend asks its resolver for a directory, and neither of
// them has to know whether that involved a network. An isolated backend
// resolves the same reference into its own container filesystem the same way.
type ArtifactResolver struct {
	// Store is where artifacts live. Nil means the install offers no artifact
	// store, and only local references resolve — the VM case, unchanged.
	Store artifact.Store
	// AppsDir is the root the release tree is unpacked under.
	AppsDir string
}

func (r ArtifactResolver) Resolve(ref runtime.ReleaseRef) (string, error) {
	switch ref.Kind {
	case runtime.ReleaseLocal:
		// No artifact store means no replace-on-start, and that is stated here
		// rather than silently skipped. A local release has nothing to be
		// restored *from*: the directory is the only copy, and replacing it
		// with itself is not a thing that can be done. An install with no
		// object store therefore keeps the persistence every install used to
		// have — a write into a release tree outlives a restart there — which
		// is the optional-capability rule as it already reads. Absent, not
		// dormant, and not a degraded version of the capability either.
		return runtime.LocalResolver{}.Resolve(ref)

	case runtime.ReleaseArtifact:
		if r.Store == nil {
			return "", fmt.Errorf("%w: this install has no artifact store configured",
				runtime.ErrUnresolvableRelease)
		}
		appID, deployID, err := parseReleaseKey(ref.Value)
		if err != nil {
			return "", err
		}
		dir := ReleaseDir(r.AppsDir, appID, deployID)

		// A tree on disk is what a shared start runs; the store is consulted
		// only when the tree is absent (ADR §7, mode-scoped). Replacing it on
		// every start was the rule, and shared mode never kept it: the cost is a
		// fetch and an unpack on every wake — minutes for an R app — against an
		// edit the app's own account can persist anywhere else it may write.
		// A redeploy, a rollback to another release and an emptied disk still
		// replace the tree; an isolated pod fetches on every start regardless.
		//
		// Store.Get stages, verifies the digest, then removes and renames, so
		// an unreachable store leaves nothing half-written and this returns an
		// error instead.
		if fileExists(dir) {
			return dir, nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
		defer cancel()

		// Get would otherwise create the missing parents 0700, and a release
		// the app account cannot traverse to is one it cannot run.
		if err := EnsureAppDirs(r.AppsDir, appID); err != nil {
			return "", err
		}

		log.Printf("release: fetching %s from the artifact store", ref.Value)
		if err := r.Store.Get(ctx, ref.Value, dir); err != nil {
			return "", err
		}
		// The `current` link is what an operator follows to find live code;
		// a restored release must be reachable the same way a built one is.
		if err := linkCurrent(r.AppsDir, appID, deployID); err != nil {
			log.Printf("release: updating current symlink for %s: %v", appID, err)
		}
		return dir, nil

	default:
		return runtime.LocalResolver{}.Resolve(ref)
	}
}

// ResolvePrivate fetches an artifact release into dest, a directory the caller
// owns, and never touches the app's release tree — see
// runtime.PrivateReleaseResolver. A local reference is the directory itself.
func (r ArtifactResolver) ResolvePrivate(ref runtime.ReleaseRef, dest string) (string, error) {
	if ref.Kind != runtime.ReleaseArtifact {
		return r.Resolve(ref)
	}
	if r.Store == nil {
		return "", fmt.Errorf("%w: this install has no artifact store configured",
			runtime.ErrUnresolvableRelease)
	}
	if _, _, err := parseReleaseKey(ref.Value); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	if err := r.Store.Get(ctx, ref.Value, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// ReleaseDeployID names the deployment a persisted release reference belongs
// to: the key's deployment for an artifact, the directory's name for a local
// tree. "" when the reference names neither.
func ReleaseDeployID(ref string) string {
	r := runtime.ParseReleaseRef(ref)
	switch r.Kind {
	case runtime.ReleaseArtifact:
		_, deployID, err := parseReleaseKey(r.Value)
		if err != nil {
			return ""
		}
		return deployID
	case runtime.ReleaseLocal:
		if filepath.Base(filepath.Dir(r.Value)) != "releases" {
			return ""
		}
		return filepath.Base(r.Value)
	}
	return ""
}

// PinnedByJobRuns is the set of an app's deployments that an unfinished job
// run is pinned to — see database.JobRunPinnedReleases.
func PinnedByJobRuns(db *database.DB, appID string) (map[string]bool, error) {
	if db == nil {
		return nil, nil
	}
	refs, err := db.JobRunPinnedReleases(appID)
	if err != nil {
		return nil, err
	}
	pinned := map[string]bool{}
	for _, ref := range refs {
		if id := ReleaseDeployID(ref); id != "" {
			pinned[id] = true
		}
	}
	return pinned, nil
}

// parseReleaseKey reverses artifact.ReleaseKey so a reference alone is enough
// to know where its tree belongs. Keeping the two in one package is what stops
// them drifting.
// ServingTreeDir is the directory on this machine a shared-mode workload of
// ref runs from, whether or not it exists yet: a local reference's path, or
// the tree an artifact reference is unpacked into (ArtifactResolver.Resolve).
// "" for a reference that names neither.
func ServingTreeDir(appsDir string, ref runtime.ReleaseRef) string {
	switch ref.Kind {
	case runtime.ReleaseLocal:
		return ref.Value
	case runtime.ReleaseArtifact:
		if appID, deployID, err := parseReleaseKey(ref.Value); err == nil {
			return ReleaseDir(appsDir, appID, deployID)
		}
	}
	return ""
}

func parseReleaseKey(key string) (appID, deployID string, err error) {
	const prefix = "releases/"
	if !strings.HasPrefix(key, prefix) {
		return "", "", fmt.Errorf("%w: %q is not a release key", runtime.ErrUnresolvableRelease, key)
	}
	rest := strings.TrimPrefix(key, prefix)
	slash := strings.Index(rest, "/")
	if slash < 1 {
		return "", "", fmt.Errorf("%w: %q has no app segment", runtime.ErrUnresolvableRelease, key)
	}
	appID = rest[:slash]
	deployID = strings.TrimSuffix(rest[slash+1:], ".tar.gz")
	if deployID == "" || strings.ContainsAny(appID+deployID, `/\`) {
		return "", "", fmt.Errorf("%w: %q is malformed", runtime.ErrUnresolvableRelease, key)
	}
	return appID, deployID, nil
}

// publishRelease packs the built tree into the artifact store and returns the
// reference to persist. With no store configured it returns a local reference,
// which is exactly today's behaviour: absent configuration means the
// capability is not offered, not that a degraded version of it runs.
// The descriptor comes back rather than just the key so the caller can record
// what the release cost in the store without asking for it a second time.
func (b *Builder) publishRelease(appID, deployID, releaseDir string, appendLog func(string)) (runtime.ReleaseRef, artifact.Descriptor, error) {
	if b.artifacts == nil {
		return runtime.LocalRelease(releaseDir), artifact.Descriptor{}, nil
	}

	key := artifact.ReleaseKey(appID, deployID)
	appendLog("==> Publishing release to the artifact store...")

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	// Said as it goes: a release of tens of thousands of files read off a
	// network file system takes a while, and a log that stops at the line
	// above for twenty minutes reads as a hang whether it is one or not.
	var mu sync.Mutex
	last := ""
	nextSay := time.Now().Add(publishSayEvery)
	ctx = artifact.WithProgress(ctx, func(entries int, bytes int64, path string) {
		mu.Lock()
		last = path
		say := time.Now().After(nextSay)
		if say {
			nextSay = time.Now().Add(publishSayEvery)
		}
		mu.Unlock()
		if say {
			appendLog(fmt.Sprintf("    packed %d entries (%.1f MiB) so far, at %s",
				entries, float64(bytes)/(1<<20), path))
		}
	})

	// The store's Put honours ctx between files, but a read that blocks inside
	// one file -- a network file system that stops answering -- cannot be
	// interrupted from here. So the deadline is enforced by waiting on it, not
	// by trusting the callee: the deploy fails with where it stopped, and the
	// stuck goroutine is left to finish or die with the process. A deploy left
	// in "building" for good, with nothing in its log, is what this replaces
	// (2026-10-22, trytoft.dev).
	type result struct {
		desc artifact.Descriptor
		err  error
	}
	done := make(chan result, 1)
	go func() {
		d, err := b.artifacts.Put(ctx, key, releaseDir)
		done <- result{d, err}
	}()
	var desc artifact.Descriptor
	var err error
	select {
	case r := <-done:
		desc, err = r.desc, r.err
	case <-ctx.Done():
		mu.Lock()
		at := last
		mu.Unlock()
		err = fmt.Errorf("publishing did not finish within %s (last at %q): %w",
			publishTimeout, at, ctx.Err())
	}
	if err != nil {
		// A deploy that cannot be published is not durable, and on a topology
		// where the release directory is ephemeral that means the app dies at
		// the next task replacement. Better to fail now, with the old release
		// still serving, than to report success for something that will
		// silently vanish.
		return runtime.ReleaseRef{}, artifact.Descriptor{}, err
	}

	appendLog(fmt.Sprintf("    Stored %s (%.1f MiB)", key, float64(desc.Size)/(1<<20)))
	return runtime.ArtifactRelease(key), desc, nil
}

// pendingPublish is a release packed before its start and uploading while it
// starts (ADR §123). wait is called before traffic moves to it.
type pendingPublish struct {
	key   string
	began time.Time
	done  chan publishResult
	ctx   context.Context
	// cancel ends the upload early, and finished is closed once its goroutine
	// has returned — so a failed deploy can remove the object knowing nothing
	// will land after the delete (PB-7).
	cancel   context.CancelFunc
	finished chan struct{}
}

type publishResult struct {
	desc artifact.Descriptor
	err  error
}

// publishBehind is publishRelease with the upload taken off the deploy's
// critical path: the tree is packed here, before anything starts — an app's
// own process may write into its release once it runs, and the artifact is the
// build's output, not the app's state — and the packed file is uploaded in the
// background while the release starts and answers.
//
// nil, nil means the store cannot be split (no store, or one that writes
// straight to its destination) and the caller publishes as before.
func (b *Builder) publishBehind(appID, deployID, releaseDir string, appendLog func(string)) (*pendingPublish, error) {
	if b.artifacts == nil {
		return nil, nil
	}
	up, ok := artifact.UploaderOf(b.artifacts)
	if !ok {
		return nil, nil
	}
	key := artifact.ReleaseKey(appID, deployID)
	appendLog("==> Packaging the release...")
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)

	// Packed under the same two guards publishRelease has: said as it goes,
	// and bounded by waiting on the deadline rather than trusting Stage, which
	// honours ctx only between files. A read blocked inside one file on a
	// network file system otherwise left the deploy in "building" for good,
	// holding its start token and the cache read lock (AS-6).
	var mu sync.Mutex
	last := ""
	nextSay := time.Now().Add(publishSayEvery)
	ctx = artifact.WithProgress(ctx, func(entries int, bytes int64, path string) {
		mu.Lock()
		last = path
		say := time.Now().After(nextSay)
		if say {
			nextSay = time.Now().Add(publishSayEvery)
		}
		mu.Unlock()
		if say {
			appendLog(fmt.Sprintf("    packed %d entries (%.1f MiB) so far, at %s",
				entries, float64(bytes)/(1<<20), path))
		}
	})
	type stageResult struct {
		staged *artifact.Staged
		err    error
	}
	// Buffered, so a Stage that finishes after the deadline does not block;
	// the drain below then removes what it packed.
	stagedCh := make(chan stageResult, 1)
	go func() {
		st, err := up.Stage(ctx, releaseDir)
		stagedCh <- stageResult{st, err}
	}()
	var staged *artifact.Staged
	var err error
	select {
	case r := <-stagedCh:
		staged, err = r.staged, r.err
	case <-ctx.Done():
		mu.Lock()
		at := last
		mu.Unlock()
		err = fmt.Errorf("packing did not finish within %s (last at %q): %w", publishTimeout, at, ctx.Err())
		go func() {
			if r := <-stagedCh; r.staged != nil {
				r.staged.Remove()
			}
		}()
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("packing the release: %w", err)
	}
	appendLog(fmt.Sprintf("    Packed %.1f MiB; uploading to the artifact store while the release starts",
		float64(staged.Size)/(1<<20)))
	p := &pendingPublish{key: key, began: time.Now(), done: make(chan publishResult, 1), ctx: ctx,
		cancel: cancel, finished: make(chan struct{})}
	go func() {
		defer close(p.finished)
		defer cancel()
		defer staged.Remove()
		d, err := up.Upload(ctx, key, staged)
		p.done <- publishResult{d, err}
	}()
	return p, nil
}

// wait is the upload's result, bounded by the same deadline publishRelease
// keeps and for the same reason: an upload that stops answering must fail the
// deploy rather than hold it in "building" for good.
//
// **A result that has arrived wins over a done context.** The upload goroutine
// cancels the context as it returns, right after sending, so by the time wait
// runs both cases are usually ready — and select picks among ready cases at
// random, which failed about half of all successful uploads as a timeout.
// discardPublished removes the artifact a deploy that did not flip published
// of its own release: pending's upload, cancelled and waited for first so the
// object cannot land after the delete, or key, which a synchronous publish
// already stored. The row records an artifact_key only on success, and with
// auto-delete off the release sweep expires nothing, so without this every
// deploy that failed at its start, readiness or flip left its object in the
// bucket until the app was deleted (PB-7).
//
// Never called with a rollback's key: that names the source deployment's
// artifact, a live version.
func (b *Builder) discardPublished(p *pendingPublish, key string) {
	if b.artifacts == nil {
		return
	}
	remove := func(key string) {
		ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()
		if err := b.artifacts.Delete(ctx, key); err != nil && !errors.Is(err, artifact.ErrNotFound) {
			log.Printf("Warning: removing %s, published by a deploy that failed: %v", key, err)
		}
	}
	if p != nil {
		p.cancel()
		select {
		case <-p.finished:
		case <-time.After(deleteTimeout):
			// An upload that does not honour its context: delete once it has
			// returned, without holding the failing deploy for it.
			go func() {
				<-p.finished
				remove(p.key)
			}()
			return
		}
		key = p.key
	}
	if key != "" {
		remove(key)
	}
}

func (p *pendingPublish) wait(appendLog func(string)) (artifact.Descriptor, error) {
	var r publishResult
	select {
	case r = <-p.done:
	case <-p.ctx.Done():
		select {
		case r = <-p.done:
		default:
			return artifact.Descriptor{}, fmt.Errorf("uploading did not finish within %s: %w",
				publishTimeout, p.ctx.Err())
		}
	}
	if r.err != nil {
		return artifact.Descriptor{}, r.err
	}
	appendLog(fmt.Sprintf("    Stored %s (%.1f MiB), %s after packing",
		p.key, float64(r.desc.Size)/(1<<20), time.Since(p.began).Round(100*time.Millisecond)))
	return r.desc, nil
}

// restoreRelease fetches one release's tree back onto this machine. It is what
// makes a rollback target reachable after its directory has been pruned or
// destroyed with the host.
func (b *Builder) restoreRelease(appID, deployID, dest string, appendLog func(string)) error {
	if b.artifacts == nil {
		return errNoRelease
	}

	key := artifact.ReleaseKey(appID, deployID)
	appendLog(fmt.Sprintf("==> Fetching release %s from the artifact store...", deployID))

	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()

	if err := b.artifacts.Get(ctx, key, dest); err != nil {
		return err
	}
	appendLog("    Restored.")
	return nil
}

// restoreAdmitted is restoreRelease for a fetch the disk may refuse — a
// rollback, a reader. A start is never refused here: an app the platform is
// bringing back is attempted, and the disk answers for itself.
func (b *Builder) restoreAdmitted(appID, deployID, dest string, appendLog func(string)) error {
	var need int64
	if b.db != nil {
		if d, err := b.db.GetDeployment(deployID); err == nil && d != nil && d.DiskBytes != nil {
			need = *d.DiskBytes
		}
	}
	if err := b.admitTree(need); err != nil {
		return err
	}
	return b.restoreRelease(appID, deployID, dest, appendLog)
}

// sweepReleases retires the releases this deploy pushed past the keep count —
// the directories, the artifacts and the rows, in that order.
//
// A sweep like every other, and named like one (`releases`), even though it is
// the only one triggered by a deploy rather than a timer: the enum is named for
// the subsystem whose data is touched, never for the thing that started it.
//
// Retention is read here rather than when the job was queued: a build can sit
// in the queue, and the answer that matters is the one in force when the
// release actually lands. An unreadable settings row means keeping everything —
// failing to prune costs disk, and pruning on a guess costs somebody their
// rollback target.
func (b *Builder) sweepReleases(app *database.App, releaseDeployID string) {
	ctx, span := telemetry.Start(context.Background(), "releases.sweep",
		attribute.String("toft.app.id", app.ID))
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepReleases, outcome, time.Since(start))
	}()

	ret, err := b.retentionFor(app)
	if err != nil {
		log.Printf("Warning: reading retention for %s, keeping every release: %v", app.Slug, err)
		return
	}
	if !ret.AutoDelete {
		// Off is not a failure. The owner turned automatic cleanup off and the
		// sweep did exactly what it was supposed to.
		outcome = telemetry.OutcomeSkipped
		return
	}
	outcome = telemetry.OutcomeSucceeded

	deployments, err := b.db.ListDeployments(app.ID)
	if err != nil {
		// Without the statuses the sweep cannot tell a version from a failed
		// build, and guessing is how rollback targets used to go.
		log.Printf("Warning: listing deployments of %s, keeping every release: %v", app.Slug, err)
		outcome = telemetry.OutcomeFailed
		return
	}
	status := make(map[string]string, len(deployments))
	for _, d := range deployments {
		status[d.ID] = d.Status
	}
	// A queued retry waits with its first attempt's release; reclaiming it
	// would fail every remaining attempt before any code ran.
	pinned, err := PinnedByJobRuns(b.db, app.ID)
	if err != nil {
		log.Printf("Warning: reading %s's job runs, keeping every release: %v", app.Slug, err)
		outcome = telemetry.OutcomeFailed
		return
	}
	// And the release a serving rebuild restored — a move, or a rollback
	// rebuilt for a new address — is kept beside it, outside the count. The
	// rebuild owns a fresh tree, so the original looked spare, but its row is
	// where the serving release stands in the history: pruned, the next
	// rollback could not tell what is older than the serving release and went
	// forward to the release just left (TOFT-BB-001, 2026-11-04 second list).
	kept := make(map[string]bool, len(pinned)+2)
	for id := range pinned {
		kept[id] = true
	}
	if restored := restoredBy(deployments, releaseDeployID); restored != "" {
		kept[restored] = true
	}
	// And the release a queued or running rollback is returning to, as the
	// delete route protects it (withRollbackSources): a deploy that flipped
	// while a rollback waited swept its target — tree trashed, artifact
	// expired, row deleted — and the rollback then failed on a release that
	// no longer existed (code review 2026-11-11, F-4).
	for id := range inFlightRollbackSources(deployments) {
		kept[id] = true
	}
	var pruned, failed []string
	var objects int
	if b.artifacts == nil {
		// The disk is the only copy, so it is also the list.
		pruned, failed = pruneReleases(b.opts.AppsDir, app.ID, releaseDeployID, ret.Keep, status, kept)
	} else {
		// With a store the disk is a cache of it, and a cache that decides what
		// is kept is how a task replacement became a deletion: an emptied
		// APPS_DIR left one or two directories, and the store was pruned to
		// match. What is kept is counted against the rows; each place that holds
		// a release is then pruned to that set.
		keepSet, retired := retainedReleases(deployments, releaseDeployID, ret.Keep, kept)
		// Failed builds' directories only: no successful release is past an
		// unbounded keep.
		_, failed = pruneReleases(b.opts.AppsDir, app.ID, releaseDeployID, math.MaxInt, status, kept)
		for _, id := range retired {
			if err := toTrash(b.opts.AppsDir, ReleaseDir(b.opts.AppsDir, app.ID, id)); err != nil {
				// Still on disk is still a version; its row stays with it.
				log.Printf("Warning: removing retired release %s of %s: %v", id, app.Slug, err)
				continue
			}
			pruned = append(pruned, id)
		}
		objects = b.expireArtifacts(app.ID, app.Slug, keepSet)
		// And the disk down to the live release and its rollback target — none
		// at all for an isolated app — every other tree being in the store.
		b.evictForApp(app, deployments, releaseDeployID, pinned)
	}
	if len(failed) > 0 {
		log.Printf("release: removed %d failed build(s) of %s: %s",
			len(failed), app.Slug, strings.Join(failed, ", "))
	}
	if len(pruned) > 0 {
		// Named, not counted. This is the deletion somebody walks into —
		// "where did version 12 go" — and a number answers a different
		// question from the one they are asking.
		log.Printf("release: reclaimed %d release(s) of %s (keeping %d): %s",
			len(pruned), app.Slug, ret.Keep, strings.Join(pruned, ", "))
	}
	// And the rows, so the history lists versions that exist. A build whose
	// release has been reclaimed cannot be made current, downloaded, or rolled
	// back to; keeping its row only offers all three and then refuses. The
	// build log goes with it, which is the price of a list that means what it
	// says — a *failed* build keeps its row and its log, because that row is
	// the explanation and it never owned a release.
	var rows int64
	for _, deployID := range pruned {
		gone, err := b.db.DeleteDeployment(app.ID, deployID)
		if err != nil {
			log.Printf("Warning: removing pruned deployment %s of %s: %v", deployID, app.Slug, err)
			continue
		}
		if gone {
			rows++
		}
	}

	b.recordPruned(app, pruned, ret.Keep)

	// Three subjects on one pass, which is the case `subject` exists for: a
	// release is a rollback target, an object is storage, and a row is the
	// build's explanation. The app id is on the span and never on the series.
	telemetry.RecordSweepRemoved(ctx, span, telemetry.SweepReleases, telemetry.Removed{
		Releases: int64(len(pruned) + len(failed)),
		Objects:  int64(objects),
		Rows:     rows,
	})
}

// recordPruned writes the versions a sweep retired as a releases_pruned app
// event, so `toft events` and the Events tab answer "where did that version
// go" — the row it would have been read from is deleted with its release.
// Silent: retention working as configured is nobody's mail.
func (b *Builder) recordPruned(app *database.App, pruned []string, keep int) {
	if len(pruned) == 0 || b.opts.Events == nil {
		return
	}
	noun := "versions"
	if len(pruned) == 1 {
		noun = "version"
	}
	b.opts.Events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindReleasesPruned,
		Severity: database.EventSeverityInfo,
		Source:   database.EventSourceBuilder,
		Summary: fmt.Sprintf("%d old %s removed: this app keeps its %d newest successful deploys",
			len(pruned), noun, keep),
		Detail: "removed: " + strings.Join(pruned, ", ") + "\n" +
			"Keep more with \"Deployments to keep\" in the app's settings (releases_kept); " +
			"an administrator's max_versions_per_app caps it.",
		Silent: true,
	})
}

// expireArtifacts prunes the store to exactly the releases the rows retain.
// It takes the keep set rather than a count so the store and the history cannot
// drift into disagreeing about which releases still exist.
//
// It returns how many objects went, because the caller says so out loud: the
// count used to be discarded here, which made this the one deletion in the
// system with no trace of any kind.
func (b *Builder) expireArtifacts(appID, slug string, keepSet map[string]bool) int {
	if b.artifacts == nil {
		return 0
	}

	keep := make([]string, 0, len(keepSet))
	for id := range keepSet {
		keep = append(keep, artifact.ReleaseKey(appID, id))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	n, err := b.artifacts.Expire(ctx, appID, keep)
	if err != nil {
		// Stale artifacts cost storage, not correctness.
		log.Printf("release: expiring artifacts for %s: %v", slug, err)
		return n
	}
	if n > 0 {
		log.Printf("release: expired %d artifact(s) for %s", n, slug)
	}
	return n
}

// retainedReleases is version retention as a question about the rows: which of
// an app's releases are kept, and which successful ones this sweep retires.
//
// Kept: the release being served, every release a job run is pinned to, every
// deployment that has not finished (a build in flight may already have
// published), and the newest keep−1 other successful deployments that own a
// release — newest by when they started, never by a directory's mtime, which
// on a restored tree is the restore's. A rollback row owns no release (it
// serves its source's), so it is neither counted nor retired.
//
// Retired is newest first, and every id in it is a successful deployment whose
// directory, artifact and row this sweep removes.
func retainedReleases(deployments []database.Deployment, liveID string, keep int, pinned map[string]bool) (map[string]bool, []string) {
	kept := map[string]bool{}
	if liveID != "" {
		kept[liveID] = true
		keep--
	}
	for id := range pinned {
		kept[id] = true
	}
	for id := range inFlightRollbackSources(deployments) {
		kept[id] = true
	}

	owners := make([]database.Deployment, 0, len(deployments))
	for _, d := range deployments {
		switch {
		case d.SourceDeployID != "":
			continue
		case d.Status == database.DeployStatusSuccess:
			owners = append(owners, d)
		case d.Status != database.DeployStatusFailed:
			kept[d.ID] = true
		}
	}
	sort.SliceStable(owners, func(i, j int) bool { return startedAfter(owners[i].StartedAt, owners[j].StartedAt) })

	var retired []string
	for _, d := range owners {
		if kept[d.ID] {
			continue
		}
		if keep > 0 {
			kept[d.ID] = true
			keep--
			continue
		}
		retired = append(retired, d.ID)
	}
	return kept, retired
}

// inFlightRollbackSources are the releases the pending and building rollbacks
// among deployments are returning to. Each is kept by every sweep until its
// rollback has flipped or failed — the sweep's half of the rule the delete
// route keeps with withRollbackSources (F-4).
func inFlightRollbackSources(deployments []database.Deployment) map[string]bool {
	out := map[string]bool{}
	for _, d := range deployments {
		if d.SourceDeployID == "" {
			continue
		}
		if d.Status == database.DeployStatusPending || d.Status == database.DeployStatusBuilding {
			out[d.SourceDeployID] = true
		}
	}
	return out
}

// startedAfter orders newest first, with a row that never recorded a start
// last: it cannot be shown to be newer than anything.
func startedAfter(a, b *time.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return a.After(*b)
}

// AppsDir exposes the release root for callers that must derive a path from an
// app ID — the reconciler's missing-release check, and nothing else.
func (b *Builder) AppsDir() string { return b.opts.AppsDir }

// releaseRefFor reads the app's persisted release reference, falling back to
// the current-link path for rows written before references were stored.
func (b *Builder) releaseRefFor(appID, stored string) runtime.ReleaseRef {
	if stored != "" {
		return runtime.ParseReleaseRef(stored)
	}
	return runtime.LocalRelease(CurrentLink(b.opts.AppsDir, appID))
}

// LiveReleaseID names the deployment whose release the app is actually
// serving, which is not always the app's newest deployment row.
//
// A promote makes the source deployment current again, so the current row and
// the release it owns normally agree — but a promote in flight, and rollback
// rows written before promotes were folded back onto their source row, serve
// the *source* deployment's directory and artifact while a different row is
// what the app calls current. Anything that deletes or prunes storage has to
// agree with the filesystem rather than with history, or promoting an old
// build and then tidying up the list would delete the code that is running.
//
// Returns "" when the app has never had a release, which callers must treat as
// "nothing is protected" rather than as a match.
func LiveReleaseID(appsDir string, app *database.App) string {
	if app.ReleaseRef != "" {
		ref := runtime.ParseReleaseRef(app.ReleaseRef)
		switch ref.Kind {
		case runtime.ReleaseArtifact:
			if _, deployID, err := parseReleaseKey(ref.Value); err == nil {
				return deployID
			}
		case runtime.ReleaseLocal:
			// The local reference is the release directory itself, whose last
			// element is the deployment that owns it. The `current` symlink is
			// deliberately not followed here: it is one hop away from the
			// truth and is only repaired on a best-effort basis.
			if base := filepath.Base(ref.Value); base != "." && base != string(filepath.Separator) && base != "current" {
				return base
			}
		}
	}
	// Rows written before release references existed, and any release whose
	// reference failed to parse: the symlink is the only remaining record of
	// what is being served.
	if target, err := os.Readlink(CurrentLink(appsDir, app.ID)); err == nil {
		return filepath.Base(target)
	}
	return ""
}

// missingReleaseErr reports whether an error means the release is genuinely
// gone, as opposed to temporarily unreachable. Only the former justifies
// telling an operator to redeploy; an outage must not be reported as data
// loss, or a store coming back would find every app marked beyond recovery.
func missingReleaseErr(err error) bool {
	return errors.Is(err, artifact.ErrNotFound) || errors.Is(err, errNoRelease)
}

// MissingRelease is missingReleaseErr for callers outside this package.
//
// There is one, and it is the API's restart handler: a start that found no code
// on this machine is a refusal the caller can act on — deploy the app — and not
// a server error. It answered 500 with the sentence below in it, which is the
// shape of a bug report from somebody whose app had simply never been deployed.
func MissingRelease(err error) bool { return missingReleaseErr(err) }

// restoredBy is the release the serving row rebuilt — a move's, or a rollback
// rebuilt for a new address — or "" when it is no rebuild.
func restoredBy(deployments []database.Deployment, servingID string) string {
	for i := range deployments {
		if d := &deployments[i]; d.ID == servingID {
			if orig := OriginalRelease(d); orig != d.ID {
				return orig
			}
			return ""
		}
	}
	return ""
}

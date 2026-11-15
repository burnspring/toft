package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"toft/internal/artifact"
)

// deleteTimeout bounds one artifact removal.
const deleteTimeout = 30 * time.Second

// DeleteRelease removes the release a deployment owns: the unpacked tree on
// this host and the packed artifact in the store. Both, because either one
// alone leaves the release costing money somewhere.
//
// The release a deployment owns is always the one named after that deployment.
// A promote row owns nothing — while one exists (in flight, failed, or written
// before promotes were folded back onto their source row) it serves the
// directory and artifact its source deployment published — so deleting one
// frees no space and, more importantly, cannot reach through to the source's
// storage and delete a release something else is still serving. Keying on the
// row's own ID is what makes that true by construction rather than by a check
// that could be forgotten.
//
// artifactKey comes from the deployment row rather than being derived: a row
// that published nothing has no key, and inventing one would mean issuing
// deletes for objects this deployment never owned.
//
// Storage is removed before the caller drops the row, so a failure leaves a
// deployment that still lists what it still costs, and the operation can simply
// be retried. An absent directory or object is success — deletion has to be
// idempotent to be retryable.
// It is a function rather than a Builder method because it needs no build
// machinery — a release is a directory and an object, and the API is the only
// caller. Keeping it in this package is what keeps the path construction here,
// where every other release path is built.
func DeleteRelease(appsDir string, store artifact.Store, appID, deployID, artifactKey string) error {
	if err := toTrash(appsDir, ReleaseDir(appsDir, appID, deployID)); err != nil {
		return fmt.Errorf("removing the release directory: %w", err)
	}

	if store == nil || artifactKey == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()

	if err := store.Delete(ctx, artifactKey); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		// Deliberately fatal to the request. The directory is already gone, so
		// reporting success here would tell an operator the storage was
		// reclaimed while the artifact quietly keeps billing — and the row
		// recording which artifact it was is about to be deleted, which is what
		// would make it unreclaimable. Better to fail and keep the row.
		return fmt.Errorf("removing the artifact: %w", err)
	}
	return nil
}

// DeleteAppStorage removes everything an app stored: its whole directory on
// this host (every release, the current symlink, any staging left behind) and
// every artifact it published. Deleting the row alone leaves both behind, and
// nothing afterwards names them — the app id was the only handle.
//
// Same failure rule as DeleteRelease, for the same reason: an absent directory
// or object is success, but a store that refuses is reported rather than
// swallowed. The caller keeps the row when this fails, because the row is what
// still says which bytes exist.
//
// It takes the app id, not the slug: storage is keyed by id everywhere, so a
// renamed app deletes exactly what it wrote.
//
// jobRunDir is JOB_RUN_DIR, where a host job run's directory lives when it is
// not under APPS_DIR. Each finished run removes its own, but the app's level
// above them is the app's: left behind, every deleted app that ever ran a job
// left one empty directory there for good.
//
// logsDir is LOGS_DIR, where the shared backend wrote the app's output under
// LOGS_DIR/<app-id>. Removed after the backend's own Delete, which drains and
// removes a live release's files first; this takes whatever is left.
//
// **Those two never keep the app.** Unlike APPS_DIR and the artifacts, they
// are found again without the row: the orphan sweep removes any directory
// there whose name no app has. And they are the two a delete cannot always
// remove at once — a deploy still starting writes into the log directory as
// it is removed ("directory not empty"), and on EFS a file something still
// holds open is a `.nfs…` entry nothing can unlink until it is closed. Refusing
// the delete for either left an app nobody could remove, and its owner with
// it. So a failure there is logged and left to the sweep.
func DeleteAppStorage(appsDir, jobRunDir, logsDir string, store artifact.Store, appID string) error {
	if err := toTrash(appsDir, AppDir(appsDir, appID)); err != nil {
		return fmt.Errorf("removing the app directory: %w", err)
	}
	for _, d := range []struct{ root, what string }{{jobRunDir, "job-run"}, {logsDir, "log"}} {
		if d.root == "" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.root, appID)); err != nil {
			log.Printf("Deleting app %s: its %s directory is left for the orphan sweep: %v", appID, d.what, err)
		}
	}

	if store == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()

	// An empty keep set means "keep none": every artifact under this app.
	if _, err := store.Expire(ctx, appID, nil); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		return fmt.Errorf("removing the app's artifacts: %w", err)
	}
	// Its runs' outputs too: the job_runs rows that held their keys cascade
	// away with the app, so after this nothing could ever find them again.
	if _, err := artifact.DeleteAppRuns(ctx, store, appID); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		return fmt.Errorf("removing the app's run outputs: %w", err)
	}
	return nil
}

// ReleaseAvailable reports whether the release a deployment owns can still be
// served — present on this host, or fetchable from the artifact store.
//
// This is the question "can this build be made current again", and it is not
// the same as "is its directory here". Pruning deletes directories and leaves
// artifacts, and restoring one is exactly what the artifact store is for; a
// caller that looks only at the filesystem refuses builds the builder would
// have restored without complaint.
func ReleaseAvailable(appsDir string, store artifact.Store, appID, deployID, artifactKey string) bool {
	if fileExists(ReleaseDir(appsDir, appID, deployID)) {
		return true
	}
	if store == nil || artifactKey == "" {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
	defer cancel()

	_, err := store.Stat(ctx, artifactKey)
	// An unreachable store answers "not gone" — a build is not unrecoverable
	// because the store is having a bad minute, and the deploy that follows
	// reports the outage for what it is.
	return err == nil || !errors.Is(err, artifact.ErrNotFound)
}

// stagingName is APPS_DIR's scratch for a release fetched for a reader rather
// than a start — an archive download — so it sits on the same disk as the
// trees and never in a TMPDIR that is a small tmpfs on some hosts.
const stagingName = ".staging"

// WithRelease hands fn a directory holding one release's tree: the one on disk
// when it is there, and otherwise a verified copy fetched from the artifact
// store into a scratch directory that is removed when fn returns.
//
// The disk is a cache once there is a store, so a reader that only looked
// there answered "pruned" for every release a task replacement or an eviction
// had taken off it, while the release sat in the store. ErrReleaseGone means
// neither place has it.
func (b *Builder) WithRelease(appID, deployID string, fn func(dir string) error) error {
	if dir := ReleaseDir(b.opts.AppsDir, appID, deployID); fileExists(dir) {
		return fn(dir)
	}
	if b.artifacts == nil {
		return ErrReleaseGone
	}
	parent := filepath.Join(b.opts.AppsDir, stagingName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	holder, err := os.MkdirTemp(parent, deployID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(holder)
	dir := filepath.Join(holder, "tree")
	if err := b.restoreAdmitted(appID, deployID, dir, func(string) {}); err != nil {
		if missingReleaseErr(err) {
			return ErrReleaseGone
		}
		return err
	}
	return fn(dir)
}

// ErrReleaseGone is a release that is neither on disk nor in the store.
var ErrReleaseGone = errors.New("that release no longer exists")

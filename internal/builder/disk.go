package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"toft/internal/artifact"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The release disk: what is on it, how full it is, and what the platform does
// about that before the disk decides for it.
//
// With an artifact store every release tree on disk is a copy of something the
// store holds, so most of what fills the disk can be reclaimed losing nothing
// but a fetch later. Two watermarks, read off APPS_DIR's filesystem:
//
//   - **soft** (under DiskSoftFree percent free): reclaim, cheapest loss first — the
//     trash, scratch left by a reader, the build caches when they share the
//     disk, then the trees of releases that are neither live nor the one-click
//     rollback target, then the rollback targets of stopped and sleeping apps.
//   - **hard** (under DiskHardFree percent, or hardFloorBytes, free): refuse to
//     write another release tree — a build (`disk_full`) or a fetch for a wake,
//     a rollback or a reader. Running apps are never touched.

// The watermarks, in percent free. Constants rather than a dial: what an
// operator does about a full disk is grow it, and the alarm that tells them to
// is theirs (a CloudWatch alarm, a PVC monitor), set against these.
const (
	DiskSoftFree = 15
	DiskHardFree = 5
)

// hardFloorBytes is the free space under which a write is refused whatever the
// percentage says: 5% of a 20 GB claim is a single R release.
const hardFloorBytes = 1 << 30

// DiskLevel is how full the release disk is.
type DiskLevel int

const (
	DiskOK DiskLevel = iota
	DiskSoft
	DiskHard
)

func (l DiskLevel) String() string {
	switch l {
	case DiskSoft:
		return "soft"
	case DiskHard:
		return "hard"
	}
	return "ok"
}

// DiskUsage is one filesystem's free space as the watermarks read it.
type DiskUsage struct {
	Path string
	// Free is what an unprivileged writer can still use (statfs f_bavail);
	// Used is what is taken (f_blocks - f_bfree). The two do not add up to
	// Total: the difference is the blocks a filesystem reserves for root,
	// which is why "how full" is Used / (Used + Free), as df computes it.
	Free  uint64
	Used  uint64
	Total uint64
	Level DiskLevel
	// Device identifies the filesystem, so two directories on one disk are
	// reported, and reclaimed for, once.
	Device uint64
}

// ErrDiskFull is a write refused at the hard watermark.
var ErrDiskFull = errors.New("the release disk is full")

func (b *Builder) watermarks() (soft, hard int) {
	soft, hard = b.opts.DiskSoftFreePercent, b.opts.DiskHardFreePercent
	if hard <= 0 {
		hard = DiskHardFree
	}
	if soft <= hard {
		soft = max(DiskSoftFree, hard+1)
	}
	return soft, hard
}

// MeasureDisk reads the filesystem holding path against the watermarks.
func (b *Builder) MeasureDisk(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskUsage{Path: path}, err
	}
	u := DiskUsage{
		Path:  path,
		Free:  uint64(st.Bavail) * uint64(st.Bsize),
		Used:  (uint64(st.Blocks) - uint64(st.Bfree)) * uint64(st.Bsize),
		Total: uint64(st.Blocks) * uint64(st.Bsize),
	}
	if info, err := os.Stat(path); err == nil {
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			u.Device = uint64(sys.Dev)
		}
	}
	u.Level = b.levelOf(u.Free, u.Total)
	return u, nil
}

func (b *Builder) levelOf(free, total uint64) DiskLevel {
	if total == 0 {
		return DiskOK
	}
	soft, hard := b.watermarks()
	// In floating point: free*100 overflows a uint64 on a network
	// filesystem, which reports exabytes (EFS answers 8 EiB).
	pct := float64(free) * 100 / float64(total)
	switch {
	case pct < float64(hard) || free < hardFloorBytes:
		return DiskHard
	case pct < float64(soft):
		return DiskSoft
	}
	return DiskOK
}

// admitTree refuses a write of about need bytes into APPS_DIR when it would
// leave the disk past the hard watermark. A disk that cannot be measured is
// admitted: the refusal is a courtesy ahead of ENOSPC, never a new way to fail.
func (b *Builder) admitTree(need int64) error {
	u, err := b.MeasureDisk(b.opts.AppsDir)
	if err != nil {
		return nil
	}
	after := u.Free
	if need > 0 {
		if uint64(need) >= after {
			after = 0
		} else {
			after -= uint64(need)
		}
	}
	if u.Level == DiskHard || b.levelOf(after, u.Total) == DiskHard {
		_, hard := b.watermarks()
		msg := fmt.Sprintf("%s free of %s on %s", humanBytes(u.Free), humanBytes(u.Total), u.Path)
		if need > 0 {
			msg += fmt.Sprintf("; this needs about %s", humanBytes(uint64(need)))
		}
		return fmt.Errorf("%w: %s, and no release tree is written below %d%% free — an administrator frees or grows the disk",
			ErrDiskFull, msg, hard)
	}
	return nil
}

// buildEstimate is what a build of this app is expected to write: its last
// successful release's tree plus the tarball staged while publishing it. Zero
// for an app with no measured release, which admits on the watermark alone.
func buildEstimate(deployments []database.Deployment) int64 {
	for _, d := range deployments {
		if d.Status != database.DeployStatusSuccess || d.SourceDeployID != "" {
			continue
		}
		var n int64
		if d.DiskBytes != nil {
			n += *d.DiskBytes
		}
		if d.ArtifactBytes != nil {
			n += *d.ArtifactBytes
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

func humanBytes(n uint64) string { return FormatBytes(int64(n)) }

// rollbackTarget is REL.2's: the newest successful release that owns a tree
// and is not the one serving.
func rollbackTarget(deployments []database.Deployment, liveID string) string {
	owners := make([]database.Deployment, 0, len(deployments))
	for _, d := range deployments {
		if d.Status == database.DeployStatusSuccess && d.SourceDeployID == "" && d.ID != liveID {
			owners = append(owners, d)
		}
	}
	sort.SliceStable(owners, func(i, j int) bool { return startedAfter(owners[i].StartedAt, owners[j].StartedAt) })
	if len(owners) == 0 {
		return ""
	}
	return owners[0].ID
}

// diskKeep is which of an app's release trees stay on disk once a store holds
// them all: a shared app keeps the live release and its rollback target; an
// isolated app keeps none, because its pod fetches from the store and never
// reads this disk. Pinned releases stay — a job run starts from them.
func diskKeep(app *database.App, deployments []database.Deployment, liveID string, pinned map[string]bool) map[string]bool {
	keep := map[string]bool{}
	for id := range pinned {
		keep[id] = true
	}
	for _, d := range deployments {
		// A build in flight writes its own tree.
		if d.Status != database.DeployStatusSuccess && d.Status != database.DeployStatusFailed {
			keep[d.ID] = true
		}
	}
	// A rollback in flight is about to copy its source's tree (F-4).
	for id := range inFlightRollbackSources(deployments) {
		keep[id] = true
	}
	// The mode the live release runs under, from its own row: an app's
	// recorded running mode can lag the deploy that just changed it.
	mode := app.RunningModeOrShared()
	for _, d := range deployments {
		if d.ID == liveID && d.ExecutionMode != "" {
			if m, err := runtime.ParseMode(d.ExecutionMode); err == nil {
				mode = m
			}
		}
	}
	if mode == runtime.ModeIsolated {
		return keep
	}
	if liveID != "" {
		keep[liveID] = true
	}
	if prev := rollbackTarget(deployments, liveID); prev != "" {
		keep[prev] = true
	}
	return keep
}

// evictTrees removes an app's release trees outside keep, each only after the
// store confirms it holds the release: a tree built before the store existed
// is the only copy and stays. Returns the ids evicted.
func (b *Builder) evictTrees(app *database.App, keep map[string]bool) []string {
	if b.artifacts == nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(b.opts.AppsDir, app.ID, "releases"))
	if err != nil {
		return nil
	}
	var evicted []string
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		if !b.storeHolds(app.ID, e.Name()) {
			continue
		}
		dir := ReleaseDir(b.opts.AppsDir, app.ID, e.Name())
		if err := toTrash(b.opts.AppsDir, dir); err != nil {
			log.Printf("Warning: evicting release %s of %s: %v", e.Name(), app.Slug, err)
			continue
		}
		evicted = append(evicted, e.Name())
	}
	if len(evicted) > 0 && !keep[LiveReleaseID(b.opts.AppsDir, app)] {
		// The live tree went too (an isolated app): `current` would dangle.
		_ = os.Remove(CurrentLink(b.opts.AppsDir, app.ID))
	}
	return evicted
}

func (b *Builder) storeHolds(appID, deployID string) bool {
	if b.artifacts == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := b.artifacts.Stat(ctx, artifact.ReleaseKey(appID, deployID))
	return err == nil
}

// evictForApp applies the disk keep to one app after its deploy.
func (b *Builder) evictForApp(app *database.App, deployments []database.Deployment, liveID string, pinned map[string]bool) {
	if evicted := b.evictTrees(app, diskKeep(app, deployments, liveID, pinned)); len(evicted) > 0 {
		log.Printf("release: %d tree(s) of %s left the disk, kept in the store: %v", len(evicted), app.Slug, evicted)
	}
}

// diskWatchInterval is how often the soft watermark is read.
const diskWatchInterval = 5 * time.Minute

// startDiskWatcher reclaims at the soft watermark for the life of the process.
func (b *Builder) startDiskWatcher() {
	go func() {
		t := time.NewTicker(diskWatchInterval)
		defer t.Stop()
		for range t.C {
			b.sweepOrphans()
			b.reclaimDisk()
		}
	}()
}

// reclaimDisk frees what can be freed losing nothing the store does not hold,
// cheapest loss first, and stops as soon as the disk is back above the soft
// watermark. The trash empties itself as it is filled, so it is not a step.
func (b *Builder) reclaimDisk() {
	u, err := b.MeasureDisk(b.opts.AppsDir)
	if err != nil || u.Level == DiskOK {
		return
	}
	log.Printf("disk: %s free of %s on %s (%s watermark); reclaiming",
		humanBytes(u.Free), humanBytes(u.Total), u.Path, u.Level)
	healthy := func() bool {
		u, err := b.MeasureDisk(b.opts.AppsDir)
		return err == nil && u.Level == DiskOK
	}

	// A reader's scratch is removed when it returns; what is left is a crash's.
	sweepStaging(filepath.Join(b.opts.AppsDir, stagingName), time.Hour)
	if healthy() {
		return
	}

	// The build caches, when they share the disk: a miss costs one slow build.
	// The reclaim runs on its own; the next tick reads what it freed.
	if b.opts.CacheDir != "" {
		if cu, err := b.MeasureDisk(b.opts.CacheDir); err == nil && cu.Device == u.Device {
			if started, _ := b.StartCacheReclaim(func(r ReclaimResult) {
				log.Printf("disk: build cache reclaimed at the soft watermark: %s", r.Problems())
			}); started {
				return
			}
		}
	}

	// Rollback targets: stopped, sleeping and failed apps first, running ones
	// last. A rollback to one is then a fetch rather than instant.
	if b.db == nil || b.artifacts == nil {
		return
	}
	apps, err := b.db.ListAllApps()
	if err != nil {
		return
	}
	sort.SliceStable(apps, func(i, j int) bool {
		return apps[i].Status != database.AppStatusRunning && apps[j].Status == database.AppStatusRunning
	})
	for i := range apps {
		// The row as it is now, not as it was when the list was read: a deploy
		// that flipped in between made the stale row's rollback target the new
		// live release, and this evicted the serving tree (code review
		// 2026-11-11, F-12). Read under the app's flip lock, so no flip lands
		// between the read and the eviction.
		unlock := b.flipLock(apps[i].ID)
		app, err := b.db.GetApp(apps[i].ID)
		if err != nil || app == nil {
			unlock()
			continue
		}
		deployments, err := b.db.ListDeployments(app.ID)
		if err != nil {
			unlock()
			continue
		}
		pinned, err := PinnedByJobRuns(b.db, app.ID)
		if err != nil {
			unlock()
			continue
		}
		liveID := LiveReleaseID(b.opts.AppsDir, app)
		keep := diskKeep(app, deployments, liveID, pinned)
		if prev := rollbackTarget(deployments, liveID); prev != "" && !pinned[prev] {
			delete(keep, prev)
		}
		evicted := b.evictTrees(app, keep)
		unlock()
		if len(evicted) > 0 {
			log.Printf("disk: %s's rollback target left the disk, kept in the store: %v", app.Slug, evicted)
			if healthy() {
				return
			}
		}
	}
	if u, err := b.MeasureDisk(b.opts.AppsDir); err == nil && u.Level != DiskOK {
		log.Printf("disk: still %s free of %s on %s after reclaiming everything the store holds — free or grow the disk",
			humanBytes(u.Free), humanBytes(u.Total), u.Path)
	}
}

// sweepStaging removes scratch older than age.
func sweepStaging(dir string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < age {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

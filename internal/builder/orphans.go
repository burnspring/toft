package builder

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// The orphan sweep: directories under APPS_DIR and JOB_RUN_DIR named for an app
// that no longer exists.
//
// Deleting an app removes both, and a build that outlives its app takes its
// files with it (dropIfDeleted), so this is the net under those two rather than
// the mechanism — whatever a crash, an older build of this binary or a path
// nobody thought of left behind. Unlike the build-cache reclaim it runs on its
// own, on the disk watcher's tick: a cache is worth keeping and dropping one is
// a decision, while a directory no app row names is worth nothing to anybody.
//
// Three rules keep it from ever taking something live:
//
//   - only names shaped like an app id; `.staging`, `.trash` and the rest keep
//     their own rules;
//   - only what has been untouched for orphanGrace, so it cannot race a create
//     whose row and directory are seconds apart, or a build still writing;
//   - only after the app list was read, and never when it came back empty while
//     directories exist — a database that answers "no apps" to an install full
//     of them is a misconfiguration, not a reason to delete the estate.

// orphanGrace is how long a directory must sit untouched before it is judged.
const orphanGrace = time.Hour

// appIDShape is how an app id is spelled: a UUID.
var appIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// OrphanSweep is what the last sweep found, for Admin → Checks.
type OrphanSweep struct {
	At      time.Time
	Removed int
	Bytes   int64
}

var (
	lastOrphanSweepMu sync.Mutex
	lastOrphanSweep   OrphanSweep
)

// LastOrphanSweep is the most recent sweep that removed something; zero At
// means none has.
func (b *Builder) LastOrphanSweep() OrphanSweep {
	lastOrphanSweepMu.Lock()
	defer lastOrphanSweepMu.Unlock()
	return lastOrphanSweep
}

// sweepOrphans removes directories no app owns. Called on every disk-watcher
// tick whatever the disk's level: an orphan costs space whether or not the
// disk is short of it, and the walk is one ReadDir per root.
func (b *Builder) sweepOrphans() {
	if b.db == nil {
		return
	}
	type root struct {
		dir   string
		trash bool // under APPS_DIR, through the trash like a delete
	}
	roots := []root{{b.opts.AppsDir, true}}
	if b.opts.JobRunDir != "" {
		roots = append(roots, root{b.opts.JobRunDir, false})
	}
	// LOGS_DIR's app directories, under the same rules. An install whose
	// LOGS_DIR another install shares would judge the other's directories by
	// its own database, which is why each must have its own, as each has its
	// own APPS_DIR.
	if b.opts.LogsDir != "" {
		roots = append(roots, root{b.opts.LogsDir, false})
	}

	var candidates []string
	for _, r := range roots {
		for _, name := range appIDDirs(r.dir) {
			candidates = append(candidates, filepath.Join(r.dir, name))
		}
	}
	if len(candidates) == 0 {
		return
	}

	apps, err := b.db.ListAllApps()
	if err != nil {
		log.Printf("orphans: listing apps: %v; sweeping nothing", err)
		return
	}
	if len(apps) == 0 {
		log.Printf("orphans: the database lists no apps while %d app directories exist; sweeping nothing", len(candidates))
		return
	}
	live := make(map[string]bool, len(apps))
	for _, a := range apps {
		live[a.ID] = true
	}

	var removed int
	var bytes int64
	for _, r := range roots {
		for _, name := range appIDDirs(r.dir) {
			if live[name] {
				continue
			}
			path := filepath.Join(r.dir, name)
			info, err := os.Lstat(path)
			if err != nil || time.Since(info.ModTime()) < orphanGrace {
				continue
			}
			// Only what this platform made: under a shared JOB_RUN_DIR or
			// LOGS_DIR another program's UUID-named directory would otherwise
			// be removed as root once it was an hour old (code review
			// 2026-11-11, Q2-7).
			if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
				continue
			}
			size := treeBytes(path)
			if r.trash {
				err = toTrash(b.opts.AppsDir, path)
			} else {
				err = os.RemoveAll(path)
			}
			if err != nil {
				log.Printf("orphans: removing %s: %v", path, err)
				continue
			}
			log.Printf("orphans: removed %s (%s), which no app owns", path, FormatBytes(size))
			removed++
			bytes += size
		}
	}
	if removed > 0 {
		lastOrphanSweepMu.Lock()
		lastOrphanSweep = OrphanSweep{At: time.Now(), Removed: removed, Bytes: bytes}
		lastOrphanSweepMu.Unlock()
	}
}

// appIDDirs lists the directories in dir whose names are app ids.
func appIDDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && appIDShape.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// treeBytes is the size of what is under path, for the log line.
func treeBytes(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// dropIfDeleted removes an app's directories when its row is gone — the end of
// a build that outlived its app. A read that fails keeps everything: only a
// clean "no such app" is grounds to delete.
func (b *Builder) dropIfDeleted(appID string) {
	if b.db == nil {
		return
	}
	app, err := b.db.GetApp(appID)
	if err != nil || app != nil {
		return
	}
	// With the store: the release this build may have published after the
	// delete's own Expire ran is the object nothing else can ever name (F-8).
	if err := DeleteAppStorage(b.opts.AppsDir, b.opts.JobRunDir, b.opts.LogsDir, b.artifacts, appID); err != nil {
		log.Printf("build: removing the files of deleted app %s: %v", appID, err)
		return
	}
	log.Printf("build: app %s was deleted while it built; its files are removed", appID)
}

package builder

import (
	"io/fs"
	"os"
	"path/filepath"
)

// dirSize sums the apparent size of every regular file under root.
//
// Apparent bytes rather than allocated blocks, for two reasons. It is the same
// quantity the artifact's packed size is derived from, so the two numbers a
// deployment reports are comparable rather than measured on different scales.
// And it needs no platform-specific stat, which keeps a plain byte count out of
// the small set of places that have to care what OS this is.
//
// The consequence is that a tree of many tiny files — which is exactly what a
// node_modules or a venv is — reads slightly under what `du` would say, because
// per-file slack is not counted. It is an understatement of a real cost, never
// an overstatement, which is the right direction for a number an operator uses
// to decide what to delete.
//
// Symlinks are counted as links, not as their targets: following them would
// double-count a tree that links within itself, and a release directory is
// full of those.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable entry should not lose the whole measurement — a
			// slightly low number is more useful than none at all.
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// measureReleaseDir reports the size of the release a deployment owns, or nil
// when it owns no directory on this host. Both are ordinary: a rollback row's
// release is the directory its *source* deployment owns, and an isolated
// workload's release never lands here at all.
func measureReleaseDir(appsDir, appID, deployID string) *int64 {
	dir := ReleaseDir(appsDir, appID, deployID)
	if !fileExists(dir) {
		return nil
	}
	n, err := dirSize(dir)
	if err != nil {
		return nil
	}
	return &n
}

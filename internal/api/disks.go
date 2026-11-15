package api

import (
	"sort"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
)

// The Disks block on Admin → Checks: every filesystem this install writes to,
// how full it is, and which of its folders sit on it.
//
// **Measured only when the page asks, and only with statfs.** One syscall per
// folder, no directory walk — the build cache's size is the walk, and that
// stays on its fifteen-minute ticker. Nothing polls this: the page fetches the
// report when it is opened. What can still be slow is a network mount that has
// stopped answering, where statfs blocks; each measurement is given
// diskMeasureTimeout and a folder that does not answer in it is reported as
// such rather than holding the page.

const diskMeasureTimeout = time.Second

// unboundedBytes is past any disk a host has. A network filesystem reports a
// size it does not have — EFS answers 8 EiB — and "0% full" of that is a
// figure nobody can act on.
const unboundedBytes = 1 << 50 // 1 PiB

// diskFolder is one folder the install writes to and what it holds.
type diskFolder struct {
	Path string `json:"path"`
	What string `json:"what"`
}

// diskReport is one filesystem.
type diskReport struct {
	Folders     []diskFolder `json:"folders"`
	TotalBytes  uint64       `json:"total_bytes"`
	UsedBytes   uint64       `json:"used_bytes"`
	FreeBytes   uint64       `json:"free_bytes"`
	UsedPercent float64      `json:"used_percent"`
	// Unbounded is a network filesystem with no size worth stating; the page
	// draws no bar for it.
	Unbounded bool `json:"unbounded,omitempty"`
	// Level is the release disk's watermarks applied to this filesystem:
	// ok, low (below the soft watermark) or full (below the hard one).
	Level string `json:"level"`
}

// diskUnanswered is a folder whose filesystem did not answer in time, or at
// all.
type diskUnanswered struct {
	diskFolder
	Error string `json:"error"`
}

// diskFolders is what the install writes to, in the order the page lists it.
func (s *Server) diskFolders() []diskFolder {
	var out []diskFolder
	for _, f := range []diskFolder{
		{s.cfg.AppsDir, "App releases"},
		{s.cfg.JobRunDir, "Job runs"},
		{s.cfg.CacheDir, "Build cache"},
		{s.cfg.LogsDir, "Logs"},
	} {
		if f.Path != "" {
			out = append(out, f)
		}
	}
	roots := s.storageRoots()
	if len(roots) == 0 {
		return out
	}
	labels, _ := s.db.ListStorageDriveLabels()
	for _, r := range roots {
		name := r.Name
		if l, ok := labels[database.DriveKeyForRoot(r.Name)]; ok && l.Label != "" {
			name = l.Label
		}
		out = append(out, diskFolder{r.Path, "Storage drive “" + name + "”"})
	}
	return out
}

// disksBlock measures the folders and groups them by filesystem. Nil when
// there is no builder to measure with.
func (s *Server) disksBlock() map[string]any {
	if s.builder == nil {
		return nil
	}
	disks, unanswered := disksFrom(s.diskFolders(), func(path string) (builder.DiskUsage, error) {
		return measureWithin(s.builder.MeasureDisk, path, diskMeasureTimeout)
	})
	block := map[string]any{"disks": disks}
	if len(unanswered) > 0 {
		block["unanswered"] = unanswered
	}
	return block
}

// measureWithin runs measure, giving up after d. A statfs on a hung mount
// cannot be cancelled, so the goroutine is left to finish on its own; the
// buffered channel lets it.
func measureWithin(measure func(string) (builder.DiskUsage, error), path string, d time.Duration) (builder.DiskUsage, error) {
	type result struct {
		u   builder.DiskUsage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		u, err := measure(path)
		ch <- result{u, err}
	}()
	select {
	case r := <-ch:
		return r.u, r.err
	case <-time.After(d):
		return builder.DiskUsage{Path: path}, errDiskTimeout
	}
}

type diskTimeoutError struct{}

func (diskTimeoutError) Error() string { return "did not answer within a second" }

var errDiskTimeout error = diskTimeoutError{}

// disksFrom is the grouping itself, a pure function of what each folder
// measured, so it can be asserted without a disk.
func disksFrom(folders []diskFolder, measure func(string) (builder.DiskUsage, error)) ([]diskReport, []diskUnanswered) {
	var (
		order      []uint64
		byDev      = map[uint64]*diskReport{}
		unanswered []diskUnanswered
	)
	for _, f := range folders {
		u, err := measure(f.Path)
		if err != nil {
			unanswered = append(unanswered, diskUnanswered{f, err.Error()})
			continue
		}
		if d, ok := byDev[u.Device]; ok {
			d.Folders = append(d.Folders, f)
			continue
		}
		d := &diskReport{
			Folders:    []diskFolder{f},
			TotalBytes: u.Total,
			UsedBytes:  u.Used,
			FreeBytes:  u.Free,
			Unbounded:  u.Total >= unboundedBytes,
			Level:      diskLevelWord(u.Level),
		}
		if u.Used+u.Free > 0 {
			d.UsedPercent = float64(u.Used) * 100 / float64(u.Used+u.Free)
		}
		byDev[u.Device] = d
		order = append(order, u.Device)
	}
	out := make([]diskReport, 0, len(order))
	for _, dev := range order {
		out = append(out, *byDev[dev])
	}
	// Fullest first: the row somebody opened the page for leads. A network
	// filesystem has no fullness and goes last. Stable, so equal disks keep
	// the order their folders were listed in.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Unbounded != out[j].Unbounded {
			return !out[i].Unbounded
		}
		return out[i].UsedPercent > out[j].UsedPercent
	})
	return out, unanswered
}

func diskLevelWord(l builder.DiskLevel) string {
	switch l {
	case builder.DiskHard:
		return "full"
	case builder.DiskSoft:
		return "low"
	}
	return "ok"
}

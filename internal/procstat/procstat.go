// Package procstat reads a workload's CPU time and resident memory from /proc,
// either as one process (`Read`) or as a whole process tree (`TreeSampler`).
//
// The tree is the one both callers want and the single process is what they
// used to get: a launcher that wraps the workload — `npm run start` — puts the
// real server one level down, where a single-pid reading cannot see it. Why the
// tree has to be sampled statefully is in tree.go.
//
// It is a leaf — it imports nothing of Toft's — because it has two
// unrelated callers with no business knowing about each other:
// `internal/resource`, which bills a workload for what it used, and
// `internal/runtime`, whose shared backend reports what a workload is doing
// right now. The alternative was exporting it from `internal/resource` and
// importing that from the workload seam, which costs no package and creates no
// cycle, but points the seam at the accounting package — a direction that would
// read as an accident later. That it was already separately tested is the tell
// that it was always its own subject.
package procstat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Stat is the pair of counters worth having from /proc.
type Stat struct {
	// CPUSeconds is cumulative user+system CPU time since the process started.
	// A rate is the difference between two of these over the wall time between
	// them; this number alone says nothing about how busy anything is.
	CPUSeconds float64
	// RSSBytes is resident memory now.
	RSSBytes int64
}

// clockTicks is the kernel's USER_HZ. It is 100 on every Linux platform Go
// supports, and reading it properly needs cgo — which this codebase avoids
// deliberately (pgx is used precisely to stay CGO-free).
const clockTicks = 100

// pageSize converts /proc's RSS, which is counted in pages.
var pageSize = int64(os.Getpagesize())

// Read reads one process's cumulative CPU time and resident memory.
//
// /proc/[pid]/stat is used rather than a `ps` subprocess: sampling every app
// every interval would otherwise fork once per app per tick, which is the kind
// of overhead that makes an operator turn measurement off.
//
// A process that has gone, a pid the caller may not read, and a kernel with no
// procfs are all one thing to a caller: an error, and therefore no reading —
// never a zero, which would be a claim that the process is idle.
func Read(pid int) (Stat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Stat{}, err
	}
	return Parse(string(data))
}

// Parse pulls utime, stime and rss out of a /proc/[pid]/stat line.
//
// The comm field (2) is parenthesised and may itself contain spaces and
// parentheses, so the line is split at the *last* ')' rather than tokenised
// from the start — a process named "my app) (weird" is otherwise enough to
// shift every subsequent field and silently produce nonsense.
func Parse(line string) (Stat, error) {
	endComm := strings.LastIndex(line, ")")
	if endComm < 0 || endComm+2 >= len(line) {
		return Stat{}, fmt.Errorf("malformed /proc stat line")
	}

	// Fields after comm, 1-indexed from field 3 (state).
	fields := strings.Fields(line[endComm+2:])
	// utime is field 14, stime 15, rss 24 — offsets 11, 12 and 21 here.
	const (
		utimeIdx = 11
		stimeIdx = 12
		rssIdx   = 21
	)
	if len(fields) <= rssIdx {
		return Stat{}, fmt.Errorf("/proc stat line has %d fields after comm, want > %d", len(fields), rssIdx)
	}

	utime, err := strconv.ParseFloat(fields[utimeIdx], 64)
	if err != nil {
		return Stat{}, fmt.Errorf("parsing utime: %w", err)
	}
	stime, err := strconv.ParseFloat(fields[stimeIdx], 64)
	if err != nil {
		return Stat{}, fmt.Errorf("parsing stime: %w", err)
	}
	rssPages, err := strconv.ParseInt(fields[rssIdx], 10, 64)
	if err != nil {
		return Stat{}, fmt.Errorf("parsing rss: %w", err)
	}

	return Stat{
		CPUSeconds: (utime + stime) / clockTicks,
		RSSBytes:   rssPages * pageSize,
	}, nil
}

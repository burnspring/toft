package builder

import (
	"fmt"
	"sort"
	"strings"
)

// What is never part of an app's source (ADR §60).
//
// This repository had decided the same thing three times before there was a
// list: `toft deploy` decided what is not source, the deployment-archive download
// decided what is not worth carrying, and `copyTree` decided what is never
// carried between releases. Three files, one idea — and the only door that
// ignored all of them was the browser upload, the one a person uses by hand.
//
// Two maps rather than one because the test is not the same test: a *directory*
// named `node_modules` is freight, and a *file* of that name is somebody's
// source.

// ExcludedDirs are directory names that never travel: dependency trees the
// build reinstalls from a manifest, and VCS metadata.
//
// **Build output is deliberately absent, and that is where this diverges from
// the CLI.** `toft deploy` also drops `dist`, `build`, `out` and their siblings,
// conditional on a `package.json` at the tree root — the right bet for a client
// looking at a working tree it can reason about, and the wrong one here. A
// dropped dependency tree is always rebuildable; a dropped output tree
// sometimes is not, because a static site's `dist/` is the deliverable rather
// than output. Getting that wrong deploys an empty app whose build log says
// every step succeeded, which is the worst failure shape there is.
//
// **Not configurable**, on the same reasoning as the static-hosting refusal
// list: an install that can edit it is an install where the answer to "what did
// the platform do to my archive" is different on every box.
var ExcludedDirs = map[string]bool{
	"node_modules": true,
	"venv":         true,
	".venv":        true,
	"__pycache__":  true,
	".git":         true,
	".hg":          true,
	".svn":         true,
}

// ExcludedFiles are matched on a file's own name rather than on a directory
// component above it. `.git` is here as well as in ExcludedDirs: a worktree's
// or submodule's `.git` is a file naming a local path, and one that survived an
// upload made a redeploy run git against a directory the uploader wrote
// (security review 2026-11-14, CS-7).
var ExcludedFiles = map[string]bool{
	".DS_Store": true,
	".git":      true,
}

// ExcludedNode reports whether one node of a tree is excluded. This is the form
// the two directory walkers use: they meet a file or a directory, never a path.
func ExcludedNode(name string, isDir bool) bool {
	if isDir {
		return ExcludedDirs[name]
	}
	return ExcludedFiles[name]
}

// ExcludedArchiveEntry names the component that excludes a slash-separated
// archive entry, or "" if nothing does.
//
// **Any segment, not the first.** `packages/web/node_modules/…` is the ordinary
// shape in a monorepo, and a rooted test would pass it straight through.
//
// The name it returns is what the removal is reported under, so every
// `node_modules` in a tree aggregates to one line rather than one line per
// workspace — which is what somebody reading the report wants to know.
func ExcludedArchiveEntry(name string, isDir bool) string {
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return ""
	}
	segs := strings.Split(name, "/")
	last := len(segs) - 1
	for i, s := range segs {
		if i == last && !isDir {
			if ExcludedFiles[s] {
				return s
			}
			return ""
		}
		if ExcludedDirs[s] {
			return s
		}
	}
	return ""
}

// RemovedEntry is one name taken out of an uploaded archive, totalled over
// every place in the tree it appeared.
//
// Bytes come from the archive's own headers rather than from what was written,
// because nothing excluded is ever read. That is the one place in this file
// where a lying archive is believed, and it is safe: this is a report, and the
// caps that are not a report count what actually lands on disk.
type RemovedEntry struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// RemovedTally accumulates removals while an archive is being read, and orders
// them largest-first when it is done — the order somebody reads the answer in.
type RemovedTally struct {
	byName map[string]*RemovedEntry
}

// Add records one removed entry under the name that excluded it.
//
// A negative size is dropped rather than added: the caller's source is the
// archive's own header, and an archive that declares 2^63 bytes would otherwise
// have the report render a negative total. Nothing else is at stake — the caps
// count what lands on disk — so the answer is to say nothing about that entry's
// size rather than to say something absurd.
func (t *RemovedTally) Add(name string, bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	if t.byName == nil {
		t.byName = map[string]*RemovedEntry{}
	}
	e := t.byName[name]
	if e == nil {
		e = &RemovedEntry{Path: name}
		t.byName[name] = e
	}
	e.Files++
	e.Bytes += bytes
}

// Entries returns the tally, largest first, or nil if nothing was removed.
// Nil rather than an empty slice: `omitempty` is what keeps an unfiltered
// upload's response byte-for-byte what it was.
func (t *RemovedTally) Entries() []RemovedEntry {
	if len(t.byName) == 0 {
		return nil
	}
	out := make([]RemovedEntry, 0, len(t.byName))
	for _, e := range t.byName {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// RemovedBlock renders the report for the head of a build log.
//
// The platform changed what somebody shipped. That is fine, and it has to be
// legible — the same standard the `.env.<tag>` seeder is already held to.
func RemovedBlock(removed []RemovedEntry) []string {
	if len(removed) == 0 {
		return nil
	}
	nameW, countW := 0, 0
	counts := make([]string, len(removed))
	for i, e := range removed {
		counts[i] = commas(int64(e.Files)) + " file"
		if e.Files != 1 {
			counts[i] += "s"
		}
		if len(e.Path) > nameW {
			nameW = len(e.Path)
		}
		if len(counts[i]) > countW {
			countW = len(counts[i])
		}
	}
	// Counts right-aligned so the commas line up whether or not a row is
	// singular; names left-aligned, because they are read as words.
	lines := []string{"==> Removed from the uploaded archive before installing:"}
	for i, e := range removed {
		lines = append(lines, fmt.Sprintf("      %-*s  %*s, %s",
			nameW, e.Path, countW, counts[i], FormatBytes(e.Bytes)))
	}
	return append(lines, "    These are rebuilt by the deploy or are not needed to run the app.")
}

// FormatBytes renders a size the way a person reads one. Deliberately the same
// shape as the CLI's, because the two report the same fact about the same
// archive from opposite ends of the wire.
func FormatBytes(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// commas groups a count in threes. A five-digit file count is the normal case
// here — a mid-sized node_modules is 20,000 files — and unseparated it reads as
// noise rather than as a number.
func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

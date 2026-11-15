package builder

import (
	"strings"
	"testing"
)

// The archive test is on any path segment, not the first one: a monorepo's
// `packages/*/node_modules` is the ordinary shape, and a rooted test would pass
// it straight through.
func TestExcludedArchiveEntry(t *testing.T) {
	cases := []struct {
		name  string
		isDir bool
		want  string
	}{
		{"node_modules/left-pad/index.js", false, "node_modules"},
		{"node_modules/", true, "node_modules"},
		{"packages/web/node_modules/react/index.js", false, "node_modules"},
		{"my-app/.git/config", false, ".git"},
		{".hg/store/data.i", false, ".hg"},
		{".svn/entries", false, ".svn"},
		{"api/venv/pyvenv.cfg", false, "venv"},
		{"api/.venv/bin/python", false, ".venv"},
		{"api/__pycache__/mod.pyc", false, "__pycache__"},
		{".DS_Store", false, ".DS_Store"},
		{"assets/.DS_Store", false, ".DS_Store"},

		// Kept. A *file* named node_modules is somebody's source, and build
		// output is deliberately not on the list at all — a static site's
		// `dist/` is the deliverable, and nothing in such a tree can rebuild
		// what was dropped.
		{"docs/.git", false, ".git"},
		{"sub/.git", false, ".git"},
		{".git", false, ".git"},

		{"node_modules", false, ""},
		{"dist/index.html", false, ""},
		{"build/main.js", false, ""},
		{"out/index.html", false, ""},
		{".next/server/page.js", false, ""},
		{"src/node_modules_helper.ts", false, ""},
		{"src/app.py", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		if got := ExcludedArchiveEntry(c.name, c.isDir); got != c.want {
			t.Errorf("ExcludedArchiveEntry(%q, isDir=%v) = %q, want %q", c.name, c.isDir, got, c.want)
		}
	}
}

// The walkers meet one node at a time, and the same asymmetry holds there.
func TestExcludedNode(t *testing.T) {
	if !ExcludedNode("node_modules", true) {
		t.Error("a node_modules directory should be excluded")
	}
	if ExcludedNode("node_modules", false) {
		t.Error("a file named node_modules is source")
	}
	if !ExcludedNode(".DS_Store", false) {
		t.Error(".DS_Store should be excluded")
	}
	if !ExcludedNode(".git", false) {
		t.Error("a .git file (a worktree's gitdir pointer) should be excluded")
	}
	if ExcludedNode(".next", true) {
		t.Error(".next is build output and is not in the shared set — the two " +
			"call sites that drop it spell it as an explicit union")
	}
}

// `.next` is the one member the release-to-release copy adds, and it stays an
// explicit union rather than a member of the shared set.
func TestBuildArtifactIsTheSharedSetPlusNext(t *testing.T) {
	if !buildArtifact(".next", true) {
		t.Error("copyTree must still drop .next between releases")
	}
	if !buildArtifact("node_modules", true) || !buildArtifact(".git", true) {
		t.Error("copyTree must drop everything the shared set names")
	}
	if buildArtifact("dist", true) {
		t.Error("dist is not output the platform may assume it can rebuild")
	}
}

// Largest first, aggregated by the name that excluded them: every
// `node_modules` in a tree is one line, because "what did node_modules cost me"
// is the question the report answers.
func TestRemovedTallyAggregatesAndOrders(t *testing.T) {
	var tally RemovedTally
	tally.Add(".git", 22)
	tally.Add("node_modules", 100)
	tally.Add("node_modules", 84)
	tally.Add(".DS_Store", 6)

	got := tally.Entries()
	if len(got) != 3 {
		t.Fatalf("entries = %+v, want three names", got)
	}
	if got[0].Path != "node_modules" || got[0].Files != 2 || got[0].Bytes != 184 {
		t.Errorf("first entry = %+v", got[0])
	}
	if got[1].Path != ".git" || got[2].Path != ".DS_Store" {
		t.Errorf("order = %+v, want largest first", got)
	}

	// Nil rather than empty, so `omitempty` keeps an unfiltered upload's
	// response byte-for-byte what it was.
	var empty RemovedTally
	if empty.Entries() != nil {
		t.Error("an empty tally must render as nothing at all")
	}
}

// The block the build log opens with. It is checked as text because it is text
// somebody reads — the alignment is what makes two sizes comparable at a
// glance, and the closing line is what says why the platform did this.
func TestRemovedBlock(t *testing.T) {
	lines := RemovedBlock([]RemovedEntry{
		{Path: "node_modules", Files: 12431, Bytes: 193164902},
		{Path: ".git", Files: 892, Bytes: 23803821},
	})
	if len(lines) != 4 {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.HasPrefix(lines[0], "==> Removed from the uploaded archive") {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "node_modules") || !strings.Contains(lines[1], "12,431 files") ||
		!strings.Contains(lines[1], "184.2 MB") {
		t.Errorf("first row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "892 files") || !strings.Contains(lines[2], "22.7 MB") {
		t.Errorf("second row = %q", lines[2])
	}
	// Aligned: the sizes start in the same column, so two rows read as the
	// different magnitudes they are.
	if strings.Index(lines[1], ", ") != strings.Index(lines[2], ", ") {
		t.Errorf("columns do not line up:\n%q\n%q", lines[1], lines[2])
	}

	// One file is one file. This is a report somebody reads.
	one := RemovedBlock([]RemovedEntry{{Path: ".git", Files: 1, Bytes: 270177}})
	if !strings.Contains(one[1], "1 file,") {
		t.Errorf("singular row = %q", one[1])
	}
	if !strings.Contains(lines[3], "rebuilt by the deploy") {
		t.Errorf("footer = %q", lines[3])
	}

	// Nothing removed, nothing said: an ordinary archive's log is unchanged.
	if RemovedBlock(nil) != nil {
		t.Error("RemovedBlock(nil) must print nothing")
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:         "0 B",
		512:       "512 B",
		1536:      "1.5 KB",
		193164902: "184.2 MB",
		2 << 30:   "2.0 GB",
		// EFS reports an elastic filesystem as 8 EiB; capped at TB it read
		// as "8388608.0 TB" on Admin → Checks.
		7 << 60: "7.0 EB",
		3 << 50: "3.0 PB",
	}
	for n, want := range cases {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

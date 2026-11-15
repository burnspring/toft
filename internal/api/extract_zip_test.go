package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/builder"
)

// zipOf builds an archive from name → contents.
func zipOf(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := io.WriteString(f, body); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

func extractInto(t *testing.T, r *bytes.Reader) (string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release")
	_, err := extractArchive(r, r.Size(), dir, "app.zip")
	return dir, err
}

// extractReporting is extractInto for the tests that are about the report
// rather than about what landed.
func extractReporting(t *testing.T, r *bytes.Reader) (string, []builder.RemovedEntry) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release")
	removed, err := extractArchive(r, r.Size(), dir, "app.zip")
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	return dir, removed
}

// absent fails when a path landed in the release.
func absent(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
		t.Errorf("%s survived the filter", rel)
	}
}

// present fails when a path did not land in the release.
func present(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Errorf("%s is missing: %v", rel, err)
	}
}

// The compressed size is capped by both callers at 100MB, which bounds nothing
// on its own: a few kilobytes of zeroes expand to gigabytes. This mattered less
// while every archive was one a person had chosen; the gallery makes the
// platform fetch archives on its own initiative from a list it did not write.
func TestExtractZipRefusesAnArchiveThatExpandsTooFar(t *testing.T) {
	old := maxUnzippedBytes
	maxUnzippedBytes = 4096
	t.Cleanup(func() { maxUnzippedBytes = old })

	r := zipOf(t, map[string]string{"big.bin": strings.Repeat("\x00", 64<<10)})
	_, err := extractInto(t, r)
	if err == nil || !strings.Contains(err.Error(), "expands") {
		t.Fatalf("err = %v, want the expansion cap", err)
	}
}

func TestExtractZipRefusesTooManyEntries(t *testing.T) {
	old := maxUnzippedFiles
	maxUnzippedFiles = 10
	t.Cleanup(func() { maxUnzippedFiles = old })

	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = "x"
	}
	_, err := extractInto(t, zipOf(t, files))
	// By name rather than generically: a documentation build or an image-heavy
	// static site reaches this cap long before a codebase does, and "your site
	// is too big" is an answer somebody can act on.
	if err == nil || !strings.Contains(err.Error(), "files") || !strings.Contains(err.Error(), "10") {
		t.Fatalf("err = %v, want the file-count cap named", err)
	}
}

// The caps must not disturb an ordinary archive, including the common-root
// stripping every uploaded ZIP relies on.
func TestExtractZipStillExtractsAnOrdinaryArchive(t *testing.T) {
	dir, err := extractInto(t, zipOf(t, map[string]string{
		"my-app/app.py":           "print('hi')\n",
		"my-app/requirements.txt": "flask\n",
	}))
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	for _, name := range []string{"app.py", "requirements.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is missing: %v", name, err)
		}
	}
}

// A folder zipped by the Finder still strips its wrapper.
//
// This is the failure a static site is most likely to arrive by: somebody drags
// a folder of HTML in rather than pushing it to git, and macOS puts `__MACOSX/`
// beside `mysite/`. Read literally that is two top-level directories, so the
// prefix would not be stripped and the release root would hold a *directory*
// instead of a site — no index.html at the root, and a deploy that fails for a
// reason nobody could act on.
func TestExtractZipStripsAFinderWrappedFolder(t *testing.T) {
	dir, err := extractInto(t, zipOf(t, map[string]string{
		"mysite/index.html":       "<h1>hi</h1>",
		"mysite/css/site.css":     "body{}",
		"mysite/.DS_Store":        "junk",
		".DS_Store":               "junk",
		"__MACOSX/._mysite":       "junk",
		"__MACOSX/mysite/._index": "junk",
	}))
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	for _, name := range []string{"index.html", "css/site.css"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s did not land at the release root: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "mysite")); err == nil {
		t.Error("the wrapper directory survived — the release root holds a folder, not a site")
	}
}

// The traversal guard predates this work; it is asserted here because the
// gallery makes the input remote.
func TestExtractZipIgnoresTraversal(t *testing.T) {
	dir, err := extractInto(t, zipOf(t, map[string]string{
		"../escape.txt": "nope",
		"app.py":        "ok",
	}))
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Error("a ../ entry escaped the destination")
	}
}

// ---------------------------------------------------------------------------
// What the platform removes from an uploaded archive (ADR §60).
// ---------------------------------------------------------------------------

// The common case, and the one the whole design is about: a dependency tree at
// the root of the archive.
func TestExtractZipRemovesARootDependencyTree(t *testing.T) {
	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"package.json":               "{}",
		"server.js":                  "listen()",
		"node_modules/left-pad/i.js": "module.exports=1",
		"node_modules/.package-lock": "{}",
		".git/config":                "[remote \"origin\"]",
		".git/objects/ab/cd":         "binary",
		".DS_Store":                  "junk",
	}))
	present(t, dir, "package.json")
	present(t, dir, "server.js")
	absent(t, dir, "node_modules")
	absent(t, dir, ".git")
	absent(t, dir, ".DS_Store")

	// Largest first, and aggregated by the name that excluded them rather than
	// one line per file.
	if len(removed) != 3 {
		t.Fatalf("removed = %+v, want three names", removed)
	}
	byName := map[string]builder.RemovedEntry{}
	for _, e := range removed {
		byName[e.Path] = e
	}
	if got := byName["node_modules"].Files; got != 2 {
		t.Errorf("node_modules: %d files, want 2", got)
	}
	if got := byName[".git"].Files; got != 2 {
		t.Errorf(".git: %d files, want 2", got)
	}
	if byName[".DS_Store"].Files != 1 {
		t.Errorf(".DS_Store was not reported: %+v", removed)
	}
}

// Nested, not rooted: `packages/*/node_modules` is the ordinary monorepo shape,
// and a rooted test would pass it straight through.
func TestExtractZipRemovesANestedDependencyTree(t *testing.T) {
	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"package.json":                         "{}",
		"packages/web/src/main.ts":             "//",
		"packages/web/node_modules/react/i.js": "//",
		"packages/api/node_modules/pg/i.js":    "//",
	}))
	present(t, dir, "packages/web/src/main.ts")
	absent(t, dir, "packages/web/node_modules")
	absent(t, dir, "packages/api/node_modules")

	// Two workspaces, one line: what somebody reads the report to learn is
	// "node_modules cost me this much", not which workspace it was in.
	if len(removed) != 1 || removed[0].Path != "node_modules" || removed[0].Files != 2 {
		t.Fatalf("removed = %+v, want one node_modules line covering both workspaces", removed)
	}
}

// A *file* named node_modules is somebody's source. The two maps in
// builder/exclude.go exist for exactly this.
func TestExtractZipKeepsAFileNamedLikeAnExcludedDirectory(t *testing.T) {
	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"app.py":       "print(1)",
		"node_modules": "this is a file, not a tree",
	}))
	present(t, dir, "node_modules")
	if len(removed) != 0 {
		t.Fatalf("removed = %+v, want nothing", removed)
	}
}

// A `.git` *file* is the exception: a worktree's gitdir pointer is never
// source, and one that survived the upload made a redeploy run git against a
// git directory the uploader wrote (security review 2026-11-14, CS-7).
func TestExtractZipDropsAGitFile(t *testing.T) {
	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"app.py":    "print(1)",
		"docs/.git": "gitdir: g",
	}))
	absent(t, dir, "docs/.git")
	if len(removed) != 1 || removed[0].Path != ".git" {
		t.Fatalf("removed = %+v, want one .git line", removed)
	}
}

// Build output is deliberately not on the list, and the condition the CLI
// applies — a package.json at the root — is deliberately not applied here. A
// static site's dist/ is the deliverable and nothing in the tree can rebuild
// it; guessing wrong deploys an empty app whose log says every step succeeded.
func TestExtractZipKeepsBuildOutputWithAndWithoutAManifest(t *testing.T) {
	for _, manifest := range []bool{true, false} {
		files := map[string]string{
			"dist/index.html":      "<h1>hi</h1>",
			"dist/assets/app.js":   "//",
			"build/out.txt":        "x",
			".next/server/page.js": "//",
		}
		if manifest {
			files["package.json"] = "{}"
		}
		dir, removed := extractReporting(t, zipOf(t, files))
		for _, rel := range []string{"dist/index.html", "dist/assets/app.js", "build/out.txt", ".next/server/page.js"} {
			present(t, dir, rel)
		}
		if len(removed) != 0 {
			t.Errorf("package.json=%v: removed = %+v, want nothing", manifest, removed)
		}
	}
}

// An archive whose only top-level directory is excluded would otherwise have
// that directory read as the wrapper folder, stripped, and its contents
// extracted at the release root with nothing left to match on.
func TestExtractZipDoesNotTreatAnExcludedDirectoryAsTheWrapper(t *testing.T) {
	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"node_modules/left-pad/index.js": "//",
	}))
	absent(t, dir, "left-pad")
	absent(t, dir, "left-pad/index.js")
	if len(removed) != 1 || removed[0].Path != "node_modules" {
		t.Fatalf("removed = %+v, want the tree reported", removed)
	}
}

// The wrapper folder still strips, and the filter reads the stripped name —
// `my-app/node_modules/…` and `node_modules/…` are the same thing.
func TestExtractZipFiltersInsideAWrapperFolder(t *testing.T) {
	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"my-app/package.json":                   "{}",
		"my-app/src/index.js":                   "//",
		"my-app/node_modules/left-pad/index.js": "//",
		"my-app/.git/config":                    "[core]",
	}))
	present(t, dir, "package.json")
	present(t, dir, "src/index.js")
	absent(t, dir, "node_modules")
	absent(t, dir, ".git")
	if len(removed) != 2 {
		t.Fatalf("removed = %+v, want node_modules and .git", removed)
	}
}

// Counted after the filter, and this is the test the decision exists for: a
// mid-sized node_modules is the file cap by itself, so counting before it
// refused an app whose own source was two hundred files — and blamed the app.
func TestExtractZipCountsTheFileCapAfterTheFilter(t *testing.T) {
	old := maxUnzippedFiles
	maxUnzippedFiles = 100
	t.Cleanup(func() { maxUnzippedFiles = old })

	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("src/f%d.js", i)] = "//"
	}
	for i := 0; i < 500; i++ {
		files[fmt.Sprintf("node_modules/p%d/index.js", i)] = "//"
	}
	dir, removed := extractReporting(t, zipOf(t, files))
	present(t, dir, "src/f0.js")
	if len(removed) != 1 || removed[0].Files != 500 {
		t.Fatalf("removed = %+v, want 500 files under node_modules", removed)
	}
}

// And the cap keeps doing its real job: a zip bomb of a million *source* files
// is refused with the message it is refused with today.
func TestExtractZipStillRefusesTooManySourceFiles(t *testing.T) {
	old := maxUnzippedFiles
	maxUnzippedFiles = 100
	t.Cleanup(func() { maxUnzippedFiles = old })

	files := map[string]string{}
	for i := 0; i < 150; i++ {
		files[fmt.Sprintf("src/f%d.js", i)] = "//"
	}
	_, err := extractInto(t, zipOf(t, files))
	if err == nil || !strings.Contains(err.Error(), "files") || !strings.Contains(err.Error(), "100") {
		t.Fatalf("err = %v, want the file-count cap named", err)
	}
}

// The byte cap is counted after the filter too: removed bytes are never read,
// so they cannot be written.
func TestExtractZipCountsTheByteCapAfterTheFilter(t *testing.T) {
	old := maxUnzippedBytes
	maxUnzippedBytes = 8 << 10
	t.Cleanup(func() { maxUnzippedBytes = old })

	dir, removed := extractReporting(t, zipOf(t, map[string]string{
		"app.py":                   "print(1)",
		"node_modules/big/blob.js": strings.Repeat("x", 64<<10),
	}))
	present(t, dir, "app.py")
	if len(removed) != 1 || removed[0].Bytes != 64<<10 {
		t.Fatalf("removed = %+v, want the tree's declared size reported", removed)
	}
}

// The report is what the build log prints, and it prints nothing when nothing
// was removed — an ordinary archive's deploy log is unchanged.
func TestRemovedBlockIsSilentWhenNothingWasRemoved(t *testing.T) {
	if lines := builder.RemovedBlock(nil); lines != nil {
		t.Fatalf("RemovedBlock(nil) = %v, want nothing", lines)
	}
}

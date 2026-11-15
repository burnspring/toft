package bundle

import (
	"archive/zip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The matcher, against the patterns that actually appear in `.gitignore` files.
//
// This is the table test the design asked for by name, and it earns its place:
// the whole reason this package exists rather than shelling out to
// `git check-ignore` is that a plain folder has to bundle too — so this
// implementation is the only definition of the rule, in both cases.
func TestIgnorePatterns(t *testing.T) {
	cases := []struct {
		name     string
		lines    []string
		path     string
		isDir    bool
		excluded bool
	}{
		{"a bare name matches at the root", []string{"secrets.txt"}, "secrets.txt", false, true},
		{"a bare name matches at any depth", []string{"secrets.txt"}, "app/config/secrets.txt", false, true},
		{"an anchored name matches only at the root", []string{"/secrets.txt"}, "app/secrets.txt", false, false},
		{"a directory pattern matches the directory", []string{"build/"}, "build", true, true},
		{"a directory pattern does not match a file of that name", []string{"build/"}, "build", false, false},
		{"a suffix glob", []string{"*.log"}, "logs/app.log", false, true},
		{"a glob does not cross a slash", []string{"logs/*.log"}, "logs/nested/app.log", false, false},
		{"** does cross a slash", []string{"logs/**/*.log"}, "logs/a/b/app.log", false, true},
		{"a comment is not a pattern", []string{"# secrets.txt"}, "secrets.txt", false, false},
		{"a blank line is not a pattern", []string{"", "  "}, "secrets.txt", false, false},

		// Negation, which is the reason the matcher does not return on the
		// first hit. A matcher that stopped early would silently drop every
		// `!keep.me` in every repository.
		{"a negation rescues a file", []string{"*.log", "!keep.log"}, "keep.log", false, false},
		{"a negation only rescues what it names", []string{"*.log", "!keep.log"}, "other.log", false, true},
		{"order matters: a later rule re-excludes", []string{"!keep.log", "*.log"}, "keep.log", false, true},

		{"? matches one character", []string{"app.?s"}, "app.js", false, true},

		// A slash in the middle anchors, which is git's rule and the one people
		// are most often surprised by: `a/b` is about the root, and `**/b` is
		// the way to say "at any depth".
		{"a slash in the middle anchors the pattern", []string{"a/b"}, "root/a/b", false, false},
		{"an anchored pattern matches at its own level", []string{"a/b"}, "a/b", false, true},
		{"**/ matches at any depth", []string{"**/b"}, "root/a/b", false, true},

		// A leading slash is "at this level", and a one-segment glob there
		// does not cross into subdirectories: the whitelist `/*` + `!/src`
		// dropped everything under src/ (code review 2026-11-11, M2-1).
		{"an anchored star stays at the root", []string{"/*"}, "src/app.js", false, false},
		{"an anchored star matches at the root", []string{"/*"}, "root.json", false, true},
		{"an anchored suffix glob stays at the root", []string{"/*.json"}, "config/app.json", false, false},
		{"the whitelist keeps what it names and what is under it", []string{"/*", "!/src", "!/package.json"}, "src/app.js", false, false},
		{"the whitelist still drops the rest at the root", []string{"/*", "!/src"}, "config/app.json", false, false},
		// A trailing ** is what is inside, never the directory itself, so the
		// walk enters it and a negation under it can rescue (M2-5).
		{"a trailing ** does not match the directory itself", []string{"data/**"}, "data", true, false},
		{"a trailing ** matches what is inside", []string{"data/**"}, "data/x.bin", false, true},
		{"a negation under a trailing ** rescues", []string{"data/**", "!data/schema.sql"}, "data/schema.sql", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Matcher{}
			m.Add("", tc.lines)
			if got := m.Excluded(tc.path, tc.isDir); got != tc.excluded {
				t.Errorf("Excluded(%q, dir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.excluded)
			}
		})
	}
}

// A `.gitignore` in a subdirectory speaks about that subtree and nothing else.
func TestNestedIgnoreFileIsScopedToItsDirectory(t *testing.T) {
	m := &Matcher{}
	m.Add("", []string{"*.tmp"})
	m.Add("web", []string{"dist/"})

	if !m.Excluded("web/dist", true) {
		t.Error("web/.gitignore's dist/ should exclude web/dist")
	}
	if m.Excluded("api/dist", true) {
		t.Error("web/.gitignore's dist/ must not reach into api/")
	}
	if !m.Excluded("api/scratch.tmp", false) {
		t.Error("the root .gitignore reaches everywhere")
	}
}

// tree writes a directory layout from a map of path → contents. A path ending
// in "/" is an empty directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(abs, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// entries lists what actually made it into the archive.
func entries(t *testing.T, r *Result) []string {
	t.Helper()
	zr, err := zip.OpenReader(r.Path)
	if err != nil {
		t.Fatalf("opening the bundle: %v", err)
	}
	defer zr.Close()
	var out []string
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

func TestBundleHonoursGitignoreAndTheAlwaysList(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":                 "*.log\nsecret.txt\n",
		"app.js":                     "console.log('hi')",
		"package.json":               "{}",
		"app.log":                    "noise",
		"secret.txt":                 "shh",
		".git/config":                "[core]",
		"node_modules/left-pad/i.js": "module.exports = 1",
		"dist/bundle.js":             "built",
		"src/main.py":                "print(1)",
		"src/__pycache__/main.pyc":   "\x00",
		".DS_Store":                  "\x00",
	})

	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	got := entries(t, r)
	want := []string{".gitignore", "app.js", "package.json", "src/main.py"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bundled %v, want %v", got, want)
	}
}

// The condition on dist/ is not fussiness. A folder of HTML with no
// package.json is a static app, and its dist/ is *the deliverable* — nothing in
// that tree can rebuild it, so excluding it would deploy an empty app and the
// build log would say nothing was wrong.
func TestDistSurvivesInATreeWithNoPackageJSON(t *testing.T) {
	root := tree(t, map[string]string{
		"dist/index.html": "<h1>hi</h1>",
		"README.md":       "docs",
	})
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	found := false
	for _, name := range entries(t, r) {
		if name == "dist/index.html" {
			found = true
		}
	}
	if !found {
		t.Error("a static site's dist/ is the deliverable and must ship")
	}
}

// And when the tree *does* have a package.json, a declared static root inside
// dist/ still wins — the always-list yields to it, or an app that declares its
// build output as its root deploys nothing.
func TestADeclaredStaticRootBeatsTheAlwaysList(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json":    "{}",
		"dist/index.html": "<h1>hi</h1>",
		"dist/app.js":     "built",
		"src/app.js":      "source",
	})
	r, err := Build(root, Options{KeepPaths: []string{"dist"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	got := strings.Join(entries(t, r), ",")
	if !strings.Contains(got, "dist/index.html") {
		t.Errorf("the declared static root was excluded: %s", got)
	}
}

// And every directory *under* the declared root ships, whatever it is called:
// a page at dist/docs/build/ is the deliverable, not build output to leave out
// (code review 2026-11-11, L-2).
func TestEverythingUnderADeclaredStaticRootShips(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json":               "{}",
		"dist/index.html":            "<h1>hi</h1>",
		"dist/docs/build/index.html": "<h1>the build page</h1>",
		"dist/out/index.html":        "<h1>the out page</h1>",
		"dist/.git/HEAD":             "ref",
		"build/index.html":           "real build output",
	})
	r, err := Build(root, Options{KeepPaths: []string{"dist"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)
	got := strings.Join(entries(t, r), ",")
	for _, want := range []string{"dist/docs/build/index.html", "dist/out/index.html"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s under the declared root was dropped: %s", want, got)
		}
	}
	if strings.Contains(got, "dist/.git/") {
		t.Errorf("version control under the root shipped: %s", got)
	}
	if strings.Contains(got, "build/index.html") && !strings.Contains(got, "dist/docs/build/index.html") {
		t.Errorf("the root-level build output shipped: %s", got)
	}
}

// A root one level down does not open its parent: the walk enters frontend/
// to reach frontend/dist, but frontend/ itself is walked under the normal
// rules, so its node_modules and its git-ignored files stay home (security
// review 2026-11-14, CS-3).
func TestANestedStaticRootKeepsOnlyItself(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json":                     "{}",
		".gitignore":                       "secrets.json\n",
		"frontend/dist/index.html":         "<h1>hi</h1>",
		"frontend/dist/build/page.html":    "<h1>page</h1>",
		"frontend/node_modules/x/index.js": "dep",
		"frontend/secrets.json":            "{\"key\":1}",
		"frontend/src/app.js":              "source",
	})
	r, err := Build(root, Options{KeepPaths: []string{"frontend/dist"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)
	got := strings.Join(entries(t, r), ",")
	for _, want := range []string{"frontend/dist/index.html", "frontend/dist/build/page.html", "frontend/src/app.js"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s should ship: %s", want, got)
		}
	}
	for _, not := range []string{"frontend/node_modules/", "frontend/secrets.json"} {
		if strings.Contains(got, not) {
			t.Errorf("%s shipped under the root's parent: %s", not, got)
		}
	}
}

// An ancestor that is itself ignored is entered only to reach the root: with
// `build/` ignored and the root build/site, build/other/ stays home.
func TestANestedStaticRootInsideAnIgnoredDirectory(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":            "build/\n",
		"build/site/index.html": "<h1>hi</h1>",
		"build/other/x.txt":     "not the site",
		"build/top.txt":         "not the site either",
	})
	r, err := Build(root, Options{KeepPaths: []string{"build/site"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)
	got := strings.Join(entries(t, r), ",")
	if got != ".gitignore,build/site/index.html" {
		t.Errorf("only the root should ship, got %s", got)
	}
}

// `.toftignore` is additional, never a replacement: it exists for the file
// that belongs in the repository and not in a deploy.
func TestToftIgnoreAddsToGitignore(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":       "*.log\n",
		".toftignore":      "fixtures/\n",
		"app.js":           "x",
		"app.log":          "x",
		"fixtures/big.csv": "x",
	})
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	got := strings.Join(entries(t, r), ",")
	if strings.Contains(got, "fixtures/") {
		t.Errorf(".toftignore was not applied: %s", got)
	}
	if strings.Contains(got, "app.log") {
		t.Errorf(".toftignore replaced .gitignore instead of adding to it: %s", got)
	}
	if !strings.Contains(got, "app.js") {
		t.Errorf("the source was dropped: %s", got)
	}
}

// A warning, not a refusal: a key in the tree is sometimes the app's own. But
// "I did not know that shipped" is a bad thing to learn later.
func TestSecretsAreNamedButNotRefused(t *testing.T) {
	root := tree(t, map[string]string{
		"server.key": "-----BEGIN",
		"app.js":     "x",
	})
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	if len(r.Secrets) != 1 || r.Secrets[0] != "server.key" {
		t.Errorf("named %v as secret-ish, want server.key", r.Secrets)
	}
	if !strings.Contains(strings.Join(entries(t, r), ","), "server.key") {
		t.Error("the warning must not become a refusal — server.key still ships")
	}
}

// A dotenv file never ships — not even negated in .toftignore — and the two
// templates do, because they are documentation rather than configuration.
func TestDotenvFilesAreNeverBundled(t *testing.T) {
	root := tree(t, map[string]string{
		".env":         "TOKEN=1",
		".env.prod":    "TOKEN=2",
		"sub/.env":     "TOKEN=3",
		".env.example": "TOKEN=",
		".env.sample":  "TOKEN=",
		".toftignore":  "!.env\n",
		"app.js":       "x",
	})
	r, err := Build(root, Options{KeepPaths: []string{".env"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	got := "," + strings.Join(entries(t, r), ",") + ","
	for _, gone := range []string{",.env,", ",.env.prod,", ",sub/.env,"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s was bundled: %s", strings.Trim(gone, ","), got)
		}
	}
	for _, kept := range []string{",.env.example,", ",.env.sample,"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s was dropped: %s", strings.Trim(kept, ","), got)
		}
	}
	if strings.Join(r.EnvFiles, ",") != ".env,.env.prod,sub/.env" {
		t.Errorf("EnvFiles = %v", r.EnvFiles)
	}
	if len(r.Secrets) != 0 {
		t.Errorf("a left-out file was also named secret-ish: %v", r.Secrets)
	}
}

// The five largest paths answer "why is my bundle 300MB", which is nearly
// always one directory nobody meant to include.
func TestFilesAreOrderedLargestFirst(t *testing.T) {
	root := tree(t, map[string]string{
		"small.txt":  "x",
		"medium.txt": strings.Repeat("x", 100),
		"large.txt":  strings.Repeat("x", 10000),
	})
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	if len(r.Files) != 3 || r.Files[0].Rel != "large.txt" {
		t.Errorf("files = %v, want largest first", r.Files)
	}
	if r.Bytes != 10101 {
		t.Errorf("Bytes = %d, want the uncompressed total", r.Bytes)
	}
}

// A symlink out of the tree is a way to bundle somebody's home directory, and
// one back into it is a way to bundle the tree twice.
func TestSymlinksAreNotFollowed(t *testing.T) {
	root := tree(t, map[string]string{"app.js": "x"})
	if err := os.Symlink("/etc", filepath.Join(root, "etc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)

	if got := strings.Join(entries(t, r), ","); got != "app.js" {
		t.Errorf("bundled %q, want app.js alone", got)
	}
}

// In a git worktree or a submodule `.git` is a file. The bundle still builds,
// and the file does not ship (security review 2026-11-14, CS-2).
func TestAGitFileIsNotAnError(t *testing.T) {
	root := tree(t, map[string]string{
		".git":   "gitdir: /nonexistent/x\n",
		"app.js": "source",
	})
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build in a worktree-shaped tree: %v", err)
	}
	defer os.Remove(r.Path)
	if got := strings.Join(entries(t, r), ","); got != "app.js" {
		t.Errorf("want only app.js, got %s", got)
	}
}

// A worktree shares the main repository's info/exclude, which its gitdir's
// commondir names.
func TestAGitFileReadsTheWorktreesSharedExclude(t *testing.T) {
	main := tree(t, map[string]string{
		"info/exclude":           "local-only.txt\n",
		"worktrees/wt/commondir": "../..\n",
		"worktrees/wt/HEAD":      "ref: refs/heads/wt\n",
	})
	root := tree(t, map[string]string{
		".git":           "gitdir: " + filepath.Join(main, "worktrees", "wt") + "\n",
		"app.js":         "source",
		"local-only.txt": "not for anyone else",
	})
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)
	if got := strings.Join(entries(t, r), ","); got != "app.js" {
		t.Errorf("the shared exclude file was not honoured: %s", got)
	}
}

// A submodule's gitdir has no commondir and its own info/exclude; a relative
// gitdir is relative to the tree root.
func TestAGitFileReadsASubmodulesOwnExclude(t *testing.T) {
	root := tree(t, map[string]string{
		".git":        "gitdir: ../modgit\n",
		"app.js":      "source",
		"scratch.txt": "mine",
	})
	modgit := filepath.Join(filepath.Dir(root), "modgit")
	if err := os.MkdirAll(filepath.Join(modgit, "info"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(modgit) })
	if err := os.WriteFile(filepath.Join(modgit, "info", "exclude"), []byte("scratch.txt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := Build(root, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)
	if got := strings.Join(entries(t, r), ","); got != "app.js" {
		t.Errorf("the submodule's exclude file was not honoured: %s", got)
	}
}

// A `.git` file under a declared static root does not ship either.
func TestAGitFileUnderAStaticRootDoesNotShip(t *testing.T) {
	root := tree(t, map[string]string{
		"dist/index.html": "<h1>hi</h1>",
		"dist/.git":       "gitdir: /elsewhere\n",
	})
	r, err := Build(root, Options{KeepPaths: []string{"dist"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer os.Remove(r.Path)
	if got := strings.Join(entries(t, r), ","); got != "dist/index.html" {
		t.Errorf("want only dist/index.html, got %s", got)
	}
}

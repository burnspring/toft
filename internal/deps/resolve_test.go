package deps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"toft/internal/advisory"
	"toft/internal/toolchain"
)

// A lockfile that exercises the three things the npm reader has to get right:
// a direct dependency, a transitive one, and a *nested* copy of a package that
// also exists at the root at a different version.
const lockfile = `{
  "name": "demo",
  "lockfileVersion": 3,
  "packages": {
    "": {
      "name": "demo",
      "dependencies": {"webpack": "^5.0.0", "lodash": "^4.17.0"},
      "devDependencies": {"jest": "^29.0.0"}
    },
    "node_modules/lodash": {"version": "4.17.21"},
    "node_modules/webpack": {
      "version": "5.90.0",
      "dependencies": {"lodash": "^3.0.0", "tapable": "^2.0.0"}
    },
    "node_modules/webpack/node_modules/lodash": {"version": "3.10.1"},
    "node_modules/tapable": {"version": "2.2.1"},
    "node_modules/jest": {"version": "29.7.0", "dev": true},
    "node_modules/orphan": {"version": "1.0.0"},
    "node_modules/link-me": {"link": true, "resolved": "packages/thing"}
  }
}`

func writeRelease(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	return dir
}

func TestResolveNPMFromALockfile(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json":      `{"name":"demo"}`,
		"package-lock.json": lockfile,
	})

	deps, _, err := resolveNPM(context.Background(), dir, nil, nil)
	if err != nil {
		t.Fatalf("resolveNPM: %v", err)
	}

	got := map[string]Dependency{}
	for _, d := range deps {
		// Two lodashes at two versions, so the key has to carry both.
		got[d.Package+"@"+d.Version] = d
	}

	// A link is a workspace or file: reference, not a published package, and
	// nothing can be published about it.
	if _, ok := got["link-me@"]; ok {
		t.Error("a linked package must not be recorded")
	}
	if len(deps) != 6 {
		t.Errorf("want 6 packages, got %d: %+v", len(deps), deps)
	}

	// The nested copy keeps its own version rather than being merged with the
	// root one — which is the whole reason the tree is walked instead of the
	// root package.json being read.
	nested, ok := got["lodash@3.10.1"]
	if !ok {
		t.Fatalf("the nested lodash is missing: %+v", deps)
	}
	if nested.Path != "(this app) > webpack > lodash" {
		t.Errorf("nested lodash Path = %q", nested.Path)
	}
	if nested.Direct {
		// The root manifest names *a* lodash, but this copy is webpack's, and
		// calling it direct would tell an owner to bump a version they do not
		// control.
		t.Error("a nested duplicate must not be reported as a direct dependency")
	}

	root := got["lodash@4.17.21"]
	if root.Path != "(this app) > lodash" || !root.Direct {
		t.Errorf("root lodash = %+v", root)
	}

	if d := got["tapable@2.2.1"]; d.Direct || d.Path != "(this app) > webpack > tapable" {
		t.Errorf("tapable = %+v", d)
	}
	if d := got["jest@29.7.0"]; !d.Dev || !d.Direct {
		t.Errorf("jest should be a direct dev dependency: %+v", d)
	}
	// A package nothing reaches still gets a chain, because a blank one would
	// read as "direct".
	if d := got["orphan@1.0.0"]; d.Path == "" {
		t.Errorf("an unreachable package should say so: %+v", d)
	}
}

// Breadth-first, so the chain names the dependency closest to the app's own
// manifest — the one its owner can actually change.
func TestNPMPathPrefersTheShortestChain(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json": `{"name":"demo"}`,
		"package-lock.json": `{
		  "lockfileVersion": 3,
		  "packages": {
		    "": {"dependencies": {"a": "1", "deep": "1"}},
		    "node_modules/a": {"version": "1.0.0", "dependencies": {"target": "1"}},
		    "node_modules/deep": {"version": "1.0.0", "dependencies": {"mid": "1"}},
		    "node_modules/mid": {"version": "1.0.0", "dependencies": {"target": "1"}},
		    "node_modules/target": {"version": "1.0.0"}
		  }
		}`,
	})
	deps, _, err := resolveNPM(context.Background(), dir, nil, nil)
	if err != nil {
		t.Fatalf("resolveNPM: %v", err)
	}
	for _, d := range deps {
		if d.Package == "target" && d.Path != "(this app) > a > target" {
			t.Errorf("target should take the shortest chain, got %q", d.Path)
		}
	}
}

func TestResolveNPMRefusesAVersionOneLockfile(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json":      `{"name":"demo"}`,
		"package-lock.json": `{"lockfileVersion": 1, "dependencies": {"lodash": {"version": "4.17.15"}}}`,
	})
	_, _, err := resolveNPM(context.Background(), dir, nil, nil)
	if err == nil {
		t.Fatal("expected a refusal naming the lockfile version")
	}
	// A refusal a person can act on says what to do about it.
	if want := "npm 7"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to name %q", err, want)
	}
}

// A compiled file puts each --hash on a continuation line. Reading the hash as
// a directive made every one of them "not pinned", which threw away the
// direct-only fallback for exactly the files that pin most carefully.
func TestReadPinnedRequirementsJoinsHashedContinuations(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"requirements.txt": `# autogenerated
certifi==2024.8.30 \
    --hash=sha256:aaaa \
    --hash=sha256:bbbb
    # via requests
requests==2.32.3 \
    --hash=sha256:cccc
    # via -r requirements.in
`,
	})
	deps, pinned, err := readPinnedRequirements(filepath.Join(dir, "requirements.txt"))
	if err != nil {
		t.Fatalf("readPinnedRequirements: %v", err)
	}
	if !pinned {
		t.Error("every requirement is pinned; the hashes are decoration")
	}
	if len(deps) != 2 || deps[0].Package != "certifi" || deps[1].Version != "2.32.3" {
		t.Errorf("deps = %+v", deps)
	}
}

func TestReadPinnedRequirements(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"requirements.txt": `
# a comment
Flask==3.0.0
Flask_SQLAlchemy==3.1.1
requests==2.31.0  # trailing comment
uvicorn[standard]==0.27.0
pytest==8.0.0 ; python_version >= "3.9"
`,
	})
	deps, pinned, err := readPinnedRequirements(filepath.Join(dir, "requirements.txt"))
	if err != nil {
		t.Fatalf("readPinnedRequirements: %v", err)
	}
	if !pinned {
		t.Error("every line is pinned, so the file is the answer")
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Package] = d.Version
	}
	want := map[string]string{
		"flask": "3.0.0",
		// PEP 503 normalization at read time, so an advisory filed under one
		// spelling matches an app that pinned another.
		"flask-sqlalchemy": "3.1.1",
		"requests":         "2.31.0",
		// Extras decorate a requirement without changing which package it is.
		"uvicorn": "0.27.0",
		// An environment marker likewise.
		"pytest": "8.0.0",
	}
	for name, version := range want {
		if got[name] != version {
			t.Errorf("%s = %q, want %q (all: %+v)", name, got[name], version, got)
		}
	}
	if len(deps) != len(want) {
		t.Errorf("got %d requirements, want %d: %+v", len(deps), len(want), deps)
	}
}

// An unpinned line means the file is a request rather than an answer, and the
// answer has to be resolved — reading it as complete would record a version
// the build is not going to install.
func TestUnpinnedRequirementsAreNotAnAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"a range":       "flask>=3.0.0\n",
		"a bare name":   "flask\n",
		"a URL":         "flask @ https://example.com/flask.whl\n",
		"an -r include": "-r other.txt\nflask==3.0.0\n",
	} {
		dir := writeRelease(t, map[string]string{"requirements.txt": body})
		_, pinned, err := readPinnedRequirements(filepath.Join(dir, "requirements.txt"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pinned {
			t.Errorf("%s should not read as fully pinned", name)
		}
	}
}

func TestReadUVLock(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"uv.lock": `
version = 1

[[package]]
name = "demo"
version = "0.1.0"
dependencies = [{ name = "flask" }]

[[package]]
name = "flask"
version = "3.0.0"
dependencies = [{ name = "werkzeug" }, { name = "Jinja2" }]

[[package]]
name = "werkzeug"
version = "3.0.1"

[[package]]
name = "jinja2"
version = "3.1.3"
`,
	})
	deps, err := readUVLock(filepath.Join(dir, "uv.lock"))
	if err != nil {
		t.Fatalf("readUVLock: %v", err)
	}
	got := map[string]Dependency{}
	for _, d := range deps {
		got[d.Package] = d
	}
	if len(deps) != 4 {
		t.Errorf("want 4 packages, got %d: %+v", len(deps), deps)
	}
	if d := got["flask"]; d.Version != "3.0.0" || d.Path != "(this app) > demo > flask" {
		t.Errorf("flask = %+v", d)
	}
	// The dependency was written "Jinja2" and the package "jinja2"; PEP 503
	// folding at read time is what joins them into one chain.
	if d := got["jinja2"]; d.Version != "3.1.3" || d.Path != "(this app) > demo > flask > jinja2" {
		t.Errorf("jinja2 = %+v", d)
	}
	if d := got["demo"]; !d.Direct {
		t.Errorf("the project itself is a root: %+v", d)
	}
}

// A committed dependency tree is not resolved, and the warning is the whole of
// the mitigation — so it has to be produced where somebody will read it.
func TestResolveWarnsAboutACommittedTree(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json":             `{"name":"demo"}`,
		"package-lock.json":        `{"lockfileVersion":3,"packages":{"":{}}}`,
		"node_modules/lodash/x.js": `//`,
	})
	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var warned bool
	for _, n := range res.Notes {
		if strings.Contains(n, "node_modules") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("Notes should warn about the committed tree: %+v", res.Notes)
	}
}

// An app with two manifests has two answers, and both ecosystems have to be
// recorded — the fix, the lockfile and often the person are different for each.
func TestResolveReadsEveryEcosystemPresent(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json":      `{"name":"demo"}`,
		"package-lock.json": `{"lockfileVersion":3,"packages":{"":{},"node_modules/lodash":{"version":"4.17.21"}}}`,
		"requirements.txt":  "flask==3.0.0\n",
	})
	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Ecosystems) != 2 {
		t.Errorf("Ecosystems = %v, want npm and pypi", res.Ecosystems)
	}
	var npm, pypi bool
	for _, d := range res.Dependencies {
		switch d.Ecosystem {
		case advisory.EcosystemNPM:
			npm = true
		case advisory.EcosystemPyPI:
			pypi = true
		}
	}
	if !npm || !pypi {
		t.Errorf("both ecosystems should be present: %+v", res.Dependencies)
	}
}

// An app that locks with pnpm or Yarn has pinned its versions and is about to
// be installed at different ones, because every Node path keys on
// package-lock.json. The note has to name the file that was passed over —
// "no package-lock.json" beside a lockfile is the half that misleads.
func TestUnlockedNPMNamesAForeignLockfile(t *testing.T) {
	for _, tc := range []struct{ file, tool string }{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "Yarn"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			dir := writeRelease(t, map[string]string{
				"package.json": `{"name":"demo"}`,
				tc.file:        "# pinned\n",
			})
			line, note := unlockedNPM(dir)
			if !strings.Contains(line, tc.file) {
				t.Errorf("build log line should name the lockfile: %s", line)
			}
			// The note carries the actionable half, because the terse line
			// above cannot: which file was passed over, and which one would
			// have been read instead.
			for _, want := range []string{tc.file, tc.tool, "package-lock.json"} {
				if !strings.Contains(note, want) {
					t.Errorf("note should mention %q: %s", want, note)
				}
			}
		})
	}
}

// An app with no lockfile at all has never pinned anything, and keeps the
// plainer wording: there is no file to name.
func TestUnlockedNPMWithNoLockfileAtAll(t *testing.T) {
	dir := writeRelease(t, map[string]string{"package.json": `{"name":"demo"}`})
	line, note := unlockedNPM(dir)
	if !strings.Contains(line, "No package-lock.json") {
		t.Errorf("unexpected build log line: %s", line)
	}
	if !strings.Contains(note, "no package-lock.json") || strings.Contains(note, "pnpm") {
		t.Errorf("unexpected note: %s", note)
	}
}

// The gate judges what the build installs. uv.lock is ignored by `uv pip
// install`, so with a requirements.txt or pyproject.toml beside it the lock is
// not the answer; and a requirements.txt that pins only its top level is not a
// closed set. With no resolver available, neither is silently accepted.
func TestThePythonGateDoesNotJudgeWhatTheInstallerIgnores(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no uv: the resolver cannot run
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lock := "version = 1\n\n[[package]]\nname = \"pkgx\"\nversion = \"1.0.0\"\n"

	dir := t.TempDir()
	write(dir, "uv.lock", lock)
	write(dir, "pyproject.toml", "[project]\nname = \"demo\"\ndependencies = [\"pkgx>=1.0\"]\n")
	deps, notes, err := resolvePyPI(context.Background(), dir, "", nil, func(string, ...any) {}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 || len(notes) == 0 {
		t.Errorf("uv.lock beside pyproject.toml was judged as the install set: deps=%v notes=%v", deps, notes)
	}

	dir = t.TempDir()
	write(dir, "requirements.txt", "flask==3.0.0\n")
	_, notes, err = resolvePyPI(context.Background(), dir, "", nil, func(string, ...any) {}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 {
		t.Error("a top-level-pinned requirements.txt was accepted as complete without a word")
	}

	dir = t.TempDir()
	write(dir, "uv.lock", lock)
	deps, _, err = resolvePyPI(context.Background(), dir, "", nil, func(string, ...any) {}, nil, nil)
	if err != nil || len(deps) != 1 {
		t.Errorf("a lock with nothing else to install from is the answer: deps=%v err=%v", deps, err)
	}
}

// An ecosystem whose resolver gave up is not listed as resolved. A good npm
// tree beside a go.mod whose build list cannot be read: Ecosystems is npm
// alone, and the gap says Go was not read — before this, the verdict
// recorded go as resolved with no packages, which reads as "checked, clean".
func TestAnUnresolvedEcosystemIsNotResolved(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json":      `{"name":"demo"}`,
		"package-lock.json": lockfile,
		"go.mod":            "this is not a go.mod\n",
		"node_modules/x/a":  "",
	})
	res, err := Resolve(context.Background(), dir, nil)
	// A go.mod go cannot parse is the app's own file unread (G-4), not a gap
	// alone: the error names it, and the npm inventory still travels.
	var unread *UnreadError
	if err != nil && (!errors.As(err, &unread) || unread.Files()[0] != "go.mod") {
		t.Fatal(err)
	}
	if len(res.Ecosystems) != 1 || res.Ecosystems[0] != advisory.EcosystemNPM {
		t.Errorf("Ecosystems = %v, want npm alone", res.Ecosystems)
	}
	want := map[string]bool{GapUnresolved + "/" + advisory.EcosystemGo: false, GapVendored + "/": false}
	for _, g := range res.Gaps {
		want[g.Kind+"/"+g.Ecosystem] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("gap %s missing from %+v", k, res.Gaps)
		}
	}
}

// A release with no package-lock.json and a pnpm lock records which lock was
// passed over.
func TestAForeignLockIsAGap(t *testing.T) {
	var gaps []Gap
	dir := writeRelease(t, map[string]string{"package.json": `{"name":"demo","dependencies":{"ms":"2"}}`, "pnpm-lock.yaml": "lockfileVersion: 9\n"})
	_, _, _ = resolveNPM(context.Background(), dir, func(string, ...any) {}, &gaps)
	if len(gaps) == 0 || gaps[0].Kind != GapForeignLock || gaps[0].File != "pnpm-lock.yaml" {
		t.Errorf("gaps = %+v, want foreign_lock naming pnpm-lock.yaml", gaps)
	}
}

// A resolved release's verdict always carries the unread key, so its absence
// can mean "built before this was recorded" and nothing else.
func TestAVerdictAlwaysWritesItsUnreadKey(t *testing.T) {
	v := AllowedVerdict("warn", true, 3, []string{"npm"}, nil)
	v.Unread = UnreadOf(nil)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"unread":[]`) {
		t.Errorf("verdict = %s, want an empty unread list", b)
	}
}

// An empty requirements.txt resolves to nothing, and nothing is not an
// unlocked dependency set: no gap, so no deploy warning that versions a later
// build resolves "can differ" (the e2e fixture fx-py, on an install with a
// dependency policy).
func TestAnEmptyRequirementsFileIsNotUnlocked(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not on PATH")
	}
	dir := writeRelease(t, map[string]string{"requirements.txt": ""})
	var gaps []Gap
	deps, _, err := resolvePyPI(context.Background(), dir, hostPython(t), nil, func(string, ...any) {}, &gaps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 || len(gaps) != 0 {
		t.Errorf("deps=%v gaps=%v, want neither", deps, gaps)
	}
}

// A manifest the build installs from and this platform cannot read is not a
// way past `block`: a version 1 package-lock.json (which `npm ci` installs
// from) or a junk renv.lock beside a Python app comes back as an UnreadError
// that refuses under an enforcing policy — and never takes the other
// ecosystems' inventory down with it.
func TestAnUnreadableManifestRefusesUnderBlock(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		file  string
		kept  string // an ecosystem whose packages must survive
	}{
		"version 1 lock": {
			files: map[string]string{
				"package.json":      `{"name":"demo"}`,
				"package-lock.json": `{"lockfileVersion": 1, "dependencies": {"evil": {"version": "1.0.0"}}}`,
				"requirements.txt":  "flask==3.0.0\n",
			},
			file: "package-lock.json", kept: advisory.EcosystemPyPI,
		},
		"junk renv.lock": {
			files: map[string]string{
				"renv.lock":        "x",
				"requirements.txt": "flask==3.0.0\n",
			},
			file: "renv.lock", kept: advisory.EcosystemPyPI,
		},
		// LB-1: a value Postgres refuses lost the whole inventory.
		"a NUL in a renv.lock version": {
			files: map[string]string{
				"renv.lock":        `{"Packages":{"shiny":{"Package":"shiny","Version":"1.9.1\u0000"}}}`,
				"requirements.txt": "flask==3.0.0\n",
			},
			file: "renv.lock", kept: advisory.EcosystemPyPI,
		},
		"an over-long lockfile version": {
			files: map[string]string{
				"package.json": `{"name":"demo","dependencies":{"x":"1"}}`,
				"package-lock.json": `{"lockfileVersion": 3, "packages": {"": {"dependencies": {"x": "1"}},
				  "node_modules/x": {"version": "` + strings.Repeat("1", 300) + `"}}}`,
				"requirements.txt": "flask==3.0.0\n",
			},
			file: "package-lock.json", kept: advisory.EcosystemPyPI,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeRelease(t, tc.files)
			res, err := Resolve(context.Background(), dir, nil)
			for _, d := range res.Dependencies {
				if !Recordable(d.Package) || !Recordable(d.Version) {
					t.Errorf("an unrecordable row was kept: %+v", d)
				}
			}
			var unread *UnreadError
			if !errors.As(err, &unread) {
				t.Fatalf("err = %v, want an UnreadError", err)
			}
			if files := unread.Files(); len(files) != 1 || files[0] != tc.file {
				t.Errorf("Files() = %v, want [%s]", files, tc.file)
			}
			kept := false
			for _, d := range res.Dependencies {
				kept = kept || d.Ecosystem == tc.kept
			}
			if !kept {
				t.Errorf("the %s inventory was thrown away: %+v", tc.kept, res.Dependencies)
			}
			found := false
			for _, g := range res.Gaps {
				found = found || (g.Kind == GapUnresolved && g.File == tc.file)
			}
			if !found {
				t.Errorf("Gaps = %+v, want an unresolved gap naming %s", res.Gaps, tc.file)
			}

			msg := UnreadRefusal(base(), unread, true)
			if !strings.HasPrefix(msg, "REFUSED:") || !strings.Contains(msg, tc.file) {
				t.Errorf("block refusal = %q, want it to refuse naming %s", msg, tc.file)
			}
			warn := base()
			warn.Mode = "warn"
			if msg := UnreadRefusal(warn, unread, true); msg != "" {
				t.Errorf("warn refused: %q", msg)
			}
		})
	}
}

// A requirements.txt that pins and hashes every package is read as the lock:
// no resolver runs (there is no uv on PATH here), no gap is recorded, and the
// resolution says which reader produced it.
func TestAHashedRequirementsFileIsTheLock(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := writeRelease(t, map[string]string{
		"requirements.txt": `flask==3.1.0 \
    --hash=sha256:aaaa
    # via -r requirements.in
markupsafe==3.0.4 \
    --hash=sha256:bbbb
    # via
    #   jinja2
    #   werkzeug
werkzeug==3.1.9 \
    --hash=sha256:cccc
    # via
    #   -r requirements.in
    #   flask
`,
	})
	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HashedRequirements {
		t.Error("the resolution does not say it read a hashed requirements.txt")
	}
	if len(res.Gaps) != 0 {
		t.Errorf("gaps = %+v, want none: the file is the whole answer", res.Gaps)
	}
	if len(res.Ecosystems) != 1 || res.Ecosystems[0] != advisory.EcosystemPyPI {
		t.Errorf("Ecosystems = %v, want PyPI", res.Ecosystems)
	}
	direct := map[string]bool{}
	for _, d := range res.Dependencies {
		direct[d.Package] = d.Direct
		if SourceOf(d) != SourceResolved {
			t.Errorf("%s source = %q, want resolved", d.Package, SourceOf(d))
		}
	}
	want := map[string]bool{"flask": true, "markupsafe": false, "werkzeug": true}
	if !reflect.DeepEqual(direct, want) {
		t.Errorf("direct = %v, want %v", direct, want)
	}
}

// Partly hashed is not locked: the build installs it without the flag, so the
// scan must not read it as the closed set. With no resolver, the pinned
// fallback says only the named packages were checked.
func TestAPartlyHashedRequirementsFileIsNotTheLock(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := writeRelease(t, map[string]string{
		"requirements.txt": "flask==3.1.0 --hash=sha256:aaaa\nwerkzeug==3.1.9\n",
	})
	res, _ := Resolve(context.Background(), dir, nil)
	if res.HashedRequirements {
		t.Error("a partly hashed file was read as the lock")
	}
	if len(res.Gaps) == 0 {
		t.Error("a partly hashed file was read without a gap")
	}
}

func TestDirectFollowsVia(t *testing.T) {
	for _, tc := range []struct {
		via    string
		direct bool
	}{
		{"-r requirements.in", true},
		{"requirements.in", true},
		{"demo (pyproject.toml)", true},
		{"flask", false},
		{"zope.interface", false},
		{"-c constraints.txt", false},
	} {
		r := toolchain.ParseRequirements("pkg==1.0 --hash=sha256:aaaa\n    # via " + tc.via + "\n")
		rows := hashedRows(r)
		if len(rows) != 1 || rows[0].Direct != tc.direct {
			t.Errorf("via %q: rows = %+v, want direct %v", tc.via, rows, tc.direct)
		}
	}
	// No annotations at all: a hand-written file lists what its author chose.
	rows := hashedRows(toolchain.ParseRequirements("a==1.0 --hash=sha256:aa\nb==2.0 --hash=sha256:bb\n"))
	if len(rows) != 2 || !rows[0].Direct || !rows[1].Direct {
		t.Errorf("rows = %+v, want both direct", rows)
	}
}

// BB-029 (second 2026-11-03 bash): a package.json with no dependencies is
// not an unlocked set — no lockfile gap, nothing resolved.
func TestAPackageJSONWithNoDependenciesHasNoLockfileGap(t *testing.T) {
	dir := writeRelease(t, map[string]string{"package.json": `{"name":"x","scripts":{"start":"node a.js"},"dependencies":{}}`})
	var gaps []Gap
	deps, _, err := resolveNPM(context.Background(), dir, func(string, ...any) {}, &gaps)
	if err != nil || len(deps) != 0 || len(gaps) != 0 {
		t.Fatalf("deps %v gaps %v err %v", deps, gaps, err)
	}
	if !declaresNPMDependencies(writeRelease(t, map[string]string{"package.json": `{"devDependencies":{"ms":"2"}}`})) {
		t.Error("a devDependency is a dependency")
	}
}

func TestMergeDuplicatesFoldsOneKeyIntoOneRow(t *testing.T) {
	got := MergeDuplicates([]Dependency{
		{Ecosystem: "npm", Package: "ms", Version: "2.0.0", Dev: true, Path: "app > a > debug > ms"},
		{Ecosystem: "npm", Package: "lodash", Version: "4.17.21"},
		{Ecosystem: "npm", Package: "ms", Version: "2.0.0", Direct: true, Path: "app > ms"},
		{Ecosystem: "pypi", Package: "ms", Version: "2.0.0"},
	})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	if m := got[0]; m.Package != "ms" || !m.Direct || m.Dev || m.Path != "app > ms" {
		t.Errorf("merged = %+v", m)
	}
	if got[1].Package != "lodash" || got[2].Ecosystem != "pypi" {
		t.Errorf("order not kept: %+v", got)
	}
}

// npm prefers npm-shrinkwrap.json over package-lock.json, so the scan does too
// (code review 2026-11-11, E-3).
func TestResolveNPMPrefersTheShrinkwrap(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json":        `{"name":"demo","dependencies":{"lodash":"^4.17.0"}}`,
		"package-lock.json":   `{"lockfileVersion": 3, "packages": {"": {}, "node_modules/lodash": {"version": "4.17.21"}}}`,
		"npm-shrinkwrap.json": `{"lockfileVersion": 3, "packages": {"": {}, "node_modules/lodash": {"version": "4.17.15"}}}`,
	})
	deps, _, err := resolveNPM(context.Background(), dir, nil, nil)
	if err != nil {
		t.Fatalf("resolveNPM: %v", err)
	}
	if len(deps) != 1 || deps[0].Version != "4.17.15" {
		t.Fatalf("deps = %+v, want lodash 4.17.15 from the shrinkwrap", deps)
	}
	if got := NPMLockfile(dir); got != "npm-shrinkwrap.json" {
		t.Errorf("NPMLockfile = %q", got)
	}
}

// An aliased install — `"foo": "npm:event-stream@3.3.6"` — lands at
// node_modules/foo with the published name in the entry; the inventory records
// the package installed, not the folder (code review 2026-11-11, E-4).
func TestResolveNPMRecordsAnAliasUnderThePackageInstalled(t *testing.T) {
	dir := writeRelease(t, map[string]string{
		"package.json": `{"name":"demo","dependencies":{"harmless":"npm:event-stream@3.3.6"}}`,
		"package-lock.json": `{"lockfileVersion": 3, "packages": {
		  "": {"dependencies": {"harmless": "npm:event-stream@3.3.6"}},
		  "node_modules/harmless": {"name": "event-stream", "version": "3.3.6"}}}`,
	})
	deps, _, err := resolveNPM(context.Background(), dir, nil, nil)
	if err != nil {
		t.Fatalf("resolveNPM: %v", err)
	}
	if len(deps) != 1 || deps[0].Package != "event-stream" || deps[0].Version != "3.3.6" || !deps[0].Direct {
		t.Fatalf("deps = %+v, want event-stream 3.3.6, direct", deps)
	}
}

// A workspaces root with no dependencies of its own is not "nothing to lock":
// npm install installs every member's from here (code review 2026-11-11, G-3).
func TestAWorkspacesRootDeclaresDependencies(t *testing.T) {
	dir := writeRelease(t, map[string]string{"package.json": `{"name":"app","workspaces":["pkgs/*"]}`})
	if !declaresNPMDependencies(dir) {
		t.Error("a workspaces root was read as declaring nothing")
	}
	dir = writeRelease(t, map[string]string{"package.json": `{"name":"app","workspaces":[]}`})
	if declaresNPMDependencies(dir) {
		t.Error("an empty workspaces list was read as declaring something")
	}
}

// A resolver failure the app's own files produce is the manifest not read —
// refused under block — and only a network failure stays a soft gap (code
// review 2026-11-11, G-4).
func TestAResolverFailureOfTheAppsOwnMakingIsUnread(t *testing.T) {
	for msg, network := range map[string]bool{
		"error: Failed to build `fakepkg @ file:///x`":                                                                 false,
		"go: missing go.sum entry for go.mod file":                                                                     false,
		"error sending request for url (https://pypi.org/simple/x/): connection refused":                               true,
		"go: github.com/x/y@v1.0.0: Get https://proxy.golang.org/...: dial tcp: lookup proxy.golang.org: no such host": true,
		// LB-2: a name the app chose is never read as a network sign.
		"go: github.com/gin-contrib/timeout@v1.1.0: missing go.sum entry for go.mod file; to add it:\n\tgo mod download github.com/gin-contrib/timeout": false,
		"error: Failed to build `fakepkg @ https://h.example/proxy/fakepkg-1.0.tar.gz`":                                                                 false,
		"go: github.com/google/certificate-transparency-go@v1.1.0: missing go.sum entry for go.mod file":                                                false,
		"go: example.com/eof@v1.0.0: reading go.mod: unknown revision":                                                                                  false,
		"error: Couldn't parse requirement\ndial tcp connection refused\n^^^^":                                                                          false,
		`go: Get "https://proxy.golang.org/x/@v/list": dial tcp 1.2.3.4:443: i/o timeout`:                                                               true,
		"error: Failed to fetch: `https://pypi.org/simple/x/`\n  Caused by: operation timed out":                                                        true,
	} {
		if got := networkFailure(msg); got != network {
			t.Errorf("networkFailure(%q) = %v", msg, got)
		}
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	dir := writeRelease(t, map[string]string{"go.mod": "module demo\n\ngo 1.26\n\nrequire example.com/nothing v1.0.0 // indirect\n", "main.go": "package main\nfunc main() {}\n"})
	var gaps []Gap
	_, _, err := resolveGo(context.Background(), dir, func(string, ...any) {}, &gaps)
	var unread *UnreadError
	if !errors.As(err, &unread) || len(unread.Problems) != 1 || unread.Problems[0].File != "go.mod" {
		t.Fatalf("a go.sum with no entries: err = %v, gaps = %v; want an UnreadError naming go.mod", err, gaps)
	}
}

// fakeNPM puts an npm on PATH that records each argv and, for an install,
// whether its --cache directory existed, then writes an empty lockfile.
// flags names files that change its answers: "undefined" makes the
// allow-git probe answer as npm 10 does, "eallowgit" fails the install as a
// git dependency refused by --allow-git=none does.
func fakeNPM(t *testing.T) (calls, flags string) {
	t.Helper()
	bin := t.TempDir()
	flags = t.TempDir()
	calls = filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
echo "$@" >> ` + calls + `
if [ "$1" = config ]; then
  if [ -e ` + flags + `/undefined ]; then echo undefined; else echo all; fi
  exit 0
fi
for a in "$@"; do case "$a" in --cache=*) d="${a#--cache=}"; [ -d "$d" ] && echo "cache-existed $d" >> ` + calls + ` ;; esac; done
if [ -e ` + flags + `/eallowgit ]; then echo "npm error code EALLOWGIT" >&2; exit 1; fi
printf '{"lockfileVersion":3,"packages":{"":{}}}' > package-lock.json
`
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls, flags
}

// LB-3 and LB-U1 (security review 2026-11-15): every unlocked resolve runs
// npm with a cache of its own — never shared with another app's scan, never
// under HOME, gone afterwards — and with --allow-git=none, which outranks the
// release's .npmrc and is exported to any child npm.
func TestTheUnlockedNPMResolveHasItsOwnCacheAndNoGit(t *testing.T) {
	calls, _ := fakeNPM(t)
	t.Setenv("HOME", t.TempDir())
	for i := 0; i < 2; i++ {
		dir := writeRelease(t, map[string]string{"package.json": `{"name":"a","dependencies":{"x":"1"}}`})
		if _, _, err := resolveNPM(context.Background(), dir, func(string, ...any) {}, nil); err != nil {
			t.Fatalf("resolveNPM: %v", err)
		}
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	caches := map[string]bool{}
	existed := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if d, ok := strings.CutPrefix(l, "cache-existed "); ok {
			existed[d] = true
			continue
		}
		if !strings.HasPrefix(l, "install ") {
			continue
		}
		if !strings.Contains(l, "--allow-git=none") || !strings.Contains(l, "--git=git") {
			t.Errorf("an install without --allow-git=none and --git=git: %q", l)
		}
		for _, a := range strings.Fields(l) {
			if d, ok := strings.CutPrefix(a, "--cache="); ok {
				caches[d] = true
			}
		}
	}
	if len(caches) != 2 {
		t.Fatalf("want two installs with two different caches, got %v:\n%s", caches, raw)
	}
	home := os.Getenv("HOME")
	for d := range caches {
		if !existed[d] {
			t.Errorf("the cache %s did not exist while npm ran", d)
		}
		if home != "" && strings.HasPrefix(d, home+string(filepath.Separator)) {
			t.Errorf("the cache %s is under HOME", d)
		}
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("the cache %s outlived the resolve: %v", d, err)
		}
	}
	if env := strings.Join(resolveEnv(), "\n"); strings.Contains(env, "npm_config_cache") {
		t.Errorf("resolveEnv still carries npm_config_cache:\n%s", env)
	}
}

// A git dependency the scan refused, and an npm too old to be told not to
// fetch one, are both the app's manifest unread: never a soft gap.
func TestTheUnlockedNPMResolveRefusesAGitDependency(t *testing.T) {
	t.Run("EALLOWGIT", func(t *testing.T) {
		_, flags := fakeNPM(t)
		if err := os.WriteFile(filepath.Join(flags, "eallowgit"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		dir := writeRelease(t, map[string]string{"package.json": `{"name":"a","dependencies":{"x":"github:o/r#0123456789012345678901234567890123456789"}}`})
		_, err := Resolve(context.Background(), dir, nil)
		var unread *UnreadError
		if !errors.As(err, &unread) || unread.Files()[0] != "package.json" ||
			!strings.Contains(err.Error(), "git dependency") {
			t.Fatalf("err = %v, want an UnreadError naming package.json and the git dependency", err)
		}
	})
	t.Run("npm without allow-git", func(t *testing.T) {
		calls, flags := fakeNPM(t)
		if err := os.WriteFile(filepath.Join(flags, "undefined"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		dir := writeRelease(t, map[string]string{"package.json": `{"name":"a","dependencies":{"x":"1"}}`})
		_, err := Resolve(context.Background(), dir, nil)
		var unread *UnreadError
		if !errors.As(err, &unread) {
			t.Fatalf("err = %v, want an UnreadError", err)
		}
		raw, _ := os.ReadFile(calls)
		if strings.Contains(string(raw), "install") {
			t.Errorf("an npm that cannot refuse git was asked to install:\n%s", raw)
		}
	})
}

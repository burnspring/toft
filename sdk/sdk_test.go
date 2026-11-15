package sdk

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// The payload must be the shipped surface and nothing else. `__pycache__` is
// gitignored, so it exists in a developer's checkout and not in a clean one —
// embedding the directory would make what lands in every app's environment
// depend on whose machine built the binary.
func TestThePythonPayloadIsSourceAndNothingElse(t *testing.T) {
	dir, err := Materialize(t.TempDir(), "python")
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			got = append(got, strings.TrimPrefix(p, dir+string(os.PathSeparator)))
		}
		return nil
	})

	for _, want := range []string{
		filepath.Join("toft", "__init__.py"), // the one a `_` rule would drop
		filepath.Join("toft", "jobs.py"),
		filepath.Join("toft", "notifier.py"),
		"pyproject.toml", // uv pip install needs the manifest beside the package
	} {
		if !contains(got, want) {
			t.Errorf("%s is missing — got %v", want, got)
		}
	}
	for _, name := range got {
		if strings.Contains(name, "__pycache__") || strings.HasSuffix(name, ".pyc") {
			t.Errorf("compiled bytecode was embedded: %s", name)
		}
		if strings.Contains(name, "tests"+string(os.PathSeparator)) {
			t.Errorf("the test suite was embedded: %s", name)
		}
	}
}

// A directory copy, not `npm install <dir>`: npm records a local path as a
// `file:` dependency and symlinks it, which would point at a directory that is
// gone by the time the app runs.
func TestNodeIsInstalledAsRealFiles(t *testing.T) {
	release := t.TempDir()
	installed, err := EnsureNode(release)
	if err != nil || !installed {
		t.Fatalf("EnsureNode = %v, %v — want it installed", installed, err)
	}

	dest := filepath.Join(release, "node_modules", "toft-sdk")
	fi, err := os.Lstat(filepath.Join(dest, "index.js"))
	if err != nil {
		t.Fatalf("index.js: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("the SDK was linked rather than copied; it would dangle at run time")
	}
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err != nil {
		t.Errorf("package.json: %v", err)
	}
}

// The platform's copy is a floor, not a ceiling: an app that brought its own
// keeps it. For Python that falls out of install order; Node needs the check,
// because `npm ci` empties node_modules and runs first.
func TestAnAppsOwnNodeCopyIsKept(t *testing.T) {
	release := t.TempDir()
	dest := filepath.Join(release, "node_modules", "toft-sdk")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dest, "package.json"), []byte(`{"version":"9.9.9"}`), 0o644)

	installed, err := EnsureNode(release)
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Error("the app's own copy was overwritten")
	}
	body, _ := os.ReadFile(filepath.Join(dest, "package.json"))
	if !strings.Contains(string(body), "9.9.9") {
		t.Errorf("package.json = %s, want the app's own", body)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// Every file the package manifest says it ships must actually be embedded.
//
// Written after `notify.js` was added to the SDK, imported by `index.js`, and
// left out of a hand-written embed list — which would have failed at each app's
// first `import`, in production, rather than here.
func TestTheNodePayloadMatchesWhatThePackageShips(t *testing.T) {
	dir, err := Materialize(t.TempDir(), "node")
	if err != nil {
		t.Fatal(err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Files []string `json:"files"`
		Main  string   `json:"main"`
	}
	if err := json.Unmarshal(manifest, &pkg); err != nil {
		t.Fatal(err)
	}

	for _, name := range append(pkg.Files, pkg.Main) {
		// README.md is documentation, not code an app imports.
		if name == "" || strings.HasSuffix(name, ".md") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("package.json ships %s but it is not embedded — every app's import would fail", name)
		}
	}

	// And the test suite is still not along for the ride.
	if _, err := os.Stat(filepath.Join(dir, "test")); err == nil {
		t.Error("the node test directory was embedded")
	}
}

// Whatever index.js imports at load time has to be there, for the same reason.
func TestEveryNodeModuleIndexImportsIsEmbedded(t *testing.T) {
	dir, err := Materialize(t.TempDir(), "node")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`from "\./([^"]+)"`).FindAllStringSubmatch(string(body), -1) {
		if _, err := os.Stat(filepath.Join(dir, m[1])); err != nil {
			t.Errorf("index.js imports ./%s, which is not embedded", m[1])
		}
	}
}

// The Go payload is the shipped surface and nothing else. Go keeps a package's
// tests beside its source, inside the same glob, so the one language that cannot
// separate them by directory is the one where this can regress.
func TestTheGoPayloadIsSourceWithoutItsTests(t *testing.T) {
	dir, err := Materialize(t.TempDir(), "go")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if strings.HasSuffix(entry.Name(), "_test.go") {
			t.Errorf("the test suite was embedded: %s", entry.Name())
		}
	}
	for _, want := range []string{"go.mod", "doc.go", "toft.go", "jobs.go", "notify.go", "viewer.go"} {
		if !contains(names, want) {
			t.Errorf("%s is missing — got %v", want, names)
		}
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(manifest); !strings.Contains(got, "module "+GoModule) || !strings.Contains(got, "go "+GoLanguageFloor) {
		t.Errorf("go.mod = %q, want the module path an app imports and the declared floor", got)
	}
	// No require block: the SDK has no third-party dependencies in any language,
	// and in Go that promise is readable straight off the manifest.
	if strings.Contains(string(manifest), "require") {
		t.Errorf("go.mod = %q, want no dependencies at all", manifest)
	}
}

// The floor is a claim about which toolchains can compile this, and the platform
// module's own go directive is far above it — so nothing else in this repository
// would notice the SDK using a language feature an app on the floor cannot build.
//
// It is also the whole mechanism end to end: a module that imports the SDK by the
// path an app would write, built through the workspace PrepareGo writes.
func TestTheGoModuleBuildsAtItsDeclaredFloorThroughTheWorkspace(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	release := t.TempDir()
	write(t, release, "go.mod", "module example.com/app\n\ngo "+GoLanguageFloor+"\n")
	write(t, release, "main.go", `package main

import (
	"context"
	"fmt"

	"`+GoModule+`"
)

func main() {
	info, err := toft.App.Self(context.Background())
	fmt.Println(info, err, toft.Version)
}
`)

	prep, err := PrepareGo(release)
	if err != nil {
		t.Fatal(err)
	}
	if !prep.Installed {
		t.Fatalf("PrepareGo skipped a plain module: %s", prep.Skipped)
	}
	defer prep.Done()

	cmd := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "app"), ".")
	cmd.Dir = release
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building an app against the SDK at go %s failed: %v\n%s", GoLanguageFloor, err, out)
	}
}

// Nothing the platform wrote survives the build. A go.work left behind is
// captured by the release artifact and then names a temporary directory on
// another machine, so the pod's own rebuild fails on a path that never existed
// there.
func TestPreparingGoLeavesTheReleaseAsItFoundIt(t *testing.T) {
	release := t.TempDir()
	write(t, release, "go.mod", "module example.com/app\n\ngo 1.24.0\n")
	write(t, release, "main.go", "package main\n\nfunc main() {}\n")
	before := listing(t, release)

	prep, err := PrepareGo(release)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.ReadFile(filepath.Join(release, GoWorkspaceFile))
	if err != nil {
		t.Fatalf("no workspace was written: %v", err)
	}
	if !strings.Contains(string(workspace), "go 1.24.0") {
		t.Errorf("go.work = %q, want the app's own directive — a workspace may not "+
			"declare less than a module it uses", workspace)
	}
	if !strings.Contains(string(workspace), prep.Dir) {
		t.Errorf("go.work = %q, want it to name the materialized module", workspace)
	}
	if strings.HasPrefix(prep.Dir, release) {
		t.Errorf("the SDK was materialized at %s, inside the release the artifact captures", prep.Dir)
	}

	prep.Done()
	if after := listing(t, release); after != before {
		t.Errorf("the release is %v, want %v", after, before)
	}
	if _, err := os.Stat(prep.Dir); err == nil {
		t.Error("the materialized module outlived the build")
	}
}

// The platform's copy is a floor, not a ceiling: an app that has made its own
// arrangement keeps it, and the build log says which one it was.
func TestAnAppsOwnArrangementIsKept(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		says  string
	}{
		{"vendored", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "vendor"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "vendor"},
		{"its own workspace", func(t *testing.T, dir string) {
			write(t, dir, GoWorkspaceFile, "go 1.24.0\n\nuse .\n")
		}, "go.work"},
		{"not a module at all", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "go.mod"))
		}, "go.mod"},
		// LB-5: a FIFO named go.mod blocked a repair under the cache lock.
		{"a go.mod that is a FIFO", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "go.mod"))
			if err := syscall.Mkfifo(filepath.Join(dir, "go.mod"), 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		}, "go.mod"},
		// A go.work that is a dangling link is the app's: never written through.
		{"a go.work that is a dangling link", func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, GoWorkspaceFile)); err != nil {
				t.Fatal(err)
			}
		}, "go.work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := t.TempDir()
			write(t, release, "go.mod", "module example.com/app\n\ngo 1.24.0\n")
			tc.setup(t, release)

			type result struct {
				prep GoPrep
				err  error
			}
			done := make(chan result, 1)
			go func() { p, err := PrepareGo(release); done <- result{p, err} }()
			var prep GoPrep
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatal(r.err)
				}
				prep = r.prep
			case <-time.After(time.Second):
				t.Fatal("PrepareGo blocked")
			}
			if prep.Installed {
				t.Fatal("the platform overrode the app's own arrangement")
			}
			if !strings.Contains(prep.Skipped, tc.says) {
				t.Errorf("Skipped = %q, want it to name %s", prep.Skipped, tc.says)
			}
			prep.Done()
		})
	}
}

// A workspace may not declare less than any module it uses, and it uses two.
func TestTheWorkspaceDeclaresTheHigherOfTheTwo(t *testing.T) {
	for _, tc := range []struct{ goMod, want string }{
		{"module m\n\ngo 1.24.0\n", "1.24.0"},
		{"module m\n\ngo 1.21\n", "1.21"},
		{"module m\n\ngo 1.19\n", goWorkspaceFloor},    // below the SDK's floor
		{"module m\n", goWorkspaceFloor},               // no directive at all
		{"module m\n\n// go 1.99\n", goWorkspaceFloor}, // a comment is not a directive
		{"module m\n\ngo 1.30.4\n", "1.30.4"},
		// A trailing comment and a pre-release suffix are legal directives the
		// go command reads; read strictly they fell to the floor and the build
		// stopped on "go.work lists go 1.21" (code review 2026-11-11, K-2).
		{"module m\n\ngo 1.23 // minimum\n", "1.23"},
		{"module m\n\ngo 1.24rc1\n", "1.24"},
	} {
		if got := workspaceLanguage(tc.goMod); got != tc.want {
			t.Errorf("workspaceLanguage(%q) = %q, want %q", tc.goMod, got, tc.want)
		}
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listing(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// The operator chose a module mode, and a workspace cannot honour two of the
// three. Overriding it would turn every Go build on that install into a failure
// whose message names a flag rather than the SDK.
func TestAnOperatorsModuleModeWins(t *testing.T) {
	for _, tc := range []struct {
		goflags string
		skipped bool
	}{
		{"", false},
		{"-trimpath", false},
		{"-mod=readonly", false},
		{"-mod=mod", true},
		{"-mod=vendor", true},
		{"-trimpath -mod=mod", true},
		{"-mod=mod -mod=readonly", false}, // the last one wins, as on a command line
	} {
		t.Setenv("GOFLAGS", tc.goflags)
		release := t.TempDir()
		write(t, release, "go.mod", "module example.com/app\n\ngo 1.24.0\n")

		prep, err := PrepareGo(release)
		if err != nil {
			t.Fatal(err)
		}
		if prep.Installed == tc.skipped {
			t.Errorf("GOFLAGS=%q: Installed = %v, want %v", tc.goflags, prep.Installed, !tc.skipped)
		}
		if tc.skipped && !strings.Contains(prep.Skipped, "GOFLAGS") {
			t.Errorf("GOFLAGS=%q: Skipped = %q, want it to name the variable", tc.goflags, prep.Skipped)
		}
		prep.Done()
	}
}

// The wire contract says the payload is text. It is true today because every
// embed glob names a source extension, and this is what keeps it true — a
// future glob that swept in a binary would otherwise be discovered by a client
// decoding a JSON string.
func TestTheShippedSurfaceIsAllText(t *testing.T) {
	for _, lang := range Languages() {
		payload, err := Files(lang)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if len(payload.Files) == 0 {
			t.Errorf("%s ships nothing", lang)
		}
		for path, body := range payload.Files {
			if !utf8.ValidString(body) {
				t.Errorf("%s/%s is not text", lang, path)
			}
			if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
				t.Errorf("%s ships %q, which a client would write outside the directory it chose", lang, path)
			}
		}
	}
	if _, err := Files("ruby"); err == nil {
		t.Error("a language this binary does not carry was answered")
	}
}

// What the build writes and what the route serves are the same bytes. Two
// definitions of "the SDK" is the drift the embedding exists to prevent.
func TestTheWireAndTheDiskAgree(t *testing.T) {
	for _, lang := range Languages() {
		dir, err := Materialize(t.TempDir(), lang)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := Files(lang)
		if err != nil {
			t.Fatal(err)
		}

		onDisk := map[string]string{}
		root := dir
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			body, _ := os.ReadFile(p)
			onDisk[filepath.ToSlash(rel)] = string(body)
			return nil
		})

		if len(onDisk) != len(payload.Files) {
			t.Errorf("%s: %d files on disk, %d on the wire", lang, len(onDisk), len(payload.Files))
		}
		for path, body := range payload.Files {
			if onDisk[path] != body {
				t.Errorf("%s/%s differs between the build's copy and the wire's", lang, path)
			}
		}
	}
}

// Every language names itself the way its own ecosystem does, and a client
// renders whichever field is there.
func TestEachLanguageNamesItselfOnce(t *testing.T) {
	for _, tc := range []struct{ lang, module, pkg, floor string }{
		{"go", GoModule, "", GoLanguageFloor},
		{"python", "", "toft-sdk", ""},
		{"node", "", "toft-sdk", ""},
	} {
		payload, err := Files(tc.lang)
		if err != nil {
			t.Fatal(err)
		}
		if payload.Module != tc.module || payload.Package != tc.pkg || payload.LanguageFloor != tc.floor {
			t.Errorf("%s = %+v, want module %q package %q floor %q",
				tc.lang, payload, tc.module, tc.pkg, tc.floor)
		}
	}
}

// Every language Files answers names its package, and nothing off the list is
// answered — the R SDK was once served with an empty package name because the
// embedded tree, not the list, decided what existed.
func TestFilesAnswersTheListAndOnlyTheList(t *testing.T) {
	for _, lang := range Languages() {
		p, err := Files(lang)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if p.Package == "" && p.Module == "" {
			t.Errorf("%s answered with no package name", lang)
		}
	}
	if p, _ := Files("r"); p.Package != "toft" {
		t.Errorf("R package = %q, want toft (DESCRIPTION's Package)", p.Package)
	}
	for _, lang := range []string{"R", "python/toft", "", "ruby"} {
		if _, err := Files(lang); err == nil {
			t.Errorf("Files(%q) answered", lang)
		}
	}
}

// A vendored copy on disk hashes to the same build id as the payload it was
// written from — the comparison the Go build log makes when the app brought its
// own SDK.
func TestBuildOfDirAgreesWithBuild(t *testing.T) {
	shipped, err := payload("go")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for p, b := range shipped {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := BuildOfDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := BuildOrEmpty("go"); got != want {
		t.Errorf("BuildOfDir = %s, Build = %s", got, want)
	}
	if line := GoSkippedLine(filepath.Dir(filepath.Dir(dir)), "x"); strings.Contains(line, "own copy") {
		t.Errorf("a release with no vendored copy names one: %q", line)
	}
}

// The vendored directory is the publisher's, read on the platform's side of a
// build: a tree far larger than any SDK is refused before it is read, rather
// than loaded into the platform's memory to be hashed.
func TestBuildOfDirRefusesATreeNoSDKIsThatLarge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("package toft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sparse: the size is what the walk measures, and no disk is spent.
	f, err := os.Create(filepath.Join(dir, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxVendoredBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if id, err := BuildOfDir(dir); err == nil {
		t.Fatalf("BuildOfDir hashed a %d-byte tree: %s", maxVendoredBytes+1, id)
	}
}

// BB-059, fifth 2026-11-03 black-box list: a Go app that brought its own
// go.work but shipped no vendored copy is told so, rather than the build
// line being left out without a reason.
func TestGoSkippedLineSaysWhyNoBuildIsNamed(t *testing.T) {
	line := GoSkippedLine(t.TempDir(), "this app brought its own go.work")
	if !strings.Contains(line, "No copy of the SDK shipped at "+VendoredGoDir) {
		t.Errorf("line = %q", line)
	}
}

// BB-085: a Node app's own copy is named by build, through the symlink a
// file: dependency leaves, and matches what toft sdk vendor reports.
func TestNodeKeptLineNamesTheAppsBuild(t *testing.T) {
	root := t.TempDir()
	vendored := filepath.Join(root, "vendor", "toft")
	if err := Write(vendored, "node"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vendored, filepath.Join(root, "node_modules", "toft-sdk")); err != nil {
		t.Fatal(err)
	}
	want, _ := Build("node")
	if line := NodeKeptLine(root); !strings.Contains(line, "build "+want) {
		t.Errorf("line = %q, want build %s", line, want)
	}
}

// The vendored copy is in a tree the app's account can rewrite by the time it
// is hashed: a FIFO swapped in after the walk answers at once instead of
// parking the build worker, and a link out of the tree is refused (security
// review 2026-11-14, PB-5).
func TestHashFileRefusesAFIFOAndALinkOutOfTheTree(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	done := make(chan error, 1)
	go func() { done <- hashFile(io.Discard, root, "pipe", 0) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was hashed as a file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hashFile blocked opening a FIFO")
	}
	if err := hashFile(io.Discard, root, "link", 1); err == nil {
		t.Error("a link leaving the tree was followed and hashed")
	}
}

// The Node SDK was renamed after the product (toft-sdk); the old name,
// @larchway/toft, is installed beside it for one minor so an app still
// importing it keeps working, and a copy an app brought under either name is
// its own.
func TestNodeIsInstalledUnderBothNames(t *testing.T) {
	release := t.TempDir()
	if installed, err := EnsureNode(release); err != nil || !installed {
		t.Fatalf("EnsureNode = %v, %v", installed, err)
	}
	for _, rel := range []string{"toft-sdk", "@larchway/toft"} {
		if _, err := os.Stat(filepath.Join(release, "node_modules", rel, "index.js")); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}

	own := t.TempDir()
	legacy := filepath.Join(own, "node_modules", "@larchway", "toft")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(legacy, "package.json"), []byte(`{"version":"9.9.9"}`), 0o644)
	if installed, err := EnsureNode(own); err != nil || installed {
		t.Errorf("an app's own copy under the old name was not kept: %v, %v", installed, err)
	}
}

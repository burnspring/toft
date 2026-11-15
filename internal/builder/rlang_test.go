package builder

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"toft/internal/runner"
	"toft/internal/toolchain"
)

// The R release must not carry a link out of its own tree.
//
// This is the first of the three tests whose failure would otherwise be silent.
// renv::restore() defaults to a machine-global cache and fills the library with
// symlinks into it; internal/artifact's pack stores a symlink as a symlink and
// unpack recreates it, so the artifact is *smaller* and every package in it
// points at a path that exists on no pod. Nothing fails until the workload
// starts and dyn.load reports a missing shared object, one package at a time.
//
// So the environment that keeps renv copying is asserted directly, and the walk
// that the build runs is asserted against a tree that has the failure in it.
// Without both, the setting can be dropped in a refactor and the only symptom is
// a deploy that succeeds.
func renvEnvMap(env []string) map[string]string {
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	return got
}

func TestRenvEnvironmentCopiesOutOfTheCache(t *testing.T) {
	got := renvEnvMap(runner.RenvEnv("/srv/app/renv/library/R-4.5", "https://example.invalid/cran",
		"/var/cache/toft/renv"))
	want := map[string]string{
		"RENV_PATHS_LIBRARY":         "/srv/app/renv/library/R-4.5",
		"RENV_CONFIG_CACHE_ENABLED":  "TRUE",
		"RENV_CONFIG_CACHE_SYMLINKS": "FALSE",
		"RENV_PATHS_CACHE":           "/var/cache/toft/renv",
		"RENV_CONFIG_REPOS_OVERRIDE": "https://example.invalid/cran",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("RenvEnv %s = %q, want %q — a cache renv links into rather than copies "+
				"out of is what puts machine-global paths into a release that then travels",
				k, got[k], v)
		}
	}
}

// A restore never resolves the app's DESCRIPTION `Remotes:` against GitHub:
// the lock already pins them, and the anonymous API quota is shared by every
// build on the install. Both cache modes, because a repair restores too.
func TestRenvEnvironmentIgnoresTheProjectsRemotes(t *testing.T) {
	for _, cache := range []string{"/var/cache/toft/renv", ""} {
		if got := renvEnvMap(runner.RenvEnv("/lib", "", cache)); got["RENV_CONFIG_INSTALL_REMOTES"] != "FALSE" {
			t.Errorf("RENV_CONFIG_INSTALL_REMOTES = %q (cache %q), want FALSE", got["RENV_CONFIG_INSTALL_REMOTES"], cache)
		}
	}
}

// With the cache on, renv's binary load test is off: renv 1.3 runs it before a
// cache hit's own dependencies are installed, and fails restores that are fine.
func TestRenvEnvironmentWithTheCacheSkipsTheLoadTest(t *testing.T) {
	if got := renvEnvMap(runner.RenvEnv("/lib", "", "/var/cache/toft/renv")); got["RENV_INSTALL_TEST_LOAD"] != "FALSE" {
		t.Errorf("RENV_INSTALL_TEST_LOAD = %q with the cache on, want FALSE", got["RENV_INSTALL_TEST_LOAD"])
	}
	if got := renvEnvMap(runner.RenvEnv("/lib", "", "")); got["RENV_INSTALL_TEST_LOAD"] != "" {
		t.Errorf("RENV_INSTALL_TEST_LOAD = %q with the cache off, want renv's default", got["RENV_INSTALL_TEST_LOAD"])
	}
}

// No cache directory — a pod's repair — is the cache off, and emptied rather
// than left to whatever the process inherited.
func TestRenvEnvironmentWithNoCacheTurnsItOff(t *testing.T) {
	got := renvEnvMap(runner.RenvEnv("/lib", "https://example.invalid/cran", ""))
	if got["RENV_CONFIG_CACHE_ENABLED"] != "FALSE" {
		t.Errorf("RENV_CONFIG_CACHE_ENABLED = %q with no cache directory, want FALSE",
			got["RENV_CONFIG_CACHE_ENABLED"])
	}
	if v, ok := got["RENV_PATHS_CACHE"]; !ok || v != "" {
		t.Errorf("RENV_PATHS_CACHE = %q (set: %v), want set and empty — renv reads an "+
			"inherited value otherwise", v, ok)
	}
}

// The cache path reaches the renv commands and nothing else. A render or an
// app's own .Rprofile that loads renv would see the path without
// RENV_CONFIG_CACHE_SYMLINKS=FALSE beside it, and link the library into it.
func TestTheRenvCacheIsNotInTheGeneralBuildEnvironment(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: "/var/lib/toft/cache"}}
	if got := b.renvCacheDir(); got != "/var/lib/toft/cache/renv" {
		t.Errorf("renvCacheDir() = %q, want /var/lib/toft/cache/renv", got)
	}
	for k, v := range b.buildEnvironment(nil, "/apps/x") {
		if strings.HasPrefix(k, "RENV_") || strings.Contains(v, "/cache/renv") {
			t.Errorf("buildEnvironment carries %s=%q — every build command sees it, "+
				"including the ones that do not turn renv's symlinks off", k, v)
		}
	}
	if (&Builder{}).renvCacheDir() != "" {
		t.Error("a builder with no cache directory still named an renv cache")
	}
}

func TestRSourceCompilesUseEveryCPU(t *testing.T) {
	if got, want := rMakeFlags(), "-j"+strconv.Itoa(goruntime.GOMAXPROCS(0)); got != want {
		t.Errorf("rMakeFlags() = %q, want %q — one compiler per CPU this process may use", got, want)
	}
}

// No repository override when the operator has named none: the lockfile's own
// repositories stand, which is slow but correct, rather than an empty override
// that would resolve to nothing.
func TestRenvEnvironmentOmitsAnEmptyOverride(t *testing.T) {
	for _, kv := range runner.RenvEnv("/lib", "", "") {
		if strings.HasPrefix(kv, "RENV_CONFIG_REPOS_OVERRIDE=") {
			t.Fatalf("RenvEnv set %q with no repository configured — an empty override is not "+
				"the same as no override, and renv would resolve against nothing", kv)
		}
	}
}

// The walk that runs at the end of every R build, against a tree that has the
// failure in it.
func TestAnEscapingLinkInTheLibraryIsFound(t *testing.T) {
	release := t.TempDir()
	lib := filepath.Join(release, "renv", "library", "R-4.5")
	if err := os.MkdirAll(filepath.Join(lib, "glue"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A package that is real files: what D5's environment produces.
	if err := os.WriteFile(filepath.Join(lib, "glue", "DESCRIPTION"), []byte("Package: glue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	offender, err := verifyNoEscapingLinks(release, lib)
	if err != nil {
		t.Fatal(err)
	}
	if offender != "" {
		t.Fatalf("a library of real files was reported as escaping: %s", offender)
	}

	// A package that is a link into a cache, which is what renv writes when the
	// cache is left on. The target deliberately exists, because a *dangling*
	// link is the easy case — this is the one that looks fine on the build host.
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "R6"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cache, "R6"), filepath.Join(lib, "R6")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	offender, err = verifyNoEscapingLinks(release, lib)
	if err != nil {
		t.Fatal(err)
	}
	if offender == "" {
		t.Fatal("a package symlinked into a cache outside the release was not reported. " +
			"That release packs, unpacks, and fails at dyn.load in the pod")
	}
	if !strings.Contains(offender, "R6") {
		t.Errorf("reported %q, want the R6 link", offender)
	}
}

// A link that stays inside the release is not a failure: the rule is about
// leaving the tree, not about links.
func TestALinkInsideTheReleaseIsAllowed(t *testing.T) {
	release := t.TempDir()
	lib := filepath.Join(release, "renv", "library", "R-4.5")
	if err := os.MkdirAll(filepath.Join(lib, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(lib, "real"), filepath.Join(lib, "alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	offender, err := verifyNoEscapingLinks(release, lib)
	if err != nil {
		t.Fatal(err)
	}
	if offender != "" {
		t.Errorf("a link within the release was reported as escaping: %s", offender)
	}
}

// The build execs the R the deploy resolved, not the install default.
//
// The fourth test whose failure is otherwise silent, and the one that was
// missing while the bug was live. rBuildBinary took toolchain.DefaultPath, and
// on every install carrying a single R the default *is* the resolved one — so
// the build log, the deployment row and the library path all said 4.3 and were
// all correct, while `R CMD INSTALL` used 4.5's Makeconf and compiled every
// source package against the wrong headers. Nothing failed until a package
// that R 4.5 will not compile reached the compiler, several minutes in, in a
// log that had already printed the right version at the top.
//
// So the resolved path is asserted to beat the default rather than to exist:
// a test that only checked "returns something executable" passes against the
// bug.
func TestTheBuildExecsTheResolvedRRatherThanTheDefault(t *testing.T) {
	rt := &resolvedRuntime{
		Language:  "r",
		Chosen:    toolchain.Version{Language: "r", Version: "4.3.3", Path: "/opt/R/4.3.3/bin/Rscript"},
		BuildWith: toolchain.Version{Language: "r", Version: "4.3.3", Path: "/opt/R/4.3.3/bin/Rscript"},
		Pin:       ">=4.3,<4.4",
	}
	if got := rBuildBinary(rt); got != "/opt/R/4.3.3/bin/Rscript" {
		t.Errorf("rBuildBinary = %q, want the resolved interpreter /opt/R/4.3.3/bin/Rscript", got)
	}

	// The series the library is stamped with has to agree with the interpreter
	// that fills it. These are read from one place for that reason: a release
	// whose library says R-4.3 and whose .so files were built by 4.5 is broken
	// in the way only the workload finds out about.
	if got := rt.rSeries(); got != "4.3" {
		t.Errorf("rSeries = %q, want 4.3 to match the resolved interpreter", got)
	}

	// No resolution — a language with no pin and no probed interpreter — still
	// falls back rather than returning empty, because a build that cannot name
	// an R should fail at exec with a name in the message.
	if got := rBuildBinary(nil); got == "" {
		t.Error("rBuildBinary(nil) = \"\", want the install default or the bare binary name")
	}
}

// An R release gets a Makevars that lets old source compile on this image —
// ragg 1.3.0 against FreeType 2.13.3 needs -fpermissive, and in every C++
// standard's own flags, because R swaps CXXFLAGS for CXX17FLAGS by CXX_STD.
func TestTheRMakevarsLetsOldSourceCompile(t *testing.T) {
	dir := t.TempDir()
	path, err := runner.WriteRMakevars(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, runner.RMakevarsFile) {
		t.Errorf("path = %q, want it at the release root", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CXXFLAGS += -fpermissive", "CXX17FLAGS += -fpermissive", "CXX20FLAGS += -fpermissive",
		"-Wno-error=incompatible-pointer-types", "-Wno-error=int-conversion",
		"-Wno-error=implicit-function-declaration",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("Makevars lacks %q:\n%s", want, body)
		}
	}
	// Appended, never assigned: R's own -O2 and -fstack-protector stay.
	if strings.Contains(string(body), "FLAGS =") {
		t.Errorf("Makevars replaces R's flags rather than adding to them:\n%s", body)
	}
}

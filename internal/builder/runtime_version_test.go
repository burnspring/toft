package builder

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/toolchain"
)

func runtimeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseRuntimePins(t *testing.T) {
	dir := runtimeManifest(t, "[runtime]\npython = \">=3.12,<3.14\"\nnode = \"24\"\n")
	pins, err := ParseRuntimePins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pins[toolchain.Python] != ">=3.12,<3.14" || pins[toolchain.Node] != "24" {
		t.Fatalf("got %v", pins)
	}
}

// The same contract [jobs] and [static] rely on: a table this reader does not
// name is ignored, so the first developer to write [app] does not break their
// own deploy.
func TestNoRuntimeTableIsNotAnError(t *testing.T) {
	for _, body := range []string{"", "[app]\nid = \"x\"\n", "[jobs.nightly]\ncommand = \"x\"\n"} {
		pins, err := ParseRuntimePins(runtimeManifest(t, body))
		if err != nil {
			t.Errorf("%q: %v", body, err)
		}
		if len(pins) != 0 {
			t.Errorf("%q produced pins %v", body, pins)
		}
	}
	if pins, err := ParseRuntimePins(t.TempDir()); err != nil || pins != nil {
		t.Errorf("a release with no toft.toml at all: %v %v", pins, err)
	}
}

// D4. go.mod already says it, and a second declaration could only disagree —
// so the key is refused rather than ignored, and the refusal says where to put
// it instead.
func TestAGoKeyIsRefused(t *testing.T) {
	_, err := ParseRuntimePins(runtimeManifest(t, "[runtime]\ngo = \">=1.26\"\n"))
	if err == nil {
		t.Fatal("[runtime] go was accepted")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("the refusal does not point at go.mod: %v", err)
	}
}

// A range the platform half understood is a pin that silently does not exist.
func TestAMalformedRangeFailsTheDeploy(t *testing.T) {
	for _, body := range []string{
		"[runtime]\npython = \"^3.12\"\n",
		"[runtime]\nnode = \"24.1\"\n",
		"[runtime]\npython = \"latest\"\n",
	} {
		if _, err := ParseRuntimePins(runtimeManifest(t, body)); err == nil {
			t.Errorf("%q was accepted", body)
		}
	}
}

func TestGoModPin(t *testing.T) {
	for _, tc := range []struct{ mod, want string }{
		{"module x\n\ngo 1.25\n", ">=1.25"},
		{"module x\n\ngo 1.25.3\n", ">=1.25"},
		{"module x\n", ""},
		{"module x\n\ngo tip\n", ""},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tc.mod), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := GoModPin(dir); got != tc.want {
			t.Errorf("GoModPin(%q) = %q, want %q", tc.mod, got, tc.want)
		}
	}
	if got := GoModPin(t.TempDir()); got != "" {
		t.Errorf("a release with no go.mod produced %q", got)
	}
}

func TestLanguageOfCoversEveryFramework(t *testing.T) {
	for f, want := range map[Framework]string{
		FrameworkNode:      toolchain.Node,
		FrameworkNextJS:    toolchain.Node,
		FrameworkGo:        toolchain.Go,
		FrameworkPython:    toolchain.Python,
		FrameworkFlask:     toolchain.Python,
		FrameworkFastAPI:   toolchain.Python,
		FrameworkStreamlit: toolchain.Python,
		FrameworkStatic:    "",
	} {
		if got := LanguageOf(f); got != want {
			t.Errorf("LanguageOf(%s) = %q, want %q", f, got, want)
		}
	}
}

// Open question 4, decided: a static app runs no commands and has no
// interpreter, so a [runtime] table on one is a manifest error rather than a
// no-op. Same call as a declared job on a static app, for the same reason.
func TestARuntimeTableOnAStaticAppIsRefused(t *testing.T) {
	b := &Builder{}
	dir := runtimeManifest(t, "[runtime]\npython = \"3.13\"\n")
	_, err := b.resolveRuntime(FrameworkStatic, dir, "shared", func(string) {})
	if err == nil {
		t.Fatal("a [runtime] table on a static app was accepted")
	}
	if !strings.Contains(err.Error(), "no interpreter") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// And a static app with no table is fine, with nothing to record.
	got, err := b.resolveRuntime(FrameworkStatic, t.TempDir(), "shared", func(string) {})
	if err != nil || got != nil {
		t.Errorf("a static app with no [runtime] table: %v %v", got, err)
	}
}

// The build log has to name the resolved version on every build, pinned or not
// (D2) — it is the only thing that makes an older platform's silent ignoring of
// the table detectable.
func TestTheResolvedVersionIsReportedEvenWithNoPin(t *testing.T) {
	r := &resolvedRuntime{
		Language:  toolchain.Python,
		Chosen:    toolchain.Version{Language: toolchain.Python, Version: "3.13.7", Path: "/usr/bin/python3", Default: true},
		BuildWith: toolchain.Version{Language: toolchain.Python, Version: "3.13.7", Path: "/usr/bin/python3", Default: true},
	}
	var log []string
	r.report(func(s string) { log = append(log, s) })
	joined := strings.Join(log, "\n")
	if !strings.Contains(joined, "Python 3.13.7") || !strings.Contains(joined, "install default") {
		t.Errorf("build log does not name the resolved version and how it was chosen:\n%s", joined)
	}

	r.Pin = ">=3.12,<3.14"
	log = nil
	r.report(func(s string) { log = append(log, s) })
	if !strings.Contains(strings.Join(log, "\n"), "satisfies >=3.12,<3.14") {
		t.Errorf("a pinned build does not quote the range back: %v", log)
	}
}

// uv with no --python does not fall through to `python3` on PATH — it runs its
// own discovery and can prefer a managed standalone build it downloaded
// earlier. Measured, not reasoned about: on a host whose `python3` was 3.13, a
// bare `uv venv venv` produced a 3.12 environment, so the build log said one
// version and the running app reported another. The resolved interpreter is
// therefore passed explicitly **even when it is the default**.
func TestPythonArgs(t *testing.T) {
	def := &resolvedRuntime{Language: toolchain.Python,
		BuildWith: toolchain.Version{Version: "3.13.7", Path: "/usr/bin/python3", Default: true}}
	if got := def.pythonArgs(); len(got) != 2 || got[1] != "/usr/bin/python3" {
		t.Errorf("pythonArgs = %v — the default must be passed explicitly too, or uv's own "+
			"discovery decides and the build log becomes a lie", got)
	}

	alt := &resolvedRuntime{Language: toolchain.Python,
		BuildWith: toolchain.Version{Version: "3.12.9", Path: "/opt/py312/bin/python3"}}
	if got := alt.pythonArgs(); len(got) != 2 || got[0] != "--python" || got[1] != "/opt/py312/bin/python3" {
		t.Errorf("pythonArgs = %v, want --python /opt/py312/bin/python3", got)
	}

	node := &resolvedRuntime{Language: toolchain.Node,
		BuildWith: toolchain.Version{Version: "22.1.0", Path: "/opt/node22/bin/node"}}
	if got := node.pythonArgs(); got != nil {
		t.Errorf("Node got a --python: %v", got)
	}
}

// Node and Go take their interpreter by PATH, because the npm beside a node is
// the one that has to run against it.
func TestApplyPutsTheInterpreterFirstOnPath(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin"}
	r := &resolvedRuntime{Language: toolchain.Node,
		BuildWith: toolchain.Version{Version: "22.1.0", Path: "/opt/node22/bin/node"}}
	r.apply(env)
	if !strings.HasPrefix(env["PATH"], "/opt/node22/bin"+string(os.PathListSeparator)) {
		t.Errorf("PATH = %q, want the resolved node's directory first", env["PATH"])
	}

	// Python is passed by argument, so PATH is left alone.
	env = map[string]string{"PATH": "/usr/bin"}
	(&resolvedRuntime{Language: toolchain.Python,
		BuildWith: toolchain.Version{Path: "/opt/py/bin/python3"}}).apply(env)
	if env["PATH"] != "/usr/bin" {
		t.Errorf("Python rewrote PATH: %q", env["PATH"])
	}

	// Nothing resolved on this host: nothing to prepend.
	env = map[string]string{"PATH": "/usr/bin"}
	(&resolvedRuntime{Language: toolchain.Node}).apply(env)
	if env["PATH"] != "/usr/bin" {
		t.Errorf("PATH was rewritten with nothing to put on it: %q", env["PATH"])
	}
}

// The eight enforcement points, and the list is the specification. Each is
// tested on its own so that none can come to rely on an earlier one having run;
// a ninth place that chooses an interpreter without consulting these is a bug
// of the same class as a handler that forgets to authorize.
//
//  1. The platform refuses to start on a TOFT_*_PATH entry that is absent
//     or will not exec — toolchain.TestADeclaredPathThatWillNotExecIsAnError,
//     wired into config.Load.
//  2. builder reads [runtime] from the release manifest, and go.mod for Go —
//     TestParseRuntimePins, TestGoModPin.
//  3. builder refuses a deploy whose constraint nothing in the effective mode's
//     set satisfies — below.
//  4. builder passes the resolved interpreter to `uv venv --python` and to the
//     Node invocation — TestPythonArgs, TestApplyPutsTheInterpreterFirstOnPath.
//  5. builder sets GOTOOLCHAIN, GOPROXY, GOFLAGS and the proxy variables from
//     configuration, and refuses GOOS/GOARCH — below.
//  6. The build records the version it built against and reports it in the log,
//     for every language, pinned or not — TestTheResolvedVersionIsReportedEvenWithNoPin,
//     and for Go the one the *binary* names rather than the one resolved,
//     because the go command may re-exec after the platform has decided —
//     TestPointSix_TheRecordedGoVersionIsTheOneThatCompiledTheBinary.
//  7. The runner refuses to start a workload whose declared version is not what
//     it probes in its own image — runner.TestVerifyRuntimeVersion.
//  8. Both launchers start a workload under the interpreter its *release*
//     names, and a release naming one this host no longer has is rebuilt rather
//     than started on another — below, and
//     runtime.TestANodeWorkloadIsStartedUnderTheNodeItsReleaseNames,
//     runner.TestANodeReleaseBuiltForAVersionThisHostLostIsRebuilt.

// Point 8, at the end a build owns: the resolved interpreter is written into the
// release, because a decision the launcher cannot read is a decision that stops
// at the build. The other half of the point — both launchers reading it back —
// is tested where those launchers are.
func TestPointEight_TheBuildRecordsTheInterpreterInTheReleaseItself(t *testing.T) {
	dir := t.TempDir()
	lg := buildLog{step: func(string) {}, output: func(string) {}}

	stampRuntime(dir, &resolvedRuntime{
		Language:  toolchain.Node,
		Chosen:    toolchain.Version{Language: toolchain.Node, Version: "20.20.2"},
		BuildWith: toolchain.Version{Language: toolchain.Node, Version: "20.20.2", Path: "/opt/node/20/bin/node"},
	}, lg)
	if lang, v := runner.ReleaseRuntime(dir); lang != toolchain.Node || v != "20.20.2" {
		t.Errorf("the release records %q %q; a pin honoured at build time and unreadable at "+
			"start is a pin the launcher throws away", lang, v)
	}

	// R has a stamp of its own — the series in its library directory — and Go
	// leaves nothing in the tree to be wrong about. A second record for either
	// would be a second place for the truth to live.
	for _, lang := range []string{toolchain.R, toolchain.Go} {
		dir := t.TempDir()
		stampRuntime(dir, &resolvedRuntime{
			Language: lang,
			Chosen:   toolchain.Version{Language: lang, Version: "4.4.3"},
		}, lg)
		if l, _ := runner.ReleaseRuntime(dir); l != "" {
			t.Errorf("%s releases carry a runtime stamp, and %s already answers the question "+
				"elsewhere", lang, lang)
		}
	}
}

// Point 6, the half that was wrong for Go: the recorded version has to be the
// toolchain that compiled the artifact, not the one the platform resolved.
//
// They are the same answer for every other family -- uv is handed `--python`,
// npm is the one beside the resolved node, R's library is stamped -- and they
// legitimately differ for Go, because `GOTOOLCHAIN` sends the go command into
// another toolchain after the platform has finished deciding. Found on this
// platform's own cluster: a row reading 1.27.0 for an app whose binary reports
// go1.25.14, and an estate census counting it under a version it does not run.
func TestPointSix_TheRecordedGoVersionIsTheOneThatCompiledTheBinary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, runtime.GoBinaryName)

	// The stamp is read out of the binary itself, never by exec'ing a go
	// (security review 2026-11-10, E-1b): this test's own executable is a Go
	// binary with a stamp, so a copy of it is the release's binary.
	if got := goToolchainOf(dir, runtime.GoBinaryName); got != "" {
		t.Errorf("a binary that does not exist reported %q; there is nothing to read "+
			"and nothing may be invented", got)
	}
	if err := os.WriteFile(binary, []byte("not really a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := goToolchainOf(dir, runtime.GoBinaryName); got != "" {
		t.Errorf("a file with no stamp reported %q rather than nothing", got)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, exe, 0o755); err != nil {
		t.Fatal(err)
	}
	want := toolchain.GoStamp(goruntime.Version())
	if got := goToolchainOf(dir, runtime.GoBinaryName); got == "" || got != want {
		t.Fatalf("goToolchainOf = %q, want %q read out of the stamp", got, want)
	}
	// A link out of the release is not followed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "bin"), exe, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(binary)
	if err := os.Symlink(filepath.Join(outside, "bin"), binary); err != nil {
		t.Fatal(err)
	}
	if got := goToolchainOf(dir, runtime.GoBinaryName); got != "" {
		t.Errorf("a binary linked from outside the release was read: %q", got)
	}
	os.Remove(binary)
	if err := os.WriteFile(binary, exe, 0o755); err != nil {
		t.Fatal(err)
	}

	// And the correction is only spoken when there is something to correct: a
	// line printed on every build is a line nobody reads.
	for _, tc := range []struct {
		name, resolved string
		wantSaid       bool
	}{
		{"the toolchain moved under the build", "1.99.0", true},
		{"the go command was left to settle it", "", true},
		{"the build used what was resolved", want, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var said []string
			lg := buildLog{
				step:   func(s string) { said = append(said, s) },
				output: func(string) {},
			}
			rt := &resolvedRuntime{
				Language: toolchain.Go,
				Chosen:   toolchain.Version{Language: toolchain.Go, Version: tc.resolved},
			}
			// No deployment id, so nothing is written: this is the repair path,
			// and it exercises everything but the row.
			(&Builder{}).confirmGoToolchain("", dir, rt, lg)
			if got := len(said) > 0; got != tc.wantSaid {
				t.Errorf("said %v, want spoken=%v", said, tc.wantSaid)
			}
			if tc.wantSaid && !strings.Contains(strings.Join(said, " "), want) {
				t.Errorf("the correction does not name the toolchain that built it: %v", said)
			}
		})
	}
}

// Point 3. The range is one no install can satisfy, so this holds whatever is
// installed on the machine running the test."""
func TestPointThree_ARangeNothingSatisfiesRefusesTheDeploy(t *testing.T) {
	b := &Builder{}
	dir := runtimeManifest(t, "[runtime]\npython = \">=99.0\"\n")
	_, err := b.resolveRuntime(FrameworkPython, dir, "shared", func(string) {})
	if err == nil {
		t.Fatal("a pin nothing satisfies was approximated instead of refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, ">=99.0") {
		t.Errorf("the refusal does not quote the range back: %v", err)
	}
	if !strings.Contains(msg, "this build host") {
		t.Errorf("the refusal does not say which set was searched: %v", err)
	}
}

// Point 5. The operator's toolchain environment reaches a build, and the
// cross-compilation variables do not.
func TestPointFive_TheBuildEnvironmentIsTheOperatorsAndNotTheApps(t *testing.T) {
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "https://mirror.internal/go")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")

	b := &Builder{opts: Options{CacheDir: "/var/lib/toft/cache"}}
	env := b.buildEnvironment(map[string]string{
		"GOOS":         "windows",
		"GOARCH":       "arm64",
		"GOPROXY":      "https://the-app-would-rather-use-this",
		"DATABASE_URL": "the app's own",
	}, "/apps/x")

	for k, want := range map[string]string{
		"GOTOOLCHAIN": "local",
		"GOPROXY":     "https://mirror.internal/go",
		"GOFLAGS":     "-mod=mod",
		"HTTPS_PROXY": "http://proxy.internal:3128",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q — a build behind a proxy cannot reach anything without it", k, env[k], want)
		}
	}
	for _, k := range []string{"GOOS", "GOARCH"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s reached the build environment; a cross-compiled binary passes every "+
				"check in a deploy and dies at exec", k)
		}
	}
	if env["DATABASE_URL"] != "the app's own" {
		t.Error("the app's own variables were dropped")
	}
	// The platform's caches beat both: a Go toolchain fetched by GOTOOLCHAIN=auto
	// belongs in a cache the platform owns and can account for.
	if env["GOMODCACHE"] != "/var/lib/toft/cache/go-mod" {
		t.Errorf("GOMODCACHE = %q, want the platform's own", env["GOMODCACHE"])
	}
	// LB-10: no go environment file in the build's persistent HOME shapes
	// what it installs, whatever the app declared.
	env = b.buildEnvironment(map[string]string{"GOENV": "/apps/x/goenv"}, "/apps/x")
	if env["GOENV"] != "off" {
		t.Errorf("GOENV = %q, want off", env["GOENV"])
	}
}

// The release's own statement of which R it was built against is honoured
// without anyone hand-writing a pin.
//
// The gap this closes had a visible cost: every Posit Connect bundle on this
// install failed its build, and the fix an operator could reach for was to add
// a `toft.toml` to somebody else's bundle. The declaration was already in
// the release — `manifest.json`'s `platform`, translated into `renv.lock` by
// adoptConnectManifest before the runtime is resolved — and nothing read it.
func TestTheDeclaredRSeriesIsReadFromTheLockfile(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// The patch is dropped, because R's binaries are published per minor and
	// nobody wrote this string for this platform to parse.
	if got := RDeclaredSeries(write(t, `{"R":{"Version":"4.4.1"},"Packages":{}}`)); got != "4.4" {
		t.Errorf("RDeclaredSeries = %q, want 4.4 — the series 4.4.1 belongs to", got)
	}

	// Three ways of declaring nothing, all of which have to reach the caller as
	// "" so it takes the install default rather than refusing on a series no
	// interpreter can match.
	for name, body := range map[string]string{
		"no R block":    `{"Packages":{}}`,
		"empty version": `{"R":{"Version":""},"Packages":{}}`,
		"not a version": `{"R":{"Version":"unknown"},"Packages":{}}`,
	} {
		if got := RDeclaredSeries(write(t, body)); got != "" {
			t.Errorf("%s: RDeclaredSeries = %q, want \"\"", name, got)
		}
	}

	// A lockfile that will not parse is the restore's to complain about, in a
	// message that names the file. A version decision is the wrong place to
	// hear it.
	if got := RDeclaredSeries(write(t, `{{{`)); got != "" {
		t.Errorf("unparseable renv.lock: RDeclaredSeries = %q, want \"\"", got)
	}

	// No lockfile at all — a Shiny app that never ran renv::init.
	if got := RDeclaredSeries(t.TempDir()); got != "" {
		t.Errorf("no renv.lock: RDeclaredSeries = %q, want \"\"", got)
	}
}

// A declaration is a preference; a pin is a constraint. The difference is the
// whole reason this is applied beside toolchain.Resolve rather than passed into
// it, and it is asserted on the log because that is where an operator meets it.
func TestADeclaredSeriesTheInstallLacksFallsBackLoudly(t *testing.T) {
	r := &resolvedRuntime{
		Language:  toolchain.R,
		Chosen:    toolchain.Version{Language: toolchain.R, Version: "4.5.3", Path: "/usr/bin/Rscript", Default: true},
		BuildWith: toolchain.Version{Language: toolchain.R, Version: "4.5.3", Path: "/usr/bin/Rscript", Default: true},
		Where:     "this build host",
		Declared:  "4.4",
	}

	var log []string
	r.report(func(s string) { log = append(log, s) })
	joined := strings.Join(log, "\n")

	// Both numbers. A line naming only the one it used sends the reader to the
	// compiler error, which is where this failure was being diagnosed from.
	for _, want := range []string{"4.5.3", "4.4", "TOFT_R_PATH"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the fallback line does not mention %q:\n%s", want, joined)
		}
	}
	// It has to say *why* falling back is not merely cosmetic, because the
	// consequence — source instead of binaries — is the actual failure.
	if !strings.Contains(joined, "source") {
		t.Errorf("the fallback line does not say what it costs:\n%s", joined)
	}

	// Honoured: the log quotes the declaration back rather than calling it the
	// install default, which would be indistinguishable from not having read it.
	r.Declared, r.DeclaredHonoured = "4.5", true
	log = nil
	r.report(func(s string) { log = append(log, s) })
	joined = strings.Join(log, "\n")
	if strings.Contains(joined, "install default") {
		t.Errorf("an honoured declaration is reported as the install default:\n%s", joined)
	}
	if !strings.Contains(joined, "renv.lock") {
		t.Errorf("an honoured declaration does not name where it came from:\n%s", joined)
	}
}

// A lock built against a series the install lacks gets the closest newer one —
// the R its old source packages are likeliest to compile against — and an
// install with nothing newer keeps the default.
func TestAMissingDeclaredSeriesTakesTheNearestNewer(t *testing.T) {
	r := func(v string) toolchain.Version {
		return toolchain.Version{Language: toolchain.R, Version: v, Path: "/opt/R/" + v + "/bin/Rscript"}
	}
	set := []toolchain.Version{r("4.6.1"), r("4.5.3"), r("4.4.3"), r("4.4.1")}
	for declared, want := range map[string]string{"4.3": "4.4.3", "4.2": "4.4.3", "4.4": "4.5.3"} {
		got, ok := nearestNewerSeries(set, declared)
		if !ok || got.Version != want {
			t.Errorf("declared %s: got %q (%v), want %s", declared, got.Version, ok, want)
		}
	}
	if got, ok := nearestNewerSeries(set, "4.7"); ok {
		t.Errorf("declared 4.7: got %q, want nothing newer", got.Version)
	}
}

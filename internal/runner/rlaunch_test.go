package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/toolchain"
)

// Nothing from the app may reach the R expression.
//
// The launchers evaluate an expression rather than exec a binary with flags,
// because neither Shiny nor plumber has a --port. That makes the platform the
// author of a piece of R source, and the one property that must hold is that
// the source is a constant: everything that varies arrives as an argument and
// is read back with commandArgs.
//
// A detected file name pasted into R code would be an eval of repository
// content inside the platform's own launcher. This asserts the shape rather
// than the escaping, because a shape cannot be got subtly wrong later.
func TestTheRLaunchExpressionNeverCarriesTheAppsInput(t *testing.T) {
	hostile := `x"; system("id"); "`

	for _, framework := range []string{"shiny", "plumber"} {
		argv := RLaunchArgs(framework, hostile, "127.0.0.1", 3100)

		// -e is located rather than assumed at 0: R's own options come before
		// it (--restore, so a bundle's .RData is loaded the way Connect loads
		// it), and this test is about what follows -e, not about how many
		// flags precede it.
		e := -1
		for i, a := range argv {
			if a == "-e" {
				e = i
				break
			}
		}
		if e < 0 {
			t.Fatalf("%s: argv has no -e: %q", framework, argv)
		}
		expr := argv[e+1]
		if strings.Contains(expr, hostile) {
			t.Errorf("%s: the entrypoint was interpolated into the R expression:\n%s", framework, expr)
		}
		if expr != RLaunchExpr(framework) {
			t.Errorf("%s: the expression is not the constant", framework)
		}
		// The entrypoint is present, as data, after the expression.
		if got := argv[e+2]; got != hostile {
			t.Errorf("%s: the entrypoint is not passed as an argument after the expression: %q", framework, got)
		}
		// commandArgs is how it is read back, which is what makes the above
		// true rather than incidental.
		if !strings.Contains(expr, "commandArgs(trailingOnly = TRUE)") {
			t.Errorf("%s: the expression does not read its arguments with commandArgs:\n%s", framework, expr)
		}
	}
}

// The order of the argument vector is the contract both expressions state.
func TestTheRLaunchArgumentOrderIsEntrypointHostPort(t *testing.T) {
	argv := RLaunchArgs("shiny", "app.R", "0.0.0.0", 8080)
	// --restore first: it is an option to R and has to precede -e, after which
	// nothing separates the expression from its three arguments.
	want := []string{"--restore", "-e", ShinyLaunchExpr, "app.R", "0.0.0.0", "8080"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

// rLaunchEntrypoint is the entrypoint argument out of a launch vector, found
// by position relative to -e rather than by a fixed index.
func rLaunchEntrypoint(t *testing.T, framework string) string {
	t.Helper()
	argv := RLaunchArgs(framework, "", "127.0.0.1", 1)
	for i, a := range argv {
		if a == "-e" {
			return argv[i+2]
		}
	}
	t.Fatalf("%s: argv has no -e: %q", framework, argv)
	return ""
}

// A Shiny app whose ui.R and server.R sit at the release root is started with
// the directory: runApp takes one there, and naming either file alone starts
// half an app.
func TestAnEmptyEntrypointFallsBackPerFramework(t *testing.T) {
	if got := rLaunchEntrypoint(t, "shiny"); got != "." {
		t.Errorf("shiny fallback entrypoint = %q, want \".\"", got)
	}
	if got := rLaunchEntrypoint(t, "plumber"); got != "plumber.R" {
		t.Errorf("plumber fallback entrypoint = %q, want plumber.R", got)
	}
}

// launch.browser = FALSE is not decoration: without it R tries to open a
// browser on a host that has neither one nor a display, and writes an error
// into the app's log at every start.
func TestTheShinyExpressionDisablesTheBrowser(t *testing.T) {
	if !strings.Contains(ShinyLaunchExpr, "launch.browser = FALSE") {
		t.Errorf("the Shiny expression does not disable the browser:\n%s", ShinyLaunchExpr)
	}
}

// The library path is stamped with the series, which is what makes the repair
// version-aware rather than an existence check.
func TestTheLibraryPathCarriesTheSeries(t *testing.T) {
	if got := RLibsSite("4.5"); got != "renv/library/R-4.5" {
		t.Errorf("RLibsSite(4.5) = %q, want renv/library/R-4.5", got)
	}
	// An unprobeable R is not a verdict. "" means the caller sets no
	// R_LIBS_SITE at all rather than pointing R at renv/library/R-.
	if got := RLibsSite(""); got != "" {
		t.Errorf("RLibsSite(\"\") = %q, want \"\"", got)
	}
}

// D13's version-aware repair, against the R this box actually has.
//
// A Python-shaped existence check would pass a library built against another R
// minor. That release starts, loads far enough to fail at dyn.load, and reports
// a missing shared object rather than a version — which is the failure this
// distinction exists to convert into a rebuild.
func TestAnRLibraryBuiltForAnotherSeriesIsUnrunnable(t *testing.T) {
	series := toolchainDefaultRSeries()
	if series == "" {
		t.Skip("no R on this host, so there is no series to judge a library against")
	}

	t.Run("a library for this series is runnable", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAllT(t, dir, RLibsSite(series))
		for _, framework := range []string{"shiny", "plumber"} {
			if bad, why := Unrunnable(framework, dir); bad {
				t.Errorf("%s: a library for R %s was rejected: %s", framework, series, why)
			}
		}
	})

	t.Run("a library for another series is not", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAllT(t, dir, "renv/library/R-0.1")
		for _, framework := range []string{"shiny", "plumber"} {
			bad, why := Unrunnable(framework, dir)
			if !bad {
				t.Errorf("%s: a library built for R 0.1 was accepted on R %s. It would start and "+
					"fail at dyn.load", framework, series)
			}
			if bad && why == "" {
				t.Errorf("%s: no reason given for the log", framework)
			}
		}
	})

	// DiscardUnusable removes the whole library root, not just the series that
	// was wanted: leaving the wrong one behind keeps a tree of unloadable
	// objects that no later step removes, growing the artifact on every repair.
	t.Run("the discard takes the whole library root", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAllT(t, dir, "renv/library/R-0.1")
		mkdirAllT(t, dir, RLibsSite(series))
		if err := DiscardUnusable("shiny", dir); err != nil {
			t.Fatal(err)
		}
		if dirExists(dir, "renv/library") {
			t.Error("renv/library survived the discard, so the next repair adds to it rather than replacing it")
		}
	})
}

func toolchainDefaultRSeries() string { return toolchain.DefaultSeries(toolchain.R) }

func mkdirAllT(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func dirExists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, rel))
	return err == nil
}

// Both build paths must produce the same tree, and for R that is structural
// rather than asserted: internal/builder calls RInstallRenvExpr, RRestoreExpr
// and RenvEnv out of this package instead of holding copies. Every other
// framework's install list is written twice — the runner binary must not import
// the builder — and held together by a test that the two match. R needs no such
// test because there is only one list, and this walks the half that is a
// sequence rather than a string.
func TestTheRBuildInstallsRenvBeforeRestoring(t *testing.T) {
	if toolchainDefaultRSeries() == "" {
		t.Skip("no R on this host")
	}
	dir := t.TempDir()
	boot := &BootstrapResponse{Framework: "shiny", RRepos: "https://example.invalid/cran"}

	// With no lockfile: renv is installed and nothing is restored. A repository
	// that names nothing installs nothing — the same answer internal/builder
	// gives, and not an error.
	steps, cleanup, err := buildSteps(boot, dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	// renv, then the SDK. Nothing is restored, because nothing names what to
	// install — the same answer internal/builder gives, and not an error.
	if len(steps) != 2 {
		t.Fatalf("with no renv.lock: %d steps, want 2 (renv, then the SDK)", len(steps))
	}
	if !strings.Contains(strings.Join(steps[0].args, " "), "install.packages(\"renv\"") {
		t.Errorf("the first step is not the renv install: %q", steps[0].args)
	}
	for _, st := range steps {
		if strings.Contains(strings.Join(st.args, " "), "renv::restore") {
			t.Error("a release with no renv.lock ran a restore")
		}
	}

	// With one: renv first, then the restore. The order is the point — renv is
	// the tool that reads the lockfile.
	if err := os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(`{"Packages":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, cleanup, err = buildSteps(boot, dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if len(steps) != 3 {
		t.Fatalf("with a renv.lock: %d steps, want 3 (renv, the SDK, the restore)", len(steps))
	}
	last := strings.Join(steps[len(steps)-1].args, " ")
	if !strings.Contains(last, "renv::restore") {
		t.Errorf("the last step is not the restore: %q", steps[len(steps)-1].args)
	}
	// The app's own packages come last, so a version it declares itself
	// replaces the platform's copy: a floor, not a ceiling.
	if !strings.Contains(strings.Join(steps[1].args, " "), "repos = NULL") {
		t.Errorf("the second step is not the SDK install: %q", steps[1].args)
	}

	// Every R step carries the environment that keeps the cache off. A pod has
	// no cache volume, and a restore run without this writes links into a
	// machine-global cache, so the repair that was supposed to fix the release
	// breaks it a second way.
	for i, st := range steps {
		var sawCacheOff bool
		for _, kv := range st.env {
			if kv == "RENV_CONFIG_CACHE_ENABLED=FALSE" {
				sawCacheOff = true
			}
		}
		if !sawCacheOff {
			t.Errorf("step %d runs without RENV_CONFIG_CACHE_ENABLED=FALSE: %q", i, st.args)
		}
	}
}

// Every expression that reaches out to a repository has to say which R is
// asking, or a binary repository answers with source.
//
// This is a whole-list test rather than three assertions because the list is
// the thing that goes wrong: a fourth install expression added later would be
// individually correct, would pass every other test in this package, and would
// fail only on an install whose TOFT_R_REPOS is a P3M snapshot — as a
// missing compiler, several minutes into a deploy, on an image that has no
// compiler on purpose. The failure names `make`, and nothing in it names the
// User-Agent that caused it.
func TestEveryRInstallExpressionIdentifiesRToTheRepository(t *testing.T) {
	for name, expr := range map[string]string{
		"RInstallRenvExpr": RInstallRenvExpr,
		"RRestoreExpr":     RRestoreExpr,
		"RInstallSDKExpr":  RInstallSDKExpr,
	} {
		if !strings.Contains(expr, "HTTPUserAgent") {
			t.Errorf("%s does not set HTTPUserAgent. P3M reads the R version out of the "+
				"User-Agent to decide whether src/contrib is answered with a binary or with "+
				"source, and R's own default is not the form it parses — so this expression "+
				"asks a binary repository for binaries and is handed source", name)
			continue
		}
		// The version is the interpreter's. A literal here would be a second
		// copy of TOFT_R_SERIES, free to drift from the R the image
		// installed, and wrong in the direction that still looks like it works.
		if !strings.Contains(expr, "getRversion()") {
			t.Errorf("%s writes an R version into the User-Agent rather than asking the "+
				"running interpreter for it", name)
		}
	}
}

// An R workload finds its packages through R_LIBS_SITE and through nothing
// else, and the isolated launcher has to set it for the same frameworks the
// shared one does.
//
// The gap this guards was not a wrong value, it was no value: shared mode set
// it in internal/runtime's PM2 launcher, isolated mode set it nowhere, and the
// two comments each said the other end was keeping it. An R app therefore ran
// as a process and died in a pod with `there is no package called 'shiny'` — on
// a release whose build log lists every package installing cleanly, which sends
// the reader to the build rather than to the environment.
func TestAnIsolatedRWorkloadIsPointedAtItsPackageLibrary(t *testing.T) {
	series := toolchainDefaultRSeries()
	if series == "" {
		t.Skip("no R on this host")
	}
	dir := t.TempDir()
	want := filepath.Join(dir, RenvLibraryRoot+"/R-"+series)

	// The four the build stamps a library for. `unrunnable` asks about the same
	// four, and a fifth R framework added to one list and not the other is a
	// workload that starts and cannot load itself.
	for _, framework := range []string{"shiny", "plumber", "rmarkdown", "quarto"} {
		boot := &BootstrapResponse{Framework: framework}
		var got string
		for _, kv := range appEnv(boot, &Client{}, "serve", dir) {
			if strings.HasPrefix(kv, "R_LIBS_SITE=") {
				got = strings.TrimPrefix(kv, "R_LIBS_SITE=")
			}
		}
		if got != want {
			t.Errorf("%s: R_LIBS_SITE is %q, want %q", framework, got, want)
		}
	}

	// And not for anything else: a Python app handed an R library path would be
	// harmless today and is still a variable nobody set on purpose.
	for _, kv := range appEnv(&BootstrapResponse{Framework: "fastapi"}, &Client{}, "serve", dir) {
		if strings.HasPrefix(kv, "R_LIBS_SITE=") {
			t.Errorf("a fastapi app was given %s", kv)
		}
	}

	// The app does not get to redirect its own library.
	boot := &BootstrapResponse{Framework: "shiny", Env: map[string]string{"R_LIBS_SITE": "/tmp/somewhere-else"}}
	for _, kv := range appEnv(boot, &Client{}, "serve", dir) {
		if kv == "R_LIBS_SITE=/tmp/somewhere-else" {
			t.Error("an app's own R_LIBS_SITE overrode the platform's")
		}
	}
}

// The workload starts under the R that built its library, not the host default.
//
// The launcher half of the resolved-interpreter bug, and it had exactly the
// same shape as the build half: on a host carrying one R the default *is* the
// series the release was stamped with, so both launchers could read
// toolchain.DefaultSeries and be right for as long as TOFT_R_PATH named a
// single interpreter. List a second and it comes apart three ways at once — a
// release built for 4.3 is started by 4.5, pointed at renv/library/R-4.5 which
// does not exist, and judged unrunnable by a repair that rebuilds 4.3 and asks
// again on the next start.
//
// So the assertions are about *disagreement*: a stamp that is not the default
// has to win, and one the host cannot satisfy has to refuse rather than fall
// back. A test that only checked a release stamped with the default would pass
// against every one of those three bugs.
func TestTheWorkloadStartsUnderTheRThatBuiltItsLibrary(t *testing.T) {
	if toolchainDefaultRSeries() == "" {
		t.Skip("no R on this host")
	}

	t.Run("the stamp is read back off the release", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAllT(t, dir, "renv/library/R-0.1")
		if got := RSeriesInRelease(dir); got != "0.1" {
			t.Errorf("RSeriesInRelease = %q, want 0.1 — the series the build stamped", got)
		}
		// Not the host default, which is the whole point: R_LIBS_SITE has to
		// name the directory that is actually there.
		if got := RLibsSiteFor(dir); got != "renv/library/R-0.1" {
			t.Errorf("RLibsSiteFor = %q, want renv/library/R-0.1, not the host default", got)
		}
	})

	t.Run("a release with no library falls back rather than naming R-", func(t *testing.T) {
		dir := t.TempDir()
		if got := RSeriesInRelease(dir); got != "" {
			t.Errorf("RSeriesInRelease on a release with no library = %q, want \"\"", got)
		}
		if got := RLibsSiteFor(dir); got != RLibsSite(toolchainDefaultRSeries()) {
			t.Errorf("RLibsSiteFor = %q, want the host default when there is no stamp to read", got)
		}
	})

	t.Run("a series this host does not carry resolves to no interpreter", func(t *testing.T) {
		// "" rather than the default, so Unrunnable can refuse with the version
		// in the message instead of handing the library to an R that will fail
		// at dyn.load one shared object at a time.
		if got := RExecutableForSeries("0.1"); got != "" {
			t.Errorf("RExecutableForSeries(0.1) = %q, want \"\" — never a fallback to the default", got)
		}
	})

	t.Run("the interpreter matches the stamp when the host has it", func(t *testing.T) {
		series := toolchainDefaultRSeries()
		dir := t.TempDir()
		mkdirAllT(t, dir, RLibsSite(series))
		want := RExecutableForSeries(series)
		if want == "" {
			t.Fatalf("the host reports R %s but names no interpreter for it", series)
		}
		if got := RLaunchBinary(dir); got != want {
			t.Errorf("RLaunchBinary = %q, want %q", got, want)
		}
	})

	t.Run("a library this host cannot load is unrunnable by version", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAllT(t, dir, "renv/library/R-0.1")
		bad, why := Unrunnable("shiny", dir)
		if !bad {
			t.Fatal("a library built for R 0.1 was accepted")
		}
		if !strings.Contains(why, "0.1") {
			t.Errorf("the reason is %q; it has to name the version, which is the fact "+
				"an operator acts on", why)
		}
	})
}

// A `runtime: shiny` document is detected as Shiny with the .Rmd as its
// entrypoint, and runApp cannot start a file that is not app.R — the document
// is started by rmarkdown::run, with the same three arguments (BB-002).
func TestAShinyDocumentStartsUnderRmarkdownRun(t *testing.T) {
	for _, name := range []string{"doc.Rmd", "Report.rmd"} {
		argv := RLaunchArgs("shiny", name, "127.0.0.1", 3100)
		want := []string{"--restore", "-e", ShinyDocumentLaunchExpr, name, "127.0.0.1", "3100"}
		if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("argv = %q, want %q", argv, want)
		}
	}
	if !strings.Contains(ShinyDocumentLaunchExpr, "rmarkdown::run(") ||
		!strings.Contains(ShinyDocumentLaunchExpr, "host = a[2]") ||
		!strings.Contains(ShinyDocumentLaunchExpr, "port = as.integer(a[3])") {
		t.Errorf("the document expression does not bind host and port: %s", ShinyDocumentLaunchExpr)
	}
	// app.R and the ui.R+server.R directory still start under runApp.
	for _, ep := range []string{"app.R", "."} {
		if got := RLaunchArgs("shiny", ep, "127.0.0.1", 3100)[2]; got != ShinyLaunchExpr {
			t.Errorf("%s started with %q", ep, got)
		}
	}
}

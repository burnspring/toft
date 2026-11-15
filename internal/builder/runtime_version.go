package builder

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/toolchain"
)

// Which language version a release is built against, and who decided.
//
// Three languages, and exactly one of them already knew how to do this. A Go
// module states the toolchain it needs in its own manifest and the `go` command
// fetches it. A Python project can state `requires-python` and a Node package
// can state `engines.node`, and nothing whatsoever reads either one — not this
// platform, and not the package managers in any enforcing default. The version
// an app got was a property of the machine it landed on.
//
// The failure that closes is not that a version is wrong. It is that nothing
// anywhere stated an expectation, so a mismatch could not be detected, reported
// or refused: it surfaced as a syntax error in a dependency three layers into a
// build log, or as a native module that would not load.
//
// **The decision is made during the deploy phase, before anything is built**,
// and that is not only better UX. The build always happens on the platform
// host, in both execution modes: by the time an isolated pod exists, the `venv`
// and `node_modules` in the artifact are already committed to an interpreter.
// A version decision made after the build is a decision about an artifact that
// is already wrong.

// resolvedRuntime is what a build settled on, and how it got there.
type resolvedRuntime struct {
	Language string
	// Chosen is the version the pin resolved to — the runner image's, on an
	// isolated deploy where the operator declared one.
	Chosen toolchain.Version
	// BuildWith is the interpreter on *this host* the build will actually use.
	// The same as Chosen in shared mode. In isolated mode it is the host
	// interpreter of the same release line, when there is one.
	BuildWith toolchain.Version
	Pin       string
	// Where names the set the pin was judged against, for the log line and for
	// a refusal.
	Where string
	// Declared is the series the *release* says it was built against, read from
	// renv.lock rather than written by anyone. Empty for every language but R,
	// and for an R release that states nothing.
	//
	// It is a preference and never a constraint, which is the whole of its
	// difference from Pin: a pin is a developer's sentence and is refused when
	// nothing satisfies it, and this is a fact about how the release was made.
	// Refusing on it would take out every Connect bundle an install deploys
	// today, over a declaration its author never wrote for this platform.
	Declared string
	// RequiresPython is pyproject.toml's `requires-python` when it chose the
	// interpreter: no pin, and it excluded the install default (ADR §64).
	RequiresPython string
	// Excluded is the install default it excluded, for the log line.
	Excluded string
	// Unsatisfied is why the pin matched nothing here, for Go alone: Go's is the
	// one unsatisfied pin that is not a refusal (D5), so the build goes ahead
	// and the go command fetches or refuses. Carried so a preflight can say so
	// before the build spends itself finding out.
	Unsatisfied string
	// DeclaredHonoured is whether the effective set actually had it. False with
	// a non-empty Declared is the case the log has to be loudest about: the
	// build fell back, and R's binary repositories publish per series, so it is
	// about to compile from source and may fail doing it.
	DeclaredHonoured bool
	// DeclaredNearest is whether, Declared not being there, the build took the
	// oldest series newer than it rather than the install default.
	DeclaredNearest bool
}

// nearestNewerSeries is the oldest version in set whose series is newer than
// declared — the closest R a lock built against a missing series can have.
func nearestNewerSeries(set []toolchain.Version, declared string) (toolchain.Version, bool) {
	var best toolchain.Version
	found := false
	for _, v := range set {
		if toolchain.Compare(v.Series(), declared) <= 0 {
			continue
		}
		if !found || toolchain.Compare(v.Series(), best.Series()) < 0 ||
			(v.Series() == best.Series() && toolchain.Compare(v.Version, best.Version) > 0) {
			best, found = v, true
		}
	}
	return best, found
}

// resolveRuntime settles a release's language version, or refuses the deploy.
//
// The effective set depends on the mode the release will *run* in, because a
// pin is a statement about the interpreter the app ends up executing under:
//
//   - **shared** — what this host has. The build host and the run host are the
//     same machine, so what is probed is what the app gets.
//   - **isolated** — what the runner image is declared to carry. The decision
//     is made before any pod exists, so it has to read a declaration; the
//     runner verifies its own image at bootstrap and refuses to start a
//     workload the declaration lied about (ADR §64 D17).
//
// An isolated install that has declared nothing is the one case with no set to
// judge against. It is not refused — that would break every install that has
// not yet set RUNNER_PYTHON_VERSION, over a variable added in the same release
// as the table it governs — but it is not silently approximated either: the pin
// is checked against the build host, the log says exactly that, and the runner
// is still the thing that catches a real divergence at start.
func (b *Builder) resolveRuntime(
	framework Framework, releaseDir string, mode runtime.Mode, appendLog func(string),
) (*resolvedRuntime, error) {
	pins, err := ParseRuntimePins(releaseDir)
	if err != nil {
		return nil, err
	}

	lang := LanguageOf(framework)
	if lang == "" {
		// A static app has no interpreter, no dependencies and no commands, so
		// a `[runtime]` table on one is a mistake rather than a no-op — the
		// same call as a declared job on a static app, and for the same reason:
		// accepting a declaration nothing can act on means the developer
		// believes something is in force that is not.
		if len(pins) > 0 {
			names := make([]string, 0, len(pins))
			for l := range pins {
				names = append(names, l)
			}
			return nil, ManifestError{Field: "[runtime]", Msg: fmt.Sprintf(
				"declares %s, and a static app has no interpreter to pin — it is a folder of "+
					"files served by the platform. Remove the [runtime] table, or deploy this "+
					"as an app in a language Toft supports",
				strings.Join(sortedStrings(names), " and "))}
		}
		return nil, nil
	}

	pin := pins[lang]
	if lang == toolchain.Go {
		// Go's declaration is go.mod's, and there is no second one to read.
		pin = GoModPin(releaseDir)
	}

	set, where := b.effectiveSet(lang, mode, appendLog)

	chosen, err := toolchain.Resolve(lang, pin, set, where)
	if err != nil {
		if lang == toolchain.Go {
			// Go is the exception, and it is Go's own property that makes it
			// one. With GOTOOLCHAIN=auto a build whose go.mod requires a newer
			// toolchain downloads that exact toolchain from the module proxy
			// and re-execs into it — including versions released after this
			// image was built. With `local` the build gets Go's own refusal,
			// which already names both versions: "go.mod requires go >= 1.27.0;
			// running go 1.26.5". That is a better message than one we would
			// write, so we do not write one (D5).
			appendLog(fmt.Sprintf("    %v", err))
			appendLog("    Leaving it to the go command: with GOTOOLCHAIN=auto it will fetch " +
				"the toolchain go.mod asks for, and with GOTOOLCHAIN=local it will refuse and " +
				"name both versions.")
			return &resolvedRuntime{Language: lang, Pin: pin, Where: where, Unsatisfied: err.Error()}, nil
		}
		return nil, err
	}

	// **A release that states which R it was built against gets that R**, when
	// this install has one — and no `toft.toml` to hand-write, which is
	// the point: the declaration is already in the release, in the file the
	// platform itself writes for a Connect bundle.
	//
	// Applied here rather than passed into Resolve as a pin, so that Resolve
	// keeps meaning exactly one thing. A pin that nothing satisfies is a
	// refused deploy; a *declaration* that nothing satisfies is a fallback and
	// a loud line in the log. Folding the second into the first would either
	// start refusing bundles that deploy today or quietly weaken the refusal
	// the first one exists to be.
	//
	// An explicit `[runtime] r` wins, and is why this is guarded on an empty
	// pin: one declaration, and between a developer's sentence and an inference
	// from a lockfile the sentence is the one that was written on purpose.
	declared := ""
	if lang == toolchain.R && pin == "" {
		declared = RDeclaredSeries(releaseDir)
	}
	honoured := false
	for _, v := range set {
		if declared != "" && v.Series() == declared {
			chosen, honoured = v, true
			break
		}
	}
	// Not there: the nearest *newer* series, not the install default. A lock
	// pinned to R 4.3 on a host with 4.4, 4.5 and 4.6 compiles its packages
	// from source whichever it gets, and two-year-old source compiles against
	// the next R's headers far more often than against the newest — stringi
	// 1.8.3 builds on 4.4 and fails on 4.6, where LEVELS left the API.
	nearest := false
	if declared != "" && !honoured {
		if v, ok := nearestNewerSeries(set, declared); ok {
			chosen, nearest = v, true
		}
	}

	res := &resolvedRuntime{
		Language: lang, Chosen: chosen, BuildWith: chosen, Pin: pin, Where: where,
		Declared: declared, DeclaredHonoured: honoured, DeclaredNearest: nearest,
	}
	if lang == toolchain.Python && pin == "" {
		if err := res.followRequiresPython(releaseDir, set); err != nil {
			return nil, err
		}
		chosen = res.Chosen
	}

	// In isolated mode the chosen version is a declaration with no path on this
	// machine, and the build still happens here. Find the host interpreter of
	// the same release line to build against; without one, the venv or the
	// native modules in the artifact belong to a different interpreter than the
	// pod will run, which is what `repairRelease` exists to notice and rebuild.
	if chosen.Path == "" {
		res.BuildWith = toolchain.Version{}
		for _, host := range toolchain.Available(lang) {
			if toolchain.MatchSeries(lang, host.Version, chosen.Version) {
				res.BuildWith = host
				break
			}
		}
	}
	return res, nil
}

// EffectiveVersions is the set a pin would be judged against for a release
// running in mode — what `GET /me/capabilities` reports, read from the same
// function a deploy resolves against so the two cannot disagree.
func EffectiveVersions(lang string, mode runtime.Mode) []toolchain.Version {
	var b Builder
	set, _ := b.effectiveSet(lang, mode, func(string) {})
	return set
}

// effectiveSet is the versions a pin is judged against, and the words for it.
func (b *Builder) effectiveSet(lang string, mode runtime.Mode, appendLog func(string)) ([]toolchain.Version, string) {
	if mode != runtime.ModeIsolated {
		return toolchain.Available(lang), "this build host"
	}
	if declared := toolchain.Configured(lang); len(declared) > 0 {
		return declared, fmt.Sprintf("the %s runner image", lang)
	}
	if lang == toolchain.Go {
		// Go never reaches an isolated runner as a version: `go build` produces
		// a binary and the workload execs that. The build host is the only
		// place its version is a fact.
		return toolchain.Available(lang), "this build host"
	}
	appendLog(fmt.Sprintf("    Note: this install has not declared what the %s runner image "+
		"carries (RUNNER_%s_VERSION), so the version below is this build host's. Set it and "+
		"the pin will be judged against the image the app actually runs in.",
		lang, strings.ToUpper(lang)))
	return toolchain.Available(lang), "this build host"
}

// report writes the resolved version to the build log and to the deployment
// row, on every build, for every language, pinned or not (D2).
//
// Without it, an older platform's silent ignoring of the `[runtime]` table is
// undetectable — toft.toml ignores any table its readers do not name, so
// an app deploys against the wrong interpreter, behaves exactly as it did
// before, and the developer believes they have pinned it. A pin that was not
// honoured has to be visible in the log of the deploy that ignored it.
func (r *resolvedRuntime) report(appendLog func(string)) {
	if r == nil {
		return
	}
	label := toolchain.Label(r.Language)

	if r.Chosen.Version == "" {
		// Only Go reaches here: nothing installed satisfies go.mod and the go
		// command has been left to settle it.
		appendLog(fmt.Sprintf("==> Building against %s: left to the go command", label))
		return
	}

	how := "(install default)"
	if r.Pin != "" {
		how = fmt.Sprintf("(satisfies %s)", r.Pin)
	} else if r.RequiresPython != "" {
		how = fmt.Sprintf("(pyproject.toml's requires-python %q excludes the install default, %s)",
			r.RequiresPython, r.Excluded)
	} else if r.DeclaredHonoured {
		how = fmt.Sprintf("(renv.lock was built against R %s)", r.Declared)
	} else if r.DeclaredNearest {
		how = fmt.Sprintf("(the nearest series newer than renv.lock's R %s)", r.Declared)
	}
	appendLog(fmt.Sprintf("==> Building against %s %s %s", label, r.Chosen.Version, how))

	// The fallback, said in full. This is the line that explains the whole
	// shape of an R failure an operator is otherwise left to derive from a C
	// compiler error forty packages later: a binary repository publishes a
	// package version only for the R series it was current in, so a lockfile
	// pinned to an older R is served *source* to a newer one, and source from
	// two years ago frequently will not compile against today's R headers.
	if r.Declared != "" && !r.DeclaredHonoured {
		used := "the default above"
		if r.DeclaredNearest {
			used = "the nearest newer series above"
		}
		appendLog(fmt.Sprintf("    renv.lock was built against %s %s, and %s has none, so this "+
			"build used %s.", label, r.Declared, r.Where, used))
		// The remedy names the lever for the *mode*, because they are different
		// levers and the wrong one sends an operator to edit a file that will
		// not be read. Shared mode builds on this host, so the host is where
		// the interpreter goes; isolated mode judges against what the runner
		// image is declared to carry, and no amount of TOFT_R_PATH on the
		// platform changes that.
		remedy := fmt.Sprintf("Install %s %s on this host and name it in TOFT_R_PATH.",
			label, r.Declared)
		if strings.Contains(r.Where, "runner image") {
			remedy = fmt.Sprintf("The runner image would have to carry %s %s and declare it in "+
				"RUNNER_%s_VERSION.", label, r.Declared, strings.ToUpper(r.Language))
		}
		appendLog(fmt.Sprintf("    Package versions pinned to an older R are published as source "+
			"for a newer one, so this build may compile rather than install binaries — and may "+
			"fail doing it. %s", remedy))
	}

	if r.BuildWith.Version == "" {
		appendLog(fmt.Sprintf("    This host has no %s %s, so the release is built against its "+
			"default and the runner rebuilds the dependency tree in the pod on first start.",
			label, r.Chosen.Series()))
	} else if r.BuildWith.Version != r.Chosen.Version {
		appendLog(fmt.Sprintf("    Built on this host with %s.", r.BuildWith.String()))
	}

}

// record writes the resolved version to the deployment row. Separate from
// report because the two have different failure modes: a log line cannot fail,
// and a write that does must not take the build down with it — a build never
// fails because the platform could not describe it.
func (r *resolvedRuntime) record(b *Builder, deployID string, appendLog func(string)) {
	if r == nil || r.Chosen.Version == "" {
		return
	}
	if err := b.db.SetDeploymentRuntime(deployID, r.Language, r.Chosen.Version); err != nil {
		appendLog(fmt.Sprintf("    Warning: could not record the resolved version: %v", err))
	}
}

// goToolchainOf reads the toolchain that actually compiled a binary, or "".
//
// The binary's own stamp rather than the go command's view of itself: after a
// Go build those are not reliably the same fact (confirmGoToolchain).
//
// **Read in-process, through the release's os.Root, never by exec.** This used
// to be `go version <binary>` exec'd as the platform with the build's
// environment, the one build-phase command that bypassed the privilege drop —
// and with an app-chosen GOTOOLCHAIN or PATH the go command could re-exec a
// toolchain the app supplied, as root (security review 2026-11-10, E-1b).
// debug/buildinfo reads the same stamp from a file opened the way every other
// release file is (openInRelease: no link out of the tree, regular files
// only), so nothing the app controls runs and nothing outside it is read.
//
// Every failure is "" — a missing binary, one with no stamp — because nothing
// here may fail a build that worked.
func goToolchainOf(dir, name string) string {
	f, err := openInRelease(dir, name)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := buildinfo.Read(f)
	if err != nil {
		return ""
	}
	return toolchain.GoStamp(info.GoVersion)
}

// confirmGoToolchain corrects the deployment row with the toolchain that
// actually compiled the binary, and it is the only place in this section where
// the platform revises its own answer after the fact.
//
// **The resolution and the artifact can legitimately disagree, and only for
// Go.** `resolveRuntime` runs before the build and records what it resolved,
// which is what D14 asks for and is right for every other family: uv is handed
// `--python <path>`, npm is the one beside the resolved node, and R's library is
// stamped with the series that built it. The go command is the exception — with
// `GOTOOLCHAIN` naming a toolchain it re-execs into that one, *after* the
// platform has finished deciding, so the row said 1.27.0 for an app whose binary
// reports go1.25.14 and the estate census counted it under a version it does not
// run. Measured on this platform's own k3s install before it was fixed.
//
// **It also closes the case where nothing was recorded at all.** `record`
// declines to write when the resolver left the decision to the go command —
// there was no version to write — so those apps were permanently "unresolved"
// in the census even though the artifact demonstrably used *some* toolchain.
// The binary knows, and now the row does.
//
// The *log* keeps both facts rather than replacing one with the other: the line
// before the build says what the platform resolved, and this says what came out.
// A build whose toolchain did not move says nothing extra, because a line that
// appears on every build is a line nobody reads.
//
// Not fatal, ever. A build never fails because the platform could not describe
// it — the same rule `record` states — and a row left holding the resolved
// version is exactly the behaviour that stood before this existed.
func (b *Builder) confirmGoToolchain(deployID, releaseDir string, rt *resolvedRuntime, lg buildLog) {
	built := goToolchainOf(releaseDir, runtime.GoBinaryName)
	if built == "" || built == rt.Chosen.Version {
		return
	}
	if rt.Chosen.Version == "" {
		lg.stepf("    Built by Go %s, which the go command chose for itself.", built)
	} else {
		lg.stepf("    Built by Go %s rather than the %s above: GOTOOLCHAIN sent the go "+
			"command into another toolchain, and the binary is what it says it is.",
			built, rt.Chosen.Version)
	}
	// A repair has no deployment to correct -- it is handed a spec by the
	// runtime rather than a build job -- and the row it would write to belongs
	// to a deploy that finished long ago.
	if deployID == "" {
		return
	}
	if err := b.db.SetDeploymentRuntime(deployID, toolchain.Go, built); err != nil {
		lg.output(fmt.Sprintf("    (the toolchain that built the binary could not be "+
			"recorded: %v)", err))
	}
}

// apply puts the resolved interpreter into the build environment.
//
// Python takes it by argument — `uv venv --python <path>` — because uv accepts
// one and an explicit path cannot be shadowed. Node and Go take it by PATH,
// because `npm` and `go` have no equivalent flag and the tool beside the
// interpreter is the one that must be used: an alternate Node install's `npm`
// lives in the same bin directory as its `node`, and running the default `npm`
// against a different `node` is the mismatch this exists to prevent.
func (r *resolvedRuntime) apply(env map[string]string) {
	if r == nil || r.BuildWith.Path == "" {
		return
	}
	if r.Language == toolchain.Python {
		// Passed as an argument at the call site; nothing to do here.
		return
	}
	dir := filepath.Dir(r.BuildWith.Path)
	existing := env["PATH"]
	if existing == "" {
		existing = os.Getenv("PATH")
	}
	env["PATH"] = dir + string(os.PathListSeparator) + existing
}

// tool is the build tool that belongs beside the resolved interpreter — `npm`
// for the Node this build settled on, rather than whichever one the platform's
// own PATH finds first.
//
// **`apply` above is not enough on its own, and the reason is a Go detail with
// a real failure behind it**: exec.Command resolves a bare name against the
// parent's PATH, not against cmd.Env, so the prefix `apply` writes decides what
// npm's shebang resolves `node` to and does not decide which npm runs. The
// first Node 20 deploy this platform accepted ran npm 12 out of /usr/local/bin
// on Node 20.20.2 and said so in a warning, in a log that had just reported the
// right version twice.
//
// Python needs none of this — uv is one binary for every interpreter and takes
// `--python` — and R never did, because rBuildBinary has passed an absolute
// interpreter since it was written.
func (r *resolvedRuntime) tool(name string) string {
	if r == nil || r.BuildWith.Path == "" {
		return name
	}
	return runner.ToolBeside(filepath.Dir(r.BuildWith.Path), name)
}

// nodeVersion is the interpreter the build runs, for a key that must change
// when it does. Empty when nothing was resolved (the host's default node).
func (r *resolvedRuntime) nodeVersion() string {
	if r == nil {
		return ""
	}
	return r.BuildWith.Version + "@" + r.BuildWith.Path
}

// pythonArgs is the `--python <path>` uv takes.
//
// **Passed even when the resolved interpreter is this host's default**, and
// that is not belt-and-braces — it is the whole point. `uv venv` with no
// `--python` does not simply take `python3` off `PATH`: it runs its own
// discovery, which enumerates versioned binaries and can prefer a
// uv-managed standalone build it downloaded earlier. On this box, with
// `/usr/bin/python3` resolving to 3.13, a bare `uv venv venv` produced a
// **3.12** environment — the platform's build log said one version and the
// running app reported another, which is exactly the class of silent mismatch
// this whole feature exists to close.
//
// So the platform supplies the whole description here too. An explicit path
// cannot be shadowed by somebody else's discovery order.
func (r *resolvedRuntime) pythonArgs() []string {
	if r == nil || r.Language != toolchain.Python || r.BuildWith.Path == "" {
		return nil
	}
	return []string{"--python", r.BuildWith.Path}
}

// rSeries is the series the R library is built for and stamped with, which both
// launchers then name in R_LIBS_SITE.
//
// BuildWith rather than Chosen, and the distinction is the one pythonArgs draws
// above: in isolated mode Chosen is a *declaration* about the runner image with
// no path here, while the build happens on this host. The library has to be
// stamped with the R that will load it, which is the runner's — so Chosen when
// there is one, and the host interpreter that matched it otherwise.
func (r *resolvedRuntime) rSeries() string {
	if r == nil || r.Language != toolchain.R {
		return ""
	}
	if r.Chosen.Version != "" {
		return r.Chosen.Series()
	}
	if r.BuildWith.Version != "" {
		return r.BuildWith.Series()
	}
	return toolchain.DefaultSeries(toolchain.R)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// followRequiresPython moves an unpinned Python build off the install default
// when pyproject.toml's `requires-python` excludes it, onto the newest
// interpreter in set that it allows (ADR §64).
//
// **Only when the default is excluded.** `requires-python` is written for PyPI,
// where `>=3.8` means "not older"; obeying it outright would hand every such app
// the install's newest interpreter forever. Consulted only against the default,
// a lower bound never moves anything, and a project saying `<3.13` or `==3.12.*`
// gets the version it said rather than a warning about the one it did not.
//
// Nothing in set satisfying it is a refusal — a *RequiresPythonError, which
// carries the default so a dependency repair of a release already running can
// carry on with it rather than stop the app. An unreadable specifier is a
// guess this declines to make: the default stands.
func (r *resolvedRuntime) followRequiresPython(releaseDir string, set []toolchain.Version) error {
	spec := requiresPython(releaseDir)
	if spec == "" || r.Chosen.Version == "" {
		return nil
	}
	if allowed, ok := toolchain.PEP440Allows(spec, r.Chosen.Version); !ok || allowed {
		return nil
	}
	var best toolchain.Version
	for _, v := range set {
		if a, ok := toolchain.PEP440Allows(spec, v.Version); ok && a {
			if best.Version == "" || toolchain.Compare(v.Version, best.Version) > 0 {
				best = v
			}
		}
	}
	if best.Version == "" {
		return &RequiresPythonError{Spec: spec, Available: set, Where: r.Where, Default: r}
	}
	r.Excluded = r.Chosen.Version
	r.Chosen, r.BuildWith, r.RequiresPython = best, best, spec
	return nil
}

// RequiresPythonError is `requires-python` that nothing in the effective set
// satisfies, with no `[runtime] python` to say otherwise.
type RequiresPythonError struct {
	Spec      string
	Available []toolchain.Version
	Where     string
	// Default is the resolution on the install default, for the one caller
	// that must not refuse: repairing a release that is already deployed.
	Default *resolvedRuntime
}

func (e *RequiresPythonError) Error() string {
	have := make([]string, 0, len(e.Available))
	for _, v := range e.Available {
		have = append(have, v.Series())
	}
	return fmt.Sprintf("pyproject.toml's requires-python %q allows none of the Python versions %s has "+
		"(%s). Widen requires-python, or pin one of those with [runtime] python = \"...\" in %s",
		e.Spec, e.Where, strings.Join(have, ", "), ManifestFile)
}

// pinExcludedByRequiresPython is toft check's warning for a [runtime] python
// pin that pyproject.toml's own requires-python excludes — two files of one
// repository disagreeing, which check passed without a word (BB-029). The pin
// wins, as it always has; the warning says the project claims it cannot run
// on what it will get.
func pinExcludedByRequiresPython(srcDir string, rt *resolvedRuntime) (Warning, bool) {
	if rt == nil || rt.Language != toolchain.Python || rt.Pin == "" || rt.Chosen.Version == "" {
		return Warning{}, false
	}
	spec := requiresPython(srcDir)
	if spec == "" {
		return Warning{}, false
	}
	if allowed, ok := toolchain.PEP440Allows(spec, rt.Chosen.Version); !ok || allowed {
		return Warning{}, false
	}
	return Warning{Code: WarningPinExcluded, Message: fmt.Sprintf("[runtime] python = %q builds on Python %s, "+
		"which pyproject.toml's requires-python %q excludes — change one so they agree",
		rt.Pin, rt.Chosen.Version, spec)}, true
}

// requiresPython is `[project] requires-python` from the release's
// pyproject.toml, or "" when there is none or the file does not parse.
func requiresPython(releaseDir string) string {
	data, err := readInRelease(releaseDir, "pyproject.toml")
	if err != nil {
		return ""
	}
	var p struct {
		Project struct {
			RequiresPython string `toml:"requires-python"`
		} `toml:"project"`
	}
	if _, err := toml.Decode(string(data), &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Project.RequiresPython)
}

// nodeVersionUnread is the warning for a Node version stated where Toft does not
// look. `engines.node` and `.nvmrc` are what a Node developer writes first, and
// nothing reads either (the comment at the top of this file says why the
// platform does not start): a tree pinned only there builds on the install
// default and nothing said so — a pin that silently does not exist, which is
// the one outcome ParseRuntimePins refuses for its own table. Said only when
// `[runtime] node` is absent, because then the statement is the only one made.
func nodeVersionUnread(releaseDir string, rt *resolvedRuntime) (Warning, bool) {
	if rt == nil || rt.Language != toolchain.Node || rt.Pin != "" {
		return Warning{}, false
	}
	var stated []string
	if raw, err := readInRelease(releaseDir, "package.json"); err == nil {
		var pkg struct {
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
		}
		if json.Unmarshal(raw, &pkg) == nil && strings.TrimSpace(pkg.Engines.Node) != "" {
			stated = append(stated, fmt.Sprintf("package.json engines.node (%q)", strings.TrimSpace(pkg.Engines.Node)))
		}
	}
	for _, name := range []string{".nvmrc", ".node-version"} {
		if raw, err := readInRelease(releaseDir, name); err == nil && strings.TrimSpace(string(raw)) != "" {
			stated = append(stated, fmt.Sprintf("%s (%q)", name, strings.TrimSpace(string(raw))))
		}
	}
	if len(stated) == 0 {
		return Warning{}, false
	}
	verb := "is"
	if len(stated) > 1 {
		verb = "are"
	}
	return Warning{Code: WarningNodeVersionUnread, Table: "[runtime] node", Message: fmt.Sprintf(
		"%s %s not read by Toft — this build gets Node.js %s, the install default. Pin it "+
			"in %s: [runtime] node = \"<major>\"",
		strings.Join(stated, " and "), verb, rt.Chosen.Version, ManifestFile)}, true
}

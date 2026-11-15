package builder

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"toft/internal/applog"
	"toft/internal/deps"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/toolchain"
	"toft/sdk"
)

// Installing a release's dependencies, and the one other caller of it.
//
// This is step 5 of a build, lifted out of `execute` for a second caller that is
// not a build at all: the *repair* of a release whose dependency tree can no
// longer run on this host. The two must produce the same tree — an app that
// behaves differently depending on which of them last touched it is the failure
// both exist to prevent — and the cheapest guarantee of that is that there is
// only one of them.

// installDependencies runs the framework's build: dependencies installed, and
// whatever compilation or bundling the framework needs.
//
// It reports an error instead of failing the deploy, because its second caller
// has no deployment to fail. `execute` turns the error back into exactly the
// message it used to pass to fail().
//
// deployID reaches runCommand and is unused by it today; it is threaded through
// rather than dropped so that a future per-deploy concern (a cancellation, a
// per-build cache) has the same handle here that the rest of the build has. A
// repair passes "".
// Repair rebuilds the release at dir when its dependency tree cannot run on this
// host, and does nothing at all when it can. It is internal/runtime's Repairer,
// and the shared execution backend's half of a repair the isolated one has always
// had.
//
// **Why the shared side needs one at all.** A Python release's `venv/bin/python`
// is a symlink to an absolute path *outside* the release tree. Move the
// platform's interpreter — an image upgrade, an operator changing
// TOFT_PYTHON_PATH — and the link dangles, while the release itself is
// unchanged and is restored byte-for-byte on the next start. So the stale venv
// comes back on every restart, every wake and every node move, and until this
// existed the only cure was redeploying each app by hand. The isolated side
// healed itself on the next pod start and printed a line saying so; shared mode
// resolved the release and exec'd it.
//
// It is worth having independently of any upgrade: it is the same answer for an
// install that crosses a 3.11/3.12 boundary, and it is the precondition for ever
// moving Node's major, where nothing can *detect* the equivalent breakage.
//
// Three things it deliberately does not do:
//
//   - **It does not re-run the dependency gate.** A restart is not a deploy and
//     the verdict is the release's, not today's: the packages installed here are
//     the ones the release already carries and was already judged on. The
//     isolated path's rebuild does consult a verdict, and it is the same one —
//     carried in that release's bootstrap response, not re-derived.
//   - **It writes nothing to the deployment row.** `report` says which version
//     it built against, in the platform log; `record` is the build's.
//   - **It approximates nothing.** The interpreter is resolved by exactly the
//     path a deploy uses, so a release pinned to a version this host no longer
//     has *fails the start*, naming the pin and the set, instead of being quietly
//     rebuilt against something else. That is the same refusal the deploy would
//     give, arriving at the moment the host changed rather than at the next
//     deploy.
func (b *Builder) Repair(spec runtime.WorkloadSpec, dir string) error {
	broken, what := runner.Unrunnable(spec.Framework, dir)
	if !broken {
		return nil
	}

	// The cache's read side, for the same reason execute takes it — and this is
	// the caller that is easy to miss. A repair is not a deploy and is not
	// queued: it runs when the shared backend starts a workload whose release
	// is unrunnable, which can be boot reconciliation, an idle wake or a
	// restart. Nobody watching the build queue would call any of those "a build
	// is running", which is exactly why a reclaim that only excluded builds
	// would delete out from under one.
	//
	// Taken after the Unrunnable check so that the overwhelmingly common case —
	// a healthy release, nothing to repair — does not touch the lock at all.
	//
	// Not taken when the caller holds it already (spec.BuildHoldsCache): a
	// rollback's start runs here from inside execute, on execute's goroutine,
	// and a reclaim queued between the two read locks waits on the outer one
	// while the inner one waits on the reclaim (PB-3).
	if !spec.BuildHoldsCache {
		b.cacheMu.RLock()
		defer b.cacheMu.RUnlock()
	}

	// As the app's own account, like a build (identity.go). This is the half
	// that matters most: the tree being repaired is one the app has owned since
	// its last start, so whatever is in it now is the app's to have written —
	// and before this, the platform ran its build backend as root.
	_, done, err := b.beginBuildAs(spec.Slug, dir)
	if err != nil {
		return err
	}
	defer done()

	// Two registers, and the split is the point.
	//
	// `output` is what npm, uv and go print, and it stays exactly where it has
	// always been: the operator's process log, one line at a time, for however
	// many thousand lines an install takes. There is no deployment to stream it
	// to — a repair is a start, not a build — and it is the app's own voice
	// about its own dependencies rather than the platform's.
	//
	// `step` is the stage marks, and it now goes to both: the operator's log as
	// before, and the app's platform log, which is the register its owner
	// reads. This is the shared-mode counterpart of the isolated runner's
	// `==> Repairing the release`, which an owner has been able to see since
	// the day the runner was built.
	output := func(line string) { log.Printf("repair: %s: %s", spec.Slug, strings.TrimSpace(line)) }
	step := func(line string) {
		output(line)
		b.narrator.For(spec.AppID, spec.ProcessName(), "").Say(applog.StreamStdout, strings.TrimSpace(line))
	}
	lg := buildLog{step: step, output: output}

	lg.stepf("==> Repairing the release: %s", what)

	// Detection off the directory rather than off the spec, for the reason
	// Builder.startOn re-detects there too: the shared backend has the tree, and
	// the tree is the honest answer for a release built before a column existed.
	// It is also the only source of HasBuild, which decides whether a Node
	// release runs its own build script.
	// Read as the account: these are the app's files, and one of them could be
	// a link to something only root can read.
	var result DetectionResult
	_ = b.asBuild(dir, func() error { result = DetectFramework(dir); return nil })
	if result.Framework == FrameworkUnsupported {
		return fmt.Errorf("this release needs its dependencies rebuilt (%s) and its framework "+
			"can no longer be detected, so there is nothing to rebuild it with. Redeploy the app",
			what)
	}

	if err := b.asBuild(dir, func() error { return runner.DiscardUnusable(string(result.Framework), dir) }); err != nil {
		return err
	}

	// ModeShared is stated rather than derived: this is the shared backend's
	// repairer and nothing else wires it. An isolated workload repairs itself
	// inside its own pod, against its own image, which is the only place that
	// answer can be correct.
	resolvedRT, err := b.resolveRuntime(result.Framework, dir, runtime.ModeShared, lg.step)
	var unsatisfied *RequiresPythonError
	if errors.As(err, &unsatisfied) {
		// A deploy refuses this; a repair is of a release already running,
		// and stopping it over a declaration is worse than the default.
		lg.step("    Warning: " + err.Error())
		resolvedRT, err = unsatisfied.Default, nil
	}
	if err != nil {
		return err
	}
	resolvedRT.report(lg.step)

	buildEnv := b.buildEnvironment(spec.Env, spec.BasePath)
	if dropped := toolchain.InstallerConfigNames(spec.Env); len(dropped) > 0 {
		lg.step("    Not passed to the build: " + strings.Join(dropped, ", "))
	}
	resolvedRT.apply(buildEnv)

	// Repair has no app row to read an override from — it is handed a spec by
	// the runtime, not a deploy — so it takes the install's default. A repair
	// is also not a deploy: it restores what dangles rather than doing the
	// work the override was raised for.
	return b.installDependencies("", result, dir, spec.BasePath,
		spec.Env, buildEnv, resolvedRT, b.buildTimeout(nil), lg)
}

// requirementsInstallArgs is the uv command that installs a release's
// requirements.txt, and whether it is hashed.
//
// **A hashed file is installed with --require-hashes** (toolchain.
// HashedRequirements). pip turns hash-checking on by itself for a file that
// carries hashes and then refuses anything unlisted; uv does not, and installs
// an unlisted dependency unpinned and unhashed. The flag is pip's semantics,
// which the file's author wrote for, and it is what lets the dependency scan
// read the file as the whole inventory — the two move together or the SBOM
// claims a set the build did not install. It is not a policy: it applies with
// the dependency policy off, because it concerns what the install does.
//
// **Only on a deploy.** A repair — here or in the runner's pod — installs a
// release already built, and for one built before this rule it would turn an
// ordinary restart into a failed start. Every release built since has a
// complete file, because an incomplete one failed its build; without the flag
// uv still checks the hashes listed, so a repair installs the same set.
func requirementsInstallArgs(releaseDir string, deploy bool) ([]string, bool, error) {
	args := []string{"pip", "install", "-r", "requirements.txt", "--python", "venv/bin/python"}
	if !deploy {
		return args, false, nil
	}
	hashed, err := toolchain.HashedRequirements(filepath.Join(releaseDir, "requirements.txt"))
	if err != nil || !hashed {
		return args, false, err
	}
	return []string{"pip", "install", "--require-hashes", "-r", "requirements.txt", "--python", "venv/bin/python"}, true, nil
}

// buildLog is the two registers a build writes in, and keeping them apart is
// the whole of D4.
//
// step is the platform's own stage marks: the `==>` lines, the interpreter it
// resolved, the warnings it attaches. output is what a build command printed —
// npm's, uv's, go's — which is the *app's* voice about the app's own
// dependencies, and which arrives one line at a time for as long as the install
// takes.
//
// They are the same function on the deploy path, where both go to the
// deployment's build log and an owner reads them together as one document.
// They differ on the repair path, where the stage marks reach the app's
// platform log and the subprocess stream stays in the operator's journal —
// because a repair is a start, a start's narration is stage marks and never a
// stream, and `uv pip install -r requirements.txt` is a thousand lines.
//
// This is the split the isolated runner has always made: it narrates `==>` with
// shipper.Logf as `platform` and pumps each build step's output as `app`.
type buildLog struct {
	step   func(string)
	output func(string)
}

func (l buildLog) stepf(format string, args ...any) { l.step(fmt.Sprintf(format, args...)) }

// sameBoth is the deploy path's arrangement: one document, both voices.
func sameBoth(appendLog func(string)) buildLog {
	return buildLog{step: appendLog, output: appendLog}
}

func (b *Builder) installDependencies(
	deployID string,
	result DetectionResult,
	releaseDir, basePath string,
	envVars, buildEnv map[string]string,
	resolvedRT *resolvedRuntime,
	timeout time.Duration,
	lg buildLog,
) error {
	switch result.Framework {
	case FrameworkNextJS:
		if !result.HasBuild {
			return errors.New("No 'build' script found in package.json")
		}
		lg.step("==> Configuring base path...")
		if err := b.asBuild(releaseDir, func() error { return injectNextBasePath(releaseDir, basePath) }); err != nil {
			return fmt.Errorf("configuring Next.js basePath: %v", err)
		}
		fallthrough

	case FrameworkNode, FrameworkBatchNode:
		// Before npm reads the project's .npmrc with the operator's credential
		// in its environment — see toolchain.CheckProjectNPMRC.
		if err := toolchain.CheckProjectNPMRC(releaseDir); err != nil {
			return err
		}
		install, installArgs := b.npmInstallCommand(releaseDir)
		// A Yarn or pnpm lockfile is passed over — npm installs — and the
		// build log says so, whether or not the dependency scan runs (BB-027).
		for _, line := range passedOverLockLines(releaseDir) {
			lg.step(line)
		}
		// The npm beside the Node this build resolved, by absolute path. See
		// resolvedRuntime.tool: the PATH prefix decides what npm's own shebang
		// finds and cannot decide which npm is started.
		install = resolvedRT.tool(install)
		// A deploy whose install inputs match an earlier build's copies that
		// build's node_modules instead (depreuse.go). Never a repair: a repair
		// exists because the tree on disk stopped running here.
		depsKey := ""
		if deployID != "" {
			depsKey, _ = nodeDepsKey(releaseDir, resolvedRT.nodeVersion(), installArgs, buildEnv)
		}
		if !b.reuseDeps(deployID, releaseDir, depsKey, "node_modules", lg) {
			lg.stepf("==> Running %s %s...", install, strings.Join(installArgs, " "))
			if !b.opts.AllowBuildScripts {
				lg.step("    (dependency lifecycle scripts disabled — set BUILD_ALLOW_SCRIPTS=1 to enable)")
			}
			if err := b.runCommandFor(timeout, deployID, releaseDir, install, installArgs, buildEnv, lg.output); err != nil {
				if strings.Contains(err.Error(), npmLockOutOfSync) {
					// Said last, so the log's tail and the failure lead with
					// the cause rather than npm's usage text (BB-028).
					why := "package.json and package-lock.json are out of sync, so npm ci refused to install — " +
						"run npm install and commit the package-lock.json it writes"
					lg.step("==> " + why)
					return fmt.Errorf("%s failed: %s\n%v", install, why, err)
				}
				return fmt.Errorf("%s failed: %v", install, err)
			}
			// Before the SDK goes in, so what is kept is exactly what the
			// install produced.
			b.keepDeps(deployID, releaseDir, depsKey, "node_modules", lg)
		}
		// After the install, never before: `npm ci` empties node_modules.
		_ = b.asBuild(releaseDir, func() error { installNodeSDK(releaseDir, lg); return nil })
		if result.HasBuild {
			lg.step("==> Running npm run build...")
			if err := b.runCommandFor(timeout, deployID, releaseDir, resolvedRT.tool("npm"), []string{"run", "build"}, buildEnv, lg.output); err != nil {
				return fmt.Errorf("npm run build failed: %v", err)
			}
		}
		// Guarded, because Next.js falls through into this branch and is not a
		// case of the app choosing: `next start` is given --port and --hostname.
		// A generic Node service is given neither, and binding every interface
		// is what `app.listen(port)` does by default rather than something the
		// author has to type — so this is the framework most likely to break
		// the loopback rule, and until now the only bring-your-own-listener one
		// the build log said nothing to.
		if result.Framework == FrameworkNode && readsHostAndPort(releaseDir, ".js", ".mjs", ".cjs", ".ts", ".mts") {
			lg.step(readsHostAndPortLine)
		} else if result.Framework == FrameworkNode {
			lg.step("    Warning: Node services choose their own bind address. Listen on the PORT")
			lg.step("    env var and pass HOST — app.listen(port) alone binds 0.0.0.0, which is")
			lg.step("    reachable without authorization. BASE_PATH announces the mount prefix.")
		}

	case FrameworkGo, FrameworkBatchGo:
		// Refused, not corrected. A binary cross-compiled for another OS or
		// architecture satisfies every check this deploy makes and then
		// dies at exec with a message about an exec format error, on a
		// workload nobody is watching (ADR §64 D16).
		if bad := toolchain.RefuseCrossCompile(envVars); len(bad) > 0 {
			return fmt.Errorf("%s %s set in this app's environment. A Go binary built "+
				"for another operating system or architecture passes every check in this "+
				"deploy and then fails to exec on the workload that runs it. Remove %s "+
				"from the app's environment variables",
				strings.Join(bad, " and "), plural(len(bad), "is", "are"),
				plural(len(bad), "it", "them"))
		}
		pkg := result.Entrypoint
		if pkg == "" {
			pkg = "."
		}
		// Before the build, because Go links the SDK in rather than loading it
		// at run time — and removed again after, for the same reason: nothing of
		// it needs to survive, so nothing of it should be captured by the
		// artifact. Kept in step with internal/runner, which prepares it the same
		// way at the same point.
		var prep sdk.GoPrep
		var prepErr error
		_ = b.asBuild(releaseDir, func() error {
			prep, prepErr = sdk.PrepareGoIn(releaseDir, b.scratchFor(releaseDir))
			return nil
		})
		defer b.asBuild(releaseDir, func() error { prep.Done(); return nil })
		switch {
		case prepErr != nil:
			// The cause is the platform's own plumbing — a temp directory, an
			// embedded filesystem — so it goes to the register the operator reads
			// and the owner gets the consequence instead.
			lg.output(fmt.Sprintf("    (the Toft SDK could not be unpacked: %v)", prepErr))
			lg.step("    Warning: the Toft SDK is unavailable on this install.")
			lg.stepf("    'import %q' will not work unless the app vendors it.", sdk.GoModule)
		case prep.Installed:
			lg.stepf("==> Installed the Toft SDK (%s) %s", sdk.GoModule, sdk.Describe("go"))
		default:
			// As the account, like the prepare beside it: the line hashes a
			// vendored copy inside a tree the account can rewrite, and root
			// never resolves a path there (identity.go; PB-5).
			line := ""
			if err := b.asBuild(releaseDir, func() error {
				line = sdk.GoSkippedLine(releaseDir, prep.Skipped)
				return nil
			}); err != nil || line == "" {
				line = "    The Toft SDK was not added: " + prep.Skipped + "."
			}
			lg.step(line)
		}
		lg.stepf("==> Building Go binary (%s)...", pkg)
		if err := b.runCommandFor(timeout, deployID, releaseDir, "go", []string{"build", "-o", runtime.GoBinaryName, pkg}, buildEnv, lg.output); err != nil {
			return goBuildError(err, resolvedRT)
		}
		// What actually compiled it, which is not always what was resolved: the
		// go command re-execs into the toolchain GOTOOLCHAIN names, after the
		// platform has finished deciding. Here rather than beside `record`,
		// because the binary is the thing being read and it does not exist
		// until this line has run.
		b.confirmGoToolchain(deployID, releaseDir, resolvedRT, lg)
		if result.Framework == FrameworkBatchGo {
			// A batch app listens on nothing, so the bind-address warning
			// below would be advice about a server it does not have.
			lg.stepf("    A job runs this binary as ./%s.", runtime.GoBinaryName)
			break
		}
		// HOST rather than an address, on the Python warning's reasoning a
		// hundred lines down: loopback is right in shared mode and wrong in
		// isolated, and this line does not know which it is printing into.
		if readsHostAndPort(releaseDir, ".go") {
			lg.step(readsHostAndPortLine)
			break
		}
		lg.step("    Warning: Go apps choose their own bind address. Serve HTTP on the PORT env")
		lg.step("    var, bound to the HOST env var — an app that picks its own address is either")
		lg.step("    unreachable or reachable without authorization, and which one depends on")
		lg.step("    where it is running. BASE_PATH announces the mount prefix.")

	case FrameworkFlask, FrameworkFastAPI, FrameworkStreamlit, FrameworkPython,
		FrameworkNotebook, FrameworkMarimo, FrameworkBatchPython:
		lg.step("==> Creating Python virtual environment...")
		// `uv venv` with no --python falls through to the system python3,
		// which is what let the base image's Debian release silently decide
		// which Python a deployed app got — 3.11 on bookworm while the
		// isolated runner was already on 3.12, so the same app got a
		// different interpreter depending on which mode it landed in.
		venvArgs := append([]string{"venv", "venv"}, resolvedRT.pythonArgs()...)
		if err := b.runCommandFor(timeout, deployID, releaseDir, "uv", venvArgs, buildEnv, lg.output); err != nil {
			return fmt.Errorf("uv venv failed: %v", err)
		}
		// Before the app's own dependencies, so that a version it declares
		// itself replaces this one: the platform's copy is a floor, not a
		// ceiling (SDK ADR §130).
		if dir, err := b.materializeSDK(releaseDir, "python"); err != nil {
			// The cause is the platform's own plumbing — a temp directory, an
			// embedded filesystem — so it goes to the register the operator
			// reads and the owner gets the consequence instead. Two lines
			// where there was one, and only on the path that failed.
			lg.output(fmt.Sprintf("    (the Toft SDK could not be unpacked: %v)", err))
			lg.step("    Warning: the Toft SDK is unavailable on this install.")
			lg.step("    'import toft' will not work unless the app vendors it itself.")
		} else {
			// **Which copy, not just which version.** The SDK reaches an app by
			// two independent build paths — this one and the runner's, inside
			// the family image — and the two binaries upgrade separately. An
			// install that moved one and not the other serves one SDK to `toft
			// sdk vendor` and installs another here, both declaring the same
			// version. Until this line the build log said nothing about the SDK
			// at all, so the only way to find the difference was to read
			// `inspect.getsource` inside the running app.
			lg.step("==> Installing the Toft SDK " + sdk.Describe("python") + "...")
			err := b.runCommandFor(timeout, deployID, releaseDir, "uv",
				[]string{"pip", "install", dir, "--python", "venv/bin/python"}, buildEnv, lg.output)
			_ = b.asBuild(releaseDir, func() error { return os.RemoveAll(dir) })
			if err != nil {
				// Not fatal. An app that never imports it should not fail to
				// deploy over a convenience, and one that does gets this line
				// rather than a puzzling ImportError. The cause is already in
				// lg.output — runCommand streamed uv's own words there — so
				// wrapping it again here would put uv's stderr in front of the
				// owner to no purpose.
				lg.step("    Warning: installing the Toft SDK failed.")
				lg.step("    'import toft' will not work unless the app vendors it itself.")
			}
		}

		// The renderer, before the app's own dependencies, so a repo pinning
		// its own version replaces the platform's. Nothing for the frameworks
		// that have no renderer, which is every other case of this branch.
		b.installRenderPackages(deployID, result.Framework, releaseDir, buildEnv, lg)

		lg.step("==> Installing Python dependencies...")
		if fileExists(filepath.Join(releaseDir, "requirements.txt")) {
			// A repair passes no deployID, and is a start rather than a deploy.
			args, hashed, err := requirementsInstallArgs(releaseDir, deployID != "")
			if err != nil {
				return fmt.Errorf("requirements.txt could not be read: %v", err)
			}
			if hashed {
				lg.step("    requirements.txt pins and hashes every package, so only those packages are installed.")
			}
			if err := b.runCommandFor(timeout, deployID, releaseDir, "uv", args, buildEnv, lg.output); err != nil {
				if hashed {
					lg.step("    A hashed requirements.txt must list every package the app needs. Regenerate it with")
					lg.step("    `uv pip compile --generate-hashes`, which lists every dependency.")
				}
				return fmt.Errorf("uv pip install failed: %v", err)
			}
		} else if fileExists(filepath.Join(releaseDir, "pyproject.toml")) {
			if err := b.runCommandFor(timeout, deployID, releaseDir, "uv", []string{"pip", "install", ".", "--python", "venv/bin/python"}, buildEnv, lg.output); err != nil {
				return fmt.Errorf("uv pip install failed: %v", err)
			}
		}
		if result.Framework == FrameworkFlask {
			flaskServerLine(releaseDir, result.StartCmd, lg.step)
		}
		if err := fastAPIServerCheck(releaseDir, result.Framework); err != nil {
			return err
		}
		if result.Framework == FrameworkPython {
			// **HOST, not an address**, for the reason Node's warning says it
			// that way: the right address is not the same in both execution
			// modes, and this line is printed without knowing which one the
			// app is in. Shared mode binds loopback because the process shares
			// Toft's network namespace; an isolated workload binds every
			// interface because the namespace *is* the boundary and the gateway
			// dials it at a service address. Naming 127.0.0.1 flatly was advice
			// that, followed on an isolated install, produced an app that never
			// became ready — and the readiness failure says nothing about the
			// warning that caused it.
			if readsHostAndPort(releaseDir, ".py") {
				lg.step(readsHostAndPortLine)
			} else {
				lg.step("    Warning: generic Python apps choose their own bind address. Listen on the")
				lg.step("    PORT env var and bind the HOST env var — an app that picks its own address")
				lg.step("    is either unreachable or reachable without authorization, and which one")
				lg.step("    depends on where it is running. BASE_PATH announces the mount prefix.")
			}
		}

	case FrameworkShiny, FrameworkPlumber, FrameworkRMarkdown, FrameworkQuarto, FrameworkBatchR:
		series := resolvedRT.rSeries()
		if series == "" {
			return errors.New("no R interpreter is available on this install: set TOFT_R_PATH")
		}
		libDir := rLibraryPath(releaseDir, series)
		renvEnvironment := mergeEnv(buildEnv, runner.RenvEnv(libDir, b.envelope().RRepos, b.renvCacheDir()))
		uncoloured(renvEnvironment, buildEnv)
		if _, set := buildEnv["MAKEFLAGS"]; !set {
			// A package compiled from source runs `make` once per package, and
			// make runs one compiler at a time unless told otherwise — so on
			// trytoft Rcpp took 1.4 minutes on one core of eight while the
			// rest sat idle. An app that sets its own MAKEFLAGS keeps it.
			renvEnvironment["MAKEFLAGS"] = rMakeFlags()
		}
		if _, set := buildEnv["R_MAKEVARS_USER"]; !set {
			// Old source against this image's compiler (runner.RLegacyMakevars). An
			// app that names its own Makevars keeps it, as with MAKEFLAGS.
			var path string
			err := b.asBuild(releaseDir, func() (err error) { path, err = runner.WriteRMakevars(releaseDir); return err })
			if err != nil {
				return fmt.Errorf("writing the R build flags: %v", err)
			}
			renvEnvironment["R_MAKEVARS_USER"] = path
		}

		lg.stepf("==> Preparing the R package library (R %s)...", series)
		if err := b.asBuild(releaseDir, func() error { return os.MkdirAll(libDir, 0o755) }); err != nil {
			return fmt.Errorf("creating the R package library: %v", err)
		}
		// renv itself, before the app's packages and never from the app's
		// manifest — it is the tool that reads the lockfile, so an app that has
		// not vendored it could not be restored at all.
		if err := b.runCommandFor(timeout, deployID, releaseDir, rBuildBinary(resolvedRT),
			[]string{"-e", runner.RInstallRenvExpr, libDir, b.envelope().RRepos}, renvEnvironment, lg.output); err != nil {
			return fmt.Errorf("installing renv failed: %v", err)
		}

		// The SDK, before the app's own packages and never from its manifest —
		// the floor-not-ceiling rule the Python and Node installs already
		// follow. Never fatal, for the same reason: an app that never calls it
		// should not fail to deploy over a convenience, and one that does gets
		// this line rather than a puzzling "there is no package called
		// 'toft'".
		if dir, err := b.materializeSDK(releaseDir, "r"); err != nil {
			lg.output(fmt.Sprintf("    (the Toft SDK could not be unpacked: %v)", err))
			lg.step("    Warning: the Toft SDK is unavailable on this install.")
			lg.step("    library(toft) will not work unless the app vendors it itself.")
		} else {
			lg.step("==> Installing the Toft SDK " + sdk.Describe("r") + "...")
			args := append([]string{"-e", runner.RInstallSDKExpr, libDir, b.envelope().RRepos, dir},
				sdk.RSDKDependencies...)
			err := b.runCommandFor(timeout, deployID, releaseDir, rBuildBinary(resolvedRT), args, renvEnvironment, lg.output)
			_ = b.asBuild(releaseDir, func() error { return os.RemoveAll(dir) })
			if err != nil {
				lg.step("    Warning: installing the Toft SDK failed.")
				lg.step("    library(toft) will not work unless the app vendors it itself.")
			}
		}

		if deployID != "" {
			// What the library holds now is Toft's: renv, the SDK and its
			// dependencies. Recorded before the app's own packages arrive, so
			// the gate after the install can say whose each one is.
			b.rPlatform.Store(filepath.Clean(releaseDir), installedNames(libDir))
		}

		if !hasRenvLock(releaseDir) {
			// A repository that names nothing installs nothing, and the deploy
			// says so rather than refusing. The same judgement the Python case
			// makes for a repo with no requirements.txt: a repository that
			// installs nothing is one somebody may be about to finish, and the
			// platform's job is to be honest about what it did rather than to
			// predict the app's first line.
			lg.step("    No renv.lock in this repository, so no R packages were installed.")
			if result.Framework == FrameworkBatchR {
				lg.step("    Each job will fail at its first library() call. Run renv::init() and")
			} else {
				lg.step("    The app will fail at its first library() call. Run renv::init() and")
			}
			lg.step("    renv::snapshot() locally, then commit renv.lock.")
			// A document still renders — and fails *here*, at the render step,
			// naming the package it could not load. Skipping the render too
			// left a release with no toft-render/, which surfaced at the ready
			// check as a [static] root error on an app that declared no
			// [static] (BB-015).
			if result.Framework == FrameworkRMarkdown || result.Framework == FrameworkQuarto {
				if err := b.renderRDocument(deployID, result, releaseDir, resolvedRT, envVars, buildEnv, lg); err != nil {
					return fmt.Errorf("%w\n    This repository has no renv.lock, so the packages the document "+
						"needs (rmarkdown among them) were not installed. Commit renv.lock.", err)
				}
			}
			break
		}

		lg.step("==> Restoring R packages from renv.lock...")
		// Pins the repository has moved past come from a dated snapshot, as
		// binaries (rsnapshot.go). A deploy only: a repair runs on a start, and
		// a start does not wait on P3M.
		restoreEnv := renvEnvironment
		if deployID != "" {
			repos := b.envelope().RRepos
			if planned := rRestoreRepos(releaseDir, repos, lg.step); planned != repos {
				restoreEnv = mergeEnv(renvEnvironment, []string{"RENV_CONFIG_REPOS_OVERRIDE=" + planned})
			}
		}
		if err := b.runCommandFor(timeout, deployID, releaseDir, rBuildBinary(resolvedRT),
			[]string{"-e", runner.RRestoreExpr, libDir, releaseDir}, restoreEnv, lg.output); err != nil {
			return fmt.Errorf("renv::restore failed: %v", err)
		}

		// The release has to be able to leave this machine. See rlang.go.
		if offender, err := verifyNoEscapingLinks(releaseDir, libDir); err != nil {
			lg.output(fmt.Sprintf("    (the R package library could not be checked for links: %v)", err))
		} else if offender != "" {
			return errors.New(escapingLinkMessage(offender))
		}

		// The document half renders here, after its packages and never before:
		// a report's first line is library(dplyr).
		if result.Framework == FrameworkRMarkdown || result.Framework == FrameworkQuarto {
			if err := b.renderRDocument(deployID, result, releaseDir, resolvedRT, envVars, buildEnv, lg); err != nil {
				return err
			}
		}

	case FrameworkStatic:
		// Nothing is installed and nothing is compiled. What happens here
		// instead is the deploy's one chance to say what became public: an
		// operator reading a build log should never have to guess which
		// directory was published or how much of it.
		if err := describeStaticRelease(releaseDir, lg.step); err != nil {
			return err
		}
	}

	// As the account, like every other write after the handover: the tree is
	// the app's by now, and the repository's code has run in it (F-1).
	_ = b.asBuild(releaseDir, func() error { stampRuntime(releaseDir, resolvedRT, lg); return nil })
	return nil
}

// stampRuntime records the interpreter this build resolved into the release
// itself, for the two languages whose tree does not already say (ADR §64 D20).
//
// **After the build and not before**, because the stamp is a statement about a
// tree that exists: a failed install leaves the previous release's answer
// standing, which is the one that still describes what is on the disk.
//
// The *chosen* version rather than the one the build ran, because they are the
// same series by construction and the chosen one is the workload's: in isolated
// mode the build uses this host's interpreter of the release line the runner
// image declares, and it is the pod that reads this back.
//
// Never fatal. A release with no stamp starts the way every release started
// before there was one — on the default — so a record that could not be written
// must not take down a deploy that worked.
func stampRuntime(releaseDir string, rt *resolvedRuntime, lg buildLog) {
	if rt == nil || !slices.Contains(runner.StampedLanguages, rt.Language) {
		return
	}
	version := rt.Chosen.Version
	if version == "" {
		version = rt.BuildWith.Version
	}
	if err := runner.WriteRuntimeStamp(releaseDir, rt.Language, version); err != nil {
		lg.output(fmt.Sprintf("    (the resolved runtime could not be recorded in the release: %v)", err))
	}
}

// flaskServerLine says which server a Flask release will run on, decided by
// runner.FlaskServesWithGunicorn — the question both launchers ask. ADR §116:
// the platform never installs gunicorn itself, so the line is what tells an
// owner the development server is what they got and how to change it.
func flaskServerLine(releaseDir, target string, step func(string)) {
	switch {
	case runner.FlaskServesWithGunicorn(releaseDir, target):
		step(fmt.Sprintf("    Serving with gunicorn (%s), from this app's requirements.", target))
	case runner.HasGunicorn(releaseDir):
		step("    Note: gunicorn is installed but the Flask app object was not found (no top-level")
		step("    `app = Flask(...)` or `def create_app()`), so the app runs on Flask's development server.")
	default:
		step("    Note: this app will run on Flask's development server. Add gunicorn to")
		step("    requirements.txt and it is served by gunicorn instead.")
	}
}

// fastAPIServerCheck fails a FastAPI build whose dependencies did not install
// uvicorn. Unlike Flask there is no server to fall back to, and ADR §116 rules
// out installing one the app did not declare, so without this the build
// succeeded and the release crash-looped on "No module named uvicorn" with
// nothing naming the fix (BB-78).
func fastAPIServerCheck(releaseDir string, framework Framework) error {
	if framework != FrameworkFastAPI || runner.HasUvicorn(releaseDir) {
		return nil
	}
	return fmt.Errorf("FastAPI is served by uvicorn, which this app's dependencies do not include — " +
		"add `uvicorn` (or `fastapi[standard]`) to requirements.txt or pyproject.toml and deploy again; " +
		"Toft never adds a web server the app did not ask for")
}

// uncoloured turns colour off in an R build's environment. renv and the
// packages it installs draw through cli/crayon, which colour whenever they
// guess a terminal — and a build log is a file a person reads through `toft
// logs` or a pipe, where the escapes arrive as 38 lines of noise (docs/bugs
// 2026-10-28 D21). NO_COLOR is the convention cli honours; R_CLI_NUM_COLORS=1
// is cli's own switch, for a version too old to read NO_COLOR. A value the
// app's build environment sets itself is kept.
func uncoloured(env, app map[string]string) {
	for k, v := range map[string]string{"NO_COLOR": "1", "R_CLI_NUM_COLORS": "1"} {
		if _, set := app[k]; !set {
			env[k] = v
		}
	}
}

// goBuildError is a failed `go build`, and when go.mod asked for a Go this
// install does not have, the versions it does: the go command's own refusal
// names only the one it could not fetch ("go: download go1.99.0 …: toolchain
// not available"), which leaves the reader nothing to lower the line to
// (BB-011, third 2026-11-03 list).
func goBuildError(err error, rt *resolvedRuntime) error {
	if rt != nil && rt.Unsatisfied != "" {
		return fmt.Errorf("go build failed: %v — go.mod %s, and the go command could not fetch "+
			"that toolchain; lower the go line in go.mod to one this install has", err, rt.Unsatisfied)
	}
	return fmt.Errorf("go build failed: %v", err)
}

// passedOverNodeLocks are the lockfiles of the package managers Toft does not
// run, in the order they are named.
var passedOverNodeLocks = []struct{ file, tool string }{
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "Yarn"},
}

// passedOverLockLines is one build-log line per Yarn or pnpm lockfile in the
// release: Toft installs with npm, which reads package-lock.json and nothing
// else, so the versions that file pins are not the ones installed.
func passedOverLockLines(releaseDir string) []string {
	var out []string
	for _, l := range passedOverNodeLocks {
		if !fileExists(filepath.Join(releaseDir, l.file)) {
			continue
		}
		from := "package.json"
		if lock := deps.NPMLockfile(releaseDir); fileExists(filepath.Join(releaseDir, lock)) {
			from = lock
		}
		out = append(out, fmt.Sprintf("    %s is passed over: Toft installs with npm, from %s, "+
			"not with %s — the versions it pins are not the ones installed. Commit a package-lock.json "+
			"(npm install writes one) to pin them.", l.file, from, l.tool))
	}
	return out
}

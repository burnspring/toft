package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"toft/internal/toolchain"
)

// The R launchers, defined here because R has no equivalent of `--port`: Shiny
// and plumber are both started by *evaluating an R expression*, so the platform
// writes the expression.
//
// **Here, and not in two places.** The runner is the one launcher in both
// modes — the shared-mode agent places `toft runner` exactly as a pod's
// entrypoint is the runner — so serviceCommand is the whole specification of
// how a framework starts, and these are its R cases. They are exported because
// the builder evaluates its own R expressions (renv, the SDK install) and
// derives the library path from the same names, and one definition is what
// keeps a build and a start from disagreeing about where R's packages are.
//
// **Nothing from the app is interpolated.** The expressions are constants. The
// entrypoint, bind address and port arrive as command-line arguments and are
// read back with commandArgs(trailingOnly = TRUE). A detected file name pasted
// into R source would make the platform's own launcher an eval of repository
// content; quoting it would be a defence that has to stay correct forever,
// where this is a shape with nothing to quote.

// ShinyLaunchExpr starts a Shiny application.
//
// launch.browser = FALSE because there is no browser and no display; without it
// R tries to open one and writes an error into the app's log at every start.
// The app directory is argument one, so a repository whose ui.R and server.R sit
// at the release root is started with "." exactly as one with an app.R is —
// runApp takes a directory there, and naming either file alone starts half an
// app.
const ShinyLaunchExpr = `a <- commandArgs(trailingOnly = TRUE); ` +
	`shiny::runApp(appDir = a[1], host = a[2], port = as.integer(a[3]), launch.browser = FALSE)`

// ShinyDocumentLaunchExpr starts an R Markdown document whose YAML says
// `runtime: shiny` (or shinyrmd, or shiny_prerendered) — detection records the
// .Rmd itself as a Shiny entrypoint, and runApp refuses a file that is not
// app.R: "No Shiny application exists at the path". rmarkdown::run is the
// front end that serves one, and it hands host and port on to runApp through
// shiny_args. auto_reload = FALSE because a release never changes under the
// process; launch.browser is not passed because run() defaults it to
// interactive(), which is FALSE under Rscript, and passing it again would be
// matched twice in the runApp call run() builds.
const ShinyDocumentLaunchExpr = `a <- commandArgs(trailingOnly = TRUE); ` +
	`rmarkdown::run(file = a[1], dir = dirname(a[1]), default_file = basename(a[1]), ` +
	`auto_reload = FALSE, shiny_args = list(host = a[2], port = as.integer(a[3])))`

// PlumberLaunchExpr starts a plumber API.
//
// plumb() then pr_run() rather than the one-call convenience wrapper, whose
// signature has moved across plumber majors while these two have not.
const PlumberLaunchExpr = `a <- commandArgs(trailingOnly = TRUE); ` +
	`plumber::pr_run(plumber::plumb(a[1]), host = a[2], port = as.integer(a[3]))`

// RBinaryName is what the expression is evaluated by when the operator has
// declared no path. Rscript rather than R for the reason toolchain's
// defaultBinary names it: it is the non-interactive front end.
const RBinaryName = "Rscript"

// RLaunchExpr is the expression for one R framework.
func RLaunchExpr(framework string) string {
	if framework == "plumber" {
		return PlumberLaunchExpr
	}
	return ShinyLaunchExpr
}

// rLaunchExprFor is RLaunchExpr with the one case the framework alone cannot
// decide: a Shiny entrypoint that is an .Rmd is a Shiny document. The choice is
// made here on the name's extension, so the expression stays a constant.
func rLaunchExprFor(framework, entrypoint string) string {
	if framework != "plumber" && strings.EqualFold(filepath.Ext(entrypoint), ".rmd") {
		return ShinyDocumentLaunchExpr
	}
	return RLaunchExpr(framework)
}

// RDefaultEntrypoint is what an R framework starts when detection recorded
// nothing — a plumber API's conventional file, and for Shiny the release
// directory itself.
func RDefaultEntrypoint(framework string) string {
	if framework == "plumber" {
		return "plumber.R"
	}
	return "."
}

// RLaunchArgs is the argument vector the expression reads back: entrypoint,
// bind address, port. The order is the contract and both expressions state it.
func RLaunchArgs(framework, entrypoint, bind string, port int) []string {
	if entrypoint == "" {
		entrypoint = RDefaultEntrypoint(framework)
	}
	// **--restore, because `Rscript` means `--no-restore` and a bundle's
	// `.RData` is data.**
	//
	// R restores `.RData` from the working directory at startup under `R`, and
	// not under `Rscript`, which is the front end every launcher here uses.
	// Posit Connect starts an app the way that restores it, so a Connect bundle
	// listing `.RData` among its files arrives with its objects and Toft
	// silently dropped them: the app loaded every package cleanly and died in
	// its own UI code at `object 'regions4map' not found`, naming a variable
	// that is in a file sitting in the release.
	//
	// The blast radius is exactly the apps this fixes. A release with no
	// `.RData` has nothing to restore and behaves identically, so this is not a
	// default being changed for everyone — it is R's own default for the front
	// end this is imitating, put back.
	return []string{"--restore", "-e", rLaunchExprFor(framework, entrypoint), entrypoint, bind, strconv.Itoa(port)}
}

// rExecutable is the R this workload runs under: the operator's declared path
// when there is one, and the name on PATH otherwise. Resolved rather than
// hard-coded, because a family image is a *base* — a customer's own image may
// carry several R versions and declare where they are, exactly as one may for
// Python.
func rExecutable() string {
	if p := toolchain.DefaultPath(toolchain.R); p != "" {
		return p
	}
	return RBinaryName
}

// RLibsSite is the release-relative package library for one R series, or "" when
// the series is unknown — in which case the launcher sets no R_LIBS_SITE at all
// and R falls back to its own defaults, which is the honest outcome for a host
// whose R could not be probed.
func RLibsSite(series string) string {
	if series == "" {
		return ""
	}
	return RenvLibraryRoot + "/R-" + series
}

// RSeriesInRelease reports the R series a release's package library was built
// for, read back from the directory the build stamped.
//
// The stamp is the honest source on both sides of this seam, and it is the only
// one that is always in hand: a restart, a wake and a repair all have the tree
// and none of them has the deploy's resolved runtime. Reading it back is the
// same judgement Builder.startOn makes when it re-detects the framework off the
// directory instead of trusting a column — and it cannot drift from the library
// it names, because it *is* the library's name.
//
// "" means the release carries no R library at all, which is a different thing
// from one built for an R this host no longer has. Unrunnable tells those two
// apart, and only the second is a refusal with a version in it.
func RSeriesInRelease(dir string) string {
	entries, err := os.ReadDir(filepath.Join(dir, RenvLibraryRoot))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if s, ok := strings.CutPrefix(e.Name(), "R-"); ok && s != "" {
			return s
		}
	}
	return ""
}

// RExecutableForSeries is the R of one series on this host, or "" when the host
// carries none of it.
//
// **Never a fallback to the default, and that is the whole of it.** A library
// built by 4.3 and loaded by 4.5 does not fail at the interpreter — it fails at
// dyn.load, one shared object at a time, in an app log naming a file rather
// than a version. Returning "" lets the caller refuse with both numbers, which
// is the same refusal the deploy would give and the reason Repair says it
// approximates nothing.
func RExecutableForSeries(series string) string {
	if series == "" {
		return ""
	}
	for _, v := range toolchain.Available(toolchain.R) {
		if v.Path != "" && v.Series() == series {
			return v.Path
		}
	}
	return ""
}

// RLibsSiteFor and RLaunchBinary are what a workload is started with: the
// series the release was stamped with, and an interpreter of that series.
//
// **One pair, called by both launchers**, for the reason this whole file is one
// definition rather than two strings and a test that they match. The shared
// backend reads a host that may carry several R versions and the isolated one
// reads an image that carries exactly one; the question — *which R matches the
// library about to be loaded* — is the same, and asking it in one place is what
// stops the two answering differently.
//
// The fallback is the host default and it applies only where there is no stamp
// to read, which is a release with no R library: nothing to mismatch, and R's
// own defaults are the honest outcome.
func RLibsSiteFor(dir string) string {
	if s := RSeriesInRelease(dir); s != "" {
		return RLibsSite(s)
	}
	return RLibsSite(toolchain.DefaultSeries(toolchain.R))
}

// RLaunchBinary is the R a workload starts under. See RLibsSiteFor.
func RLaunchBinary(dir string) string {
	if p := RExecutableForSeries(RSeriesInRelease(dir)); p != "" {
		return p
	}
	return rExecutable()
}

// RenvLibraryRoot is the release-relative directory holding an R app's package
// library. The path is the platform's rather than renv's: left to itself renv
// writes renv/library/<platform>/R-<major.minor>/<arch>, a layout renv is free
// to change and whose leading component names a distribution this release may
// not be loaded on.
const RenvLibraryRoot = "renv/library"

// The R build expressions. Here rather than in internal/builder for the reason
// the launch expressions are: internal/runtime and internal/builder can both
// import this package, and the runner binary must not import the builder — so
// this is the one place both build paths can share rather than the one list
// that has to be written twice and held by a test.

// rP3MUserAgent makes a binary repository actually serve binaries.
//
// **This is the line the "no compiler anywhere" decision rests on.** Posit
// Package Manager decides whether a request for src/contrib/<pkg>.tar.gz is
// answered with source or redirected to a prebuilt binary, and it decides by
// reading the R version out of the User-Agent. R's own default — `R (4.5.0
// x86_64-pc-linux-gnu ...)` — is not the form P3M parses, so an unmodified R
// asks a binary repository for binaries and is quietly handed source.
//
// Quietly is the whole problem. Nothing errors at the repository: the download
// succeeds, `R CMD INSTALL` starts, and the deploy fails several minutes later
// at `sh: 1: make: not found` — a message about a missing compiler on an image
// that is *designed* to have no compiler, which reads as the image being wrong
// rather than the request. Every R deploy on this install failed this way.
//
// The version is computed in R rather than written here for the reason
// TOFT_R_SERIES is stated by the image: the running interpreter is the one
// that knows, and a string in this file would be a second copy free to drift
// from the R the stage actually installed.
const rP3MUserAgent = `options(HTTPUserAgent = sprintf("R/%s R (%s)", getRversion(), ` +
	`paste(getRversion(), R.version$platform, R.version$arch, R.version$os))); `

// RInstallRenvExpr installs renv itself into the release's library.
//
// The platform installs renv rather than trusting the app to: it is the tool
// that reads the lockfile, so an app that has not vendored it could not be
// restored at all. Before the app's packages and never from the app's manifest
// — the floor-not-ceiling rule the SDK install already follows.
const RInstallRenvExpr = rP3MUserAgent +
	`a <- commandArgs(trailingOnly = TRUE); ` +
	`dir.create(a[1], recursive = TRUE, showWarnings = FALSE); ` +
	`.libPaths(a[1]); ` +
	`if (!requireNamespace("renv", quietly = TRUE)) ` +
	`install.packages("renv", lib = a[1], repos = a[2])`

// RRestoreExpr restores the app's lockfile into the release's library.
//
// prompt = FALSE because there is no terminal. The library is passed explicitly
// as well as through RENV_PATHS_LIBRARY, which is what makes the two agree when
// a repository ships an .Rprofile calling renv::activate().
const RRestoreExpr = rP3MUserAgent +
	`a <- commandArgs(trailingOnly = TRUE); ` +
	`.libPaths(a[1]); ` +
	`renv::restore(project = a[2], library = a[1], prompt = FALSE)`

// RMakevarsFile is the Makevars the platform writes into an R release and
// points R_MAKEVARS_USER at, beside RuntimeStamp at the release root.
const RMakevarsFile = ".toft-makevars"

// RLegacyMakevars lets a package pinned years back compile against this
// image's compiler and system libraries. A lock from 2024 is served *source*
// for anything the binary repository no longer carries for this distribution,
// and the C and C++ it holds was written for an older GCC and older headers:
// ragg 1.3.0 assigns FreeType 2.13.3's `unsigned char*` outline tags to a
// `char*`, which GCC refuses without -fpermissive, and GCC 14 made three C
// warnings into errors that a great deal of older package code trips. Each
// flag turns a refusal back into the warning it was when the package was
// published; none changes the code a package compiles to. C++ is named per
// standard because R swaps CXXFLAGS for CXX17FLAGS and the rest by CXX_STD.
const RLegacyMakevars = `CFLAGS += -Wno-error=incompatible-pointer-types -Wno-error=int-conversion -Wno-error=implicit-function-declaration
CXXFLAGS += -fpermissive
CXX11FLAGS += -fpermissive
CXX14FLAGS += -fpermissive
CXX17FLAGS += -fpermissive
CXX20FLAGS += -fpermissive
`

// WriteRMakevars writes RLegacyMakevars into the release and returns its path.
// Both build paths call it — internal/builder at deploy, a pod's repair here —
// because the two must produce the same tree.
func WriteRMakevars(releaseDir string) (string, error) {
	path := filepath.Join(releaseDir, RMakevarsFile)
	return path, os.WriteFile(path, []byte(RLegacyMakevars), 0o644)
}

// RenvEnv is the environment every R install command runs under, on both build
// paths.
//
// **This is the decision the rest of R's packaging rests on.** renv::restore()
// defaults to a machine-global cache and fills the project library with
// *symlinks into it*. On the build host that produces a correct tree; the
// artifact then carries hundreds of links to paths that exist on no pod, and
// internal/artifact's pack stores a symlink as a symlink. The failure surfaces
// inside a container as dyn.load errors, one package at a time, and the release
// is *smaller*, so nothing about it looks wrong.
//
// **What was wrong was the links, never the cache.** With cacheDir set the cache
// is on and RENV_CONFIG_CACHE_SYMLINKS is off, so renv *copies* a package out of
// the cache into the library: real directories, no links, an inode of its own
// (measured: the second restore of the same lock says "copied from cache" and
// the tree has no symlink in it). What the cache keeps is the install — a
// package the lockfile pins below what the repository serves as a binary is
// compiled once per version, R minor and distribution rather than on every
// deploy, which on trytoft was one to three minutes a package (Rcpp, S7,
// openxlsx).
//
// With cacheDir empty — a pod's repair, which has no cache volume — the cache is
// off and the restore writes real directories exactly as before. Two apps
// depending on dplyr each still carry their own copy either way: the copy is
// what lets a release move between execution modes and between machines.
func RenvEnv(libDir, repos, cacheDir string) []string {
	env := []string{
		// Inside the tree that ships, and stamped with the series it was built
		// for so a repair can tell whether it matches the R about to load it.
		"RENV_PATHS_LIBRARY=" + libDir,
		// A repository's own .Rprofile must not decide the platform's paths.
		"RENV_CONFIG_AUTOLOADER_ENABLED=FALSE",
		"RENV_CONFIG_SYNCHRONIZED_CHECK=FALSE",
		// The lockfile is the whole answer; the app's own DESCRIPTION is not.
		// renv 1.3's graph install otherwise reads the project's `Remotes:`
		// (`USEPA/rExpertQuery@develop`) and asks api.github.com which commit
		// each branch is now — on every restore, unauthenticated, although the
		// lock already pins that package to a commit. Every build on an install
		// shares one egress address and its 60 anonymous calls an hour, so the
		// restore failed at random (TADAShiny on trytoft, error code 22). A
		// dependency's own Remotes field is moot for the same reason: the lock
		// names every package and where it comes from.
		"RENV_CONFIG_INSTALL_REMOTES=FALSE",
	}
	if cacheDir != "" {
		env = append(env,
			"RENV_CONFIG_CACHE_ENABLED=TRUE",
			// Copies, not links: the half of renv's default that broke releases.
			"RENV_CONFIG_CACHE_SYMLINKS=FALSE",
			"RENV_PATHS_CACHE="+cacheDir,
			// renv 1.3's graph install copies cache hits first and leaves them
			// out of its dependency graph, so a binary whose only link to rlang
			// runs through a cached htmltools is load-tested before rlang is
			// installed, and the restore fails "there is no package called
			// 'rlang'". The test is renv's own escape hatch; a package that
			// truly will not load still fails the render or the ready check.
			"RENV_INSTALL_TEST_LOAD=FALSE",
		)
	} else {
		env = append(env,
			"RENV_CONFIG_CACHE_ENABLED=FALSE",
			// Emptied rather than merely unset: renv reads the ambient value and
			// the build process may have inherited one.
			"RENV_PATHS_CACHE=",
		)
	}
	if repos != "" {
		// The versions are the lock's and the source is the operator's. A
		// committed renv.lock records the repositories it resolved against,
		// usually CRAN's source server; restoring against that compiles every
		// package. Measured: three small pure-R packages took 43 seconds from
		// source where a binary repository would have unpacked them.
		env = append(env, "RENV_CONFIG_REPOS_OVERRIDE="+repos)
	}
	return env
}

// RInstallSDKExpr installs the Toft R SDK and the two packages it needs.
//
// Argument one is the library, two the repository, three the unpacked SDK
// directory, and everything after that is a dependency name — so the list is
// the caller's and this expression stays a constant, on the launchers' terms.
//
// The dependencies are installed first and only when absent: an app that pins
// its own jsonlite has already had it restored by the time a repair runs here,
// and reinstalling would replace the app's version with the repository's.
const RInstallSDKExpr = rP3MUserAgent +
	`a <- commandArgs(trailingOnly = TRUE); ` +
	`.libPaths(a[1]); ` +
	`deps <- a[-(1:3)]; ` +
	`missing <- deps[!vapply(deps, requireNamespace, logical(1), quietly = TRUE)]; ` +
	`if (length(missing)) install.packages(missing, lib = a[1], repos = a[2]); ` +
	`install.packages(a[3], lib = a[1], repos = NULL, type = "source")`

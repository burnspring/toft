package builder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"toft/internal/runner"
	"toft/internal/toolchain"
)

// The R build: renv restored into the release, copying out of renv's cache.
//
// **This is the decision the rest of R's packaging rests on.** renv::restore()
// defaults to a machine-global cache and fills the project library with
// *symlinks into it*. On the build host that produces a correct tree; the
// artifact then carries hundreds of links to paths that exist on no pod, and
// internal/artifact's pack stores a symlink as a symlink. The failure surfaces
// inside a container as dyn.load errors, one package at a time, in a log nobody
// reads as a packaging bug — and the release is *smaller*, so nothing about it
// looks wrong.
//
// Measured rather than reasoned about: with the cache on, a restore writes
// R6 -> /opt/renv-cache/v5/linux-ubuntu-noble/R-4.5/.../R6/2.6.1/<hash>/R6 and
// every other package the same way. **The links were the defect, not the
// cache**: with RENV_CONFIG_CACHE_SYMLINKS off (runner.RenvEnv) the same restore
// copies out of the cache into real directories, so a package compiled once is
// copied rather than compiled on every later deploy, and the release still has
// no link in it. verifyNoEscapingLinks below is what holds that.
//
// The stated cost is that two apps depending on dplyr each carry their own
// copy. That is the trade a venv already makes, it is what lets a release move
// between execution modes and between machines, and the alternative is an
// artifact that is smaller because it is broken.

// rMakeFlags is the parallelism an R source compile gets: one job per CPU this
// process may use. GOMAXPROCS rather than NumCPU, because it honours a
// container's CPU limit and NumCPU reports the machine's. Not divided by the
// number of builds at once: a compile is the minority of an R build, and two
// builds oversubscribing the CPU for a minute costs less than every compile
// running on one core.
func rMakeFlags() string {
	return fmt.Sprintf("-j%d", goruntime.GOMAXPROCS(0))
}

// rLibraryPath is the absolute library directory for a release and series.
func rLibraryPath(releaseDir, series string) string {
	return filepath.Join(releaseDir, runner.RLibsSite(series))
}

// hasRenvLock reports whether the release names its dependencies. Lstat, so
// a committed link counts as the author's lock whatever it points at: a
// dangling one followed by Stat read as "no lock", and the platform then wrote
// one through it (security review 2026-11-10, E-4).
func hasRenvLock(releaseDir string) bool {
	_, err := os.Lstat(filepath.Join(releaseDir, "renv.lock"))
	return err == nil
}

// rPackagesUnlocked is toft check's warning for an R tree with no renv.lock.
// The build does not refuse one — a repository that names nothing installs
// nothing, and may be about to be finished — but it says the app will fail at
// its first library(), and the check passed the same tree in silence
// (drills on trytoft, 2026-10-23).
func rPackagesUnlocked(dir, language string, framework Framework) (Warning, bool) {
	if language != toolchain.R || hasRenvLock(dir) {
		return Warning{}, false
	}
	// A document is rendered by the deploy, so it fails there — at the render
	// step, naming the package — and never reaches a first library() call of
	// a running app (9b9d1816; BB-013, third 2026-11-03 list).
	fails := "the app will fail at its first library() call"
	switch framework {
	case FrameworkRMarkdown, FrameworkQuarto:
		fails = "the deploy will fail at its render step, at the first package the document loads"
	case FrameworkBatchR:
		// A batch app has no process of its own: what fails is a job, at its
		// run (D-16, 2026-11-04 black-box list).
		fails = "each job will fail at its first library() call"
	}
	return Warning{Code: WarningNoRenvLock, Message: "no renv.lock, so the build will install no R " +
		"packages and " + fails + " — run renv::init() and " +
		"renv::snapshot() locally, then commit renv.lock"}, true
}

// RDeclaredSeries is the R series a release says it was built against, or ""
// when it says nothing.
//
// **One file, and it is the one the platform already writes.** A Posit Connect
// bundle states its R in `manifest.json`'s `platform` and a native renv project
// states it in `renv.lock`'s `R.Version` — but `adoptConnectManifest` translates
// the first into the second before the runtime is resolved, precisely so that
// nothing downstream has to know which kind of release it is holding. Reading
// `renv.lock` reads both, and teaching this a second format would undo the
// reason that translation exists (§64: one declaration).
//
// **The patch is dropped rather than refused**, which is the opposite of what a
// *written* pin gets. `>=4.4.1` from a developer is refused because truncating
// it would mean something other than what they wrote; nobody wrote this one,
// R's binaries are published per minor, and 4.4.1 here can only mean the series
// it belongs to.
func RDeclaredSeries(releaseDir string) string {
	data, err := readInRelease(releaseDir, "renv.lock")
	if err != nil {
		return ""
	}
	var lock renvLockFile
	if err := json.Unmarshal(data, &lock); err != nil {
		// A lockfile that will not parse is not this function's to refuse: the
		// restore is about to fail on it with a message naming the file, which
		// is a better place to hear it than a version decision.
		return ""
	}
	return toolchain.SeriesOf(toolchain.R, lock.R.Version)
}

// verifyNoEscapingLinks walks a built R library and reports the first entry that
// is a symlink leaving the release directory.
//
// The check runs at build time rather than only in a test, because the failure
// it catches is silent in every other place it could be noticed: pack stores
// symlinks verbatim, unpack recreates them, and the release is smaller. A link
// out of the tree means the cache was on — an renv upgrade changing a default,
// an environment variable arriving from somewhere new — and the deploy that
// produced it is the last moment anyone can be told.
func verifyNoEscapingLinks(releaseDir, libDir string) (string, error) {
	root, err := filepath.Abs(releaseDir)
	if err != nil {
		return "", err
	}
	var offender string
	err = filepath.WalkDir(libDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || offender != "" {
			return nil //nolint:nilerr // a library that cannot be walked is not this check's to refuse
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			// A dangling link is already the failure this exists to find.
			offender = path
			return filepath.SkipAll
		}
		if rel, err := filepath.Rel(root, target); err != nil || rel == ".." ||
			len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator) {
			offender = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return offender, nil
}

// escapingLinkMessage is what a deploy says when the library it just built
// cannot travel.
func escapingLinkMessage(offender string) string {
	return fmt.Sprintf("the R package library contains a link out of the release (%s), "+
		"which would arrive in the pod pointing at nothing. renv must copy out of its "+
		"cache rather than link into it for a release to be movable", offender)
}

// rBuildBinary is the R the *build* runs under. The build always happens on the
// platform host, in both execution modes, so this is the host's R and never the
// runner image's — which is exactly why resolvedRuntime.rSeries stamps the
// library with the series the *workload* will load it under rather than with
// this one.
//
// **It is the resolved interpreter, not the install default**, and the
// difference is invisible on an install carrying one R — which is every install
// until an operator lists a second path in TOFT_R_PATH. Taking the default
// meant a build could resolve 4.3, write its library into renv/library/R-4.3,
// record 4.3 on the deployment row and print "Building against R 4.3.3
// (satisfies >=4.3,<4.4)" — and then hand every source package to 4.5's
// Makeconf. A pin reported and not honoured is worse than no pin at all: it is
// the substitution ADR §64 exists to refuse, printing the right answer while
// doing the other thing.
func rBuildBinary(rt *resolvedRuntime) string {
	if rt != nil && rt.BuildWith.Path != "" {
		return rt.BuildWith.Path
	}
	if p := toolchain.DefaultPath(toolchain.R); p != "" {
		return p
	}
	return runner.RBinaryName
}

// mergeEnv overlays KEY=VALUE strings onto a copy of the build environment.
//
// A copy, not a mutation: the build host may be restoring several apps at once,
// and an exported RENV_PATHS_LIBRARY would send one app's packages into
// another's release.
func mergeEnv(base map[string]string, overlay []string) map[string]string {
	env := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		env[k] = v
	}
	for _, kv := range overlay {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

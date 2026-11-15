package runner

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"toft/internal/toolchain"
)

// The interpreter a release was built against, written into the release by the
// build and read back by whatever starts it.
//
// **Why a release needs a stamp at all, and why R did not need this one.** An
// R release is stamped already — `renv/library/R-4.4` is a directory the
// platform names, so the library *is* the record — and a Python release very
// nearly is, because `uv venv` writes an absolute interpreter path into
// `venv/bin/python` and a workload that execs it cannot get the wrong one.
// **Node has neither.** `node_modules` says nothing about the Node that filled
// it, `npm` and `node` are found on PATH, and a native module compiled against
// one major fails to load under another. So for as long as an install carried
// one Node, the pin was honoured at build time and the launcher ran whatever
// the image's PATH offered — which was the same binary, and the substitution
// was invisible because there was nothing to substitute.
//
// A second Node makes it visible: an app pinned to 20, built against 20, and
// started with the 24 on PATH is exactly the failure ADR §64 exists to refuse,
// arriving after a build that printed the right answer. The stamp is what lets
// a start ask the same question the build answered — *which interpreter is this
// tree for* — from the tree itself, which is the only thing a restart, a wake,
// a node move and a pod all have in hand.
//
// **Python is stamped too, for the repair.** An isolated Python workload
// rebuilds its venv in the pod because the platform's interpreter path does not
// exist there, and a rebuild with no `--python` is uv's discovery order
// deciding a version somebody pinned. The venv it would read that from is the
// thing being discarded, so the answer has to survive it.
//
// **Only the two.** Go's version leaves nothing in the tree to be wrong about —
// `go build` produces a binary the workload execs — and R already has a stamp
// of its own, which this must not become a second opinion about. A stamp
// nothing reads would be exactly the second place for the truth to live that
// ADR §64 keeps out of variable names.
//
// Dot-prefixed, so `staticserve.Refused` keeps it out of anything a static app
// publishes, and so it reads as the platform's rather than the author's.
const RuntimeStamp = ".toft-runtime"

// StampedLanguages are the languages whose releases carry one. The list is the
// specification: anything not here is answered by the tree itself.
var StampedLanguages = []string{toolchain.Python, toolchain.Node}

// WriteRuntimeStamp records the interpreter a build resolved for this release.
//
// Best-effort by contract, and the caller treats a failure as a warning: a
// release with no stamp behaves exactly as every release did before there was
// one — it runs on the default — which is the right way for a record to fail.
// An empty language or version removes it rather than writing a blank, so a
// repair that could not resolve anything does not leave a stamp claiming it did.
//
// **Written through the release's own root, never following a link.** The
// stamp is the last write of a build, after the tree is the app account's and
// the repository's code has run in it; a build that left `.toft-runtime ->
// /etc/passwd` had the platform truncate the target (code review 2026-11-11,
// F-1). The builder also makes this write as the account (asBuild); this is
// the half that holds when it has no account to be.
func WriteRuntimeStamp(dir, lang, version string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	// Whatever is there goes first — a link, a directory, a FIFO — so the
	// create below is of a fresh regular file and nothing else.
	if fi, err := root.Lstat(RuntimeStamp); err == nil && !fi.Mode().IsRegular() {
		if err := root.RemoveAll(RuntimeStamp); err != nil {
			return err
		}
	}
	if lang == "" || version == "" {
		if err := root.Remove(RuntimeStamp); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	f, err := root.OpenFile(RuntimeStamp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(lang + " " + version + "\n")); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReleaseRuntime reads the stamp back: the language and the version it names,
// or two empty strings for a release that carries none.
//
// A release built before the stamp existed is the ordinary empty case and not
// an error — it resolved against an install that had one version of everything,
// which is what "" then means to every caller here.
func ReleaseRuntime(dir string) (lang, version string) {
	// Read the way it was written: inside the release, no link followed, a
	// regular file only, bounded — the tree is the app account's.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", ""
	}
	defer root.Close()
	f, err := root.OpenFile(RuntimeStamp, os.O_RDONLY|syscall.O_NOFOLLOW|toolchain.OpenNonblock, 0)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	b, err := toolchain.ReadRegular(f, RuntimeStamp, 4096)
	if err != nil {
		return "", ""
	}
	fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
	if len(fields) != 2 {
		return "", ""
	}
	if toolchain.SeriesOf(fields[0], fields[1]) == "" {
		return "", ""
	}
	return fields[0], fields[1]
}

// ReleaseSeries is the matching series this release was built against for one
// language, or "" when it says nothing about that language.
func ReleaseSeries(lang, dir string) string {
	l, v := ReleaseRuntime(dir)
	if l != lang {
		return ""
	}
	return toolchain.SeriesOf(lang, v)
}

// InterpreterForRelease is the interpreter matching the release's stamp, and
// the series that was wanted.
//
// The two are returned together because the three answers a caller has to tell
// apart are three combinations of them. A zero version with an empty series is
// a release that states nothing — start it the way releases have always been
// started. A zero version with a series is a tree built for an interpreter this
// host does not have, which is a refusal with both numbers in it and never a
// quiet fall back to the default. A version with a series is the ordinary case.
//
// **Never a fallback**, for the reason RExecutableForSeries is not one: a
// native module built against Node 20 and loaded by 24 fails at `require`
// naming a file, and a venv rebuilt on the wrong Python resolves a different
// dependency tree and reports success.
func InterpreterForRelease(lang, dir string) (toolchain.Version, string) {
	series := ReleaseSeries(lang, dir)
	if series == "" {
		return toolchain.Version{}, ""
	}
	for _, v := range toolchain.Available(lang) {
		if v.Series() == series {
			return v, series
		}
	}
	return toolchain.Version{}, series
}

// HasSeries reports whether this host or image carries an interpreter of one
// series — a question rather than a binary, because the caller that asks it,
// `Unrunnable`, is deciding whether a tree can run at all and has no use for
// the path.
//
// R's peer is RExecutableForSeries, which predates this and stays where it is:
// it is called from the R launchers, which need the binary, and collapsing the
// two would move R's answer for no reason but symmetry.
func HasSeries(lang, series string) bool {
	if series == "" {
		return false
	}
	for _, v := range toolchain.Available(lang) {
		if v.Series() == series {
			return true
		}
	}
	return false
}

// NodePathPrefix is the directory to put in front of PATH so that `node`, `npm`
// and every shebang resolving through them are the ones this release was built
// against.
//
// **A directory rather than a binary, and that is the whole of it: `npm` is a
// Node script.** `npm run start` is `#!/usr/bin/env node` and so is
// `node_modules/next/dist/bin/next`, so naming one interpreter and leaving the
// tool beside it to be found by an untouched PATH is how a release ends up
// installed by one major and executed by another.
//
// Empty in three cases and each is right to be. A release that states nothing
// starts the way it always did; one whose series this host does not have is
// refused by `Unrunnable` before it reaches here; and one whose series *is* the
// default needs no prefix, because the default is what PATH already resolves
// to — putting its own directory in front would be a no-op spelled as a change.
func NodePathPrefix(dir string) string {
	v, _ := InterpreterForRelease(toolchain.Node, dir)
	if v.Path == "" || v.Default {
		return ""
	}
	return filepath.Dir(v.Path)
}

// PythonPathPrefix is the directory to put in front of PATH so that `python`,
// `python3` and everything they resolve are the release's own virtual
// environment rather than the image's interpreter.
//
// **This is the Python half of the same seam R_LIBS_SITE closes for R, and it
// was missing for the same reason: every launcher the platform writes names
// `venv/bin/python` itself, so nothing ever needed PATH — until a job.** A job's
// command comes from the app's own manifest and the documented example is
// `python jobs/report.py`, which without this resolves to the image's
// interpreter: no Flask, no pandas, and no `toft`, on a release whose build
// log lists every one of them installing cleanly. The same is true of anything a
// service workload shells out to.
//
// An absolute path, because the environment it is written into travels to a
// process whose working directory is the release but whose PATH is read by a
// shell that does not know that.
//
// Empty when there is no usable venv — a release of a family that has none, or
// one whose interpreter link dangles because the image moved under it. The
// second is the case `Unrunnable` refuses by name first, so this returning ""
// means the tree is about to be repaired and asked again.
func PythonPathPrefix(dir string) string {
	bin := filepath.Join(dir, "venv", "bin")
	if _, err := os.Stat(filepath.Join(bin, "python")); err != nil {
		return ""
	}
	return bin
}

// ToolBeside is the absolute path of a tool sitting beside an interpreter, or
// the bare name when there is none there.
//
// **This exists because Go's exec.Command resolves a bare name against the
// *parent's* PATH and not against cmd.Env.** Prepending a directory to a
// child's PATH therefore decides which `node` a shebang finds and does not
// decide which `npm` is started — so a build could run npm 12 out of
// /usr/local/bin on the Node 20 the prefix correctly supplied, which is the
// "installed by one major, executed by another" split this whole section exists
// to close, arriving from the one direction nobody looks. Measured on the first
// Node 20 deploy this platform ever accepted: `npm warn cli npm v12.0.2 does
// not support Node.js v20.20.2`, in a build log that had just printed the right
// answer twice.
//
// Both halves are kept: the absolute path decides which tool runs, and the PATH
// prefix beside it decides what that tool and everything it spawns resolve
// `node` to. R has never needed this — `rBuildBinary` has passed an absolute
// interpreter since it was written — which is the same lesson from the family
// that learned it first.
//
// The bare name when the tool is not there, so an image that ships an
// interpreter without its package manager behaves exactly as it did before
// rather than failing with a path.
func ToolBeside(binDir, name string) string {
	if binDir == "" {
		return name
	}
	p := filepath.Join(binDir, name)
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return name
	}
	return p
}

// PrependPath puts dir in front of an existing PATH value, and is a no-op for
// an empty dir so that every caller reads as one line.
func PrependPath(dir, path string) string {
	if dir == "" {
		return path
	}
	if path == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}

// stampedLanguage is the language a framework's release is stamped with, or ""
// for the frameworks whose tree already answers the question.
//
// Deliberately narrower than internal/builder's LanguageOf and not a copy of
// it: this one says *which stamp to read*, and R and Go are absent from it
// because R has a stamp of its own and Go has nothing to be wrong about. A
// general language map here would be a second opinion about a mapping that
// already exists, for a question it is not being asked.
func stampedLanguage(framework string) string {
	switch framework {
	case "flask", "fastapi", "streamlit", "python", "notebook", "marimo", "batch-python":
		return toolchain.Python
	case "node", "nextjs", "", "batch-node":
		return toolchain.Node
	}
	return ""
}

// RepairInterpreter is the interpreter a repair rebuilds this release with: the
// one its stamp names when this image has it, and the image's default when the
// release states nothing.
//
// **The fallback is deliberate and it is the one approximation in this
// section**, so it is worth being explicit about why it is not the substitution
// ADR §64 refuses. A pin is refused at the *deploy*, where the developer is
// present and a message can name the range and the set. A repair is a rescue of
// a tree that is already built and already deployed, reached at a start — and
// the alternative to rebuilding it against the default is an app that never
// comes back and a workload that will be refused identically at every restart
// for as long as the image stands. R makes the same trade at the same seam and
// says so in the same words.
//
// A zero Version means this image has no interpreter of that language at all,
// which is a real configuration for every family but its own.
func RepairInterpreter(lang, dir string) toolchain.Version {
	available := toolchain.Available(lang)
	if series := ReleaseSeries(lang, dir); series != "" {
		for _, v := range available {
			if v.Series() == series {
				return v
			}
		}
	}
	for _, v := range available {
		if v.Default {
			return v
		}
	}
	if len(available) > 0 {
		// Highest, since Available sorts descending. Reached only by an image
		// that declares every interpreter it has and none of them by name on
		// PATH, which is a configuration rather than a mistake.
		return available[0]
	}
	return toolchain.Version{}
}

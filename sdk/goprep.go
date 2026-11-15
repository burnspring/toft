package sdk

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"toft/internal/toolchain"
)

// Making the Go SDK importable from a release the platform is about to build.
//
// The other two languages install a *runtime* artifact: a wheel in the venv, a
// directory under node_modules, both of which the app still needs when it runs.
// Go links statically, so the SDK is a build-time input and nothing of it
// survives — which is why this leaves the release tree exactly as it found it.
//
// The mechanism is a workspace. `go.work` naming the release and the
// materialized module makes `import "toft"` resolve **without a require
// line in the app's go.mod**, so the platform never edits a file the developer
// wrote and never puts a module path into a go.sum that cannot be verified
// against anything. The alternatives were both worse: `go mod edit -replace`
// mutates the app's own manifest and is captured by the release artifact, and
// vendoring the SDK into the app's `vendor/` would change how every one of its
// *other* dependencies resolves.
//
// It is skipped rather than forced in three cases, and each is the same rule the
// other two languages have: **the platform's copy is a floor, not a ceiling**, so
// an app that has made its own arrangement keeps it.

// GoWorkspaceFile is the file this writes into a release, and removes again.
const GoWorkspaceFile = "go.work"

// GoPrep is the result of [PrepareGo]: what happened, and what to undo.
type GoPrep struct {
	// Installed is whether the SDK was made importable for this build.
	Installed bool

	// Skipped is why it was not, in words a developer reads in a build log.
	// Empty when Installed is true.
	Skipped string

	// Dir is where the module was materialized, outside the release tree.
	Dir string

	release string
}

// Done removes everything [PrepareGo] wrote: the materialized module, the
// workspace file, and the sum file a workspace can leave behind.
//
// Always call it, and call it after the build rather than after the start. A
// `go.work` left in a release would be captured by the artifact, shipped to a
// pod, and there point at a temporary directory on another machine — so the
// pod's own rebuild would fail on a path that never existed there.
func (p GoPrep) Done() {
	if p.Dir != "" {
		os.RemoveAll(p.Dir)
	}
	if p.release == "" {
		return
	}
	for _, name := range []string{GoWorkspaceFile, GoWorkspaceFile + ".sum"} {
		os.Remove(filepath.Join(p.release, name))
	}
}

// PrepareGo materializes the Go SDK beside a release and writes the workspace
// that makes it importable.
//
// The returned [GoPrep] is always usable — a skip is not an error, and its Done
// is safe to call. An error means the platform's own plumbing failed (a temp
// directory, the embedded filesystem), which is a line in the operator's log
// rather than a reason to fail somebody's deploy.
func PrepareGo(releaseDir string) (GoPrep, error) { return PrepareGoIn(releaseDir, "") }

// PrepareGoIn is PrepareGo with the SDK unpacked under parent rather than the
// process's TMPDIR — where a build running as an app's own account can read it.
func PrepareGoIn(releaseDir, parent string) (GoPrep, error) {
	// Read inside the tree, non-blocking, regular files only: on a repair the
	// tree is the app's, and a FIFO named go.mod parked the repair under the
	// build cache lock — and every later build behind it (security review
	// 2026-11-15, LB-5).
	root, err := os.OpenRoot(releaseDir)
	if err != nil {
		return GoPrep{Skipped: "this release has no go.mod, so there is no module to add the SDK to"}, nil
	}
	defer root.Close()
	declared, err := readGoMod(root)
	if err != nil {
		return GoPrep{Skipped: "this release has no go.mod, so there is no module to add the SDK to"}, nil
	}
	// A workspace turns vendoring off. An app that vendors is saying it means to
	// build from `vendor/` — possibly on a machine with no network at all — and
	// silently building from the module cache instead would be a change to how
	// every one of its own dependencies resolves, in exchange for a convenience.
	if info, err := os.Stat(filepath.Join(releaseDir, "vendor")); err == nil && info.IsDir() {
		return GoPrep{Skipped: "this app vendors its dependencies, and a workspace would " +
			"stop the build reading vendor/"}, nil
	}
	// The app brought its own workspace. Rewriting it would be the platform
	// overwriting a file the developer wrote, and merging into it would leave a
	// change behind after the build.
	// Lstat, through the root: a go.work that is a link, a FIFO or anything at
	// all is the app's, and the write below must never open or follow it.
	if _, err := root.Lstat(GoWorkspaceFile); err == nil {
		return GoPrep{Skipped: "this app brought its own go.work, which the platform will not rewrite"}, nil
	}
	// The operator chose a module mode. `go` refuses `-mod=mod` outright in
	// workspace mode, and `-mod=vendor` there looks for a *workspace* vendor
	// directory that does not exist — so on an install with either set, adding a
	// workspace would turn every Go build that worked yesterday into a failure
	// whose message names a flag rather than the SDK. What is on the toolchain
	// environment is the operator's (ADR §64 D15), which settles it: the
	// convenience gives way, and says so.
	if mode := moduleMode(os.Getenv("GOFLAGS")); mode != "" && mode != "readonly" {
		return GoPrep{Skipped: "this install sets GOFLAGS=-mod=" + mode +
			", which a workspace cannot honour"}, nil
	}

	dir, err := Materialize(parent, "go")
	if err != nil {
		return GoPrep{}, err
	}
	prep := GoPrep{Installed: true, Dir: dir, release: releaseDir}

	workspace := fmt.Sprintf("go %s\n\nuse (\n\t.\n\t%s\n)\n",
		workspaceLanguage(string(declared)), filepath.ToSlash(dir))
	if err := writeNew(root, GoWorkspaceFile, []byte(workspace)); err != nil {
		prep.Done()
		return GoPrep{}, err
	}
	return prep, nil
}

func readGoMod(root *os.Root) ([]byte, error) {
	f, err := root.OpenFile("go.mod", os.O_RDONLY|toolchain.OpenNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return toolchain.ReadRegular(f, "go.mod", toolchain.MaxManifestBytes)
}

// writeNew creates name under root, refusing one that appeared since the
// Lstat: O_EXCL neither follows a link nor opens a FIFO.
func writeNew(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var modeFlag = regexp.MustCompile(`(?:^|\s)-mod=(\S+)`)

// moduleMode reads the `-mod=` setting out of a GOFLAGS value, or "" when there
// is none. The last one wins, as it does on a command line.
func moduleMode(goflags string) string {
	matches := modeFlag.FindAllStringSubmatch(goflags, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// goDirective reads the `go` line as the go command does: a version with an
// optional rc/beta suffix, and a trailing `//` comment. Read strictly, `go
// 1.23 // minimum` and `go 1.24rc1` did not match, the workspace was written
// at the floor, and the build stopped with "go.work lists go 1.21" on a tree
// that built on its own (code review 2026-11-11, K-2).
var goDirective = regexp.MustCompile(`(?m)^\s*go\s+([0-9]+(?:\.[0-9]+){1,2})(?:(?:rc|beta)[0-9]+)?\s*(?://.*)?$`)

// workspaceLanguage picks the `go` line for the workspace.
//
// A workspace may not declare less than any module it uses, and it uses two: the
// app, whose directive is whatever its author wrote, and the SDK, which sits at
// [GoLanguageFloor] precisely so it is never the higher of the pair. So the
// answer is the app's own directive whenever it is readable and above the floor,
// which also means the toolchain a workspace build selects is the one the app
// would have selected on its own.
func workspaceLanguage(goMod string) string {
	match := goDirective.FindStringSubmatch(goMod)
	if match == nil {
		return goWorkspaceFloor
	}
	if compareLanguage(match[1], goWorkspaceFloor) < 0 {
		return goWorkspaceFloor
	}
	return match[1]
}

// compareLanguage orders two Go language versions field by field. Not semver:
// "1.21" and "1.21.0" are the same version, and a missing field is zero.
func compareLanguage(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		if field(left, i) != field(right, i) {
			if field(left, i) < field(right, i) {
				return -1
			}
			return 1
		}
	}
	return 0
}

func field(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	value, _ := strconv.Atoi(parts[i])
	return value
}

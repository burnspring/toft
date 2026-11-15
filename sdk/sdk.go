// Package sdk carries the app SDKs inside the binaries that install them.
//
// Both build paths need the same files — `internal/builder` on the platform
// host, and the runner's own build steps inside the family image — and neither
// can fetch them. An on-prem install has no reason to reach PyPI or npm for a
// library that ships in the same release as the platform, and a version skew
// between the SDK an app imports and the API it is talking to is exactly the
// failure D10's contract header exists to make loud. Embedding is what keeps
// the two in step by construction.
//
// Only the shipped surface is embedded — the package and its manifest, not the
// test suites — because this is what goes into a customer's app tree.
package sdk

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"toft/internal/toolchain"
)

// Every file is named by a glob rather than by embedding the directory, and the
// difference is not style. `all:python/toft` would sweep in whatever the
// working tree happens to hold — `__pycache__` is gitignored, so a developer's
// checkout carries stale `.pyc` files that a clean one does not, and the payload
// shipped into every app would then depend on whose machine built the binary.
// A glob over `*.py` embeds the source and nothing else.
//
// A glob per directory, not a file list: naming files individually means a new
// module is added to the SDK, imported by `index.js`, and silently left out of
// every app's copy — which fails at the app's first `import`, not here. `*` does
// not descend, so `node/test` and `python/tests` stay out.
//
// Go is embedded by the same glob and needs one extra rule, in copyTree: the
// language keeps a package's tests beside its source, so `go/*.go` sweeps up
// `*_test.go` too and they are dropped on the way out. Its manifest is not a
// file here at all — see [GoModule].
//
//go:embed python/toft/*.py python/pyproject.toml
//go:embed node/*.js node/package.json
//go:embed go/*.go
//go:embed r/R/*.R r/DESCRIPTION r/NAMESPACE r/LICENSE
var files embed.FS

// RSDKDependencies is what the R package needs in order to load, installed
// beside it and never from the app's manifest.
//
// **R has no HTTP client and no JSON parser in its base library**, which is the
// one place this SDK cannot follow the Node one's dependency-free rule. Both of
// these are near-universal, both are served as binaries by any package
// repository TOFT_R_REPOS would point at, and installing them takes
// seconds — but it is a dependency the other SDKs do not have, and it is named
// here rather than buried in a DESCRIPTION so that the cost is visible.
//
// Installed before the app's own packages, so a version the app declares itself
// replaces these: the platform's copy is a floor, not a ceiling.
var RSDKDependencies = []string{"jsonlite", "curl"}

// GoModule is the import path a Go app writes, and GoLanguageFloor is the `go`
// directive the module declares.
//
// The manifest is generated rather than committed, because `sdk/go` cannot hold
// a `go.mod`: a directory with one is a different module, and `go:embed` refuses
// to reach into it — which would leave the payload unembeddable by the only
// mechanism both build paths have. Keeping the package inside the platform
// module has a second effect worth having: `scripts/check.sh` builds, vets and
// tests the SDK along with everything else, where a second module would need
// remembering. What it does *not* check is the floor, because the platform's own
// go.mod is far above it — [TestTheGoModuleBuildsAtItsDeclaredFloor] materializes
// the module and builds it for that.
//
// The floor is low on purpose. It has to sit at or below the `go` directive of
// every app that might import this, because the workspace's own directive is the
// higher of the two and a workspace may not declare less than a module it uses.
const (
	GoModule         = "toft"
	GoLanguageFloor  = "1.21"
	goWorkspaceFloor = "1.21"
)

// Materialize writes one language's SDK into a fresh directory under parent
// (the system temp directory when parent is empty) and returns its path, ready
// to be handed to `uv pip install`, which builds a wheel from it and *copies*
// the result — so the caller may remove the directory afterwards.
//
// It is written outside the release tree on purpose. Nothing the platform
// installs belongs in an app's own directory, where it would be captured by the
// release artifact, shipped to every workload, and quietly become part of the
// app's source.
func Materialize(parent, lang string) (string, error) {
	dir, err := os.MkdirTemp(parent, "toft-sdk-"+lang+"-")
	if err != nil {
		return "", err
	}
	if err := Write(dir, lang); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// Write puts one language's SDK at an exact path.
//
// This is how the Node SDK is installed, rather than `npm install <dir>`: npm
// records a local path as a `file:` dependency and **symlinks** it into
// `node_modules`, so the install would point at a temporary directory that no
// longer exists by the time the app runs — and the release tarball would carry
// a dangling link. Copying the tree in is what `node_modules/toft-sdk`
// means anyway, and it is valid precisely because the package is
// dependency-free by construction.
func Write(dest, lang string) error {
	shipped, err := payload(lang)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for path, body := range shipped {
		target := filepath.Join(dest, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Languages are the SDKs this binary carries, in the order a reader meets them,
// and **the list is the specification** of what `GET /api/v1/sdk/{lang}`
// answers: Files refuses anything else, so a directory that happens to sit in
// the embedded tree is never served by accident. R is on it — the builder
// installs it into every R release and `toft sdk vendor` detects an R tree — and
// was once missing, which made the route answer R with an empty package name.
func Languages() []string { return []string{"python", "node", "go", "r"} }

// Payload is one language's SDK as text, ready to travel.
//
// It exists because the SDK has two destinations and only one definition: the
// build writes it into a release, and `GET /api/v1/sdk/{lang}` hands the same
// bytes to `toft sdk vendor` so a developer's own tree can compile against the
// copy that shipped with the install they deploy to. A registry would have been
// the third destination and a fourth version.
type Payload struct {
	Language string `json:"language"`

	// Module is the Go import path; Package is the Python distribution or npm
	// name. One of the two, never both, because the languages name things
	// differently and a single field would make a reader guess which.
	Module  string `json:"module,omitempty"`
	Package string `json:"package,omitempty"`

	// LanguageFloor is the `go` directive of the generated module, which a
	// client needs to work out its own workspace's directive. Empty for the
	// languages that have no such thing.
	LanguageFloor string `json:"language_floor,omitempty"`

	// Files is path (slash-separated, relative) to contents. Text, because
	// every embedded file is source — see TestTheShippedSurfaceIsAllText, which
	// is what keeps this contract true rather than merely true today.
	Files map[string]string `json:"files"`

	// Build is [Build] for this language: which *copy* of the SDK this is, as
	// opposed to which version it declares. It is on the wire so that a
	// developer holding a vendored tree can compare it against the line the
	// build log printed when their app was built — the two come from different
	// binaries on an isolated install, and until this existed nothing could
	// tell them apart.
	//
	// Empty from an install older than the field, which means "this install
	// does not say", never "they match".
	Build string `json:"build,omitempty"`
}

// Build is a content identity for one language's SDK: twelve hex characters
// over the bytes this binary actually carries.
//
// **A version number cannot answer the question this does.** The SDK reaches an
// app by two independent paths — `internal/builder` on the platform host, and
// `internal/runner` inside the family image — and the two binaries are built
// and deployed separately, on purpose. An install upgraded on one side and not
// the other serves one SDK from `GET /api/v1/sdk/{lang}`, which is what `toft sdk
// vendor` hands a developer, and installs a different one into every app's
// venv. Both call themselves the same version, because a version is a
// declaration and both were made from a tree that declared it.
//
// So the identity is **derived, not declared**: a digest of the payload, which
// cannot be forgotten the way a bump can and cannot agree when the bytes
// differ. It is the same move `toft --version` made when it gained a build hash,
// for the same reason — two builds of one tag were indistinguishable.
//
// Twelve characters because this is read by people in a build log and compared
// by eye; it is not a security boundary, and nothing authenticates with it.
//
// Stable across machines and across runs: the paths are sorted, the length of
// each is written in, and nothing about the filesystem or the build host enters
// it. Two binaries built from one tree agree by construction, which is what
// makes a disagreement mean what it says.
func Build(lang string) (string, error) {
	shipped, err := payload(lang)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(shipped))
	for path := range shipped {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	sum := sha256.New()
	for _, path := range paths {
		// The path and the length both go in, so that moving a byte from the
		// end of one file to the start of the next cannot produce the same
		// digest as leaving it alone.
		fmt.Fprintf(sum, "%s\x00%d\x00", path, len(shipped[path]))
		sum.Write(shipped[path])
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

// VendoredGoDir is where `toft sdk vendor` writes the Go SDK, relative to the
// app's root.
const VendoredGoDir = ".toft/sdk-go"

// BuildOfDir is [Build] over a directory on disk — a copy `toft sdk vendor`
// wrote — hashed exactly as the payload is, so the two compare. It is what lets
// a Go app's build log name the copy it compiled against when the app brought
// its own: the platform stands aside then, and "not added" alone left the
// vendor command's "the deploy log names the copy" untrue.
//
// The directory is the publisher's, read on the platform's side of a build, so
// nothing is held in memory: each file is streamed into the hash, and a tree
// past maxVendoredBytes is not hashed at all — the vendored SDK is a few
// hundred kilobytes, and a copy that size is not one `toft sdk vendor` wrote.
func BuildOfDir(dir string) (string, error) {
	sizes := map[string]int64{}
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sizes[filepath.ToSlash(rel)] = info.Size()
		if total += info.Size(); total > maxVendoredBytes {
			return fmt.Errorf("%s holds more than %d bytes; not hashed", dir, maxVendoredBytes)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(sizes) == 0 {
		return "", fmt.Errorf("%s holds no files", dir)
	}
	paths := make([]string, 0, len(sizes))
	for p := range sizes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// Every file is re-opened through a root at dir, never by its path: the
	// tree belongs to the app's account by now, so a file the walk measured
	// can be swapped for a link out of the tree or a FIFO before it is opened
	// (security review 2026-11-14, PB-5).
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	sum := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(sum, "%s\x00%d\x00", p, sizes[p])
		if err := hashFile(sum, root, filepath.FromSlash(p), sizes[p]); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

// maxVendoredBytes bounds the tree BuildOfDir will hash.
const maxVendoredBytes = 16 << 20

// hashFile streams one file into the digest, refusing one whose length moved
// since it was measured: the length is already in the hash, and a digest over
// bytes that disagree with it would name no copy that exists.
//
// Opened beneath root, so a link leaving the tree is refused, and without
// blocking, so a FIFO swapped in since the walk answers at once rather than
// parking the build worker for good; the descriptor itself must then be a
// regular file of the measured size.
func hashFile(w io.Writer, root *os.Root, path string, size int64) error {
	f, err := root.OpenFile(path, os.O_RDONLY|toolchain.OpenNonblock, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file", path)
	}
	if info.Size() != size {
		return fmt.Errorf("%s changed while it was being hashed", path)
	}
	n, err := io.Copy(w, io.LimitReader(f, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while it was being hashed", path)
	}
	return nil
}

// GoSkippedLine is the build log's sentence for a Go app the platform did not
// give the SDK to, naming the app's own copy when it vendored one.
func GoSkippedLine(releaseDir, skipped string) string {
	line := "    The Toft SDK was not added: " + skipped + "."
	dir := filepath.Join(releaseDir, VendoredGoDir)
	id, err := BuildOfDir(dir)
	switch {
	case err == nil:
		line += fmt.Sprintf("\n    The app's own copy is build %s; this install carries %s.", id, Describe("go"))
	case errors.Is(err, fs.ErrNotExist):
		// Said rather than left out: a go.work naming a copy that did not ship
		// (an ignore rule on .toft/) is the likeliest reason, and silence was
		// all BB-059 (fifth 2026-11-03 black-box list) had to go on.
		line += fmt.Sprintf("\n    No copy of the SDK shipped at %s, so its build cannot be named (this install carries %s).",
			VendoredGoDir, Describe("go"))
	default:
		line += fmt.Sprintf("\n    The copy at %s could not be read to name its build: %v (this install carries %s).",
			VendoredGoDir, strings.TrimPrefix(err.Error(), releaseDir+string(filepath.Separator)), Describe("go"))
	}
	return line
}

// NodeKeptLine is the build log's sentence for a Node app that declares its own
// copy of the SDK, naming that copy's build as [GoSkippedLine] does for Go — so
// the "the deploy log names the copy" that `toft sdk vendor` prints is true for
// Node too (BB-085). A file: dependency is a symlink into the app's tree, which
// is resolved before hashing.
func NodeKeptLine(releaseDir string) string {
	line := "    Toft SDK: the app declares its own copy, which is kept."
	dir := nodeOwnCopy(releaseDir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if id, err := BuildOfDir(dir); err == nil {
		line += fmt.Sprintf("\n    The app's own copy is build %s; this install carries %s.", id, Describe("node"))
	} else {
		line += fmt.Sprintf("\n    Its build could not be named (this install carries %s).", Describe("node"))
	}
	return line
}

// BuildOrEmpty is Build for the callers that are describing something rather
// than deciding with it — a log line, a wire field. An SDK this binary cannot
// read is already reported by whatever tried to install it, and a second error
// from the line that was going to say which one it was helps nobody.
func BuildOrEmpty(lang string) string {
	id, err := Build(lang)
	if err != nil {
		return ""
	}
	return id
}

// Describe is what a build log says about the SDK it is installing: the version
// the package declares and the copy it actually is.
//
// One function so the two build paths cannot describe it differently — which
// is the whole subject: they are different binaries, and a reader comparing
// two build logs has to be comparing the same sentence.
func Describe(lang string) string {
	id := BuildOrEmpty(lang)
	if id == "" {
		return Version(lang)
	}
	return Version(lang) + "+" + id
}

// Version is the version one language's SDK declares, read out of the payload
// rather than restated here — a constant beside the file that carries it is a
// constant that documents rather than decides.
//
// Empty when the payload does not say, which for a reader is the same answer
// the SDK's own package gives.
func Version(lang string) string {
	shipped, err := payload(lang)
	if err != nil {
		return ""
	}
	for _, spelling := range []struct {
		file string
		re   *regexp.Regexp
	}{
		{"toft/__init__.py", regexp.MustCompile(`__version__ = "([^"]*)"`)},
		{"package.json", regexp.MustCompile(`"version":\s*"([^"]*)"`)},
		{"toft.go", regexp.MustCompile(`const Version = "([^"]*)"`)},
		{"DESCRIPTION", regexp.MustCompile(`(?m)^Version:\s*(\S+)`)},
	} {
		if body, ok := shipped[spelling.file]; ok {
			if m := spelling.re.FindSubmatch(body); m != nil {
				return string(m[1])
			}
		}
	}
	return ""
}

// Files is one language's shipped surface, as text.
func Files(lang string) (Payload, error) {
	if !slices.Contains(Languages(), lang) {
		return Payload{}, fmt.Errorf("no SDK for %q", lang)
	}
	shipped, err := payload(lang)
	if err != nil {
		return Payload{}, err
	}
	out := Payload{Language: lang, Files: make(map[string]string, len(shipped))}
	for path, body := range shipped {
		if !utf8.Valid(body) {
			return Payload{}, fmt.Errorf("the %s SDK carries a file that is not text: %s", lang, path)
		}
		out.Files[path] = string(body)
	}
	out.Build = BuildOrEmpty(lang)
	switch lang {
	case "go":
		out.Module, out.LanguageFloor = GoModule, GoLanguageFloor
	case "python":
		out.Package = "toft-sdk"
	case "node":
		out.Package = NodePackage
	case "r":
		out.Package = "toft"
	}
	if out.Package == "" && out.Module == "" {
		// Every language on the list names what it is installed as; one that
		// does not is a list that grew without this switch.
		return Payload{}, fmt.Errorf("the %s SDK has no package name", lang)
	}
	return out, nil
}

// payload is one language's shipped surface, path to contents.
//
// One definition, because [Write] puts it on disk and [Files] puts it on the
// wire, and the two must not disagree about what the SDK *is* — which is
// exactly the drift the embedding exists to prevent between an install and the
// app talking to it.
func payload(lang string) (map[string][]byte, error) {
	sub, err := fs.Sub(files, lang)
	if err != nil {
		return nil, fmt.Errorf("no embedded SDK for %q: %w", lang, err)
	}
	out := map[string][]byte{}
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// Go has nowhere else to keep a package's tests: they are `_test.go`
		// beside the code, inside the same glob, and they are not what goes
		// into a customer's app. Python and Node keep theirs in a directory the
		// glob does not descend into, so this costs them nothing.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		out[path] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no embedded SDK for %q", lang)
	}
	if lang == "go" {
		out["go.mod"] = []byte(fmt.Sprintf("module %s\n\ngo %s\n", GoModule, GoLanguageFloor))
	}
	return out, nil
}

// NodePackage is the name a Node app imports the SDK by, after the product as
// Python's `toft-sdk` and R's `toft` are, so a change of company costs nothing.
// NodeLegacyPackage is the name it had under the placeholder owner: installed
// beside it for at least one minor (0.10.x), so an app still importing it keeps
// working, and the build log says it is deprecated.
const (
	NodePackage       = "toft-sdk"
	NodeLegacyPackage = "@larchway/toft"
)

// NodeDeprecatedLine is the build log's sentence about the old name.
const NodeDeprecatedLine = "    Also installed as " + NodeLegacyPackage + ", which is deprecated: import " + NodePackage + " instead."

func nodeDir(releaseDir, pkg string) string {
	return filepath.Join(append([]string{releaseDir, "node_modules"}, strings.Split(pkg, "/")...)...)
}

// nodeOwnCopy is the copy an app brought, under whichever name it used; the
// new name when it brought none.
func nodeOwnCopy(releaseDir string) string {
	for _, pkg := range []string{NodePackage, NodeLegacyPackage} {
		dir := nodeDir(releaseDir, pkg)
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return dir
		}
	}
	return nodeDir(releaseDir, NodePackage)
}

// EnsureNode installs the Node SDK into a release's node_modules unless the app
// brought its own, and reports whether it did.
//
// The rule lives here rather than at the two call sites because both build
// paths — `internal/builder` natively, and the runner's build steps inside the
// family image — must agree about what is present in an app's environment. Two
// copies of "skip if present" is two places for that to drift.
//
// A presence check rather than ordering, which is what the Python side uses:
// `npm ci` empties node_modules and runs before this, so anything installed
// ahead of it would simply be erased. A copy under either name is the app's
// own; otherwise the SDK goes in under both, [NodePackage] and the deprecated
// [NodeLegacyPackage].
func EnsureNode(releaseDir string) (bool, error) {
	for _, pkg := range []string{NodePackage, NodeLegacyPackage} {
		if _, err := os.Stat(filepath.Join(nodeDir(releaseDir, pkg), "package.json")); err == nil {
			return false, nil
		}
	}
	for _, pkg := range []string{NodePackage, NodeLegacyPackage} {
		if err := Write(nodeDir(releaseDir, pkg), "node"); err != nil {
			return false, err
		}
	}
	return true, nil
}

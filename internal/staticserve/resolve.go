// Package staticserve is the whole of the static framework: the declaration a
// repo may write, the three decisions that declaration settles, and the HTTP
// file server that acts on them.
//
// It is one package because both binaries call it. The shared backend starts
// the *platform* binary as `serve-static`; an isolated pod starts the *runner*
// binary as `serve-static`. An app that behaved differently across execution
// modes would make the mode a one-way door, which is the warning both launchers
// already carry — so there is one implementation and neither launcher may grow
// serving logic of its own.
//
// Nothing here reaches a database, a config, or the network. A release
// directory in, an http.Handler out, which is what makes the whole framework
// testable with no process and no port.
package staticserve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// ManifestFile is the repo's own declaration, at the root of a release. Named
// here as well as in internal/builder because this package is compiled into the
// runner binary and must not import the builder to learn a filename.
const ManifestFile = "toft.toml"

// Declaration is the `[static]` table of toft.toml.
//
//	[static]
//	root     = "dist"
//	entry    = "report.html"
//	fallback = true
//
// Present is what makes the table itself meaningful: it is the signal that the
// author knows something the detector cannot, and it is what overrides the
// disqualifiers that otherwise keep a Vite repo out of static hosting.
type Declaration struct {
	Present  bool
	Root     string
	Entry    string
	Fallback bool
}

type manifestFile struct {
	Static *struct {
		Root     string `toml:"root"`
		Entry    string `toml:"entry"`
		Fallback bool   `toml:"fallback"`
	} `toml:"static"`
}

// DeclarationError names the key that is wrong, in the same shape and for the
// same reason internal/builder's ManifestError does: the developer is looking
// at a file they just edited.
type DeclarationError struct {
	Field string
	Msg   string
}

func (e DeclarationError) Error() string {
	where := ManifestFile + " [static]"
	if e.Field != "" {
		where += " " + e.Field
	}
	return where + ": " + e.Msg
}

// ReadDeclaration reads a release's `[static]` table.
//
// A missing file, or a file with no `[static]` table, is not an error — it is
// the ordinary case, and the answer is a zero Declaration with Present false.
// A file that cannot be parsed *is* an error, matching how the same file's
// `[jobs]` table is treated: accepting a manifest the platform half understood
// means a declaration silently not taking effect.
//
// The two path keys are validated here rather than at use, so a repo that names
// an absolute or escaping root is refused by the deploy that read it instead of
// by a file server three steps later.
func ReadDeclaration(releaseDir string) (Declaration, error) {
	data, err := readInRoot(releaseDir, ManifestFile)
	if os.IsNotExist(err) {
		return Declaration{}, nil
	}
	if err != nil {
		return Declaration{}, fmt.Errorf("reading %s: %w", ManifestFile, err)
	}

	var m manifestFile
	if _, err := toml.Decode(string(data), &m); err != nil {
		// A value of the wrong type is in a file that parses (D-27).
		if strings.Contains(err.Error(), "incompatible types") {
			return Declaration{}, DeclarationError{Msg: "has a value of the wrong type: " + err.Error()}
		}
		return Declaration{}, DeclarationError{Msg: "could not be parsed as TOML: " + err.Error()}
	}
	if m.Static == nil {
		return Declaration{}, nil
	}

	d := Declaration{
		Present:  true,
		Root:     strings.TrimSpace(m.Static.Root),
		Entry:    strings.TrimSpace(m.Static.Entry),
		Fallback: m.Static.Fallback,
	}
	// `root = "."` is the repository itself — what an omitted root means —
	// and is said that way by anyone serving a plain folder of HTML. Refusing
	// it ("must name something") also dropped the whole table at detection,
	// which swallows the error, so the same line in a Python tree was ignored
	// without a word and the app built as Python (BB-014).
	if r := filepath.ToSlash(filepath.Clean(d.Root)); d.Root != "" && r == "." {
		d.Root = ""
	}
	if d.Root != "" {
		if err := checkRelative("root", d.Root); err != nil {
			return Declaration{}, err
		}
	}
	if d.Entry != "" {
		if err := checkRelative("entry", d.Entry); err != nil {
			return Declaration{}, err
		}
		if !isHTML(d.Entry) {
			return Declaration{}, DeclarationError{Field: "entry",
				Msg: fmt.Sprintf("names %q, which is not an .html file", d.Entry)}
		}
	}
	return d, nil
}

// checkRelative refuses anything that is not a plain relative path inside the
// release. Rejected at the declaration rather than clamped at serve time: a
// root the platform silently rewrote would publish a directory the author did
// not name.
func checkRelative(field, value string) error {
	clean := filepath.ToSlash(filepath.Clean(value))
	switch {
	case filepath.IsAbs(value) || strings.HasPrefix(value, "/"):
		return DeclarationError{Field: field, Msg: fmt.Sprintf("must be a path inside the repository, and %q is absolute", value)}
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return DeclarationError{Field: field, Msg: fmt.Sprintf("must be a path inside the repository, and %q leaves it", value)}
	case clean == ".":
		return DeclarationError{Field: field, Msg: "must name something"}
	}
	return nil
}

func isHTML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".html" || ext == ".htm"
}

// Config is the settled answer for one release: where the document root is,
// what is served at the mount path, and whether unmatched paths fall back to
// it. Every field is derived from the release directory, which is what makes
// re-resolving at wake, at boot and in either execution mode return the same
// answer as the build did — there is no second place a value could live and
// disagree from.
type Config struct {
	// Root is the document root, as an absolute path.
	Root string
	// RootRel is the same directory relative to the release, for the log line.
	// "." when the release directory is itself the root.
	RootRel string
	// Entry is the document served at the mount path, relative to Root.
	Entry string
	// Fallback serves Entry, with status 200, for an unmatched path that looks
	// like a client route. Opt-in — see the package's ADR section.
	Fallback bool
	// Files is how many files the root publishes, for the log line. An operator
	// reading a deploy log should never have to guess what became public.
	Files int
}

// Resolve settles a release into a serving decision, or explains why it cannot.
//
// The entry document is resolved in one order and there is no fourth place to
// look: the declaration, then `index.html`, then the root's only `.html` file.
// A root with several candidate documents and no declaration is a *deploy
// failure* naming them — picking one alphabetically would deploy an app whose
// front page is somebody's guess, and picking none would deploy one whose front
// page is a 404.
func Resolve(releaseDir string) (Config, error) {
	decl, err := ReadDeclaration(releaseDir)
	if err != nil {
		return Config{}, err
	}
	return resolveWith(releaseDir, decl)
}

func resolveWith(releaseDir string, decl Declaration) (Config, error) {
	cfg := Config{RootRel: ".", Root: releaseDir, Fallback: decl.Fallback}

	if decl.Root != "" {
		cfg.RootRel = filepath.ToSlash(filepath.Clean(decl.Root))
		cfg.Root = filepath.Join(releaseDir, filepath.FromSlash(cfg.RootRel))
		info, err := lstatInRoot(releaseDir, filepath.FromSlash(cfg.RootRel))
		switch {
		case os.IsNotExist(err):
			// The declaration is right about intent and wrong about this
			// release — a `dist/` produced by a build the platform is not
			// running. Named, because "no such directory" is something the
			// author can act on and a blank page is not.
			return Config{}, DeclarationError{Field: "root",
				Msg: fmt.Sprintf("names %q, which this release does not contain", cfg.RootRel)}
		case err != nil:
			return Config{}, DeclarationError{Field: "root", Msg: err.Error()}
		case info.Mode()&fs.ModeSymlink != 0:
			// Refused rather than followed: the build reads the root as the
			// platform, and `dist -> /etc` would have it walk a host tree.
			return Config{}, DeclarationError{Field: "root",
				Msg: fmt.Sprintf("names %q, which is a symbolic link; name the directory itself", cfg.RootRel)}
		case !info.IsDir():
			return Config{}, DeclarationError{Field: "root",
				Msg: fmt.Sprintf("names %q, which is a file rather than a directory", cfg.RootRel)}
		}
	}

	root, err := openDocRoot(releaseDir, cfg.RootRel)
	if err != nil && decl.Root == "" && errors.Is(err, fs.ErrNotExist) {
		// Nothing declared a root, so nothing in [static] is wrong: the
		// directory handed to the server is missing — a document's render
		// output that was never written (BB-015). Blaming `[static] root`
		// sent the author to a table they never wrote.
		return Config{}, fmt.Errorf("the directory to serve, %s, does not exist — "+
			"for a rendered document, the render step did not produce it", filepath.Base(releaseDir))
	}
	if err != nil {
		return Config{}, DeclarationError{Field: "root", Msg: err.Error()}
	}
	defer root.Close()
	entry, err := resolveEntry(root, cfg.RootRel, decl.Entry)
	if err != nil {
		return Config{}, err
	}
	cfg.Entry = entry
	cfg.Files = countFiles(root)
	return cfg, nil
}

// Reading the release as the platform.
//
// The build resolves and lints a static release as the platform's uid, over a
// tree the repository supplied (and, after the handover, the app's account
// owns and can relink at any moment). So every read here goes through an
// os.Root on the release: a link that resolves inside it is the app's own
// content, and one that leaves it is refused — never a host file read as root
// into the build log (docs/invariants.md § Process and network boundaries).

// openDocRoot opens the document root, rootRel inside releaseDir, as an
// os.Root entered from the release's own, so a component that is a link out of
// the release is refused rather than followed.
func openDocRoot(releaseDir, rootRel string) (*os.Root, error) {
	release, err := os.OpenRoot(releaseDir)
	if err != nil {
		return nil, err
	}
	if rootRel == "" || rootRel == "." {
		return release, nil
	}
	defer release.Close()
	return release.OpenRoot(filepath.FromSlash(rootRel))
}

func readInRoot(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}

func lstatInRoot(dir, name string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Lstat(name)
}

func resolveEntry(root *os.Root, rootRel, declared string) (string, error) {
	if declared != "" {
		rel := filepath.ToSlash(filepath.Clean(declared))
		info, err := root.Stat(filepath.FromSlash(rel))
		if err != nil || info.IsDir() {
			return "", DeclarationError{Field: "entry",
				Msg: fmt.Sprintf("names %q, which is not a file in %s", rel, describeRoot(rootRel))}
		}
		return rel, nil
	}

	if info, err := root.Stat("index.html"); err == nil && !info.IsDir() {
		return "index.html", nil
	}

	// A folder holding one HTML document has an unambiguous front page, and
	// refusing to serve it would be pedantry.
	candidates := topLevelHTML(root)
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("%s holds no HTML document to serve. Add an index.html, "+
			"or name the entry document in %s:\n\n    [static]\n    entry = \"report.html\"",
			describeRoot(rootRel), ManifestFile)
	default:
		return "", fmt.Errorf("%s holds %d HTML documents (%s) and no index.html, so there is "+
			"no front page to serve. Name one in %s:\n\n    [static]\n    entry = %q",
			describeRoot(rootRel), len(candidates), strings.Join(candidates, ", "),
			ManifestFile, candidates[0])
	}
}

func describeRoot(rootRel string) string {
	if rootRel == "." || rootRel == "" {
		return "this repository"
	}
	return rootRel + "/"
}

// topLevelHTML lists the .html files directly in the root, sorted. Only the top
// level: a documentation build has an HTML file in every directory, and none of
// them is a claim about the front page.
func topLevelHTML(root *os.Root) []string {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || Refused(e.Name()) || !isHTML(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// countFiles counts what the root would publish, refusals excluded, so the
// figure in the deploy log is the number of files that actually became
// reachable. Best-effort: an unreadable subtree is skipped rather than failing
// a deploy over a log line.
func countFiles(root *os.Root) int {
	n := 0
	_ = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == "." {
			return nil
		}
		if Refused(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

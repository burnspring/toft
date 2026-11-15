package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"toft/cli/internal/bundle"
	"toft/cli/internal/client"

	"toft/cli/internal/ui"
)

// `toft sdk vendor` — the install's own app SDK, written into your tree.
//
// The SDK ships inside the platform binary and is installed by the build, which
// is what keeps an app's copy in step with the API it is talking to. The cost is
// that an app's own tooling cannot see it until the platform has built the app —
// and in Go that is not a nuisance but a stop: nothing resolves `toft`,
// so `go build`, `go vet`, `gopls` and the app's own CI all fail on the import
// line.
//
// This is the answer, and the thing it deliberately is not is a registry. A
// published package would be a *second* version of the SDK, and a developer
// would then be compiling against whichever their package manager resolved
// rather than against the install they deploy to. What this writes comes from
// `GET /api/v1/sdk/{lang}` on the install named by the ordinary resolution
// order — so the bytes on your disk are the bytes that install's build would
// have used, by construction.
//
// **What it writes is meant to be deployable, not hidden.** For Go it also
// writes a `go.work`, and both that and the vendored module ship with
// `toft deploy` — at which point the platform's own copy stands aside, because an
// app that brought its own keeps it. That is the same floor-not-ceiling rule
// Python and Node already have, and it is why the two paths must be ignored
// together or not at all: ignoring only `go.work` leaves a workspace on the
// platform pointing at a directory that did not ship.

const sdkUsage = `toft sdk — the app SDK this install carries

Usage:
  toft sdk vendor [flags]        Write it into this directory

Flags:
  --lang <go|python|node|r> Which SDK. Detected from the tree when absent.
  --dir <path>              Where to write it. Default .toft
  --json                    Machine-readable output on stdout

The SDK is installed by the build, so a deployed app never needs this. What it
is for is everything before the deploy: compiling, vetting and testing on your
own machine, and an editor that can see the package.

What lands where:

  go       .toft/sdk-go/ and a go.work naming it. No require line is
           added — a workspace resolves the import without one.
  python   .toft/sdk-python/, then: uv pip install -e .toft/sdk-python
           It is added to .gitignore, so it does not ship: the platform
           installs its own copy.
  node     .toft/sdk-node/, then add to package.json:
             "toft-sdk": "file:.toft/sdk-node"
           It ships, because package.json names it, and the platform keeps
           the app's copy.
  r        .toft/sdk-r/, an R package source tree, then install it into the
           library you develop against: R CMD INSTALL .toft/sdk-r
           (library(toft) then finds it). It is added to .gitignore, so it
           does not ship: the platform installs its own copy.

For Go, go.work and .toft/ ship together with toft deploy and the platform
then stands aside — an app that brought its own SDK keeps it. Ignore both in
.gitignore or neither: ignoring only one is the case that builds here and fails
there.

Re-run it after upgrading the install. Delete both paths to go back to the
platform's own copy.
`

func cmdSDK(e *env, args []string) error {
	if len(args) == 0 {
		ui.Help(os.Stderr, sdkUsage)
		return usageCodef("missing_argument", "toft sdk needs a subcommand: vendor")
	}
	switch args[0] {
	case "-h", "--help", "help":
		ui.Help(os.Stdout, sdkUsage)
		return nil
	case "vendor":
		return cmdSDKVendor(e, args[1:])
	}
	return unknownSubcommand("sdk", args, "vendor")
}

// sdkPayload is the install's answer: one language's shipped surface as text.
type sdkPayload struct {
	Language      string `json:"language"`
	Module        string `json:"module"`
	Package       string `json:"package"`
	LanguageFloor string `json:"language_floor"`
	Version       string `json:"version"`
	// SDKVersion is the version the SDK itself declares (`0.3.0`), where
	// Version is the install's. Empty from an install older than the field.
	SDKVersion string `json:"sdk_version"`
	// Build is which *copy* of the SDK this is, as opposed to which version it
	// declares — see sdk.Build. Worth printing because on an isolated install
	// the SDK an app is built with comes from the runner image and this one
	// comes from the platform, and the two upgrade separately: they can be
	// different code under one version, and this is the only thing that says
	// so. The build log prints the same identifier for the copy that was
	// actually installed, so the two can be compared.
	//
	// Empty from an install older than the field, which means "this install
	// does not say" and never "they match".
	Build string            `json:"build"`
	Files map[string]string `json:"files"`
}

func cmdSDKVendor(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("sdk vendor", sdkUsage)
	g.register(fs)
	lang := fs.String("lang", "", "which SDK")
	dir := fs.String("dir", ".toft", "where to write it")
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	chosen := strings.ToLower(strings.TrimSpace(*lang))
	if chosen == "" {
		chosen, err = detectLanguage(e.dir)
		if err != nil {
			return err
		}
	}

	var payload sdkPayload
	if err := s.client.Do(http.MethodGet, "/api/v1/sdk/"+chosen, nil, &payload); err != nil {
		// A language this install does not carry is a flag the caller got
		// wrong, not the platform failing — and the exit code is the only part
		// of that a pipeline reads. The set is the *install's*, so it is not
		// checked here: a client with its own copy of the list would refuse a
		// language a newer install had learned.
		return sdkLanguageError(err)
	}

	// Relative to the working directory on purpose: a go.work carrying an
	// absolute path breaks for the next person who clones the repository, and
	// breaks on the platform if it ships.
	target := filepath.Join(*dir, "sdk-"+chosen)
	written, err := writeSDKFiles(e.dir, target, payload.Files)
	if err != nil {
		return err
	}

	note := ""
	if chosen == "go" {
		note, err = writeGoWorkspace(e.dir, target, payload.LanguageFloor)
		if err != nil {
			return err
		}
	}
	// Python's and R's copies are for the developer's own environment and nothing in the
	// app names it, so it is kept out of the deploy — by .gitignore, the one
	// place the bundler reads that decision from, rather than by a rule hidden
	// in the bundler.
	ignored := ""
	if chosen == "python" || chosen == "r" {
		ignored, err = ignoreVendored(e.dir, target)
		if err != nil {
			return err
		}
	}

	if g.json {
		out := map[string]any{
			"language": payload.Language,
			// `version` stays the install's, as it always was; the two
			// named fields say which number is which.
			"version":         payload.Version,
			"install_version": payload.Version,
			"dir":             filepath.ToSlash(target),
			"files":           written,
		}
		// Absent rather than empty from an install that does not report one, so
		// a consumer testing for the key learns that instead of reading "" as
		// an identity.
		if payload.Build != "" {
			out["build"] = payload.Build
		}
		if payload.SDKVersion != "" {
			out["sdk_version"] = payload.SDKVersion
		}
		if ignored != "" {
			out["gitignore"] = ignored
		}
		return json.NewEncoder(e.out.Stdout).Encode(out)
	}

	if e.out.Rich() {
		e.out.Answer("%s Wrote the %s SDK (%s) to %s %s", e.out.OK("✓"),
			payload.Language, e.out.Bold(sdkName(payload)), e.out.Bold(filepath.ToSlash(target)),
			e.out.Dimmed(fmt.Sprintf("— %d %s, from %s", written, plural(written, "file", "files"), s.InstallURL)))
	} else {
		e.out.Answer("Wrote the %s SDK (%s) to %s — %d %s, from %s",
			payload.Language, e.out.Bold(sdkName(payload)), e.out.Bold(filepath.ToSlash(target)),
			written, plural(written, "file", "files"), s.InstallURL)
	}
	if payload.Build != "" {
		// The number to compare against the build log. On an isolated install
		// this copy comes from the platform and the one your app is built with
		// comes from the runner image, and a version alone cannot tell them
		// apart — which is how a fix could ship, be vendored, and not be in the
		// deployed app.
		e.out.Dim("  Build %s — the deploy log names the copy your app was built "+
			"with; they should match.", payload.Build)
	}
	if note != "" {
		e.out.Say("%s", note)
	}
	for _, line := range sdkNextSteps(chosen, target) {
		// The indented lines are the commands; on a terminal they are drawn
		// as the next thing to run.
		if e.out.Rich() && strings.HasPrefix(line, "    ") {
			e.out.Next("%s", strings.TrimSpace(line))
			continue
		}
		e.out.Say("%s", line)
	}
	switch {
	case ignored != "":
		e.out.Dim("Added %s to .gitignore — the deploy does not ship it, and the platform "+
			"installs its own copy.", ignored)
	case chosen == "node":
		e.out.Dim("%s", nodeShipsLine(e.dir, filepath.ToSlash(target)))
	}
	if warning := shippingWarning(e.dir, *dir, chosen); warning != "" {
		e.out.Warn("%s", warning)
	}
	return nil
}

// nodeShipsLine says why the vendored Node SDK ships — and says it truly: a
// tree with no package.json, or one that does not name the copy yet, was told
// "because package.json names it" (BB-050).
func nodeShipsLine(root, target string) string {
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	switch {
	case err != nil:
		return fmt.Sprintf("There is no package.json here yet. Once one names %s as above, it ships "+
			"with toft deploy and the platform keeps the app's copy rather than installing its own.", target)
	case !strings.Contains(string(raw), "file:"+target) && !strings.Contains(string(raw), "file:./"+target):
		return fmt.Sprintf("package.json does not name %s yet. Once it does, as above, it ships with "+
			"toft deploy and the platform keeps the app's copy rather than installing its own.", target)
	}
	return fmt.Sprintf("%s ships with toft deploy, because package.json names it; the platform "+
		"then keeps the app's copy rather than installing its own.", target)
}

// sdkLanguageError turns "this install carries no such SDK" into a usage error.
//
// The set is the *install's*, so it is not checked client-side: a client with
// its own copy of the list would refuse a language a newer install had learned.
func sdkLanguageError(err error) error {
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Code == "sdk_language_unknown" {
		// The hint in this client's own words: an older install
		// named `toft sdk python`, a command that does not exist.
		fixed := *apiErr
		fixed.Hint = "pass --lang with one of the languages the sentence lists, like `toft sdk vendor --lang python`"
		return &usageError{msg: apiErr.Message, code: apiErr.Code, from: &fixed}
	}
	return err
}

func sdkName(p sdkPayload) string {
	if p.Module != "" {
		return p.Module
	}
	return p.Package
}

// detectLanguage reads the tree the way the platform's own detector does, so a
// developer never has to name what the build works out for itself.
//
// It refuses rather than guessing when a tree is two things at once — a Go
// service with a bundled frontend is the ordinary case, and picking one of them
// silently is how somebody vendors the wrong SDK and reads a compile error
// about the other.
func detectLanguage(dir string) (string, error) {
	found := []string{}
	if fileThere(dir, "go.mod") {
		found = append(found, "go")
	}
	if fileThere(dir, "package.json") {
		found = append(found, "node")
	}
	for _, name := range []string{"requirements.txt", "pyproject.toml", "Pipfile"} {
		if fileThere(dir, name) {
			found = append(found, "python")
			break
		}
	}
	if rTree(dir) {
		// **R is added after python and then removes it**, because that is the
		// platform's own order: detectR runs before detectPython, so a tree
		// holding both an app.R and a requirements.txt is an R app there and
		// must be an R app here. Left as an ambiguity this would refuse to
		// vendor anything for the commonest R repository shape — an R app whose
		// data step is a Python script.
		found = append(found, "r")
		found = slices.DeleteFunc(found, func(l string) bool { return l == "python" })
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", usageCodef("unknown_language", "nothing here says what language this app is — no go.mod, "+
			"package.json, requirements.txt, pyproject.toml or renv.lock. Name it with --lang.")
	default:
		sort.Strings(found)
		return "", usageCodef("ambiguous_language", "this tree looks like %s at once, so which SDK you want is "+
			"yours to say: --lang %s", strings.Join(found, " and "), found[0])
	}
}

// rTree reports whether this is an R app, by the markers the platform's own
// detector keys off — the two process shapes and the two document ones. renv.lock
// is in the list because it is the only one of these that is *required* for a
// deployable R app: without it the build installs nothing and the app fails at
// its first library() call, so a tree carrying one is an R app whatever else it
// holds.
//
// Written out rather than imported: cli/ is a second module and cannot see
// internal/builder. Kept deliberately coarse — this answers "which SDK", not
// "which framework", and the four R shapes all take the same one.
func rTree(dir string) bool {
	for _, name := range []string{"renv.lock", "app.R", "plumber.R"} {
		if fileThere(dir, name) {
			return true
		}
	}
	if fileThere(dir, "ui.R") && fileThere(dir, "server.R") {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".rmd", ".qmd":
			return true
		}
	}
	return false
}

func fileThere(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}

// writeSDKFiles puts the payload on disk under root/target, refusing any path
// that would land outside it.
//
// The install is trusted and the check is here anyway: a path is the one part of
// this response that names a location on *this* machine, and a client that
// writes wherever it is told is a client that a compromised install can write
// through.
func writeSDKFiles(root, target string, files map[string]string) (int, error) {
	// An absolute target is where the person said: filepath.Join does not let
	// a later absolute element win, so `--dir /tmp/sdk` landed under the
	// working directory while every line printed named /tmp/sdk (code review
	// 2026-11-11, M2-3).
	base := filepath.Join(root, target)
	if filepath.IsAbs(target) {
		base = target
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return 0, err
	}
	for path, body := range files {
		clean := filepath.Clean(filepath.FromSlash(path))
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return 0, fmt.Errorf("the install sent a file path that would land outside %s: %s", target, path)
		}
		full := filepath.Join(base, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}

// writeGoWorkspace creates the go.work that makes the import resolve, or
// explains why it left one alone.
//
// A workspace rather than a require line, for the reason the platform's build
// uses one: it resolves the import without touching the app's own go.mod. Its
// `go` directive is the higher of the app's own and the floor the install
// reported, because a workspace may not declare less than a module it uses.
func writeGoWorkspace(root, target, floor string) (string, error) {
	if floor == "" {
		floor = "1.21"
	}
	// A go.work carries a relative path (see the caller): an absolute target
	// is written relative to the tree when it is inside it, and refused when
	// it is not — `use //tmp/sdk/sdk-go` was neither (M2-3).
	if filepath.IsAbs(target) {
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", fmt.Errorf("the Go SDK has to be vendored inside this tree for go.work to use it: %s is outside %s", target, root)
		}
		target = rel
	}
	use := "./" + filepath.ToSlash(target)
	path := filepath.Join(root, "go.work")

	if existing, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(existing), use) {
			return "go.work already uses it.", nil
		}
		// Not rewritten. A workspace an app wrote is a file with its own
		// reasons — a second module in the repository, most often — and merging
		// into it by hand is how a client corrupts one.
		return "go.work is already here and was left alone. Add the SDK to it with:\n" +
			"    go work use " + use, nil
	}

	declared := floor
	if mod, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		if version := goDirectiveOf(string(mod)); version != "" && compareGoVersions(version, floor) > 0 {
			declared = version
		}
	}
	body := fmt.Sprintf("go %s\n\nuse (\n\t.\n\t%s\n)\n", declared, use)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return "Wrote go.work. No require line was added — a workspace resolves the import without one.", nil
}

var goDirective = regexp.MustCompile(`(?m)^\s*go\s+([0-9]+(?:\.[0-9]+){1,2})\s*$`)

func goDirectiveOf(goMod string) string {
	if m := goDirective.FindStringSubmatch(goMod); m != nil {
		return m[1]
	}
	return ""
}

// compareGoVersions orders two Go language versions field by field. Not semver:
// "1.21" and "1.21.0" are the same version, and a missing field is zero.
func compareGoVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		l, r := goField(left, i), goField(right, i)
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
}

func goField(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}

func sdkNextSteps(lang, target string) []string {
	slash := filepath.ToSlash(target)
	switch lang {
	case "python":
		return []string{"Install it into the environment you develop in:",
			"    uv pip install -e " + slash + "    # or: pip install -e " + slash}
	case "node":
		return []string{"Point package.json at it, then npm install:",
			`    "toft-sdk": "file:` + slash + `"`}
	case "r":
		return []string{"Install it into the library you develop against:",
			"    R CMD INSTALL " + slash}
	}
	return nil
}

// ignoreVendored appends the vendored directory to the tree's .gitignore unless
// an ignore rule already covers it, and returns the line it added ("" when it
// added none). A directory outside the tree is never bundled and is left alone.
func ignoreVendored(root, target string) (string, error) {
	if filepath.IsAbs(target) {
		return "", nil
	}
	rel := filepath.ToSlash(filepath.Clean(target))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", nil
	}
	m := &bundle.Matcher{}
	_ = m.AddFile("", filepath.Join(root, ".gitignore"))
	_ = m.AddFile("", filepath.Join(root, ".toftignore"))
	if ignoredOrUnder(m, rel) {
		return "", nil
	}
	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	line := "/" + rel + "/"
	var b strings.Builder
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	b.WriteString("# toft sdk vendor: a local copy for your editor; the platform installs its own\n")
	b.WriteString(line + "\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return "", err
	}
	return line, nil
}

// ignoredOrUnder is whether an ignore rule excludes the directory rel or any
// folder it sits in: `/.toft/` already covers .toft/sdk-python, as git reads
// it, and the bundle never walks into an excluded folder. Asking about rel
// alone appended a second line under a rule that had it covered.
func ignoredOrUnder(m *bundle.Matcher, rel string) bool {
	parts := strings.Split(rel, "/")
	for i := range parts {
		if m.Excluded(strings.Join(parts[:i+1], "/"), true) {
			return true
		}
	}
	return false
}

// shippingWarning catches the one arrangement that builds here and fails on the
// platform: a Go tree whose vendored SDK is ignored while its go.work is not.
//
// The two are one decision. Shipping both is the ordinary answer and the
// platform then stands aside; shipping neither is the ordinary answer too, and
// the platform installs its own copy. Shipping only the workspace leaves a `use`
// directive naming a directory that did not arrive.
func shippingWarning(root, dir, lang string) string {
	if lang != "go" {
		return ""
	}
	m := &bundle.Matcher{}
	_ = m.AddFile("", filepath.Join(root, ".gitignore"))
	_ = m.AddFile("", filepath.Join(root, ".toftignore"))

	vendored := ignoredOrUnder(m, filepath.ToSlash(filepath.Clean(dir)))
	workspace := m.Excluded("go.work", false)
	if vendored == workspace {
		return ""
	}
	if vendored {
		return dir + " is ignored but go.work is not. On the platform the workspace " +
			"would name a directory that did not ship, and the build would fail — ignore " +
			"go.work too, or stop ignoring " + dir + "."
	}
	return "go.work is ignored but " + dir + " is not. The vendored SDK would ship " +
		"unused, which is harmless — ignore both if you meant to keep them local."
}

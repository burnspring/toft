package builder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"toft/internal/staticserve"
	"toft/internal/toolchain"
)

// Notebooks, both halves of them.
//
// Two frameworks live here, and the boundary between them is *who executes the
// code and when*. A `.ipynb` or a `.qmd` is executed once, at build time, and
// what gets served is the document that fell out — a `serve-static` workload
// over a directory the platform owns. A marimo notebook is a program: it is
// executed per viewer session, by a `marimo run` process, in exactly the shape
// Streamlit already has.
//
// They are not two modes of one framework. They have different sources,
// different workloads, different launchers, and they disagree at six of the
// fourteen places this codebase switches on `app.framework`. A mode key that
// changed all of that would be a framework wearing a disguise.
//
// **This file decides nothing about the install.** Which engine a release wants
// is a property of the release; whether this install *has* that engine is a
// question for the build, and asking it here would make detection answer
// differently depending on configuration — while detection re-runs against the
// release directory at every wake, every crash restore and every boot.

// The two render engines. There is no third, and there is no "auto": the
// engine is decided by the file, then by declaration, and never assumed (D4).
const (
	// EngineNBConvert is the default for a `.ipynb`, because it is what
	// `File → Export as HTML` produces — so the deployed page matches the one
	// the author already looked at.
	EngineNBConvert = "nbconvert"
	// EngineQuarto is what a `.qmd` is rendered with, and what an `.ipynb` is
	// rendered with when the repo declares it. It is discovered where the build
	// runs and is never a dependency of the app.
	EngineQuarto = "quarto"
)

// RenderDir is the directory the render writes into, relative to the release,
// and the document root of the workload that serves it.
//
// A fixed path the platform owns, in keeping with the other things it writes
// beside an app's own files — `venv/`, `toft-app`,
// `next.config.toft-user.*`. Not a `[static] root` written into the
// author's manifest: the platform does not edit a file the developer wrote, and
// the next `git pull` would show a diff nobody made.
//
// **Never the release directory.** `staticserve` publishes everything under its
// root that is not dot-prefixed and its refusal list is not configurable —
// rooting a notebook's file server at the release would publish
// `requirements.txt`, the notebook source, and the whole of `venv/`.
//
// Spelled again in internal/runtime and internal/runner, which start the
// workload and must not import this package. `notebook_test.go` walks both
// launchers against this constant so the three cannot drift.
const RenderDir = "toft-render"

// RenderEntry is the document the render produces inside RenderDir.
// `staticserve.Resolve` finds it on its own, with no declaration anywhere.
const RenderEntry = "index.html"

// renderMarker is written inside RenderDir by a successful render, and is what
// tells the platform's own output apart from a directory of that name the
// author committed.
//
// The distinction is load-bearing in one direction each way. A committed
// `toft-render/` must fail the deploy by name rather than being silently
// overwritten. A *repair* — a shared-mode release whose venv no longer resolves,
// rebuilt in place — re-runs the same build and must be allowed to replace the
// render it produced last time, or every repair of a notebook app would refuse
// itself.
//
// Dot-prefixed, so `staticserve.Refused` keeps it out of what the app publishes.
const renderMarker = ".toft-render"

// notebookExtensions are the executable-document sources, in no order of
// preference — a release carrying several of them is ambiguous whichever kinds
// they are.
var notebookExtensions = []string{".ipynb", ".qmd"}

// NotebookPlan is the settled answer for one release: which file is executed,
// and by what. Every field is derived from the release directory, which is what
// makes re-resolving at wake, at boot and in either execution mode return the
// same answer the build got.
type NotebookPlan struct {
	// Framework is FrameworkNotebook or FrameworkMarimo.
	Framework Framework
	// File is the source, slash-relative to the release directory.
	File string
	// Engine is EngineNBConvert or EngineQuarto for a notebook, and "" for
	// marimo — which is a framework, not an engine.
	Engine string
}

// NotebookError is a release that claimed one of these frameworks and cannot be
// served as one. It carries the framework because the caller has to know *which*
// half refused before it has a plan to read it off.
//
// Same shape and same reason as staticserve.DeclarationError and
// builder.ManifestError: the developer is looking at a file they just edited,
// and naming the key and the reason is the difference between a fix and a guess.
type NotebookError struct {
	Framework Framework
	Field     string
	Msg       string
}

func (e NotebookError) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return ManifestFile + " [notebook] " + e.Field + ": " + e.Msg
}

// notebookDeclaration is the `[notebook]` table of toft.toml.
//
//	[notebook]
//	file   = "reports/weekly.qmd"
//	engine = "quarto"
//
// Both keys are optional. `file` is how a repo holding several documents says
// which one is the app, and it is also what makes a notebook in a subdirectory
// reachable — the bare rule is root-only. `engine` is how an `.ipynb` asks for
// Quarto instead of the default.
type notebookDeclaration struct {
	Present bool
	File    string
	Engine  string
}

// readNotebookDeclaration reads the `[notebook]` table, and treats a manifest it
// cannot parse as no table at all.
//
// Swallowing the parse error is deliberate and is what `[static]` detection
// already does: a malformed toft.toml is refused, with the line number, by
// ParseManifest during the same deploy. Refusing it twice in two different
// vocabularies would mean a TOML typo could report itself as "this is not a
// notebook".
func readNotebookDeclaration(releaseDir string) notebookDeclaration {
	data, err := readInRelease(releaseDir, ManifestFile)
	if err != nil {
		return notebookDeclaration{}
	}
	var m struct {
		Notebook *struct {
			File   string `toml:"file"`
			Engine string `toml:"engine"`
		} `toml:"notebook"`
	}
	if _, err := toml.Decode(string(data), &m); err != nil || m.Notebook == nil {
		return notebookDeclaration{}
	}
	return notebookDeclaration{
		Present: true,
		File:    strings.TrimSpace(m.Notebook.File),
		Engine:  strings.TrimSpace(m.Notebook.Engine),
	}
}

// ResolveNotebook settles a release into a notebook plan, or explains why it
// cannot.
//
// Three outcomes, and they are three: (nil, nil) is "this release is not a
// notebook of either kind", which is the ordinary answer for every other app on
// the install. A NotebookError is "it claimed to be one and cannot be served" —
// several candidates, a declared file that is not there, an engine that is not a
// name — and it fails the deploy. A plan is the answer.
func ResolveNotebook(releaseDir string) (*NotebookPlan, error) {
	decl := readNotebookDeclaration(releaseDir)
	docs := notebookCandidates(releaseDir)
	marimos := marimoCandidates(releaseDir)

	// The two halves cannot both be true of one release, and the deploy says
	// so rather than picking. A repo holding a Jupyter notebook *and* a marimo
	// program is two apps, and guessing which one the author meant would deploy
	// the other one silently.
	if len(marimos) > 0 && (decl.Present || len(docs) > 0) {
		return nil, NotebookError{Framework: FrameworkNotebook, Msg: fmt.Sprintf(
			"this release holds a marimo notebook (%s) and %s. Those are two different "+
				"frameworks — a marimo notebook is executed for each viewer, and a %s is "+
				"executed once at build time and served as a document — so deploy them as "+
				"two apps",
			strings.Join(marimos, ", "), describeDocs(decl, docs), notebookKinds())}
	}

	if len(marimos) > 1 {
		return nil, NotebookError{Framework: FrameworkMarimo, Msg: fmt.Sprintf(
			"this release holds %d marimo notebooks (%s), so there is no app to serve. "+
				"Deploy one of them, or move the others out of the repository root",
			len(marimos), strings.Join(marimos, ", "))}
	}
	if len(marimos) == 1 {
		return &NotebookPlan{Framework: FrameworkMarimo, File: marimos[0]}, nil
	}

	if !decl.Present && len(docs) == 0 {
		return nil, nil
	}

	// R Markdown is the document half of R (rdocument.go), and the
	// `[notebook]` table speaks for it too: `file` picks one among several or
	// reaches one in a subdirectory, and `knitr` is its engine. It is not a
	// notebook plan — rmarkdown::render renders it, not this file — so the
	// table is checked here and the answer is "not a notebook": refused as
	// "not a .ipynb or a .qmd" before, for a layout the same table accepts
	// with a .qmd (BB-001).
	if isRmd(decl.File) || (decl.File == "" && len(docs) == 0 && len(rSourceFiles(releaseDir, ".Rmd")) > 0) {
		return nil, checkRmdDeclaration(releaseDir, decl)
	}

	file, err := resolveNotebookFile(releaseDir, decl, docs)
	if err != nil {
		return nil, err
	}
	engine, err := resolveEngine(decl, file)
	if err != nil {
		return nil, err
	}
	return &NotebookPlan{Framework: FrameworkNotebook, File: file, Engine: engine}, nil
}

// resolveNotebookFile answers "which document is this app", in one order and
// with no fourth place to look: the declaration, then the release root's only
// candidate, then a refusal naming them all.
func resolveNotebookFile(releaseDir string, decl notebookDeclaration, docs []string) (string, error) {
	if decl.File != "" {
		// Validated with the same shape `[static] root` uses, and for the same
		// reason: a path the platform silently rewrote would execute a file the
		// author did not name.
		if err := checkNotebookPath(decl.File); err != nil {
			return "", err
		}
		rel := filepath.ToSlash(filepath.Clean(decl.File))
		if !hasNotebookExtension(rel) {
			return "", NotebookError{Framework: FrameworkNotebook, Field: "file", Msg: fmt.Sprintf(
				"names %q, which is not %s or an .Rmd", rel, notebookKinds())}
		}
		info, err := os.Stat(filepath.Join(releaseDir, filepath.FromSlash(rel)))
		if err != nil || info.IsDir() {
			return "", NotebookError{Framework: FrameworkNotebook, Field: "file", Msg: fmt.Sprintf(
				"names %q, which this release does not contain", rel)}
		}
		return rel, nil
	}

	switch len(docs) {
	case 1:
		return docs[0], nil
	case 0:
		return "", NotebookError{Framework: FrameworkNotebook, Msg: fmt.Sprintf(
			"this release declares a [notebook] table and holds no .ipynb, .qmd or .Rmd at its root. "+
				"Name the document in %s:\n\n    [notebook]\n    file = \"reports/weekly.qmd\"",
			ManifestFile)}
	default:
		return "", NotebookError{Framework: FrameworkNotebook, Msg: fmt.Sprintf(
			"this release holds %d notebooks (%s), so there is no document to serve. "+
				"Name one in %s:\n\n    [notebook]\n    file = %q",
			len(docs), strings.Join(docs, ", "), ManifestFile, docs[0])}
	}
}

// resolveEngine is D4's table, and it is short on purpose: the file decides,
// then the declaration, and nothing is assumed.
//
//	.ipynb   nbconvert, unless the repo declares quarto
//	.qmd     quarto — there is no other engine for it
func resolveEngine(decl notebookDeclaration, file string) (string, error) {
	ext := strings.ToLower(filepath.Ext(file))

	if decl.Engine != "" {
		switch decl.Engine {
		case EngineNBConvert:
			if ext == ".qmd" {
				return "", NotebookError{Framework: FrameworkNotebook, Field: "engine", Msg: fmt.Sprintf(
					"is %q, and %q is a Quarto document — nbconvert cannot render one. "+
						"Remove the key, or rename the file to .ipynb if it really is a notebook",
					EngineNBConvert, file)}
			}
			return EngineNBConvert, nil
		case EngineQuarto:
			return EngineQuarto, nil
		case EngineJupyter:
			// Quarto's Python engine: the file's own default, said out loud
			// (rdocument.go). Refused as "not an engine" before, with the
			// value the hint itself offered (BB-012).
		case EngineKnitr:
			// Quarto's R engine, which only a Quarto document has.
			if ext != ".qmd" {
				return "", NotebookError{Framework: FrameworkNotebook, Field: "engine", Msg: fmt.Sprintf(
					"is %q, Quarto's R engine, and %q is a Jupyter notebook. Remove the key, or "+
						"write engine = %q to render it with Quarto", EngineKnitr, file, EngineQuarto)}
			}
			return EngineQuarto, nil
		default:
			return "", NotebookError{Framework: FrameworkNotebook, Field: "engine", Msg: fmt.Sprintf(
				"is %q, which is not an engine. The four are %q, %q, %q and %q",
				decl.Engine, EngineNBConvert, EngineQuarto, EngineKnitr, EngineJupyter)}
		}
	}

	if ext == ".qmd" {
		return EngineQuarto, nil
	}
	return EngineNBConvert, nil
}

// isRmd reports whether a declared file is an R Markdown document.
func isRmd(file string) bool {
	return strings.EqualFold(filepath.Ext(file), ".rmd")
}

// checkRmdDeclaration is the `[notebook]` table judged for an R Markdown
// document: the file by the rules a notebook's is held to, and the engine as
// the one an .Rmd has. rmarkdown::render knits it — so `knitr`, or nothing.
func checkRmdDeclaration(releaseDir string, decl notebookDeclaration) error {
	if decl.File != "" {
		if err := checkNotebookPath(decl.File); err != nil {
			return err
		}
		rel := filepath.ToSlash(filepath.Clean(decl.File))
		info, err := os.Stat(filepath.Join(releaseDir, filepath.FromSlash(rel)))
		if err != nil || info.IsDir() {
			return NotebookError{Framework: FrameworkRMarkdown, Field: "file", Msg: fmt.Sprintf(
				"names %q, which this release does not contain", rel)}
		}
	}
	switch decl.Engine {
	case "", EngineKnitr:
		return nil
	case EngineNBConvert, EngineQuarto, EngineJupyter:
		return NotebookError{Framework: FrameworkRMarkdown, Field: "engine", Msg: fmt.Sprintf(
			"is %q, and an .Rmd is an R Markdown document, rendered by rmarkdown with knitr. "+
				"Remove the key, or write engine = %q", decl.Engine, EngineKnitr)}
	}
	_, err := resolveEngine(decl, "")
	if ne, ok := err.(NotebookError); ok {
		ne.Framework = FrameworkRMarkdown
		return ne
	}
	return err
}

// unknownNotebookEngine is the refusal for a declared engine that is none of
// the four, judged without the file — for a check that has already found the
// file missing and must still name the engine (BB-065). Nil when the engine is
// absent or one of the four; whether it fits the file is resolveEngine's.
func unknownNotebookEngine(releaseDir string) *NotebookError {
	decl := readNotebookDeclaration(releaseDir)
	switch decl.Engine {
	case "", EngineNBConvert, EngineQuarto, EngineJupyter, EngineKnitr:
		return nil
	}
	_, err := resolveEngine(decl, "")
	ne, ok := err.(NotebookError)
	if !ok {
		return nil
	}
	return &ne
}

// checkNotebookPath refuses anything that is not a plain relative path inside
// the release — the same three refusals staticserve's checkRelative makes, in
// this package's error type so the message names `[notebook] file`.
func checkNotebookPath(value string) error {
	clean := filepath.ToSlash(filepath.Clean(value))
	switch {
	case filepath.IsAbs(value) || strings.HasPrefix(value, "/"):
		return NotebookError{Framework: FrameworkNotebook, Field: "file", Msg: fmt.Sprintf(
			"must be a path inside the repository, and %q is absolute", value)}
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return NotebookError{Framework: FrameworkNotebook, Field: "file", Msg: fmt.Sprintf(
			"must be a path inside the repository, and %q leaves it", value)}
	case clean == ".":
		return NotebookError{Framework: FrameworkNotebook, Field: "file", Msg: "must name something"}
	}
	return nil
}

// notebookCandidates lists the executable documents directly in the release
// root, sorted.
//
// Only the top level, for the reason staticserve's topLevelHTML is: a repo of
// analyses has a notebook in every directory and none of them is a claim about
// which one is the app. A repo like that declares one.
func notebookCandidates(releaseDir string) []string {
	entries, err := os.ReadDir(releaseDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || staticserve.Refused(e.Name()) || !hasNotebookExtension(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// marimoCandidates lists the marimo notebooks directly in the release root.
//
// **A marimo dependency is not the trigger, and that is the rule somebody will
// later "fix".** Unlike Streamlit, marimo is a library people import into
// ordinary programs, so `marimo` in requirements.txt says nothing about what
// this repository *is* — a Flask app that happens to depend on it has to stay a
// Flask app. The file is the declaration; the dependency is not.
func marimoCandidates(releaseDir string) []string {
	entries, err := os.ReadDir(releaseDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || staticserve.Refused(name) || !strings.HasSuffix(strings.ToLower(name), ".py") {
			continue
		}
		if isMarimoNotebook(releaseDir, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// isMarimoNotebook reports whether a Python file imports marimo and constructs
// a marimo.App.
//
// Scanned line-by-line rather than parsed, in the spirit of goPackageIsMain and
// the Python framework heuristics already here. Both halves are required: an
// import alone is any program that uses the library, and `App(` alone is any
// program with a class of that name.
func isMarimoNotebook(dir, name string) bool {
	f, err := openInRelease(dir, name)
	if err != nil {
		return false
	}
	defer f.Close()

	// A marimo notebook puts both near the top, and a bound keeps this off the
	// far end of a large data file that happens to end in .py.
	const maxLines = 200
	var alias string
	constructs := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 0; n < maxLines && scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if a, ok := marimoImportAlias(line); ok {
			alias = a
			continue
		}
		if alias != "" && strings.Contains(line, alias+".App(") {
			constructs = true
		}
	}
	return alias != "" && constructs
}

// marimoImportAlias recognises `import marimo` and `import marimo as mo`, and
// returns the name the module is bound to. `from marimo import ...` is
// deliberately not recognised: it does not bind the module, so there is nothing
// for `.App(` to hang off.
func marimoImportAlias(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "import marimo")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "marimo", true
	}
	if alias, ok := strings.CutPrefix(rest, "as "); ok {
		alias = strings.TrimSpace(alias)
		if alias != "" && !strings.ContainsAny(alias, " \t,.()") {
			return alias, true
		}
	}
	return "", false
}

func hasNotebookExtension(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, want := range notebookExtensions {
		if ext == want {
			return true
		}
	}
	return false
}

// notebookKinds is the phrase every refusal uses for the source extensions, so
// the list is written once.
func notebookKinds() string {
	quoted := make([]string, 0, len(notebookExtensions))
	for _, ext := range notebookExtensions {
		quoted = append(quoted, "a "+ext)
	}
	return strings.Join(quoted, " or ")
}

// describeDocs names the other half of an ambiguous release.
func describeDocs(decl notebookDeclaration, docs []string) string {
	if len(docs) > 0 {
		return "a notebook (" + strings.Join(docs, ", ") + ")"
	}
	if decl.Present {
		return "a [notebook] table in " + ManifestFile
	}
	return "a notebook"
}

// --- The render ---

// renderPackages are what the platform puts in a notebook's venv so that the
// most common first deploy does not fail on a dependency only the platform
// needs.
//
// Installed immediately after the SDK and **before** the app's own
// requirements.txt, so a repo pinning its own version replaces the platform's —
// a floor, not a ceiling, which is the rule the SDK install already follows.
//
// Stated consequence: the renderer is not in the app's manifest, so the
// dependency gate does not see it. Already true of the SDK, and the same trade.
var renderPackages = map[Framework][]string{
	// nbconvert converts, ipykernel is what `--execute` runs the cells in, and
	// pyyaml is Quarto's — its Jupyter engine's own `jupyter.py` imports it, and
	// without it a `.qmd` render dies on `No module named 'yaml'` from inside a
	// file the developer has never seen.
	//
	// **One list for both engines rather than one per engine**, and the reason
	// is the isolated runner. It rebuilds a notebook release's venv on a
	// repair, and it knows the framework but not the engine — that is a
	// property of the release's manifest, which the bootstrap response does not
	// carry. An engine-keyed list would therefore be a list the two build paths
	// could not both compute, and "the two paths produce the same tree" is
	// worth more here than three megabytes.
	FrameworkNotebook: {"nbconvert", "ipykernel", "pyyaml"},
	// marimo is the workload itself, not a renderer, and it is here for the
	// same reason: `marimo run` has to exist in the venv the launcher points at.
	FrameworkMarimo: {"marimo"},
}

// installRenderPackages puts the platform's own Python packages in the venv.
//
// Never fatal on its own terms — the failure is reported and the deploy carries
// on to the render, which fails with the interpreter's own words about what is
// missing. Two messages where one would do, but the second one is the useful
// one and suppressing the first would hide a network problem behind an
// ImportError.
func (b *Builder) installRenderPackages(deployID string, f Framework, releaseDir string,
	buildEnv map[string]string, lg buildLog) {
	pkgs := renderPackages[f]
	if len(pkgs) == 0 {
		return
	}
	lg.stepf("==> Installing %s...", strings.Join(pkgs, " and "))
	args := append([]string{"pip", "install"}, pkgs...)
	args = append(args, "--python", "venv/bin/python")
	if err := b.runCommand(deployID, releaseDir, "uv", args, buildEnv, lg.output); err != nil {
		lg.stepf("    Warning: installing %s failed. The app's own requirements.txt may still "+
			"provide %s.", strings.Join(pkgs, " and "), plural(len(pkgs), "it", "them"))
	}
}

// renderNotebook executes the notebook and writes the document the workload
// serves.
//
// This is the point of the whole framework and it is also the sharpest edge in
// it: **the render runs with the app's environment**, which is what lets a
// report read DATABASE_URL, and is therefore how a connection string can end up
// in a document an anonymous visitor may read. The build has held those
// variables since long before this existed — `runCommand` already hands
// repo-controlled code the app's decrypted environment — so nothing new is
// exposed here. What is new is that the *output* is published.
//
// The platform cannot know which cell output is a secret, and a switch that
// refused the environment would refuse the feature. So it is said instead: the
// names that were exported go in the build log, never the values, matching
// envdecl.go's discretion (names in the log, never a value).
func (b *Builder) renderNotebook(deployID string, plan *NotebookPlan, releaseDir string,
	envVars, buildEnv map[string]string, lg buildLog) error {
	renderDir := filepath.Join(releaseDir, RenderDir)
	if err := b.asBuild(releaseDir, func() error { return prepareRenderDir(renderDir) }); err != nil {
		return err
	}

	lg.stepf("==> Rendering %s with %s...", plan.File, plan.Engine)
	reportRenderEnv(envVars, lg)

	timeout := b.notebookRenderTimeout()
	name, args, extra, err := renderCommand(plan, releaseDir)
	if err != nil {
		return err
	}
	// The engine's own variables, over the app's. The app cannot set them
	// itself to something that would point the renderer somewhere else: what
	// the render must use is not the app's to choose.
	env := make(map[string]string, len(buildEnv)+len(extra))
	for k, v := range buildEnv {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	if err := b.runCommandFor(timeout, deployID, releaseDir, name, args, env, lg.output); err != nil {
		// The clock's remedy only when the clock is what stopped it: a
		// document that failed on its own error was told to ask for more
		// time (docs/bugs 2026-10-28 #4).
		if !timedOut(err) {
			return fmt.Errorf("rendering %s failed: %v", plan.File, err)
		}
		return fmt.Errorf("rendering %s failed: %v\n    If the notebook needs longer than %s, "+
			"an administrator can raise %s in Settings",
			plan.File, err, timeout, "notebook_render_timeout_seconds")
	}

	out := filepath.Join(renderDir, RenderEntry)
	if !fileExists(out) {
		return fmt.Errorf("rendering %s produced no %s. The engine exited successfully and "+
			"wrote nothing there, which is a bug in this platform rather than in the notebook",
			plan.File, filepath.Join(RenderDir, RenderEntry))
	}
	if err := b.asBuild(releaseDir, func() error {
		return os.WriteFile(filepath.Join(renderDir, renderMarker),
			[]byte("Written by Toft. This directory is the rendered notebook, not the app's source.\n"),
			0o600)
	}); err != nil {
		return fmt.Errorf("marking the render directory: %w", err)
	}
	lg.stepf("    Rendered to %s/%s", RenderDir, RenderEntry)
	return nil
}

// renderCommand is D4's table as an invocation, plus the environment the engine
// itself needs. Every argument here is fixed: the *inputs* to a render are the
// notebook, the release and the app's environment, and there is nowhere else
// for one to come from.
func renderCommand(plan *NotebookPlan, releaseDir string) (string, []string, map[string]string, error) {
	switch plan.Engine {
	case EngineNBConvert:
		// --to html embeds figures as data URIs, which is what makes the output
		// one self-contained file — and therefore what makes a document root of
		// exactly one document possible.
		//
		// --template lab because nbconvert's plain output is a notebook dump:
		// In[1]/Out[1] prompts, no contents, no styling. `lab` is shipped with
		// nbconvert, so this is the whole of the cheap improvement and it costs
		// no new dependency.
		return "venv/bin/python", []string{
			"-m", "nbconvert", "--to", "html", "--template", "lab", "--execute",
			"--output", RenderEntry, "--output-dir", RenderDir, plan.File,
		}, nil, nil

	case EngineQuarto:
		bin, err := QuartoBin()
		if err != nil {
			return "", nil, nil, err
		}
		// **QUARTO_PYTHON is not optional and its absence is silent.** Quarto
		// runs a document's `{python}` cells through whatever `python3` it finds
		// on PATH, which on this host is the *platform's* interpreter — so a
		// report importing pandas would render against the platform's packages,
		// or fail on an import the app declared and installed. Naming the
		// release's own interpreter is what makes a Quarto app's
		// requirements.txt mean the same thing an nbconvert app's does.
		//
		// Absolute, because the engine's working directory is the release but
		// its subprocesses' are not guaranteed to be.
		return bin, []string{
			"render", plan.File, "--to", "html",
			"--output", RenderEntry, "--output-dir", RenderDir,
		}, map[string]string{
			"QUARTO_PYTHON": filepath.Join(releaseDir, "venv", "bin", "python"),
		}, nil
	}
	return "", nil, nil, NotebookError{Framework: FrameworkNotebook, Field: "engine",
		Msg: fmt.Sprintf("is %q, which is not an engine", plan.Engine)}
}

// QuartoBin is the Quarto this install has, or the refusal that names what an
// operator would have to set.
//
// **Refused, never substituted.** An author who declared `engine = "quarto"`,
// or committed a `.qmd`, asked for a different document — a contents sidebar,
// code folding, callouts, cross-references, figure captions — and silently
// rendering it with nbconvert would deploy something else under the same name.
func QuartoBin() (string, error) {
	path, _ := toolchain.Quarto()
	if path == "" {
		return "", NotebookError{Framework: FrameworkNotebook, Msg: fmt.Sprintf(
			"this document needs Quarto, and this install has none. Quarto is not bundled — "+
				"it is 892 MB of toolchain that would otherwise be packed into every release "+
				"— so an operator installs it on the machine that builds and names it in %s, "+
				"or runs the Toft image tag that carries it. A .ipynb with no [notebook] "+
				"engine declared renders without it",
			toolchain.QuartoBinEnv)}
	}
	return path, nil
}

// prepareRenderDir clears the platform's own render directory, or refuses a
// directory of that name the author committed.
//
// The distinction is the marker file, and it has to exist because both
// directions matter. A committed `toft-render/` must fail the deploy by
// name — overwriting a directory somebody put in their repository is worse than
// refusing it. A *repair* — a shared-mode release rebuilt in place because its
// venv no longer resolves — re-runs this same build against a tree that already
// holds last time's render, and refusing that would make every notebook app
// unrepairable.
func prepareRenderDir(renderDir string) error {
	info, err := os.Stat(renderDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(renderDir, 0o755)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", RenderDir, err)
	}
	if !info.IsDir() || !fileExists(filepath.Join(renderDir, renderMarker)) {
		return NotebookError{Framework: FrameworkNotebook, Msg: fmt.Sprintf(
			"this release contains %q, which is the name the platform writes the rendered "+
				"document to. Rename it — a notebook app serves only what the render produces, "+
				"so nothing under that name would have been published anyway",
			RenderDir)}
	}
	if err := os.RemoveAll(renderDir); err != nil {
		return fmt.Errorf("clearing the previous render: %w", err)
	}
	return os.MkdirAll(renderDir, 0o755)
}

// reportRenderEnv says which of the app's variables the render can read.
//
// Names only, sorted, never values. The list is the honest statement of the
// risk: whatever the notebook prints from these is in a document the app then
// publishes, and on a public app that means the internet.
func reportRenderEnv(envVars map[string]string, lg buildLog) {
	lg.step("    The notebook runs with this app's environment, and whatever it prints is")
	lg.step("    published in the rendered document. Nothing here is redacted.")
	if len(envVars) == 0 {
		lg.step("    This app has no environment variables set.")
		return
	}
	names := make([]string, 0, len(envVars))
	for k := range envVars {
		names = append(names, k)
	}
	sort.Strings(names)
	lg.stepf("    Exported: %s", strings.Join(names, ", "))
}

// notebookRenderTimeout is the install's figure, and the default when it cannot
// be read.
//
// A settings read that fails must not fail a deploy: the figure is a bound on
// something that is about to happen anyway, and the compiled-in default is the
// same ten minutes every other build command already gets.
func (b *Builder) notebookRenderTimeout() time.Duration {
	if b.db != nil {
		if s, err := b.db.GetPlatformSettings(); err == nil {
			return s.NotebookRenderTimeout()
		}
	}
	return commandTimeout
}

// renderIfNotebook is the deploy's render step, and a no-op for every other
// framework.
//
// Called from `execute` rather than from installDependencies, because that
// function's other caller is Repair — see the note at the call site.
func (b *Builder) renderIfNotebook(deployID string, result DetectionResult, releaseDir string,
	envVars, buildEnv map[string]string, lg buildLog) error {
	if result.Framework != FrameworkNotebook {
		return nil
	}
	plan, err := ResolveNotebook(releaseDir)
	if err != nil {
		return err
	}
	if plan == nil || plan.Framework != FrameworkNotebook {
		// Detection said notebook and resolution no longer agrees, which can
		// only mean the tree changed underneath the build. Refused rather than
		// skipped: a file server pointed at a directory nothing wrote is a 404
		// nobody can explain.
		return fmt.Errorf("this release was detected as a notebook and no longer resolves to " +
			"one. Redeploy the app")
	}
	return b.renderNotebook(deployID, plan, releaseDir, envVars, buildEnv, lg)
}

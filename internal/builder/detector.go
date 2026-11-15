package builder

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"toft/internal/staticserve"
)

type Framework string

const (
	FrameworkNextJS      Framework = "nextjs"
	FrameworkNode        Framework = "node"
	FrameworkGo          Framework = "go"
	FrameworkFlask       Framework = "flask"
	FrameworkFastAPI     Framework = "fastapi"
	FrameworkStreamlit   Framework = "streamlit"
	FrameworkPython      Framework = "python"
	FrameworkNotebook    Framework = "notebook"
	FrameworkMarimo      Framework = "marimo"
	FrameworkShiny       Framework = "shiny"
	FrameworkPlumber     Framework = "plumber"
	FrameworkRMarkdown   Framework = "rmarkdown"
	FrameworkQuarto      Framework = "quarto"
	FrameworkStatic      Framework = "static"
	FrameworkUnsupported Framework = "unsupported"
)

type DetectionResult struct {
	Framework  Framework `json:"framework"`
	HasBuild   bool      `json:"has_build"`
	Entrypoint string    `json:"entrypoint,omitempty"`
	// StartCmd carries the framework-specific start hint: the npm script name
	// for Node apps, the ASGI attribute name for FastAPI.
	StartCmd string `json:"start_cmd,omitempty"`
}

func DetectFramework(srcDir string) DetectionResult {
	// The repo's own `[static]` declaration, read first and answered last.
	//
	// Read *here*, in detection, rather than by the builder — which is the one
	// line of this file the static design depends on. `Builder.Start` re-runs
	// detection against the release directory at every wake from idle sleep,
	// every restore after a crash and every platform boot, deliberately, so
	// that a restore stays honest about a release built before the columns
	// existed. A declared entry that only the *builder* read would be
	// overwritten by that re-detection, and the app's front page would quietly
	// revert overnight. The manifest is a file in the release directory, so
	// detection run against that directory returns the same answer every time,
	// in both execution modes, with no precedence rule anywhere.
	//
	// A malformed manifest is not a detection failure: the deploy has its own
	// place to refuse one, with the field named, and detection must not turn a
	// typo into "Unsupported framework".
	decl, _ := staticserve.ReadDeclaration(srcDir)

	// A `[batch]` declaration first (ADR §107): a release that serves nothing
	// must never fall through to a service detector, which would read its
	// main.py or its Go main as a web app and crash-loop it. Before `[static]`
	// too; a release declaring both is refused by the manifest with both named.
	if b := readBatchDeclaration(srcDir); b.Present {
		return detectBatch(srcDir, b)
	}

	// A declaration overrides every disqualifier below. A repo that commits its
	// build output and says `root = "dist"` knows something the detector cannot,
	// which is exactly what the manifest is for.
	if decl.Present {
		return staticResult(srcDir)
	}

	// Check for Next.js
	for _, name := range []string{"next.config.js", "next.config.ts", "next.config.mjs"} {
		if _, err := os.Stat(filepath.Join(srcDir, name)); err == nil {
			return DetectionResult{Framework: FrameworkNextJS, HasBuild: hasBuildScript(srcDir)}
		}
	}

	// Check package.json for next dependency, then fall back to generic Node
	data, err := readInRelease(srcDir, "package.json")
	if err == nil {
		var pkg map[string]interface{}
		if json.Unmarshal(data, &pkg) == nil {
			for _, depKey := range []string{"dependencies", "devDependencies"} {
				if deps, ok := pkg[depKey].(map[string]interface{}); ok {
					if _, hasNext := deps["next"]; hasNext {
						return DetectionResult{Framework: FrameworkNextJS, HasBuild: hasBuildScript(srcDir)}
					}
				}
			}

			// Any Node project with a start script can be hosted: it is run
			// with `npm run start` and told its mount point via BASE_PATH.
			if hasScript(pkg, "start") {
				return DetectionResult{
					Framework: FrameworkNode,
					HasBuild:  hasScript(pkg, "build"),
					StartCmd:  "start",
				}
			}
		}
	}

	// Check for Go
	if result := detectGo(srcDir); result.Framework != FrameworkUnsupported {
		return result
	}

	// Notebooks, before Python and independently of requirements.txt.
	//
	// Before, because a notebook repository has a requirements.txt like any
	// other Python project and would otherwise detect as generic Python — which
	// is exactly the false success this pair of frameworks exists to replace.
	// Independently of it, because a notebook needs no requirements.txt at all:
	// the platform installs the renderer itself.
	//
	// After Node and Go, which is not arbitrary either. A repository whose
	// package.json has a `start` script is a Node service that happens to have
	// committed an analysis beside it, and the same is true of a Go module.
	// Detection is keyed off the *source*, never the rendered output — forced,
	// because it runs before the render exists and again at every start after
	// it does, and a rule reading the output would answer `notebook` on a
	// restart and something else on the build that produced it.
	// R's documents, before the notebooks — because a .qmd is claimed by that
	// detector and only its *engine* decides which family it belongs to. An
	// .Rmd has no other family to belong to; a .qmd with no engine declaration
	// and no R chunks falls through to `notebook`, which is what a .qmd on this
	// platform means today and is the rule that keeps apps deployed before R
	// from changing framework after it.
	//
	// After detectShiny, which runs inside detectR below? No — detectShiny is
	// reached later, so the one document that is really an application is
	// claimed here first: an .Rmd declaring `runtime: shiny`. That check is in
	// this branch rather than in detectShiny for exactly that ordering reason.
	// So is an app a Connect bundle declares as Shiny, unless the author's own
	// `[notebook] file` names a document: the manifest outranks the guess that
	// a lone .Rmd beside app.R is the content, never the author (ADR §109).
	if isShinyRmd(srcDir) || (connectDeclaresShiny(srcDir) && readNotebookDeclaration(srcDir).File == "") {
		if r := detectShiny(srcDir); r.Framework != FrameworkUnsupported {
			return r
		}
	}
	if result := detectRDocument(srcDir); result.Framework != FrameworkUnsupported {
		return result
	}

	if result := detectNotebook(srcDir); result.Framework != FrameworkUnsupported {
		return result
	}

	// R, before Python and after the notebooks, and each of those has its own
	// reason.
	//
	// Before Python, because an R repository frequently carries a
	// requirements.txt — a Shiny app calling reticulate, an analysis whose data
	// was fetched by a Python script — and detectPython keys off exactly those
	// files. Detected as generic Python it would deploy, find no app to start,
	// and crash-loop.
	//
	// After the notebooks, because that is where a .qmd is answered and the
	// engine decides which family it belongs to. What is left for R here are
	// the two shapes that are *processes* rather than documents.
	if result := detectR(srcDir); result.Framework != FrameworkUnsupported {
		return result
	}

	// Check for Python
	if result := detectPython(srcDir); result.Framework != FrameworkUnsupported {
		return result
	}

	// Static comes last, and only for a repo that is *nothing else*.
	//
	// Adding index.html to the list above would be a one-line change and a
	// trap: a Vite or CRA repo that fails to deploy today — package.json
	// present, no `start` script — has an index.html at its root holding
	// `<div id="root">` and a script tag pointing at TypeScript source. Detected
	// as static it would deploy *successfully* and serve a blank page forever,
	// with a build log full of green ticks. Those repos keep failing with the
	// message they get today, which now says static hosting exists and what
	// disqualified them from it.
	if disqualifier(srcDir) != "" {
		return DetectionResult{Framework: FrameworkUnsupported, HasBuild: false}
	}
	// Resolution rather than a file-name check, so "detected as static" and
	// "servable as static" are the same question asked once. A tree with no
	// HTML in it is not a static site; it is an unsupported repository, and the
	// message it already gets is the better one.
	cfg, err := staticserve.Resolve(srcDir)
	if err != nil {
		// A Python entry file and nothing else — no manifest, no HTML — is a
		// plain Python app with no dependencies, the "anything else" row of the
		// Python guide. Here rather than in detectPython, so a folder of HTML
		// that ships a preview server.py stays the static site it is today.
		if entry := detectPythonEntrypoint(srcDir); entry != "" {
			return DetectionResult{Framework: FrameworkPython, Entrypoint: entry}
		}
		return DetectionResult{Framework: FrameworkUnsupported, HasBuild: false}
	}
	return DetectionResult{Framework: FrameworkStatic, Entrypoint: cfg.Entry}
}

// staticDisqualifiers are the files whose presence means the repo is a program
// rather than a folder of files. The list *is* the specification of what static
// detection will not swallow.
var staticDisqualifiers = []string{
	"package.json", "requirements.txt", "pyproject.toml", "Pipfile", "go.mod",
}

// disqualifier names the first one this release carries, or "".
func disqualifier(srcDir string) string {
	for _, name := range staticDisqualifiers {
		if fileExists(filepath.Join(srcDir, name)) {
			return name
		}
	}
	return ""
}

// staticResult is what detection returns for a *declared* static app. The
// chosen entry document goes in Entrypoint — the column Python and Go already
// use for the same purpose, so no schema change — and is empty when the release
// cannot be resolved, which the deploy refuses by name a few steps later rather
// than here, where the only vocabulary is "unsupported".
func staticResult(srcDir string) DetectionResult {
	result := DetectionResult{Framework: FrameworkStatic}
	if cfg, err := staticserve.Resolve(srcDir); err == nil {
		result.Entrypoint = cfg.Entry
	}
	return result
}

// UnsupportedMessage is the refusal a repo gets when nothing recognised it. It
// names static hosting, because a folder of HTML is the shape this platform is
// most often asked for — and, when the repo *is* a program, names the file that
// ruled static out, so the answer is not "add an index.html" to somebody whose
// index.html is already there.
func UnsupportedMessage(srcDir string) string {
	// A declared batch release that could not be resolved gets its own reason:
	// its author said what it is, and the list of web frameworks is not the
	// answer to a question they did not ask.
	if why := BatchRefusal(srcDir); why != "" {
		return why
	}
	msg := "Unsupported framework. Supported: Next.js, Node, Go, Flask, FastAPI, " +
		"Streamlit, Python, notebooks (.ipynb and .qmd), R Markdown (.Rmd), marimo, Shiny, plumber, " +
		"and static sites (a folder of HTML with no server in it)."

	// A release that carries a notebook and was not detected as one is a repo
	// whose author is looking at a file the platform recognises. It gets the
	// reason — several candidates, a declared file that is missing, an engine
	// that is not a name — rather than the generic sentence.
	if _, err := ResolveNotebook(srcDir); err != nil {
		return msg + "\n    " + err.Error() + "\n"
	}

	// An R repository that matched none of the named shapes, which is a refusal
	// this platform owes an explanation for in a way the others do not: there is
	// no generic R framework to fall back to, deliberately. `Rscript app.R`
	// exits, and a workload that exits is a crash loop — so the four shapes are
	// the whole of what R can be here, and the author is told which four rather
	// than being told R is unsupported, which is not true.
	if why := severalRmdMessage(srcDir); why != "" {
		return msg + "\n    " + why
	}
	if repositoryLooksLikeR(srcDir) {
		return msg + "\n    This repository is R, and R apps are hosted in the four shapes R " +
			"comes in: a Shiny app (app.R, ui.R and server.R together, or server.R with a " +
			"www/index.html), a plumber API (plumber.R, or an .R file with #* @get " +
			"annotations), an R Markdown document (.Rmd), or a Quarto document (.qmd). " +
			"Any of those names may be spelled .r. There is no generic R app, because an R " +
			"script runs to the end and exits, and a workload that exits is a crash loop.\n"
	}

	// Before the disqualifier branch, because a Python project *is* disqualified
	// from static hosting and the generic sentence would send its author looking
	// for an index.html they were never missing. The specific answer — "this is
	// a Python app and there is no file to start" — is the one they can act on.
	if decl := pythonProjectFile(srcDir); decl != "" && detectPythonEntrypoint(srcDir) == "" {
		return msg + fmt.Sprintf("\n    This repository has a %s, so it is a Python app, but it "+
			"contains none of the files a Python app is started from: %s. Add one, or, if the "+
			"code that should run lives elsewhere, rename or add a small %s that imports it.\n",
			decl, strings.Join(pythonEntrypoints, ", "), pythonEntrypoints[0])
	}

	if fileExists(filepath.Join(srcDir, "go.mod")) && goMainPackage(srcDir) == "" {
		return msg + "\n    This repository has a go.mod, so it is a Go module, but it has no " +
			"main package to build — package main with a func main, at the module root or " +
			"under cmd/<name>. A module of library packages alone is not an app.\n"
	}
	if name := disqualifier(srcDir); name != "" {
		return msg + fmt.Sprintf("\n    This repository has a %s, so it is a program rather than a "+
			"folder of files, and static hosting was not considered for it. If it builds to a "+
			"directory this repo commits, declare that directory in %s:\n"+
			"\n        [static]\n        root = \"dist\"\n",
			name, staticserve.ManifestFile)
	}
	// A server file or a main package with no manifest: the program is there,
	// but Node starts from package.json's start script and Go builds from a
	// module, so the sentence names the manifest rather than "no HTML document
	// to serve", which does not fit an API (BB-19).
	if entry := nodeEntryWithoutManifest(srcDir); entry != "" {
		return msg + fmt.Sprintf("\n    This repository has %s, so it looks like a Node app, but it has no "+
			"package.json. A Node app is started by the \"start\" script in its package.json; "+
			"add one, like {\"scripts\": {\"start\": \"node %s\"}}.\n", entry, entry)
	}
	if goMainWithoutModule(srcDir) {
		return msg + "\n    This repository has a Go main package, so it looks like a Go app, but it " +
			"has no go.mod, and a Go app is built as a module. Add one with go mod init <name>.\n"
	}
	if _, err := staticserve.Resolve(srcDir); err != nil {
		return msg + "\n    " + err.Error()
	}
	return msg
}

// nodeEntries are the files a Node server is conventionally started from.
// None of them starts an app on its own — Node starts from package.json — so
// they only name what a tree with no package.json is missing.
var nodeEntries = []string{
	"server.js", "index.js", "app.js", "main.js",
	"server.mjs", "index.mjs", "app.mjs", "main.mjs",
	"server.ts", "index.ts", "app.ts", "main.ts",
}

// nodeEntryWithoutManifest names the Node entry file a tree holds when it has
// no package.json, or "".
func nodeEntryWithoutManifest(srcDir string) string {
	if fileExists(filepath.Join(srcDir, "package.json")) {
		return ""
	}
	for _, name := range nodeEntries {
		if fileExists(filepath.Join(srcDir, name)) {
			return name
		}
	}
	return ""
}

// goMainWithoutModule reports a `package main` at the root with no go.mod.
func goMainWithoutModule(srcDir string) bool {
	return !fileExists(filepath.Join(srcDir, "go.mod")) && dirHasMainPackage(srcDir)
}

// pythonProjectFile names the declaration that makes this release a Python
// project, or "". The order matches detectPython's.
func pythonProjectFile(srcDir string) string {
	for _, name := range []string{"requirements.txt", "pyproject.toml", "Pipfile"} {
		if fileExists(filepath.Join(srcDir, name)) {
			return name
		}
	}
	return ""
}

// detectNotebook is the two notebook frameworks, resolved from the release.
//
// A release that claims one and cannot be served as one keeps the framework and
// loses the entrypoint: the deploy refuses a few steps later, by name and with
// the field, which is the same arrangement `[static]` has and for the same
// reason — detection's only vocabulary is "unsupported", and that is the wrong
// word for a notebook with two candidates.
func detectNotebook(srcDir string) DetectionResult {
	plan, err := ResolveNotebook(srcDir)
	if err != nil {
		var ne NotebookError
		if errors.As(err, &ne) {
			return DetectionResult{Framework: ne.Framework}
		}
		return DetectionResult{Framework: FrameworkUnsupported}
	}
	if plan == nil {
		return DetectionResult{Framework: FrameworkUnsupported}
	}
	return DetectionResult{Framework: plan.Framework, Entrypoint: plan.File}
}

// detectGo recognizes a Go module containing a buildable main package. The
// package path ("." or "./cmd/<name>") is carried in Entrypoint for `go build`.
// A module with no main package is a library, not a hostable app.
func detectGo(srcDir string) DetectionResult {
	if !fileExists(filepath.Join(srcDir, "go.mod")) {
		return DetectionResult{Framework: FrameworkUnsupported}
	}
	pkg := goMainPackage(srcDir)
	if pkg == "" {
		return DetectionResult{Framework: FrameworkUnsupported}
	}
	return DetectionResult{Framework: FrameworkGo, HasBuild: true, Entrypoint: pkg}
}

// goMainPackage finds the package to build: the module root if it holds a
// `package main` file, otherwise the first cmd/* directory (sorted, so the
// choice is deterministic when a repo ships several commands) that does.
func goMainPackage(srcDir string) string {
	if dirHasMainPackage(srcDir) {
		return "."
	}
	entries, err := os.ReadDir(filepath.Join(srcDir, "cmd"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && dirHasMainPackage(filepath.Join(srcDir, "cmd", e.Name())) {
			return "./cmd/" + e.Name()
		}
	}
	return ""
}

func dirHasMainPackage(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if goPackageIsMain(dir, name) {
			return true
		}
	}
	return false
}

// goPackageIsMain reports whether a Go file's package clause is `main`,
// scanning line-by-line rather than parsing (same spirit as the Python
// framework detection heuristics).
func goPackageIsMain(dir, name string) bool {
	f, err := openInRelease(dir, name)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "package ") {
			rest := strings.TrimPrefix(line, "package ")
			if i := strings.IndexAny(rest, " \t/"); i >= 0 {
				rest = rest[:i]
			}
			return rest == "main"
		}
	}
	return false
}

func detectPython(srcDir string) DetectionResult {
	// Check for Python project indicators
	hasRequirements := fileExists(filepath.Join(srcDir, "requirements.txt"))
	hasPipfile := fileExists(filepath.Join(srcDir, "Pipfile"))
	hasPyproject := fileExists(filepath.Join(srcDir, "pyproject.toml"))

	if !hasRequirements && !hasPipfile && !hasPyproject {
		return DetectionResult{Framework: FrameworkUnsupported}
	}

	// Identify specific framework from dependencies
	framework := FrameworkPython
	if hasRequirements {
		framework = detectPythonFrameworkFromRequirements(srcDir, "requirements.txt")
	} else if hasPyproject {
		framework = detectPythonFrameworkFromFile(srcDir, "pyproject.toml")
	} else if hasPipfile {
		framework = detectPythonFrameworkFromFile(srcDir, "Pipfile")
	}

	// Find entrypoint. A Python repository with none is refused here rather
	// than deployed and left to crash-loop: there is no file to run, and every
	// step between here and the first restart would otherwise report success.
	entrypoint := detectPythonEntrypoint(srcDir)
	if entrypoint == "" {
		return DetectionResult{Framework: FrameworkUnsupported}
	}

	result := DetectionResult{
		Framework:  framework,
		HasBuild:   false, // Python apps don't have a separate build step
		Entrypoint: entrypoint,
	}
	switch framework {
	case FrameworkFastAPI:
		result.StartCmd = detectASGIAttr(srcDir, entrypoint)
	case FrameworkFlask:
		result.StartCmd = detectWSGITarget(srcDir, entrypoint)
	}
	return result
}

// detectWSGITarget finds what gunicorn should be pointed at in a Flask
// entrypoint, in the order Flask's own CLI searches: a module-level name bound
// to Flask(...), then a module-level `app` or `application`, then a
// `create_app`/`make_app` factory callable with no arguments, written
// "create_app()". "" when none is there — unlike detectASGIAttr there is no
// default, because a wrong guess costs a crash loop and the Flask CLI, which
// searches for itself, is the fallback (internal/runner/flasklaunch.go).
//
// Module level means unindented: an `app = Flask(__name__)` inside a factory
// is not an attribute of the module.
func detectWSGITarget(dir, name string) string {
	data, err := readInRelease(dir, name)
	if err != nil {
		return ""
	}
	var named, conventional, factory string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		line = strings.TrimRight(line, " \t\r")
		if rest, ok := strings.CutPrefix(line, "def "); ok {
			open := strings.Index(rest, "(")
			close := strings.LastIndex(rest, ")")
			if open < 0 || close < open {
				continue
			}
			name := strings.TrimSpace(rest[:open])
			if (name == "create_app" || name == "make_app") && factory == "" &&
				callableWithoutArgs(rest[open+1:close]) {
				factory = name + "()"
			}
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 || strings.HasPrefix(line[eq:], "==") {
			continue
		}
		name := line[:eq]
		if colon := strings.Index(name, ":"); colon >= 0 { // app: Flask = Flask(...)
			name = name[:colon]
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t,.[]()+-*/%&|^<>!") {
			continue
		}
		if named == "" && strings.Contains(line[eq:], "Flask(") {
			named = name
		}
		if conventional == "" && (name == "app" || name == "application") {
			conventional = name
		}
	}
	for _, t := range []string{named, conventional, factory} {
		if t != "" {
			return t
		}
	}
	return ""
}

// callableWithoutArgs reports whether a def's parameter list can be called
// with nothing: empty, or every parameter defaulted or variadic.
func callableWithoutArgs(params string) bool {
	for _, p := range strings.Split(params, ",") {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "*") || strings.Contains(p, "=") {
			continue
		}
		return false
	}
	return true
}

// detectASGIAttr finds the name bound to FastAPI(...) so uvicorn can be pointed
// at "module:attr". Defaults to "app", the overwhelmingly common choice.
func detectASGIAttr(dir, name string) string {
	data, err := readInRelease(dir, name)
	if err != nil {
		return "app"
	}

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		eq := strings.Index(trimmed, "=")
		if eq < 0 || !strings.Contains(trimmed[eq:], "FastAPI(") {
			continue
		}
		name := strings.TrimSpace(trimmed[:eq])
		if name != "" && !strings.ContainsAny(name, " \t,.[]()") {
			return name
		}
	}
	return "app"
}

func detectPythonFrameworkFromRequirements(dir, name string) Framework {
	f, err := openInRelease(dir, name)
	if err != nil {
		return FrameworkPython
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip version specifiers: "flask>=2.0" -> "flask"
		// A line made only of specifier characters ("==", "[") splits to
		// nothing: it names no package, and indexing it panicked the worker.
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == '=' || r == '>' || r == '<' || r == '!' || r == '~' || r == '[' || r == ';'
		})
		if len(fields) == 0 {
			continue
		}
		pkg := strings.TrimSpace(fields[0])
		switch pkg {
		case "streamlit":
			return FrameworkStreamlit
		case "fastapi":
			return FrameworkFastAPI
		case "flask":
			return FrameworkFlask
		}
	}
	return FrameworkPython
}

func detectPythonFrameworkFromFile(dir, name string) Framework {
	data, err := readInRelease(dir, name)
	if err != nil {
		return FrameworkPython
	}
	content := strings.ToLower(string(data))
	if strings.Contains(content, "streamlit") {
		return FrameworkStreamlit
	}
	if strings.Contains(content, "fastapi") {
		return FrameworkFastAPI
	}
	if strings.Contains(content, "flask") {
		return FrameworkFlask
	}
	return FrameworkPython
}

// pythonEntrypoints are the file names a Python app may be started from, in
// preference order. The list *is* the specification: every Python launcher —
// both of them, in both execution modes — runs `venv/bin/python <entrypoint>`
// or a module derived from it, so a release holding none of these has nothing
// to start.
var pythonEntrypoints = []string{"app.py", "main.py", "streamlit_app.py", "server.py", "wsgi.py"}

// detectPythonEntrypoint returns the first candidate this release carries, or
// "" when it carries none.
//
// It used to return "app.py" whether or not the file existed, and that fallback
// was a lie with a long fuse: a repository of notebooks and a requirements.txt
// detected as Python, installed its dependencies successfully, published a
// release, reported a green deploy — and then crash-looped forever on
// `venv/bin/python app.py` against a file that was never there. The owner got a
// tick and a dead app, which is worse than a refusal in every way.
func detectPythonEntrypoint(srcDir string) string {
	for _, name := range pythonEntrypoints {
		if fileExists(filepath.Join(srcDir, name)) {
			return name
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasBuildScript(srcDir string) bool {
	data, err := readInRelease(srcDir, "package.json")
	if err != nil {
		return false
	}

	var pkg map[string]interface{}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}

	return hasScript(pkg, "build")
}

func hasScript(pkg map[string]interface{}, name string) bool {
	scripts, ok := pkg["scripts"].(map[string]interface{})
	if !ok {
		return false
	}
	_, found := scripts[name]
	return found
}

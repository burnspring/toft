package builder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// R's document half: R Markdown, and Quarto under the knitr engine.
//
// **`quarto-jupyter` does not exist.** The concept behind this work proposed two
// Quarto framework identifiers, one per family. The second is not a new
// framework: a `.qmd` declaring the Jupyter engine is exactly the app the
// notebooks work already ships — `notebook`, Python family, a venv, a Quarto
// render at build and staticserve over the output — and giving it a second
// identifier would mean two framework values, two badges, two landing rows and
// two names for one code path, plus a rename of `apps.framework` values written
// before R landed.
//
// So the split is one identifier wide:
//
//	.qmd, knitr engine    quarto      r        quarto render, with R
//	.qmd, Jupyter engine  notebook    python   the notebooks work, unchanged
//	.ipynb                notebook    python   the notebooks work, unchanged
//	.Rmd                  rmarkdown   r        rmarkdown::render
//
// **The engine decides the family, so the engine is read in detection** rather
// than at build time. That is what keeps `languageFamily` a pure function of
// the framework string: it is called from podSpec, which holds a WorkloadSpec
// and no release directory, for both the service pod and the job pod. Making
// the family a per-app question would mean threading a release into pod
// construction for one file format.
//
// It is safe for the reason the `[static]` declaration is safe: the manifest and
// the document are files *in the release directory*, so detection run against
// that directory returns the same answer every time, in both execution modes,
// with no precedence rule anywhere.

// The two Quarto computation engines.
//
// These are values of the same `[notebook] engine` key that already carries
// EngineNBConvert and EngineQuarto, and the key is coherent across all four:
// it answers "how is this document turned into a page". `nbconvert` and
// `quarto` name the *renderer*; `knitr` and `jupyter` name Quarto's own
// computation engine and therefore imply the Quarto renderer as well. What the
// two new values add is the one thing the renderer alone cannot say — which
// language family executes the chunks.
const (
	// EngineKnitr is Quarto's R engine. A .qmd under it is an R app.
	EngineKnitr = "knitr"
	// EngineJupyter is Quarto's Python engine, which is the notebooks work's
	// existing behaviour under an explicit name.
	EngineJupyter = "jupyter"
)

// detectRDocument answers for the two R shapes that are *documents*: rendered
// at build time and served as files.
//
// Runs before detectNotebook, because a `.qmd` is claimed by that detector and
// only the engine decides which family it belongs to.
func detectRDocument(srcDir string) DetectionResult {
	decl := readNotebookDeclaration(srcDir)
	// Below `[notebook] file`, which is the author speaking to this platform
	// directly: a Connect bundle's own record of which document it publishes.
	// rsconnect writes it for every R Markdown and Quarto bundle, and without it
	// a report beside the email template it renders is two candidates and a
	// refusal, for an author who did say which one they meant (ADR §109).
	if decl.File == "" {
		decl.File = connectPrimaryDocument(srcDir)
	}

	// A declared file settles which document is the app, and its extension
	// settles the rest. Read first, because it is the author saying so.
	if decl.File != "" {
		switch strings.ToLower(filepath.Ext(decl.File)) {
		case ".rmd":
			return DetectionResult{Framework: FrameworkRMarkdown, Entrypoint: decl.File}
		case ".qmd":
			if qmdIsR(srcDir, decl.File, decl.Engine) {
				return DetectionResult{Framework: FrameworkQuarto, Entrypoint: decl.File}
			}
		}
		return DetectionResult{Framework: FrameworkUnsupported}
	}

	// .Rmd, which has no other family to belong to. A document declaring
	// `runtime: shiny` was already claimed as an application by detectShiny,
	// which runs first and is where that rule is stated.
	if rmds := rSourceFiles(srcDir, ".Rmd"); len(rmds) == 1 {
		return DetectionResult{Framework: FrameworkRMarkdown, Entrypoint: rmds[0]}
	} else if len(rmds) > 1 {
		// Several candidates and no declaration. Unsupported here, and
		// UnsupportedMessage names the candidates and the declaration that
		// settles it, so the author is not sent looking for an index.html.
		return DetectionResult{Framework: FrameworkUnsupported}
	}

	// .qmd, where the engine decides the family.
	if qmds := rSourceFiles(srcDir, ".qmd"); len(qmds) == 1 {
		if qmdIsR(srcDir, qmds[0], decl.Engine) {
			return DetectionResult{Framework: FrameworkQuarto, Entrypoint: qmds[0]}
		}
	}
	return DetectionResult{Framework: FrameworkUnsupported}
}

// qmdIsR is D11's order of reading, and every step is a positive signal.
//
//  1. `[notebook] engine = "knitr"` — the author said so.
//  2. `engine: knitr` or a `knitr:` key in the document's YAML header.
//  3. `engine: jupyter`, or a `jupyter:` key — not R.
//  4. An R chunk header, found by the same line scan plumber's annotations use.
//  5. Otherwise not R, which is what a `.qmd` on this platform means today.
//
// **Step 5 is the compatibility rule and it is deliberate: no app that deployed
// before R landed changes framework after it.** Requiring a declaration was the
// alternative; it refuses a document that renders fine on the author's laptop,
// and it would move `.qmd` apps that are already running.
func qmdIsR(srcDir, file, declaredEngine string) bool {
	switch declaredEngine {
	case EngineKnitr:
		return true
	case EngineJupyter, EngineNBConvert:
		return false
	}

	switch qmdHeaderEngine(srcDir, file) {
	case EngineKnitr:
		return true
	case EngineJupyter:
		return false
	}

	// Quarto's own default is knitr when the document has R chunks and Jupyter
	// when it does not, so the accurate answer requires looking at the chunks.
	return fileHasRChunk(srcDir, file)
}

// qmdHeaderEngine reads the engine out of a Quarto document's YAML header, or
// "" when it says nothing.
//
// Only the header — the block between the first `---` and the next one. An
// `engine:` further down is inside a chunk or a sentence about engines.
func qmdHeaderEngine(dir, name string) string {
	f, err := openInRelease(dir, name)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRScanBytes)

	inHeader := false
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "---" {
			if !inHeader {
				inHeader = true
				continue
			}
			return ""
		}
		if !inHeader {
			if line == "" {
				continue
			}
			return ""
		}
		if v, ok := strings.CutPrefix(line, "engine:"); ok {
			switch strings.TrimSpace(v) {
			case EngineKnitr:
				return EngineKnitr
			case EngineJupyter:
				return EngineJupyter
			}
			return ""
		}
		// A top-level `knitr:` or `jupyter:` key is Quarto's other way of
		// naming the engine — it carries that engine's options, and a document
		// carrying options for one engine is a document using it. Top-level
		// only: the same keys appear indented under `execute:` and elsewhere.
		if raw == strings.TrimLeft(raw, " \t") {
			switch {
			case strings.HasPrefix(line, "knitr:"):
				return EngineKnitr
			case strings.HasPrefix(line, "jupyter:"):
				return EngineJupyter
			}
		}
	}
	return ""
}

// fileHasRChunk reports whether a document carries a fenced R chunk header —
// ```{r} or ```{r, …} or ```{r name}.
//
// A line match, in the spirit of goPackageIsMain and plumber's annotations, and
// never a parse. Markdown is not a language this platform has any business
// parsing, and a fence header is a prefix.
func fileHasRChunk(dir, name string) bool {
	f, err := openInRelease(dir, name)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRScanBytes)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		rest, ok := strings.CutPrefix(line, "```{")
		if !ok {
			if rest, ok = strings.CutPrefix(line, "````{"); !ok {
				continue
			}
		}
		rest = strings.TrimSpace(rest)
		if rest == "r}" {
			return true
		}
		// ```{r name}, ```{r, echo=FALSE}, ```{r setup, include=FALSE}
		if after, ok := strings.CutPrefix(rest, "r"); ok {
			if after != "" && (after[0] == ' ' || after[0] == ',' || after[0] == '\t') {
				return true
			}
		}
	}
	return false
}

// isShinyRmd reports whether any .Rmd at the release root declares
// `runtime: shiny`.
//
// Asked before the document detectors rather than inside them, because such a
// document is not a document at all: it holds reactive expressions and needs a
// live R process, and rendering it would produce a page of broken widgets under
// a green deploy.
func isShinyRmd(srcDir string) bool {
	for _, name := range rSourceFiles(srcDir, ".Rmd") {
		if rmdRuntimeIsShiny(srcDir, name) {
			return true
		}
	}
	return false
}

// The R render, which reaches the notebook seam and does not build a second
// one: the same RenderDir, the same RenderEntry, the same refusal when a
// release already holds that path, and the same document root that is never the
// release directory.
//
// Rooting at the release would publish renv/, renv.lock and the .Rmd source,
// exactly as it would publish venv/ for a notebook — staticserve's refusal list
// is not configurable and covers dotfiles, toft.toml and
// ecosystem.config.json, not a library directory.

// RRenderExpr renders an R Markdown document.
//
// output_dir and output_file are passed because rmarkdown::render defaults to
// writing *beside the source*, which would drop an .html into the release root
// at every build and give the next detection something new to read.
//
// knit_root_dir is left to rmarkdown, which knits in the document's own
// directory (or the front matter's knit_root_dir) — what the Knit button does,
// so a report in reports/ reads reports/data.csv as "data.csv" here as it does
// on the author's laptop. Forcing the release root was the same thing for a
// document at the root and a different one for `[notebook] file =
// "reports/weekly.Rmd"` (BB-001). output_dir and intermediates_dir are
// normalised by rmarkdown before it changes directory, so the relative
// RenderDir still means the release's.
//
// A constant, on the launchers' terms: the file and the directory arrive as
// arguments and are read back with commandArgs, so no repository content is
// ever pasted into R source the platform evaluates.
const RRenderExpr = `a <- commandArgs(trailingOnly = TRUE); ` +
	`rmarkdown::render(input = a[1], output_dir = a[2], output_file = a[3], ` +
	`intermediates_dir = a[2], quiet = FALSE)`

// rRenderCommand is the render invocation for one R document framework.
//
// Quarto's is the same `quarto render` the notebooks work already uses, with
// one difference that decides the whole family split: QUARTO_R names the
// release's R rather than letting Quarto find one on PATH, for the reason
// QUARTO_PYTHON is not optional there.
func (b *Builder) rRenderCommand(framework Framework, file, releaseDir string, rt *resolvedRuntime) (
	string, []string, map[string]string, error) {

	lib := rLibraryPath(releaseDir, rt.rSeries())

	switch framework {
	case FrameworkRMarkdown:
		return rBuildBinary(rt), []string{"-e", RRenderExpr, file, RenderDir, RenderEntry},
			map[string]string{"R_LIBS_SITE": lib}, nil

	case FrameworkQuarto:
		bin, err := QuartoBin()
		if err != nil {
			return "", nil, nil, err
		}
		return bin, []string{
			"render", file, "--to", "html",
			"--output", RenderEntry, "--output-dir", RenderDir,
		}, map[string]string{
			// The release's own library, so a report's library(dplyr) means
			// what its renv.lock says rather than whatever the build host
			// happens to have.
			"R_LIBS_SITE": lib,
			// Absolute, because the engine's working directory is the
			// release but its subprocesses' are not guaranteed to be.
			"QUARTO_R": rBuildBinary(rt),
		}, nil
	}
	return "", nil, nil, NotebookError{Framework: framework, Field: "engine",
		Msg: "is not an R document framework"}
}

// renderRDocument runs the render and leaves RenderDir holding exactly one
// document, on the notebook seam's terms.
//
// **The render runs with the app's environment**, which is what lets a report
// read DATABASE_URL and is therefore how a connection string can end up in a
// document an anonymous visitor may read. That is the notebook framework's
// sharpest edge and it is identical here — the build has held those variables
// since long before either existed. What is new for a *document* is that the
// output is published, so the names that were exported go in the build log and
// never the values.
func (b *Builder) renderRDocument(deployID string, result DetectionResult, releaseDir string,
	rt *resolvedRuntime, envVars, buildEnv map[string]string, lg buildLog) error {

	file := result.Entrypoint
	if file == "" {
		return NotebookError{Framework: result.Framework,
			Msg: "no document was resolved for this release"}
	}

	renderDir := filepath.Join(releaseDir, RenderDir)
	if err := b.asBuild(releaseDir, func() error { return prepareRenderDir(renderDir) }); err != nil {
		return err
	}

	engine := "rmarkdown::render"
	if result.Framework == FrameworkQuarto {
		engine = "quarto render (knitr)"
	}
	lg.stepf("==> Rendering %s with %s...", file, engine)
	reportRenderEnv(envVars, lg)

	name, args, extra, err := b.rRenderCommand(result.Framework, file, releaseDir, rt)
	if err != nil {
		return err
	}
	// The engine's own variables, over the app's: what the render must use is
	// not the app's to choose.
	env := make(map[string]string, len(buildEnv)+len(extra))
	for k, v := range buildEnv {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}

	timeout := b.notebookRenderTimeout()
	if err := b.runCommandFor(timeout, deployID, releaseDir, name, args, env, lg.output); err != nil {
		// The clock's remedy only when the clock is what stopped it: a
		// document that failed on its own error was told to ask for more
		// time (docs/bugs 2026-10-28 #4).
		if !timedOut(err) {
			return fmt.Errorf("rendering %s failed: %v", file, err)
		}
		return fmt.Errorf("rendering %s failed: %v\n    If the document needs longer than %s, "+
			"an administrator can raise %s in Settings",
			file, err, timeout, "notebook_render_timeout_seconds")
	}

	out := filepath.Join(renderDir, RenderEntry)
	if !fileExists(out) {
		return fmt.Errorf("rendering %s produced no %s. The engine exited successfully and wrote "+
			"nothing there, which is a bug in this platform rather than in the document",
			file, filepath.Join(RenderDir, RenderEntry))
	}
	if err := b.asBuild(releaseDir, func() error {
		return os.WriteFile(filepath.Join(renderDir, renderMarker),
			[]byte("Written by Toft. This directory is the rendered document, not the app's source.\n"),
			0o600)
	}); err != nil {
		return fmt.Errorf("marking the render directory: %w", err)
	}
	lg.stepf("    Rendered to %s/%s", RenderDir, RenderEntry)
	return nil
}

// severalRmdMessage is the refusal for a release holding more than one R
// Markdown document and nothing naming the app, or "" when that is not the
// case.
func severalRmdMessage(srcDir string) string {
	rmds := rSourceFiles(srcDir, ".Rmd")
	if len(rmds) < 2 || readNotebookDeclaration(srcDir).File != "" {
		return ""
	}
	return fmt.Sprintf("This repository has %d R Markdown documents — %s — and nothing saying "+
		"which one is the app. Name it in %s:\n\n        [notebook]\n        file = %q\n",
		len(rmds), strings.Join(rmds, ", "), ManifestFile, rmds[0])
}

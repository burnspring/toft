package builder

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// R detection: the shapes R actually comes in, and deliberately not a generic
// one.
//
// `python app.py` is a program somebody wrote and generic Python hosts it.
// `Rscript app.R` is not the same thing: an R script runs to the end and exits,
// and a workload that exits is a crash loop. So there is no generic R framework
// — a repository that is R and matches none of the named shapes is refused with
// the shapes named, which is a better message than a green deploy and a
// restarting process.
//
// Detection reads the *source* and never a render. An author who previews an R
// Notebook in RStudio gets report.nb.html beside report.Rmd and many
// repositories commit it; treating a committed render as the artifact would
// break detection stability, because detection runs again at every wake, every
// restore and every boot — by which time the directory contains the build's own
// output as well.

// maxRScanBytes bounds the line scan below. A plumber annotation or a shiny
// runtime declaration lives in the first few lines of a file by convention;
// reading a whole data file to find out it is not one is the cost this avoids.
const maxRScanBytes = 64 * 1024

// detectR answers for the two R shapes that are *processes*: an application
// somebody connects to. The two that are documents are rendered at build time
// and are detected elsewhere, with the render seam the notebook work owns.
//
// Order is the specification. Shiny before plumber, because a repository that
// declares a shiny app and happens to carry an R file with a plumber annotation
// in a comment is a shiny app; the reverse mistake is the one that produces a
// process bound to a port nobody is talking to.
func detectR(srcDir string) DetectionResult {
	if r := detectShiny(srcDir); r.Framework != FrameworkUnsupported {
		return r
	}
	if r := detectPlumber(srcDir); r.Framework != FrameworkUnsupported {
		return r
	}
	return DetectionResult{Framework: FrameworkUnsupported}
}

// detectShiny finds the four shapes a Shiny application comes in, plus the one
// that looks like a document and is not.
func detectShiny(srcDir string) DetectionResult {
	// app.R — one file holding ui and server, which is what `shiny::runApp`
	// looks for and what every tutorial produces.
	if name := rFileNamed(srcDir, "app.R"); name != "" {
		return DetectionResult{Framework: FrameworkShiny, Entrypoint: name}
	}
	// ui.R and server.R together. Either alone is not a Shiny app: the pair is
	// the contract, and a lone ui.R is a fragment somebody is part-way through.
	if rFileNamed(srcDir, "ui.R") != "" && rFileNamed(srcDir, "server.R") != "" {
		return DetectionResult{Framework: FrameworkShiny, Entrypoint: "."}
	}
	// server.R with no ui.R, and a www/index.html to stand in for it. This is
	// Shiny's custom-HTML-UI shape, documented by the project and older than
	// app.R: the browser is served the hand-written page and it binds to the
	// same outputs. Without this branch the repository reads as "server.R
	// alone", which the rule above is right to refuse — the difference is the
	// index.html, so that is what this looks for rather than relaxing the pair.
	if rFileNamed(srcDir, "server.R") != "" &&
		fileExists(filepath.Join(srcDir, "www", "index.html")) {
		return DetectionResult{Framework: FrameworkShiny, Entrypoint: "."}
	}

	// An .Rmd whose YAML header says `runtime: shiny` is a server, not a
	// document. It holds reactive expressions and needs a live R process;
	// rendering it would produce a page of broken widgets and a green deploy.
	// This is why the scan happens here rather than in the document detector —
	// the answer changes which half of R the repository belongs to.
	for _, name := range rSourceFiles(srcDir, ".Rmd") {
		if rmdRuntimeIsShiny(srcDir, name) {
			return DetectionResult{Framework: FrameworkShiny, Entrypoint: name}
		}
	}
	return DetectionResult{Framework: FrameworkUnsupported}
}

// detectPlumber finds an R file that publishes HTTP routes.
func detectPlumber(srcDir string) DetectionResult {
	// The conventional name, which `plumber::plumb()` finds with no argument.
	if name := rFileNamed(srcDir, "plumber.R"); name != "" {
		return DetectionResult{Framework: FrameworkPlumber, Entrypoint: name}
	}
	// Otherwise any R file carrying plumber's annotations, scanned line by line
	// in the spirit of goPackageIsMain and never parsed. R is not a language
	// this platform has any business parsing, and an annotation is a comment:
	// a prefix match on a trimmed line is the whole of what can be claimed.
	for _, name := range rSourceFiles(srcDir, ".R") {
		if fileHasPlumberAnnotation(srcDir, name) {
			return DetectionResult{Framework: FrameworkPlumber, Entrypoint: name}
		}
	}
	return DetectionResult{Framework: FrameworkUnsupported}
}

// rFileNamed resolves one of R's conventional entrypoint names against the
// repository root, case-insensitively, and answers the name **as it is on
// disk** — which is what the entrypoint has to record, because the launcher
// hands it to R on a case-sensitive filesystem.
//
// The case is not a detail and not a typo to be lenient about. R's own
// documentation writes app.R, and the whole R ecosystem also writes app.r:
// RStudio creates either, `shiny::runApp` finds either, and Posit Connect
// deploys either. A platform that matched only one of them would refuse
// working applications and tell their authors that R is unsupported, which is
// the one thing the refusal below is careful not to say.
//
// The root only, on rSourceFiles' terms and for its reason.
func rFileNamed(srcDir, name string) string {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), name) {
			return e.Name()
		}
	}
	return ""
}

// rSourceFiles lists the repository root's files with the given extension, in
// directory order, matched case-insensitively — .R and .r are both ordinary on
// disk and neither is a different language.
//
// The root only. A Shiny app's helper files live in R/ and its data in data/,
// and descending into them would let a vendored example or a test fixture
// decide the whole repository's framework.
func rSourceFiles(srcDir, ext string) []string {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ext) {
			out = append(out, e.Name())
		}
	}
	return out
}

// rmdRuntimeIsShiny reports whether an R Markdown document's YAML header
// declares `runtime: shiny`.
//
// Only the header, which is the block between the first `---` and the next one.
// A `runtime: shiny` appearing later in the document is inside a chunk or a
// sentence about Shiny, and neither makes the document an application.
func rmdRuntimeIsShiny(dir, name string) bool {
	f, err := openInRelease(dir, name)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRScanBytes)

	inHeader := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			if !inHeader {
				inHeader = true
				continue
			}
			return false // header closed without saying so
		}
		if !inHeader {
			// Anything before the opening --- means there is no YAML header.
			if line == "" {
				continue
			}
			return false
		}
		// `runtime: shiny` and `runtime: shinyrmd` are both live documents.
		if v, ok := strings.CutPrefix(line, "runtime:"); ok {
			return strings.HasPrefix(strings.TrimSpace(v), "shiny")
		}
	}
	return false
}

// fileHasPlumberAnnotation reports whether a file carries a plumber route
// annotation. Both comment markers are plumber's own — `#*` is current and `#'`
// is what older code and roxygen-styled examples use.
func fileHasPlumberAnnotation(dir, name string) bool {
	f, err := openInRelease(dir, name)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRScanBytes)

	verbs := []string{"@get", "@post", "@put", "@delete", "@head", "@options", "@patch", "@use"}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		marker, ok := strings.CutPrefix(line, "#*")
		if !ok {
			if marker, ok = strings.CutPrefix(line, "#'"); !ok {
				continue
			}
		}
		marker = strings.TrimSpace(marker)
		for _, v := range verbs {
			if strings.HasPrefix(marker, v) {
				return true
			}
		}
	}
	return false
}

// repositoryLooksLikeR reports whether a repository is R at all, which is what
// turns "matched none of the named shapes" into a refusal that names them
// rather than the generic unsupported message.
func repositoryLooksLikeR(srcDir string) bool {
	if fileExists(filepath.Join(srcDir, "renv.lock")) || fileExists(filepath.Join(srcDir, "DESCRIPTION")) {
		return true
	}
	for _, ext := range []string{".R", ".Rmd", ".qmd"} {
		if len(rSourceFiles(srcDir, ext)) > 0 {
			return true
		}
	}
	return false
}

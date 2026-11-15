package builder

import (
	"os"
	"path/filepath"
	"testing"
)

func writeR(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShinyIsDetectedInEveryShapeItComesIn(t *testing.T) {
	t.Run("app.R", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "app.R", "library(shiny)\nshinyApp(ui, server)\n")
		if got := DetectFramework(dir); got.Framework != FrameworkShiny {
			t.Fatalf("framework = %q, want shiny", got.Framework)
		}
	})

	t.Run("ui.R and server.R together", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "ui.R", "fluidPage()\n")
		writeR(t, dir, "server.R", "function(input, output) {}\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkShiny {
			t.Fatalf("framework = %q, want shiny", got.Framework)
		}
		// runApp takes a directory here; naming either file alone starts half
		// an app.
		if got.Entrypoint != "." {
			t.Errorf("entrypoint = %q, want \".\"", got.Entrypoint)
		}
	})

	// server.R with a hand-written UI in www/index.html. Shiny's custom-HTML-UI
	// shape, and the reason the pair rule above is a rule about ui.R rather
	// than about server.R being enough on its own.
	t.Run("server.R with www/index.html", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "server.R", "function(input, output) {}\n")
		if err := os.MkdirAll(filepath.Join(dir, "www"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeR(t, dir, filepath.Join("www", "index.html"), "<html></html>")
		got := DetectFramework(dir)
		if got.Framework != FrameworkShiny {
			t.Fatalf("framework = %q, want shiny", got.Framework)
		}
		if got.Entrypoint != "." {
			t.Errorf("entrypoint = %q, want \".\"", got.Entrypoint)
		}
	})

	// A lone ui.R is somebody part-way through, not a Shiny app.
	t.Run("ui.R alone is not shiny", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "ui.R", "fluidPage()\n")
		if got := DetectFramework(dir); got.Framework == FrameworkShiny {
			t.Fatal("a lone ui.R was detected as shiny")
		}
	})

	// A lone server.R is the same fragment, and www/index.html is what makes
	// the difference — not the extension's case, and not leniency.
	t.Run("server.R alone is not shiny", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "server.r", "function(input, output) {}\n")
		if got := DetectFramework(dir); got.Framework == FrameworkShiny {
			t.Fatal("a lone server.R was detected as shiny")
		}
	})
}

// R writes app.R and R writes app.r, and both are ordinary. Thirty real Shiny
// applications published through Posit Connect were the evidence: four of them
// were refused as "unsupported framework" for nothing but the case of a letter,
// and their authors were told R was not supported, which was not true.
//
// The entrypoint has to come back as it is on disk. The launcher hands it to R
// on a case-sensitive filesystem, so a detector that normalized the name to
// app.R here would trade this refusal for a file-not-found at start.
func TestRsEntrypointNamesAreMatchedInEitherCase(t *testing.T) {
	t.Run("app.r", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "app.r", "library(shiny)\nshinyApp(ui, server)\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkShiny {
			t.Fatalf("framework = %q, want shiny", got.Framework)
		}
		if got.Entrypoint != "app.r" {
			t.Errorf("entrypoint = %q, want the name on disk", got.Entrypoint)
		}
	})

	t.Run("App.R", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "App.R", "library(shiny)\nshinyApp(ui, server)\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkShiny {
			t.Fatalf("framework = %q, want shiny", got.Framework)
		}
		if got.Entrypoint != "App.R" {
			t.Errorf("entrypoint = %q, want the name on disk", got.Entrypoint)
		}
	})

	t.Run("ui.r and server.r", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "ui.r", "fluidPage()\n")
		writeR(t, dir, "server.r", "function(input, output) {}\n")
		if got := DetectFramework(dir); got.Framework != FrameworkShiny {
			t.Fatalf("framework = %q, want shiny", got.Framework)
		}
	})

	t.Run("plumber.r", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "plumber.r", "#* @get /health\nfunction() list(ok = TRUE)\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkPlumber {
			t.Fatalf("framework = %q, want plumber", got.Framework)
		}
		if got.Entrypoint != "plumber.r" {
			t.Errorf("entrypoint = %q, want the name on disk", got.Entrypoint)
		}
	})
}

// An .Rmd whose header says `runtime: shiny` is a server, not a document. It
// holds reactive expressions and needs a live R process; rendering it would
// produce a page of broken widgets and a green deploy.
func TestAnRmdDeclaringShinyRuntimeIsAnApplication(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "report.Rmd", `---
title: "Live"
runtime: shiny
output: html_document
---

Some prose.
`)
	got := DetectFramework(dir)
	if got.Framework != FrameworkShiny {
		t.Fatalf("framework = %q, want shiny — an .Rmd with runtime: shiny is never rendered", got.Framework)
	}
	if got.Entrypoint != "report.Rmd" {
		t.Errorf("entrypoint = %q, want report.Rmd", got.Entrypoint)
	}
}

// The same file without that key is not an application. It is a document, and
// until the document half lands it is simply not shiny.
func TestAnOrdinaryRmdIsNotShiny(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "report.Rmd", `---
title: "Report"
output: html_document
---

Some prose about runtime: shiny, which is not a declaration.
`)
	if got := DetectFramework(dir); got.Framework == FrameworkShiny {
		t.Fatal("an .Rmd mentioning runtime: shiny in its prose was detected as an application")
	}
}

func TestPlumberIsDetectedByFileAndByAnnotation(t *testing.T) {
	t.Run("plumber.R", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "plumber.R", "#* @get /health\nfunction() list(ok = TRUE)\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkPlumber {
			t.Fatalf("framework = %q, want plumber", got.Framework)
		}
		if got.Entrypoint != "plumber.R" {
			t.Errorf("entrypoint = %q, want plumber.R", got.Entrypoint)
		}
	})

	t.Run("an annotated .R file under another name", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "api.R", "#* @post /submit\nfunction(req) NULL\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkPlumber {
			t.Fatalf("framework = %q, want plumber", got.Framework)
		}
		if got.Entrypoint != "api.R" {
			t.Errorf("entrypoint = %q, want api.R", got.Entrypoint)
		}
	})

	// An .R file with no route annotation publishes nothing.
	t.Run("an unannotated .R file is not plumber", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "helpers.R", "# a comment\nf <- function() 1\n")
		if got := DetectFramework(dir); got.Framework == FrameworkPlumber {
			t.Fatal("an .R file with no annotations was detected as plumber")
		}
	})
}

// Shiny wins over a stray annotation, which is the order D10 states.
func TestShinyBeatsAStrayPlumberAnnotation(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "app.R", "shinyApp(ui, server)\n")
	writeR(t, dir, "notes.R", "#* @get /old\n")
	if got := DetectFramework(dir); got.Framework != FrameworkShiny {
		t.Fatalf("framework = %q, want shiny", got.Framework)
	}
}

// R is checked *before* Python, because an R repository frequently carries a
// requirements.txt — a Shiny app calling reticulate, an analysis whose data was
// fetched by a Python script. Detected as generic Python it would deploy, find
// no app to start, and crash-loop.
func TestRBeatsPythonWhenARepositoryIsBoth(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "app.R", "shinyApp(ui, server)\n")
	writeR(t, dir, "requirements.txt", "pandas\n")
	writeR(t, dir, "main.py", "print('hi')\n")
	if got := DetectFramework(dir); got.Framework != FrameworkShiny {
		t.Fatalf("framework = %q, want shiny — an R app carrying a requirements.txt is still R", got.Framework)
	}
}

// A Node service that happens to carry an R script beside it is a Node service.
func TestNodeStillWinsOverAStrayRFile(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "package.json", `{"scripts":{"start":"node index.js"}}`)
	writeR(t, dir, "analysis.R", "#* @get /x\n")
	if got := DetectFramework(dir); got.Framework != FrameworkNode {
		t.Fatalf("framework = %q, want node", got.Framework)
	}
}

// Detection reads the source and never the render. An author who previews an R
// Notebook in RStudio gets report.nb.html beside report.Rmd and many
// repositories commit it — and detection runs again at every wake, restore and
// boot, by which time the build's own output is in the directory too.
func TestACommittedRenderDoesNotChangeDetection(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "app.R", "shinyApp(ui, server)\n")
	before := DetectFramework(dir)

	writeR(t, dir, "report.nb.html", "<html>rendered earlier</html>")
	writeR(t, dir, "index.html", "<html>also rendered</html>")
	after := DetectFramework(dir)

	if before.Framework != after.Framework {
		t.Fatalf("detection changed once a render was present: %q then %q", before.Framework, after.Framework)
	}
}

// A repository that is R and matches none of the shapes is refused with the
// shapes named — not told that R is unsupported, which is not true.
func TestAnRRepositoryMatchingNothingIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "analysis.R", "x <- 1\nprint(x)\n")
	writeR(t, dir, "renv.lock", `{"Packages":{}}`)

	if got := DetectFramework(dir); got.Framework != FrameworkUnsupported {
		t.Fatalf("framework = %q, want unsupported — there is no generic R app", got.Framework)
	}
	msg := UnsupportedMessage(dir)
	for _, want := range []string{"Shiny", "plumber", "R Markdown", "Quarto", "exits"} {
		if !contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

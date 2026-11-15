package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/appint"
)

// D11's compatibility rule, which is the row that protects apps deployed before
// R: a .qmd that declares no engine and holds no R chunks stays `notebook`, in
// the Python family, exactly as it was.
func TestAQmdWithNoEngineAndNoRChunksStaysANotebook(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "report.qmd", `---
title: "Report"
---

Some prose, and a Python chunk.

`+"```{python}"+`
print("hi")
`+"```"+`
`)
	got := DetectFramework(dir)
	if got.Framework != FrameworkNotebook {
		t.Fatalf("framework = %q, want notebook — no app deployed before R may change framework "+
			"after it", got.Framework)
	}
	if LanguageOf(got.Framework) != "python" {
		t.Errorf("language = %q, want python", LanguageOf(got.Framework))
	}
}

// A .qmd with R chunks is Quarto under knitr, and belongs to the R family.
func TestAQmdWithRChunksIsQuartoInTheRFamily(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "report.qmd", `---
title: "Report"
---

`+"```{r}"+`
summary(cars)
`+"```"+`
`)
	got := DetectFramework(dir)
	if got.Framework != FrameworkQuarto {
		t.Fatalf("framework = %q, want quarto", got.Framework)
	}
	if LanguageOf(got.Framework) != "r" {
		t.Errorf("language = %q, want r", LanguageOf(got.Framework))
	}
}

// A declared engine settles it in both directions, and beats the chunk scan.
func TestTheDeclaredEngineDecidesTheFamily(t *testing.T) {
	t.Run("jupyter beats R chunks", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "report.qmd", "---\ntitle: t\n---\n\n```{r}\n1\n```\n")
		writeR(t, dir, ManifestFile, "[notebook]\nengine = \"jupyter\"\nfile = \"report.qmd\"\n")
		if got := DetectFramework(dir); got.Framework != FrameworkNotebook {
			t.Fatalf("framework = %q, want notebook", got.Framework)
		}
	})

	t.Run("knitr with no R chunks", func(t *testing.T) {
		dir := t.TempDir()
		writeR(t, dir, "report.qmd", "---\ntitle: t\n---\n\nprose only\n")
		writeR(t, dir, ManifestFile, "[notebook]\nengine = \"knitr\"\nfile = \"report.qmd\"\n")
		got := DetectFramework(dir)
		if got.Framework != FrameworkQuarto {
			t.Fatalf("framework = %q, want quarto", got.Framework)
		}
	})
}

// The YAML header's own engine key, which is how Quarto itself is told.
func TestTheHeaderEngineDecidesTheFamily(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   Framework
	}{
		{"engine: knitr", FrameworkQuarto},
		{"engine: jupyter", FrameworkNotebook},
		{"knitr:\n  opts_chunk:\n    echo: false", FrameworkQuarto},
		{"jupyter: python3", FrameworkNotebook},
	} {
		dir := t.TempDir()
		writeR(t, dir, "report.qmd", "---\ntitle: t\n"+tc.header+"\n---\n\nprose\n")
		if got := DetectFramework(dir); got.Framework != tc.want {
			t.Errorf("header %q: framework = %q, want %q", tc.header, got.Framework, tc.want)
		}
	}
}

// An .Rmd is R Markdown and has no other family to belong to.
func TestAnRmdIsRMarkdown(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "report.Rmd", "---\ntitle: t\n---\n\n```{r}\n1\n```\n")
	got := DetectFramework(dir)
	if got.Framework != FrameworkRMarkdown {
		t.Fatalf("framework = %q, want rmarkdown", got.Framework)
	}
	if got.Entrypoint != "report.Rmd" {
		t.Errorf("entrypoint = %q, want report.Rmd", got.Entrypoint)
	}
}

// An .Rmd declaring `runtime: shiny` is an application and is never rendered —
// checked before the document detectors, which is where the ordering lives.
func TestAShinyRmdBeatsTheDocumentDetector(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "live.Rmd", "---\ntitle: t\nruntime: shiny\n---\n\n```{r}\n1\n```\n")
	if got := DetectFramework(dir); got.Framework != FrameworkShiny {
		t.Fatalf("framework = %q, want shiny", got.Framework)
	}
}

// The document frameworks are R at build time and a file server after, so they
// are refused viewer identity on `notebook`'s terms while the two R
// applications are granted it.
func TestOnlyTheRApplicationsGetViewerIdentity(t *testing.T) {
	// Kept here rather than in internal/appint so the framework constants are
	// the detector's own rather than restated strings.
	for _, tc := range []struct {
		framework Framework
		want      bool
	}{
		{FrameworkShiny, true},
		{FrameworkPlumber, true},
		{FrameworkRMarkdown, false},
		{FrameworkQuarto, false},
	} {
		got := appint.FrameworkHasAProcess(string(tc.framework))
		if got != tc.want {
			t.Errorf("%s: viewer identity granted = %v, want %v", tc.framework, got, tc.want)
		}
	}
}

// The render directory is the platform's and is never the release: rooting a
// file server at the release would publish renv/, renv.lock and the source.
func TestTheRenderDirIsNotTheRelease(t *testing.T) {
	if RenderDir == "" || RenderDir == "." {
		t.Fatalf("RenderDir = %q", RenderDir)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, RenderDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(dir, RenderDir)) {
		t.Fatal("render dir not created")
	}
}

// salesReportBundle is the shape rsconnect publishes for an R Markdown report
// that mails itself: the report, the blastula template it renders, and a
// manifest naming the report as the content.
func salesReportBundle(t *testing.T, primary string) string {
	t.Helper()
	dir := t.TempDir()
	writeR(t, dir, "sales-report.Rmd", "---\ntitle: Sales\n---\n\n```{r}\n1\n```\n")
	writeR(t, dir, "email.Rmd", "---\noutput: blastula::blastula_email\n---\n\nHi\n")
	manifest := `{"version":1,"platform":"4.6.1",
		"metadata":{"appmode":"rmd-static","primary_rmd":` + primary + `},
		"environment":{"r":{"requires":"4.6.1"}},
		"packages":{"knitr":{"Source":"CRAN","Repository":"https://cloud.r-project.org",
			"description":{"Package":"knitr","Version":"1.50"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Two documents are two candidates, but a Connect bundle says which one it
// publishes — and that is the author's answer, read as theirs.
func TestAConnectBundleNamesItsPrimaryDocument(t *testing.T) {
	got := DetectFramework(salesReportBundle(t, `"sales-report.Rmd"`))
	if got.Framework != FrameworkRMarkdown || got.Entrypoint != "sales-report.Rmd" {
		t.Fatalf("detected %q / %q, want rmarkdown / sales-report.Rmd", got.Framework, got.Entrypoint)
	}
}

// The author's own declaration to this platform outranks the bundle's.
func TestNotebookFileOutranksTheBundlesPrimary(t *testing.T) {
	dir := salesReportBundle(t, `"sales-report.Rmd"`)
	writeR(t, dir, ManifestFile, "[notebook]\nfile = \"email.Rmd\"\n")
	if got := DetectFramework(dir); got.Entrypoint != "email.Rmd" {
		t.Fatalf("entrypoint = %q, want the declared email.Rmd", got.Entrypoint)
	}
}

// A primary that is not in the bundle, or not a document, decides nothing — and
// the refusal then names the candidates rather than listing frameworks.
func TestAMissingPrimaryLeavesTheCandidatesNamed(t *testing.T) {
	for _, primary := range []string{`"gone.Rmd"`, `"../sales-report.Rmd"`, `"R/data.R"`, `null`} {
		dir := salesReportBundle(t, primary)
		if got := DetectFramework(dir); got.Framework != FrameworkUnsupported {
			t.Fatalf("primary %s: detected %q, want unsupported", primary, got.Framework)
		}
		msg := UnsupportedMessage(dir)
		for _, want := range []string{"email.Rmd", "sales-report.Rmd", "[notebook]", ManifestFile} {
			if !strings.Contains(msg, want) {
				t.Errorf("primary %s: refusal does not name %q:\n%s", primary, want, msg)
			}
		}
	}
}

// A manifest.json that is not a Connect bundle's names nothing, however it is
// spelled.
func TestOnlyAnAdoptedManifestNamesADocument(t *testing.T) {
	dir := t.TempDir()
	writeR(t, dir, "a.Rmd", "---\ntitle: a\n---\n")
	writeR(t, dir, "b.Rmd", "---\ntitle: b\n---\n")
	os.WriteFile(filepath.Join(dir, "manifest.json"),
		[]byte(`{"name":"pwa","metadata":{"primary_rmd":"a.Rmd"}}`), 0o644)
	if got := DetectFramework(dir); got.Framework != FrameworkUnsupported {
		t.Fatalf("detected %q from a manifest that is not Connect's", got.Framework)
	}
}

// A Shiny app with a README.Rmd at its root is still the app its Connect
// bundle says it is — the .Rmd is one document candidate, not the content.
func TestAConnectShinyBundleIsNotItsReadme(t *testing.T) {
	for _, shape := range []map[string]string{
		{"app.R": "shinyApp(ui, server)\n"},
		{"ui.R": "fluidPage()\n", "server.R": "function(input, output) {}\n"},
	} {
		dir := t.TempDir()
		for name, body := range shape {
			writeR(t, dir, name, body)
		}
		writeR(t, dir, "README.Rmd", "---\ntitle: About\n---\n\nHello\n")
		manifest := `{"version":1,"platform":"4.4.1",
			"metadata":{"appmode":"shiny","primary_rmd":null},
			"packages":{"shiny":{"Source":"CRAN","description":{"Package":"shiny","Version":"1.9.1"}}}}`
		writeR(t, dir, "manifest.json", manifest)
		if got := DetectFramework(dir); got.Framework != FrameworkShiny {
			t.Fatalf("%v: detected %q / %q, want shiny", shape, got.Framework, got.Entrypoint)
		}
		// Without the bundle's word, the lone .Rmd is still the document.
		os.Remove(filepath.Join(dir, "manifest.json"))
		if got := DetectFramework(dir); got.Framework != FrameworkRMarkdown {
			t.Fatalf("%v without a manifest: detected %q, want rmarkdown", shape, got.Framework)
		}
	}
}

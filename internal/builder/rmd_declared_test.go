package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/config"
)

// BB-001: the [notebook] table speaks for an .Rmd as it does for a .qmd —
// `file` picks one of several or reaches a subdirectory, and knitr is its
// engine. Every one of these was refused as "not a .ipynb or a .qmd".
func TestTheNotebookTableNamesAnRmd(t *testing.T) {
	doc := []byte("---\ntitle: x\n---\n\n```{r}\n1\n```\n")
	cases := []struct {
		name, manifest, want string
		files                []string
	}{
		{"one of two", "[notebook]\nfile = \"b.Rmd\"\n", "b.Rmd", []string{"a.Rmd", "b.Rmd"}},
		{"a subdirectory", "[notebook]\nfile = \"reports/weekly.Rmd\"\n", "reports/weekly.Rmd", []string{"reports/weekly.Rmd"}},
		{"engine only", "[notebook]\nengine = \"knitr\"\n", "report.Rmd", []string{"report.Rmd"}},
		{"file and engine", "[notebook]\nfile = \"b.Rmd\"\nengine = \"knitr\"\n", "b.Rmd", []string{"a.Rmd", "b.Rmd"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeManifest(t, c.manifest)
			for _, f := range c.files {
				p := filepath.Join(dir, filepath.FromSlash(f))
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, doc, 0o644)
			}
			r := DetectFramework(dir)
			if r.Framework != FrameworkRMarkdown || r.Entrypoint != c.want {
				t.Fatalf("detected %s %q, want rmarkdown %q", r.Framework, r.Entrypoint, c.want)
			}
			if _, err := ResolveNotebook(dir); err != nil {
				t.Fatalf("ResolveNotebook: %v", err)
			}
			for _, p := range RunPreflight(dir, "shared", nil, config.Envelope{}).Problems {
				if strings.HasPrefix(p.Code, "invalid_notebook_") || p.Code == "unsupported_framework" {
					t.Errorf("problem %s: %s", p.Code, p.Message)
				}
			}
		})
	}
}

func TestTheNotebookTableStillJudgesAnRmd(t *testing.T) {
	doc := []byte("---\ntitle: x\n---\n")
	cases := []struct{ name, manifest, field string }{
		{"missing", "[notebook]\nfile = \"c.Rmd\"\n", "file"},
		{"escaping", "[notebook]\nfile = \"../c.Rmd\"\n", "file"},
		{"python engine", "[notebook]\nfile = \"a.Rmd\"\nengine = \"nbconvert\"\n", "engine"},
		{"no engine", "[notebook]\nengine = \"latex\"\n", "engine"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeManifest(t, c.manifest)
			os.WriteFile(filepath.Join(dir, "a.Rmd"), doc, 0o644)
			_, err := ResolveNotebook(dir)
			ne, ok := err.(NotebookError)
			if !ok || ne.Field != c.field {
				t.Fatalf("err = %v, want a [notebook] %s refusal", err, c.field)
			}
		})
	}
}

// Two .Rmd and no file is the several-documents refusal, which names R
// Markdown in the list of what is supported.
func TestTwoRmdsAreAskedWhichOne(t *testing.T) {
	dir := writeManifest(t, "[notebook]\nengine = \"knitr\"\n")
	os.WriteFile(filepath.Join(dir, "a.Rmd"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.Rmd"), []byte("x"), 0o644)
	if r := DetectFramework(dir); r.Framework != FrameworkUnsupported {
		t.Fatalf("detected %s", r.Framework)
	}
	msg := UnsupportedMessage(dir)
	if !strings.Contains(msg, "2 R Markdown documents") || !strings.Contains(msg, "R Markdown (.Rmd)") {
		t.Errorf("message = %s", msg)
	}
}

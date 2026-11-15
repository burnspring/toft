package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/config"
)

// BB-058, fifth 2026-11-03 black-box list: a library-only Go tree is told it
// has no framework and that its [runtime] go is wrong — every problem, not the
// first.
func TestCheckOfALibraryTreeStillJudgesItsRuntime(t *testing.T) {
	dir := writeManifest(t, "[runtime]\ngo = \"1.26\"\n")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/lib\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package lib\n"), 0o644)
	codes := map[string]bool{}
	for _, p := range RunPreflight(dir, "shared", nil, config.Envelope{}).Problems {
		codes[p.Code] = true
	}
	if !codes["unsupported_framework"] || !codes["invalid_runtime"] {
		t.Errorf("codes = %v, want unsupported_framework and invalid_runtime", codes)
	}
}

// BB-064: "holds no a .ipynb or a .qmd".
func TestANotebookTableWithNoDocumentReadsCleanly(t *testing.T) {
	dir := writeManifest(t, "[notebook]\n")
	_, err := ResolveNotebook(dir)
	if err == nil || !strings.Contains(err.Error(), "holds no .ipynb, .qmd or .Rmd at its root") {
		t.Errorf("err = %v", err)
	}
}

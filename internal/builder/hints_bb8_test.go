package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D-42: a frontend-only package.json is told to add a server and a start
// script, as the SDK guide says, or to declare the built folder.
func TestFrontendOnlyPackageJSONHintNamesTheStartScript(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"build":"vite build"}}`), 0o644)
	got := unsupportedHint(dir)
	if !strings.Contains(got, `"start" script`) || !strings.Contains(got, `[static] root = "dist"`) {
		t.Errorf("hint = %q", got)
	}
}

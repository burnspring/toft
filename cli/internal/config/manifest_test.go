package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The line a TOML error names is counted from the byte the parser stopped at,
// so a table left open on the last line of a three-line file is line 3 — the
// parser's own counter had stepped past the newline and said 4.
func TestATOMLErrorNamesALineTheFileHas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toft.toml")
	os.WriteFile(path, []byte("[app]\nslug = \"x\"\n[jobs.a\n"), 0o644)
	var into map[string]any
	err := decodeTOMLFile(path, &into)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v, want line 3", err)
	}
}

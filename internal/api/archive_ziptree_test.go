package api

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The archive holds regular files inside the release and nothing a link
// reaches outside it.
func TestZipTreeNeverLeavesTheRelease(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "app.py"), []byte("print(1)"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "pkg", "mod.py"), []byte("x=1"), 0o644))
	must(os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link.txt")))
	must(os.Symlink(outside, filepath.Join(dir, "linkdir")))

	var buf bytes.Buffer
	must(zipTree(&buf, dir))
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	must(err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		must(err)
		b, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Contains(b, []byte("OUTSIDE")) {
			t.Errorf("%s carries content from outside the release", f.Name)
		}
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "app.py" || names[1] != "pkg/mod.py" {
		t.Errorf("archive = %v, want [app.py pkg/mod.py]", names)
	}
}

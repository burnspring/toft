package builder

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// After the fetch no link leaves the release or dangles; one inside it stays
// (security review 2026-11-10, E-2 and E-4).
func TestALinkOutOfAFreshReleaseIsDropped(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644)
	links := map[string]string{
		"requirements.txt":   filepath.Join(outside, "secret"),
		"renv.lock":          filepath.Join(outside, "nothing-yet"),
		"sub/up":             "../../",
		"sub/ok":             "../real.txt",
		".toft-resolved.txt": "/etc/passwd",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	dropped, err := dropEscapingLinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(dropped)
	want := []string{".toft-resolved.txt", "renv.lock", "requirements.txt", "sub/up"}
	if !slices.Equal(dropped, want) {
		t.Errorf("dropped %v, want %v", dropped, want)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub/ok")); err != nil {
		t.Error("a link inside the release was dropped")
	}
}

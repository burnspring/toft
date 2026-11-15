package deps

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeDescription(t *testing.T, lib, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lib, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(lib, dir, "DESCRIPTION"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadRLibrary(t *testing.T) {
	lib := t.TempDir()
	writeDescription(t, lib, "jsonlite", "Package: jsonlite\nVersion: 1.8.9\nTitle: A Simple and Robust JSON Parser\n  and Generator for R\nRepository: CRAN\n")
	writeDescription(t, lib, "curl", "Package: curl\nType: Package\nVersion: 5.2.3\n\nPackage: ignored\nVersion: 0\n")
	// R itself: versioned with the interpreter, never in a CRAN advisory.
	writeDescription(t, lib, "stats", "Package: stats\nVersion: 4.5.1\nPriority: base\n")
	// Recommended packages are on CRAN and are read.
	writeDescription(t, lib, "Matrix", "Package: Matrix\nVersion: 1.7-0\nPriority: recommended\n")
	// No DESCRIPTION: not a package — a staging or lock directory.
	writeDescription(t, lib, "00LOCK-dplyr", "")
	// A DESCRIPTION with no version: counted, not recorded.
	writeDescription(t, lib, "broken", "Package: broken\n")
	if err := os.WriteFile(filepath.Join(lib, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, notes, err := ReadRLibrary(lib)
	if err != nil {
		t.Fatalf("ReadRLibrary: %v", err)
	}
	var names []string
	for _, d := range got {
		if d.Ecosystem != "cran" || d.Direct || d.Source != "" {
			t.Errorf("%s: row %+v, want a bare cran row", d.Package, d)
		}
		names = append(names, d.Package+"@"+d.Version)
	}
	want := []string{"Matrix@1.7-0", "curl@5.2.3", "jsonlite@1.8.9"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("packages %v, want %v", names, want)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "broken") {
		t.Errorf("notes %v, want one naming broken", notes)
	}
}

func TestReadRLibraryMissingIsEmpty(t *testing.T) {
	got, notes, err := ReadRLibrary(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 || len(notes) != 0 {
		t.Errorf("got %v %v %v, want nothing", got, notes, err)
	}
}

func TestParseDCFFoldsContinuations(t *testing.T) {
	f := parseDCF([]byte("Package: x\nDescription: one\n    two\n\tthree\nVersion:  2.0 \n"))
	if f["Description"] != "one two three" || f["Version"] != "2.0" {
		t.Errorf("fields %v", f)
	}
}

func TestRLibraryDirs(t *testing.T) {
	rel := t.TempDir()
	for _, s := range []string{"R-4.5", "R-4.4"} {
		if err := os.MkdirAll(filepath.Join(rel, "renv", "library", s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := RLibraryDirs(rel)
	if len(got) != 2 || !strings.HasSuffix(got[0], "R-4.4") {
		t.Errorf("dirs %v", got)
	}
}

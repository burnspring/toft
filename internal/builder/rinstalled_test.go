package builder

import (
	"os"
	"path/filepath"
	"testing"

	"toft/internal/deps"
)

func installR(t *testing.T, lib, name, version string) {
	t.Helper()
	dir := filepath.Join(lib, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Package: " + name + "\nVersion: " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Whose each installed package is decides whose fix a finding is, so the
// classification is the part of the R gate a wrong answer would hide: a
// platform row marked installed tells an owner to update a package they never
// chose; an installed row marked platform tells them nothing they do matters.
func TestInstalledRRowsSaysWhoseEachPackageIs(t *testing.T) {
	rel := t.TempDir()
	lib := filepath.Join(rel, "renv", "library", "R-4.5")

	// What the library held before the app's packages were restored.
	installR(t, lib, "renv", "1.0.7")
	installR(t, lib, "jsonlite", "1.8.9")
	installR(t, lib, "curl", "5.2.3")
	installR(t, lib, "toft", "0.9.6")
	platform := installedNames(lib)

	// The restore: shiny is locked, jsonlite is locked too (the app chose it
	// as well), and httpuv came in with shiny without a lockfile entry.
	installR(t, lib, "shiny", "1.8.1")
	installR(t, lib, "httpuv", "1.6.15")
	lock := `{"R":{"Version":"4.5.1"},"Packages":{"shiny":{"Package":"shiny","Version":"1.8.1"},"jsonlite":{"Package":"jsonlite","Version":"1.8.9"}}}`
	if err := os.WriteFile(filepath.Join(rel, "renv.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, notes := installedRRows(rel, platform)
	if len(notes) != 0 {
		t.Errorf("notes %v", notes)
	}
	want := map[string]string{
		"curl": deps.SourcePlatform, "renv": deps.SourcePlatform, "toft": deps.SourcePlatform,
		"jsonlite": deps.SourceInstalled, "shiny": deps.SourceInstalled, "httpuv": deps.SourceInstalled,
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for _, d := range rows {
		if d.Source != want[d.Package] {
			t.Errorf("%s: %s, want %s", d.Package, d.Source, want[d.Package])
		}
		if d.Ecosystem != "cran" {
			t.Errorf("%s: ecosystem %s", d.Package, d.Ecosystem)
		}
	}
}

// Without a lockfile the build installs only Toft's packages, and every row
// says so.
func TestInstalledRRowsWithNoLockfileIsAllPlatform(t *testing.T) {
	rel := t.TempDir()
	lib := filepath.Join(rel, "renv", "library", "R-4.5")
	installR(t, lib, "renv", "1.0.7")
	installR(t, lib, "curl", "5.2.3")
	rows, _ := installedRRows(rel, installedNames(lib))
	for _, d := range rows {
		if d.Source != deps.SourcePlatform {
			t.Errorf("%s: %s, want platform", d.Package, d.Source)
		}
	}
}

func TestCoveredEcosystemsLeavesCRANOut(t *testing.T) {
	got := coveredEcosystems([]deps.Dependency{
		{Ecosystem: "cran", Package: "x"}, {Ecosystem: "npm", Package: "y"},
	})
	if len(got) != 1 || got[0] != "npm" {
		t.Errorf("covered %v, want [npm]", got)
	}
}

package deps

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"toft/internal/advisory"
)

func writeLock(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sampleLock = `{
  "R": {"Version": "4.5.0"},
  "Packages": {
    "dplyr": {"Package": "dplyr", "Version": "1.1.4", "Source": "Repository", "Repository": "CRAN"},
    "glue":  {"Package": "glue",  "Version": "1.8.0", "Source": "Repository", "Repository": "CRAN"},
    "shiny": {"Package": "shiny", "Version": "1.13.0", "Source": "Repository", "Repository": "CRAN"}
  }
}`

// **R's resolver runs no command**, which is the property worth a test rather
// than a comment: both other ecosystems need one because their manifests are
// not lockfiles, and renv.lock already is one.
//
// Asserted by emptying PATH. If anything in this path ever shells out — to
// Rscript, to renv, to anything — it fails here rather than in an air-gapped
// install months later.
func TestTheRResolverRunsNoCommand(t *testing.T) {
	dir := t.TempDir()
	writeLock(t, dir, sampleLock)

	t.Setenv("PATH", "")

	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("resolving with an empty PATH failed, so something executed: %v", err)
	}
	if len(res.Dependencies) != 3 {
		t.Fatalf("got %d dependencies, want 3", len(res.Dependencies))
	}
	for _, d := range res.Dependencies {
		if d.Ecosystem != advisory.EcosystemCRAN {
			t.Errorf("%s: ecosystem = %q, want cran", d.Package, d.Ecosystem)
		}
		if d.Version == "" {
			t.Errorf("%s: no version recorded", d.Package)
		}
	}
}

// The inventory is recorded; coverage is not claimed. CRAN stays out of
// Ecosystems, so the tab reads "not scanned" rather than a zero that says more
// than OSV's 14 CRAN advisories support.
func TestTheRResolverRecordsWithoutClaimingCoverage(t *testing.T) {
	dir := t.TempDir()
	writeLock(t, dir, sampleLock)

	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dependencies) == 0 {
		t.Fatal("nothing was recorded")
	}
	for _, e := range res.Ecosystems {
		if e == advisory.EcosystemCRAN {
			t.Fatal("the resolver claimed cran coverage. The inventory is recorded and displayed; " +
				"claiming it is scanned would put a permanent zero on every R app")
		}
	}
}

// Keyed on the file, never on the framework — so a repository that is both is
// resolved as both, and the fix for each is a different lockfile and often a
// different person.
func TestARepositoryWithBothLockfilesResolvesBoth(t *testing.T) {
	dir := t.TempDir()
	writeLock(t, dir, sampleLock)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawCRAN bool
	for _, d := range res.Dependencies {
		if d.Ecosystem == advisory.EcosystemCRAN {
			sawCRAN = true
		}
	}
	if !sawCRAN {
		t.Error("the renv.lock was ignored because the repository was also something else")
	}
}

// A malformed lockfile is a refusal, not an empty inventory: reporting no
// dependencies for a file the platform could not read would be a status that is
// a lie, and the deploy is the moment its author can act on it.
func TestAMalformedLockIsRefusedRatherThanRead(t *testing.T) {
	dir := t.TempDir()
	writeLock(t, dir, `{"Packages": [this is not json`)

	if _, err := Resolve(context.Background(), dir, nil); err == nil {
		t.Fatal("a malformed renv.lock resolved cleanly, which would record an empty inventory " +
			"for an app that has dependencies")
	}
}

// A package with no version cannot be matched against anything, so it is
// skipped and said out loud rather than recorded as a row no advisory can apply
// to.
func TestAPackageWithNoVersionIsSkippedAndReported(t *testing.T) {
	dir := t.TempDir()
	writeLock(t, dir, `{"Packages": {
      "good": {"Package": "good", "Version": "1.0"},
      "bad":  {"Package": "bad"}
    }}`)

	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dependencies) != 1 {
		t.Fatalf("got %d dependencies, want 1", len(res.Dependencies))
	}
	if len(res.Notes) == 0 {
		t.Error("a package was dropped with nothing said about it")
	}
}

// renv.lock is read after the app's own code has run in the tree, so a FIFO
// planted at its name must not hold the build worker: the read is bounded and
// non-blocking, and a lock that is not a regular file is no packages (code
// review 2026-11-11, G-5).
func TestRenvLockPackagesDoesNotBlockOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "renv.lock"), 0o644); err != nil {
		t.Skip("no FIFO here:", err)
	}
	done := make(chan map[string]bool, 1)
	go func() { done <- RenvLockPackages(dir) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("a FIFO read as packages: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RenvLockPackages blocked on a FIFO")
	}
}

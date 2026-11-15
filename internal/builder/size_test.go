package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// The number an operator reads is a sum of file contents, not of directory
// entries or of link targets — a release tree is full of both, and counting
// them would inflate every figure the storage view is used to compare.
func TestDirSizeSumsRegularFilesOnly(t *testing.T) {
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.js"), make([]byte, 100), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "pkg", "index.js"), make([]byte, 400), 0644); err != nil {
		t.Fatal(err)
	}
	// A dependency tree links to itself constantly; following this would count
	// server.js twice.
	if err := os.Symlink(filepath.Join(root, "server.js"), filepath.Join(root, "node_modules", "link.js")); err != nil {
		t.Fatal(err)
	}

	got, err := dirSize(root)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if got != 500 {
		t.Errorf("dirSize = %d, want 500", got)
	}
}

// A release that is not on this host is unmeasured, which is not the same as
// measuring zero: an isolated workload's release never lands here, and a
// rollback row's release is named after the deployment it rolled back to.
func TestMeasureReleaseDirIsNilWhenAbsent(t *testing.T) {
	appsDir := t.TempDir()

	if got := measureReleaseDir(appsDir, "app-1", "never-built"); got != nil {
		t.Errorf("measureReleaseDir = %d, want nil", *got)
	}

	dir := ReleaseDir(appsDir, "app-1", "deploy-1")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), make([]byte, 64), 0644); err != nil {
		t.Fatal(err)
	}
	got := measureReleaseDir(appsDir, "app-1", "deploy-1")
	if got == nil {
		t.Fatal("measureReleaseDir = nil for a release that is on disk")
	}
	if *got != 64 {
		t.Errorf("measureReleaseDir = %d, want 64", *got)
	}

	// An empty release measures zero, and zero is a measurement.
	empty := ReleaseDir(appsDir, "app-1", "deploy-2")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	if got := measureReleaseDir(appsDir, "app-1", "deploy-2"); got == nil || *got != 0 {
		t.Errorf("an empty release measured %v, want 0", got)
	}
}

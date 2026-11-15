package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"toft/internal/artifact"
)

// A reader of a release — the archive download — gets it wherever it is: on
// disk, or from the store into scratch that is gone when the reader returns.
func TestWithReleaseFetchesWhatTheDiskNoLongerHas(t *testing.T) {
	store := newStore(t)
	appsDir := t.TempDir()
	b := &Builder{opts: Options{AppsDir: appsDir}, artifacts: store}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.py"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), artifact.ReleaseKey("app", "d1"), src); err != nil {
		t.Fatal(err)
	}

	var seen string
	err := b.WithRelease("app", "d1", func(dir string) error {
		seen = dir
		body, err := os.ReadFile(filepath.Join(dir, "app.py"))
		if err != nil || string(body) != "v1" {
			t.Errorf("fetched tree: %v %q", err, body)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithRelease: %v", err)
	}
	if fileExists(seen) {
		t.Errorf("the scratch copy %s outlived the reader", seen)
	}
	if fileExists(ReleaseDir(appsDir, "app", "d1")) {
		t.Error("a reader's fetch became a release directory")
	}

	// On disk: that directory, as it is.
	onDisk := ReleaseDir(appsDir, "app", "d2")
	if err := os.MkdirAll(onDisk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := b.WithRelease("app", "d2", func(dir string) error {
		if dir != onDisk {
			t.Errorf("dir = %s, want %s", dir, onDisk)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Neither place, and no store at all.
	for name, bb := range map[string]*Builder{"store": b, "no store": {opts: Options{AppsDir: appsDir}}} {
		if err := bb.WithRelease("app", "gone", func(string) error { return nil }); !errors.Is(err, ErrReleaseGone) {
			t.Errorf("%s: err = %v, want ErrReleaseGone", name, err)
		}
	}
}

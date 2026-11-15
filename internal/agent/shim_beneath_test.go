//go:build linux

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A Keep path is resolved beneath its cover: a link planted at an app-writable
// Keep path pointing at / is refused, while the release's own relative
// `current` link still resolves (RT-9). The clone itself needs root and is
// covered by netns_root_test.go; this is the resolution, which does not.
func TestAKeepPathIsResolvedBeneathItsCover(t *testing.T) {
	cover := t.TempDir()
	app := filepath.Join(cover, "app-1")
	if err := os.MkdirAll(filepath.Join(app, "releases", "dep-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/dep-1", filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
	resource := filepath.Join(cover, "resource")
	if err := os.MkdirAll(resource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(resource, "f")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../..", filepath.Join(resource, "up")); err != nil {
		t.Fatal(err)
	}
	covers := []string{"/tmp-not-this-one", cover}

	fd, err := openBeneath(filepath.Join(app, "current"), covers)
	if err != nil {
		t.Fatalf("the release's relative current link was refused: %v", err)
	}
	unix.Close(fd)

	for _, name := range []string{"f", "up"} {
		if fd, err := openBeneath(filepath.Join(resource, name), covers); err == nil {
			unix.Close(fd)
			t.Errorf("a Keep through a link out of its cover (%s) was opened", name)
		}
	}
	if _, err := openBeneath(filepath.Join(resource, "absent"), covers); !errors.Is(err, unix.ENOENT) {
		t.Errorf("an absent Keep = %v, want ENOENT", err)
	}
}

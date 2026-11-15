package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BB-048, fifth 2026-11-03 black-box list: a volume attached whole at
// /storage/v and by folder at /storage/v/f failed every start with "path
// escapes from parent", and detaching the whole mapping did not recover the
// app, because its link stayed where the folder's needed a directory.
func TestAttachNestedMountsRefusedByNameAndAStaleLinkIsCleared(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
	account := &AppAccount{Username: "toft-a", UID: 5000, GID: 6000, GroupGID: 7000, GroupName: "toft-a"}
	whole := Mount{Gate: gateFor("v"), Name: "v", Path: "/storage/v", Kind: MountVolume, Access: AccessFull, Root: "data"}
	folder := Mount{Gate: gateFor("v"), Name: "v", Folder: "f", Path: "/storage/v/f", Kind: MountVolume, Access: AccessFull, Root: "data"}

	if _, err := f.Attach("app-1", account, []Mount{whole}); err != nil {
		t.Fatalf("whole: %v", err)
	}
	_, err := f.Attach("app-1", account, []Mount{whole, folder})
	if err == nil || !strings.Contains(err.Error(), "one inside the other") {
		t.Fatalf("whole and folder = %v, want the nesting named", err)
	}

	// The whole mapping detached: the folder alone starts.
	out, err := f.Attach("app-1", account, []Mount{folder})
	if err != nil {
		t.Fatalf("folder alone after the whole was detached: %v", err)
	}
	link := filepath.Join(appsDir, "app-1", "storage", "storage", "v", "f")
	if out[0].Path != link {
		t.Errorf("path = %q, want %q", out[0].Path, link)
	}
	if target, _ := os.Readlink(link); target != filepath.Join(rootDir, "v", "f") {
		t.Errorf("link target = %q", target)
	}
	parent, err := os.Lstat(filepath.Dir(link))
	if err != nil || parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() {
		t.Errorf("the whole mapping's link is still in the way: %v %v", parent, err)
	}
	// The resource's own files are untouched by clearing the link.
	if _, err := os.Stat(filepath.Join(rootDir, "v")); err != nil {
		t.Errorf("the resource directory went with the link: %v", err)
	}
}

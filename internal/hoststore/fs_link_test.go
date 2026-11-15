package hoststore

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A link on the way to a new folder is refused, never followed into a
// sibling folder (security review 2026-11-10, C-2); a real path is made.
func TestDescendRefusesALinkComponent(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "a"), 0o755)
	os.MkdirAll(filepath.Join(base, "b"), 0o755)
	if err := os.Symlink("../b", filepath.Join(base, "a", "into-b")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, _, err := descend(root, "a/into-b/new", true, 0); !errors.Is(err, errLinkedFolder) {
		t.Fatalf("descend through a link = %v, want errLinkedFolder", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "b", "new")); err == nil {
		t.Fatal("a folder was made in the sibling through the link")
	}
	folder, existed, err := descend(root, "a/real/new", true, 0)
	if err != nil || existed {
		t.Fatalf("a real path: %v (existed %v)", err, existed)
	}
	folder.Close()
	if info, err := os.Stat(filepath.Join(base, "a", "real", "new")); err != nil || info.Mode().Perm() != gatedFolderMode.Perm() {
		t.Fatalf("the new folder: %v %v", info, err)
	}
}

// linkedRig is a whole-resource grant over two gates, X and Y, with Y holding
// a file — the deception AS-13 closes: app code on X links or swaps a folder
// of its own for ../Y, and a person whose grant covers both acts on Y while
// looking at X.
func linkedRig(t *testing.T) *rig {
	t.Helper()
	g := newRig(t)
	if rec := g.put("", "Y/f", []byte("y's"), true); rec.Code != http.StatusOK {
		t.Fatalf("seeding Y: %d %s", rec.Code, rec.Body.String())
	}
	if rec := g.put("", "X/keep", []byte("x's"), true); rec.Code != http.StatusOK {
		t.Fatalf("seeding X: %d %s", rec.Code, rec.Body.String())
	}
	return g
}

// A folder swapped for a link to a sibling between the descent and the
// upload's landing cannot move the upload into the sibling (AS-13).
func TestAPutWhoseFolderIsSwappedForALinkDoesNotLandInTheSibling(t *testing.T) {
	g := linkedRig(t)
	sub := filepath.Join(g.dir, "X", "sub")
	if err := os.MkdirAll(sub, 0o770); err != nil {
		t.Fatal(err)
	}
	beforeLanding = func() {
		if err := os.Rename(sub, sub+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../Y", sub); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { beforeLanding = nil }()

	g.put("", "X/sub/new", []byte("upload"), false)
	if _, err := os.Lstat(filepath.Join(g.dir, "Y", "new")); err == nil {
		t.Fatal("the upload landed in the sibling folder through the swapped link")
	}
	entries, _ := os.ReadDir(filepath.Join(g.dir, "Y"))
	if len(entries) != 1 {
		t.Errorf("Y holds %d entries, want only its own f", len(entries))
	}
}

// With `X/sub -> ../Y` in place: a delete of X/sub/f leaves Y/f, a GET of it
// is refused, a listing of X shows no sub, and a PUT through it is refused
// (AS-13).
func TestALinkBelowTheScopeIsNeverFollowed(t *testing.T) {
	g := linkedRig(t)
	if err := os.Symlink("../Y", filepath.Join(g.dir, "X", "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Y/f", filepath.Join(g.dir, "X", "file-link")); err != nil {
		t.Fatal(err)
	}
	signed, _ := g.signer("").PresignDelete(context.Background(), "", "X/sub/f", time.Hour)
	g.do(signed, nil)
	if _, err := os.Stat(filepath.Join(g.dir, "Y", "f")); err != nil {
		t.Fatalf("a delete through the link removed Y/f: %v", err)
	}
	if rec := g.get("", "X/sub/f"); rec.Code == http.StatusOK {
		t.Errorf("a GET through the link = 200 %q", rec.Body.String())
	}
	if rec := g.get("", "X/file-link"); rec.Code == http.StatusOK {
		t.Errorf("a GET of a link to a file = 200 %q", rec.Body.String())
	}
	if rec := g.put("", "X/sub/planted", []byte("x"), true); rec.Code == http.StatusOK {
		t.Error("a PUT through the link was accepted")
	}
	if _, err := os.Lstat(filepath.Join(g.dir, "Y", "planted")); err == nil {
		t.Error("a PUT through the link landed in Y")
	}
	for _, delim := range []string{"/", ""} {
		page, err := g.signer("").ListObjects(context.Background(), "", "X/", "", delim, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(page); strings.Contains(got, "sub") || strings.Contains(got, "file-link") || !strings.Contains(got, "keep") {
			t.Errorf("listing of X (delimiter %q) = %q", delim, got)
		}
		page, _ = g.signer("").ListObjects(context.Background(), "", "X/sub/", "", delim, 100)
		if len(page.Objects)+len(page.Prefixes) != 0 {
			t.Errorf("listing through the link (delimiter %q) = %q", delim, names(page))
		}
	}
	if ok, _ := g.signer("").HasObject(context.Background(), "", "X/sub/f"); ok {
		t.Error("HasObject followed the link")
	}
	// The link itself can still be deleted, and its target stays.
	signed, _ = g.signer("").PresignDelete(context.Background(), "", "X/file-link", time.Hour)
	g.do(signed, nil)
	if _, err := os.Lstat(filepath.Join(g.dir, "X", "file-link")); err == nil {
		t.Error("the link itself could not be deleted")
	}
	if _, err := os.Stat(filepath.Join(g.dir, "Y", "f")); err != nil {
		t.Error("deleting the link removed its target")
	}
}

package staticserve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The resolver and the lint read the release as the platform, over a tree the
// repository supplied. Nothing outside the release may be read through a link
// in it (KUBE-8).

// A `*.html` linked to a file outside the release prints nothing of it into the
// build log, and neither does a directory linked out.
func TestLintDoesNotReadThroughALinkOutOfTheRelease(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "secret.html", `<a href="/host-secret">x</a>`)
	dir := t.TempDir()
	write(t, dir, "index.html", `<p>clean</p>`)
	if err := os.Symlink(filepath.Join(outside, "secret.html"), filepath.Join(dir, "peek.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "docs")); err != nil {
		t.Fatal(err)
	}
	if got := Lint(dir, "."); got != nil {
		t.Errorf("Lint = %v, want nothing read through a link", got)
	}
}

// A declared root that is a link is refused by name rather than walked: with
// `dist -> /etc` the resolver counted, and the lint walked, a host tree.
func TestDeclaredRootThatIsALinkIsRefused(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "index.html", `<a href="/host-secret">x</a>`)
	dir := t.TempDir()
	write(t, dir, ManifestFile, "[static]\nroot = \"dist\"\n")
	if err := os.Symlink(outside, filepath.Join(dir, "dist")); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(dir)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Resolve = %v, want a refusal naming the link", err)
	}
	if got := Lint(dir, "dist"); got != nil {
		t.Errorf("Lint = %v, want nothing read through the linked root", got)
	}
}

// The manifest is read through the release's root too.
func TestDeclarationIsNotReadThroughALinkOutOfTheRelease(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "toft.toml", "[static]\nentry = \"x.html\"\n")
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "toft.toml"), filepath.Join(dir, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if d, err := ReadDeclaration(dir); err == nil && d.Present {
		t.Fatalf("ReadDeclaration read %+v through a link out of the release", d)
	}
}

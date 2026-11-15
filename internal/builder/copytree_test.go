package builder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreeSkipsBuildArtifacts(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "release")

	write(t, src, "app.py", "print('hi')\n")
	write(t, src, "requirements.txt", "flask\n")
	mkdir(t, src, "templates")
	write(t, filepath.Join(src, "templates"), "index.html", "<h1>hi</h1>")

	// A venv holds absolute paths to the release it was built in; carrying it
	// over would leave the new release pointing at the old one.
	mkdir(t, src, "venv")
	write(t, filepath.Join(src, "venv"), "pyvenv.cfg", "home = /old/release")
	mkdir(t, src, "node_modules")
	write(t, filepath.Join(src, "node_modules"), "junk.js", "//")

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	for _, want := range []string{"app.py", "requirements.txt", "templates/index.html"} {
		if !fileExists(filepath.Join(dst, want)) {
			t.Errorf("%s was not copied", want)
		}
	}
	for _, unwanted := range []string{"venv", "node_modules"} {
		if fileExists(filepath.Join(dst, unwanted)) {
			t.Errorf("%s should not have been copied", unwanted)
		}
	}

	if got := read(t, dst, "app.py"); got != "print('hi')\n" {
		t.Errorf("content = %q", got)
	}
}

// The builder passes the `current` symlink, so copyTree must descend through it
// rather than stopping at the link.
func TestCopyTreeResolvesSymlinkedSource(t *testing.T) {
	base := t.TempDir()
	release := filepath.Join(base, "releases", "abc")
	if err := os.MkdirAll(release, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, release, "app.py", "x = 1\n")

	link := filepath.Join(base, "current")
	if err := os.Symlink(release, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	dst := filepath.Join(base, "releases", "def")
	if err := copyTree(link, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	if !fileExists(filepath.Join(dst, "app.py")) {
		t.Error("copyTree did not follow the current symlink")
	}
}

// The release being copied is the app's own and it may plant links in it. No
// link is reproduced, and nothing outside the tree is ever read through one —
// a file, or a directory the walk might descend into.
func TestCopyTreeNeverReadsThroughALink(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "secret", "root's\n")

	src := t.TempDir()
	write(t, src, "app.py", "x = 1\n")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(src, "stolen.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "elsewhere")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "release")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if !fileExists(filepath.Join(dst, "app.py")) {
		t.Error("the app's own file was not copied")
	}
	for _, unwanted := range []string{"stolen.txt", "elsewhere", "elsewhere/secret"} {
		if _, err := os.Lstat(filepath.Join(dst, unwanted)); err == nil {
			t.Errorf("%s reached the new release", unwanted)
		}
	}
}

func TestCopyTreeMissingSourceIsAnError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "release")

	if err := copyTree(filepath.Join(t.TempDir(), "nope"), dst); err == nil {
		t.Error("expected an error for a missing source")
	}
}

func TestIsUploadedSource(t *testing.T) {
	if !IsUploadedSource("upload://bundle.zip") {
		t.Error("upload:// should be recognised")
	}
	if IsUploadedSource("https://github.com/acme/tool") {
		t.Error("an https repo is not an upload")
	}
}

func mkdir(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
}

// The regression that stranded an uploaded app: the staging tree was *moved*
// into the release, so a deploy that failed after the fetch step took the only
// copy of the source with it. Every retry then reported "No source available"
// — the app was undeployable without uploading the archive a second time —
// which is a failed start deciding that the source no longer exists.
func TestStagingSurvivesTheDeployThatUsesIt(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "upload-staging")
	release := filepath.Join(t.TempDir(), "releases", "deploy-1")

	mkdir(t, filepath.Dir(staging), "upload-staging")
	write(t, staging, "index.html", "<h1>catalog</h1>")

	if err := stageUploadedSource(staging, release); err != nil {
		t.Fatalf("stageUploadedSource: %v", err)
	}
	if !fileExists(filepath.Join(release, "index.html")) {
		t.Error("the release was not populated from the staging tree")
	}
	if !fileExists(filepath.Join(staging, "index.html")) {
		t.Error("the staging tree was consumed — a deploy that fails after this " +
			"point leaves the app with no source and no way to redeploy")
	}

	// And a second attempt, as a retry would, still finds it.
	again := filepath.Join(t.TempDir(), "releases", "deploy-2")
	if err := stageUploadedSource(staging, again); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if !fileExists(filepath.Join(again, "index.html")) {
		t.Error("the retry could not populate a release from the same upload")
	}
}

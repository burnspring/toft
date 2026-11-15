package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSubdir(t *testing.T) {
	for s, ok := range map[string]bool{
		"":                              true,
		"extensions/request-desk":       true,
		"examples/flask-hello":          true,
		"app":                           true,
		"a.b/c_d/e-f":                   true,
		"../elsewhere":                  false,
		"apps/../../etc":                false,
		"./apps":                        false,
		"apps/.":                        false,
		"/etc":                          false,
		"apps//web":                     false,
		".git":                          false,
		"apps/.GIT/hooks":               false,
		"apps\\web":                     false,
		"-rf":                           false,
		"apps/-x":                       false,
		"apps/web space":                false,
		strings.Repeat("a/", 128) + "b": false,
	} {
		if err := ValidateSubdir(s); (err == nil) != ok {
			t.Errorf("ValidateSubdir(%q) = %v, want ok=%v", s, err, ok)
		}
	}
}

func TestNormalizeSubdirIsWhatValidateSubdirIsHandedFromAPerson(t *testing.T) {
	for in, want := range map[string]string{
		" apps/web/ ": "apps/web",
		"/apps/web":   "apps/web",
		"":            "",
		"/":           "",
	} {
		if got := NormalizeSubdir(in); got != want {
			t.Errorf("NormalizeSubdir(%q) = %q, want %q", in, got, want)
		}
	}
}

// gitRepo makes a committed repository holding files, and returns its path.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "x"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// The failure this guards is silent: if the commit were read from the release
// after the move, the folder has no .git, the deploy records no SHA, and the
// poller rebuilds the same commit on every pass without an error anywhere.
func TestASubdirReleaseIsTheFolderAloneAndKeepsTheCloneCommit(t *testing.T) {
	clone := gitRepo(t, map[string]string{
		"catalog.json":              "{}",
		"extensions/desk/toft.toml": "[app]\n",
		"extensions/desk/main.py":   "print('desk')\n",
		"extensions/other/main.py":  "print('other')\n",
	})
	sha, err := (&Builder{}).getCommitSHA(clone)
	if err != nil || len(sha) != 40 {
		t.Fatalf("commit of the clone: %q, %v", sha, err)
	}

	release := filepath.Join(t.TempDir(), "releases", "d1")
	if err := PlaceSubdir(clone, "extensions/desk", release); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(release, "toft.toml")); err != nil {
		t.Errorf("the folder's manifest is not at the release root: %v", err)
	}
	for _, absent := range []string{".git", "catalog.json", "extensions", "other"} {
		if _, err := os.Stat(filepath.Join(release, absent)); err == nil {
			t.Errorf("%s from outside the folder reached the release", absent)
		}
	}
	if _, err := (&Builder{}).getCommitSHA(release); err == nil {
		t.Error("the release answers a commit of its own; the clone's is the one to record")
	}
}

func TestPlaceSubdirRefusesWhatIsNotAFolderOfTheClone(t *testing.T) {
	clone := gitRepo(t, map[string]string{
		"apps/web/index.html": "<p>hi</p>",
		"apps/file":           "not a folder",
	})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(clone, "apps", "escape")); err != nil {
		t.Fatal(err)
	}
	for subdir, want := range map[string]string{
		"apps/missing": "is not in the repository",
		"apps/file":    "not a folder",
		"apps/escape":  "symlink",
	} {
		release := filepath.Join(t.TempDir(), "r")
		err := PlaceSubdir(clone, subdir, release)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("PlaceSubdir(%q) = %v, want an error containing %q", subdir, err, want)
		}
		if _, statErr := os.Stat(release); statErr == nil {
			t.Errorf("PlaceSubdir(%q) left a release behind after refusing", subdir)
		}
	}
}

// A `.git` *file* is not a checkout: only an uploaded archive could have put one
// there, naming a git directory the uploader wrote (security review
// 2026-11-14, CS-7).
func TestAGitFileIsNotACheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"g/objects", "g/refs/heads"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "g", "HEAD"), []byte("0123456789abcdef0123456789abcdef01234567\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sha, err := (&Builder{}).getCommitSHA(dir); err == nil {
		t.Errorf("a .git file was read as a checkout, commit %q", sha)
	}
}

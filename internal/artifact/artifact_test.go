package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTree materialises files (path -> contents) under a fresh directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			out[filepath.ToSlash(rel)] = "-> " + target
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- Round trip ---

func TestLocalStoreRoundTrip(t *testing.T) {
	src := writeTree(t, map[string]string{
		"app.py":                  "print('hi')",
		"requirements.txt":        "flask\n",
		"venv/lib/site/mod.py":    "x = 1",
		"static/nested/deep/a.js": "console.log(1)",
	})

	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")

	desc, err := store.Put(ctx, key, src)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if desc.Size == 0 || len(desc.SHA256) != 64 {
		t.Fatalf("descriptor looks wrong: %+v", desc)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("get: %v", err)
	}

	want, got := readTree(t, src), readTree(t, dest)
	if len(want) != len(got) {
		t.Fatalf("restored %d files, packed %d\n want %v\n  got %v", len(got), len(want), want, got)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s = %q, want %q", name, got[name], body)
		}
	}
}

// Executable bits survive: a Go app's compiled binary is useless without them,
// and so is every script in a venv's bin directory.
func TestExecutableBitSurvives(t *testing.T) {
	src := t.TempDir()
	bin := filepath.Join(src, "toft-app")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}

	store, _ := NewLocalStore(t.TempDir())
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")
	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dest, "toft-app"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0111 == 0 {
		t.Errorf("mode = %v, executable bit lost", fi.Mode().Perm())
	}
}

// A venv is mostly symlinks. Storing them as links rather than following them
// is what keeps the artifact from exploding in size.
func TestSymlinksInsideTheTreeSurviveAsLinks(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "venv/bin"), 0755)
	os.WriteFile(filepath.Join(src, "venv/bin/python3.12"), []byte("binary"), 0755)
	if err := os.Symlink("python3.12", filepath.Join(src, "venv/bin/python")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	store, _ := NewLocalStore(t.TempDir())
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")
	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dest, "venv/bin/python"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was materialised as a regular file")
	}
}

// --- Failure modes ---

func TestGetMissingKeyIsNotFound(t *testing.T) {
	store, _ := NewLocalStore(t.TempDir())
	err := store.Get(context.Background(), ReleaseKey("app-1", "nope"), filepath.Join(t.TempDir(), "d"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A corrupt artifact must be refused, and — the part that matters — must leave
// the destination untouched, so nothing is ever started from a half-tree.
func TestCorruptArtifactIsRefusedAndLeavesDestinationAlone(t *testing.T) {
	src := writeTree(t, map[string]string{"app.py": "print('new')"})
	root := t.TempDir()
	store, _ := NewLocalStore(root)
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")

	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}

	// Flip a byte deep in the payload, past the gzip header.
	p := filepath.Join(root, key)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}

	// A destination that already holds the previous, working release.
	dest := filepath.Join(t.TempDir(), "current")
	os.MkdirAll(dest, 0755)
	os.WriteFile(filepath.Join(dest, "app.py"), []byte("print('old')"), 0644)

	err = store.Get(ctx, key, dest)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}

	body, err := os.ReadFile(filepath.Join(dest, "app.py"))
	if err != nil {
		t.Fatalf("the existing release was destroyed by a failed restore: %v", err)
	}
	if string(body) != "print('old')" {
		t.Errorf("existing release was overwritten: %q", body)
	}
}

// A truncated transfer must not read as a successful restore of a smaller tree.
func TestTruncatedArtifactIsRefused(t *testing.T) {
	src := writeTree(t, map[string]string{
		"a.py": strings.Repeat("x", 200000),
		"b.py": strings.Repeat("y", 200000),
	})
	root := t.TempDir()
	store, _ := NewLocalStore(root)
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")

	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, key)
	data, _ := os.ReadFile(p)
	os.WriteFile(p, data[:len(data)/2], 0600)

	err := store.Get(ctx, key, filepath.Join(t.TempDir(), "d"))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
}

// --- Hostile archives ---

// tarball builds an archive by hand so a test can put things in it that pack()
// would never produce.
func tarball(t *testing.T, entries []*tar.Header, bodies []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if i < len(bodies) && bodies[i] != "" {
			if _, err := tw.Write([]byte(bodies[i])); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestUnpackRefusesPathTraversal(t *testing.T) {
	cases := []string{
		"../escaped.txt",
		"nested/../../escaped.txt",
		"/etc/cron.d/evil",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			data := tarball(t,
				[]*tar.Header{{Name: name, Mode: 0644, Size: 4, Typeflag: tar.TypeReg}},
				[]string{"evil"})

			dest := filepath.Join(t.TempDir(), "release")
			os.MkdirAll(dest, 0755)
			err := unpack(bytes.NewReader(data), dest)
			if !errors.Is(err, ErrIntegrity) {
				t.Fatalf("err = %v, want ErrIntegrity", err)
			}
		})
	}
}

// A symlink out of the tree is restored, because a Python virtual environment
// cannot be shipped without one: `uv venv` writes
// `venv/bin/python -> /usr/bin/python3`. Refusing it refused every Python
// release on the way back out of the store.
func TestUnpackRestoresASymlinkOutOfTheTree(t *testing.T) {
	cases := []struct{ name, target string }{
		{"venv/bin/python", "/usr/bin/python3"},
		{"link", "../../../etc/passwd"},
		{"deep/nested/link", "../../../../outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"->"+tc.target, func(t *testing.T) {
			data := tarball(t,
				[]*tar.Header{{Name: tc.name, Linkname: tc.target, Mode: 0777, Typeflag: tar.TypeSymlink}},
				nil)

			dest := filepath.Join(t.TempDir(), "release")
			os.MkdirAll(dest, 0755)
			if err := unpack(bytes.NewReader(data), dest); err != nil {
				t.Fatalf("err = %v, want the link restored", err)
			}
			if got, err := os.Readlink(filepath.Join(dest, tc.name)); err != nil || got != tc.target {
				t.Errorf("readlink = %q, %v — want %q", got, err, tc.target)
			}
		})
	}
}

// The half that actually escapes: a link, and then something written through
// it. filepath.Join cannot see this, because it resolves "etc/passwd" without
// knowing "etc" is a link to /etc — so refusing the *write* is what closes it.
func TestUnpackRefusesAWriteThroughAnEscapingSymlink(t *testing.T) {
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "passwd"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		headers []*tar.Header
		bodies  []string
	}{
		{
			"through a linked directory",
			[]*tar.Header{
				{Name: "etc", Linkname: victim, Mode: 0777, Typeflag: tar.TypeSymlink},
				{Name: "etc/passwd", Mode: 0644, Size: 4, Typeflag: tar.TypeReg},
			},
			[]string{"", "evil"},
		},
		{
			// The last component, which the ancestor check cannot see:
			// O_NOFOLLOW is what refuses this one.
			"over the link itself",
			[]*tar.Header{
				{Name: "passwd", Linkname: filepath.Join(victim, "passwd"), Mode: 0777, Typeflag: tar.TypeSymlink},
				{Name: "passwd", Mode: 0644, Size: 4, Typeflag: tar.TypeReg},
			},
			[]string{"", "evil"},
		},
		{
			// Every hop stays inside the tree except the last, so no single link
			// looks like it escapes. Only resolving the whole chain sees it.
			"through a chain of links that each look local",
			[]*tar.Header{
				{Name: "a", Linkname: "b", Mode: 0777, Typeflag: tar.TypeSymlink},
				{Name: "b", Linkname: victim, Mode: 0777, Typeflag: tar.TypeSymlink},
				{Name: "a/passwd", Mode: 0644, Size: 4, Typeflag: tar.TypeReg},
			},
			[]string{"", "", "evil"},
		},
		{
			// A hardlink pulls the outside inode *in*, and O_NOFOLLOW cannot
			// refuse the write that follows because a hardlink is not a symlink.
			"through a hardlink taken past a linked directory",
			[]*tar.Header{
				{Name: "etc", Linkname: victim, Mode: 0777, Typeflag: tar.TypeSymlink},
				{Name: "grab", Linkname: "etc/passwd", Mode: 0644, Typeflag: tar.TypeLink},
				{Name: "grab", Mode: 0644, Size: 4, Typeflag: tar.TypeReg},
			},
			[]string{"", "", "evil"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tarball(t, tc.headers, tc.bodies)

			dest := filepath.Join(t.TempDir(), "release")
			os.MkdirAll(dest, 0755)
			if err := unpack(bytes.NewReader(data), dest); err == nil {
				t.Error("the write through the symlink was accepted")
			}
			// The refusal is only worth anything if the file outside is intact.
			body, err := os.ReadFile(filepath.Join(victim, "passwd"))
			if err != nil || string(body) != "original" {
				t.Fatalf("the file outside the release was modified: %q, %v", body, err)
			}
		})
	}
}

// A read-only directory must not stop its own contents being written.
func TestReadOnlyDirectoryIsRestoredWithItsContents(t *testing.T) {
	src := t.TempDir()
	sub := filepath.Join(src, "locked")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("data"), 0644)
	os.Chmod(sub, 0500)
	defer os.Chmod(sub, 0755)

	store, _ := NewLocalStore(t.TempDir())
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")
	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("get: %v", err)
	}
	defer os.Chmod(filepath.Join(dest, "locked"), 0755)

	body, err := os.ReadFile(filepath.Join(dest, "locked/inner.txt"))
	if err != nil || string(body) != "data" {
		t.Errorf("contents of a 0500 directory were lost: %v %q", err, body)
	}
}

// --- Expiry ---

// Expire takes the keep set rather than an age, so that pruning in the store
// matches the on-disk prune exactly instead of drifting from it.
func TestExpireKeepsExactlyTheNamedReleases(t *testing.T) {
	src := writeTree(t, map[string]string{"app.py": "x"})
	store, _ := NewLocalStore(t.TempDir())
	ctx := context.Background()

	var keys []string
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		k := ReleaseKey("app-1", id)
		if _, err := store.Put(ctx, k, src); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	// Another app's artifacts must be untouched.
	otherKey := ReleaseKey("app-2", "d1")
	if _, err := store.Put(ctx, otherKey, src); err != nil {
		t.Fatal(err)
	}

	keep := keys[2:] // d3, d4
	removed, err := store.Expire(ctx, "app-1", keep)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}

	for _, k := range keep {
		if _, err := store.Stat(ctx, k); err != nil {
			t.Errorf("kept release %s was removed: %v", k, err)
		}
	}
	for _, k := range keys[:2] {
		if _, err := store.Stat(ctx, k); !errors.Is(err, ErrNotFound) {
			t.Errorf("release %s should be gone, got %v", k, err)
		}
	}
	if _, err := store.Stat(ctx, otherKey); err != nil {
		t.Errorf("another app's release was expired: %v", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	store, _ := NewLocalStore(t.TempDir())
	ctx := context.Background()
	if err := store.Delete(ctx, ReleaseKey("app-1", "never-existed")); err != nil {
		t.Errorf("deleting an absent key must not error: %v", err)
	}
}

func TestExpireOnAnAppWithNoArtifacts(t *testing.T) {
	store, _ := NewLocalStore(t.TempDir())
	n, err := store.Expire(context.Background(), "app-unknown", nil)
	if err != nil || n != 0 {
		t.Errorf("got (%d, %v), want (0, nil)", n, err)
	}
}

func TestReleaseKeyIsAppScoped(t *testing.T) {
	k := ReleaseKey("app-1", "deploy-1")
	if !strings.HasPrefix(k, releasePrefix("app-1")) {
		t.Errorf("%q is not under its app's prefix %q", k, releasePrefix("app-1"))
	}
	if strings.HasPrefix(k, releasePrefix("app-2")) {
		t.Error("app scoping is not doing anything")
	}
}

func TestLocalStoreSatisfiesTheInterface(t *testing.T) {
	var _ Store = (*LocalStore)(nil)
	var _ Store = (*S3Store)(nil)
}

// A cancelled context stops a pack between entries and says how far it got --
// the publish that left a deploy "building" for twenty minutes had no way to
// stop and nothing to say.
func TestPackStopsOnItsContextAndSaysWhere(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	ctx = WithProgress(ctx, func(entries int, _ int64, _ string) {
		seen = entries
		if entries == 2 {
			cancel()
		}
	})
	err := pack(ctx, dir, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pack ran on after its context ended: %v", err)
	}
	if seen != 2 || !strings.Contains(err.Error(), "after 2 entries") {
		t.Errorf("the refusal does not say where it stopped (seen %d): %v", seen, err)
	}
}

// Expire leaves a concurrent Put's temp file alone — removing it failed that
// build's rename — and removes one a crash left behind (AS-9).
func TestExpireKeepsAFreshPartialAndRemovesAStaleOne(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "releases", "app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh, stale := filepath.Join(dir, partialPrefix+"fresh"), filepath.Join(dir, partialPrefix+"stale")
	for _, p := range []string{fresh, stale} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * partialGrace)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Expire(context.Background(), "app", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh partial was removed: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a stale partial survived: %v", err)
	}
}

// A directory entry and then a symlink of the same name: the link would
// replace the empty directory, and the closing chmod follow it to the outside
// file with the directory's mode. Refused, and the outside file's mode stays
// (AS-12).
func TestUnpackRefusesASymlinkReplacingADirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeLink} {
		link := outside
		if typ == tar.TypeLink {
			link = "f"
		}
		data := tarball(t, []*tar.Header{
			{Name: "f", Mode: 0o600, Typeflag: tar.TypeReg},
			{Name: "d", Mode: 0o777, Typeflag: tar.TypeDir},
			{Name: "d", Linkname: link, Mode: 0o777, Typeflag: typ},
		}, []string{"", "", ""})
		dest := filepath.Join(t.TempDir(), "release")
		os.MkdirAll(dest, 0o755)
		if err := unpack(bytes.NewReader(data), dest); !errors.Is(err, ErrIntegrity) {
			t.Errorf("type %c: unpack = %v, want ErrIntegrity", typ, err)
		}
		if info, _ := os.Stat(outside); info.Mode().Perm() != 0o600 {
			t.Errorf("type %c: the outside file's mode is now %v", typ, info.Mode().Perm())
		}
	}
}

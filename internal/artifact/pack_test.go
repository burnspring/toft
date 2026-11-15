package artifact

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packedBytes is everything pack wrote, decompressed as far as it goes — a pack
// that stopped part way still closes its gzip stream, and a leak is a leak
// whether or not the publish then failed.
func packedBytes(t *testing.T, ctx context.Context, dir string) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	packErr := pack(ctx, dir, &buf)
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	raw, _ := io.ReadAll(zr)
	return raw, packErr
}

// The tree is the app account's while it is packed, so the app can swap an
// entry between the walk listing it and the walk reading it. Swapped here at
// the exact moment, through the progress callback: a directory for a link to a
// tree outside, and a file for a link to a file outside. Neither target's bytes
// may reach the artifact (KUBE-2). The outside names and sizes match the
// inside ones, so a walk that listed or sized the entry before the swap has
// nothing to trip over.
func TestPackDoesNotFollowAnEntrySwappedForALinkMidWalk(t *testing.T) {
	const secret, mine = "host-secret-bytes", "mine-mine-mine-mi"
	outside := writeTree(t, map[string]string{"keep.txt": secret})
	for _, tc := range []struct{ name, entry, target string }{
		{"directory", "data", outside},
		{"file", "page.txt", filepath.Join(outside, "keep.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"data/keep.txt": mine, "page.txt": mine})
			swapped := false
			swap := func(_ int, _ int64, rel string) {
				if rel != tc.entry {
					return
				}
				p := filepath.Join(dir, tc.entry)
				if err := os.RemoveAll(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(tc.target, p); err != nil {
					t.Fatal(err)
				}
				swapped = true
			}
			raw, _ := packedBytes(t, WithProgress(context.Background(), swap), dir)
			if !swapped {
				t.Fatal("the walk never reached the entry to swap")
			}
			if bytes.Contains(raw, []byte(secret)) {
				t.Error("the artifact carries the bytes of a file outside the tree")
			}
		})
	}
}

// A link that leaves the tree is stored as the link, never as what it names.
func TestPackStoresALinkOutOfTheTreeAsALink(t *testing.T) {
	outside := writeTree(t, map[string]string{"secret.txt": "host-secret-bytes"})
	dir := writeTree(t, map[string]string{"app.py": "print()"})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "peek.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "etc")); err != nil {
		t.Fatal(err)
	}
	raw, err := packedBytes(t, context.Background(), dir)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if bytes.Contains(raw, []byte("host-secret-bytes")) {
		t.Error("the artifact carries the bytes of a file outside the tree")
	}
	if !bytes.Contains(raw, []byte("print()")) {
		t.Error("the tree's own file is missing")
	}
}

// A tree past the unpack limit is refused at pack time, naming the limit,
// rather than published and then never restorable (AS-10).
func TestPackPastTheUnpackLimitFailsNamingIt(t *testing.T) {
	was := packLimit
	packLimit = 1 << 20
	t.Cleanup(func() { packLimit = was })

	dir := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(600 << 10); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	err := pack(context.Background(), dir, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "1048576 bytes limit") {
		t.Fatalf("pack = %v, want a refusal naming the limit", err)
	}
}

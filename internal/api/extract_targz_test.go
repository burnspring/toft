package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is one member of a test tarball. Ordered, unlike the map the ZIP
// helper takes, because a tar is a stream and order is part of what is being
// asserted — a directory entry arriving before or after its children changes
// nothing, and that is worth being able to write down.
type tarEntry struct {
	name string
	body string
	dir  bool
	link string // non-empty for a symlink, which the unpacker drops
}

func tarGzOf(t *testing.T, entries []tarEntry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	w := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}
		switch {
		case e.dir:
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		case e.link != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, e.link, 0
		default:
			hdr.Typeflag = tar.TypeReg
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar write %s: %v", e.name, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

func untarInto(t *testing.T, r *bytes.Reader, filename string) (string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release")
	_, err := extractArchive(r, r.Size(), dir, filename)
	return dir, err
}

// The point of the whole change: `tar czf app.tar.gz my-app/` deploys, and it
// lands exactly where the same folder zipped would have — wrapper stripped,
// files at the release root.
func TestExtractTarGzExtractsAnOrdinaryArchive(t *testing.T) {
	for _, name := range []string{"app.tar.gz", "app.tgz", "APP.TAR.GZ"} {
		t.Run(name, func(t *testing.T) {
			dir, err := untarInto(t, tarGzOf(t, []tarEntry{
				{name: "my-app/", dir: true},
				{name: "my-app/app.py", body: "print('hi')\n"},
				{name: "my-app/requirements.txt", body: "flask\n"},
			}), name)
			if err != nil {
				t.Fatalf("extractArchive: %v", err)
			}
			present(t, dir, "app.py")
			present(t, dir, "requirements.txt")
			absent(t, dir, "my-app")
		})
	}
}

// `tar czf ../app.tar.gz .` from inside the directory names every entry `./x`.
// Left alone, the prefix detector reads `.` as the wrapper folder that every
// entry shares and strips it, which would put the whole tree one level up.
func TestExtractTarGzHandlesDotSlashNames(t *testing.T) {
	dir, err := untarInto(t, tarGzOf(t, []tarEntry{
		{name: "./", dir: true},
		{name: "./index.html", body: "<h1>hi</h1>"},
		{name: "./css/", dir: true},
		{name: "./css/site.css", body: "body{}"},
	}), "site.tar.gz")
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	present(t, dir, "index.html")
	present(t, dir, "css/site.css")
}

// The filter is a property of the unpacker, not of the container: a tarball
// carrying a dependency tree loses it and is told so, exactly as a ZIP is
// (ADR §60).
func TestExtractTarGzRemovesDependencyTrees(t *testing.T) {
	r := tarGzOf(t, []tarEntry{
		{name: "package.json", body: "{}"},
		{name: "server.js", body: "listen()"},
		{name: "node_modules/", dir: true},
		{name: "node_modules/left-pad/i.js", body: "module.exports=1"},
		{name: ".git/config", body: "[remote]"},
		{name: ".DS_Store", body: "junk"},
	})
	dir := filepath.Join(t.TempDir(), "release")
	removed, err := extractArchive(r, r.Size(), dir, "app.tar.gz")
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	present(t, dir, "package.json")
	present(t, dir, "server.js")
	absent(t, dir, "node_modules")
	absent(t, dir, ".git")
	absent(t, dir, ".DS_Store")

	names := map[string]int{}
	for _, e := range removed {
		names[e.Path] = e.Files
	}
	if names["node_modules"] != 1 {
		t.Errorf("node_modules: %d files, want 1 (the directory entry is not a file)", names["node_modules"])
	}
	if _, ok := names[".git"]; !ok {
		t.Errorf("removed = %+v, want .git named", removed)
	}
}

// A tar can name a path outside the destination, and unlike a ZIP it can also
// carry a symlink pointing at one. The first is refused by the same guard the
// ZIP path uses; the second is dropped, because a release tree is source and
// the one legitimate tree of links is built on this side rather than uploaded.
func TestExtractTarGzRefusesTraversalAndDropsLinks(t *testing.T) {
	dir, err := untarInto(t, tarGzOf(t, []tarEntry{
		{name: "../escape.txt", body: "nope"},
		{name: "etc", link: "/etc"},
		{name: "app.py", body: "ok"},
	}), "app.tar.gz")
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	present(t, dir, "app.py")
	absent(t, dir, "etc")
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Error("a ../ entry escaped the destination")
	}
}

// The caps are counted over what the filter left, in both formats. Asserted
// here because a tar has no central directory: the sizes come from the stream,
// and the byte cap is counted as bytes are written rather than believed from a
// header.
func TestExtractTarGzEnforcesTheCaps(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		old := maxUnzippedBytes
		maxUnzippedBytes = 4096
		t.Cleanup(func() { maxUnzippedBytes = old })

		_, err := untarInto(t, tarGzOf(t, []tarEntry{
			{name: "big.bin", body: strings.Repeat("\x00", 64<<10)},
		}), "bomb.tar.gz")
		if err == nil || !strings.Contains(err.Error(), "expands") {
			t.Fatalf("err = %v, want the expansion cap", err)
		}
	})

	// API-APPS-2: the planning pass decompressed the whole stream before any
	// cap counted a byte, so a gzip of zeros was unbounded CPU with nothing
	// written. Freight that the filter drops is in the stream too.
	t.Run("stream", func(t *testing.T) {
		old := maxArchiveStreamBytes
		maxArchiveStreamBytes = 32 << 10
		t.Cleanup(func() { maxArchiveStreamBytes = old })

		_, err := untarInto(t, tarGzOf(t, []tarEntry{
			{name: "app.py", body: "ok"},
			{name: "node_modules/huge.bin", body: strings.Repeat("\x00", 256<<10)},
		}), "bomb.tar.gz")
		if err == nil || !strings.Contains(err.Error(), "decompresses to more than") {
			t.Fatalf("err = %v, want the stream cap", err)
		}
	})

	t.Run("files", func(t *testing.T) {
		old := maxUnzippedFiles
		maxUnzippedFiles = 10
		t.Cleanup(func() { maxUnzippedFiles = old })

		var entries []tarEntry
		for i := 0; i < 20; i++ {
			entries = append(entries, tarEntry{name: fmt.Sprintf("f%d.txt", i), body: "x"})
		}
		_, err := untarInto(t, tarGzOf(t, entries), "many.tar.gz")
		if err == nil || !strings.Contains(err.Error(), "files") {
			t.Fatalf("err = %v, want the file-count cap named", err)
		}
	})

	// Counted after the filter, in the format that has to read the whole stream
	// to know: an app of two files inside a tarball of a large dependency tree
	// deploys.
	t.Run("counted after the filter", func(t *testing.T) {
		old := maxUnzippedFiles
		maxUnzippedFiles = 10
		t.Cleanup(func() { maxUnzippedFiles = old })

		entries := []tarEntry{{name: "app.py", body: "ok"}}
		for i := 0; i < 50; i++ {
			entries = append(entries, tarEntry{name: fmt.Sprintf("node_modules/p%d/index.js", i), body: "x"})
		}
		dir, err := untarInto(t, tarGzOf(t, entries), "app.tar.gz")
		if err != nil {
			t.Fatalf("extractArchive: %v", err)
		}
		present(t, dir, "app.py")
		absent(t, dir, "node_modules")
	})
}

// The name decides the reader, and the bytes are not sniffed: an upload that
// says `.tar.gz` and carries a ZIP is a broken upload, and saying so is better
// than deciding the name was wrong.
func TestExtractArchiveGoesByTheName(t *testing.T) {
	z := zipOf(t, map[string]string{"app.py": "ok"})
	if _, err := untarInto(t, z, "app.tar.gz"); err == nil {
		t.Fatal("a ZIP named .tar.gz was accepted")
	}

	tgz := tarGzOf(t, []tarEntry{{name: "app.py", body: "ok"}})
	if _, err := untarInto(t, tgz, "app.zip"); err == nil {
		t.Fatal("a tarball named .zip was accepted")
	}

	if _, err := untarInto(t, tgz, "app.rar"); err == nil {
		t.Fatal("an unknown extension was accepted")
	}
}

// The suffix table is the one place that decides, and both upload routes ask it
// before a byte of the body is read.
func TestArchiveSuffix(t *testing.T) {
	cases := map[string]string{
		"app.zip":       ".zip",
		"App.ZIP":       ".zip",
		"app.tar.gz":    ".tar.gz",
		"app.TGZ":       ".tgz",
		"my.app.tar.gz": ".tar.gz",
		"app.gz":        "",
		"app.tar":       "",
		"app":           "",
		"app.zip.txt":   "",
	}
	for name, want := range cases {
		if got := archiveSuffix(name); got != want {
			t.Errorf("archiveSuffix(%q) = %q, want %q", name, got, want)
		}
	}
}

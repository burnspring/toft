package builder

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"toft/internal/toolchain"
)

// Reading a file the repository supplied.
//
// Every file the builder reads out of a release — its manifest, its .env
// defaults, its lockfiles, the source it detects a framework from — came from
// the repository, and a repository can commit a symlink (git keeps them; only
// an upload drops them). Read by joined path as the platform's uid, a
// `.env -> /etc/...` link is root reading a host file into an app owner's
// environment variables. After the handover, and at every boot that re-detects
// the current release, the tree is the app account's and it can plant such a
// link at any time.
//
// So these reads go through an os.Root on the release: a link that stays
// inside the tree is followed (the content is the app's own either way), and
// one that leaves it is refused. A missing file still reads as fs.ErrNotExist,
// so callers keep their "absent is the ordinary case" branch.

// openInRelease opens name, a slash- or OS-separated path relative to dir,
// without letting it resolve outside dir. The file must be a regular file.
func openInRelease(dir, name string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Non-blocking, so a FIFO planted where a file was expected fails the
	// type check below instead of holding the open until a writer comes.
	f, err := root.OpenFile(name, os.O_RDONLY|toolchain.OpenNonblock, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, &os.PathError{Op: "open", Path: name, Err: fmt.Errorf("not a regular file")}
	}
	return f, nil
}

// readInRelease is os.ReadFile for a file of the release, under the same rule
// as openInRelease.
func readInRelease(dir, name string) ([]byte, error) {
	f, err := openInRelease(dir, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return toolchain.ReadRegular(f, name, toolchain.MaxManifestBytes)
}

// dropEscapingLinks removes every symbolic link in a freshly fetched release
// that resolves outside it, or resolves nowhere, and returns the removed
// paths relative to dir.
//
// **Straight after the clone, while only root can write the tree.** The steps
// between the fetch and the handover — the Connect manifest's renv.lock, the
// dependency scan's resolvers, detection — run as the platform, and two of
// them run tools that read and write by path (npm, uv) and cannot be put
// behind an os.Root. A committed `requirements.txt -> /etc/...` was read by uv
// as root and its parse error, carrying the content, landed in the owner's
// build log; a committed `.toft-resolved.txt` or dangling `renv.lock` link was
// written through (security review 2026-11-10, E-2 and E-4). With the links
// gone nothing a tool resolves leaves the release, and nothing can put one
// back before the handover. A link inside the tree stays: the content is the
// app's own either way.
//
// The walk itself follows no link (WalkDir reports a link, never descends it).
// A dangling link is removed whatever it names: it can serve nothing, and a
// dangling link is exactly what a create-by-path writes through.
func dropEscapingLinks(dir string) ([]string, error) {
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var dropped []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil && within(base, resolved) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		dropped = append(dropped, filepath.ToSlash(rel))
		return nil
	})
	return dropped, err
}

// within reports whether path is base or below it, both already clean.
func within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// firstN is at most n of s, with a count of the rest when there is one.
func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("and %d more", len(s)-n))
}

package hoststore

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"toft/internal/artifact"
)

// tempPrefix names an upload in flight. It is beside its target so the final
// link or rename is on one file system, and a listing never shows it.
const tempPrefix = ".toft-upload-"

// staleTemp is how old an upload's temp file has to be before it is taken for
// one a killed process left: no upload runs for a day. The deferred removal in
// servePut is the only other cleanup, and a kill skips it, leaving up to 5 GiB
// hidden from every listing and a folder that looks empty but cannot be
// removed (AS-11).
const staleTemp = 24 * time.Hour

// sweepTemp removes name from folder if it is an upload's temp file older
// than staleTemp, and reports whether name was a temp file at all.
func sweepTemp(folder *os.Root, name string, info fs.FileInfo) bool {
	if !strings.HasPrefix(path.Base(name), tempPrefix) {
		return false
	}
	if info != nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > staleTemp {
		_ = folder.Remove(name)
	}
	return true
}

// list answers a listing the way S3 answers one, so nothing above the signer
// can tell the two apart: names relative to the listed prefix, folders as
// slash-terminated prefixes one level down when there is a delimiter, and with
// none every file beneath plus a marker for every folder — what a bucket shows
// where each folder was made with one, and what lets a recursive copy carry an
// empty folder.
//
// Sorted by name and paged by the last name returned, which is an offset that
// cannot skip a name: what is added before the cursor is simply not seen.
//
// It walks inside the listed folder, descended to without following a link,
// and **a link is never followed or listed** below the scope (AS-13): an app
// on one gate could otherwise show a person another gate's files under its own
// folder. A prefix through a link lists empty.
func list(resourceDir, scope, prefix, cursor, delimiter string, limit int) (*artifact.ObjectPage, error) {
	page := &artifact.ObjectPage{Objects: []artifact.StoreObject{}}
	root, err := openScope(resourceDir, scope, false, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return page, nil // a folder nothing has been written to yet lists empty, as on S3
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()

	rel := strings.TrimSuffix(relToScope(scope, prefix), "/")
	if strings.TrimSuffix(prefix, "/") == scope {
		rel = ""
	}
	folder, _, err := descend(root, rel, false, 0)
	// A file where a folder was asked for lists empty, as a key prefix that
	// names an object does on S3 — never an error the API reads as the store
	// not answering (M1, 2026-11-04 black-box list). So does a folder that
	// does not exist, and one reached through a link.
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotFolder) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, errLinkedFolder) {
		return page, nil
	}
	if err != nil {
		return nil, err
	}
	defer folder.Close()

	type entry struct {
		name   string // relative to the listed prefix
		folder bool
		info   fs.FileInfo
	}
	var entries []entry
	fsys := folder.FS()
	if delimiter != "" {
		des, err := fs.ReadDir(fsys, ".")
		if err != nil {
			return nil, err
		}
		for _, de := range des {
			info, err := de.Info() // an lstat: a link is a link, not its target
			if err != nil {
				continue // gone since the read
			}
			if sweepTemp(folder, de.Name(), info) {
				continue
			}
			switch {
			case info.IsDir():
				entries = append(entries, entry{name: de.Name() + "/", folder: true})
			case info.Mode().IsRegular():
				entries = append(entries, entry{name: de.Name(), info: info})
			}
		}
	} else {
		// WalkDir never descends a link; and an entry's Info is an lstat, so a
		// link to a file is not listed either.
		err := fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
			if err != nil || p == "." {
				return nil
			}
			info, err := de.Info()
			if err != nil {
				return nil
			}
			if sweepTemp(folder, p, info) {
				return nil
			}
			switch {
			case info.IsDir():
				entries = append(entries, entry{name: p + "/", info: info})
			case info.Mode().IsRegular():
				entries = append(entries, entry{name: p, info: info})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	last := ""
	for _, e := range entries {
		if cursor != "" && e.name <= cursor {
			continue
		}
		if limit > 0 && len(page.Objects)+len(page.Prefixes) == limit {
			page.NextCursor = last
			break
		}
		if e.folder {
			page.Prefixes = append(page.Prefixes, e.name)
		} else {
			var size int64
			if !strings.HasSuffix(e.name, "/") {
				size = e.info.Size()
			}
			page.Objects = append(page.Objects, artifact.StoreObject{
				Name: e.name, Size: size, LastModified: e.info.ModTime().UTC()})
		}
		last = e.name
	}
	return page, nil
}

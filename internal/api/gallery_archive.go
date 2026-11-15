package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/gallery"
)

// The example half of a gallery deploy: fetch the archive the catalog names and
// stage it exactly where a human upload would have landed.
//
// Nothing here is a new way to run code. The app is created with an `upload://`
// source, so builder.IsUploadedSource reports true and every existing guard
// that means "this app has no remote" applies unchanged — and from the moment
// it lands it is an ordinary uploaded app its owner owns. That is the whole
// design of an example: a copy, taken once, never updated, with no provenance
// rule that reaches back and changes what it may do.

// exampleArchiveName is the filename recorded in the app's `upload://` source.
// It is cosmetic — it appears where an uploaded ZIP's name would — but it is
// worth being the item and version rather than "gallery.zip", because it is
// what somebody reads when they ask where this app came from.
func exampleArchiveName(it gallery.Item) string {
	base := it.SuggestedSlug
	if base == "" {
		base = sanitizeSlug(it.Name)
	}
	v := strings.TrimSpace(it.Version)
	if v != "" {
		base += "-" + v
	}
	return base + ".zip"
}

// stageExampleArchive downloads the item's archive and returns the function
// that unpacks it into the app's staging directory.
//
// Split in two on purpose. The download happens *before* the app row exists, so
// a catalog pointing at a dead link leaves nothing behind to explain; the
// unpack happens after, because the staging directory is named for the app id.
// The temporary file is removed on every path, including the ones that fail:
// the unpack removes it when it runs, and the cleanup is returned separately
// for the caller whose app row could not be created and who therefore never
// unpacks — up to 100 MB per attempt on a /tmp that is a small tmpfs.
//
// The unpack function reports what the filter removed, and the caller puts it
// on the build job: a catalog archive carrying a `node_modules` gets the same
// treatment and the same report as one somebody uploaded, because the strip is
// a property of the unpacker rather than of the door (ADR §60).
func (s *Server) stageExampleArchive(r *http.Request, it gallery.Item, app *database.App) (unpack func() ([]builder.RemovedEntry, error), cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "toft-gallery-*.zip")
	if err != nil {
		return nil, nil, fmt.Errorf("could not stage the download")
	}
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			tmp.Close()
			os.Remove(tmp.Name())
		})
	}

	size, err := s.gallery.Fetcher().FetchTo(r.Context(), it.ZipURL, gallery.MaxZipBytes, tmp)
	if err != nil {
		cleanup()
		// The catalog's own address is named: this is the one failure an
		// administrator can act on, by telling whoever publishes the catalog.
		return nil, nil, fmt.Errorf("could not download %s: %v", it.ZipURL, err)
	}

	return func() ([]builder.RemovedEntry, error) {
		defer cleanup()
		if _, err := tmp.Seek(0, 0); err != nil {
			return nil, fmt.Errorf("could not read the downloaded archive")
		}
		dir := builder.UploadStagingDir(s.cfg.AppsDir, app.ID)
		if it.Path == "" {
			removed, err := extractArchive(tmp, size, dir, exampleArchiveName(it))
			if err != nil {
				return nil, fmt.Errorf("the example's archive could not be unpacked: %v", err)
			}
			return removed, nil
		}
		// A catalog whose items share one repository serves each example as an
		// archive of the whole repository, and Path is the folder that is the
		// example. The archive is unpacked beside the staging directory and the
		// folder alone moves in, by the same function a git build uses, so the
		// two kinds cannot disagree about what a folder of a source may be.
		whole := dir + ".whole"
		defer os.RemoveAll(whole)
		removed, err := extractArchive(tmp, size, whole, exampleArchiveName(it))
		if err != nil {
			return nil, fmt.Errorf("the example's archive could not be unpacked: %v", err)
		}
		if err := builder.PlaceSubdir(whole, it.Path, dir); err != nil {
			return nil, fmt.Errorf("the example's archive: %v", err)
		}
		return removed, nil
	}, cleanup, nil
}

package api

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// archiveSkipDir reports whether a directory is left out of the download:
// build and dependency artifacts reproducible from the source itself, which
// would balloon a download from kilobytes to hundreds of megabytes without
// adding anything the recipient can't regenerate.
//
// `builder.ExcludedDirs` plus `.next`, spelled as an explicit union rather than
// as a third list that happens to agree with it (ADR §60). The extra member is
// deliberate and stays: the shared set holds no build output, because on the
// way *in* a `dist/` may be the only copy that exists — a release directory on
// the way *out* was built from the manifests beside it.
func archiveSkipDir(name string) bool {
	return builder.ExcludedDirs[name] || name == ".next"
}

// handleDeploymentArchive — GET /apps/{appID}/deployments/{deployID}/archive.
// Streams the deployment's release directory as a ZIP: the source of that
// version as it was deployed, minus regenerable artifacts. Source is more
// sensitive than the running app, so this requires edit rights, not view.
func (s *Server) handleDeploymentArchive(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}

	// Pin the deployment to the app in the URL — a deployment ID alone is an
	// IDOR (authorization was evaluated against the app in the path).
	deploy, err := s.db.GetDeployment(chi.URLParam(r, "deployID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if deploy == nil || deploy.AppID != app.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found", "hint": "toft deployments lists this app's deployments with their ids"})
		return
	}
	if deploy.Status != database.DeployStatusSuccess {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "only a successful deployment can be downloaded", "hint": "pick a successful deployment; toft deployments lists them with their status"})
		return
	}
	version := deploy.CommitSHA
	if len(version) > 12 {
		version = version[:12]
	}
	if version == "" {
		version = deploy.ID
	}
	filename := fmt.Sprintf("%s-%s.zip", app.Slug, version)

	// The release on disk, or — where the disk is only a cache of the artifact
	// store and this release is not on it — a verified copy fetched for the
	// length of the download. A rollback row owns no tree; its source does.
	releaseID := deploy.ID
	if deploy.SourceDeployID != "" {
		releaseID = deploy.SourceDeployID
	}
	err = s.builder.WithRelease(app.ID, releaseID, func(releaseDir string) error {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		w.WriteHeader(http.StatusOK)

		if err := zipTree(w, releaseDir); err != nil {
			// Headers are gone; all we can do is log and cut the stream short so
			// the client sees a truncated (invalid) zip rather than silence.
			log.Printf("archive %s deployment %s: %v", app.Slug, deploy.ID, err)
		}
		return nil
	})
	switch {
	case errors.Is(err, builder.ErrDiskFull):
		writeJSON(w, http.StatusInsufficientStorage, map[string]string{"error": err.Error()})
	case errors.Is(err, builder.ErrReleaseGone):
		writeJSON(w, http.StatusGone, map[string]string{"error": "that release no longer exists", "hint": "pick a newer deployment; toft deployments lists the ones still kept"})
	case err != nil:
		log.Printf("archive %s deployment %s: fetching the release: %v", app.Slug, deploy.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the release could not be fetched from the artifact store"})
	}
}

// zipTree writes dir's regular files into a ZIP on out, skipping regenerable
// artifact directories, symlinks (a venv is full of them, and a link
// escaping the release dir must not widen the archive), and the compiled Go
// binary.
//
// **The tree is read through an os.Root.** In the shared runtime this process
// is root and the release is owned by the app's own account, which can rename
// and symlink entries while the walk is in progress: a type check on the
// listing followed by a path-based open is a race the app wins. Every
// directory read and file open here resolves inside dir and refuses a link
// that leaves it, and the opened descriptor is checked to be a regular file
// before a byte is copied.
func zipTree(out io.Writer, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	zw := zip.NewWriter(out)
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			// Vanished or swapped for a link that leaves the tree, mid-walk.
			return nil
		}
		if d.IsDir() {
			if rel != "." && archiveSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || rel == runtime.GoBinaryName {
			return nil
		}
		// The shared backend writes its process-manager config into the release
		// root at deploy time, clobbering any file of the same name — so what is
		// here is platform output, not the user's source.
		if rel == "ecosystem.config.json" {
			return nil
		}

		src, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil
		}
		defer src.Close()
		info, err := src.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		f, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, src)
		return err
	})
	if err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

package staticserve

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// NotFoundDocument is the app's own 404 body, if it ships one. An app's 404
// belongs to the app; the platform's interstitial is for states of the
// *workload*, which is a different thing being wrong.
const NotFoundDocument = "404.html"

// refusedNames are the files a static app never publishes, whatever its root.
//
// The list is not configurable and there is no switch: a file server whose
// exclusions can be turned off is a file server whose exclusions get turned
// off. Two of these are the platform's own doing — the manifest the author
// wrote for the platform, and the process-manager entry the shared backend
// writes *into* the release directory — and the third is the whole family of
// dotfiles, which is where `.git/`, `.env`, `.env.production`, deploy keys and
// CI configuration all live.
//
// Refusals are 404, never 403, for the house reason: a 403 confirms the file is
// there.
var refusedNames = map[string]bool{
	ManifestFile:            true,
	"ecosystem.config.json": true,
}

// Refused reports whether one path segment may never be served. Applied to
// every segment, so a dot-directory hides its whole subtree at any depth.
func Refused(segment string) bool {
	return strings.HasPrefix(segment, ".") || refusedNames[segment]
}

// Handler serves one release's document root.
//
// Everything resolves through os.Root, which refuses to leave the directory —
// symlinks included. A repo may legally contain `link -> /etc/passwd`:
// copyTree drops symlinks on the promote path, but a *clone* keeps them and a
// naive http.FileServer follows them. os.Root is the answer that needs no path
// cleaning anybody has to review.
type Handler struct {
	root *os.Root
	cfg  Config
	// release tells this process's validators from any other's. Every deploy,
	// rollback and wake starts a new serve-static process, so a release's files
	// never share an ETag with another release's — which modification times
	// alone cannot promise: a rollback serves files *older* than the copy the
	// browser holds, and If-Modified-Since would answer 304 to it.
	release string
}

// NewHandler opens the document root. The returned handler owns the os.Root
// and Close releases it.
func NewHandler(cfg Config) (*Handler, error) {
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	var salt [6]byte
	_, _ = rand.Read(salt[:])
	return &Handler{root: root, cfg: cfg, release: hex.EncodeToString(salt[:])}, nil
}

func (h *Handler) Close() error { return h.root.Close() }

// Config reports the decision this handler is acting on.
func (h *Handler) Config() Config { return h.cfg }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A file server answers two verbs. Anything else is a request for a server
	// this app does not have, and saying so is more use than a 404. The
	// sentence names what is true of every app served here — a static site
	// and a rendered document (R Markdown, Quarto, a notebook) alike — not
	// "static site", which a document's owner does not recognise as theirs.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "this app serves files, and answers GET and HEAD only",
			http.StatusMethodNotAllowed)
		return
	}

	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
	}
	clean := path.Clean(upath)

	// The entry document is served *at* the mount path, not redirected to.
	// A 302 to /apps/{slug}/report.html would put the file name into every
	// bookmark, every link pasted into chat and every screenshot, and break all
	// of them the day the file is renamed.
	if clean == "/" {
		h.serveFile(w, r, h.cfg.Entry, http.StatusOK)
		return
	}

	name := strings.TrimPrefix(clean, "/")
	if refusedPath(name) {
		h.miss(w, r)
		return
	}

	info, err := h.root.Stat(name)
	if err != nil {
		h.miss(w, r)
		return
	}

	if info.IsDir() {
		// Without the trailing slash every relative URL on the document below
		// resolves one directory too high. The Location is root-relative and
		// the proxy's rewriteLocation puts the app's prefix back on it.
		if !strings.HasSuffix(upath, "/") {
			target := clean + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		index := name + "/index.html"
		if st, err := h.root.Stat(index); err != nil || st.IsDir() {
			// No listing, and no switch to turn one on: a listing publishes the
			// names of files nobody chose to publish.
			h.miss(w, r)
			return
		}
		h.serveFile(w, r, index, http.StatusOK)
		return
	}

	h.serveFile(w, r, name, http.StatusOK)
}

// refusedPath applies the refusal list to every segment of a path.
func refusedPath(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if seg != "" && Refused(seg) {
			return true
		}
	}
	return false
}

// serveFile writes one file. status is 200 for a hit and 404 for the app's own
// error document, which is the one case where the body and the status come
// from different decisions.
//
// http.ServeContent is what makes conditional requests and Range work, which is
// what makes a 40 MB video or a PDF behave. It writes its own status, so the
// 404 case cannot use it.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string, status int) {
	f, err := h.root.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Every file is revalidated before a browser reuses it. With no
	// Cache-Control a browser picks its own freshness — Safari keeps a page for
	// a tenth of its age since Last-Modified — so a redeploy stayed invisible
	// for hours. A revalidation that finds nothing changed is a 304 with no
	// body. Nothing here is marked immutable: the server cannot tell a hashed
	// asset name from a hand-written one.
	w.Header().Set("Cache-Control", "no-cache")

	if status == http.StatusOK {
		w.Header().Set("ETag", fmt.Sprintf(`"%s-%x-%x"`, h.release, info.Size(), info.ModTime().UnixNano()))
		http.ServeContent(w, r, path.Base(name), info.ModTime(), f)
		return
	}

	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

// miss answers a path that matched no file: the SPA fallback if this app asked
// for one, then the app's own 404 document, then a plain 404.
func (h *Handler) miss(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Fallback && looksLikeRoute(r) {
		h.serveFile(w, r, h.cfg.Entry, http.StatusOK)
		return
	}
	if st, err := h.root.Stat(NotFoundDocument); err == nil && !st.IsDir() {
		h.serveFile(w, r, NotFoundDocument, http.StatusNotFound)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// looksLikeRoute is the shape SPA fallback applies to: a GET or HEAD for a
// path with no file extension in its last segment, from a caller that does not
// say it wants something other than HTML.
//
// The extension rule is what keeps a missing asset a 404. Without it, every
// missing script arrives as an HTML document where JavaScript was expected —
// the failure that renders as `Unexpected token '<'` and costs an afternoon.
//
// The Accept rule only refuses a caller that *names* what it wants and HTML is
// not among it — `application/json` with no wildcard. It used to require
// `text/html`, which a browser navigation sends and curl, `toft fetch` and an
// uptime probe do not, so every client route on a fallback app answered 404 to
// anything but a browser (`*/*`, or no Accept at all).
func looksLikeRoute(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if path.Ext(path.Base(r.URL.Path)) != "" {
		return false
	}
	accept := strings.TrimSpace(r.Header.Get("Accept"))
	if accept == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		switch strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) {
		case "text/html", "*/*", "text/*", "application/xhtml+xml":
			return true
		}
	}
	return false
}

func contentType(name string) string {
	if isHTML(name) {
		return "text/html; charset=utf-8"
	}
	return "application/octet-stream"
}

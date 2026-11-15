package spa

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed dist/*
var embeddedFiles embed.FS

func Handler() http.Handler {
	// In development, serve from web/dist if it exists
	if _, err := os.Stat("web/dist/index.html"); err == nil {
		return spaFileServer(http.Dir("web/dist"))
	}

	// In production, serve from embedded files
	dist, _ := fs.Sub(embeddedFiles, "dist")
	return spaFileServer(http.FS(dist))
}

// Cache policy (ADR §124). Everything under /assets/ is named for its content
// hash by Vite, so a changed file is a new URL and the old one may be kept
// forever. index.html is what names them, so it is revalidated on every load —
// otherwise a release is invisible until the cache expires.
const (
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "no-cache"
)

// compressible is the list of extensions gzipped on the way out. Fonts and
// images are already compressed formats; gzipping them costs CPU for nothing.
var compressible = map[string]bool{
	".html": true, ".js": true, ".css": true, ".svg": true, ".json": true,
	".txt": true, ".map": true,
}

func spaFileServer(root http.FileSystem) http.Handler {
	fileServer := http.FileServer(root)
	gz := &gzipCache{entries: map[string]gzipEntry{}}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip API routes
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		// Try to serve the file directly
		p := path.Clean(r.URL.Path)
		if p == "/" {
			p = "/index.html"
		}

		// Check if file exists
		f, err := root.Open(p)
		if err != nil {
			// File not found, serve index.html for SPA routing
			p = "/index.html"
			r.URL.Path = "/"
			f, err = root.Open(p)
			if err != nil {
				http.NotFound(w, r)
				return
			}
		}
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(p, "/assets/") {
			w.Header().Set("Cache-Control", cacheImmutable)
		} else {
			w.Header().Set("Cache-Control", cacheRevalidate)
		}

		ext := path.Ext(p)
		if !compressible[ext] || !acceptsGzip(r) {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")

		body, err := gz.get(p, info, f)
		f.Close()
		if err != nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		if ct := mime.TypeByExtension(ext); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Content-Encoding", "gzip")
		// A byte range of the compressed body is not one anybody asks for, and
		// ServeContent would compute it against the wrong length's validators.
		r.Header.Del("Range")
		http.ServeContent(w, r, p, info.ModTime(), bytes.NewReader(body))
	})
}

// acceptsGzip reports whether the request lists gzip with a non-zero q.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		if v, found := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); found {
			q, err := strconv.ParseFloat(v, 64)
			return err == nil && q > 0
		}
		return true
	}
	return false
}

// gzipCache compresses each file once. The embedded tree never changes; the
// dev tree (web/dist) does on every `npm run build`, so an entry is keyed by
// path and replaced when the file's size or mtime moves.
type gzipCache struct {
	mu      sync.Mutex
	entries map[string]gzipEntry
}

type gzipEntry struct {
	size int64
	mod  time.Time
	body []byte
}

func (c *gzipCache) get(p string, info fs.FileInfo, src io.Reader) ([]byte, error) {
	c.mu.Lock()
	e, ok := c.entries[p]
	c.mu.Unlock()
	if ok && e.size == info.Size() && e.mod.Equal(info.ModTime()) {
		return e.body, nil
	}

	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := io.Copy(zw, src); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[p] = gzipEntry{size: info.Size(), mod: info.ModTime(), body: buf.Bytes()}
	c.mu.Unlock()
	return buf.Bytes(), nil
}

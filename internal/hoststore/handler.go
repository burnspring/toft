package hoststore

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/time/rate"
)

// maxConcurrent is how many transfers one subject may have open at once — the
// CLI's default -j — and perSecond is how fast they may start more. Its own
// bucket, because this route carries bytes and every other route carries
// sentences.
const (
	maxConcurrent = 4
	perSecond     = 20
	burst         = 40
)

// ServeHTTP is `/store-data/{token}`. **The token is the whole credential**: no
// cookie and no Authorization header is read, so a session beside a bad token
// authorizes nothing, and there is no ambient credential for a forged request
// to borrow.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := verify(s.key, strings.TrimPrefix(r.URL.Path, Route), s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if r.Method != c.Method && !(r.Method == http.MethodHead && c.Method == http.MethodGet) {
		http.Error(w, "this link is for "+c.Method, http.StatusMethodNotAllowed)
		return
	}
	release, ok := s.admit(c.Subject)
	if !ok {
		http.Error(w, "too many transfers at once — wait for one to finish", http.StatusTooManyRequests)
		return
	}
	defer release()

	dir, err := s.resourceDir(c.Root, c.Slug)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch c.Method {
	case http.MethodGet:
		s.serveGet(w, r, dir, c)
	case http.MethodPut:
		s.servePut(w, r, dir, c)
	case http.MethodDelete:
		s.serveDelete(w, dir, c)
	default:
		http.Error(w, "this link is not valid", http.StatusForbidden)
	}
}

// admit takes a transfer slot for one subject, and returns its release.
func (s *Store) admit(subject string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lim, ok := s.limiters[subject]
	if !ok {
		lim = rate.NewLimiter(perSecond, burst)
		s.limiters[subject] = lim
	}
	if s.inFlight[subject] >= maxConcurrent || !lim.Allow() {
		return nil, false
	}
	s.inFlight[subject]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.inFlight[subject]--; s.inFlight[subject] <= 0 {
			delete(s.inFlight, subject)
		}
	}, true
}

// serveGet streams one regular file. **Never as anything a browser renders**:
// this is the platform's own origin, so a person's uploaded HTML served with
// its own type would be a page running as the platform. Always a download,
// always opaque bytes, in a sandbox.
func (s *Store) serveGet(w http.ResponseWriter, r *http.Request, dir string, c Claims) {
	root, err := openScope(dir, c.Scope, false, 0)
	if err != nil {
		failure(w, err)
		return
	}
	defer root.Close()
	rel := relToScope(c.Scope, c.Key)
	parent, _, err := descend(root, path.Dir(rel), false, 0)
	if err != nil {
		failure(w, err)
		return
	}
	defer parent.Close()
	// Non-blocking: a blocking open of a FIFO an app planted never returns,
	// and the transfer slot held above would never be given back (AS-8). A
	// regular file reads normally; anything else fails the check below. And
	// never through a link (AS-13): a name that is one is not served.
	f, err := parent.OpenFile(path.Base(rel), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ELOOP) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		failure(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// A second name for a file is the one escape a walk does not see: an app
	// could link a file it cannot read into its folder, and a person would
	// then read it through root. protected_hardlinks forbids the link; this
	// refuses whatever is there anyway, at the cost of a file deliberately
	// linked by its own app.
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		http.Error(w, "this file has more than one name on the disk, so the platform will not serve it",
			http.StatusForbidden)
		return
	}
	name := c.AttachAs
	if name == "" {
		name = path.Base(c.Key)
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", asciiName(name)))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// servePut lands an upload under the ladder (ADR §12a): create-only at `write`,
// replace at `full`, and **never through an existing inode**.
// The body goes to a temp file beside the target, is checked against the
// length and both digests the URL was signed with, and only then takes the
// name — by link(2), which fails on an existing one, or by rename(2), which
// replaces it in one step. A mismatch, a short body or a client that goes away
// leaves the name exactly as it was.
func (s *Store) servePut(w http.ResponseWriter, r *http.Request, dir string, c Claims) {
	root, err := openScope(dir, c.Scope, true, c.GID)
	if err != nil {
		failure(w, err)
		return
	}
	defer root.Close()
	rel := relToScope(c.Scope, c.Key)

	if strings.HasSuffix(c.Key, "/") {
		folder, existed, err := descend(root, strings.TrimSuffix(rel, "/"), true, c.GID)
		if err != nil {
			failure(w, err)
			return
		}
		folder.Close()
		if existed && c.IfNoneMatch {
			http.Error(w, "that folder already exists", http.StatusPreconditionFailed)
			return
		}
		s.audited(c, 0)
		w.WriteHeader(http.StatusOK)
		return
	}

	// The parent is descended to once and held open: the temp create, the
	// link and the rename are all by base name inside that one directory, so
	// a component swapped for a link after the descent cannot move the upload
	// into a sibling folder under the wrong gate (AS-13).
	parent, _, err := descend(root, path.Dir(rel), true, c.GID)
	if err != nil {
		failure(w, err)
		return
	}
	defer parent.Close()
	if beforeLanding != nil {
		beforeLanding()
	}
	base := path.Base(rel)
	tmp := tempPrefix + randomSuffix()
	f, err := parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, gatedFileMode)
	if err != nil {
		failure(w, err)
		return
	}
	landed := false
	defer func() {
		if !landed {
			_ = parent.Remove(tmp)
		}
	}()
	if c.GID > 0 {
		if err := f.Chown(-1, c.GID); err != nil {
			f.Close()
			failure(w, err)
			return
		}
	}
	if err := f.Chmod(gatedFileMode); err != nil {
		f.Close()
		failure(w, err)
		return
	}

	md5h, shah := md5.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(f, md5h, shah), io.LimitReader(r.Body, c.Length+1))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		failure(w, err)
		return
	}
	if n != c.Length {
		http.Error(w, fmt.Sprintf("the body was %d bytes and the link was signed for %d", n, c.Length),
			http.StatusBadRequest)
		return
	}
	if !digestMatches(md5h, c.MD5) || !digestMatches(shah, c.SHA256) {
		http.Error(w, "content digest mismatch — the bytes are not the ones the link was signed for",
			http.StatusBadRequest)
		return
	}

	if c.IfNoneMatch {
		if err := parent.Link(tmp, base); err != nil {
			if errors.Is(err, fs.ErrExist) {
				http.Error(w, "that name already exists", http.StatusPreconditionFailed)
				return
			}
			failure(w, err)
			return
		}
	} else if err := parent.Rename(tmp, base); err != nil {
		failure(w, err)
		return
	} else {
		landed = true
	}
	s.audited(c, n)
	w.WriteHeader(http.StatusOK)
}

// serveDelete removes one name. A file or a link goes; a folder marker is an
// empty-only rmdir, so a folder with anything in it answers 204 and stays —
// it exists while something is under it, as on S3. The scope folder itself is
// never removed: it is what the grant is on.
func (s *Store) serveDelete(w http.ResponseWriter, dir string, c Claims) {
	root, err := openScope(dir, c.Scope, false, 0)
	if errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	defer root.Close()
	rel := strings.TrimSuffix(relToScope(c.Scope, c.Key), "/")
	folder := strings.HasSuffix(c.Key, "/")
	if rel == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Addressed inside its parent, descended to without following a link:
	// `X/sub -> ../Y` made a delete of X/sub/f remove Y/f (AS-13). The name
	// itself may be a link, and then the link goes, never what it points at.
	parent, _, err := descend(root, path.Dir(rel), false, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotFolder) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	defer parent.Close()
	base := path.Base(rel)
	info, err := parent.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	if info.IsDir() != folder {
		// A folder addressed as a file, or the reverse: there is no such
		// object, which S3 answers the same way.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if folder {
		// A temp file a killed upload left would keep the folder for good:
		// hidden from every listing, and the rmdir answers ENOTEMPTY (AS-11).
		sweepFolderTemps(parent, base)
	}
	if err := parent.Remove(base); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sweepFolderTemps removes the stale upload temp files directly in one folder,
// opened without following a link.
func sweepFolderTemps(parent *os.Root, name string) {
	d, _, err := descend(parent, name, false, 0)
	if err != nil {
		return
	}
	defer d.Close()
	des, err := fs.ReadDir(d.FS(), ".")
	if err != nil {
		return
	}
	for _, de := range des {
		if info, err := de.Info(); err == nil {
			sweepTemp(d, de.Name(), info)
		}
	}
}

// beforeLanding runs in servePut after the parent is held and before the
// upload's temp file is made. Nil outside the test that swaps a component for
// a link in exactly that gap.
var beforeLanding func()

func (s *Store) audited(c Claims, n int64) {
	if s.audit != nil {
		s.audit(c, n)
	}
}

// failure turns a file-system refusal into the status a bucket would have
// answered, and says nothing about the disk underneath.
func failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, errLinkedScope), errors.Is(err, errLinkedFolder), errors.Is(err, fs.ErrPermission):
		http.Error(w, "the platform will not reach this path", http.StatusForbidden)
	case errors.Is(err, errNotFolder), errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		http.Error(w, "a file and a folder would share that name", http.StatusConflict)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		http.Error(w, "the disk is full", http.StatusInsufficientStorage)
	case errors.Is(err, syscall.EROFS):
		http.Error(w, "this storage is mounted read-only", http.StatusForbidden)
	default:
		// A path that tries to leave the root lands here too: os.Root's
		// refusal has no errno of its own.
		if strings.Contains(err.Error(), "path escapes") {
			http.Error(w, "the platform will not reach this path", http.StatusForbidden)
			return
		}
		log.Printf("hoststore: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func digestMatches(h hash.Hash, want string) bool {
	return want != "" && base64.StdEncoding.EncodeToString(h.Sum(nil)) == want
}

func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// asciiName is a download's filename with anything a header cannot carry
// replaced, so a quote in a name cannot end the header value early.
func asciiName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '"' || r == '\\' || r < 0x20 || r > 0x7e {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

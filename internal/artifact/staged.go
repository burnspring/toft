package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Staged is a release packed onto local disk and not yet stored: the half of a
// Put that reads the release tree, done, and the half that talks to the network
// still to come.
type Staged struct {
	file   *os.File
	Size   int64
	SHA256 string
}

// Remove deletes the packed file. Safe to call more than once and on nil.
func (s *Staged) Remove() {
	if s == nil || s.file == nil {
		return
	}
	s.file.Close()
	os.Remove(s.file.Name())
	s.file = nil
}

// body rewinds the packed file for an upload, which needs a seekable body to
// sign and to retry.
func (s *Staged) body() (*os.File, error) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return s.file, nil
}

// Uploader is a store whose Put is two halves the caller may run apart.
//
// **Why the split exists** (ADR §123): packing reads the release tree, which an
// app's own process may write to once it starts, so it has to finish before
// the start; uploading reads only the packed file, so it can run while the
// release starts and is waited for before traffic moves. A store that writes
// straight to its destination (LocalStore) has no network half and does not
// implement it.
type Uploader interface {
	// Stage packs dir into a file under the store's staging directory.
	Stage(ctx context.Context, dir string) (*Staged, error)
	// Upload stores a staged release under key. It does not remove the file.
	Upload(ctx context.Context, key string, s *Staged) (Descriptor, error)
}

// UploaderOf reports the store's two halves, seeing through the telemetry
// decorator, which would otherwise hide them.
func UploaderOf(s Store) (Uploader, bool) {
	if i, ok := s.(instrumented); ok {
		u, ok := i.Store.(Uploader)
		if !ok {
			return nil, false
		}
		return instrumentedUploader{u: u, i: i}, true
	}
	u, ok := s.(Uploader)
	return u, ok
}

// StageTo packs dir into a temp file in staging, computing its digest as it
// goes, so the digest is known before the object is written and can travel with
// it. It is every Uploader's Stage.
func StageTo(ctx context.Context, staging, dir string) (*Staged, error) {
	tmp, err := os.CreateTemp(staging, "toft-artifact-*.tar.gz")
	if err != nil {
		return nil, err
	}
	s := &Staged{file: tmp}
	digest := sha256.New()
	if err := pack(ctx, dir, io.MultiWriter(tmp, digest)); err != nil {
		s.Remove()
		return nil, err
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		s.Remove()
		return nil, err
	}
	s.Size, s.SHA256 = size, hex.EncodeToString(digest.Sum(nil))
	return s, nil
}

// putStaged is Put for an Uploader: both halves, back to back.
func putStaged(ctx context.Context, u Uploader, key, dir string) (Descriptor, error) {
	s, err := u.Stage(ctx, dir)
	if err != nil {
		return Descriptor{}, err
	}
	defer s.Remove()
	return u.Upload(ctx, key, s)
}

// instrumentedUploader times the upload as the put it is; the pack is local
// work and the put's own metric never counted it apart.
type instrumentedUploader struct {
	u Uploader
	i instrumented
}

func (w instrumentedUploader) Stage(ctx context.Context, dir string) (*Staged, error) {
	return w.u.Stage(ctx, dir)
}

func (w instrumentedUploader) Upload(ctx context.Context, key string, s *Staged) (Descriptor, error) {
	ctx, done := w.i.begin(ctx, opPut)
	d, err := w.u.Upload(ctx, key, s)
	done(err)
	return d, err
}

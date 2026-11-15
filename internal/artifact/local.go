package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalStore keeps artifacts in a directory on this machine. It is what a VM
// install uses: the same publish-and-restore path as an object store, with no
// object store, registry, or cloud account involved. On a VM the artifact adds
// durability against a wiped APPS_DIR rather than against a replaced host, and
// more importantly it means both topologies exercise one code path.
type LocalStore struct {
	root string
}

// NewLocalStore roots a store at dir, creating it if needed.
func NewLocalStore(dir string) (*LocalStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("artifact: local store needs a directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return &LocalStore{root: abs}, nil
}

// path is the artifact file for a key; meta is its descriptor sidecar.
func (s *LocalStore) path(key string) (string, error) { return safeJoin(s.root, key) }
func (s *LocalStore) metaPath(key string) (string, error) {
	p, err := s.path(key)
	return p + ".meta.json", err
}

func (s *LocalStore) Put(ctx context.Context, key, dir string) (Descriptor, error) {
	dest, err := s.path(key)
	if err != nil {
		return Descriptor{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return Descriptor{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	// Pack to a temporary neighbour and rename. A reader must never see a
	// half-written artifact under a key that a deployment row already points
	// at, and a rename within one directory is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(dest), partialPrefix+"*")
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	digest := sha256.New()
	if err := pack(ctx, dir, io.MultiWriter(tmp, digest)); err != nil {
		tmp.Close()
		return Descriptor{}, err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		tmp.Close()
		return Descriptor{}, err
	}
	if err := tmp.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	desc := Descriptor{
		Key:      key,
		Size:     size,
		SHA256:   hex.EncodeToString(digest.Sum(nil)),
		StoredAt: time.Now().UTC(),
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return Descriptor{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if err := s.writeMeta(key, desc); err != nil {
		return Descriptor{}, err
	}
	return desc, nil
}

func (s *LocalStore) Get(ctx context.Context, key, dest string) error {
	src, err := s.path(key)
	if err != nil {
		return err
	}
	desc, err := s.Stat(ctx, key)
	if err != nil {
		return err
	}

	f, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer f.Close()

	// Hash while unpacking rather than in a separate pass — but that means the
	// tree is written before the digest is known, so unpack into a staging
	// directory and only publish it once the bytes have proven out. A caller
	// must never be handed a tree assembled from corrupt input.
	return verifyAndUnpack(f, desc, dest)
}

func (s *LocalStore) Stat(ctx context.Context, key string) (Descriptor, error) {
	mp, err := s.metaPath(key)
	if err != nil {
		return Descriptor{}, err
	}
	data, err := os.ReadFile(mp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return Descriptor{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var desc Descriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return Descriptor{}, fmt.Errorf("%w: descriptor for %s is unreadable: %v", ErrIntegrity, key, err)
	}
	return desc, nil
}

func (s *LocalStore) Delete(ctx context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	mp, _ := s.metaPath(key)
	os.Remove(mp)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}

// partialPrefix names a Put's temp file, and partialGrace is how long Expire
// leaves one alone: past the builder's publish deadline (15 minutes) with
// room for a stalled write the deadline abandoned but did not stop.
const (
	partialPrefix = ".partial-"
	partialGrace  = time.Hour
)

func (s *LocalStore) Expire(ctx context.Context, appID string, keep []string) (int, error) {
	dir, err := safeJoin(s.root, releasePrefix(appID))
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}

	removed := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		// Another build's Put in flight packs into a .partial- beside its
		// key; removing it failed that build's rename and its deploy (AS-9).
		// One older than any publish could run was left by a crash.
		if strings.HasPrefix(e.Name(), partialPrefix) {
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < partialGrace {
				continue
			}
		}
		key := releasePrefix(appID) + e.Name()
		if kept[key] {
			continue
		}
		if err := s.Delete(ctx, key); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *LocalStore) writeMeta(key string, desc Descriptor) error {
	mp, err := s.metaPath(key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(desc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(mp, data, 0600); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}

// verifyAndUnpack streams r into a staging directory while hashing it, and
// publishes the staging tree over dest only if the digest matches. A failed
// restore therefore leaves dest exactly as it was, so nothing can be started
// from a tree assembled out of corrupt or truncated bytes.
func verifyAndUnpack(r io.Reader, desc Descriptor, dest string) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return err
	}

	staging, err := os.MkdirTemp(filepath.Dir(abs), ".restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	digest := sha256.New()
	if err := unpack(io.TeeReader(r, digest), staging); err != nil {
		return err
	}

	if desc.SHA256 != "" {
		got := hex.EncodeToString(digest.Sum(nil))
		if got != desc.SHA256 {
			return fmt.Errorf("%w: %s digest %s, expected %s", ErrIntegrity, desc.Key, got, desc.SHA256)
		}
	}

	// Swap the verified tree in. Removing first means a crash between the two
	// leaves no directory rather than a stale one, which the reconciler
	// already knows how to handle (errNoRelease → redeploy).
	if err := os.RemoveAll(abs); err != nil {
		return err
	}
	return os.Rename(staging, abs)
}

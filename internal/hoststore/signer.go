package hoststore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"

	"toft/internal/artifact"
	"toft/internal/runtime"
)

// Route is where the data plane is mounted: the platform listener, outside
// /api/v1, never the apps listener.
const Route = "/store-data/"

// WriteAudit records a completed write. The platform saw the bytes here, so it
// can say they landed — which a bucket's mint-only audit cannot.
type WriteAudit func(c Claims, bytes int64)

// Store is the host data plane: the key it signs with, where its URLs point,
// and the roots it serves from.
type Store struct {
	key     []byte
	baseURL string
	roots   map[string]runtime.StorageRoot
	audit   WriteAudit
	now     func() time.Time

	mu       sync.Mutex
	inFlight map[string]int
	limiters map[string]*rate.Limiter
}

// New builds the data plane. baseURL is the platform's public address; roots
// are the operator's, as parsed from SHARED_STORAGE_ROOTS.
func New(key []byte, baseURL string, roots []runtime.StorageRoot, audit WriteAudit) *Store {
	byName := make(map[string]runtime.StorageRoot, len(roots))
	for _, r := range roots {
		byName[r.Name] = r
	}
	return &Store{
		key: key, baseURL: strings.TrimRight(baseURL, "/"), roots: byName, audit: audit,
		now: time.Now, inFlight: map[string]int{}, limiters: map[string]*rate.Limiter{},
	}
}

// ErrHardlinksUnprotected is why the data plane is absent on a host whose
// kernel lets an account hard-link a file it cannot read: an app could link
// one into its folder, and a person could then download it through root.
var ErrHardlinksUnprotected = errors.New(
	"fs.protected_hardlinks is not 1 on this host, so the platform cannot serve a host directory " +
		"without an app being able to hand a person a file it could not read itself")

// Probe reports whether this host can carry the data plane at all. Read once
// at boot; a kernel setting moved under a running install is the operator's.
func Probe() error {
	b, err := os.ReadFile("/proc/sys/fs/protected_hardlinks")
	if err != nil {
		return fmt.Errorf("%w (reading it: %v)", ErrHardlinksUnprotected, err)
	}
	if strings.TrimSpace(string(b)) != "1" {
		return ErrHardlinksUnprotected
	}
	return nil
}

// Binding is one request's worth of what a signature needs beyond the verb and
// the key: which resource, which scope opens the containment root, who asked,
// and the gate a write takes.
type Binding struct {
	Resource string
	Root     string
	Slug     string
	Scope    string
	Subject  string
	GID      int
}

// For returns a signer for one resource at one scope. The bucket argument of
// its methods is ignored: the resource is the binding's.
func (s *Store) For(b Binding) artifact.Signer { return &bound{store: s, b: b} }

// ReadOnly reports whether a root is mounted read-only, which the grant route
// reads to refuse a level the file system would refuse anyway.
func (s *Store) ReadOnly(root string) bool { return s.roots[root].ReadOnly }

type bound struct {
	store *Store
	b     Binding
}

func (b *bound) mint(method, key string, ttl time.Duration, fill func(*Claims)) (*artifact.SignedRequest, error) {
	c := Claims{
		Resource: b.b.Resource, Root: b.b.Root, Slug: b.b.Slug, Key: key, Method: method,
		Scope: b.b.Scope, Subject: b.b.Subject, Expires: b.store.now().Add(ttl).Unix(),
	}
	if fill != nil {
		fill(&c)
	}
	if !underScope(c.Scope, key) {
		return nil, fmt.Errorf("hoststore: %q is outside the scope %q it was signed under", key, c.Scope)
	}
	token, err := sign(b.store.key, c)
	if err != nil {
		return nil, err
	}
	return &artifact.SignedRequest{URL: b.store.baseURL + Route + token, Method: method,
		Headers: map[string]string{}}, nil
}

func (b *bound) PresignGet(_ context.Context, _, key string, ttl time.Duration, attachAs string) (*artifact.SignedRequest, error) {
	return b.mint(http.MethodGet, key, ttl, func(c *Claims) { c.AttachAs = attachAs })
}

func (b *bound) PresignPut(_ context.Context, _, key string, ttl time.Duration, cond artifact.PutConditions) (*artifact.SignedRequest, error) {
	signed, err := b.mint(http.MethodPut, key, ttl, func(c *Claims) {
		c.Length, c.MD5, c.SHA256, c.IfNoneMatch, c.GID =
			cond.ContentLength, cond.ContentMD5, cond.ChecksumSHA256, cond.IfNoneMatch, b.b.GID
	})
	if err == nil && cond.IfNoneMatch {
		signed.ExistsStatus = http.StatusPreconditionFailed
	}
	return signed, err
}

func (b *bound) PresignDelete(_ context.Context, _, key string, ttl time.Duration) (*artifact.SignedRequest, error) {
	return b.mint(http.MethodDelete, key, ttl, nil)
}

// HasObject is a regular file at key inside the scope, opened the way serveGet
// opens one, so "exists" here is "a GET of it would be served".
func (b *bound) HasObject(_ context.Context, _, key string) (bool, error) {
	if !underScope(b.b.Scope, key) {
		return false, nil
	}
	dir, err := b.store.resourceDir(b.b.Root, b.b.Slug)
	if err != nil {
		return false, err
	}
	root, err := openScope(dir, b.b.Scope, false, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotFolder) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	rel := relToScope(b.b.Scope, key)
	parent, _, err := descend(root, path.Dir(rel), false, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotFolder) || errors.Is(err, errLinkedFolder) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer parent.Close()
	// Lstat, as serveGet opens O_NOFOLLOW: a link is never served (AS-13).
	info, err := parent.Lstat(path.Base(rel))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func (b *bound) ListObjects(_ context.Context, _, prefix, cursor, delimiter string, limit int32) (*artifact.ObjectPage, error) {
	dir, err := b.store.resourceDir(b.b.Root, b.b.Slug)
	if err != nil {
		return nil, err
	}
	if !underScope(b.b.Scope, prefix) && strings.TrimSuffix(prefix, "/") != b.b.Scope {
		return nil, fmt.Errorf("hoststore: listing %q is outside the scope %q", prefix, b.b.Scope)
	}
	return list(dir, b.b.Scope, prefix, cursor, delimiter, int(limit))
}

// resourceDir is `<root>/<slug>`, the root resolved by name against this
// install's configuration.
func (s *Store) resourceDir(root, slug string) (string, error) {
	r, ok := s.roots[root]
	if !ok || slug == "" || strings.ContainsAny(slug, `/\`) {
		return "", fmt.Errorf("hoststore: no root %q here", root)
	}
	return filepath.Join(r.Path, slug), nil
}

// underScope reports whether key is inside the scope folder — on a separator,
// as a grant covers a key.
func underScope(scope, key string) bool {
	return scope == "" || strings.HasPrefix(key, scope+"/")
}

// relToScope is key with the scope folder taken off the front, which is the
// name inside the containment root.
func relToScope(scope, key string) string {
	if scope == "" {
		return key
	}
	return strings.TrimPrefix(key, scope+"/")
}

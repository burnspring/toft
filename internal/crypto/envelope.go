package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"
)

// Envelope encryption: the key that encrypts data is itself encrypted by a key
// that never leaves the KMS.
//
// **The reason for the envelope is functional rather than performance.** AWS
// KMS caps a direct Encrypt at 4 KB of plaintext, and an app environment
// variable holding a certificate or a pasted private key will exceed that.
// Latency would have survived either way — decryption happens at job and run
// start and in the builder, and the data-path invariant already keeps all of it
// off the proxy request path.
//
// **What this buys, precisely.** A stolen database dump or a stolen host disk
// is useless without the KMS grant, which is the threat the local backend
// fails. What it does not buy is protection from memory disclosure on a running
// platform: the DEK is plaintext in this process while it is cached, exactly as
// the local key is. Moving the KEK out of the process is not moving the DEK out
// of it, and no copy may imply otherwise.

// KEK is the key-encrypting key, wherever it lives. Two implementations ship —
// AWS KMS and Vault Transit — plus an in-memory fake so the test suite and CI
// need no cloud account. There is deliberately no plugin system: a third scheme
// is a code change, which is the right amount of friction.
type KEK interface {
	// Wrap encrypts a data key. Called once per key generation.
	Wrap(ctx context.Context, dek []byte) ([]byte, error)
	// Unwrap decrypts one. Called on a cache miss, which is once per TTL.
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
	// URI is what the operator configured, for reports and errors. It carries
	// no credential, by construction.
	URI() string
}

// DEKStore is where wrapped data keys live between restarts — the platform
// database, in practice. The interface is here rather than in internal/database
// so this package depends on nothing; Go's structural interfaces mean the
// database package satisfies it without importing this one.
//
// The wrapped key is stored rather than carried in every record because the
// record format is one format (ADR §62) and a fourth field for the managed
// backend would make it two. The key id is the handle.
type DEKStore interface {
	// ActiveDEK returns the generation new records are written under. A store
	// with none returns ok=false rather than an error — a first boot is not a
	// failure.
	ActiveDEK() (id string, wrapped []byte, ok bool, err error)
	// DEK returns one generation by id, for reading an older record.
	DEK(id string) (wrapped []byte, ok bool, err error)
	// PutDEK records a new generation and makes it the active one.
	PutDEK(id string, wrapped []byte, kekURI string) error
}

// DefaultDEKCacheTTL is how long an unwrapped data key is held in memory.
//
// It is a real number rather than "forever" on purpose. Holding a DEK for the
// life of the process would make a revoked KMS grant take effect only at the
// next restart, which is the same "rotation is not revocation" trap the local
// backend has — and the whole argument for a KMS is that the grant is the
// control. Fifteen minutes bounds that, absorbs a short KMS outage, and costs
// four unwrap calls an hour.
const DefaultDEKCacheTTL = 15 * time.Minute

type envelope struct {
	kek   KEK
	store DEKStore
	ttl   time.Duration

	// currentID is the generation new records are written under. Resolved once
	// at startup and never changed while the process runs: a generation that
	// could change underneath a running platform would mean two nodes writing
	// under different ids with no reason to agree.
	currentID string

	mu    sync.Mutex
	cache map[string]cachedKey
}

type cachedKey struct {
	gcm     cipher.AEAD
	expires time.Time
}

// newEnvelope resolves the active generation, creating one if the store has
// none, and proves the KMS is reachable before returning. **Resolution is at
// startup and there is no runtime fallback**: an install told to use a KMS and
// unable to reach it fails loudly and keeps failing. Quietly falling back to a
// local key on a KMS outage would be an escape hatch that triggers itself,
// which is a downgrade attack — and the same rule the Slack invariant already
// states, that a chosen channel which stops working fails rather than falling
// back.
func newEnvelope(ctx context.Context, kek KEK, store DEKStore, ttl time.Duration) (*envelope, error) {
	if ttl <= 0 {
		ttl = DefaultDEKCacheTTL
	}
	e := &envelope{kek: kek, store: store, ttl: ttl, cache: map[string]cachedKey{}}

	id, wrapped, ok, err := store.ActiveDEK()
	if err != nil {
		return nil, fmt.Errorf("reading the stored data key: %w", err)
	}
	if !ok {
		if id, wrapped, err = e.generate(ctx); err != nil {
			return nil, err
		}
	}
	e.currentID = id

	// Fail here rather than at the first secret an hour from now. This is the
	// one call that proves the URI names a key that exists, that the ambient
	// credentials reach it, and that the grant allows decrypt as well as
	// encrypt.
	if _, err := e.unwrap(ctx, id, wrapped); err != nil {
		return nil, fmt.Errorf("the configured KMS could not unwrap this install's data key: %w", err)
	}
	return e, nil
}

// generate mints a data key, wraps it, and records it. The id is a fingerprint
// of the *plaintext* data key, exactly as a local key's is, so one derivation
// rule covers both backends and a record does not say which kind of key wrote
// it — which is what lets an install move between them.
func (e *envelope) generate(ctx context.Context) (id string, wrapped []byte, err error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return "", nil, fmt.Errorf("generating a data key: %w", err)
	}
	wrapped, err = e.kek.Wrap(ctx, dek)
	if err != nil {
		return "", nil, fmt.Errorf("wrapping the data key with %s: %w", e.kek.URI(), err)
	}
	id = keyID(dek)
	if err := e.store.PutDEK(id, wrapped, e.kek.URI()); err != nil {
		return "", nil, fmt.Errorf("storing the wrapped data key: %w", err)
	}
	return id, wrapped, nil
}

// aead returns the cipher for one generation, unwrapping through the KMS on a
// cache miss.
func (e *envelope) aead(ctx context.Context, id string) (cipher.AEAD, error) {
	e.mu.Lock()
	if c, ok := e.cache[id]; ok && time.Now().Before(c.expires) {
		e.mu.Unlock()
		return c.gcm, nil
	}
	e.mu.Unlock()

	wrapped, ok, err := e.store.DEK(id)
	if err != nil {
		return nil, fmt.Errorf("reading data key %s: %w", id, err)
	}
	if !ok {
		return nil, fmt.Errorf("this record was written under data key %s, which this install has no wrapped copy of", id)
	}
	return e.unwrap(ctx, id, wrapped)
}

func (e *envelope) unwrap(ctx context.Context, id string, wrapped []byte) (cipher.AEAD, error) {
	dek, err := e.kek.Unwrap(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrapping data key %s with %s: %w", id, e.kek.URI(), err)
	}
	if len(dek) != 32 {
		return nil, fmt.Errorf("data key %s unwrapped to %d bytes, want 32", id, len(dek))
	}
	// The id is the fingerprint of the key material, so a KMS that returned the
	// wrong key is caught here rather than as an authentication failure on
	// every record it touches.
	if got := keyID(dek); got != id {
		return nil, fmt.Errorf("data key %s unwrapped to key %s — the KMS returned a different key", id, got)
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	e.mu.Lock()
	e.cache[id] = cachedKey{gcm: gcm, expires: time.Now().Add(e.ttl)}
	e.mu.Unlock()
	return gcm, nil
}

// FakeKEK wraps with a fixed key held in memory. It exists so the whole managed
// path — generation, wrapping, storage, the cache, unwrap failure — is covered
// by tests that need no cloud account and no network, which is the difference
// between the managed backend being tested and being hoped about.
type FakeKEK struct {
	gcm cipher.AEAD
	uri string
	// Err, when set, is returned by every call. For the outage cases.
	Err error
	// Wraps and Unwraps count calls, which is how the cache is asserted.
	Wraps, Unwraps int
	mu             sync.Mutex
}

// NewFakeKEK builds one from any 32-byte hex key.
func NewFakeKEK(hexKey string) (*FakeKEK, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("fake KEK needs 64 hex characters")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &FakeKEK{gcm: gcm, uri: "fake-kms://" + keyID(raw)}, nil
}

func (f *FakeKEK) URI() string { return f.uri }

func (f *FakeKEK) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Wraps++
	if f.Err != nil {
		return nil, f.Err
	}
	nonce := make([]byte, f.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return f.gcm.Seal(nonce, nonce, dek, nil), nil
}

func (f *FakeKEK) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Unwraps++
	if f.Err != nil {
		return nil, f.Err
	}
	n := f.gcm.NonceSize()
	if len(wrapped) < n {
		return nil, fmt.Errorf("wrapped key too short")
	}
	return f.gcm.Open(nil, wrapped[:n], wrapped[n:], nil)
}

// MemoryDEKStore is the DEKStore a test uses. The real one is the platform
// database.
type MemoryDEKStore struct {
	mu       sync.Mutex
	active   string
	wrapped  map[string][]byte
	KEKURIs  map[string]string
	PutCalls int
}

func NewMemoryDEKStore() *MemoryDEKStore {
	return &MemoryDEKStore{wrapped: map[string][]byte{}, KEKURIs: map[string]string{}}
}

func (m *MemoryDEKStore) ActiveDEK() (string, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == "" {
		return "", nil, false, nil
	}
	return m.active, m.wrapped[m.active], true, nil
}

func (m *MemoryDEKStore) DEK(id string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.wrapped[id]
	return w, ok, nil
}

func (m *MemoryDEKStore) PutDEK(id string, wrapped []byte, kekURI string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PutCalls++
	m.wrapped[id] = wrapped
	m.KEKURIs[id] = kekURI
	m.active = id
	return nil
}

package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// AccessToken is a provider's bearer token and when it stops working.
type AccessToken struct {
	Value     string
	ExpiresAt time.Time
}

// TokenMargin is how long before its expiry a cached token stops being handed
// out. A token anyone is given — a transport about to upload, or an app through
// /app/tokens — always has at least this long left, so an SDK that refreshes
// five minutes before expiry never holds one Toft is about to replace.
const TokenMargin = 10 * time.Minute

// TokenCache holds minted tokens per credential and scope.
//
// The key is a digest of everything that decides what a token is — the host,
// the tenant, the client, a digest of the secret and the scope — so a rotated
// secret is a miss rather than a second place the old one keeps working, and
// the map never holds a secret in the clear. Sharing one token between every
// caller of one credential changes nothing about reach: they hold the same
// credential already.
type TokenCache struct {
	mu sync.Mutex
	m  map[string]AccessToken
	// now is a variable so a test can move time.
	now func() time.Time
}

// NewTokenCache returns an empty cache.
func NewTokenCache() *TokenCache {
	return &TokenCache{m: map[string]AccessToken{}, now: time.Now}
}

// TokenKey digests the parts that identify one credential and scope.
func TokenKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Get returns the cached token for key while it has more than TokenMargin left,
// and otherwise mints, stores and returns a new one. A failed mint is never
// cached: the next call asks again.
//
// Two callers that miss at once both mint. That costs one extra round trip
// against the provider and is cheaper to read than a single-flight group.
func (c *TokenCache) Get(ctx context.Context, key string,
	mint func(context.Context) (AccessToken, error)) (AccessToken, error) {

	c.mu.Lock()
	tok, ok := c.m[key]
	now := c.now()
	c.mu.Unlock()
	if ok && now.Add(TokenMargin).Before(tok.ExpiresAt) {
		return tok, nil
	}

	tok, err := mint(ctx)
	if err != nil {
		return AccessToken{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Prune what has lapsed, so a rotated secret's entry does not live forever.
	for k, v := range c.m {
		if !now.Before(v.ExpiresAt) {
			delete(c.m, k)
		}
	}
	c.m[key] = tok
	return tok, nil
}

// Reset empties the cache, for a test that stubs the provider.
func (c *TokenCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]AccessToken{}
}

// ExpiresIn turns a token endpoint's `expires_in` into a time. A response that
// omits it is taken at an hour, both providers' own figure.
func ExpiresIn(now time.Time, seconds int64) time.Time {
	if seconds <= 0 {
		seconds = 3600
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

// IssuedToken is what an app is handed by POST /api/v1/app/tokens/{kind}: the
// connection's own token, the resource it is for, and the folder the app was
// attached to.
type IssuedToken struct {
	AccessToken
	// Resource is the API the token is for, so an app's SDK can be pointed at it.
	Resource string
	// Folder is the attachment's destination, resolved through the connection's
	// default. **A hint, not a fence**: the token reaches the connection's whole
	// grant, and the folder only tells the app where it was meant to work.
	Folder TokenFolder
}

// TokenFolder names a folder in the provider's own ids. DriveID is Graph's and
// empty for Google Drive.
type TokenFolder struct {
	DriveID string `json:"drive_id,omitempty"`
	ItemID  string `json:"item_id"`
	Label   string `json:"label"`
}

// Issuer is a kind that can hand an app its connection's access token.
// Only a kind whose catalog entry has the token action may implement it, and a
// test in internal/api says the two agree.
type Issuer interface {
	Token(ctx context.Context, t Target) (IssuedToken, error)
	// CheckToken answers whether Token would refuse the token itself — a
	// *TokenTooWideError — without resolving a folder. The admin page reads it.
	CheckToken(ctx context.Context, t Target) error
}

// TokenTooWideError refuses a token that would let an app do more than reach
// files: a Graph token carries every application permission its Entra app
// holds, and a client-credentials request cannot ask for fewer.
type TokenTooWideError struct {
	// Roles are the permissions outside the allowlist, in the token's order.
	Roles []string
}

func (e *TokenTooWideError) Error() string {
	return "this connection's Entra app holds permissions beyond files (" +
		strings.Join(e.Roles, ", ") + "), so Toft will not hand its token to an app — " +
		"remove them from the app registration, or give this connection one of its own"
}

package authprovider

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Verifying an identity token somebody else was issued (ADR §157).
//
// Sign-in never does this: it reads claims from an id_token it received over a
// channel it authenticated itself (claimsFromIDToken says why that is enough).
// A token exchange is the opposite case — the token arrives from a caller, so
// its signature is the whole of the assurance, and it is checked against the
// issuer's published keys before a single claim is believed.

// jwksTTL is how long a fetched key set is believed. A token signed by a key
// not in the cached set refetches at once — that is how a rotation is
// followed — but at most once a minute per issuer, so a stream of tokens with
// invented key ids cannot make this install hammer the IdP.
const (
	jwksTTL          = time.Hour
	jwksRefetchFloor = time.Minute
	jwksMaxBytes     = 1 << 20
	// tokenLeeway absorbs clock skew between this install and the issuer.
	tokenLeeway = time.Minute
)

type jwksEntry struct {
	keys    map[string]crypto.PublicKey
	fetched time.Time
	// failed is when the last fetch ended in an error. The floor is the last
	// fetch, failed or not: no refetch is attempted before jwksRefetchFloor
	// has passed since either failed or fetched, so a stream of tokens naming
	// an unknown key id cannot refetch on every request after a fetch that
	// succeeded (security review 2026-11-15, LSA-1). A successful fetch clears
	// failed. Both are stamped when a fetch ends, never before it starts: a
	// stamp taken before the fetch refused every concurrent exchange on a cold
	// cache and let one cancelled request hold the floor for everybody (code
	// review 2026-11-11, A-2).
	failed time.Time
	// inflight is closed when the fetch that is running ends; callers that
	// arrive while it runs wait for it instead of being refused or fetching
	// again. The fetch runs under its own context, not the first caller's,
	// so a client dropping its connection does not fail the others' fetch.
	inflight chan struct{}
}

// jwksFetchTimeout bounds one key-set fetch, which runs detached from the
// request that triggered it.
const jwksFetchTimeout = 10 * time.Second

var (
	jwksMu    sync.Mutex
	jwksCache = map[string]*jwksEntry{}
)

// VerifiedToken is what a verified identity token says.
type VerifiedToken struct {
	Subject       string
	Email         string
	EmailVerified bool
	ExpiresAt     time.Time
	Claims        jwt.MapClaims
}

// PeekIssuer reads a token's `iss` without verifying anything, so the caller
// can find which trusted rows to verify it against. Nothing read here is
// believed until VerifyIDToken has checked the signature.
func PeekIssuer(raw string) string {
	claims := claimsFromIDToken(raw)
	if claims == nil {
		return ""
	}
	iss, _ := claims["iss"].(string)
	return strings.TrimSuffix(iss, "/")
}

// VerifyIDToken checks a token's signature against the issuer's published
// keys, and that it was issued by `issuer`, for `audience`, and has not
// expired. Asymmetric algorithms only: an HS256 token would be signed with a
// secret this install does not have and must never be asked to accept.
func VerifyIDToken(ctx context.Context, raw, issuer, audience string) (*VerifiedToken, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(tokenLeeway),
	)
	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return signingKey(ctx, issuer, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("the identity token was refused: %w", err)
	}
	// The issuer is compared exactly here rather than with WithIssuer, so a
	// trailing slash one side writes and the other does not is not a refusal.
	iss, _ := claims["iss"].(string)
	if strings.TrimSuffix(iss, "/") != issuer {
		return nil, fmt.Errorf("the identity token was issued by %q, not %q", iss, issuer)
	}
	out := &VerifiedToken{Claims: claims}
	out.Subject, _ = claims["sub"].(string)
	out.Email, _ = claims["email"].(string)
	out.EmailVerified = truthy(claims["email_verified"])
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		out.ExpiresAt = exp.Time
	}
	if out.Subject == "" {
		return nil, errors.New("the identity token names no subject")
	}
	return out, nil
}

// signingKey returns the issuer's key with this id, fetching the key set when
// it is not cached, has gone stale, or does not hold the id.
func signingKey(ctx context.Context, issuer, kid string) (crypto.PublicKey, error) {
	jwksMu.Lock()
	entry := jwksCache[issuer]
	if entry == nil {
		entry = &jwksEntry{}
		jwksCache[issuer] = entry
	}
	cached := pick(entry.keys, kid)
	if cached != nil && time.Since(entry.fetched) < jwksTTL {
		jwksMu.Unlock()
		return cached, nil
	}
	if entry.inflight == nil {
		if time.Since(entry.failed) < jwksRefetchFloor {
			jwksMu.Unlock()
			// Throttled after a failure. A stale set that still holds the key
			// is better than a refusal; otherwise say which of the two it was.
			if cached != nil {
				return cached, nil
			}
			if entry.keys == nil {
				return nil, errors.New("the issuer's signing keys could not be fetched a moment ago; try again shortly")
			}
			return nil, fmt.Errorf("the issuer publishes no signing key %q", kid)
		}
		// A set fetched inside the floor that does not hold the key id is
		// the answer until the floor passes. (cached is nil here: a cached
		// key inside the floor is inside the TTL and returned above.)
		if entry.keys != nil && time.Since(entry.fetched) < jwksRefetchFloor {
			jwksMu.Unlock()
			return nil, fmt.Errorf("the issuer publishes no signing key %q", kid)
		}
		done := make(chan struct{})
		entry.inflight = done
		go func() {
			fctx, cancel := context.WithTimeout(context.Background(), jwksFetchTimeout)
			defer cancel()
			keys, err := fetchJWKS(fctx, issuer)
			jwksMu.Lock()
			if err != nil {
				entry.failed = time.Now()
			} else {
				entry.keys, entry.fetched, entry.failed = keys, time.Now(), time.Time{}
			}
			entry.inflight = nil
			jwksMu.Unlock()
			close(done)
		}()
	}
	wait := entry.inflight
	jwksMu.Unlock()

	select {
	case <-wait:
	case <-ctx.Done():
		// This caller gave up; the fetch goes on for the others.
		if cached != nil {
			return cached, nil
		}
		return nil, ctx.Err()
	}
	jwksMu.Lock()
	keys, failed := entry.keys, !entry.failed.IsZero()
	jwksMu.Unlock()
	if k := pick(keys, kid); k != nil {
		return k, nil
	}
	if failed {
		if cached != nil {
			return cached, nil
		}
		return nil, errors.New("the issuer's signing keys could not be fetched; try again shortly")
	}
	return nil, fmt.Errorf("the issuer publishes no signing key %q", kid)
}

// pick finds a key by id. A token with no `kid` is accepted only when the set
// holds exactly one key — the case of an issuer that never rotates.
func pick(keys map[string]crypto.PublicKey, kid string) crypto.PublicKey {
	if kid != "" {
		return keys[kid]
	}
	if len(keys) == 1 {
		for _, k := range keys {
			return k
		}
	}
	return nil
}

// ForgetJWKS drops an issuer's cached keys, for tests and for an
// administrator's edit.
func ForgetJWKS(issuer string) {
	jwksMu.Lock()
	delete(jwksCache, strings.TrimSuffix(issuer, "/"))
	jwksMu.Unlock()
}

func fetchJWKS(ctx context.Context, issuer string) (map[string]crypto.PublicKey, error) {
	doc, err := Discover(ctx, issuer)
	if err != nil {
		return nil, err
	}
	if doc.JWKSURI == "" {
		return nil, fmt.Errorf("%s publishes no jwks_uri", issuer)
	}
	if err := checkHTTPSURL(doc.JWKSURI); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, doc.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", doc.JWKSURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", doc.JWKSURI, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBytes))
	if err != nil {
		return nil, err
	}
	return ParseJWKS(body)
}

// jwk is the subset of RFC 7517 a signature check needs.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseJWKS reads a key set's RSA and EC signing keys. Keys of other types,
// and keys marked for encryption, are skipped rather than refused, so one
// unfamiliar key does not take the set down with it.
func ParseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("the issuer's key set could not be read: %w", err)
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
				continue
			}
			out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			var curve elliptic.Curve
			switch k.Crv {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			case "P-521":
				curve = elliptic.P521()
			default:
				continue
			}
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !curve.IsOnCurve(pub.X, pub.Y) {
				continue
			}
			out[k.Kid] = pub
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the issuer's key set holds no signing key this install can use")
	}
	return out, nil
}

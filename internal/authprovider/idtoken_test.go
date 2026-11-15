package authprovider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIssuer is an OIDC issuer on loopback: a discovery document, a key set
// with one RSA key, and a signer for tokens under it.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, kid: "k1"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		e := big.NewInt(int64(key.E)).Bytes()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.kid, "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	})
	t.Cleanup(func() {
		f.srv.Close()
		ForgetDiscovery(f.srv.URL)
		ForgetJWKS(f.srv.URL)
	})
	return f
}

func (f *fakeIssuer) sign(t *testing.T, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fakeIssuer) claims(aud string, exp time.Duration) jwt.MapClaims {
	return jwt.MapClaims{"iss": f.srv.URL, "aud": aud, "sub": "alice-sub",
		"email": "alice@example.com", "email_verified": true, "exp": time.Now().Add(exp).Unix()}
}

func TestVerifyIDTokenAcceptsAValidToken(t *testing.T) {
	f := newFakeIssuer(t)
	raw := f.sign(t, f.claims("workbench", time.Hour), f.kid)
	if got := PeekIssuer(raw); got != f.srv.URL {
		t.Fatalf("PeekIssuer = %q, want %q", got, f.srv.URL)
	}
	v, err := VerifyIDToken(context.Background(), raw, f.srv.URL+"/", "workbench")
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if v.Subject != "alice-sub" || v.Email != "alice@example.com" || !v.EmailVerified {
		t.Fatalf("claims read wrong: %+v", v)
	}
}

// Every check is its own refusal: the audience, the expiry, the issuer, the
// signing key and the algorithm.
func TestVerifyIDTokenRefuses(t *testing.T) {
	f := newFakeIssuer(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims("workbench", time.Hour))
		tok.Header["kid"] = f.kid
		s, _ := tok.SignedString(other)
		return s
	}()
	hs := func() string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, f.claims("workbench", time.Hour)).SignedString([]byte("x"))
		return s
	}()
	wrongIss := f.claims("workbench", time.Hour)
	wrongIss["iss"] = "https://elsewhere.example"
	noSub := f.claims("workbench", time.Hour)
	delete(noSub, "sub")

	for name, raw := range map[string]string{
		"another audience":   f.sign(t, f.claims("someone-else", time.Hour), f.kid),
		"expired":            f.sign(t, f.claims("workbench", -10*time.Minute), f.kid),
		"another issuer":     f.sign(t, wrongIss, f.kid),
		"an unknown key id":  f.sign(t, f.claims("workbench", time.Hour), "k-unknown"),
		"a forged signature": forged,
		"a shared secret":    hs,
		"no subject":         f.sign(t, noSub, f.kid),
		"not a token":        "nonsense",
	} {
		if _, err := VerifyIDToken(context.Background(), raw, f.srv.URL, "workbench"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseJWKSSkipsWhatItCannotUse(t *testing.T) {
	keys, err := ParseJWKS([]byte(`{"keys":[{"kty":"oct","kid":"a","k":"x"},
		{"kty":"RSA","kid":"enc","use":"enc","n":"AQAB","e":"AQAB"},
		{"kty":"RSA","kid":"ok","n":"` + strings.Repeat("A", 342) + `","e":"AQAB"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys["ok"] == nil {
		t.Fatalf("keys = %v, want only the RSA signing key", keys)
	}
	if _, err := ParseJWKS([]byte(`{"keys":[]}`)); err == nil {
		t.Fatal("an empty key set was accepted")
	}
}

// A cold cache is filled once for everybody: concurrent exchanges wait for the
// one fetch rather than being refused as throttled, a caller that drops its
// connection does not fail the fetch for the rest, and a fetch that failed
// holds the floor only until the next one succeeds (code review 2026-11-11,
// A-2).
func TestConcurrentExchangesShareOneJWKSFetch(t *testing.T) {
	f := newFakeIssuer(t)
	var fetches atomic.Int32
	keysHandler := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/keys" {
			fetches.Add(1)
			time.Sleep(150 * time.Millisecond)
		}
		keysHandler.ServeHTTP(w, r)
	})
	raw := f.sign(t, f.claims("workbench", time.Hour), f.kid)

	// The first caller cancels at once; the fetch it started must still land.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyIDToken(cancelled, raw, f.srv.URL, "workbench"); err == nil {
		t.Fatal("a cancelled caller was answered")
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = VerifyIDToken(context.Background(), raw, f.srv.URL, "workbench")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d refused on a cold cache: %v", i, err)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the key set was fetched %d times, want once", n)
	}
}

// A token naming a key id the issuer does not publish refetches the key set at
// most once a minute, even after a fetch that succeeded: the floor is the last
// fetch, failed or not (security review 2026-11-15, LSA-1).
func TestUnknownKidRefetchesAtMostOnceAMinute(t *testing.T) {
	f := newFakeIssuer(t)
	var fetches atomic.Int32
	keysHandler := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/keys" {
			fetches.Add(1)
		}
		keysHandler.ServeHTTP(w, r)
	})
	good := f.sign(t, f.claims("workbench", time.Hour), f.kid)
	if _, err := VerifyIDToken(context.Background(), good, f.srv.URL, "workbench"); err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("the key set was fetched %d times, want once", n)
	}

	bad := f.sign(t, f.claims("workbench", time.Hour), "nope")
	for i := 0; i < 5; i++ {
		if _, err := VerifyIDToken(context.Background(), bad, f.srv.URL, "workbench"); err == nil {
			t.Fatalf("token %d with an unknown kid was accepted", i)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("unknown-kid tokens inside the floor fetched the key set %d times in all, want 1", n)
	}

	jwksMu.Lock()
	jwksCache[f.srv.URL].fetched = time.Now().Add(-2 * time.Minute)
	jwksMu.Unlock()
	if _, err := VerifyIDToken(context.Background(), bad, f.srv.URL, "workbench"); err == nil {
		t.Fatal("an unknown kid was accepted after the floor")
	}
	if n := fetches.Load(); n != 2 {
		t.Errorf("past the floor the key set was fetched %d times in all, want 2", n)
	}
}

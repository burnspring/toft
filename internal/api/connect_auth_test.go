package api

import (
	"bytes"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

// rsconnectClient signs requests the way rsconnect's signatureHeaders does for a
// private key: MD5 of the body, base64; RSA PKCS#1 v1.5 over SHA-1 of
// "METHOD\npath\ndate\nmd5", base64.
type rsconnectClient struct {
	token string
	key   *rsa.PrivateKey
}

func newRSConnectClient(t *testing.T) *rsconnectClient {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	rand.Read(id[:])
	return &rsconnectClient{token: "T" + hexOf(id[:]), key: key}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func (c *rsconnectClient) publicKey(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&c.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// signed builds a signed request; mutate lets a test break one part of it.
func (c *rsconnectClient) signed(t *testing.T, method, path string, body []byte, date time.Time) *http.Request {
	t.Helper()
	sum := md5.Sum(body)
	checksum := base64.StdEncoding.EncodeToString(sum[:])
	d := date.UTC().Format(http.TimeFormat)
	u, _ := url.Parse(path)
	digest := sha1.Sum([]byte(method + "\n" + u.Path + "\n" + d + "\n" + checksum))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA1, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Date", d)
	req.Header.Set("X-Auth-Token", c.token)
	req.Header.Set("X-Auth-Signature", base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("X-Content-Checksum", checksum)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func serve(router http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// register makes rsconnect's POST /__api__/tokens and returns the user code in
// the claim URL.
func (c *rsconnectClient) register(t *testing.T, router http.Handler) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": c.token, "public_key": c.publicKey(t), "user_id": 0})
	req := httptest.NewRequest(http.MethodPost, "/__api__/tokens", bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", "anonymous-access")
	req.Header.Set("User-Agent", "rsconnect/1.11.2 R/4.5.3")
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ClaimURL string `json:"token_claim_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	u, err := url.Parse(out.ClaimURL)
	if err != nil || u.Path != "/cli" || u.Query().Get("code") == "" {
		t.Fatalf("claim url = %q", out.ClaimURL)
	}
	return u.Query().Get("code")
}

// The whole of RStudio's Add account: register, poll unapproved, approve in a
// browser session, poll again — and from then on the token publishes as the
// person who approved it, through an ordinary personal key.
func TestRSConnectTokenSignsInThroughTheApprovalPage(t *testing.T) {
	d := newDeviceEnv(t)
	c := newRSConnectClient(t)
	code := c.register(t, d.router)

	if rec := serve(d.router, c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unapproved poll = %d, want 401 (rsconnect keeps polling on it)", rec.Code)
	}

	// The approval page knows who is asking.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/cli/device?user_code="+code, nil)
	req.AddCookie(sessionCookieFor(t, d.srv, "alice"))
	rec := serve(d.router, req)
	var view deviceRequestView
	json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Client != database.DeviceClientRSConnect || view.RequestedFrom == "" ||
		!strings.HasPrefix(view.ClientVersion, "rsconnect/") {
		t.Fatalf("approval view = %+v", view)
	}

	if rec := d.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": code, "approve": true}); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}

	rec = serve(d.router, c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now()))
	if rec.Code != http.StatusOK {
		t.Fatalf("approved poll = %d: %s", rec.Code, rec.Body.String())
	}
	var me map[string]any
	json.Unmarshal(rec.Body.Bytes(), &me)
	if me["username"] != "alice" {
		t.Fatalf("users/current = %v", me)
	}

	keys, _ := d.srv.db.ListUserAPIKeys("alice", 1)
	if len(keys) != 1 || keys[0].Name != "RStudio (rsconnect)" {
		t.Fatalf("keys = %+v, want one named for rsconnect", keys)
	}

	// A signed write, body and all.
	body, _ := json.Marshal(map[string]string{"name": "signed-app"})
	rec = serve(d.router, c.signed(t, http.MethodPost, "/__api__/v1/content", body, time.Now()))
	if rec.Code != http.StatusOK {
		t.Fatalf("signed create = %d: %s", rec.Code, rec.Body.String())
	}

	// Revoking the key signs RStudio out.
	if err := d.srv.db.DeleteUserAPIKeyTx(keys[0].ID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "alice", Action: "user.api_key_delete"}); err != nil {
		t.Fatal(err)
	}
	if rec := serve(d.router, c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke = %d, want 401", rec.Code)
	}
}

// Each part of the signature is load-bearing.
func TestRSConnectSignatureRefusals(t *testing.T) {
	d := newDeviceEnv(t)
	c := newRSConnectClient(t)
	code := c.register(t, d.router)
	d.post(t, "alice", "/api/v1/auth/cli/device/approve", map[string]any{"user_code": code, "approve": true})
	if rec := serve(d.router, c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())); rec.Code != http.StatusOK {
		t.Fatalf("baseline = %d: %s", rec.Code, rec.Body.String())
	}

	stranger := newRSConnectClient(t)
	stranger.token = c.token // the right token, the wrong private key
	if rec := serve(d.router, stranger.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key = %d, want 401", rec.Code)
	}

	if rec := serve(d.router, c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now().Add(-10*time.Minute))); rec.Code != http.StatusUnauthorized {
		t.Errorf("stale Date = %d, want 401", rec.Code)
	}

	req := c.signed(t, http.MethodPost, "/__api__/v1/content", []byte(`{"name":"one"}`), time.Now())
	req.Body = io.NopCloser(strings.NewReader(`{"name":"two"}`))
	if rec := serve(d.router, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered body = %d, want 401", rec.Code)
	}

	// Signed for one path, sent to another.
	req = c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())
	req.URL.Path = "/__api__/applications"
	if rec := serve(d.router, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("path swapped = %d, want 401", rec.Code)
	}

	unknown := newRSConnectClient(t)
	if rec := serve(d.router, unknown.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())); rec.Code != http.StatusUnauthorized {
		t.Errorf("unregistered token = %d, want 401", rec.Code)
	}
}

// readCounter is a request body that records how much of it was read.
type readCounter struct {
	r io.Reader
	n int
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// Anybody can register a token, so a request whose signature does not verify
// must not have its body read: spooling it first let an anonymous caller write
// up to 100MB to the apps volume per request (API-AUTH-1).
func TestRSConnectUnsignedBodyIsNeverSpooled(t *testing.T) {
	d := newDeviceEnv(t)
	c := newRSConnectClient(t)
	c.register(t, d.router)

	stranger := newRSConnectClient(t)
	stranger.token = c.token // a registered token, a signature nobody made with its key
	body := []byte(strings.Repeat("x", 1<<20))
	req := stranger.signed(t, http.MethodPost, "/__api__/v1/content", body, time.Now())
	counted := &readCounter{r: bytes.NewReader(body)}
	req.Body = io.NopCloser(counted)
	if rec := serve(d.router, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", rec.Code)
	}
	if counted.n != 0 {
		t.Errorf("read %d bytes of an unsigned body, want none", counted.n)
	}
}

// A signature proves only that the caller holds the key it registered, which
// anybody can do: a token nobody has approved is refused before its body is
// read, so it cannot spool to the apps volume.
func TestRSConnectUnapprovedTokenBodyIsNeverSpooled(t *testing.T) {
	d := newDeviceEnv(t)
	c := newRSConnectClient(t)
	c.register(t, d.router)

	body := []byte(strings.Repeat("x", 1<<20))
	req := c.signed(t, http.MethodPost, "/__api__/v1/content", body, time.Now())
	counted := &readCounter{r: bytes.NewReader(body)}
	req.Body = io.NopCloser(counted)
	if rec := serve(d.router, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unapproved token = %d, want 401", rec.Code)
	}
	if counted.n != 0 {
		t.Errorf("read %d bytes of an unapproved token's body, want none", counted.n)
	}
	entries, _ := os.ReadDir(filepath.Join(d.srv.cfg.AppsDir, ".connect-spool"))
	if len(entries) != 0 {
		t.Errorf("spool holds %d files, want none", len(entries))
	}
}

// A denied sign-in is over: the token never authenticates and is dropped.
func TestRSConnectDeniedTokenIsDropped(t *testing.T) {
	d := newDeviceEnv(t)
	c := newRSConnectClient(t)
	code := c.register(t, d.router)
	d.post(t, "alice", "/api/v1/auth/cli/device/approve", map[string]any{"user_code": code, "approve": false})
	if rec := serve(d.router, c.signed(t, http.MethodGet, "/__api__/users/current", nil, time.Now())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("denied = %d, want 401", rec.Code)
	}
	if tok, _ := d.srv.db.GetConnectToken(c.token); tok != nil {
		t.Fatalf("denied token still registered")
	}
}

// No credential starts a credential, and off means absent.
func TestRSConnectTokenRegistrationIsAnonymousOnly(t *testing.T) {
	d := newDeviceEnv(t)
	key := mintKey(t, d.srv, "alice", "k", nil, false)
	c := newRSConnectClient(t)
	body, _ := json.Marshal(map[string]any{"token": c.token, "public_key": c.publicKey(t)})
	req := httptest.NewRequest(http.MethodPost, "/__api__/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Key "+key)
	if rec := serve(d.router, req); rec.Code != http.StatusForbidden {
		t.Fatalf("registration with a key = %d, want 403", rec.Code)
	}

	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKIXPublicKey(&small.PublicKey)
	body, _ = json.Marshal(map[string]any{"token": c.token, "public_key": base64.StdEncoding.EncodeToString(der)})
	if rec := serve(d.router, httptest.NewRequest(http.MethodPost, "/__api__/tokens", bytes.NewReader(body))); rec.Code != http.StatusBadRequest {
		t.Fatalf("1024-bit key = %d, want 400", rec.Code)
	}

	enablePersonalKeys(t, d.srv, false)
	if rec := serve(d.router, httptest.NewRequest(http.MethodPost, "/__api__/tokens",
		strings.NewReader(`{"token":"`+c.token+`","public_key":"`+c.publicKey(t)+`"}`))); rec.Code != http.StatusNotFound {
		t.Fatalf("with personal keys off = %d, want 404", rec.Code)
	}
}

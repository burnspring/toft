package safefetch

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

func TestCheckURL(t *testing.T) {
	bad := map[string]string{
		"http://example.com/x":        "must be https",
		"ftp://example.com/x":         "must be https",
		"https://u:p@example.com/x":   "credentials",
		"https://":                    "has no host",
		"file:///etc/passwd":          "must be https",
		"https://example.com\x7f/bad": "not a URL",
	}
	for raw, want := range bad {
		err := CheckURL(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckURL(%q) = %v, want %q", raw, err, want)
		}
	}
	if err := CheckURL("https://github.com/acme/x"); err != nil {
		t.Errorf("a plain https URL must pass: %v", err)
	}
}

func TestPrivateAddr(t *testing.T) {
	// The whole point is the addresses a hostname can be made to resolve to,
	// so the table is addresses rather than names.
	private := []string{
		"127.0.0.1", "::1", // itself
		"169.254.169.254", // the cloud metadata service
		"10.0.0.5",        // RFC 1918
		"172.16.0.1",      // RFC 1918
		"192.168.1.1",     // RFC 1918
		"0.0.0.0",         // unspecified
		"fe80::1",         // link-local
		"fd00::1",         // unique local, which net.IP.IsPrivate covers
	}
	for _, s := range private {
		if !PrivateAddr(net.ParseIP(s)) {
			t.Errorf("PrivateAddr(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if PrivateAddr(net.ParseIP(s)) {
			t.Errorf("PrivateAddr(%s) = true, want false", s)
		}
	}
}

// The guard is what a name resolving inward cannot get past, which is why it
// runs at dial time and not over the URL string.
func TestGuardRefusesThisNetwork(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	// A mirror host that is deliberately *not* the test server's, so the
	// operator's exemption cannot be what lets this through.
	c := NewForTesting("mirror.example.com", tlsCfg, true, testTimeout)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := c.For(srv.URL).Do(req)
	if err == nil || !strings.Contains(err.Error(), "this network") {
		t.Fatalf("err = %v, want the address guard to refuse", err)
	}
}

// The exemption is one host, and it is the operator's own decision. A URL that
// merely arrived inside a document the mirror served gets the strict client.
func TestMirrorExemptionIsOneHost(t *testing.T) {
	c := New("https://mirror.internal/feed.json", testTimeout)
	if c.MirrorHost() != "mirror.internal" {
		t.Fatalf("MirrorHost() = %q", c.MirrorHost())
	}
	if c.For("https://mirror.internal/other") != c.permissive {
		t.Error("the configured host should get the permissive client")
	}
	if c.For("https://elsewhere.internal/x") != c.strict {
		t.Error("any other host must get the strict client")
	}
	// With nothing configured there is no exemption at all.
	plain := New("", testTimeout)
	if plain.For("https://anything/x") != plain.strict {
		t.Error("an unconfigured client must never hand out the permissive one")
	}
}

func TestCheckConfiguredKeepsTheSchemeRule(t *testing.T) {
	c := New("http://mirror.internal/feed.json", testTimeout)
	// The mirror exemption covers the *address*, never the scheme: a document
	// fetched over http could be rewritten in flight.
	if err := c.CheckConfigured("http://mirror.internal/feed.json"); err == nil ||
		!strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want the scheme refused even for the mirror", err)
	}
	ok := New("https://mirror.internal/feed.json", testTimeout)
	if err := ok.CheckConfigured("https://mirror.internal/feed.json"); err != nil {
		t.Errorf("the configured https mirror must pass: %v", err)
	}
}

func TestRedirectsAreRecheckedAndCapped(t *testing.T) {
	t.Run("leaving https is refused", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		}))
		defer srv.Close()
		if err := get(t, srv.URL); err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("err = %v, want the hop refused for leaving https", err)
		}
	})

	t.Run("a loop hits the cap", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
		}))
		defer srv.Close()
		if err := get(t, srv.URL); err == nil || !strings.Contains(err.Error(), "redirects") {
			t.Fatalf("err = %v, want the redirect cap", err)
		}
	})
}

func get(t *testing.T, rawURL string) error {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	c := NewForTesting(u.Hostname(), tlsCfg, false, testTimeout)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	resp, err := c.For(rawURL).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

func TestReadLimited(t *testing.T) {
	// The limit is on the stream, not on Content-Length, because a response
	// streamed without one is the case a header check misses.
	if _, err := ReadLimited(strings.NewReader(strings.Repeat("x", 100)), 10, "snapshot"); err == nil ||
		!strings.Contains(err.Error(), "snapshot is larger") {
		t.Fatalf("err = %v, want the noun named in the refusal", err)
	}
	body, err := ReadLimited(strings.NewReader("hello"), 10, "snapshot")
	if err != nil || string(body) != "hello" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	// Exactly at the limit is allowed; the reader takes max+1 to notice.
	if _, err := ReadLimited(strings.NewReader("0123456789"), 10, "snapshot"); err != nil {
		t.Errorf("a body exactly at the limit must pass: %v", err)
	}
}

func TestCopyLimited(t *testing.T) {
	if _, err := CopyLimited(io.Discard, strings.NewReader(strings.Repeat("x", 100)), 10, "file"); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want a size refusal", err)
	}
	n, err := CopyLimited(io.Discard, strings.NewReader("hello"), 10, "file")
	if err != nil || n != 5 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
}

// The operator's egress proxy is reachable even when it is on this network,
// because a dial to it is not a fetch of a URL somebody else chose. Nothing
// else on that network moves.
func TestGuardExemptsTheConfiguredProxy(t *testing.T) {
	original := proxyDialTargets
	t.Cleanup(func() { proxyDialTargets = original })
	proxyDialTargets = func() map[string]bool { return map[string]bool{"10.0.0.9:3128": true} }

	if err := dialGuard("tcp", "10.0.0.9:3128", nil); err != nil {
		t.Errorf("the configured proxy was refused: %v", err)
	}
	// Same host, different port: not the proxy, so not exempt.
	if err := dialGuard("tcp", "10.0.0.9:8080", nil); err == nil {
		t.Error("a private address that is not the proxy was allowed")
	}
	if err := dialGuard("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("the metadata service was allowed")
	}
}

func TestResolveProxyTargetsReadsTheEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")
	t.Setenv("https_proxy", "127.0.0.1")
	t.Setenv("HTTPS_PROXY", "")

	targets := resolveProxyTargets()
	for _, want := range []string{"127.0.0.1:3128", "127.0.0.1:80"} {
		if !targets[want] {
			t.Errorf("%s missing from %v", want, targets)
		}
	}
}

// The save-time half: an inward address is refused by name, as one sentence,
// and the sentinel is still what the error is.
func TestCheckAddressRefusesThisNetworkAtSave(t *testing.T) {
	for raw, want := range map[string]string{
		"https://10.0.0.5/hook?sig=x":   "points at 10.0.0.5 on this network",
		"https://localhost/hook":        "points at localhost, which resolves to 127.0.0.1, on this network",
		"https://169.254.169.254/x":     "points at 169.254.169.254 on this network",
		"http://example.com/plain-http": "must be https",
	} {
		err := CheckAddress(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckAddress(%q) = %v, want %q", raw, err, want)
			continue
		}
		if strings.HasPrefix(raw, "https") && !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("CheckAddress(%q) is not ErrPrivateAddress: %v", raw, err)
		}
		if strings.Contains(err.Error(), "sig=") {
			t.Errorf("the refusal repeats the query string: %v", err)
		}
	}
	if err := CheckAddress("https://203.0.113.9/hook"); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
	if err := CheckAddress("https://not-yet.invalid/hook"); err != nil {
		t.Errorf("a name that does not resolve yet is refused at save: %v", err)
	}
}

// The mirror exemption is the mirror's host, not "wherever the mirror sends
// you": a redirect from it to another host on this network meets the guard.
func TestMirrorRedirectOffTheMirrorIsGuarded(t *testing.T) {
	var reached bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		io.WriteString(w, "internal")
	}))
	defer target.Close()
	tu, _ := url.Parse(target.URL)
	// "localhost" is a different host from the mirror's 127.0.0.1, and
	// resolves onto loopback.
	elsewhere := "https://localhost:" + tu.Port() + "/secret"

	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			io.WriteString(w, "mirror")
			return
		}
		http.Redirect(w, r, elsewhere, http.StatusFound)
	}))
	defer mirror.Close()
	mu, _ := url.Parse(mirror.URL)

	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	c := NewForTesting(mu.Hostname(), tlsCfg, true, testTimeout)

	// The mirror itself is reachable, with the guard on.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, mirror.URL+"/same", nil)
	resp, err := c.For(mirror.URL).Do(req)
	if err != nil {
		t.Fatalf("the configured mirror must be reachable: %v", err)
	}
	resp.Body.Close()

	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, mirror.URL+"/go", nil)
	resp, err = c.For(mirror.URL).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !RefusedAddress(err) {
		t.Fatalf("err = %v, want the redirect target refused by the guard", err)
	}
	if reached {
		t.Fatal("the redirect target was reached")
	}
}

// A header the caller set — a mirror credential — is not re-sent to another
// host a redirect names; on the same host it is.
func TestCrossHostRedirectDropsCallerHeaders(t *testing.T) {
	got := map[string]string{}
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Path] = r.Header.Get("X-Api-Key") + "|" + r.Header.Get("Accept")
	}))
	defer target.Close()
	tu, _ := url.Parse(target.URL)
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, target.URL+"/kept", http.StatusFound)
			return
		}
		http.Redirect(w, r, "https://localhost:"+tu.Port()+"/dropped", http.StatusFound)
	}))
	defer src.Close()

	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	c := NewForTesting("", tlsCfg, false, testTimeout)
	for _, path := range []string{"/same", "/other"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, src.URL+path, nil)
		req.Header.Set("X-Api-Key", "s3cret")
		req.Header.Set("Accept", "application/json")
		resp, err := c.For(src.URL).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
	}
	if got["/kept"] != "s3cret|application/json" {
		t.Errorf("same-host hop = %q, want the headers kept", got["/kept"])
	}
	if got["/dropped"] != "|application/json" {
		t.Errorf("cross-host hop = %q, want the credential dropped and Accept kept", got["/dropped"])
	}
}

func TestPrivateAddrCoversTheSpecialRanges(t *testing.T) {
	for _, s := range []string{"100.64.0.1", "100.100.100.100", "100.127.255.254",
		"192.0.0.8", "198.18.0.1", "198.19.255.1", "224.0.0.1", "ff02::1"} {
		if !PrivateAddr(net.ParseIP(s)) {
			t.Errorf("PrivateAddr(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"100.63.255.255", "100.128.0.1", "192.0.2.1", "198.20.0.1"} {
		if PrivateAddr(net.ParseIP(s)) {
			t.Errorf("PrivateAddr(%s) = true, want false", s)
		}
	}
}

package gallery

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"toft/internal/safefetch"
)

// testFetcher builds a fetcher that trusts a test server's certificate and
// keeps every other rule in force — the redirect cap, the size limits, and
// (when guard is true) the refusal to reach anything on this network.
//
// guard is false in most tests for the obvious reason: a test server is on
// loopback, which is exactly what the guard exists to refuse. The one test that
// wants the guard turns it on and expects to be refused.
func testFetcher(t *testing.T, srv *httptest.Server, guard bool) *Fetcher {
	t.Helper()
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	return &Fetcher{
		client: safefetch.NewForTesting(u.Hostname(), tlsCfg, guard, fetchTimeout),
	}
}

func TestFetchCatalogReadsADocument(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		io.WriteString(w, `{"schema_version":1,"items":[]}`)
	}))
	defer srv.Close()

	res, err := testFetcher(t, srv, false).FetchCatalog(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	if res.NotModified || res.ETag != `"abc"` || !strings.Contains(string(res.Body), "schema_version") {
		t.Fatalf("res = %+v", res)
	}
}

func TestFetchCatalogSendsAndHonoursTheETag(t *testing.T) {
	var seen string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	res, err := testFetcher(t, srv, false).FetchCatalog(context.Background(), srv.URL, `"abc"`)
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	if seen != `"abc"` {
		t.Errorf("If-None-Match = %q", seen)
	}
	if !res.NotModified || len(res.Body) != 0 {
		t.Errorf("a 304 must carry no body: %+v", res)
	}
}

func TestFetchCatalogFailsOnAnErrorStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testFetcher(t, srv, false).FetchCatalog(context.Background(), srv.URL, ""); err == nil {
		t.Error("expected an error for a 500")
	}
}

func TestFetchCatalogRefusesAnOversizeDocument(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Streamed without a Content-Length, which is the case a limit on the
		// header alone would miss.
		chunk := strings.Repeat("x", 64<<10)
		for written := 0; written < MaxCatalogBytes+(128<<10); written += len(chunk) {
			io.WriteString(w, chunk)
		}
	}))
	defer srv.Close()

	_, err := testFetcher(t, srv, false).FetchCatalog(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want a size refusal", err)
	}
}

func TestFetchRefusesARedirectOffHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/catalog.json", http.StatusFound)
	}))
	defer srv.Close()

	_, err := testFetcher(t, srv, false).FetchCatalog(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want the redirect refused for leaving https", err)
	}
}

func TestFetchRefusesARedirectLoop(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	defer srv.Close()

	_, err := testFetcher(t, srv, false).FetchCatalog(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("err = %v, want the redirect cap", err)
	}
}

// The guard is the reason a catalog cannot make this server probe its own
// network. A test server is on loopback, so turning the guard on and asking for
// it is exactly the refusal being tested.
func TestGuardedClientRefusesThisNetwork(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema_version":1,"items":[]}`)
	}))
	defer srv.Close()

	// Not the mirror host: that exemption is the operator's own decision and
	// must not apply to a URL the catalog named.
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	f := &Fetcher{client: safefetch.NewForTesting("catalog.example.com", tlsCfg, true, fetchTimeout)}

	_, err := f.FetchTo(context.Background(), srv.URL, MaxZipBytes, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "this network") {
		t.Fatalf("err = %v, want the address guard to refuse", err)
	}
}

func TestFetchToRefusesAnOversizeFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	_, err := testFetcher(t, srv, false).FetchTo(context.Background(), srv.URL, 1024, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want a size refusal", err)
	}
}

func TestCheckSourceURL(t *testing.T) {
	bad := map[string]string{
		"http://example.com/x":        "must be https",
		"ftp://example.com/x":         "must be https",
		"https://u:p@example.com/x":   "credentials",
		"https://":                    "has no host",
		"file:///etc/passwd":          "must be https",
		"https://example.com\x7f/bad": "not a URL",
	}
	for raw, want := range bad {
		err := CheckSourceURL(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSourceURL(%q) = %v, want %q", raw, err, want)
		}
	}
	if err := CheckSourceURL("https://github.com/acme/x"); err != nil {
		t.Errorf("a plain https URL must pass: %v", err)
	}
}

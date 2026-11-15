package spa

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var testTree = fstest.MapFS{
	"index.html":          {Data: []byte("<!doctype html><title>Toft</title>" + strings.Repeat(" ", 2048))},
	"assets/index-abc.js": {Data: []byte(strings.Repeat("console.log('toft');", 500))},
	"assets/font.woff2":   {Data: []byte("wOF2 not compressible")},
}

func serve(t *testing.T, target, acceptEncoding string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	spaFileServer(http.FS(testTree)).ServeHTTP(rec, req)
	return rec.Result()
}

func gunzip(t *testing.T, r io.Reader) string {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHashedAssetIsGzippedAndImmutable(t *testing.T) {
	for range 2 { // the second request is answered from the cache
		res := serve(t, "/assets/index-abc.js", "br, gzip;q=0.8")
		if res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
		if got := res.Header.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if got := res.Header.Get("Cache-Control"); got != cacheImmutable {
			t.Fatalf("Cache-Control = %q", got)
		}
		if got := res.Header.Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("Vary = %q", got)
		}
		if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
			t.Fatalf("Content-Type = %q", res.Header.Get("Content-Type"))
		}
		if body := gunzip(t, res.Body); body != string(testTree["assets/index-abc.js"].Data) {
			t.Fatal("decompressed body differs from the file")
		}
	}
}

func TestNoGzipWhenNotAccepted(t *testing.T) {
	for _, ae := range []string{"", "br", "gzip;q=0", "identity"} {
		res := serve(t, "/assets/index-abc.js", ae)
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want none", ae, got)
		}
		b, _ := io.ReadAll(res.Body)
		if string(b) != string(testTree["assets/index-abc.js"].Data) {
			t.Errorf("Accept-Encoding %q: body differs from the file", ae)
		}
	}
}

func TestFontIsNotRecompressed(t *testing.T) {
	res := serve(t, "/assets/font.woff2", "gzip")
	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none", got)
	}
	if got := res.Header.Get("Cache-Control"); got != cacheImmutable {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestClientRouteGetsIndexRevalidated(t *testing.T) {
	for _, target := range []string{"/", "/login", "/apps/1b4e28ba-2fa1-11d2-883f-0016d3cca427/logs"} {
		res := serve(t, target, "gzip")
		if res.StatusCode != 200 {
			t.Fatalf("%s: status %d", target, res.StatusCode)
		}
		if got := res.Header.Get("Cache-Control"); got != cacheRevalidate {
			t.Errorf("%s: Cache-Control = %q, want %q", target, got, cacheRevalidate)
		}
		if body := gunzip(t, res.Body); !strings.Contains(body, "<title>Toft</title>") {
			t.Errorf("%s: not index.html", target)
		}
	}
}

func TestAPIPathIsNotTheSPA(t *testing.T) {
	if res := serve(t, "/api/v1/nope", "gzip"); res.StatusCode != 404 {
		t.Fatalf("status %d, want 404", res.StatusCode)
	}
}

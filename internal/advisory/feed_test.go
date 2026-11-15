package advisory

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"toft/internal/safefetch"
)

// The archive is spooled where the fetcher is told to — APPS_DIR's staging
// directory in production — and never into TMPDIR, which is a small tmpfs on
// some hosts and fails every large ecosystem's sync there.
func TestFetchSpoolsIntoTheScratchDirectory(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "all.zip")
	writeZip(t, archive, map[string]string{
		"GHSA-good.json": `{"id":"GHSA-good","affected":[{"package":{"ecosystem":"npm","name":"lodash"},
		  "ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]}`,
	})
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's own cert
	client := safefetch.NewForTesting(u.Hostname(), tlsCfg, false, 5*time.Second)

	missing := filepath.Join(t.TempDir(), "not-there")
	f := NewFetcherWithClient(client, "")
	f.scratch = missing
	if _, err := f.Fetch(context.Background(), srv.URL, EcosystemNPM, ""); err == nil ||
		!strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v, want the spool to be attempted in %s", err, missing)
	}

	scratch := t.TempDir()
	f.scratch = scratch
	snap, err := f.Fetch(context.Background(), srv.URL, EcosystemNPM, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Records) != 1 {
		t.Errorf("Records = %+v", snap.Records)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("the spooled archive was left behind: %v", left)
	}
}

// A cut inside a multi-byte character is invalid UTF-8, which Postgres
// refuses — and one such record used to fail its whole ecosystem's sync.
func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("a", 399) + "—tail" // the em dash spans bytes 399..401
	got := truncate(s, 400)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got != strings.Repeat("a", 399)+"…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("a\x00b", 10); got != "ab" {
		t.Errorf("NUL survived: %q", got)
	}

	rec, _, err := ParseOSV([]byte(`{"id":"GHSA-x","summary":"`+strings.Repeat("é", 800)+`"}`), "osv")
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(rec.Summary) || len(rec.Summary) > maxSummary+len("…") {
		t.Errorf("summary is %d bytes, valid=%v", len(rec.Summary), utf8.ValidString(rec.Summary))
	}
}

// A reference URL is rendered as an href, so a feed cannot hand the UI a
// javascript: or data: link: only http(s) is taken, else osv.dev.
func TestAdvisoryURLTakesOnlyWebSchemes(t *testing.T) {
	rec, _, err := ParseOSV([]byte(`{"id":"GHSA-x","affected":[{"package":{"ecosystem":"npm","name":"a"},"versions":["1.0.0"]}],"references":[
		{"type":"ADVISORY","url":"javascript:alert(document.cookie)"},
		{"type":"WEB","url":"data:text/html,<script>x</script>"},
		{"type":"REPORT","url":"//no-scheme.example/x"}]}`), "osv")
	if err != nil {
		t.Fatal(err)
	}
	if rec.URL != "https://osv.dev/vulnerability/GHSA-x" {
		t.Errorf("URL = %q, want the osv.dev fallback", rec.URL)
	}
	rec, _, _ = ParseOSV([]byte(`{"id":"GHSA-y","affected":[{"package":{"ecosystem":"npm","name":"a"},"versions":["1.0.0"]}],"references":[
		{"type":"ADVISORY","url":"javascript:alert(1)"},
		{"type":"PACKAGE","url":"https://github.com/acme/x"}]}`), "osv")
	if rec.URL != "https://github.com/acme/x" {
		t.Errorf("URL = %q, want the first web reference", rec.URL)
	}
}

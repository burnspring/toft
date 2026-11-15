package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// The scan header, and both of its tests are ones whose failure is silent.
//
// Neither produces a wrong answer anybody sees. The first produces a quietly
// inflated usage figure on somebody else's app — with nothing to look at,
// because a usage row carries no cause. The second produces a viewer who can
// make their own traffic invisible, which is invisible by construction.

type scanFixture struct {
	srv    *Server
	router http.Handler
	scan   *database.App
	be     *backend
	// secret is the marked scanner account's personal key, and keyID its id.
	secret string
	keyID  string
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "scanner", database.SystemRoleViewer)
	enablePersonalKeys(t, srv, true)
	markScanner(t, srv, "scanner")

	f := &scanFixture{srv: srv, be: newBackend(t, nil)}
	f.scan = &database.App{
		ID: "app-scanned", Slug: "scanned", Name: "Scanned", OwnerID: "owner",
		Framework: "go", Port: database.Port(f.be.port), EndpointHost: "127.0.0.1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(f.scan); err != nil {
		t.Fatalf("create app: %v", err)
	}
	// A second app on the same backend port is not possible (apps.port is
	// unique per listening release), so the control app is the same one asked
	// twice — once marked and once not. What is compared is the delta.
	f.secret = mintKey(t, srv, "scanner", "scan-key", nil, false)
	f.keyID = "scan-key-scanner"
	f.router = srv.AppsRouter()
	return f
}

func (f *scanFixture) get(t *testing.T, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/apps/scanned/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *scanFixture) requests(t *testing.T, appID string) int64 {
	t.Helper()
	f.srv.usage.Flush()
	var n int64
	if err := f.srv.db.Conn().QueryRow(
		`SELECT COALESCE(SUM(requests), 0) FROM app_usage_daily WHERE app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("reading usage: %v", err)
	}
	return n
}

// Test 1. A scan-marked request does not appear in app_usage_daily, and an
// unmarked one on the same credential does — which is what makes the first
// assertion about the header rather than about the plumbing.
func TestScanHeaderIsNotUsage(t *testing.T) {
	f := newScanFixture(t)

	if rec := f.get(t, map[string]string{"X-API-Key": f.secret, scanHeader: "1"}); rec.Code != http.StatusOK {
		t.Fatalf("the scan = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.requests(t, f.scan.ID); got != 0 {
		t.Fatalf("a scan-marked request recorded %d usage requests — every scan would inflate the "+
			"owning team's read of who uses their app, for as long as the scan ran", got)
	}

	if rec := f.get(t, map[string]string{"X-API-Key": f.secret}); rec.Code != http.StatusOK {
		t.Fatalf("the unmarked request = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.requests(t, f.scan.ID); got != 1 {
		t.Errorf("the same key unmarked recorded %d requests, want 1 — without this the test above "+
			"would pass on an install where nothing counts at all", got)
	}
}

// The key's own traffic is untouched. That is the scanner's own record, on the
// credential the scanner holds, and it is where a scanner's volume belongs.
func TestScanIsStillTheKeysOwnTraffic(t *testing.T) {
	f := newScanFixture(t)

	if rec := f.get(t, map[string]string{"X-API-Key": f.secret, scanHeader: "1"}); rec.Code != http.StatusOK {
		t.Fatalf("the scan = %d, want 200", rec.Code)
	}
	f.srv.usage.Flush()

	var n int64
	if err := f.srv.db.Conn().QueryRow(
		`SELECT COALESCE(SUM(requests), 0) FROM api_key_usage_daily WHERE key_id = $1`,
		f.keyID).Scan(&n); err != nil {
		t.Fatalf("reading key usage: %v", err)
	}
	if n != 1 {
		t.Errorf("the key recorded %d requests, want 1 — the header suppresses the app's usage and "+
			"nothing else, and a scanner's own volume is on the credential it holds", n)
	}
}

// Test 2. The header is believed from the marked scanner account and from
// nothing else.
//
// The failure this prevents is a viewer who can make their own traffic
// invisible — a visitor to a public app setting one header and vanishing from
// the figures their app's owner reads.
func TestScanHeaderIsIgnoredFromEveryOtherCaller(t *testing.T) {
	f := newScanFixture(t)

	// An anonymous browser request to a public app, claiming to be a scan.
	if rec := f.get(t, map[string]string{scanHeader: "1"}); rec.Code != http.StatusOK {
		t.Fatalf("anonymous = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.requests(t, f.scan.ID); got != 1 {
		t.Errorf("an anonymous visitor claiming a scan recorded %d requests, want 1 — a viewer who "+
			"can make their own traffic uncounted is the failure this rule exists to prevent", got)
	}

	// A personal key is an identity, and an identity that can hide what it did
	// is a credential this platform does not issue.
	enablePersonalKeys(t, f.srv, true)
	userSecret := mintKey(t, f.srv, "owner", "laptop", nil, false)
	if rec := f.get(t, map[string]string{"X-API-Key": userSecret, scanHeader: "1"}); rec.Code != http.StatusOK {
		t.Fatalf("personal key = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.requests(t, f.scan.ID); got != 2 {
		t.Errorf("a personal key claiming a scan recorded a total of %d requests, want 2", got)
	}
}

// The header never reaches the workload, on any path — including the one that
// believes it. What an app receives must not include a platform header a
// visitor could have set.
func TestScanHeaderIsStrippedBeforeTheApp(t *testing.T) {
	f := newScanFixture(t)

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"the scanner's key, believed", map[string]string{"X-API-Key": f.secret, scanHeader: "1"}},
		{"anonymous, ignored", map[string]string{scanHeader: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := f.get(t, tc.headers); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := f.be.lastReq.Header.Get(scanHeader); got != "" {
				t.Errorf("the app received %s: %q — a platform header a visitor can set must not "+
					"survive the gateway", scanHeader, got)
			}
		})
	}
}

// `0` and `no` are not `1`. A header whose mere presence counted would make
// `X-Toft-Scan: 0` mean the opposite of what whoever sent it intended,
// and the consequence is a number nobody can see is wrong.
func TestScanHeaderNeedsItsValue(t *testing.T) {
	f := newScanFixture(t)

	if rec := f.get(t, map[string]string{"X-API-Key": f.secret, scanHeader: "0"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := f.requests(t, f.scan.ID); got != 1 {
		t.Errorf("X-Toft-Scan: 0 recorded %d requests, want 1 — presence is not the value", got)
	}
}

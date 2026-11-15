package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
)

// The scanner account. Every test here is about a failure that is silent: a
// scan that inflates somebody's usage, a viewer who can make their own traffic
// vanish, or a mark that quietly became a grant.

func markScanner(t *testing.T, srv *Server, who string) {
	t.Helper()
	if _, err := srv.db.SetScannerAccount(t.Context(), who, true, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin",
	}); err != nil {
		t.Fatalf("marking %s: %v", who, err)
	}
}

// newScannerFixture is the scan fixture with two accounts beside its owner: the
// scanner and an ordinary viewer, neither granted on the app, which is public.
func newScannerFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := newScanFixture(t)
	seedUser(t, f.srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, f.srv.db, "scanner", database.SystemRoleViewer)
	seedUser(t, f.srv.db, "viewer", database.SystemRoleViewer)
	enablePersonalKeys(t, f.srv, true)
	return f
}

func appSessionFor(t *testing.T, srv *Server, who string) string {
	t.Helper()
	return appSessionCookie + "=" + sessionCookieFor(t, srv, who).Value
}

// The scanner's personal key, scanning, is not usage; the same key without the
// header is; and the same header on anybody else's key is ignored.
func TestScanHeaderIsBelievedFromTheScannerAccountsKey(t *testing.T) {
	f := newScannerFixture(t)
	markScanner(t, f.srv, "scanner")
	scannerKey := mintKey(t, f.srv, "scanner", "zap", nil, false)
	viewerKey := mintKey(t, f.srv, "viewer", "laptop", nil, false)

	if rec := f.get(t, map[string]string{"X-API-Key": scannerKey, scanHeader: "1"}); rec.Code != http.StatusOK {
		t.Fatalf("the scan = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.requests(t, f.scan.ID); got != 0 {
		t.Fatalf("the scanner account's scan recorded %d usage requests, want 0", got)
	}
	f.get(t, map[string]string{"X-API-Key": scannerKey})
	if got := f.requests(t, f.scan.ID); got != 1 {
		t.Fatalf("the scanner's key unmarked recorded %d, want 1 — the mark is not a blanket exemption", got)
	}
	f.get(t, map[string]string{"X-API-Key": viewerKey, scanHeader: "1"})
	if got := f.requests(t, f.scan.ID); got != 2 {
		t.Errorf("an unmarked account claiming a scan recorded a total of %d, want 2 — a viewer "+
			"who can make their own traffic uncounted is the failure this rule exists to prevent", got)
	}
}

// The scanner in a browser — ZAP's SPA mode — is the same subtraction.
func TestScanHeaderIsBelievedFromTheScannerAccountsSession(t *testing.T) {
	f := newScannerFixture(t)
	markScanner(t, f.srv, "scanner")

	if rec := f.get(t, map[string]string{"Cookie": appSessionFor(t, f.srv, "scanner"), scanHeader: "1"}); rec.Code != http.StatusOK {
		t.Fatalf("the scan = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.requests(t, f.scan.ID); got != 0 {
		t.Fatalf("the scanner's session scan recorded %d usage requests, want 0", got)
	}
	f.get(t, map[string]string{"Cookie": appSessionFor(t, f.srv, "viewer"), scanHeader: "1"})
	if got := f.requests(t, f.scan.ID); got != 1 {
		t.Errorf("an unmarked session claiming a scan recorded %d, want 1", got)
	}
}

// A locked account's mark is inert rather than cleared, and the read is what
// keeps that true.
func TestALockedScannersMarkIsInert(t *testing.T) {
	f := newScannerFixture(t)
	markScanner(t, f.srv, "scanner")
	if ok, _ := f.srv.db.IsScannerAccount("scanner"); !ok {
		t.Fatal("the marked account does not read as a scanner")
	}
	if _, err := f.srv.db.LockUser(t.Context(), "scanner", database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin",
	}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if ok, _ := f.srv.db.IsScannerAccount("scanner"); ok {
		t.Error("a locked account still reads as a scanner")
	}
}

// **The mark never grants.** A marked viewer is admitted to the estate read and
// sees every app, and is refused a private app it holds no grant on exactly as
// it was before the mark.
func TestTheScannerMarkNeverGrants(t *testing.T) {
	f := newScannerFixture(t)
	private := &database.App{
		ID: "app-private", Slug: "private", Name: "Private", OwnerID: "owner",
		Framework: "go", Port: database.Port(f.be.port + 1), EndpointHost: "127.0.0.1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := f.srv.db.CreateApp(private); err != nil {
		t.Fatalf("create app: %v", err)
	}
	router := f.srv.Router(http.NotFoundHandler())

	// The scan fixture marks "scanner"; clear it, to see the read refused first.
	if _, err := f.srv.db.SetScannerAccount(t.Context(), "scanner", false, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin"}); err != nil {
		t.Fatal(err)
	}
	if rec := getAs(t, f.srv, router, "scanner", "/api/v1/admin/estate/targets"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unmarked viewer read the estate: %d, want 404", rec.Code)
	}
	markScanner(t, f.srv, "scanner")

	rec := getAs(t, f.srv, router, "scanner", "/api/v1/admin/estate/targets")
	if rec.Code != http.StatusOK {
		t.Fatalf("the scanner's estate read = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp estateTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	reach := map[string]bool{}
	for _, row := range resp.Targets {
		reach[row.Slug] = row.CallerCanReach
	}
	if _, ok := reach["private"]; !ok {
		t.Fatalf("the private app is missing from the scanner's estate — the reader never omits an app: %v", reach)
	}
	if reach["private"] || !reach["scanned"] {
		t.Errorf("caller_can_reach = %v, want private false and scanned (public) true", reach)
	}
	if resp.AppsReachableByCaller != 1 {
		t.Errorf("apps_reachable_by_caller = %d, want 1", resp.AppsReachableByCaller)
	}

	if got := getAs(t, f.srv, router, "viewer", "/api/v1/admin/estate/targets").Code; got != http.StatusNotFound {
		t.Errorf("an unmarked viewer's estate read = %d, want 404", got)
	}
	if got := getAs(t, f.srv, router, "scanner", "/api/v1/apps/app-private").Code; got != http.StatusNotFound {
		t.Errorf("the scanner opened a private app it holds no grant on: %d, want 404 — the mark became a grant", got)
	}
}

// Only an administrator sets it, never on themselves, and both changes are
// audited.
func TestOnlyAnAdministratorMarksAnotherAccount(t *testing.T) {
	f := newScannerFixture(t)
	router := f.srv.Router(http.NotFoundHandler())

	do := func(who, method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(sessionCookieFor(t, f.srv, who))
		req.Header.Set("Origin", "http://localhost:8080")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do("viewer", http.MethodPost, "/api/v1/users/scanner/scanner"); got != http.StatusForbidden {
		t.Errorf("a viewer marking an account = %d, want 403", got)
	}
	if got := do("admin", http.MethodPost, "/api/v1/users/admin/scanner"); got != http.StatusConflict {
		t.Errorf("an administrator marking themselves = %d, want 409", got)
	}
	if got := do("admin", http.MethodPost, "/api/v1/users/scanner/scanner"); got != http.StatusOK {
		t.Fatalf("marking = %d, want 200", got)
	}
	if got := do("admin", http.MethodDelete, "/api/v1/users/scanner/scanner"); got != http.StatusOK {
		t.Fatalf("clearing = %d, want 200", got)
	}
	var n int
	if err := f.srv.db.Conn().QueryRow(
		`SELECT count(*) FROM audit_log WHERE target_id = 'scanner' AND action IN ('user.scanner_set','user.scanner_cleared')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("audit rows = %d, want 2 (set and cleared)", n)
	}
}

// BB-73: the mark has to be readable where People reads the account — the
// person's record and the list — or the switch shows off and nobody can see,
// or take away, the estate read it grants.
func TestTheScannerMarkIsReadBackOnTheAccount(t *testing.T) {
	f := newScannerFixture(t)
	router := f.srv.Router(http.NotFoundHandler())

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(sessionCookieFor(t, f.srv, "admin"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	markScanner(t, f.srv, "scanner")
	var one struct {
		User struct {
			ScannerSince *string `json:"scanner_since"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(get("/api/v1/users/scanner")), &one); err != nil {
		t.Fatal(err)
	}
	if one.User.ScannerSince == nil {
		t.Error("GET users/{id} of a marked account carries no scanner_since")
	}
	var list []struct {
		ID           string  `json:"id"`
		ScannerSince *string `json:"scanner_since"`
	}
	if err := json.Unmarshal([]byte(get("/api/v1/users")), &list); err != nil {
		t.Fatal(err)
	}
	for _, u := range list {
		if (u.ID == "scanner") != (u.ScannerSince != nil) {
			t.Errorf("GET users: %s scanner_since = %v", u.ID, u.ScannerSince)
		}
	}
	if _, err := f.srv.db.SetScannerAccount(t.Context(), "scanner", false, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	one.User.ScannerSince = nil
	if err := json.Unmarshal([]byte(get("/api/v1/users/scanner")), &one); err != nil {
		t.Fatal(err)
	}
	if one.User.ScannerSince != nil {
		t.Error("a cleared mark still reads back")
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

// GET /api/v1/app/url — where an app is reached, for the links it sends out.
//
// The bug this answers (black-box TOFT-BB-001): an app had no way to learn its
// own address. The gateway hands it an internal Host, `toft.app` carried no
// URL, and a job has no request to read one from — so a nightly report could
// not link back to the app it came from.

func getAppURL(t *testing.T, router http.Handler, bearer string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, appURLPath, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

// The address is the apps origin plus the mount — the value GET /apps/{id}
// reports — and it is read at the call, so an install whose address moves is
// answered with the new one by an app that never restarted.
func TestAnAppIsToldItsAddressAsItIsNow(t *testing.T) {
	f := newSelfModeFixture(t, "shared", "shared")
	f.srv.cfg.AppsBaseURL = "https://apps.example.com"
	secret := f.mint(t, "tok-url", "")

	rec, body := getAppURL(t, f.router, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", appURLPath, rec.Code, rec.Body.String())
	}
	if body["public_url"] != "https://apps.example.com/apps/recon" || body["base_path"] != "/apps/recon" {
		t.Errorf("answer = %v, want the apps origin plus /apps/recon", body)
	}

	f.srv.cfg.AppsBaseURL = "https://applications.example.org"
	if _, body = getAppURL(t, f.router, secret); body["public_url"] != "https://applications.example.org/apps/recon" {
		t.Errorf("after the address moved: %v, want the new origin", body)
	}
}

// An app at the root of a hostname of its own is reached there, with no base
// path — the two halves describe one release.
func TestAnAppOnItsOwnHostnameIsToldThatHostname(t *testing.T) {
	f := newSelfModeFixture(t, "shared", "shared")
	f.srv.cfg.AppsBaseURL = "https://apps.example.com"
	f.app.ReleaseBasePath = strPtr("")
	if err := f.srv.db.SetAppRelease(f.app); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.db.SetAppHostname(f.app.ID, "", false, "recon.example.org", true, "system", "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	_, body := getAppURL(t, f.router, f.mint(t, "tok-host", ""))
	if body["public_url"] != "https://recon.example.org" || body["base_path"] != "" {
		t.Errorf("answer = %v, want https://recon.example.org and no base path", body)
	}

	// The run status page lives under the same prefix the gateway matches it
	// against, so status_url joined onto the address lands on the page.
	app, _ := f.srv.db.GetApp(f.app.ID)
	if got := runStatusPath(app, "r1"); got != "/__toft/runs/r1" {
		t.Errorf("status path on its own hostname = %q, want /__toft/runs/r1", got)
	}
}

// No address is a refusal the SDKs raise, never "" — which joined to a path is
// the relative link the route exists to replace.
func TestNoAddressIsARefusalNotAnEmptyString(t *testing.T) {
	f := newSelfModeFixture(t, "shared", "shared")
	f.srv.cfg.AppsBaseURL = ""
	rec, body := getAppURL(t, f.router, f.mint(t, "tok-none", ""))
	if rec.Code != http.StatusServiceUnavailable || body["code"] != "no_public_address" || body["hint"] == "" {
		t.Errorf("GET %s with no apps origin = %d %s, want 503 no_public_address with a hint",
			appURLPath, rec.Code, rec.Body.String())
	}
}

// A job reaches it with its run token: a report email is where a link to the
// app is most often written, and a job has no request to build one from. It
// stays the only read on the run token's list (ADR §108).
func TestAJobIsToldItsAppsAddress(t *testing.T) {
	f := newBootstrapFixture(t)
	f.srv.cfg.AppsBaseURL = "https://apps.example.com"
	run := &database.JobRun{AppID: f.app.ID, JobName: "report"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	rec, body := getAppURL(t, f.router, tok)
	if rec.Code != http.StatusOK || body["public_url"] != "https://apps.example.com/apps/reports" {
		t.Errorf("a running job asked for its address: %d %s", rec.Code, rec.Body.String())
	}
}

package sdk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// R reads a send back, as notify.status does in the other three SDKs. Without
// it an R app could send and never learn whether the message went (drills on
// trytoft, 2026-10-23).
func TestTheRSDKReadsASendBack(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/app/notify/email":
			io.WriteString(w, `{"send_id":"s-1","status":"queued"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/app/sends/"):
			asked = r.URL.Path
			io.WriteString(w, `{"send_id":"s-1","status":"failed","kind":"email","action":"send","error":"535 Authentication Credentials Invalid"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
s <- notify("email", to = list("a@example.com"), subject = "x", text = "y")
st <- notify_status(s)
cat(sprintf("status|%s|%s|%s|%s|%s\n", st$id, st$status, st$done, st$delivered, st$error))
e <- tryCatch({ notify_status(""); NULL }, toft_error = function(e) e)
cat(sprintf("empty|%s\n", e$code))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{"status|s-1|failed|TRUE|FALSE|535 Authentication Credentials Invalid", "empty|bad_request"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("R output lacks %q:\n%s", want, out)
		}
	}
	if asked != "/api/v1/app/sends/s-1" {
		t.Errorf("read %q, want the send by its id", asked)
	}
}

// An ordinary R call refuses a redirect rather than following it: libcurl
// drops Authorization on a cross-host hop but replays the act-for blob and the
// idempotency key, and the other three SDKs refuse it too.
func TestTheRSDKRefusesARedirectOnAnOrdinaryCall(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}
	followed := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		io.WriteString(w, `{"send_id":"s-1","status":"sent"}`)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
e <- tryCatch({ notify_status("s-1"); NULL }, toft_error = function(e) e)
cat(sprintf("refused|%s|%s\n", e$code, e$status))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "refused|unexpected_redirect|302") {
		t.Errorf("R output lacks the refusal:\n%s", out)
	}
	if followed {
		t.Error("the R SDK followed the redirect")
	}
}

// A lone address is sent as a one-item list: to = "a@x" reached the platform as
// a JSON string, which it refuses (2026-11-02).
func TestTheRSDKSendsALoneRecipientAsAList(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"send_id":"s-1","status":"queued"}`)
	}))
	defer srv.Close()
	cmd := exec.Command("Rscript", "-e", `for (f in list.files("R", full.names = TRUE)) source(f)
notify("email", to = "a@example.com", cc = c("b@example.com", "c@example.com"), subject = "x", text = "y")`)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{`"to":["a@example.com"]`, `"cc":["b@example.com","c@example.com"]`, `"subject":"x"`} {
		if !strings.Contains(got, want) {
			t.Errorf("body %s lacks %s", got, want)
		}
	}
}

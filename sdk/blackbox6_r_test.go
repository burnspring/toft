package sdk

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The R SDK's findings in the sixth 2026-11-03 black-box list, driven for
// real: off_platform for a missing TOFT_APP_TOKEN or TOFT_API_URL carries a
// hint in every call that reads them, as the storage forms did, and so does
// unexpected_redirect (BB-068); job_wait on a run that has already finished
// returns it at once, inside a request, with no timeout (BB-072).
func TestTheRSDKBlackBoxSixFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, !is.null(e$body$hint) && nzchar(e$body$hint), sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")

say("data", code(app_data_get()))
say("info", code(app_info()))
say("start", code(job_start("nightly")))
say("notify", code(notify("slack", text = "hi")))
Sys.setenv(TOFT_API_URL = "` + srv.URL + `", TOFT_APP_TOKEN = "tofta_test")
say("redirect", code(app_info()))
run <- structure(list(id = "r1", status = "cancelled"), class = "toft_run")
req <- list(HEADERS = c("x-toft-request-deadline" = format(Sys.time() + 300, "%Y-%m-%dT%H:%M:%SZ", tz = "UTC")))
say("wait-finished", tryCatch(job_wait(run, request = req)$status, toft_error = function(e) e$code))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TOFT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"data|off_platform/TRUE",
		"info|off_platform/TRUE",
		"start|off_platform/TRUE",
		"notify|off_platform/TRUE",
		"redirect|unexpected_redirect/TRUE",
		"wait-finished|cancelled",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

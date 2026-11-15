package sdk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// The R SDK's findings in the third 2026-11-03 black-box list, driven for
// real: a transport failure is a toft_error, uncoded and retryable, tried once
// more (BB-008); an object store's refusal of a run's output is coded
// output_unwritable (BB-008); NaN in any JSON body — a job's params, a run's
// output, a notification — is bad_request before any request (BB-009); a run
// carries done and its null fields (BB-022); and caller_can() refuses a role
// that is not one (BB-024).
func TestTheRSDKBlackBoxThreeFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		switch r.URL.Path {
		case "/store/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/store/down":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/api/v1/app/job-runs/r1":
			w.Write([]byte(`{"id":"r1","job":"j","status":"succeeded","params":{},"status_url":"https://apps.example.test/apps/demo/_toft/runs/r1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
DEFAULT_RETRY_AFTER <- 0.2
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, e$retryable, sep = "/"),
                                error = function(e) paste("raw:", class(e)[1]))
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")

api <- Sys.getenv("TOFT_API_URL")
Sys.setenv(TOFT_API_URL = "http://127.0.0.1:1")
started <- Sys.time()
say("unreachable", code(app_data_get()))
say("retried", as.numeric(difftime(Sys.time(), started, units = "secs")) >= 0.2)
Sys.setenv(TOFT_API_URL = api)

say("start-nan", code(job_start("free", params = list(x = NaN))))
say("notify-nan", code(notify("slack", text = "x", extra = NaN)))

run <- job_get("r1")
say("done", isTRUE(run$done), all(c("rerun_of", "scheduled_for", "failure_reason") %in% names(run)),
    is.null(run$rerun_of))
say("respond", grepl('"rerun_of":null', job_respond(run)$body, fixed = TRUE),
    grepl('"done"', job_respond(run)$body, fixed = TRUE))

say("can-bogus", code(caller_can(list(), "bogus")))
say("can-empty", code(caller_can(list(), "")))
say("can-superuser", code(caller_can(list(), "superuser")))
say("can-system", caller_can(list(), "system"))
say("can-viewer", caller_can(list(), "viewer"))
Sys.setenv(TOFT_API_URL = "")
say("storage-off", code(storage("x")), code(store("x")))
Sys.setenv(TOFT_API_URL = api)
say("verify-exported", "verify_role" %in% readLines("NAMESPACE"))

Sys.setenv(TOFT_RUN_ID = "r1", TOFT_JOB_NAME = "j", TOFT_JOB_TOKEN = "t")
Sys.setenv(TOFT_OUTPUT_URI = paste0(api, "/store/forbidden"))
say("out-nan", code(run_write_output(list(x = NaN), json = TRUE)))
say("out-403", code(run_write_output("x")))
Sys.setenv(TOFT_OUTPUT_URI = paste0(api, "/store/down"))
say("out-503", code(run_write_output("x")))
Sys.setenv(TOFT_OUTPUT_URI = "http://127.0.0.1:1/out")
say("out-unreachable", code(run_write_output("x")))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TOFT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"unreachable|/TRUE",
		"retried|TRUE",
		"start-nan|bad_request/FALSE",
		"notify-nan|bad_request/FALSE",
		"done|TRUE|TRUE|TRUE",
		"respond|TRUE|FALSE",
		"can-bogus|bad_request/FALSE",
		"can-empty|bad_request/FALSE",
		"can-superuser|bad_request/FALSE",
		"can-system|FALSE",
		"can-viewer|FALSE",
		"verify-exported|FALSE",
		// D-13: off the platform, as Node's.
		"storage-off|off_platform/FALSE|off_platform/FALSE",
		"out-nan|bad_request/FALSE",
		"out-403|output_unwritable/FALSE",
		"out-503|output_unwritable/TRUE",
		"out-unreachable|/TRUE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range requests {
		if strings.Contains(r, "NaN") || strings.Contains(r, "/runs") || strings.Contains(r, "/notify") {
			t.Errorf("a refused value reached the platform: %s", r)
		}
	}
}

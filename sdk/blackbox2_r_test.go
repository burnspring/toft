package sdk

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// The R SDK's findings in the second 2026-11-03 black-box report, driven for
// real: NaN and Inf in app data are refused (BB-001), output that is not JSON
// is not_json — an empty one too, since the third list's BB-022 (BB-002), notify's own arguments are
// checked before any request (BB-012, BB-025), mount_path(NULL) is bad_request
// with a sentence (BB-023), scheduled_for() is in the schedule's zone
// (BB-024), and a run call outside a job is off_platform as in the other SDKs.
func TestTheRSDKBlackBoxTwoFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/app/job-runs/notjson/output":
			w.Write([]byte("not json"))
		case "/api/v1/app/job-runs/empty/output":
			w.WriteHeader(200)
		case "/api/v1/app/job-runs/good/output":
			w.Write([]byte(`{"n":3}`))
		case "/api/v1/app/data":
			// The read app_data_update() makes first; only a GET is answered,
			// so a write that got through would show in requests below.
			if r.Method == http.MethodGet {
				w.Write([]byte(`{"data":{"x":1},"version":3}`))
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, e$retryable, length(conditionMessage(e)) == 1 && nzchar(conditionMessage(e)), sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")
run <- function(id) structure(list(id = id), class = "toft_run")

say("set-nan", code(app_data_set(list(x = NaN))))
say("set-inf", code(app_data_set(list(x = -Inf))))
say("set-nested-nan", code(app_data_set(list(a = list(b = c(1, NaN))))))
upd <- tryCatch(app_data_update(function(doc) { doc$x <- -Inf; doc }), toft_error = function(e) e)
say("update-inf", upd$code, grepl("app_data_update()", conditionMessage(upd), fixed = TRUE),
    grepl("app_data_set()", conditionMessage(upd), fixed = TRUE))
say("out-notjson", code(job_output(job_get_stub <- structure(list(id = "notjson"), class = "toft_run"), json = TRUE)))
say("out-empty", code(job_output(structure(list(id = "empty"), class = "toft_run"), json = TRUE)))
say("out-good", job_output(structure(list(id = "good"), class = "toft_run"), json = TRUE)$n)
say("slack-blank", code(notify("slack", text = "   ")))
say("slack-none", code(notify("slack")))
say("email-null", code(notify("email", to = NULL, subject = "s", text = "t")))
say("email-num", code(notify("email", to = 5, subject = "s", text = "t")))
say("email-empty", code(notify("email", to = character(0), subject = "s", text = "t")))
say("email-nosubject", code(notify("email", to = "a@example.com", text = "t")))
say("mount-null", code(mount_path(NULL)))
say("progress-off", code(run_progress(10)))
say("output-off", code(run_write_output("x")))

Sys.setenv(TOFT_SCHEDULED_FOR = "2026-11-03T22:15:00-05:00")
at <- scheduled_for()
say("sched", format(at, "%Y-%m-%dT%H:%M:%S%z"), as.numeric(at))
Sys.setenv(TOFT_SCHEDULED_FOR = "2026-11-03T22:15:00+05:30")
at <- scheduled_for()
say("sched-half", format(at, "%Y-%m-%dT%H:%M:%S%z", tz = "UTC"), attr(at, "text"))
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
		"set-nan|bad_request/FALSE/TRUE",
		// drills 2026-11-03: the update's refusal names the call the app made.
		"update-inf|bad_request|TRUE|FALSE",
		"set-inf|bad_request/FALSE/TRUE",
		"set-nested-nan|bad_request/FALSE/TRUE",
		"out-notjson|not_json/FALSE/TRUE",
		// BB-022 (third list): an output written empty is not JSON.
		"out-empty|not_json/FALSE/TRUE",
		"out-good|3",
		"slack-blank|bad_request/FALSE/TRUE",
		"slack-none|bad_request/FALSE/TRUE",
		"email-null|bad_request/FALSE/TRUE",
		"email-num|bad_request/FALSE/TRUE",
		"email-empty|no_recipient/FALSE/TRUE",
		"email-nosubject|bad_request/FALSE/TRUE",
		"mount-null|bad_request/FALSE/TRUE",
		"progress-off|off_platform/FALSE/TRUE",
		"output-off|off_platform/FALSE/TRUE",
		"sched|2026-11-03T22:15:00-0500|1793762100",
		"sched-half|2026-11-03T16:45:00+0000|2026-11-03T22:15:00+05:30",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, req := range requests {
		if !strings.Contains(req, "/job-runs/") && req != "GET /api/v1/app/data" {
			t.Errorf("a refused call reached the platform: %s", req)
		}
	}
}

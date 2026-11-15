package sdk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// The R SDK's jobs surface, driven for real against a fake platform, for the
// reason store_r_test.go gives: R has no test runner here. R used to have
// job_start returning a bare id and job_status; this is the parity table the
// other three SDKs answer — start as the app and for a viewer, respond, get,
// list, wait with the request guard, logs, output, cancel, and from inside a
// job progress with its message and the output upload.
func TestTheRSDKJobsSurface(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") {
		t.Skip("this R lacks curl or jsonlite, which the R SDK imports")
	}

	var mu sync.Mutex
	seen := map[string]string{}
	note := func(key, value string) {
		mu.Lock()
		defer mu.Unlock()
		seen[key] = value
	}
	reads := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(status int, body any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
		run := func(status string) map[string]any {
			return map[string]any{"id": "run_1", "job": "export", "status": status, "started_by": "app",
				"queue_position": 1, "status_url": "/apps/books/__toft/runs/run_1",
				"logs_url": "/api/v1/app/job-runs/run_1/logs"}
		}
		body, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/app/jobs/export/runs":
			key := "start"
			if r.Header.Get("X-Toft-Act-For") != "" {
				key = "viewer-start"
			}
			note(key, string(body)+"|"+r.Header.Get("X-Toft-Act-For")+"|"+r.Header.Get("Idempotency-Key"))
			reply(202, run("queued"))
		case "GET /api/v1/app/job-runs/run_1":
			mu.Lock()
			reads++
			n := reads
			mu.Unlock()
			// The second and third reads are the app's rate limit refusing
			// the poll: job_wait waits it out (jobs-regression #4).
			if n == 2 || n == 3 {
				w.Header().Set("Retry-After", "0")
				reply(429, map[string]any{"code": "rate_limited", "retryable": true, "error": "slow down"})
				return
			}
			if n%2 == 1 {
				reply(200, run("running"))
				return
			}
			done := run("failed")
			done["failure_reason"] = "exit 3: connection refused"
			reply(200, done)
		case "GET /api/v1/app/job-runs":
			q := r.URL.Query()
			key := "list"
			if q.Get("scope") != "" {
				key = "mine"
			}
			note(key, q.Get("job")+"|"+q.Get("status")+"|"+q.Get("limit")+"|"+q.Get("scope")+"|"+r.Header.Get("X-Toft-Act-For"))
			reply(200, map[string]any{"runs": []any{run("failed")}})
		case "GET /api/v1/app/job-runs/run_1/logs":
			note("logs", r.URL.Query().Get("after"))
			reply(200, map[string]any{"status": "running", "lines": []any{
				map[string]any{"seq": 4, "at": "2026-10-22T03:00:00Z", "stream": "stdout", "line": "loading rows"},
			}})
		case "GET /api/v1/app/job-runs/run_1/output":
			http.Redirect(w, r, srv.URL+"/store/object", http.StatusFound)
		case "GET /store/object":
			note("store-auth", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"rows":3}`))
		case "POST /api/v1/app/job-runs/run_1/cancel":
			note("cancel", r.Header.Get("X-Toft-Act-For"))
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/v1/runner/progress":
			note("progress", string(body)+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("X-Toft-Runner-Version"))
			reply(200, map[string]any{})
		case "PUT /store/out":
			note("output", string(body))
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")
refusal <- function(expr) tryCatch({ force(expr); NULL }, toft_error = function(e) e)

run <- job_start("export", params = list(month = "2026-03"), idempotency_key = "k1")
say("start", class(run)[1], run$id, run$status, run$queue_position)

final <- job_wait(run, poll = 0.01)
say("wait", final$status, final$failure_reason)

say("get", job_get("run_1")$status)
runs <- job_list(job = "export", status = "failed", limit = 5)
say("list", length(runs), runs[[1]]$status)

logs <- job_logs(run, after_seq = 3)
say("logs", nrow(logs), logs$seq[1], logs$line[1])
say("output", job_output(run, json = TRUE)$rows)

# A request: the gateway's deadline is 30s out.
deadline <- format(Sys.time() + 30, "%Y-%m-%dT%H:%M:%SZ", tz = "UTC")
req <- list("X-Toft-Act-For" = "signed-blob", "X-Toft-Request-Deadline" = deadline)
mine <- viewer_jobs(req) |> job_start("export")
e <- refusal(job_wait(mine))
say("guard", e$code, e$retryable, e$hint)
e <- refusal(job_wait(run, timeout = 60, request = req))
say("guard-long", e$code)
say("guard-fits", job_wait(mine, timeout = 5, poll = 0.01)$status)
job_cancel(mine)
invisible(job_mine(req, job = "export"))

e <- refusal(viewer_jobs(list()))
say("no-viewer", e$code, e$retryable, e$hint)
say("no-viewer-anon", grepl("anonymous visitor", conditionMessage(e), fixed = TRUE),
    grepl("viewer_anonymous", conditionMessage(e), fixed = TRUE))
e <- refusal(job_mine(list()))
say("mine-no-viewer", e$code, e$retryable, e$hint)

res <- new.env()
res$headers <- list()
res$setHeader <- function(name, value) res$headers[[name]] <- value
out <- job_respond(run, res = res, req = list(HTTP_ACCEPT = "application/json"))
say("respond-json", out$status, out$headers[["Location"]], jsonlite::fromJSON(out$body)$job)
endpoint <- function(req, res) job_respond(run)
html <- endpoint(list(HEADERS = c(accept = "text/html,application/xhtml+xml")), res)
say("respond-html", html$status, html$headers[["Location"]])
plain <- job_respond(run, req = list())
say("respond-plain", plain$status, plain$headers[["Location"]])

say("outside", is.null(current_run()))
Sys.setenv(TOFT_RUN_ID = "run_9", TOFT_JOB_TOKEN = "run-token", TOFT_JOB_NAME = "export",
           TOFT_RERUN_OF = "run_8", TOFT_DEADLINE = "2026-10-22T04:00:00Z",
           TOFT_PARAMS = '{"month":"2026-03","rows":3}', TOFT_OUTPUT_URI = paste0(Sys.getenv("TOFT_API_URL"), "/store/out"))
me <- current_run()
say("inside", me$id, me$job, me$rerun_of, format(me$deadline, "%H:%M", tz = "UTC"), me$params$month, class(me$params$rows))
run_progress(40, "loading rows")
run_write_output(list(rows = 3), json = TRUE)
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"start|toft_run|run_1|queued|1",
		"wait|failed|exit 3: connection refused",
		"get|",
		"list|1|failed",
		"logs|1|4|loading rows",
		"output|3",
		"guard|would_outlive_request|FALSE|return(job_respond(run))",
		"guard-long|would_outlive_request",
		"guard-fits|",
		"no-viewer|no_viewer|FALSE|use job_start(...) to start it as the app",
		"mine-no-viewer|no_viewer|FALSE|use job_list(...) for the app's own runs",
		// An anonymous visitor to a public app does carry the header (D5).
		"no-viewer-anon|TRUE|TRUE",
		"respond-json|202|/apps/books/__toft/runs/run_1|export",
		"respond-html|303|/apps/books/__toft/runs/run_1",
		"respond-plain|202|/apps/books/__toft/runs/run_1",
		"outside|TRUE",
		"inside|run_9|export|run_8|04:00|2026-03|integer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for key, want := range map[string]string{
		"start":        `{"params":{"month":"2026-03"}}||k1`,
		"viewer-start": `|signed-blob|`,
		"list":         "export|failed|5||",
		"mine":         "export||20|viewer|signed-blob",
		"logs":         "3",
		"store-auth":   "",
		"cancel":       "signed-blob",
		"progress":     `{"version":"2","percent":40,"message":"loading rows"}|Bearer run-token|2`,
		"output":       `{"rows":3}`,
	} {
		if seen[key] != want {
			t.Errorf("%s: the platform saw %q, want %q", key, seen[key], want)
		}
	}
}

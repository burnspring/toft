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

// The R SDK's bug-bash fixes, driven for real as the other *_r_test.go files
// are: job_cancel returns the run after the cancel (BB-021), app_data_set
// returns the document a read would (BB-030), app_data_clear exists (BB-031),
// every store_*() on an unmapped name is not_mapped (BB-033), and the
// arguments that used to reach the platform — or raise a raw R error — are
// refused bad_request before any request.
func TestTheRSDKRefusesBadArgumentsAndAnswersWithTheWrittenState(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	var requests []string
	var puts []string
	cancelled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		reply := func(status int, v any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		switch r.Method + " " + r.URL.Path {
		case "PUT /api/v1/app/data":
			puts = append(puts, string(body))
			reply(200, map[string]any{"version": 4, "bytes": len(body)})
		case "DELETE /api/v1/app/data":
			reply(200, map[string]any{"deleted": true, "if_match": r.Header.Get("If-Match")})
		case "GET /api/v1/app/job-runs/run_1":
			status := "queued"
			if cancelled {
				status = "cancelled"
			}
			reply(200, map[string]any{"id": "run_1", "job": "export", "status": status})
		case "POST /api/v1/app/job-runs/run_1/cancel":
			cancelled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			if strings.HasPrefix(r.URL.Path, "/api/v1/app/store/") {
				reply(404, map[string]any{"error": "no store named \"gone\"", "code": "store_not_mapped", "retryable": false})
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	src := dir + "/x.csv"
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, e$retryable, sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")

# app_data_set refuses what is not one JSON object, and writes list() as {}.
say("set-vector", code(app_data_set(1:3)))
say("set-string", code(app_data_set("str")))
say("set-unnamed", code(app_data_set(list(1, 2))))
say("set-frame", code(app_data_set(data.frame(a = 1))))
say("update-notfn", code(app_data_update("notfn")))
empty <- app_data_set(list())
say("set-empty", empty$version, empty$empty, length(empty$data), class(empty$data))
doc <- app_data_set(list(period = "monthly", n = 2L))
say("set-doc", doc$version, doc$data$period, doc$data$n, doc$empty)
say("clear", app_data_clear(), "exported", "export(app_data_clear)" %in% readLines("NAMESPACE"))
say("clear-if", app_data_clear(if_version = 4))

# job_cancel answers the run after the cancel.
run <- job_get("run_1")
after <- job_cancel(run)
say("cancel", run$status, after$status)

# Bad arguments are refused before any request.
say("progress-high", code(run_progress(150)))
say("progress-low", code(run_progress(-1)))
say("list-limit", code(job_list(limit = -1)))
say("list-status", code(job_list(status = "bogus")))
say("notify-null", code(notify(NULL, text = "x")))
say("token-empty", code(access_token("")))

# Every store_*() on an unmapped name is not_mapped, as store() is.
Sys.setenv(TOFT_STORAGE = '[{"name":"uploads","kind":"store","path":"","access":"write"}]')
say("store", code(store("nope")))
say("store-list", code(store_list("nope")))
say("store-upload", code(store_upload("nope", "` + src + `")))
say("store-download", code(store_download("nope", "x.csv")))
say("store-url", code(store_url("nope", "x.csv")))
say("store-remove", code(store_remove("nope", "x.csv")))
say("store-put", code(store_put("nope", "` + src + `")))
say("store-blank", code(store_list(list(name = ""))))
# A mount the platform does not map: its store_not_mapped is not_mapped too.
stale <- list(name = "gone", kind = "store", path = "", access = "write", folder = "")
say("stale-list", code(store_list(stale)))
say("stale-url", code(store_url(stale, "x.csv")))
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
		"set-vector|bad_request/FALSE",
		"set-string|bad_request/FALSE",
		"set-unnamed|bad_request/FALSE",
		"set-frame|bad_request/FALSE",
		"update-notfn|bad_request/FALSE",
		"set-empty|4|FALSE|0|list",
		"set-doc|4|monthly|2|FALSE",
		"clear|TRUE|exported|TRUE",
		"clear-if|TRUE",
		"cancel|queued|cancelled",
		"progress-high|bad_request/FALSE",
		"progress-low|bad_request/FALSE",
		"list-limit|bad_request/FALSE",
		"list-status|bad_request/FALSE",
		"notify-null|bad_request/FALSE",
		"token-empty|bad_request/FALSE",
		"store|not_mapped/FALSE",
		"store-list|not_mapped/FALSE",
		"store-upload|not_mapped/FALSE",
		"store-download|not_mapped/FALSE",
		"store-url|not_mapped/FALSE",
		"store-remove|not_mapped/FALSE",
		"store-put|not_mapped/FALSE",
		"store-blank|not_mapped/FALSE",
		"stale-list|not_mapped/FALSE",
		"stale-url|not_mapped/FALSE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Exactly the two good writes reached the platform, the empty one as {}.
	if len(puts) != 2 || strings.TrimSpace(puts[0]) != "{}" {
		t.Errorf("PUT bodies = %q, want {} then the document — and nothing refused", puts)
	}
	for _, req := range requests {
		if strings.Contains(req, "/notify/") || strings.Contains(req, "/tokens/") ||
			strings.Contains(req, "/runner/") || strings.HasSuffix(req, "/job-runs") ||
			strings.Contains(req, "/store/nope") {
			t.Errorf("a refused call reached the platform: %s", req)
		}
	}
}

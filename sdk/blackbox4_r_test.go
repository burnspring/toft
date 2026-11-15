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

// The R SDK's findings in the fourth 2026-11-03 black-box list, driven for
// real: a folder with a ".." segment is invalid_name and a TOFT_STORAGE entry
// missing its kind or access is storage_env_invalid (BB-043); a mounts() entry
// has read_only and mounted (BB-044); a run handed an offset keeps it
// (BB-045); app data off the platform with NaN is bad_request (BB-047); the
// SDK's own refusals carry a hint — a blank email subject, a wait that ran out,
// a version conflict given up on, an object store's refusal (BB-042); and
// app_data_update()'s message says the function must return a named list
// (D-28).
func TestTheRSDKBlackBoxFourFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/app/data" && r.Method == http.MethodGet:
			w.Header().Set("ETag", `"4"`)
			w.Write([]byte(`{"a":1}`))
		case r.URL.Path == "/api/v1/app/data":
			w.WriteHeader(http.StatusPreconditionFailed)
			w.Write([]byte(`{"code":"version_conflict","error":"stale","version":4}`))
		case r.URL.Path == "/api/v1/app/job-runs/r1":
			w.Write([]byte(`{"id":"r1","job":"j","status":"running"}`))
		case r.URL.Path == "/store":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
WAIT_POLL_SECONDS <- 0.05
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, !is.null(e$hint) && nzchar(e$hint), sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
msg <- function(expr) tryCatch({ force(expr); "none" }, error = function(e) conditionMessage(e))
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")
api <- Sys.getenv("TOFT_API_URL")

Sys.setenv(TOFT_STORAGE = '[{"name":"v","kind":"volume","path":"/data/v","access":"read"},{"name":"s","kind":"store","path":"","access":"write"}]')
say("folder-dotdot", code(storage("v", "..")), code(storage("v", "a/../b")), code(store("s", "/abs")))
m <- mounts()
say("mounts-fields", paste(names(m[[1]]), collapse = ","))
say("mounts-derived", m[[1]]$read_only, m[[1]]$mounted, m[[2]]$read_only, m[[2]]$mounted)
for (env in c('[{"name":"x"}]', '[{"name":"x","kind":"store"}]', '[{"kind":"store","access":"read"}]')) {
  Sys.setenv(TOFT_STORAGE = env)
  say("incomplete", code(mounts()))
}
Sys.setenv(TOFT_STORAGE = "")

Sys.setenv(TOFT_SCHEDULED_FOR = "2026-11-03T09:00:00-05:00")
at <- scheduled_for()
say("scheduled", attr(at, "text"), format(at, "%H:%M %z"))
Sys.setenv(TOFT_SCHEDULED_FOR = "")

say("update-msg", msg(app_data_update(function(doc) 5)))
say("update-conflict", code(app_data_update(function(doc) list(a = 2), attempts = 2)))
say("email-subject", code(notify("email", to = "a@example.com", subject = " ", text = "t")))
say("wait", code(job_wait(job_get("r1"), timeout = 0.01)))

Sys.setenv(TOFT_RUN_ID = "r1", TOFT_JOB_NAME = "j", TOFT_JOB_TOKEN = "t",
           TOFT_OUTPUT_URI = paste0(api, "/store"))
say("out-403", code(run_write_output("x")))
Sys.setenv(TOFT_RUN_ID = "", TOFT_JOB_TOKEN = "", TOFT_OUTPUT_URI = "")

Sys.setenv(TOFT_API_URL = "", TOFT_APP_TOKEN = "")
say("nan-off", code(app_data_set(list(x = NaN))))
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
		"folder-dotdot|invalid_name/TRUE|invalid_name/TRUE|invalid_name/TRUE",
		"mounts-fields|name,kind,path,access,folder,read_only,mounted",
		"mounts-derived|TRUE|TRUE|FALSE|FALSE",
		"incomplete|storage_env_invalid/TRUE",
		"scheduled|2026-11-03T09:00:00-05:00|09:00 -0500",
		"must return a named list",
		"update-conflict|version_conflict/TRUE",
		"email-subject|bad_request/TRUE",
		"wait|wait_timeout/TRUE",
		"out-403|output_unwritable/TRUE",
		"nan-off|bad_request/TRUE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "incomplete|storage_env_invalid/TRUE") != 3 {
		t.Errorf("every incomplete TOFT_STORAGE entry should be refused:\n%s", got)
	}
}

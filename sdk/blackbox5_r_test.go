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

// The R SDK's findings in the fifth 2026-11-03 black-box list, driven for real:
// the refusals R makes itself carry a hint — job_start(""), run_progress() out
// of range, notify_status("") and a TOFT_STORAGE that is not JSON or not an
// array — and a platform not_found for an unknown send gains a fallback hint
// (BB-053); the storage_env_invalid hint calls path optional (BB-061).
func TestTheRSDKBlackBoxFiveFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found","error":"no such send"}`))
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, !is.null(e$hint) && nzchar(e$hint), !is.null(e$body$hint) && nzchar(e$body$hint), sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
hint <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) e$hint)
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")

say("start-blank", code(job_start("")))
say("list-bogus", code(job_list(status = "bogus")))
say("get-blank", code(job_get("")))
Sys.setenv(TOFT_RUN_ID = "r1", TOFT_JOB_TOKEN = "t")
say("progress", code(run_progress(140)), code(run_progress(-1)))
Sys.setenv(TOFT_RUN_ID = "", TOFT_JOB_TOKEN = "")
say("status-blank", code(notify_status("")))
say("status-unknown", code(notify_status("snd_nope")))
for (env in c('not json', '{"name":"x"}', '"x"', '[1]')) {
  Sys.setenv(TOFT_STORAGE = env)
  say("storage-env", code(mounts()))
}
Sys.setenv(TOFT_STORAGE = '[{"name":"x"}]')
say("storage-hint", hint(mounts()))
Sys.setenv(TOFT_STORAGE = '[{"name":"drop","kind":"store","access":"read"}]')
say("pathless", length(mounts()), mounts()[[1]]$mounted)
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
		"start-blank|bad_request/TRUE/TRUE",
		"list-bogus|bad_request/TRUE/TRUE",
		"get-blank|bad_request/TRUE/TRUE",
		"progress|bad_request/TRUE/TRUE|bad_request/TRUE/TRUE",
		"status-blank|bad_request/TRUE/TRUE",
		"status-unknown|not_found/TRUE/TRUE",
		"pathless|1|FALSE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "storage-env|storage_env_invalid/TRUE/TRUE"); n != 4 {
		t.Errorf("every malformed TOFT_STORAGE should be storage_env_invalid with a hint (%d of 4):\n%s", n, got)
	}
	if !strings.Contains(got, "signed URL has none") || strings.Contains(got, "path and access") {
		t.Errorf("the storage_env_invalid hint should call path optional:\n%s", got)
	}
}

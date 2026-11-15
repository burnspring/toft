package sdk

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The R SDK's findings in the third round of the 2026-11-04 black-box list,
// driven for real: TOFT_STORAGE='{}' is storage_env_invalid, not an empty
// mapping (BB-110); a store call's name that is not a string is bad_request
// with that call in its hint, not store_upload()'s (BB-109's sweep);
// app_data_update()'s attempts is a whole number from 1 (BB-112's sweep).
func TestTheRSDKBlackBoxEightFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, isFALSE(e$retryable), !is.null(e$body$hint) && nzchar(e$body$hint), sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
hint <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) e$body$hint, error = function(e) "raw")
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")

Sys.setenv(TOFT_STORAGE = "{}")
say("storage-obj", code(storage("a")))
Sys.setenv(TOFT_STORAGE = '[{"name":"st","kind":"store","path":"","access":"full"}]')
s <- store("st")
say("download-num", code(store_download(s, 5)), startsWith(hint(store_download(s, 5)), "pass the name as a string: store_download("))
say("download-null", code(store_download(s, NULL)))
say("remove-num", code(store_remove(s, 5)), startsWith(hint(store_remove(s, 5)), "pass the name as a string: store_remove("))
say("url-num", code(store_url(s, 5)), startsWith(hint(store_url(s, 5)), "pass the name as a string: store_url("))
say("list-num", code(store_list(s, 5)))
say("update-0", code(app_data_update(function(d) d, attempts = 0)))
say("update-frac", code(app_data_update(function(d) d, attempts = 1.5)))
say("update-inf", code(app_data_update(function(d) d, attempts = Inf)))
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
		"storage-obj|storage_env_invalid/TRUE/TRUE",
		"download-num|bad_request/TRUE/TRUE|TRUE",
		"download-null|bad_request/TRUE/TRUE",
		"remove-num|bad_request/TRUE/TRUE|TRUE",
		"url-num|bad_request/TRUE/TRUE|TRUE",
		"list-num|bad_request/TRUE/TRUE",
		"update-0|bad_request/TRUE/TRUE",
		"update-frac|bad_request/TRUE/TRUE",
		"update-inf|bad_request/TRUE/TRUE",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

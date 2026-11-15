package sdk

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The R SDK's findings in the fourth round of the 2026-11-04 black-box list:
// a store call given no store, or no object name, is bad_request with a hint —
// it was R's own uncoded "argument "mount" is missing" (BB-005).
func TestTheRSDKBlackBoxNineFixes(t *testing.T) {
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
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")

Sys.setenv(TOFT_STORAGE = '[{"name":"st","kind":"store","path":"","access":"full"}]')
say("list", code(store_list()))
say("put", code(store_put()))
say("download", code(store_download()))
say("url", code(store_url()))
say("remove", code(store_remove()))
say("upload", code(store_upload()))
s <- store("st")
say("download-noname", code(store_download(s)))
say("url-noname", code(store_url(s)))
say("remove-noname", code(store_remove(s)))
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
	for _, label := range []string{"list", "put", "download", "url", "remove", "upload",
		"download-noname", "url-noname", "remove-noname"} {
		if want := label + "|bad_request/TRUE/TRUE\n"; !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

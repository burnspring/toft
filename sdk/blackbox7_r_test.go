package sdk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The R SDK's findings in the second half of the 2026-11-04 black-box list,
// driven for real: a directory or a non-string reaching store_upload() is a
// coded refusal, not "cannot open the connection" (BB-008); a negative or NaN
// job_wait() timeout is bad_request (BB-009); store_put() given a mounted
// volume's name copies onto it, as Go's Mount.Put does (BB-010);
// TOFT_STORAGE='[]' is not_mapped (BB-023); job_logs(-1) is bad_request
// (BB-025); a missing local file is not_found (BB-026); print(viewer_jobs())
// claims nothing about identity (D-17); off-platform scratch is a private
// toft-scratch-<uid>/<app> (D-12).
func TestTheRSDKBlackBoxSevenFixes(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	dir := t.TempDir()
	vol := filepath.Join(dir, "vol")
	st := filepath.Join(dir, "st")
	for _, d := range []string{vol, st} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	storage := `[{"name":"blackbox-vol","kind":"volume","path":"` + vol + `","access":"write"},` +
		`{"name":"blackbox-store","kind":"store","path":"","access":"write"},` +
		`{"name":"mounted","kind":"store","path":"` + st + `","access":"write"}]`

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
code <- function(expr) tryCatch({ force(expr); "none" }, toft_error = function(e) paste(e$code, isFALSE(e$retryable), !is.null(e$body$hint) && nzchar(e$body$hint), sep = "/"),
                                error = function(e) paste("raw:", conditionMessage(e)))
say <- function(label, ...) cat(label, "|", paste(..., sep = "|"), "\n", sep = "")
Sys.setenv(TOFT_STORAGE = '` + storage + `')

say("upload-dir", code(store_upload(store("blackbox-store"), tempdir(), "x/dir.txt")))
say("upload-missing", code(store_upload(store("blackbox-store"), "/no/such/file", "x.txt")))
say("upload-number", code(store_upload(store("blackbox-store"), 5)))
say("upload-name", code(store_upload(store("blackbox-store"), "` + src + `", 5)))
say("put-missing", code(store_put("mounted", "/no/such/file", "x.txt")))
say("put-dir", code(store_put("mounted", tempdir(), "x/dir.txt")))
say("put-volume", basename(store_put("blackbox-vol", "` + src + `", "x/put.txt")))
say("put-volume-mount", basename(store_put(storage("blackbox-vol"), "` + src + `", "y.txt")))
say("volume-upload", code(store_upload("blackbox-vol", "` + src + `")))

Sys.setenv(TOFT_STORAGE = "[]")
say("empty-storage", code(storage("x")))
Sys.unsetenv("TOFT_STORAGE")
say("no-storage", code(storage("x")))

run <- structure(list(id = "r1", status = "running"), class = "toft_run")
done <- structure(list(id = "r1", status = "succeeded"), class = "toft_run")
say("wait-neg", code(job_wait(run, timeout = -1)))
say("wait-nan", code(job_wait(run, timeout = NaN)))
say("wait-done-neg", code(job_wait(done, timeout = -1)))
say("wait-poll", code(job_wait(run, timeout = 1, poll = 0)))
say("logs-neg", code(job_logs(run, -1)))
say("logs-frac", code(job_logs(run, 1.5)))

printed <- paste(capture.output(print(structure(list(act_for = "blob"), class = "toft_viewer_jobs"))), collapse = "")
say("print-claims", grepl("cannot see who", printed), grepl("blob", printed))

s <- scratch_dir()
say("scratch", grepl("^toft-scratch-", basename(dirname(s))), file.info(s)$mode == as.octmode("700"), identical(s, scratch_dir()))
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
		"upload-dir|bad_request/TRUE/TRUE",
		"upload-missing|not_found/TRUE/TRUE",
		"upload-number|bad_request/TRUE/TRUE",
		"upload-name|bad_request/TRUE/TRUE",
		"put-missing|not_found/TRUE/TRUE",
		"put-dir|bad_request/TRUE/TRUE",
		"put-volume|put.txt",
		"put-volume-mount|y.txt",
		"volume-upload|not_a_store/TRUE/TRUE",
		"empty-storage|not_mapped/TRUE/TRUE",
		"no-storage|off_platform/TRUE/TRUE",
		"wait-neg|bad_request/TRUE/TRUE",
		"wait-nan|bad_request/TRUE/TRUE",
		"wait-done-neg|bad_request/TRUE/TRUE",
		"wait-poll|bad_request/TRUE/TRUE",
		"logs-neg|bad_request/TRUE/TRUE",
		"logs-frac|bad_request/TRUE/TRUE",
		"print-claims|FALSE|FALSE",
		"scratch|TRUE|TRUE|TRUE",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	if b, err := os.ReadFile(filepath.Join(vol, "x", "put.txt")); err != nil || string(b) != "a" {
		t.Errorf("store_put onto the volume did not land: %q, %v", b, err)
	}
}

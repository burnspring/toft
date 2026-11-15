package sdk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The R SDK's refusal is a classed condition carrying the platform's envelope,
// driven for real for the reason store_r_test.go gives: R has no test runner
// here. The table is the one the other three SDKs' own tests run — the body's
// `retryable` wins over the status, an absent one falls back to the status
// rule, and `hint` reads through — and the message is the sentence the
// transport has always raised, so code matching on it is not broken by the
// class it gained.
func TestTheRSDKSignalsTheErrorEnvelope(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") {
		t.Skip("this R lacks curl or jsonlite, which the R SDK imports")
	}

	replies := map[string]struct {
		status int
		body   any
	}{
		"/api/v1/app/self":         {503, map[string]any{"error": "not configured", "code": "unavailable", "retryable": false, "hint": "an administrator configures it"}},
		"/api/v1/app/data":         {409, map[string]any{"error": "wait", "code": "busy", "retryable": true}},
		"/api/v1/app/notify/slack": {502, "<html>bad gateway</html>"},
		"/api/v1/app/runs":         {409, map[string]any{"error": "older install"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply, ok := replies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s, isText := reply.body.(string); isText {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(reply.status)
			_, _ = w.Write([]byte(s))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_ = json.NewEncoder(w).Encode(reply.body)
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
show <- function(label, method, path) {
  e <- tryCatch({ toft_request(method, path); NULL }, toft_error = function(e) e)
  cat(sprintf("%s|%s|%s|%s|%s|%s|%s|%s\n", label, inherits(e, "error"), e$status, e$code,
              e$retryable, if (is.null(e$hint)) "-" else e$hint,
              if (is.null(e$retry_after)) "-" else e$retry_after, conditionMessage(e)))
}
show("veto", "GET", "/api/v1/app/self")
show("wait", "GET", "/api/v1/app/data")
show("proxy", "POST", "/api/v1/app/notify/slack")
show("older", "GET", "/api/v1/app/runs")
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
		"veto|TRUE|503|unavailable|FALSE|an administrator configures it|-|Toft GET /api/v1/app/self failed (503): not configured",
		"wait|TRUE|409|busy|TRUE|-|-|Toft GET /api/v1/app/data failed (409): wait",
		"proxy|TRUE|502||TRUE|-|7|Toft POST /api/v1/app/notify/slack failed (502): <html>bad gateway</html>",
		"older|TRUE|409||FALSE|-|-|Toft GET /api/v1/app/runs failed (409): older install",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
}

// A refusal the R SDK makes itself — no request, so no status — is the same
// classed condition with a code and retryable FALSE. It used to be a bare
// stop(), which `tryCatch(toft_error = )` did not catch and whose `$code` was
// NULL, so an R app could branch on the platform's refusals and not its own.
func TestTheRSDKCodesItsOwnRefusals(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") {
		t.Skip("this R lacks curl or jsonlite, which the R SDK imports")
	}
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
show <- function(label, expr) {
  e <- tryCatch({ force(expr); NULL }, toft_error = function(e) e)
  cat(sprintf("%s|%s|%s\n", label, if (is.null(e)) "none" else e$code,
              if (is.null(e)) "-" else e$retryable))
}
show("ambiguous", storage("drop"))
show("unmapped", storage("nope"))
show("unmounted", store_put(store("drop", "backup"), "x.txt"))
show("unmounted-path", mount_path(store("drop", "backup")))
# D17: the refusal names the accessor the caller used.
e <- tryCatch(store("drop"), toft_error = function(e) e)
cat(sprintf("store-names|%s\n", grepl("store('drop', folder = ", conditionMessage(e), fixed = TRUE)))
e <- tryCatch(storage("drop"), toft_error = function(e) e)
cat(sprintf("storage-names|%s\n", grepl("storage('drop', folder = ", conditionMessage(e), fixed = TRUE)))
# Drills 2026-10-28 #10: each storage refusal with a next step names it.
hinted <- function(label, expr) {
  e <- tryCatch({ force(expr); NULL }, toft_error = function(e) e)
  cat(sprintf("hint-%s|%s\n", label, !is.null(e) && is.character(e$hint) && nzchar(e$hint)))
}
hinted("ambiguous", storage("drop"))
hinted("unmapped", storage("nope"))
hinted("folder", storage("drop", "elsewhere"))
hinted("unmounted", store_put(store("drop", "backup"), "x.txt"))
hinted("unmounted-path", mount_path(store("drop", "backup")))
hinted("bad-name", check_relative_name("../x"))
hinted("no-name", storage(""))
e <- tryCatch(store("drop"), toft_error = function(e) e)
cat(sprintf("hint-names-folder|%s\n", grepl("store('drop', folder = ", e$hint, fixed = TRUE)))
# D18: the store helpers' argument checks are coded toft_error conditions too.
show("put-no-mount", store_put(NULL, "x.txt"))
show("put-no-source", store_put(list(name = "m", path = tempdir()), ""))
show("put-no-file", store_put(list(name = "m", path = tempdir()), "/no/such/file"))
show("upload-no-file", store_upload(store("drop", ""), "/no/such/file"))
show("route-no-store", store_route(NULL, "/api/v1/app/store/{slug}/objects"))
show("no-signed-url", send_signed(list(), NULL))
show("store-env", { Sys.setenv(TOFT_STORAGE = "{not json"); mounts() })
# Bug report 2026-11-04 §5: the refusal for a name passed to mount_path() named
# storage_mount(), which does not exist. Every function its message and hint
# call must be one the package exports.
exported <- sub("^export\\((.*)\\)$", "\\1", grep("^export\\(", readLines("NAMESPACE"), value = TRUE))
e <- tryCatch(mount_path("some-volume"), toft_error = function(e) e)
called <- regmatches(paste(conditionMessage(e), e$hint), gregexpr("[A-Za-z_.][A-Za-z0-9_.]*(?=\\()", paste(conditionMessage(e), e$hint), perl = TRUE))[[1]]
cat(sprintf("mount-path-names|%s|%s\n", length(called) > 1, paste(setdiff(called, exported), collapse = ",")))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), `TOFT_STORAGE=[{"name":"drop","kind":"store","path":"","access":"read"},`+
		`{"name":"drop","kind":"store","path":"","access":"write","folder":"backup"}]`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{"ambiguous|ambiguous_mount|FALSE", "unmapped|not_mapped|FALSE", "unmounted|not_mounted|FALSE", "unmounted-path|not_mounted|FALSE",
		"store-names|TRUE", "storage-names|TRUE",
		"put-no-mount|bad_request|FALSE", "put-no-source|bad_request|FALSE", "put-no-file|not_found|FALSE",
		"upload-no-file|not_found|FALSE", "route-no-store|bad_request|FALSE", "no-signed-url||TRUE",
		"store-env|storage_env_invalid|FALSE",
		"hint-ambiguous|TRUE", "hint-unmapped|TRUE", "hint-folder|TRUE", "hint-unmounted|TRUE",
		"hint-unmounted-path|TRUE", "hint-bad-name|TRUE", "hint-no-name|TRUE", "hint-names-folder|TRUE",
		"mount-path-names|TRUE|\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("want %q in\n%s", want, out)
		}
	}
}

// The R transport makes the one built-in retry the other three SDKs make: a
// 429 is tried again after Retry-After, so a call that met the app's limiter
// once gets its answer (jobs-regression #14). A refusal from a handler — a 409
// — is not retried.
func TestTheRSDKRetriesTheRateLimiterOnce(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") {
		t.Skip("this R lacks curl or jsonlite, which the R SDK imports")
	}
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/app/self" && hits[r.URL.Path] == 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "rate_limited", "retryable": true, "error": "slow down"})
		case r.URL.Path == "/api/v1/app/self":
			_ = json.NewEncoder(w).Encode(map[string]any{"slug": "books"})
		default:
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "busy", "retryable": true, "error": "wait"})
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
cat("self|", toft_request("GET", "/api/v1/app/self")$slug, "\n", sep = "")
e <- tryCatch({ toft_request("GET", "/api/v1/app/data"); NULL }, toft_error = function(e) e)
cat("busy|", e$code, "\n", sep = "")
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{"self|books", "busy|busy"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("R output lacks %q:\n%s", want, out)
		}
	}
	if hits["/api/v1/app/self"] != 2 || hits["/api/v1/app/data"] != 1 {
		t.Errorf("hits = %v, want the 429 retried once and the 409 not at all", hits)
	}
}

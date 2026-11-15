package sdk

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// app_url(): R's half of the shared join vector (testdata/app-url-vector.json)
// and of the fetch rules the other three SDKs test in their own suites — asked
// of /api/v1/app/url, reused briefly and then asked again, the last answer
// used when the platform cannot be reached, a refusal raised.
func TestTheRSDKBuildsAppURLs(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("jsonlite") {
		t.Skip("this R lacks jsonlite, which reads the vector")
	}
	vectorPath, _ := filepath.Abs(filepath.Join("testdata", "app-url-vector.json"))
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
args <- commandArgs(trailingOnly = TRUE)
for (tc in jsonlite::fromJSON(args[1], simplifyVector = FALSE)$cases) {
  got <- tryCatch(join_app_url(tc$public_url, tc$base_path, tc$path),
                  toft_error = function(e) paste0("ERR:", e$code, if (nzchar(e$hint)) "+hint" else ""))
  cat("vector|", tc$name, "|", got, "\n", sep = "")
}

calls <- 0
answer <- "https://apps.example.com/apps/reporter"
toft_request <- function(method, path, ...) {
  stopifnot(path == "/api/v1/app/url")
  calls <<- calls + 1
  list(public_url = answer, base_path = "/apps/reporter")
}
cat("built|", app_url("/reports/42"), "\n", sep = "")
invisible(app_url())
cat("asked|", calls, "\n", sep = "")
answer <- "https://applications.example.org/apps/reporter"
.app_cache$address_at <- .app_cache$address_at - APP_URL_TTL - 1
cat("moved|", app_url("/a"), "|", calls, "\n", sep = "")

.app_cache$address_at <- .app_cache$address_at - APP_URL_TTL - 1
toft_request <- function(...) stop(transport_failure("cannot reach Toft", simpleError("refused")))
cat("outage|", app_url("/a"), "\n", sep = "")

.app_cache$address <- NULL
cat("outage-cold|", tryCatch(app_url(), toft_error = function(e) "raised"), "\n", sep = "")

toft_request <- function(...) stop(toft_refusal("no_public_address", "no address", hint = "set APPS_BASE_URL"))
cat("refused|", tryCatch(app_url(), toft_error = function(e) e$code), "\n", sep = "")
cat("absolute|", tryCatch(app_url("https://x.example/"), toft_error = function(e) e$code), "\n", sep = "")
`
	cmd := exec.Command("Rscript", "-e", script, vectorPath)
	cmd.Dir = "r"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Cases []struct {
			Name  string `json:"name"`
			Want  string `json:"want"`
			Error string `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"built|https://apps.example.com/apps/reporter/reports/42",
		"asked|1",
		"moved|https://applications.example.org/apps/reporter/a|2",
		"outage|https://applications.example.org/apps/reporter/a",
		"outage-cold|raised",
		"refused|no_public_address",
		"absolute|bad_request",
	}
	for _, c := range vector.Cases {
		if c.Error != "" {
			want = append(want, "vector|"+c.Name+"|ERR:"+c.Error+"+hint")
		} else {
			want = append(want, "vector|"+c.Name+"|"+c.Want)
		}
	}
	for _, w := range want {
		if !strings.Contains(string(out), w+"\n") {
			t.Errorf("R output lacks %q:\n%s", w, out)
		}
	}
}

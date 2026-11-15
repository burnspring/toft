package sdk

import (
	"os/exec"
	"strings"
	"testing"
)

// The R SDK finds the act-for header in all three shapes an R web framework
// hands it: plumber's lowered character vector, a named list, and Shiny's
// `session$request` -- an environment whose names are CGI's, `HTTP_X_TOFT_ACT_FOR`.
// The third used to miss, so in a Shiny app viewer() and caller_role() answered
// NULL for every signed-in visitor (the eks drills, 2026-10-22).
func TestTheRSDKReadsEveryFrameworksHeaderShape(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	// No curl or jsonlite needed: sourcing defines the functions, and the
	// lookup is plain R, so this runs on an R with no packages at all.
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
shiny <- new.env()
assign("HTTP_X_TOFT_ACT_FOR", "from-shiny", envir = shiny)
assign("rook.input", new.env(), envir = shiny)
cat("plumber|", find_header(c("x-toft-act-for" = "from-plumber"), "X-Toft-Act-For"), "\n", sep = "")
cat("list|", find_header(list("X-Toft-Act-For" = "from-list"), "X-Toft-Act-For"), "\n", sep = "")
cat("shiny|", find_header(shiny, "X-Toft-Act-For"), "\n", sep = "")
cat("absent|", find_header(shiny, "X-Toft-Role"), "|\n", sep = "")
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{"plumber|from-plumber", "list|from-list", "shiny|from-shiny", "absent||"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("R output lacks %q:\n%s", want, out)
		}
	}
}

// app_info() asks once per process and answers from the cache after, as the
// other three SDKs do; refresh = TRUE asks again. Uncached, a page reading its
// base path and two capabilities on every view spent the app's rate limit on
// the same answer and a reload inside a minute was refused (the eks drills).
func TestTheRSDKCachesItsAppView(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
calls <- 0
toft_request <- function(method, path, ...) { calls <<- calls + 1; list(base_path = "/apps/x", n = calls) }
invisible(app_info()); invisible(app_base_path()); invisible(app_info())
cat("cached|", calls, "\n", sep = "")
cat("refreshed|", app_info(refresh = TRUE)$n, "\n", sep = "")
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{"cached|1", "refreshed|2"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("R output lacks %q:\n%s", want, out)
		}
	}
}

// notify()'s idempotency key is a header, never a body field. The platform
// reads only Idempotency-Key, so a key in the body was ignored and a retry
// mailed twice (the trytoft drills, 2026-10-22).
func TestTheRSDKSendsTheIdempotencyKeyAsAHeader(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
toft_request <- function(method, path, body = NULL, query = NULL, extra_headers = NULL) {
  cat("header|", extra_headers[["Idempotency-Key"]], "\n", sep = "")
  cat("body|", paste(sort(names(body)), collapse = ","), "\n", sep = "")
}
notify("email", to = "a@example.com", subject = "s", text = "t", idempotency_key = "k-1")
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, want := range []string{"header|k-1", "body|subject,text,to\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("R output lacks %q:\n%s", want, out)
		}
	}
}

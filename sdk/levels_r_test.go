package sdk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The R SDK over the three storage levels. A volume mapping's only level above
// read is `full`, and R reports it as it arrived; `store_put()` is the one R
// call that reads a level, and it refuses `read` alone — `full` copies, as
// `write` always has. What the Go, Python and Node tests check of their own
// read-only accessors.
func TestTheRSDKTreatsFullAsWritable(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	dir := t.TempDir()
	full, ro := filepath.Join(dir, "full"), filepath.Join(dir, "ro")
	for _, d := range []string{full, ro} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(dir, "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
src <- commandArgs(trailingOnly = TRUE)[1]
cat("volume:", storage("cache")$access, "\n")
cat("full:", tryCatch({ store_put(store("reports"), src); "copied" },
                      toft_error = function(e) e$code), "\n")
cat("read:", tryCatch({ store_put(store("archive"), src); "copied" },
                      toft_error = function(e) e$code), "\n")
`
	cmd := exec.Command("Rscript", "-e", script, src)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL=http://127.0.0.1:1", "TOFT_APP_TOKEN=tofta_test",
		`TOFT_STORAGE=[{"name":"cache","kind":"volume","path":"`+dir+`","access":"full"},`+
			`{"name":"reports","kind":"store","path":"`+full+`","access":"full"},`+
			`{"name":"archive","kind":"store","path":"`+ro+`","access":"read"}]`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"volume: full", "full: copied", "read: insufficient_access"} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(full, "report.csv")); err != nil {
		t.Errorf("store_put() at full wrote nothing: %v", err)
	}
}

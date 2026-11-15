package sdk

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// R bounds a platform round trip and an object-store transfer, as the other
// three SDKs do (SDK-6). Before, a platform or store that accepted the
// connection and stalled held a Shiny or plumber worker forever. The script
// shrinks both bounds to a second — they are globals the sourced files read —
// so the test waits for the stall, not for 30 s and 5 min.
func TestTheRSDKGivesUpOnAStalledPlatformAndStore(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
DEFAULT_TIMEOUT <- 1
UPLOAD_TIMEOUT <- 1
stalled <- function(label, expr) {
  started <- Sys.time()
  msg <- tryCatch({ force(expr); "answered" }, error = function(e) conditionMessage(e))
  cat(sprintf("%s|%.1f|%s\n", label, as.numeric(Sys.time() - started, units = "secs"), msg))
}
stalled("platform", notify_status("s-1"))
stalled("store", send_signed(list(url = paste0(Sys.getenv("TOFT_API_URL"), "/bucket/obj")), NULL))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	for _, label := range []string{"platform", "store"} {
		var line string
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, label+"|") {
				line = l
			}
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Errorf("no %s line in the R output:\n%s", label, out)
			continue
		}
		if !strings.Contains(strings.ToLower(parts[2]), "timeout") {
			t.Errorf("%s: %q, want a timeout", label, parts[2])
		}
		if secs, _ := time.ParseDuration(parts[1] + "s"); secs > 10*time.Second {
			t.Errorf("%s: gave up after %s, want about the one-second bound", label, secs)
		}
	}
}

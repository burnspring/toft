package sdk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// run_write_output mints its URL at the moment it writes and falls back to
// TOFT_OUTPUT_URI; every output PUT carries Azure Blob's x-ms-blob-type, which
// S3 ignores (security review 2026-11-14, AS-2 and AS-4).
func TestTheRSDKMintsItsOutputURLAtWriteTime(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	mint := true
	puts := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runner/output-url":
			if r.URL.Query().Get("off") != "" || r.Header.Get("Authorization") != "Bearer run-token" || !mint {
				http.NotFound(w, r)
				return
			}
			mint = false // the second write falls back
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"url":"http://` + r.Host + `/fresh"}`))
		case r.Method == http.MethodPut:
			puts[r.URL.Path] = r.Header.Get("x-ms-blob-type")
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
api <- Sys.getenv("TOFT_API_URL")
Sys.setenv(TOFT_RUN_ID = "r1", TOFT_JOB_NAME = "j", TOFT_JOB_TOKEN = "run-token",
           TOFT_OUTPUT_URI = paste0(api, "/stale"))
run_write_output("x")
run_write_output("x")
cat("done\n")
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TOFT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "TOFT_API_URL="+srv.URL)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "done") {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts["/fresh"] != "BlockBlob" {
		t.Errorf("the minted URL was not used, or without the blob type: %v", puts)
	}
	if puts["/stale"] != "BlockBlob" {
		t.Errorf("TOFT_OUTPUT_URI was not the fallback, or without the blob type: %v", puts)
	}
}

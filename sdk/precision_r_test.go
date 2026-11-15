package sdk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// Two ways the R SDK silently changed what it was given (code review
// 2026-11-11, J-1 and J-2): jsonlite's default four digits rounded every number
// in a document, a parameter set or an output — 0.00001234 arrived as 0 — and
// utils::URLencode left a name that already held a %HH sequence completely
// unencoded, so "Q1%20report.csv" addressed "Q1 report.csv". Numbers go out at
// full precision and every segment is encoded every time.
func TestTheRSDKKeepsPrecisionAndEncodesEveryName(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	var putBody, linkPath, startBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/app/data" && r.Method == http.MethodPut:
			putBody = string(body)
			json.NewEncoder(w).Encode(map[string]any{"version": 2})
		case strings.Contains(r.URL.EscapedPath(), "/link/"):
			linkPath = r.URL.EscapedPath()
			json.NewEncoder(w).Encode(map[string]any{"link": "https://toft.example/x"})
		case strings.HasSuffix(r.URL.Path, "/runs") && r.Method == http.MethodPost:
			startBody = string(body)
			json.NewEncoder(w).Encode(map[string]any{"id": "run-1", "status": "queued", "job": "nightly"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
app_data_set(list(rate = 0.00001234, lat = 52.520008, big = 1731300000.1234, n = 3L))
m <- store("uploads")
invisible(store_link(m, "Q1%20report.csv"))
invisible(job_start("nightly", params = list(threshold = 0.12345678)))
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test",
		`TOFT_STORAGE=[{"name":"uploads","kind":"store","path":"","access":"write","folder":"in"}]`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal([]byte(putBody), &doc); err != nil {
		t.Fatalf("document body %q: %v", putBody, err)
	}
	if doc["rate"] != 0.00001234 || doc["lat"] != 52.520008 || doc["big"] != 1731300000.1234 || doc["n"] != 3.0 {
		t.Errorf("document arrived rounded: %s", putBody)
	}
	if !strings.Contains(startBody, "0.12345678") {
		t.Errorf("job parameter arrived rounded: %s", startBody)
	}
	if !strings.HasSuffix(linkPath, "/link/Q1%2520report.csv") {
		t.Errorf("link path = %q, want the literal %%20 encoded as %%2520", linkPath)
	}
}

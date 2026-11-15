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
	"time"
)

// R gets a file connection's token with a POST, caches it, and never prints it.
func TestTheRSDKGetsAndCachesAnAccessToken(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}
	var mu sync.Mutex
	calls := 0
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/app/tokens/sharepoint" {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"eyJ.tok","token_type":"Bearer","expires_at":"`+expires+
			`","resource":"https://graph.microsoft.com","folder":{"drive_id":"b!d","item_id":"01R","label":"reports"}}`)
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
a <- access_token("sharepoint")
b <- access_token("sharepoint")
cat("value:", a$value, a$folder$drive_id, identical(a, b), "\n")
print.toft_token(a)
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "value: eyJ.tok b!d TRUE") {
		t.Errorf("R output:\n%s", got)
	}
	if strings.Count(got, "eyJ") != 1 {
		t.Errorf("print shows the token:\n%s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("%d token requests, want 1 (the second is the cache's)", calls)
	}
}

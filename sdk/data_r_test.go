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

// R can write the app's document without losing another writer's change.
// app_data_get() dropped the version, so app_data_set(if_version = ) could never
// be given one that was read (drills on trytoft, 2026-10-23); app_data_update()
// reads the version, writes conditionally and re-reads on a conflict.
func TestTheRSDKUpdatesTheDocumentConditionally(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	version, conflicted := 1, false
	doc := `{"n":1}`
	var matches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/api/v1/app/data" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `{"data":`+doc+`,"version":`+itoa(int64(version))+`}`)
		case http.MethodPut:
			matches = append(matches, r.Header.Get("If-Match"))
			if !conflicted {
				// Another writer lands between this read and this write.
				conflicted, version, doc = true, version+1, `{"n":10}`
				w.WriteHeader(http.StatusPreconditionFailed)
				io.WriteString(w, `{"error":"the document changed","code":"version_conflict","retryable":false}`)
				return
			}
			body, _ := io.ReadAll(r.Body)
			doc, version = string(body), version+1
			json.NewEncoder(w).Encode(map[string]any{"version": version})
		}
	}))
	defer srv.Close()

	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
r <- app_data_read()
cat("read:", r$version, r$empty, "\n")
v <- app_data_update(function(d) { d$n <- d$n + 1; d })
cat("updated:", v$version, v$data$n, "\n")
`
	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(), "TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"read: 1 FALSE", "updated: 3 11"} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(matches, " ") != `"1" "2"` {
		t.Errorf("If-Match sent = %v, want the read version, then the re-read one", matches)
	}
	if doc != `{"n":11}` {
		t.Errorf("document = %s, want the other writer's 10 plus one", doc)
	}
}


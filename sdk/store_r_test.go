package sdk

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The R SDK reaching a store over signed URLs, driven for real: R has no test
// runner in this repository, so its behaviour is asserted from here, against a
// fake that answers both the mint and the object store. What it checks is what
// the other three SDKs' own tests check — the folder travels, the signed header
// reaches the object store verbatim, the app's token does not, and an unmounted
// store refuses store_put() rather than copying into the working directory,
// and a download of a name the store does not hold is not_found.
func TestTheRSDKReachesAStoreOverSignedURLs(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var mu sync.Mutex
	var mints []*http.Request
	var put *http.Request
	var putBody, mintBody []byte
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/link/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"link": "https://toft.example/files/uploads/in/" + strings.SplitN(r.URL.EscapedPath(), "/link/", 2)[1] +
					"?folder=" + r.URL.Query().Get("folder")})
		case strings.HasPrefix(r.URL.Path, "/api/v1/app/store/"):
			mints = append(mints, r)
			if r.Method == http.MethodPost {
				mintBody = body
			}
			method, headers := http.MethodGet, map[string]string{}
			if r.Method == http.MethodPost {
				method, headers = http.MethodPut, map[string]string{"If-None-Match": "*"}
			}
			obj := "/bucket/obj"
			for _, name := range []string{"missing", "flaky"} {
				if strings.Contains(r.URL.Path, name) {
					obj = "/bucket/" + name
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"url": srv.URL + obj, "method": method, "headers": headers})
		case r.URL.Path == "/bucket/flaky":
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.URL.Path == "/bucket/obj" && r.Method == http.MethodPut:
			put, putBody = r, body
		case r.URL.Path == "/bucket/obj":
			_, _ = w.Write([]byte("the bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// A few MiB, so the upload is streamed through curl's read callback in
	// more than one piece (security review 2026-11-14, CS-11).
	payload := []byte("a,b\n" + strings.Repeat("1,2\n", 3<<18) + "end\n")
	src := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
m <- store("uploads")
cat("upload:", store_upload(m, commandArgs(trailingOnly = TRUE)[1]), "\n")
cat("url:", store_url(m, "2026/report.csv", attachment = TRUE), "\n")
cat("download:", rawToChar(store_download(m, "2026/report.csv")), "\n")
refused <- function(name) tryCatch({ store_download(m, name); "fetched" },
  toft_error = function(e) paste(e$code, e$status, e$retryable, conditionMessage(e)))
cat("missing:", refused("missing.db"), "\n")
cat("flaky:", refused("flaky.db"), "\n")
cat("put:", tryCatch({ store_put(m, commandArgs(trailingOnly = TRUE)[1]); "copied" },
                     error = function(e) conditionMessage(e)), "\n")
cat("link:", store_link(m, "2026/q1 report.csv"), "\n")
`
	cmd := exec.Command("Rscript", "-e", script, src)
	cmd.Dir = "r"
	cmd.Env = append(os.Environ(),
		"TOFT_API_URL="+srv.URL, "TOFT_APP_TOKEN=tofta_test",
		`TOFT_STORAGE=[{"name":"uploads","kind":"store","path":"","access":"write","folder":"in"}]`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"upload: report.csv", "url: " + srv.URL + "/bucket/obj",
		"download: the bytes", "store_upload()",
		// The platform builds the link; the R side sends the name encoded and the folder.
		"link: https://toft.example/files/uploads/in/2026/q1%20report.csv?folder=in",
		// A missing object is the store's 404, coded like every other refusal
		// and caught as a toft_error; any other status is still a toft_error.
		"missing: not_found 404 FALSE 'missing.db' is not in 'uploads'",
		"flaky: store_refused 503 TRUE the object store refused the request (503)"} {
		if !strings.Contains(got, want) {
			t.Errorf("R output lacks %q:\n%s", want, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mints) == 0 || mints[0].URL.Query().Get("folder") != "in" {
		t.Errorf("the mint did not name the folder: %v", mints)
	}
	if len(mints) > 1 && mints[1].URL.Query().Get("disposition") != "attachment" {
		t.Errorf("the link was not signed as an attachment: %s", mints[1].URL.RawQuery)
	}
	if put == nil || put.Header.Get("If-None-Match") != "*" || put.Header.Get("Authorization") != "" {
		t.Errorf("the PUT did not carry the signed header alone: %v", put)
	}
	var sent map[string]any
	_ = json.Unmarshal(mintBody, &sent)
	sum, msum := sha256.Sum256(payload), md5.Sum(payload)
	if sent["sha256"] != hex.EncodeToString(sum[:]) || sent["md5"] != hex.EncodeToString(msum[:]) {
		t.Errorf("digests sent = %v — a wrong one fails only against a real object store", sent)
	}
	if !bytes.Equal(putBody, payload) {
		t.Errorf("PUT body is %d bytes, want the file's %d", len(putBody), len(payload))
	}
	if sent["size"] != float64(len(payload)) {
		t.Errorf("size sent = %v, want %d", sent["size"], len(payload))
	}
	if put != nil && (put.ContentLength != int64(len(payload)) || len(put.TransferEncoding) != 0) {
		t.Errorf("the PUT was not sent with its length declared: Content-Length %d, Transfer-Encoding %v",
			put.ContentLength, put.TransferEncoding)
	}
}

func rHas(pkg string) bool {
	out, err := exec.Command("Rscript", "-e", `cat(requireNamespace("`+pkg+`", quietly = TRUE))`).Output()
	return err == nil && strings.TrimSpace(string(out)) == "TRUE"
}

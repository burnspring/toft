package sdk

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The R transport's single resend, and a success that is not Toft's API —
// the cases sdk/go/resend_test.go, the Node and the Python suites run too.
// BB-002: a 5xx is never resent for a write without an Idempotency-Key.
// BB-003: a timeout is not resent, and the call is held to one bound. BB-014:
// a 200 web page is not_toft, coded and not retryable, with a hint.
func TestTheRTransportResendsOnlyWhatCannotHaveReachedAHandler(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	var calls atomic.Int32
	var mode atomic.Value
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Retry-After", "0")
		switch mode.Load().(string) {
		case "500":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"boom"}`))
		case "502-then-ok":
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				w.WriteHeader(502)
				w.Write([]byte(`{"error":"gateway"}`))
				return
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"run_id":"run_1","status":"queued"}`))
		case "429-then-ok":
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				w.WriteHeader(429)
				w.Write([]byte(`{"error":"slow","code":"rate_limited","retryable":true}`))
				return
			}
			w.Write([]byte(`{"version":1,"data":{"a":1}}`))
		case "429x3-then-ok":
			w.Header().Set("Content-Type", "application/json")
			if n <= 3 {
				w.WriteHeader(429)
				w.Write([]byte(`{"error":"slow","code":"rate_limited","retryable":true}`))
				return
			}
			w.Write([]byte(`{"version":1,"data":{"a":1}}`))
		case "hang":
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(20 * time.Second):
			}
		case "html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<!doctype html><html>Sign in</html>"))
		}
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + ln.Addr().String()
	ln.Close()

	run := func(t *testing.T, url, expr string) string {
		t.Helper()
		script := `
for (f in list.files("R", full.names = TRUE)) source(f)
DEFAULT_TIMEOUT <- 1
DEFAULT_RETRY_AFTER <- 0.2
RATE_LIMIT_JITTER <- 0.05  # three waits must fit the one-second bound above
started <- Sys.time()
out <- tryCatch({ force(` + expr + `); "ok||||" }, toft_error = function(e)
  sprintf("err|%s|%s|%s|%s", e$code, e$retryable, e$status, if (is.null(e$hint)) "" else "hint"),
  error = function(e) paste0("raw|", conditionMessage(e)))
cat(sprintf("%s|%.2f\n", out, as.numeric(Sys.time() - started, units = "secs")))
`
		cmd := exec.Command("Rscript", "-e", script)
		cmd.Dir = "r"
		cmd.Env = append(os.Environ(), "TOFT_API_URL="+url, "TOFT_APP_TOKEN=tofta_test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Rscript: %v\n%s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return lines[len(lines)-1]
	}
	field := func(line string, i int) string { return strings.Split(line, "|")[i] }

	for _, c := range []struct {
		name, mode, expr, want string
		calls                  int32
	}{
		{"job_start 500", "500", `job_start("echo")`, "err||TRUE|500|", 1},
		{"app_data_set 500", "500", `app_data_set(list(a = 1))`, "err||TRUE|500|", 1},
		{"notify slack 500", "500", `notify("slack", text = "x")`, "err||TRUE|500|", 1},
		{"keyed job_start 502", "502-then-ok", `job_start("echo", idempotency_key = "k1")`, "ok||||", 2},
		{"app_data_set 429", "429-then-ok", `app_data_set(list(a = 1))`, "ok||||", 2},
		{"app_data_get 429 x3", "429x3-then-ok", `app_data_get()`, "ok||||", 4},
		{"job_start timeout", "hang", `job_start("echo")`, "err||TRUE|NA|", 1},
		{"html 200", "html", `app_data_get()`, "err|not_toft|FALSE|200|hint", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls.Store(0)
			mode.Store(c.mode)
			line := run(t, srv.URL, c.expr)
			got := strings.Join(strings.Split(line, "|")[:5], "|")
			if got != c.want {
				t.Errorf("got %q, want %q (line %q)", got, c.want, line)
			}
			if n := calls.Load(); n != c.calls {
				t.Errorf("%d requests, want %d", n, c.calls)
			}
			if c.mode == "hang" {
				if secs, _ := time.ParseDuration(field(line, 5) + "s"); secs > 3*time.Second {
					t.Errorf("took %s, want about the one-second bound", secs)
				}
			}
		})
	}

	t.Run("refused connection is resent", func(t *testing.T) {
		line := run(t, refused, `job_start("echo")`)
		if !strings.HasPrefix(line, "err||TRUE|NA|") {
			t.Fatalf("got %q", line)
		}
		// The resend waits DEFAULT_RETRY_AFTER (0.2 s) first.
		if secs, _ := time.ParseDuration(field(line, 5) + "s"); secs < 200*time.Millisecond {
			t.Errorf("took %s — no resend happened", secs)
		}
	})
}

package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"toft/internal/toolchain"
)

// The runner's interface *is* the versioned callback contract, so these are
// contract tests against a stub platform: what it sends, what it does with the
// answers, and — the part that matters most — what it refuses to pass on to
// app code.

// stubPlatform records what a runner sent and answers as the platform would.
type stubPlatform struct {
	server *httptest.Server

	mu          sync.Mutex
	bootstraps  int
	logs        []LogBatch
	logBodies   []int
	consumption []ConsumptionReport
	progress    []int
	results     []ResultReport
	// authSeen records the credential presented on each path, so a test can
	// prove the two tokens are not interchangeable in practice.
	authSeen map[string]string

	response BootstrapResponse
	// version overrides what the platform claims to speak.
	version string

	// requiredRunToken, when set, is the only run token the reporting
	// endpoints accept — everything else gets the 401 an expired one gets.
	// exchanged counts the exchanges served, and nextRunToken is what the next
	// one hands back.
	requiredRunToken string
	exchanged        int
	nextRunToken     string
	// refuseExchange answers the exchange the way the platform answers a run
	// asking for one.
	refuseExchange bool
	exchangeAuth   string
}

func newStubPlatform(t *testing.T, resp BootstrapResponse) *stubPlatform {
	t.Helper()
	s := &stubPlatform{response: resp, authSeen: map[string]string{}, version: ContractVersion}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runner/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.bootstraps++
		s.authSeen["bootstrap"] = r.Header.Get("Authorization")
		out := s.response
		out.Version = s.version
		s.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	})
	// The exchange: the identity in, a fresh run token out. Nothing else —
	// which is the property the platform handler is written to keep.
	mux.HandleFunc("/api/v1/runner/token", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.exchanged++
		s.exchangeAuth = r.Header.Get("Authorization")
		if s.refuseExchange {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		s.requiredRunToken = s.nextRunToken
		json.NewEncoder(w).Encode(TokenResponse{
			Version: s.version, RunToken: s.nextRunToken,
		})
	})
	mux.HandleFunc("/api/v1/runner/logs", func(w http.ResponseWriter, r *http.Request) {
		if s.rejects(w, r) {
			return
		}
		var b LogBatch
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &b)
		s.mu.Lock()
		s.logs = append(s.logs, b)
		s.logBodies = append(s.logBodies, len(body))
		s.authSeen["logs"] = r.Header.Get("Authorization")
		s.mu.Unlock()
	})
	mux.HandleFunc("/api/v1/runner/consumption", func(w http.ResponseWriter, r *http.Request) {
		if s.rejects(w, r) {
			return
		}
		var c ConsumptionReport
		json.NewDecoder(r.Body).Decode(&c)
		s.mu.Lock()
		s.consumption = append(s.consumption, c)
		s.authSeen["consumption"] = r.Header.Get("Authorization")
		s.mu.Unlock()
	})
	mux.HandleFunc("/api/v1/runner/progress", func(w http.ResponseWriter, r *http.Request) {
		if s.rejects(w, r) {
			return
		}
		var p ProgressReport
		json.NewDecoder(r.Body).Decode(&p)
		s.mu.Lock()
		s.progress = append(s.progress, p.Percent)
		s.mu.Unlock()
	})
	mux.HandleFunc("/api/v1/runner/result", func(w http.ResponseWriter, r *http.Request) {
		if s.rejects(w, r) {
			return
		}
		var res ResultReport
		json.NewDecoder(r.Body).Decode(&res)
		s.mu.Lock()
		s.results = append(s.results, res)
		s.authSeen["result"] = r.Header.Get("Authorization")
		s.mu.Unlock()
	})

	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// rejects is the platform refusing a stale credential: the 401 an expired run
// token gets, which is what the whole refresh path turns on.
func (s *stubPlatform) rejects(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	want := s.requiredRunToken
	s.mu.Unlock()
	if want == "" || r.Header.Get("Authorization") == "Bearer "+want {
		return false
	}
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"unauthorized"}`))
	return true
}

func (s *stubPlatform) snapshot() ([]LogBatch, []ConsumptionReport, []ResultReport, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	logs := append([]LogBatch(nil), s.logs...)
	cons := append([]ConsumptionReport(nil), s.consumption...)
	res := append([]ResultReport(nil), s.results...)
	auth := map[string]string{}
	for k, v := range s.authSeen {
		auth[k] = v
	}
	return logs, cons, res, auth
}

func (s *stubPlatform) allLines() []string {
	logs, _, _, _ := s.snapshot()
	var out []string
	for _, b := range logs {
		for _, l := range b.Lines {
			out = append(out, l.Line)
		}
	}
	return out
}

// tarball builds a gzipped tar of files, and returns it with its digest.
func tarball(t *testing.T, files map[string]string, modes map[string]int64) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		mode := int64(0o644)
		if m, ok := modes[name]; ok {
			mode = m
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()

	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// artifactServer serves a tarball at a URL, standing in for a presigned link.
func artifactServer(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/release.tar.gz"
}

// --- Credential narrowing ---

// The security property the whole two-token design exists for: app code must
// never be able to re-fetch the app's decrypted environment. The runner
// consumes the bootstrap token once and drops it, and exports only the
// narrowed run token.
func TestAppCodeNeverSeesTheBootstrapToken(t *testing.T) {
	body, digest := tarball(t, map[string]string{
		// A "job" that prints its whole environment back to us.
		"dump.sh": "#!/bin/sh\nenv\n",
	}, map[string]int64{"dump.sh": 0o755})

	stub := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "narrowed-run-token", RunID: "run-1", JobName: "dump",
		Command: "sh dump.sh",
		Env:     map[string]string{"APP_SECRET": "hunter2"},
	})

	dir := t.TempDir()
	err := Run(context.Background(), Options{
		CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "the-bootstrap-token"),
		Mode: "job", WorkDir: dir, WorkloadName: "demo-abc123",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	env := strings.Join(stub.allLines(), "\n")

	// The identity must be nowhere in the app's environment. App code in the
	// pod can read the projected file — that is the trade the design records —
	// but the runner must not hand it on, which is a different and still
	// enforceable thing.
	if strings.Contains(env, "the-bootstrap-token") {
		t.Error("the workload identity reached app code's environment")
	}
	if strings.Contains(env, "TOFT_BOOTSTRAP_TOKEN") {
		t.Error("the retired bootstrap variable is set in the app environment")
	}
	// The narrowed one is present, because a job reports progress with it.
	if !strings.Contains(env, "narrowed-run-token") {
		t.Error("the run token was not exported; the job cannot report anything")
	}
	// And the app's own secrets are, because that is the point of bootstrap.
	if !strings.Contains(env, "hunter2") {
		t.Error("the app's environment did not reach it")
	}
}

// Bootstrap is one-shot: after it, even the runner cannot re-fetch secrets, so
// a later compromise of the runner is no worse than a compromise of the app.
func TestBootstrapCannotBeRepeated(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "run-token"})
	c := NewClient(stub.server.URL, identityFile(t, "boot-token"))

	if _, err := c.Bootstrap(context.Background(), "demo-abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bootstrap(context.Background(), "demo-abc"); err == nil {
		t.Fatal("a second bootstrap succeeded; the credential was not dropped")
	}
	if c.RunToken() != "run-token" {
		t.Errorf("run token = %q", c.RunToken())
	}
}

// The cliff this whole exchange exists to remove: a service workload's run
// token expires while the workload goes on running, and before the exchange the
// runner had no way back — it bootstraps once, so every report from that minute
// on answered 401, forever.
func TestAServiceExchangesAnExpiredRunToken(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "first"})
	stub.nextRunToken = "second"
	path := identityFile(t, "identity-v1")
	c := NewClient(stub.server.URL, path)
	ctx := context.Background()

	if _, err := c.Bootstrap(ctx, "demo-abc"); err != nil {
		t.Fatal(err)
	}

	// The token the runner holds stops being accepted, which is exactly what
	// an expiry looks like from here.
	stub.mu.Lock()
	stub.requiredRunToken = "not-what-the-runner-holds"
	stub.mu.Unlock()

	// kubelet rotates the identity file under the running pod. The exchange
	// must present what is on disk *now*, not what was read at process start —
	// caching it would put the same expiry back one layer down.
	if err := os.WriteFile(path, []byte("identity-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.ReportConsumption(ctx, ConsumptionReport{CPUSeconds: 1}); err != nil {
		t.Fatalf("consumption report after expiry: %v", err)
	}

	_, cons, _, auth := stub.snapshot()
	if len(cons) != 1 {
		t.Fatalf("consumption reports landed = %d, want 1", len(cons))
	}
	if c.RunToken() != "second" {
		t.Errorf("run token = %q, want the exchanged one", c.RunToken())
	}
	if auth["consumption"] != "Bearer second" {
		t.Errorf("the retry presented %q", auth["consumption"])
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.exchanged != 1 {
		t.Errorf("exchanges = %d, want 1", stub.exchanged)
	}
	if stub.exchangeAuth != "Bearer identity-v2" {
		t.Errorf("the exchange presented %q, want the rotated identity", stub.exchangeAuth)
	}
	// And it is not a second bootstrap: the app's environment was fetched once.
	if stub.bootstraps != 1 {
		t.Errorf("bootstraps = %d; a refresh must not re-fetch the app's secrets", stub.bootstraps)
	}
}

// Two reporters expire on the same tick — logs and consumption run on separate
// goroutines — and must not each mint a token, because the second would replace
// a credential the first had just installed.
func TestConcurrentExpiriesExchangeOnce(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "first"})
	stub.nextRunToken = "second"
	c := NewClient(stub.server.URL, identityFile(t, "identity"))
	ctx := context.Background()

	if _, err := c.Bootstrap(ctx, "demo-abc"); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.requiredRunToken = "not-what-the-runner-holds"
	stub.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.ReportConsumption(ctx, ConsumptionReport{CPUSeconds: 1})
		}()
	}
	wg.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.exchanged != 1 {
		t.Errorf("exchanges = %d, want 1", stub.exchanged)
	}
}

// A run's token is bounded by the run on purpose, so that a token found after
// the run is inert. The runner does not even ask.
func TestARunNeverExchangesItsToken(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", RunToken: "first", RunID: "run-1",
	})
	stub.nextRunToken = "second"
	c := NewClient(stub.server.URL, identityFile(t, "identity"))
	ctx := context.Background()

	if _, err := c.Bootstrap(ctx, "demo-abc"); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.requiredRunToken = "not-what-the-runner-holds"
	stub.mu.Unlock()

	if err := c.ReportResult(ctx, ResultReport{ExitCode: 0}); err == nil {
		t.Fatal("a rejected report succeeded")
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.exchanged != 0 {
		t.Errorf("exchanges = %d; a run's token must not be refreshed", stub.exchanged)
	}
}

// A refusal that survives a fresh credential is not a credential problem. One
// retry, then the error — a report retried forever would be the flood this path
// was written to end.
func TestARefusalThatSurvivesTheExchangeIsReported(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "first"})
	stub.nextRunToken = "second"
	stub.refuseExchange = true
	c := NewClient(stub.server.URL, identityFile(t, "identity"))
	ctx := context.Background()

	if _, err := c.Bootstrap(ctx, "demo-abc"); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.requiredRunToken = "not-what-the-runner-holds"
	stub.mu.Unlock()

	err := c.ReportConsumption(ctx, ConsumptionReport{CPUSeconds: 1})
	if err == nil {
		t.Fatal("a report the platform refused twice was reported as a success")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("error = %v, want it to still name the rejection", err)
	}
}

// A failure that repeats unchanged says so once and then counts. Before this, a
// consumption reporter that could not authenticate wrote an identical line
// every five seconds for the life of the pod.
func TestARepeatedFailureIsSaidOnceAndCounted(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var r repeatLog
	for i := 0; i < 500; i++ {
		r.say("consumption report: %v", "platform answered 401")
	}
	if got := strings.Count(buf.String(), "401"); got != 1 {
		t.Errorf("lines mentioning the failure = %d, want 1", got)
	}

	// It changes, so the new one is said in full and the old one is counted
	// out rather than dropped.
	r.say("consumption report: %v", "platform answered 500")
	out := buf.String()
	if !strings.Contains(out, "repeated 500 times") {
		t.Errorf("the suppressed count was not reported:\n%s", out)
	}
	if !strings.Contains(out, "500") {
		t.Errorf("the new failure was not said:\n%s", out)
	}

	// And recovery is distinguishable from a log that merely went quiet.
	r.recovered("consumption reporting recovered")
	if !strings.Contains(buf.String(), "recovered") {
		t.Error("recovery was not said")
	}
}

// Every callback after bootstrap presents the *run* token, never the
// bootstrap one.
func TestPostBootstrapCallbacksUseTheNarrowedToken(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "narrowed"})
	c := NewClient(stub.server.URL, identityFile(t, "boot-token"))
	ctx := context.Background()

	if _, err := c.Bootstrap(ctx, "demo-abc"); err != nil {
		t.Fatal(err)
	}
	c.ShipLogs(ctx, LogBatch{Lines: []LogLine{{Seq: 1, Line: "hello"}}})
	c.ReportResult(ctx, ResultReport{ExitCode: 0})

	_, _, _, auth := stub.snapshot()
	if auth["bootstrap"] != "Bearer boot-token" {
		t.Errorf("bootstrap presented %q", auth["bootstrap"])
	}
	if auth["logs"] != "Bearer narrowed" {
		t.Errorf("logs presented %q, want the narrowed token", auth["logs"])
	}
	if auth["result"] != "Bearer narrowed" {
		t.Errorf("result presented %q, want the narrowed token", auth["result"])
	}
}

// The runner strips its own configuration out of the process environment, so
// nothing it read can be inherited by a child.
func TestOptionsFromEnvClearsTheEnvironment(t *testing.T) {
	t.Setenv(EnvCallbackURL, "http://platform")
	t.Setenv(EnvMode, "job")
	t.Setenv(EnvWorkDir, "/w")

	o := OptionsFromEnv()
	if o.Mode != "job" || o.CallbackURL != "http://platform" {
		t.Fatalf("options not read: %+v", o)
	}
	// The identity is a mounted file, not an environment variable, and there is
	// nothing here to strip. That is the contract-version-2 change: a
	// credential the pod spec does not carry is one that cannot expire inside
	// an immutable spec.
	if o.IdentityTokenPath != IdentityTokenPath {
		t.Errorf("identity path = %q, want the projected mount", o.IdentityTokenPath)
	}
	for _, k := range []string{EnvCallbackURL, EnvMode, EnvWorkDir} {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s is still set to %q after reading options", k, v)
		}
	}
}

// A pod with no projected identity is a platform and a runner image that
// disagree about the contract. It fails on the spot and says which file is
// missing, rather than half-working.
func TestAMissingIdentityIsRefused(t *testing.T) {
	err := Run(context.Background(), Options{
		CallbackURL: "http://platform", IdentityTokenPath: filepath.Join(t.TempDir(), "absent"),
		Mode: "serve", WorkDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a workload with no projected identity started anyway")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("the error does not name what was missing: %v", err)
	}
}

// identityFile writes a token where a projected ServiceAccount volume would
// have put one.
func identityFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- Artifact integrity ---

// A corrupt release must never be executed. Verifying before publishing the
// staging tree is what stops a bad transfer becoming a mysterious app bug.
func TestCorruptArtifactIsNeverExecuted(t *testing.T) {
	body, _ := tarball(t, map[string]string{"run.sh": "#!/bin/sh\necho SHOULD-NOT-RUN\n"},
		map[string]int64{"run.sh": 0o755})

	stub := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", Framework: "python",
		ArtifactURL: artifactServer(t, body),
		// A digest that does not match the bytes.
		ArtifactSHA256: strings.Repeat("00", 32),
		RunToken:       "run-token", RunID: "run-1", Command: "sh run.sh",
	})

	err := Run(context.Background(), Options{
		CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo-abc",
	})
	if err == nil {
		t.Fatal("a corrupt release was accepted")
	}

	for _, line := range stub.allLines() {
		if strings.Contains(line, "SHOULD-NOT-RUN") {
			t.Fatal("the app ran despite a failed integrity check")
		}
	}

	_, _, results, _ := stub.snapshot()
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one failure report", results)
	}
	// The stage is what tells an operator whose problem this is.
	if results[0].Stage != StageArtifactFetch {
		t.Errorf("stage = %q, want %q", results[0].Stage, StageArtifactFetch)
	}
}

// A release that escapes its directory is refused. The tar is built from a
// cloned repo, so it is attacker-influenced input.
func TestReleaseCannotEscapeItsDirectory(t *testing.T) {
	for _, name := range []string{"../escaped.txt", "nested/../../escaped.txt", "/etc/cron.d/evil"} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
			tw.Write([]byte("evil"))
			tw.Close()
			gz.Close()

			dest := filepath.Join(t.TempDir(), "release")
			os.MkdirAll(dest, 0o750)
			if err := untarGz(&buf, dest); err == nil {
				t.Fatalf("%q was accepted", name)
			}
		})
	}
}

// A symlink out of the tree is *allowed*, because a Python virtual environment
// cannot be shipped without one: `uv venv` writes
// `venv/bin/python -> /usr/bin/python3`, and refusing that refuses every Python
// release the moment a runner unpacks it. A symlink is inert; what an unpacker
// can be used for is writing outside the tree it was pointed at.
func TestAVenvsInterpreterSymlinkSurvivesUnpacking(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "venv/bin", Mode: 0o755, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{
		Name: "venv/bin/python", Linkname: "/usr/bin/python3",
		Mode: 0o777, Typeflag: tar.TypeSymlink,
	})
	tw.Close()
	gz.Close()

	dest := filepath.Join(t.TempDir(), "release")
	os.MkdirAll(dest, 0o750)
	if err := untarGz(&buf, dest); err != nil {
		t.Fatalf("a venv's interpreter symlink was refused: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dest, "venv/bin/python"))
	if err != nil || got != "/usr/bin/python3" {
		t.Errorf("readlink = %q, %v — want the interpreter link intact", got, err)
	}
}

// The half that actually escapes: a link, and then something written through
// it. filepath.Join cannot see this, because it resolves "etc/passwd" without
// knowing "etc" is a link to /etc — so refusing the *write* is what closes it.
func TestNothingIsWrittenThroughAnEscapingSymlink(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "passwd"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		entries func(*tar.Writer)
	}{
		{"through a linked directory", func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "etc", Linkname: victim, Mode: 0o777, Typeflag: tar.TypeSymlink})
			tw.WriteHeader(&tar.Header{Name: "etc/passwd", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
			tw.Write([]byte("evil"))
		}},
		{"through a linked directory, one level down", func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "a", Mode: 0o755, Typeflag: tar.TypeDir})
			tw.WriteHeader(&tar.Header{Name: "a/etc", Linkname: victim, Mode: 0o777, Typeflag: tar.TypeSymlink})
			tw.WriteHeader(&tar.Header{Name: "a/etc/passwd", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
			tw.Write([]byte("evil"))
		}},
		{"over the link itself", func(tw *tar.Writer) {
			// The last component, which the ancestor check cannot see: O_NOFOLLOW
			// is what refuses this one.
			tw.WriteHeader(&tar.Header{
				Name: "passwd", Linkname: filepath.Join(victim, "passwd"),
				Mode: 0o777, Typeflag: tar.TypeSymlink,
			})
			tw.WriteHeader(&tar.Header{Name: "passwd", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
			tw.Write([]byte("evil"))
		}},
		{"through a chain of links that each look local", func(tw *tar.Writer) {
			// Every hop stays inside the tree except the last, so no single link
			// looks like it escapes. Only resolving the whole chain sees it.
			tw.WriteHeader(&tar.Header{Name: "a", Linkname: "b", Mode: 0o777, Typeflag: tar.TypeSymlink})
			tw.WriteHeader(&tar.Header{Name: "b", Linkname: victim, Mode: 0o777, Typeflag: tar.TypeSymlink})
			tw.WriteHeader(&tar.Header{Name: "a/passwd", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
			tw.Write([]byte("evil"))
		}},
		{"through a hardlink taken past a linked directory", func(tw *tar.Writer) {
			// A hardlink pulls the outside inode *in*, and O_NOFOLLOW cannot
			// refuse the write that follows because a hardlink is not a symlink.
			tw.WriteHeader(&tar.Header{Name: "etc", Linkname: victim, Mode: 0o777, Typeflag: tar.TypeSymlink})
			tw.WriteHeader(&tar.Header{Name: "grab", Linkname: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeLink})
			tw.WriteHeader(&tar.Header{Name: "grab", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
			tw.Write([]byte("evil"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			tc.entries(tw)
			tw.Close()
			gz.Close()

			dest := filepath.Join(t.TempDir(), "release")
			os.MkdirAll(dest, 0o750)
			if err := untarGz(&buf, dest); err == nil {
				t.Error("the write through the symlink was accepted")
			}
			// The refusal is only worth anything if the file outside is intact.
			body, err := os.ReadFile(filepath.Join(victim, "passwd"))
			if err != nil || string(body) != "original" {
				t.Fatalf("the file outside the release was modified: %q, %v", body, err)
			}
		})
	}
}

// --- Failure classification ---

// Each stage has a different owner: the operator's registry, the artifact
// store, the app's build, the app's code, the platform's reachability.
// Collapsing them into an exit code moves debugging into the cloud console.
func TestFailuresAreClassifiedByStage(t *testing.T) {
	t.Run("artifact fetch", func(t *testing.T) {
		stub := newStubPlatform(t, BootstrapResponse{
			AppID: "app-1", Framework: "python", RunToken: "run-token",
			ArtifactURL: "http://127.0.0.1:1/nope.tar.gz", Command: "true",
		})
		Run(context.Background(), Options{
			CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
			Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo",
		})
		_, _, results, _ := stub.snapshot()
		if len(results) != 1 || results[0].Stage != StageArtifactFetch {
			t.Errorf("results = %+v, want an artifact_fetch failure", results)
		}
	})

	t.Run("app exec", func(t *testing.T) {
		body, digest := tarball(t, map[string]string{"x": "y"}, nil)
		stub := newStubPlatform(t, BootstrapResponse{
			AppID: "app-1", Framework: "python", RunToken: "run-token",
			ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
			RunID: "run-1", Command: "exit 3",
		})
		err := Run(context.Background(), Options{
			CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
			Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo",
		})
		if err == nil {
			t.Fatal("a non-zero exit should be an error")
		}
		_, _, results, _ := stub.snapshot()
		if len(results) != 1 {
			t.Fatalf("results = %+v", results)
		}
		// A non-zero exit is the *app's* failure, reported as a code — not
		// dressed up as a platform error.
		if results[0].ExitCode != 3 {
			t.Errorf("exit code = %d, want 3", results[0].ExitCode)
		}
		if results[0].Stage != "" {
			t.Errorf("stage = %q; an app's own non-zero exit is not a runner stage failure", results[0].Stage)
		}
	})

	t.Run("build", func(t *testing.T) {
		body, digest := tarball(t, map[string]string{
			"requirements.txt": "this-package-does-not-exist-xyzzy\n",
		}, nil)
		stub := newStubPlatform(t, BootstrapResponse{
			AppID: "app-1", Framework: "python", RunToken: "run-token",
			ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		})
		Run(context.Background(), Options{
			CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
			Mode: "build", WorkDir: t.TempDir(), WorkloadName: "demo",
		})
		_, _, results, _ := stub.snapshot()
		if len(results) != 1 || results[0].Stage != StageBuild {
			t.Errorf("results = %+v, want a build failure", results)
		}
		// The app's actual error must travel with it, or the developer debugs
		// the platform instead of their code.
		if results[0].Reason == "" {
			t.Error("the build failure carried no reason")
		}
	})
}

// A runner that cannot reach the platform must fail loudly rather than run on
// silently: the reconciler's sweep is what catches the orphan, and it can only
// do that if the workload stops.
func TestUnreachablePlatformFailsLoudly(t *testing.T) {
	defer func(w time.Duration) { bootstrapDialWindow = w }(bootstrapDialWindow)
	bootstrapDialWindow = time.Second
	err := Run(context.Background(), Options{
		CallbackURL: "http://127.0.0.1:1", IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo",
	})
	if err == nil {
		t.Fatal("a runner that cannot reach the platform must not continue")
	}
	if !errors.Is(err, ErrCallbackUnreachable) {
		t.Errorf("err = %v, want ErrCallbackUnreachable", err)
	}
}

// A pod's first dial can be refused while a NetworkPolicy has not yet admitted
// its address; a job pod is not restarted, so the bootstrap redials instead of
// failing the run on it.
func TestBootstrapRedialsAPlatformThatRefusedTheConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // refused until the platform comes up below

	var calls atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(BootstrapResponse{Version: ContractVersion, RunToken: "rt"})
	})}
	go func() {
		time.Sleep(600 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		_ = srv.Serve(l)
	}()
	defer srv.Close()

	c := NewClient("http://"+addr, identityFile(t, "boot"))
	if _, err := c.Bootstrap(context.Background(), "demo"); err != nil {
		t.Fatalf("bootstrap gave up on a platform that came up 600ms later: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the platform saw %d bootstraps, want exactly 1", n)
	}
}

// --- Versioning ---

// Runner and platform deploy independently, so a mismatch is a stated outcome
// rather than best-effort tolerance: a partial upgrade must fail loudly.
func TestVersionMismatchIsRefused(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "rt"})
	stub.version = "99"

	c := NewClient(stub.server.URL, identityFile(t, "boot"))
	_, err := c.Bootstrap(context.Background(), "demo")
	if err == nil {
		t.Fatal("a version mismatch was tolerated")
	}
	var mismatch VersionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a VersionMismatch", err)
	}
	if !strings.Contains(err.Error(), "partial upgrade") {
		t.Errorf("the error should tell an operator what happened: %v", err)
	}
}

func TestCheckVersion(t *testing.T) {
	if err := CheckVersion("1", "1"); err != nil {
		t.Errorf("matching versions: %v", err)
	}
	if err := CheckVersion("", "1"); err == nil {
		t.Error("an unreported version must not pass")
	}
	if err := CheckVersion("1", "2"); err == nil {
		t.Error("a mismatch must not pass")
	}
}

// --- Log shipping ---

// A job in a tight print loop must not turn log ingest into a denial of
// service. Dropping with an explicit count is honest; buffering forever is
// not.
func TestLogShippingCapsAndReportsDrops(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "rt"})
	c := NewClient(stub.server.URL, identityFile(t, "boot"))
	c.Bootstrap(context.Background(), "demo")

	s := &logShipper{client: c, stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)

	for i := 0; i < maxBufferedLines*2; i++ {
		s.Log("stdout", fmt.Sprintf("line %d", i))
	}
	s.Flush(context.Background())

	logs, _, _, _ := stub.snapshot()
	if len(logs) != 1 {
		t.Fatalf("batches = %d, want 1", len(logs))
	}
	if len(logs[0].Lines) != maxBufferedLines {
		t.Errorf("shipped %d lines, want the cap of %d", len(logs[0].Lines), maxBufferedLines)
	}
	if logs[0].Dropped != maxBufferedLines {
		t.Errorf("reported %d drops, want %d — truncation must be explicit", logs[0].Dropped, maxBufferedLines)
	}
	// The *beginning* is kept: the start of a failure explains it, the tail of
	// a runaway loop is noise.
	if logs[0].Lines[0].Line != "line 0" {
		t.Errorf("first line = %q, want the earliest", logs[0].Lines[0].Line)
	}
}

// One interval may hold far more than the ingest route accepts in one body
// (4 MiB). It goes as several shipments, each under the limit, in order — not
// as one the platform refuses whole, which lost the interval silently.
func TestLogShippingSplitsAnIntervalUnderTheIngestLimit(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "rt"})
	c := NewClient(stub.server.URL, identityFile(t, "boot"))
	c.Bootstrap(context.Background(), "demo")

	s := &logShipper{client: c, stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)

	// Quotes escape to two bytes, angle brackets to six: the encoded line is
	// far longer than the raw one.
	big := strings.Repeat("<\"", maxLineBytes/2)[:maxLineBytes]
	for i := 0; i < 400; i++ {
		s.Log("stdout", big)
	}
	s.dropped = 7
	s.Flush(context.Background())

	stub.mu.Lock()
	logs, bodies := stub.logs, stub.logBodies
	stub.mu.Unlock()
	if len(logs) < 2 {
		t.Fatalf("shipments = %d, want the interval split", len(logs))
	}
	seq := 0
	for i, b := range logs {
		if bodies[i] >= 4<<20 {
			t.Errorf("shipment %d is %d bytes, over the ingest limit", i, bodies[i])
		}
		for _, l := range b.Lines {
			seq++
			if l.Seq != seq {
				t.Fatalf("line seq %d, want %d — shipments out of order", l.Seq, seq)
			}
		}
		if want := 0; i < len(logs)-1 && b.Dropped != want {
			t.Errorf("shipment %d carries the drop count; only the last may", i)
		}
	}
	if seq != 400 {
		t.Errorf("delivered %d lines, want 400", seq)
	}
	if logs[len(logs)-1].Dropped != 7 {
		t.Errorf("last shipment dropped = %d, want 7", logs[len(logs)-1].Dropped)
	}
}

func TestSingleEnormousLineIsTruncatedNotDropped(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "rt"})
	c := NewClient(stub.server.URL, identityFile(t, "boot"))
	c.Bootstrap(context.Background(), "demo")

	s := &logShipper{client: c, stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)

	s.Log("stdout", strings.Repeat("x", maxLineBytes*3))
	s.Flush(context.Background())

	logs, _, _, _ := stub.snapshot()
	if len(logs) != 1 || len(logs[0].Lines) != 1 {
		t.Fatalf("logs = %+v", logs)
	}
	line := logs[0].Lines[0].Line
	if len(line) > maxLineBytes+64 {
		t.Errorf("line was not truncated: %d bytes", len(line))
	}
	if !strings.Contains(line, "truncated") {
		t.Error("truncation should be visible in the line itself")
	}
}

// Logs are best-effort: a platform outage drops the batch rather than growing
// memory inside the workload for the length of the outage.
func TestLogFlushFailureDoesNotBlockOrGrow(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", identityFile(t, "boot"))
	c.runToken = "rt"

	s := &logShipper{client: c, stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)

	for i := 0; i < 100; i++ {
		s.Log("stdout", "line")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Flush(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) != 0 {
		t.Errorf("buffer still holds %d lines after a failed flush", len(s.buf))
	}
}

// --- The service contract ---

// The per-framework start invocation must match the shared backend's: an app
// that behaves differently across modes makes the mode a one-way door.
func TestServiceCommandsHonourPortHostAndBasePath(t *testing.T) {
	for _, framework := range []string{"nextjs", "streamlit", "fastapi", "flask", "python", "node", "go", "static", "notebook", "marimo"} {
		t.Run(framework, func(t *testing.T) {
			boot := &BootstrapResponse{
				Framework: framework, Entrypoint: "app.py", BasePath: "/apps/demo",
				Port: 8080, BindAddress: "0.0.0.0",
			}
			name, args, err := serviceCommand(boot, "/workspace/release")
			if err != nil {
				t.Fatalf("serviceCommand: %v", err)
			}
			if name == "" {
				t.Fatal("no command")
			}
			joined := strings.Join(args, " ")

			// An isolated workload binds all interfaces: the network namespace
			// is the boundary, and loopback would make it unreachable through
			// the gateway rather than more secure.
			if strings.Contains(joined, "127.0.0.1") {
				t.Errorf("isolated workload told to bind loopback: %q", joined)
			}
		})
	}

	if _, _, err := serviceCommand(&BootstrapResponse{Framework: "cobol"}, "/workspace/release"); err == nil {
		t.Error("an unsupported framework should be refused, not guessed at")
	}
}

// Toft's own values win over the app's, so an app cannot redefine its
// base path or port out from under the gateway.
func TestPlatformEnvironmentOverridesTheApps(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	boot := &BootstrapResponse{
		AppSlug: "demo", BasePath: "/apps/demo", Port: 8080,
		Env: map[string]string{
			"BASE_PATH": "/hijacked",
			"PORT":      "9999",
			"MY_VALUE":  "kept",
		},
	}

	env := map[string]string{}
	for _, kv := range appEnv(boot, c, "serve", t.TempDir()) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}

	if env["BASE_PATH"] != "/apps/demo" {
		t.Errorf("BASE_PATH = %q; an app must not be able to redefine its mount point", env["BASE_PATH"])
	}
	if env["PORT"] != "8080" {
		t.Errorf("PORT = %q; an app must not be able to move off the port the gateway dials", env["PORT"])
	}
	if env["MY_VALUE"] != "kept" {
		t.Error("the app's own variables were lost")
	}
	if env["HOST"] != "0.0.0.0" {
		t.Errorf("HOST = %q, want all interfaces for an isolated workload", env["HOST"])
	}
}

// The run token reaches app code only in a job. For a service it would be a
// credential on the runner's own ingest routes — the platform log and the
// consumption figures — held by the code those routes report on.
func TestOnlyAJobsCodeHoldsTheRunToken(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	has := func(boot *BootstrapResponse, mode string) bool {
		for _, kv := range appEnv(boot, c, mode, t.TempDir()) {
			if strings.HasPrefix(kv, EnvRunToken+"=") {
				return true
			}
		}
		return false
	}
	if has(&BootstrapResponse{AppSlug: "demo", Port: 8080, AppToken: "tofta_x"}, "serve") {
		t.Error("a service's code was handed the run token")
	}
	if !has(&BootstrapResponse{AppSlug: "demo", RunID: "run-1", JobName: "report"}, "job") {
		t.Error("a job's code was not handed the run token it reports with")
	}
}

// Declared parameters reach a job both as JSON and individually, so a shell
// job need not parse JSON to read one value.
func TestJobParametersReachTheProcess(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	boot := &BootstrapResponse{
		AppSlug: "demo", RunID: "run-1", JobName: "report", Trigger: "cron",
		Params: json.RawMessage(`{"region":"emea","limit":50,"dry_run":true}`),
	}

	env := map[string]string{}
	for _, kv := range appEnv(boot, c, "job", t.TempDir()) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}

	if env[EnvRunID] != "run-1" || env[EnvJobName] != "report" || env[EnvTrigger] != "cron" {
		t.Errorf("job identity missing: %v", env)
	}
	if env["TOFT_PARAM_REGION"] != "emea" {
		t.Errorf("TOFT_PARAM_REGION = %q", env["TOFT_PARAM_REGION"])
	}
	if env["TOFT_PARAM_LIMIT"] != "50" {
		t.Errorf("TOFT_PARAM_LIMIT = %q", env["TOFT_PARAM_LIMIT"])
	}
	if env["TOFT_PARAM_DRY_RUN"] != "true" {
		t.Errorf("TOFT_PARAM_DRY_RUN = %q", env["TOFT_PARAM_DRY_RUN"])
	}
	if env[EnvParams] == "" {
		t.Error("the full parameter JSON should also be available")
	}
}

// A service workload has no run, so it must not be handed job-shaped
// variables that would make app code think it is one.
func TestServiceWorkloadsGetNoJobVariables(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	boot := &BootstrapResponse{AppSlug: "demo", BasePath: "/apps/demo", Port: 8080}

	for _, kv := range appEnv(boot, c, "serve", t.TempDir()) {
		k, _, _ := strings.Cut(kv, "=")
		if k == EnvRunID || k == EnvJobName || k == EnvTrigger {
			t.Errorf("%s was set on a service workload", k)
		}
	}
}

// A whole job, end to end, against the stub platform.
func TestJobRunReportsLogsAndResult(t *testing.T) {
	body, digest := tarball(t, map[string]string{
		"job.sh": "#!/bin/sh\necho starting\necho oops >&2\necho done\n",
	}, map[string]int64{"job.sh": 0o755})

	stub := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "rt", RunID: "run-1", JobName: "report", Command: "sh job.sh",
	})

	if err := Run(context.Background(), Options{
		CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo-abc",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	lines := strings.Join(stub.allLines(), "\n")
	for _, want := range []string{"starting", "done", "oops"} {
		if !strings.Contains(lines, want) {
			t.Errorf("output %q was not shipped. got:\n%s", want, lines)
		}
	}

	// stderr is distinguishable from stdout, so a failure is readable.
	logs, _, results, _ := stub.snapshot()
	sawStderr := false
	for _, b := range logs {
		for _, l := range b.Lines {
			if l.Stream == "stderr" && strings.Contains(l.Line, "oops") {
				sawStderr = true
			}
		}
	}
	if !sawStderr {
		t.Error("stderr was not distinguished from stdout")
	}

	if len(results) != 1 || results[0].ExitCode != 0 {
		t.Errorf("results = %+v, want one success", results)
	}
}

// A run stopped in the middle — a timeout, a cancel — keeps what it printed,
// and what it printed reached the platform while it was still running.
//
// The stop is the cancelled context: the host runner's context is
// signal.NotifyContext, so the SIGTERM a timeout sends cancels it. The flush
// after the app ended used to ship on that context, fail at once and drop the
// batch, and with a five-second ship interval a five-second job shipped nothing
// before it, either: a timed-out run had no log and `--logs` streamed nothing.
func TestAStoppedRunKeepsItsOutputAndStreamedItLive(t *testing.T) {
	body, digest := tarball(t, map[string]string{
		"job.sh": "#!/bin/sh\ni=0\nwhile true; do i=$((i+1)); echo \"tick $i\"; sleep 0.2; done\n",
	}, map[string]int64{"job.sh": 0o755})

	stub := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "rt", RunID: "run-1", JobName: "slow", Command: "sh job.sh",
	})

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
			Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo-abc",
		})
	}()

	// Live: a line is on the platform within a couple of ship intervals of
	// being printed, long before the run ends.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(strings.Join(stub.allLines(), "\n"), "tick 1") {
		if time.Now().After(deadline) {
			t.Fatalf("nothing was shipped while the run was going; got %q", stub.allLines())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Let a few more lines accumulate unshipped, then stop the run.
	time.Sleep(logShipInterval / 2)
	stop()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the runner did not end after its context was cancelled")
	}

	// Kept: every tick the app printed reached the platform, the last ones
	// included — the run printed at least a few after the first shipment.
	lines := stub.allLines()
	last := 0
	for _, l := range lines {
		var n int
		if _, err := fmt.Sscanf(l, "tick %d", &n); err == nil && n > last {
			last = n
		}
	}
	if last < 3 {
		t.Fatalf("shipped up to tick %d; the lines printed before the stop were lost:\n%s",
			last, strings.Join(lines, "\n"))
	}
	for i := 1; i <= last; i++ {
		want := fmt.Sprintf("tick %d", i)
		found := false
		for _, l := range lines {
			if l == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q was printed and never shipped", want)
		}
	}
}

// A flush handed an already-cancelled context still ships: it is the flush
// after a stop, and the stop is what cancelled it.
func TestAFlushOnACancelledContextStillShips(t *testing.T) {
	stub := newStubPlatform(t, BootstrapResponse{AppID: "app-1", RunToken: "rt"})
	c := NewClient(stub.server.URL, identityFile(t, "boot"))
	c.Bootstrap(context.Background(), "demo")

	s := &logShipper{client: c, isRun: true, stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)
	s.Log("stdout", "the line that explains the timeout")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Flush(ctx)

	if got := stub.allLines(); len(got) != 1 || got[0] != "the line that explains the timeout" {
		t.Fatalf("shipped %q, want the buffered line", got)
	}
}

// What a job needs beyond its environment: somewhere to write its output. It
// arrives as a presigned URL, so the job needs no object-store credential —
// one that could write its own output could read every other app's data. The
// run's deadline and the run it repeats arrive beside it.
func TestJobsGetAnOutputURINotACredential(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	boot := &BootstrapResponse{
		AppSlug: "demo", RunID: "run-1", JobName: "report", RerunOf: "run-0",
		Deadline:  "2026-10-22T03:45:00Z",
		OutputURL: "https://store.example/runs/app-1/run-1/outputs/result?X-Amz-Signature=def",
		Env:       map[string]string{"APP_DB_URL": "postgres://app-own-db"},
	}

	env := map[string]string{}
	for _, kv := range appEnv(boot, c, "job", t.TempDir()) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}

	if env[EnvRerunOf] != "run-0" || env[EnvDeadline] != boot.Deadline {
		t.Errorf("%s = %q, %s = %q", EnvRerunOf, env[EnvRerunOf], EnvDeadline, env[EnvDeadline])
	}
	if env[EnvOutputURI] != boot.OutputURL {
		t.Errorf("%s = %q", EnvOutputURI, env[EnvOutputURI])
	}

	// The app's *own* credentials are legitimately present — those are its
	// managed env vars and it needs them. What must never appear is a
	// platform-wide object-store credential.
	if env["APP_DB_URL"] == "" {
		t.Error("the app's own environment did not reach the job")
	}
	for _, banned := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if _, present := env[banned]; present {
			t.Errorf("%s reached job code — it could read every other app's data", banned)
		}
	}
}

// A service workload has no run, so it gets no run I/O either.
func TestServiceWorkloadsGetNoRunIO(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	boot := &BootstrapResponse{
		AppSlug: "demo", Port: 8080,
		OutputURL: "https://store.example/should-not-appear",
	}
	for _, kv := range appEnv(boot, c, "serve", t.TempDir()) {
		k, _, _ := strings.Cut(kv, "=")
		if k == EnvOutputURI {
			t.Errorf("%s was set on a service workload", k)
		}
	}
}

// A release built on the platform host does not necessarily run inside the
// runner image. `uv venv` records the absolute path of the interpreter it was
// built against — `/usr/bin/python3.11` on the host — and the family image is
// `python:3.12-slim`, so the link dangles here and the app's dependencies are
// invisible. The repair exists for that; these fix which trees it fires on.
func TestUnrunnableSpotsAReleaseBuiltAgainstAnotherInterpreter(t *testing.T) {
	cases := []struct {
		name      string
		framework string
		// setup builds the release tree as it would arrive.
		setup func(t *testing.T, dir string)
		want  bool
	}{
		{"a venv whose interpreter link dangles", "streamlit", func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "venv/bin"), 0o755)
			// Exactly what the platform host writes, and what is absent here.
			if err := os.Symlink("/usr/bin/python3.11", filepath.Join(dir, "venv/bin/python")); err != nil {
				t.Skipf("symlinks unsupported here: %v", err)
			}
		}, true},
		{"a venv built by this image", "streamlit", func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "venv/bin"), 0o755)
			real := filepath.Join(dir, "venv/bin/python3.12")
			os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755)
			if err := os.Symlink(real, filepath.Join(dir, "venv/bin/python")); err != nil {
				t.Skipf("symlinks unsupported here: %v", err)
			}
		}, false},
		{"no venv at all", "fastapi", func(t *testing.T, dir string) {}, true},
		{"node without its dependencies", "node", func(t *testing.T, dir string) {}, true},
		{"node with them", "node", func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
		}, false},
		{"go without its binary", "go", func(t *testing.T, dir string) {}, true},
		{"go with it", "go", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "toft-app"), []byte("ELF"), 0o755)
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			got, why := Unrunnable(tc.framework, dir)
			if got != tc.want {
				t.Errorf("unrunnable = %v (%q), want %v", got, why, tc.want)
			}
			if got && why == "" {
				t.Error("an unrunnable release was not explained")
			}
		})
	}
}

// The property that keeps this a repair rather than a build step: a tree that
// already runs here is left exactly as fetched. Were it otherwise, every
// isolated workload would pay a dependency resolution it does not need — and
// this test needs no network precisely because nothing should be rebuilt.
func TestRepairLeavesARunnableReleaseAlone(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "venv/bin"), 0o755)
	real := filepath.Join(dir, "venv/bin/python3.12")
	os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755)
	if err := os.Symlink(real, filepath.Join(dir, "venv/bin/python")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	marker := filepath.Join(dir, "venv/lib/marker")
	os.MkdirAll(filepath.Dir(marker), 0o755)
	os.WriteFile(marker, []byte("installed"), 0o644)

	c := &Client{baseURL: "http://platform", runToken: "rt"}
	s := &logShipper{client: c, stop: make(chan struct{}), done: make(chan struct{})}
	if err := repairRelease(context.Background(), &BootstrapResponse{Framework: "streamlit"}, dir, s); err != nil {
		t.Fatalf("repair of a runnable release failed: %v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "installed" {
		t.Fatalf("the existing environment was disturbed: %q, %v", body, err)
	}
}

// A service workload is handed the app's identity — the outbound credential and
// the id it belongs to; a job is handed neither.
//
// The asymmetry is ADR §27 D1 rather than an accident: the argument that a
// scheduled task must be an HTTP trigger rests on a job workload holding no app
// token, so a report a job produces has nothing able to mail it. The runner
// exports what the platform sent and applies no rule of its own about which —
// two places deciding that is how they come to disagree.
//
// TOFT_APP_ID travels on the same condition and under the same name the
// shared backend uses, so an SDK reads one variable wherever the app runs. The
// isolated runner had `boot.AppID` in hand from the start and did not export
// it, which made the two backends hand the same app a different environment by
// one name — found by RUN.13 the first time it ran against the isolated target.
func TestTheAppTokenReachesAServiceAndNotAJob(t *testing.T) {
	body, digest := tarball(t, map[string]string{
		"dump.sh": "#!/bin/sh\nenv\n",
	}, map[string]int64{"dump.sh": 0o755})

	// A job: the platform sends no app token, so app code sees none.
	job := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "narrowed", RunID: "run-1", JobName: "dump", Command: "sh dump.sh",
	})
	if err := Run(context.Background(), Options{
		CallbackURL: job.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo-abc123",
	}); err != nil {
		t.Fatalf("job run: %v", err)
	}
	jobEnv := strings.Join(job.allLines(), "\n")
	if strings.Contains(jobEnv, EnvAppToken) {
		t.Errorf("%s reached a job workload; §27 D1 rests on it not being there", EnvAppToken)
	}
	// Absent, not blank: a job holds a run token and no app token, so there is
	// no app identity for it to carry.
	if strings.Contains(jobEnv, EnvAppID) {
		t.Errorf("%s reached a job workload; it travels with the app token or not at all", EnvAppID)
	}

	// A service: the platform sends one, and it is exported under the same name
	// the shared backend uses, so an SDK reads one variable in both modes.
	svc := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "narrowed", AppToken: "tofta_the-apps-own-token",
		Command: "sh dump.sh",
	})
	// Read off the tee rather than off the wire. A service has no run to
	// attach lines to, so since the platform log landed it does not *ship* its
	// own output at all — the pod's stdout is where a service's output lives
	// and the only place this can be observed. The subject of the test is
	// unchanged: what the app process was handed in its environment.
	var tee strings.Builder
	restore := logTee
	logTee = &tee
	t.Cleanup(func() { logTee = restore })

	if err := Run(context.Background(), Options{
		CallbackURL: svc.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo-abc123",
	}); err != nil {
		t.Fatalf("service-shaped run: %v", err)
	}
	if lines := svc.allLines(); strings.Contains(strings.Join(lines, "\n"), "tofta_the-apps-own-token") {
		t.Error("a service shipped its own output; the platform discards it and the pod's log is where it belongs")
	}
	env := tee.String()
	if !strings.Contains(env, "tofta_the-apps-own-token") {
		t.Error("the app token was not exported; the app has no platform identity")
	}
	if !strings.Contains(env, EnvAppToken+"=") {
		t.Errorf("the app token is not under %s", EnvAppToken)
	}
	if !strings.Contains(env, EnvAppID+"=app-1") {
		t.Errorf("the app id is not exported as %s=app-1; the shared backend sets it and the "+
			"two backends must hand the same app the same environment", EnvAppID)
	}
}

// The confirm half of decide-then-confirm: this image against what the platform
// was told it is. The last time these drifted, the same app got Python 3.11 or
// 3.12 depending on which execution mode it landed in, and nothing held the two
// in step but a comment in each Dockerfile.
func TestVerifyRuntimeVersion(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "python3")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'Python 3.13.7'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// internal/toolchain probes once per process, which is right for a runner
	// that starts, checks itself and execs an app — and wrong for a test that
	// has to declare its own. This used to require being the first thing in the
	// package to ask about Python, which stopped being true the day a repair
	// started resolving the interpreter it rebuilds a venv with.
	t.Setenv("PATH", dir)
	toolchain.ResetForTest()

	shipper := newLogShipper(NewClient("http://127.0.0.1:1", identityFile(t, "t")), true)
	defer shipper.Close()

	if err := verifyRuntimeVersion(&BootstrapResponse{
		RuntimeLanguage: "python", RuntimeVersion: "3.13",
	}, shipper); err != nil {
		t.Errorf("a matching image was refused: %v", err)
	}

	err := verifyRuntimeVersion(&BootstrapResponse{
		RuntimeLanguage: "python", RuntimeVersion: "3.12",
	}, shipper)
	if err == nil {
		t.Fatal("an image that is not what the platform was told it is started anyway")
	}
	for _, want := range []string{"3.12", "3.13.7", "RUNNER_PYTHON_VERSION", "RUNNER_IMAGE_PYTHON"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

// The declaration is a list, and every series in it has to be here (D19).
//
// Checking only the first would confirm the one version that was never in
// doubt: the default is the interpreter on the image's PATH, and it is the
// *others* an operator adds by hand and can therefore leave out. An image with
// the default alone, declared as carrying three, is one the platform will
// resolve an app onto a series no pod can run.
//
// R is the family this exists for, so it is the family it is tested on.
//
// Against the policy rather than through the probe: internal/toolchain answers
// once per process, so a test that reached it would depend on what else in this
// package asked first and on which interpreters the build machine carries.
func TestVerifyRuntimeVersionRequiresEveryDeclaredSeries(t *testing.T) {
	// What the image has. 4.3 is deliberately absent from it.
	have := []toolchain.Version{
		{Language: toolchain.R, Version: "4.5.0", Path: "/usr/bin/Rscript", Default: true},
		{Language: toolchain.R, Version: "4.4.3", Path: "/opt/R/4.4.3/bin/Rscript"},
	}

	for _, tc := range []struct {
		name          string
		declared      string
		wantConfirmed []string
		wantMissing   []string
	}{
		// One version is the same statement with one entry, which is what lets
		// an install that declared a single version keep meaning it.
		{"the single-version spelling still means itself", "4.5", []string{"4.5.0"}, nil},
		{"every declared series present", "4.5,4.4", []string{"4.5.0", "4.4.3"}, nil},
		// The one a list makes possible, and the whole reason for the loop: the
		// default is there and the series somebody added by hand is not.
		{"the default alone is not enough", "4.5,4.4,4.3", []string{"4.5.0", "4.4.3"}, []string{"4.3"}},
		{"nothing declared is not a claim", "", nil, nil},
		// Patch never matters: 4.5.0 and 4.5.1 are one answer to every question
		// this platform asks, so a declaration naming either is satisfied.
		{"matched on the series, not the patch", "4.5.1", []string{"4.5.0"}, nil},
		// Whitespace is a person writing a list, not a different declaration.
		{"spaces around a comma are not entries", "4.5, 4.4", []string{"4.5.0", "4.4.3"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confirmed, missing := matchDeclaredSeries(toolchain.R, tc.declared, have)
			if !slices.Equal(confirmed, tc.wantConfirmed) {
				t.Errorf("confirmed %v, want %v", confirmed, tc.wantConfirmed)
			}
			if !slices.Equal(missing, tc.wantMissing) {
				t.Errorf("missing %v, want %v", missing, tc.wantMissing)
			}
		})
	}

	// The refusal has to name the series that is missing rather than only the
	// list that was asked for: with three declared, "does not match" sends
	// somebody to check all three.
	shipper := newLogShipper(NewClient("http://127.0.0.1:1", identityFile(t, "t")), true)
	defer shipper.Close()
	err := verifyRuntimeVersion(&BootstrapResponse{
		RuntimeLanguage: toolchain.R,
		// A series no machine running these tests will have, so the refusal is
		// the same wherever this runs.
		RuntimeVersion: "0.1",
	}, shipper)
	if err == nil {
		t.Fatal("an image missing a declared series started anyway — the platform will " +
			"resolve an app to it and this pod cannot run it")
	}
	for _, want := range []string{"0.1", "RUNNER_R_VERSION", "RUNNER_IMAGE_R"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

// Absent is not a claim this can contradict. An install that has not set
// RUNNER_*_VERSION has said nothing, and a static or Go workload reaches here
// with nothing declared by construction — one has no interpreter, the other
// leaves its toolchain on the build host.
func TestNothingDeclaredIsNotRefused(t *testing.T) {
	shipper := newLogShipper(NewClient("http://127.0.0.1:1", identityFile(t, "t")), true)
	defer shipper.Close()

	for _, boot := range []*BootstrapResponse{
		{},
		{RuntimeLanguage: "python"},
		{RuntimeVersion: "3.13"},
	} {
		if err := verifyRuntimeVersion(boot, shipper); err != nil {
			t.Errorf("%+v was refused: %v", boot, err)
		}
	}
}

// A service does not ship what the platform discards, and still tees it.
//
// Before this, every isolated service sent its entire stdout across the network
// so the platform could write it into its *own* log one line at a time and move
// on. Both copies existed; neither was reachable. The tee is what makes the
// pod's log worth reading and is where a service's output belongs.
func TestAServiceShipsOnlyThePlatformsVoice(t *testing.T) {
	var tee strings.Builder
	restore := logTee
	logTee = &tee
	t.Cleanup(func() { logTee = restore })

	for _, tc := range []struct {
		name      string
		isRun     bool
		wantAppOn bool
	}{
		{"a service", false, false},
		{"a run", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tee.Reset()
			s := newLogShipper(NewClient("http://127.0.0.1:1", identityFile(t, "t")), tc.isRun)
			defer s.Close()

			s.Log("stdout", "==> Fetching release")
			s.Pump("stdout", strings.NewReader("the app said something\n"))

			s.mu.Lock()
			buffered := append([]LogLine(nil), s.buf...)
			s.mu.Unlock()

			var sawApp, sawPlatform bool
			for _, l := range buffered {
				switch l.Source {
				case LogSourceApp:
					sawApp = true
				case LogSourcePlatform:
					sawPlatform = true
				}
			}
			if !sawPlatform {
				t.Error("the platform's own line was not shipped; it is the durable half")
			}
			if sawApp != tc.wantAppOn {
				t.Errorf("app line shipped = %v, want %v", sawApp, tc.wantAppOn)
			}
			// Whatever the wire carries, the pod's log carries both.
			teed := tee.String()
			if !strings.Contains(teed, "the app said something") {
				t.Error("the app's line did not reach the tee; the pod's log is where it lives")
			}
			if !strings.Contains(teed, "==> Fetching release") {
				t.Error("the platform's line did not reach the tee")
			}
		})
	}
}

// The source label survives the wire. The tee's marker is on the *pod's* copy,
// and nothing above the seam can read a line the pod no longer holds.
func TestTheShippedLineCarriesItsSource(t *testing.T) {
	s := newLogShipper(NewClient("http://127.0.0.1:1", identityFile(t, "t")), true)
	defer s.Close()
	s.Log("stderr", "ERROR at app_exec: exit 1")
	s.Pump("stdout", strings.NewReader("hello\n"))

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) != 2 {
		t.Fatalf("buffered %d lines, want 2", len(s.buf))
	}
	if s.buf[0].Source != LogSourcePlatform {
		t.Errorf("the platform's line shipped as %q", s.buf[0].Source)
	}
	if s.buf[1].Source != LogSourceApp {
		t.Errorf("the app's line shipped as %q", s.buf[1].Source)
	}
}

// A job's command is the app's own — `python jobs/report.py` is what the manual
// tells an author to write — and nothing the platform writes names the release's
// interpreter for it. Without the venv in front of PATH that `python` is the
// image's, which has neither the app's dependencies nor the SDK the build
// installed: `import toft` fails on a release whose build log lists it
// installing cleanly.
func TestAPythonWorkloadFindsItsOwnEnvironmentOnPath(t *testing.T) {
	dir := t.TempDir()
	venvBin := filepath.Join(dir, "venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venvBin, "python"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{baseURL: "http://platform", runToken: "rt"}

	for _, framework := range []string{"flask", "fastapi", "streamlit", "python", "notebook", "marimo"} {
		for _, mode := range []string{"job", "serve"} {
			env := map[string]string{}
			for _, kv := range appEnv(&BootstrapResponse{AppSlug: "demo", Framework: framework}, c, mode, dir) {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			if got := env["PATH"]; !strings.HasPrefix(got, venvBin+string(os.PathListSeparator)) && got != venvBin {
				t.Errorf("%s/%s: PATH = %q; the release's venv must come first", framework, mode, got)
			}
			if got := env["VIRTUAL_ENV"]; got != filepath.Join(dir, "venv") {
				t.Errorf("%s/%s: VIRTUAL_ENV = %q", framework, mode, got)
			}
		}
	}
}

// The build is what *creates* the venv, and a repair discards the stale one
// before rebuilding. Pointing uv at either through the environment is how a
// build resolves against something that will not exist when it finishes.
func TestABuildIsNotGivenTheVenvItIsAboutToCreate(t *testing.T) {
	dir := t.TempDir()
	venvBin := filepath.Join(dir, "venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venvBin, "python"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{baseURL: "http://platform", runToken: "rt"}

	for _, kv := range appEnv(&BootstrapResponse{AppSlug: "demo", Framework: "flask"}, c, "build", dir) {
		k, v, _ := strings.Cut(kv, "=")
		if k == "VIRTUAL_ENV" {
			t.Errorf("VIRTUAL_ENV = %q on a build", v)
		}
		if k == "PATH" && strings.Contains(v, venvBin) {
			t.Errorf("PATH = %q on a build; the venv is what the first step creates", v)
		}
	}
}

// A release with no venv — a family that has none, or a tree whose interpreter
// link dangles and that `Unrunnable` is about to send for repair — gets no
// prefix at all rather than one pointing at nothing.
func TestNoVenvMeansNoPrefix(t *testing.T) {
	dir := t.TempDir()
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	for _, kv := range appEnv(&BootstrapResponse{AppSlug: "demo", Framework: "flask"}, c, "job", dir) {
		k, v, _ := strings.Cut(kv, "=")
		if k == "VIRTUAL_ENV" || (k == "PATH" && strings.Contains(v, dir)) {
			t.Errorf("%s = %q with no venv in the release", k, v)
		}
	}
}

// BB-001/BB-002: `args` runs the single binary of a Go web app and of a
// [batch] Go app, and an argv that names the binary first is read as an argv.
func TestJobArgsRunTheSingleBinary(t *testing.T) {
	for _, tc := range []struct {
		framework string
		args      []string
		want      []string
	}{
		{"go", []string{"./toft-app", "job", "free"}, []string{"job", "free"}},
		{"go", []string{"toft-app", "job"}, []string{"job"}},
		{"batch-go", []string{"job", "work"}, []string{"job", "work"}},
		{"batch-go", []string{"/srv/release/toft-app", "work"}, []string{"work"}},
	} {
		name, got, err := jobBinaryArgs(&BootstrapResponse{Framework: tc.framework, Args: tc.args})
		if err != nil {
			t.Fatalf("%s %v: %v", tc.framework, tc.args, err)
		}
		if name != "./toft-app" || strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s %v: ran %s %v, want ./toft-app %v", tc.framework, tc.args, name, got, tc.want)
		}
	}
	if _, _, err := jobBinaryArgs(&BootstrapResponse{Framework: "flask", Args: []string{"x"}}); err == nil {
		t.Error("args on a flask release was accepted")
	}
}

// A load balancer's error page is not the platform's answer, and its markup in
// the app's log said only the status again (TOFT-BB-003).
func TestCallReportsAProxysErrorPageByItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "<html>\r\n<head><title>503 Service Temporarily Unavailable</title></head>\r\n</html>")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.call(context.Background(), http.MethodPost, "/api/v1/runner/bootstrap", "tok", nil, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "<html") || !strings.Contains(msg, "answered 503: Service Unavailable") {
		t.Errorf("got %q", msg)
	}
}

// One line over the cap neither stops the reader nor loses what follows: the
// line is truncated and marked, the pipe is read to its end, and a stream that
// never writes a newline (a \r progress bar) still drains (code review
// 2026-11-11, Q-1).
func TestScanLinesTruncatesALongLineAndKeepsReading(t *testing.T) {
	var got []string
	long := strings.Repeat("x", 3*scanLineMax)
	in := "first\n" + long + "\nlast\n" + strings.Repeat("\rprogress 50%", 200000)
	scanLines(strings.NewReader(in), func(s string) { got = append(got, s) })
	if len(got) != 4 {
		t.Fatalf("lines = %d, want 4 (first, truncated, last, the unterminated tail)", len(got))
	}
	if got[0] != "first" || got[2] != "last" {
		t.Errorf("lines around the long one: %q %q", got[0], got[2])
	}
	if len(got[1]) > scanLineMax+32 || !strings.HasSuffix(got[1], "[line truncated]") {
		t.Errorf("long line: len %d, suffix %q", len(got[1]), got[1][len(got[1])-20:])
	}
	if !strings.HasPrefix(got[3], "\rprogress") {
		t.Errorf("tail = %q", got[3][:20])
	}
}

// The runner's copy of the unpacker refuses a link replacing a directory the
// archive made, so the closing chmod cannot follow it out (AS-12).
func TestUntarRefusesASymlinkReplacingADirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "d", Mode: 0o777, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{Name: "d", Linkname: outside, Mode: 0o777, Typeflag: tar.TypeSymlink})
	tw.Close()
	gz.Close()

	dest := filepath.Join(t.TempDir(), "release")
	os.MkdirAll(dest, 0o750)
	if err := untarGz(&buf, dest); err == nil {
		t.Fatal("a symlink replacing a directory was accepted")
	}
	if info, _ := os.Stat(outside); info.Mode().Perm() != 0o600 {
		t.Errorf("the outside file's mode is now %v", info.Mode().Perm())
	}
}

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"toft/cli/internal/ui"
)

// lockedBuffer is a buffer the command and a program's renderer can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// liveInstall is a run with a person watching both streams and nobody to type:
// display programs only. Stdout and stderr are separate, so a test can say
// which stream a line reached.
func liveInstall(t *testing.T, handler http.HandlerFunc) (e *env, stdout, stderr *lockedBuffer, built *atomic.Int32) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	t.Setenv("TOFT_PLAIN", "")
	e, _ = fakeInstall(t, handler)
	stdout, stderr = &lockedBuffer{}, &lockedBuffer{}
	e.out.Stdout, e.out.Stderr = stdout, stderr
	e.out.Interactive = true
	e.out.Columns = 100
	e.out.StderrTerminal = func() bool { return true }
	e.stdinTerminal = func() bool { return false }
	built = &atomic.Int32{}
	prev := ui.SetProgramHook(func(input bool) {
		if input {
			t.Errorf("a watch built an input program")
		}
		built.Add(1)
	})
	t.Cleanup(func() { ui.SetProgramHook(prev) })
	return e, stdout, stderr, built
}

// **The job wait is one live line on a terminal**, its last state left on
// screen, --follow's log above it, and the outcome still the answer on stdout.
func TestTheJobWaitIsALiveLine(t *testing.T) {
	var polls atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/jobs/nightly/runs") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"run-1","job":"nightly","status":"queued"}`)
		case strings.HasSuffix(p, "/job-runs/run-1/logs"):
			if polls.Load() == 2 {
				fmt.Fprint(w, `{"lines":[{"seq":1,"line":"loading rows"}],"status":"running"}`)
				return
			}
			fmt.Fprint(w, `{"lines":[],"status":"running"}`)
		case strings.HasSuffix(p, "/job-runs/run-1"):
			status := "running"
			if polls.Add(1) >= 4 {
				status = "succeeded"
			}
			fmt.Fprintf(w, `{"id":"run-1","job":"nightly","status":%q,"progress":40}`, status)
		default:
			everythingFake(t)(w, r)
		}
	}
	e, stdout, stderr, built := liveInstall(t, handler)
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--follow"}); err != nil {
		t.Fatal(err)
	}
	if built.Load() != 1 {
		t.Fatalf("the wait built %d programs, want 1", built.Load())
	}
	screen := ansi.Strip(stderr.String())
	if !strings.Contains(screen, "loading rows") {
		t.Errorf("--follow's line is not on screen:\n%q", screen)
	}
	if strings.Contains(screen, "run-1  running") && strings.Count(screen, "\n") > 40 {
		t.Errorf("the live line was printed as lines rather than repainted:\n%q", screen)
	}
	if !strings.Contains(stdout.String(), "succeeded") {
		t.Errorf("the outcome is not the answer on stdout:\n%q", stdout.String())
	}
}

// **`logs -f` keeps its stream and gains a footer.** Lines arrive above it —
// through the program, since both streams are the one terminal on a live run —
// and the footer names the app, its state and the voice followed.
func TestLogsFollowHasAFooter(t *testing.T) {
	var polls atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(p, "/logs"):
			n := polls.Add(1)
			var lines []string
			for i := int32(1); i <= n; i++ {
				lines = append(lines, fmt.Sprintf(`{"at":"2026-10-28T00:00:%02dZ","source":"app","line":"line %d"}`, i, i))
			}
			fmt.Fprintf(w, `{"lines":[%s]}`, strings.Join(lines, ","))
		default:
			http.NotFound(w, r)
		}
	}
	e, stdout, stderr, built := liveInstall(t, handler)
	done := make(chan error, 1)
	go func() { done <- cmdLogs(e, []string{"--app", "demo", "-f"}) }()
	deadline := time.Now().Add(10 * time.Second)
	for polls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	e.cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if built.Load() != 1 {
		t.Fatalf("logs -f built %d programs, want 1", built.Load())
	}
	screen := ansi.Strip(stderr.String())
	if !strings.Contains(stdout.String(), "line 1") {
		t.Errorf("the history did not go to stdout:\n%q", stdout.String())
	}
	if !strings.Contains(screen, "line 2") {
		t.Errorf("a followed line is not above the footer:\n%q", screen)
	}
	if !strings.Contains(screen, "demo") || !strings.Contains(screen, "running") ||
		!strings.Contains(screen, "both voices") {
		t.Errorf("no footer:\n%q", screen)
	}
}

// **With a person on every stream, the confirm is a program** — and its answer
// is the command's: y goes ahead; Enter alone is No, and refuses as
// `cancelled`, exit 2, exactly as the line has.
func TestConfirmIsAProgramWithAPerson(t *testing.T) {
	for _, tc := range []struct {
		keys string
		want error
	}{{"y", nil}, {"\r", usageCodef("cancelled", "cancelled")}} {
		e, _, stderr, _ := liveInstall(t, everythingFake(t))
		ui.SetProgramHook(nil)
		e.stdinTerminal = func() bool { return true }
		keys, typed := io.Pipe()
		e.stdin = keys
		go func() {
			for !strings.Contains(stderr.String(), "?") {
				time.Sleep(5 * time.Millisecond)
			}
			typed.Write([]byte(tc.keys))
		}()
		err := cmdRollback(e, []string{"--app", "demo", "--no-watch"})
		typed.Close()
		if errText(err) != errText(tc.want) {
			t.Errorf("keys %q: err %v, want %v", tc.keys, err, tc.want)
		}
		if !strings.Contains(ansi.Strip(stderr.String()), "Roll") {
			t.Errorf("keys %q: the question was not on screen:\n%q", tc.keys, stderr.String())
		}
	}
}

// **`--wait` is one live line**, gone once the app is up, and the outcome is
// still the answer on stdout.
func TestStartWaitIsALiveLine(t *testing.T) {
	var reads atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"restarting"}`)
			return
		}
		if reads.Add(1) < 3 {
			fmt.Fprint(w, appRow("building"))
			return
		}
		fmt.Fprint(w, appRow("running"))
	}
	e, stdout, stderr, built := liveInstall(t, handler)
	if err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "10s"}); err != nil {
		t.Fatal(err)
	}
	if built.Load() != 1 {
		t.Fatalf("the wait built %d programs, want 1", built.Load())
	}
	if !strings.Contains(ansi.Strip(stderr.String()), "waiting for demo to come up") {
		t.Errorf("no wait line on screen:\n%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "restarted") {
		t.Errorf("the outcome is not the answer on stdout:\n%q", stdout.String())
	}
}

// The byte meter is for a person watching one transfer: off a live run, and
// for a size the store did not say, the reader is the one passed in.
func TestTheMeterIsOnlyForALiveRun(t *testing.T) {
	e, _, _, _ := liveInstall(t, everythingFake(t))
	src := strings.NewReader("bytes")
	if _, ok := metered(e, "download", src, 5).(*meterReader); !ok {
		t.Error("no meter on a live run")
	}
	if metered(e, "download", src, -1) != io.Reader(src) {
		t.Error("a meter for a size nobody knows")
	}
	e.out.Quiet = true
	if metered(e, "download", src, 5) != io.Reader(src) {
		t.Error("a meter under -q")
	}
}

package builder

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"toft/internal/runner"
	"toft/internal/runtime"
)

// deadRuntime answers Metrics and Logs and nothing else; every other method is
// the nil interface and panics if waitReady ever reached for it.
type deadRuntime struct {
	runtime.ServiceRuntime
	metrics []*runtime.AppMetrics // one per call; the last repeats
	calls   int
	logs    []runtime.LogLine
}

func (d *deadRuntime) Metrics(string) (*runtime.AppMetrics, error) {
	i := d.calls
	if i >= len(d.metrics) {
		i = len(d.metrics) - 1
	}
	d.calls++
	if d.metrics[i] == nil {
		return nil, errors.New("not found")
	}
	return d.metrics[i], nil
}

func (d *deadRuntime) Logs(string, int) ([]runtime.LogLine, error) { return d.logs, nil }

// closedEndpoint is an address nothing listens on, so only the watch can end
// the wait early.
func closedEndpoint(t *testing.T) runtime.Endpoint {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return runtime.Endpoint{Host: "127.0.0.1", Port: port}
}

func TestWaitReadyFailsFastWhenTheWorkloadDies(t *testing.T) {
	old := readinessWatchEvery
	readinessWatchEvery = 10 * time.Millisecond
	t.Cleanup(func() { readinessWatchEvery = old })

	var logs []runtime.LogLine
	logs = append(logs, runtime.LogLine{Source: runtime.LogSourcePlatform, Line: "==> Starting release demo-1"})
	for i := 0; i < 30; i++ {
		logs = append(logs, runtime.LogLine{Source: runtime.LogSourceApp, Line: fmt.Sprintf("line %d", i)})
	}
	logs = append(logs, runtime.LogLine{Source: runtime.LogSourceApp, Line: "ModuleNotFoundError: No module named 'nope'"})

	cases := map[string][]*runtime.AppMetrics{
		"errored":       {{Status: runtime.StatusErrored}},
		"crash-looping": {{Status: runtime.StatusLaunching, CrashLooping: true}},
		"restarting":    {{Status: runtime.StatusOnline, Restarts: 3}, {Status: runtime.StatusOnline, Restarts: 4}, {Status: runtime.StatusOnline, Restarts: 5}},
	}
	for name, metrics := range cases {
		t.Run(name, func(t *testing.T) {
			rt := &deadRuntime{metrics: metrics, logs: logs}
			start := time.Now()
			err := waitReady(closedEndpoint(t), 30*time.Second, watchWorkload(rt, "demo-1"))
			if time.Since(start) > 10*time.Second {
				t.Fatalf("took %s; the watch did not end the wait", time.Since(start))
			}
			if !errors.Is(err, errWorkloadExited) {
				t.Fatalf("err = %v, want errWorkloadExited", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "ModuleNotFoundError") {
				t.Errorf("log tail missing from %q", msg)
			}
			if strings.Contains(msg, "==> Starting release") || strings.Contains(msg, "line 10\n") {
				t.Errorf("tail is not the app's last %d lines: %q", outputTailLines, msg)
			}
		})
	}
}

// A runner that dies before the app starts says why in the platform's voice,
// and that line is the reason — not narration to be dropped.
func TestWaitReadyCarriesTheRunnersOwnLastWords(t *testing.T) {
	old := readinessWatchEvery
	readinessWatchEvery = 10 * time.Millisecond
	t.Cleanup(func() { readinessWatchEvery = old })

	rt := &deadRuntime{
		metrics: []*runtime.AppMetrics{{Status: runtime.StatusLaunching, CrashLooping: true}},
		logs: []runtime.LogLine{
			{Source: runtime.LogSourcePlatform, Line: "==> Starting release demo-1"},
			{Source: runtime.LogSourcePlatform, Line: "identity: reading /var/run/toft/identity/token: no such file or directory"},
		},
	}
	err := waitReady(closedEndpoint(t), 30*time.Second, watchWorkload(rt, "demo-1"))
	if !errors.Is(err, errWorkloadExited) {
		t.Fatalf("err = %v, want errWorkloadExited", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "identity: reading") || strings.Contains(msg, "==> Starting release") {
		t.Errorf("want the runner's line and no narration, got %q", msg)
	}
}

func TestWaitReadyKeepsDiallingWhileTheWorkloadLives(t *testing.T) {
	old := readinessWatchEvery
	readinessWatchEvery = 10 * time.Millisecond
	t.Cleanup(func() { readinessWatchEvery = old })

	// One restart, and a backend that cannot answer, are not death.
	rt := &deadRuntime{metrics: []*runtime.AppMetrics{
		{Status: runtime.StatusOnline, Restarts: 0}, nil, {Status: runtime.StatusOnline, Restarts: 1},
	}}
	err := waitReady(closedEndpoint(t), 1500*time.Millisecond, watchWorkload(rt, "demo-1"))
	if err == nil || errors.Is(err, errWorkloadExited) {
		t.Fatalf("err = %v, want the ordinary timeout", err)
	}
	if rt.calls < 2 {
		t.Errorf("the backend was asked %d times", rt.calls)
	}
}

// A runner that could not bootstrap ran no app code, so the headline is
// Toft's, not "the app exited" (TOFT-BB-003: task-mailer on trytoft, whose
// only lines after a platform restart were the runner's 503s).
func TestWaitReadyBlamesThePlatformWhenTheRunnerCouldNotBootstrap(t *testing.T) {
	old := readinessWatchEvery
	readinessWatchEvery = 10 * time.Millisecond
	t.Cleanup(func() { readinessWatchEvery = old })

	rt := &deadRuntime{
		metrics: []*runtime.AppMetrics{{Status: runtime.StatusLaunching, CrashLooping: true}},
		logs: []runtime.LogLine{
			{Source: runtime.LogSourceApp, Line: "[INFO] Booting worker with pid: 1474"},
			{Source: runtime.LogSourcePlatform, Line: "2026/11/06 20:00:24 " + runner.BootstrapFailed +
				": /api/v1/runner/bootstrap: platform answered 503: Service Unavailable"},
		},
	}
	err := waitReady(closedEndpoint(t), 30*time.Second, watchWorkload(rt, "demo-1"))
	if !errors.Is(err, errWorkloadExited) {
		t.Fatalf("err = %v, want errWorkloadExited underneath", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, errPlatformUnreachable) || strings.Contains(msg, "the app exited") {
		t.Errorf("want the platform named, got %q", msg)
	}
	if !strings.Contains(msg, "answered 503") {
		t.Errorf("the runner's line is missing from %q", msg)
	}

	// The same runner line followed by the app's own output is the app's death.
	rt = &deadRuntime{
		metrics: rt.metrics,
		logs:    append(rt.logs, runtime.LogLine{Source: runtime.LogSourceApp, Line: "Traceback: boom"}),
	}
	err = waitReady(closedEndpoint(t), 30*time.Second, watchWorkload(rt, "demo-1"))
	if msg := err.Error(); !strings.HasPrefix(msg, errWorkloadExited.Error()) {
		t.Errorf("an app that printed last is the app's failure, got %q", msg)
	}
}

// A redirect is an answer: the app is up. Following it sent the probe after
// https://127.0.0.1:<port>/ or an external IdP and failed every poll (code
// review 2026-11-11, F-3).
func TestWaitReadyTakesARedirectAsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://login.example/?next=/", http.StatusFound)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	if err := waitReady(runtime.Endpoint{Host: u.Hostname(), Port: port}, 5*time.Second, nil); err != nil {
		t.Fatalf("an app that redirects / never became ready: %v", err)
	}
}

// A workload killed for its memory limit during the wait is said as memory,
// not as a crash loop whose log tail reads as the cause (2026-11-15 EPA
// redeploy: nwca-planttool, 1723 MB against 1024, told only "crash-looping").
func TestWaitReadySaysAMemoryKillIsMemory(t *testing.T) {
	old := readinessWatchEvery
	readinessWatchEvery = 10 * time.Millisecond
	t.Cleanup(func() { readinessWatchEvery = old })

	logs := []runtime.LogLine{{Source: runtime.LogSourceApp, Line: "Attaching package: 'rsconnect'"}}
	cases := map[string]struct {
		m      runtime.AppMetrics
		memory bool
		phrase string
	}{
		"oom-killed": {runtime.AppMetrics{Status: runtime.StatusLaunching, CrashLooping: true,
			OOMKilled: true, ExitPeakMemoryMB: 1723, MemoryLimitMB: 1024}, true,
			"it ran out of memory (peaked at 1723 MB against a 1024 MB limit)"},
		"peak at the cap": {runtime.AppMetrics{Status: runtime.StatusLaunching, CrashLooping: true,
			ExitPeakMemoryMB: 1020, MemoryLimitMB: 1024}, true,
			"it ran out of memory (peaked at 1020 MB against a 1024 MB limit)"},
		"plain crash loop": {runtime.AppMetrics{Status: runtime.StatusLaunching, CrashLooping: true,
			ExitPeakMemoryMB: 200, MemoryLimitMB: 1024}, false, "it is crash-looping"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := c.m
			rt := &deadRuntime{metrics: []*runtime.AppMetrics{&m}, logs: logs}
			err := waitReady(closedEndpoint(t), 30*time.Second, watchWorkload(rt, "demo-1"))
			var exited *workloadExitedError
			if !errors.As(err, &exited) {
				t.Fatalf("err = %v, want a workloadExitedError", err)
			}
			if exited.memory != c.memory {
				t.Errorf("memory = %v, want %v", exited.memory, c.memory)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.phrase) {
				t.Errorf("want %q in %q", c.phrase, msg)
			}
			if c.memory && strings.Contains(msg, "crash-looping") {
				t.Errorf("a memory kill still said crash-looping: %q", msg)
			}
			if !strings.Contains(msg, "rsconnect") {
				t.Errorf("the log tail is gone from %q", msg)
			}
		})
	}
}

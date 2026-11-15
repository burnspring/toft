//go:build unix

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedTee is the pod's stdout, read while the runner is still writing it: a
// service's app output is never shipped, so the tee is where it shows.
type lockedTee struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedTee) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedTee) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// servePlannedStop starts a "python" service whose interpreter is the given
// shell script, waits for it to print "up", cancels the context the way the
// supervisor's SIGTERM does, and returns what Run returned and every line it
// shipped.
func servePlannedStop(t *testing.T, script string) (error, []string, []ResultReport) {
	t.Helper()
	release := t.TempDir()
	if err := os.MkdirAll(filepath.Join(release, "venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "venv", "bin", "python"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python", Entrypoint: "app.py",
		RunToken: "rt",
	})
	tee := &lockedTee{}
	restore := logTee
	logTee = tee
	t.Cleanup(func() { logTee = restore })

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			CallbackURL: stub.server.URL, IdentityTokenPath: identityFile(t, "boot"),
			Mode: "serve", WorkDir: t.TempDir(), WorkloadName: "demo-abc",
			ReleaseDir: release, Port: 18999, Bind: "127.0.0.1",
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(tee.String(), "] up") {
		if time.Now().After(deadline) {
			t.Fatalf("the service never started:\n%s", tee.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the runner did not end after its context was cancelled")
	}
	_, _, results, _ := stub.snapshot()
	return err, strings.Split(tee.String(), "\n"), results
}

// D16 (2026-10-28 trytoft drills): a restart, a rollback or a sleep of a
// file-server or document workload logged "ERROR at app_exec: context
// canceled", and a restart of anything else "app was killed by killed". A stop
// the platform asked for is SIGTERM, which the app may drain on, and an app
// that exits 0 on it has stopped — said as a progress line, not an ERROR.
func TestAPlannedStopIsSIGTERMAndNotAnError(t *testing.T) {
	err, lines, results := servePlannedStop(t,
		"#!/bin/sh\ntrap 'echo draining; exit 0' TERM\necho up\nwhile true; do sleep 0.1; done\n")
	if err != nil {
		t.Fatalf("a drained stop ended the runner with %v", err)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "draining") {
		t.Errorf("the app never saw SIGTERM — it was killed instead of asked:\n%s", all)
	}
	if strings.Contains(all, "ERROR") || strings.Contains(all, "context canceled") {
		t.Errorf("a planned stop was logged as an error:\n%s", all)
	}
	if !strings.Contains(all, "==> Stopped") {
		t.Errorf("the stop is not said:\n%s", all)
	}
	if len(results) != 1 || results[0].ExitCode != 0 || results[0].Stage != "" {
		t.Errorf("results = %+v, want one clean exit", results)
	}
}

// An app with no SIGTERM handler dies of it; that is still the stop the
// platform asked for, and the line names the signal as SIGTERM, never by
// syscall.Signal's own word.
func TestAnAppThatDiesOfItsStopSignalSaysSo(t *testing.T) {
	err, _, _ := servePlannedStop(t, "#!/bin/sh\necho up\nexec sleep 30\n")
	var exit AppExit
	if !errors.As(err, &exit) {
		t.Fatalf("Run returned %v, want the app's own end", err)
	}
	if exit.Signal != syscall.SIGTERM || !exit.Stopped || exit.Code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit = %+v, want SIGTERM, stopped, code 143", exit)
	}
	if got, want := exit.Error(), "app stopped on SIGTERM, as the platform asked"; got != want {
		t.Errorf("logged %q, want %q", got, want)
	}
}

func TestAppExitNamesItsSignal(t *testing.T) {
	cases := []struct {
		e    AppExit
		want string
	}{
		{AppExit{Code: 137, Signal: syscall.SIGKILL}, "app was killed by SIGKILL"},
		{AppExit{Code: 143, Signal: syscall.SIGTERM, Stopped: true}, "app stopped on SIGTERM, as the platform asked"},
		{AppExit{Code: 2}, "app exited 2"},
		{AppExit{Code: 1, Stopped: true}, "app exited 1 when the platform asked it to stop"},
	}
	for _, c := range cases {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}

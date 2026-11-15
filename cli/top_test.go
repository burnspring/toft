package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"toft/cli/internal/config"
	"toft/cli/internal/ui"
)

// **Off the live tier, top is apps** — the plain lines and the --json
// document, byte for byte, so it adds a command and no new shape.
func TestTopIsAppsOffATerminal(t *testing.T) {
	for _, args := range [][]string{nil, {"--json"}} {
		e, top := fakeInstall(t, everythingFake(t))
		if err := cmdTop(e, args); err != nil {
			t.Fatal(err)
		}
		e, apps := fakeInstall(t, everythingFake(t))
		if err := cmdApps(e, args); err != nil {
			t.Fatal(err)
		}
		if top.String() != apps.String() {
			t.Errorf("top %v differs from apps:\n%q\n%q", args, top.String(), apps.String())
		}
	}
}

// Live, it reads the estate and the linked app's runs, draws them, and q
// leaves the last frame.
func TestTopLive(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/job-runs") {
			fmt.Fprint(w, `{"runs":[{"id":"run-12345678","job":"nightly","status":"running","queued_at":"2026-10-28T00:00:00Z"}]}`)
			return
		}
		everythingFake(t)(w, r)
	}
	e, _, stderr, _ := liveInstall(t, handler)
	ui.SetProgramHook(nil)
	e.stdinTerminal = func() bool { return true }
	keys, typed := io.Pipe()
	defer typed.Close()
	e.stdin = keys
	go func() {
		for !strings.Contains(stderr.String(), "nightly") {
			time.Sleep(5 * time.Millisecond)
		}
		typed.Write([]byte("q"))
	}()
	done := make(chan error, 1)
	go func() { done <- cmdTop(e, []string{"--app", "demo"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("q did not end top")
	}
	got := ansi.Strip(stderr.String())
	for _, want := range []string{"toft top", "demo", "running", "job runs", "nightly"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// Taller than the terminal: cut, with a count of what was cut.
func TestTopCutsToTheTerminal(t *testing.T) {
	e, _ := fakeInstall(t, everythingFake(t))
	e.out.Interactive = true
	m := &topModel{e: e, s: &session{Session: &config.Session{InstallURL: "https://toft.example"}}, every: time.Second}
	for i := 0; i < 40; i++ {
		m.apps = append(m.apps, topApp{appSummary: appSummary{Slug: fmt.Sprintf("app-%02d", i), Status: "running"}})
	}
	m.at = time.Now()
	view := ansi.Strip(m.View().Content)
	lines := strings.Count(view, "\n")
	if lines > e.out.Height() {
		t.Errorf("%d lines on a %d-line terminal", lines, e.out.Height())
	}
	if !strings.Contains(view, "more") {
		t.Errorf("nothing says what was cut:\n%s", view)
	}
}

// --mine is apps --mine: off the live tier the same document, and live the
// read keeps only what the install calls yours.
func TestTopMine(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"a","slug":"teams","owner_group_id":"g1","mine":true},`+
			`{"id":"b","slug":"theirs","owner_id":"u2","mine":false}]`)
	}
	args := []string{"--mine", "--json"}
	e, top := fakeInstall(t, handler)
	if err := cmdTop(e, args); err != nil {
		t.Fatal(err)
	}
	e, apps := fakeInstall(t, handler)
	if err := cmdApps(e, args); err != nil {
		t.Fatal(err)
	}
	if top.String() != apps.String() {
		t.Errorf("top --mine differs from apps --mine:\n%q\n%q", top.String(), apps.String())
	}

	e, _ = fakeInstall(t, handler)
	s, err := e.setup(&globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	r := (&topModel{e: e, s: s, mine: true}).read().(topRead)
	if r.err != nil || len(r.apps) != 1 || r.apps[0].Slug != "teams" {
		t.Errorf("live --mine read = %+v, %v", r.apps, r.err)
	}
}

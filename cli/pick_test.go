package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"toft/cli/internal/ui"
)

// noAppLinked is the refusal session.app() gives in a directory with nothing
// linked — the one every unanswered picker must return, word for word.
const noAppLinked = "no app is linked in this directory — run `toft link <slug>`, or pass --app <slug>"

// askingInstall is a person on every stream, in a directory with no app linked,
// with stdin the given keys. It counts the input programs built.
func askingInstall(t *testing.T, handler http.HandlerFunc, keys io.Reader) (*env, *lockedBuffer, *atomic.Int32) {
	t.Helper()
	t.Setenv("TOFT_APP", "")
	e, _, stderr, _ := liveInstall(t, handler)
	inputs := &atomic.Int32{}
	ui.SetProgramHook(func(input bool) {
		if input {
			inputs.Add(1)
		}
	})
	e.stdinTerminal = func() bool { return true }
	e.stdin = keys
	return e, stderr, inputs
}

// **The eleven callers of session.app(): nine may pick, two never do.**
// `deploy` uploads this directory, so a list would choose which live app the
// tree overwrites; `check` with no app is the before-create check. Every
// picker left unanswered (Escape here) returns session.app()'s own refusal.
func TestThePickerCallers(t *testing.T) {
	cases := []struct {
		name  string
		run   func(*env, []string) error
		args  []string
		picks bool
	}{
		{"status", cmdStatus, nil, true},
		{"logs", cmdLogs, nil, true},
		{"events", cmdEvents, nil, true},
		{"deps", cmdDeps, nil, true},
		{"restart", cmdRestart, nil, true},
		{"start", cmdStart, nil, true},
		{"stop", cmdStop, nil, true},
		{"redeploy", cmdRedeploy, []string{"--no-watch"}, true},
		{"rollback", cmdRollback, []string{"--no-watch"}, true},
		{"job", cmdJob, []string{"list"}, true},
		{"deploy", cmdDeploy, nil, false},
		{"check", cmdCheck, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, inputs := askingInstall(t, lenient(t), strings.NewReader("\x1b"))
			err := tc.run(e, tc.args)
			if !tc.picks {
				if inputs.Load() != 0 {
					t.Fatalf("`toft %s` opened a picker", tc.name)
				}
				return
			}
			if inputs.Load() != 1 {
				t.Fatalf("`toft %s` built %d input programs, want the picker", tc.name, inputs.Load())
			}
			if errText(err) != noAppLinked || report(e, err) != exitUsage {
				t.Errorf("an unanswered picker returned %v, want session.app()'s refusal, exit 2", err)
			}
		})
	}
}

// A choice is used for the run and named as such; nothing is written.
func TestThePickerChoosesForTheRun(t *testing.T) {
	keys, typed := io.Pipe()
	defer typed.Close()
	e, stderr, _ := askingInstall(t, everythingFake(t), keys)
	go func() {
		for !strings.Contains(stderr.String(), "demo") {
			time.Sleep(5 * time.Millisecond)
		}
		typed.Write([]byte("dmo\r"))
	}()
	if err := cmdStatus(e, nil); err != nil {
		t.Fatal(err)
	}
	if got := ansi.Strip(stderr.String()); !strings.Contains(got, "toft link demo") {
		t.Errorf("the choice was not named as this run's:\n%s", got)
	}
}

// Nothing to choose from is the refusal, not an empty list; and nobody to ask
// is the refusal without a list being read at all.
func TestThePickerRefusesWithNothingToPick(t *testing.T) {
	var listed atomic.Int32
	empty := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/apps" {
			listed.Add(1)
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}
	e, _, inputs := askingInstall(t, empty, strings.NewReader(""))
	if err := cmdStatus(e, nil); errText(err) != noAppLinked {
		t.Errorf("empty list: %v", err)
	}
	if inputs.Load() != 0 {
		t.Error("an empty list opened a picker")
	}

	e, _, _ = askingInstall(t, empty, strings.NewReader(""))
	e.stdinTerminal = func() bool { return false }
	listed.Store(0)
	if err := cmdStatus(e, nil); errText(err) != noAppLinked || listed.Load() != 0 {
		t.Errorf("unattended: %v, listed %d times", err, listed.Load())
	}
}

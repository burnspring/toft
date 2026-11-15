package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"toft/cli/internal/ui"
)

// plainArgs is how each command is driven by TestPlainIsAPipedRun: enough to
// reach its answer, or its first question, against everythingFake. A command
// not listed runs with `--app demo` — and a refusal is an answer too, which the
// two runs have to give byte for byte.
var plainArgs = map[string][]string{
	"login":       {"--status"},
	"link":        {"demo"},
	"deploy":      {"--app", "demo", "--no-watch"},
	"rollback":    {"--app", "demo", "--no-watch"},
	"redeploy":    {"--app", "demo", "--no-watch"},
	"apps":        {},
	"store":       {"ls", "--app", "demo"},
	"sdk":         {"python"},
	"create":      {"demo"},
	"job":         {"list", "--app", "demo"},
	"api":         {"GET", "/api/v1/apps"},
	"task":        {"list", "--app", "demo"},
	"logs":        {"--app", "demo", "--deploy", "dep-1"},
	"env":         {"list", "--app", "demo"},
	"visibility":  {"--app", "demo"},
	"delete":      {"--app", "demo"},
	"deployments": {"--app", "demo"},
	"fetch":       {"--app", "demo", "/"},
	"check":       {"--app", "demo"},
	"whoami":      {},
	"skill":       {},
}

// **TOFT_PLAIN is a piped run, for every command — including the next one.**
//
// An agent in a pty is indistinguishable from a person to every isatty test,
// and TOFT_PLAIN is how it says otherwise. That only holds if no command reads
// the terminal around it, so each command in the dispatch table is run twice: as
// a real pipe (stdout and stdin not terminals) and claiming a terminal on both
// with TOFT_PLAIN=1. The bytes, and the error, have to be the same. Iterating
// commandTable() is what makes a new command answer to this without anybody
// remembering to list it.
func TestPlainIsAPipedRun(t *testing.T) {
	for name, run := range commandTable() {
		args, ok := plainArgs[name]
		if !ok {
			args = []string{"--app", "demo"}
		}
		t.Run(name, func(t *testing.T) {
			piped, pipedErr := runGated(t, run, args, false)
			plain, plainErr := runGated(t, run, args, true)
			if piped != plain {
				t.Errorf("TOFT_PLAIN=1 on a terminal differs from a piped run:\npiped:\n%s\nplain:\n%s", piped, plain)
			}
			// A refusal says why it could not ask, and the why is the one thing
			// that should differ.
			if refusalReason.ReplaceAllString(errText(pipedErr), "") !=
				refusalReason.ReplaceAllString(errText(plainErr), "") {
				t.Errorf("error differs: piped %q, plain %q", errText(pipedErr), errText(plainErr))
			}
		})
	}
}

// --plain=false wins over the environment, and --plain over its absence.
func TestPlainFlagBeatsTheEnvironment(t *testing.T) {
	cases := []struct {
		env  string
		args []string
		want bool
	}{
		{"", nil, false},
		{"1", nil, true},
		{"0", nil, false},
		{"yes", nil, true},
		{"1", []string{"--plain=false"}, false},
		{"", []string{"--plain"}, true},
		{"0", []string{"--plain"}, true},
	}
	for _, tc := range cases {
		t.Setenv("TOFT_PLAIN", tc.env)
		var g globalFlags
		fs := newFlags("x", "")
		g.register(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if got := plainRun(&g); got != tc.want {
			t.Errorf("TOFT_PLAIN=%q %v: plainRun = %v, want %v", tc.env, tc.args, got, tc.want)
		}
	}
}

// A confirmation under TOFT_PLAIN refuses, naming why — with a person's
// terminal on stdin, which would otherwise be asked.
func TestPlainRefusesAConfirmation(t *testing.T) {
	t.Setenv("TOFT_PLAIN", "1")
	e, _ := fakeInstall(t, everythingFake(t))
	e.stdinTerminal = func() bool { return true }
	err := confirm(e, &globalFlags{}, "roll back demo")
	if err == nil || errText(err) != "refusing to roll back demo without --yes (TOFT_PLAIN is set)" {
		t.Errorf("confirm under TOFT_PLAIN = %v", err)
	}
}

var refusalReason = regexp.MustCompile(` \((stdin is not a terminal|TOFT_PLAIN is set)\)$`)

func runGated(t *testing.T, run func(*env, []string) error, args []string, plain bool) (string, error) {
	t.Helper()
	e, buf := fakeInstall(t, lenient(t))
	// The run's own environment is not the test's: a CI runner sets CI, and
	// that would be a third reason to refuse on both sides.
	t.Setenv("CI", "")
	if plain {
		t.Setenv("TOFT_PLAIN", "1")
		e.out.Interactive = true
		e.stdinTerminal = func() bool { return true }
	} else {
		t.Setenv("TOFT_PLAIN", "")
		e.stdinTerminal = func() bool { return false }
	}
	e.stdin = &bytes.Buffer{}
	e.out.Columns = 100
	err := run(e, append([]string(nil), args...))
	// What differs between two runs and is not the rendering: each has its own
	// fake install and its own directory.
	norm := strings.NewReplacer(os.Getenv("TOFT_URL"), "URL", e.dir, "DIR")
	if err != nil {
		err = errors.New(norm.Replace(err.Error()))
	}
	return norm.Replace(buf.String()), err
}

// lenient is everythingFake for the routes it knows, and a 404 for the rest:
// what is compared here is two runs against each other, not either against the
// platform, so a command reaching a route the fake lacks is not this test's
// failure.
func lenient(t *testing.T) http.HandlerFunc {
	return everythingFakeOr(http.NotFound)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// gateCondition takes one claim away from a run that otherwise has a person on
// every stream. noInput conditions still allow a display (--yes and a piped
// stdin say nobody will answer, not that nobody is watching); the rest allow
// no program at all — CI included, since the tree is for a person.
type gateCondition struct {
	name    string
	args    []string
	env     map[string]string
	set     func(*env)
	noInput bool
}

var gateConditions = []gateCondition{
	{name: "--json", args: []string{"--json"}},
	{name: "-q", args: []string{"-q"}},
	{name: "TOFT_PLAIN", env: map[string]string{"TOFT_PLAIN": "1"}},
	{name: "--plain", args: []string{"--plain"}},
	{name: "stdout piped", set: func(e *env) { e.out.Interactive = false }},
	{name: "stderr piped", set: func(e *env) { e.out.StderrTerminal = func() bool { return false } }},
	{name: "TERM=dumb", env: map[string]string{"TERM": "dumb"}},
	{name: "CI", env: map[string]string{"CI": "1"}},
	{name: "--yes", args: []string{"--yes"}, noInput: true},
	{name: "stdin piped", set: func(e *env) { e.stdinTerminal = func() bool { return false } }, noInput: true},
}

// **The gate matrix: no program where nobody can watch, no keys where nobody
// can press them.** Every command in the dispatch table, under each condition,
// against a run that otherwise claims a person on stdin, stdout and stderr.
// Asserted through the constructor's hook, not by reading output — a program
// drawn into a pipe is exactly the defect that output alone does not show.
func TestGateMatrix(t *testing.T) {
	for _, c := range gateConditions {
		for name, run := range commandTable() {
			args, ok := plainArgs[name]
			if !ok {
				args = []string{"--app", "demo"}
			}
			t.Run(c.name+"/"+name, func(t *testing.T) {
				t.Setenv("TERM", "xterm-256color")
				t.Setenv("CI", "")
				t.Setenv("TOFT_PLAIN", "")
				for k, v := range c.env {
					t.Setenv(k, v)
				}
				e, _ := fakeInstall(t, lenient(t))
				e.out.Interactive = true
				e.out.StderrTerminal = func() bool { return true }
				e.stdinTerminal = func() bool { return true }
				e.stdin = &bytes.Buffer{}
				if c.set != nil {
					c.set(e)
				}
				prev := ui.SetProgramHook(func(input bool) {
					if input || !c.noInput {
						t.Errorf("a program (input %v) was built under %s", input, c.name)
						return
					}
					// A display it allows may run until Ctrl-C (`top` does),
					// so press it, as a person watching would.
					go func() {
						time.Sleep(50 * time.Millisecond)
						e.interrupt()
					}()
				})
				defer ui.SetProgramHook(prev)
				_ = run(e, append(append([]string(nil), args...), c.args...))
			})
		}
	}
}

// input() is Live and not unattended — the positive control the matrix needs,
// and each unattended signal removing it.
func TestInputIsLiveAndAttended(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	t.Setenv("TOFT_PLAIN", "")
	e, _ := fakeInstall(t, everythingFake(t))
	e.out.Interactive = true
	e.out.StderrTerminal = func() bool { return true }
	e.stdinTerminal = func() bool { return true }
	if !input(e, &globalFlags{}) {
		t.Fatal("a person on every stream cannot be asked — the matrix is vacuous")
	}
	if input(e, &globalFlags{yes: true}) {
		t.Error("--yes still asks")
	}
	t.Setenv("CI", "1")
	if input(e, &globalFlags{}) {
		t.Error("CI still asks")
	}
}

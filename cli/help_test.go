package main

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// `toft job --help` used to panic.
//
// The three commands with subcommands print their usage themselves, and they
// did it through `e.out`, which is built by setup() — and setup() is only
// reached once a subcommand has been chosen. So the one invocation that needs
// the help text most, the one where the caller does not yet know the
// subcommands, dereferenced a nil writer and dumped a Go stack trace. Help goes
// to os.Stdout, like the usage of every other command and for the same reason:
// it is the answer, not a complaint about the invocation.
func TestSubcommandHelpDoesNotNeedASession(t *testing.T) {
	for _, tc := range []struct {
		command string
		run     func(*env, []string) error
		says    string
	}{
		{"job", cmdJob, "own memory limit"},
		{"store", cmdStore, "store"},
		{"sdk", cmdSDK, "app SDK"},
	} {
		for _, arg := range []string{"--help", "-h", "help"} {
			out := captureStdout(t, func() {
				// A bare env: no writer, no session, nothing setup() built.
				// This is what `toft job --help` has before it parses anything.
				if err := tc.run(&env{}, []string{arg}); err != nil {
					t.Fatalf("toft %s %s: %v", tc.command, arg, err)
				}
			})
			if !strings.Contains(out, tc.says) {
				t.Errorf("toft %s %s did not print its usage to stdout: %q", tc.command, arg, out)
			}
		}
	}
}

// captureStdout runs f with os.Stdout pointed at a file, and returns what
// landed there.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	path := t.TempDir() + "/stdout"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = saved; file.Close() }()
	f()
	file.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// **A flag after a positional argument is read**, in every command. `flag`
// alone stops at the first word and silently drops the rest, which is how `toft
// login <url> --token-stdin` became a device flow waiting for a human on a
// machine that has none; the refusal that replaced it ("put flags first") was
// still a fix the caller should not have needed.
func TestFlagsAreReadAfterAPositional(t *testing.T) {
	for _, args := range [][]string{
		{"https://toft.corp", "--token-stdin"},
		{"--token-stdin", "https://toft.corp"},
	} {
		fs := newFlags("login", "usage\n")
		tokenStdin := fs.Bool("token-stdin", false, "read a key from stdin")
		lines := fs.Int("lines", 0, "a value flag")
		if err := parseOperands(fs, append(args, "--lines", "-5"), 1); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !*tokenStdin || fs.Arg(0) != "https://toft.corp" || *lines != -5 {
			t.Errorf("%v parsed token-stdin=%v lines=%d arg=%q", args, *tokenStdin, *lines, fs.Arg(0))
		}
	}
	// After `--` a word is an operand whatever it looks like.
	fs := newFlags("x", "usage\n")
	fs.Bool("json", false, "")
	if err := parseOperands(fs, []string{"--", "--json"}, 1); err != nil || fs.Arg(0) != "--json" {
		t.Errorf("after --: %v %v", err, fs.Args())
	}
	// And an unknown flag after an operand is still the usual refusal.
	fs = newFlags("x", "usage\n")
	if err := parseOperands(fs, []string{"a", "--nope"}, 1); err == nil || !strings.Contains(err.Error(), "--nope") {
		t.Errorf("unknown flag: %v", err)
	}
}

// An operand nobody reads is refused, and a bool flag handed a word names the
// `=` spelling. `toft task edit nightly --enabled false` once printed success over a
// task that stayed enabled: `--enabled` was a bare true and `false` a leftover
// word nothing looked at.
func TestUnreadOperandsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		args []string
		max  int
		want string // "" is accepted
	}{
		{"bool given false", []string{"--enabled", "false"}, 1, "--enabled=false"},
		{"bool given TRUE after an operand", []string{"--json", "--enabled", "TRUE"}, 0, "--enabled=true"},
		{"bool with equals", []string{"--enabled=false"}, 0, ""},
		{"stray word", []string{"--json", "extra"}, 0, "takes no argument"},
		{"one too many", []string{"a", "b"}, 1, "too many"},
		{"one allowed", []string{"a"}, 1, ""},
		{"any number", []string{"a", "b", "c"}, -1, ""},
		{"a string flag's value is not a switch", []string{"--path", "false"}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var g globalFlags
			fs := newFlags("job run", "usage\n")
			g.register(fs)
			fs.Bool("enabled", true, "")
			fs.String("path", "", "")
			err := parseOperands(fs, c.args, c.max)
			if c.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var u *usageError
			if !errors.As(err, &u) {
				t.Fatalf("got %v (%T), want a usage error", err, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%q does not say %q", err, c.want)
			}
		})
	}
}

// A command that fails before setup() has to be able to say so.
//
// `toft status --bogus` is the shape: `flag` refuses the flag, the command returns
// a usage error, and report() renders it — through a writer that at that point
// nothing has built. It printed a stack trace instead of the one line naming the
// flag, for every command, for every typo.
func TestAnErrorBeforeSetupStillReports(t *testing.T) {
	if code := report(&env{}, usageCodef("x", "no such flag")); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
}

// Every command in the top-level help is an entry the help renderer recognises
// — a name, at least two spaces, its description — or it prints uncoloured
// beside its teal neighbours. `deployments` filled the column to one space and
// was the one name `toft` showed plain.
func TestEveryTopLevelCommandIsAHelpEntry(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	usage(f)
	f.Close()
	raw, _ := os.ReadFile(f.Name())
	text := string(raw)
	start := strings.Index(text, "Commands:\n")
	end := strings.Index(text, "\n\nCommon flags:")
	if start < 0 || end < start {
		t.Fatalf("no Commands block in:\n%s", text)
	}
	entry := regexp.MustCompile(`^  \S+\s{2,}\S`)
	for _, line := range strings.Split(text[start+len("Commands:\n"):end], "\n") {
		if !entry.MatchString(line) {
			t.Errorf("not a help entry (a name needs two spaces before its description): %q", line)
		}
	}
}

// A duration flag given a bare number names the unit it wanted, on every
// command that has one — `flag`'s own sentence ("parse error") did not (drills
// 2026-10-28 #11). Refused before anything is sent: exit 2, invalid_duration.
func TestADurationFlagNamesItsUnit(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  func(*env, []string) error
		args []string
		flag string
	}{
		{"fetch", cmdFetch, []string{"/", "--timeout", "200"}, "--timeout"},
		{"deploy", cmdDeploy, []string{"--verify-timeout", "200"}, "--verify-timeout"},
		{"job run", cmdJob, []string{"run", "nightly", "--timeout", "200"}, "--timeout"},
		{"restart", cmdRestart, []string{"--wait-timeout=200"}, "--wait-timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, buf := testEnv()
			e.json = true
			if got := report(e, c.cmd(e, c.args)); got != exitUsage {
				t.Fatalf("exit %d, want %d:\n%s", got, exitUsage, buf)
			}
			want := c.flag + ` takes a duration such as 30s or 2m, not "200"`
			if !strings.Contains(buf.String(), want) || !strings.Contains(buf.String(), `"code":"invalid_duration"`) {
				t.Errorf("want %q and the code:\n%s", want, buf)
			}
		})
	}
}

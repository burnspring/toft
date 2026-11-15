package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --source is refused alongside --deploy, and refused for a value that is not
// one of the two voices.
//
// The pairing matters more than it looks: a build log is the builder's output
// and has no app half to separate from, so accepting the flag there would imply
// a distinction that does not exist.
func TestLogsSourceFlagIsValidated(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown source", []string{"--app", "demo", "--source", "runner"}},
		{"source with deploy", []string{"--app", "demo", "--source", "app", "--deploy", "dep-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testEnv()
			if err := cmdLogs(e, tc.args); err == nil {
				t.Fatal("accepted, want a refusal")
			}
		})
	}
}

// The filtered form is the pipeable form: with --source there is no marker and
// no indent, because the reason to ask for one voice is usually to pipe it and
// a prefix that survives into grep makes the CLI worse at what it is best at.
func TestRenderCLILine(t *testing.T) {
	app := logLine{Source: sourceApp, Stream: "stdout", Line: "GET / 200"}
	plat := logLine{Source: sourcePlatform, Stream: "stdout", Line: "==> Fetching release"}

	if got := renderCLILine(app, ""); got != "GET / 200" {
		t.Errorf("an app line was marked: %q", got)
	}
	if got := renderCLILine(plat, ""); got == plat.Line {
		t.Errorf("a platform line was not marked: %q", got)
	}
	// Filtered: both print bare.
	if got := renderCLILine(plat, sourcePlatform); got != plat.Line {
		t.Errorf("--source platform still marked the line: %q", got)
	}
	if got := renderCLILine(app, sourceApp); got != app.Line {
		t.Errorf("--source app changed the app's bytes: %q", got)
	}
}

func TestFilterSource(t *testing.T) {
	lines := []logLine{
		{Source: sourcePlatform, Line: "==> Fetching release"},
		{Source: sourceApp, Line: "listening on 8100"},
		{Source: sourceApp, Line: "GET / 200"},
	}
	if got := filterSource(lines, sourceApp); len(got) != 2 {
		t.Errorf("app filter kept %d lines, want 2", len(got))
	}
	if got := filterSource(lines, sourcePlatform); len(got) != 1 {
		t.Errorf("platform filter kept %d lines, want 1", len(got))
	}
}

// Following diffs lines rather than the blob. The case the blob got wrong is
// the one worth pinning: repeated identical text.
func TestNewSince(t *testing.T) {
	l := func(at, text string) logLine {
		return logLine{At: at, Source: sourceApp, Stream: "stdout", Line: text}
	}

	first := []logLine{l("t1", "tick"), l("t2", "tick"), l("t3", "tick")}
	if got := newSince(first, nil); len(got) != 3 {
		t.Errorf("first poll printed %d lines, want all 3", len(got))
	}

	// One new line arrived and the window slid; only the new one is new, even
	// though every line has the same text.
	second := []logLine{l("t2", "tick"), l("t3", "tick"), l("t4", "tick")}
	got := newSince(second, first)
	if len(got) != 1 || got[0].At != "t4" {
		t.Errorf("newSince = %#v, want just the t4 line", got)
	}

	// Nothing moved.
	if got := newSince(first, first); len(got) != 0 {
		t.Errorf("an unchanged tail reprinted %d lines", len(got))
	}

	// The platform's block moved relative to the app's: same lines, another
	// order, plus one new platform line. Only that one is new — the sequence
	// match this replaced reprinted the whole tail here.
	p := func(at, text string) logLine {
		return logLine{At: at, Source: sourcePlatform, Line: text}
	}
	a := []logLine{l("t1", "a"), l("t2", "b"), p("t0", "started")}
	b := []logLine{p("t0", "started"), p("t5", "restarted"), l("t1", "a"), l("t2", "b")}
	if got := newSince(b, a); len(got) != 1 || got[0].Line != "restarted" {
		t.Errorf("a shifted block printed %#v, want only the new platform line", got)
	}

	// No stamps at all: a second copy of a line is new by count.
	n := func(text string) logLine { return logLine{Source: sourceApp, Line: text} }
	if got := newSince([]logLine{n("x"), n("y"), n("x")}, []logLine{n("x"), n("y")}); len(got) != 1 || got[0].Line != "x" {
		t.Errorf("stampless follow printed %#v, want the second x", got)
	}
}

// --since is refused beside --deploy, for the same shape of reason --source is:
// a build log is one contiguous thing, not a stream to take the tail of.
func TestLogsSinceRefusedWithDeploy(t *testing.T) {
	e, _ := testEnv()
	err := cmdLogs(e, []string{"--app", "demo", "--since", "2h", "--deploy", "dep-1"})
	if err == nil {
		t.Fatal("--since --deploy was accepted")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
}

// A value that is neither a timestamp nor a duration is refused before any
// request, rather than filtering nothing.
func TestLogsSinceIsValidated(t *testing.T) {
	e, _ := testEnv()
	if err := cmdLogs(e, []string{"--app", "demo", "--since", "yesterdayish"}); err == nil {
		t.Fatal("an unreadable --since was accepted")
	}
}

// The filter keeps what is at or after the moment, and keeps a line whose stamp
// it cannot read — narrowing the view must not lose data to a formatting
// difference.
func TestFilterSince(t *testing.T) {
	cut := mustTime(t, "2026-08-21T12:00:00Z")
	lines := []logLine{
		{At: "2026-08-21T11:59:59Z", Line: "before"},
		{At: "2026-08-21T12:00:00Z", Line: "exactly at"},
		{At: "2026-08-21T12:00:01Z", Line: "after"},
		{At: "not a time", Line: "unparseable"},
	}
	got := filterSince(lines, cut)
	if len(got) != 3 {
		t.Fatalf("kept %d lines, want 3: %+v", len(got), got)
	}
	if got[0].Line != "exactly at" {
		t.Errorf("the boundary line was dropped: %+v", got)
	}
	if got[2].Line != "unparseable" {
		t.Errorf("a line with an unreadable stamp was dropped: %+v", got)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// --lines below 1 is refused before any request, exit 2.
func TestLogsLinesMustBePositive(t *testing.T) {
	for _, n := range []string{"0", "-5"} {
		e, _ := testEnv()
		err := cmdLogs(e, []string{"--app", "demo", "--lines", n})
		if got := report(e, err); got != exitUsage {
			t.Errorf("--lines %s exited %d, want %d", n, got, exitUsage)
		}
	}
}

// --lines tails a build log too; unset, the whole log is printed. And a pipe
// gets the app's text without its colour codes.
func TestLogsDeployTailsAndStripsColourOffATerminal(t *testing.T) {
	const id = "0b6f7a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			fmt.Fprint(w, `{"build_log":"one\ntwo\n\u001b[35mthree\u001b[0m\n"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdLogs(e, []string{"--app", "demo", "--deploy", id, "--lines", "1"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.HasSuffix(got, "three\n") || strings.Contains(got, "two") || strings.Contains(got, "\x1b") {
		t.Errorf("--lines 1 printed %q", got)
	}
	buf.Reset()
	if err := cmdLogs(e, []string{"--app", "demo", "--deploy", id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "one\ntwo\n") {
		t.Errorf("the whole log was not printed: %q", buf.String())
	}
}

func TestLogTextKeepsColourForATerminal(t *testing.T) {
	e := &env{}
	const coloured = "\x1b[35mGET /\x1b[0m"
	if got := e.logText(coloured); got != coloured {
		t.Errorf("a terminal lost the colours: %q", got)
	}
	e.plainLogs = true
	if got := e.logText(coloured + "\x1b]8;;http://x\x07link\x1b]8;;\x07"); got != "GET /link" {
		t.Errorf("stripped = %q", got)
	}
}

// A terminal keeps the app's colours and nothing else it wrote: no clipboard
// write, no title, no hyperlink, no cursor move, no reset (CLI-5).
func TestATerminalGetsOnlyTheColours(t *testing.T) {
	e := &env{}
	in := "\x1b[1;35mGET /\x1b[0m" +
		"\x1b]52;c;cGF5bG9hZA==\x07" + // clipboard
		"\x1b]0;owned\x1b\\" + // title
		"\x1b]8;;file:///etc\x07link\x1b]8;;\x07" + // hyperlink
		"\x1b[2J\x1b[H" + // clear and home
		"\x1bc" + // full reset
		"\x1b]unterminated"
	want := "\x1b[1;35mGET /\x1b[0mlinkunterminated"
	if got := e.logText(in); got != want {
		t.Errorf("logText = %q, want %q", got, want)
	}
}

// No control character but a tab and a newline reaches the terminal: a `\r` or
// `\b` overwrote the CLI's own prefix, SO switched the charset, and U+009B and
// U+009D are CSI and OSC with no ESC (security review 2026-11-14, CS-6).
func TestLogTextDropsControls(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\rb", "b"},
		{"line\r\nnext\r", "line\nnext"},
		{"x\b\by", "xy"},
		{"\x0eq", "q"},
		{"\u009d0;t\u0007z", "0;tz"},
		{"\u009b2Jz", "2Jz"},
		{"\x1b[31mred\x1b[0m\ttab\nnext", "\x1b[31mred\x1b[0m\ttab\nnext"},
		{"caf\u00e9 \u2014 ok", "caf\u00e9 \u2014 ok"},
		{"\x9bbyte", "byte"},
	}
	e := &env{}
	for _, c := range cases {
		got := e.logText(c.in)
		if got != c.want {
			t.Errorf("logText(%q) = %q, want %q", c.in, got, c.want)
		}
		for _, r := range got {
			if isControl(r) && r != 0x1b {
				t.Errorf("logText(%q) kept control %U", c.in, r)
			}
		}
		plain := plainText(c.in)
		for _, r := range plain {
			if isControl(r) {
				t.Errorf("plainText(%q) = %q kept control %U", c.in, plain, r)
			}
		}
	}
	plain := &env{plainLogs: true}
	if got := plain.logText("\x1b[31mred\x1b[0m\rover"); got != "over" {
		t.Errorf("plain logText = %q, want %q", got, "over")
	}
}

// A step boundary holds back what the steps after the finished one wrote, so
// its ✓ lands between — including when one fetch spans several steps, which is
// how "==> Detecting framework" came to print above "✓ upload fetch".
func TestHeldFromIsTheFirstLaterStepsOpening(t *testing.T) {
	spanning := "==> Source uploaded — staging release\n==> Detecting framework...\n    Detected: static\n==> Base path: /apps/x\n"
	for _, c := range []struct {
		fresh, finished string
		want            int
	}{
		{spanning, "fetch", len("==> Source uploaded — staging release\n")},
		{spanning, "setup", 0},
		{"    Detected: flask\n==> Base path: /apps/x\n", "detect", len("    Detected: flask\n==> Base path: /apps/x\n")},
		{"==> Detecting framework...\n", "scan", 0},
		{"==> Detecting framework...\n", "detect", len("==> Detecting framework...\n")},
		{"    Installed 7 packages\n", "build", len("    Installed 7 packages\n")},
		{"    built\n==> Starting release r2 (shared mode)...\n", "build", len("    built\n")},
	} {
		if got := heldFrom(c.fresh, c.finished); got != c.want {
			t.Errorf("heldFrom(%q, %q) = %d, want %d", c.fresh, c.finished, got, c.want)
		}
	}
}

// The follower's ordinary pull holds back a later step's opening too: with the
// deploy still in fetch, a log that already carries detect's lines prints
// fetch's alone, and the rest waits for the next step — then the end prints
// everything.
func TestPullHoldsWhatLaterStepsWroteUntilTheyBegin(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	const log = "==> Source uploaded — staging release\n==> Detecting framework...\n    Detected: static\n"
	tail := &logTail{echo: true, fetch: func() (string, error) { return log, nil }, stop: make(chan struct{})}
	tail.setStep("fetch")
	tail.pull(e)
	if got := buf.String(); got != "==> Source uploaded — staging release\n" {
		t.Fatalf("in fetch, pull printed %q", got)
	}
	tail.setStep("detect")
	tail.pull(e)
	if got := buf.String(); got != log {
		t.Fatalf("in detect, the log is %q", got)
	}
	buf.Reset()
	held := &logTail{echo: true, fetch: func() (string, error) { return log, nil }, stop: make(chan struct{})}
	held.setStep("fetch")
	held.close(e)
	if got := buf.String(); got != log {
		t.Errorf("the end printed %q, want the whole log", got)
	}
}

// --source job names the command that has job output, rather than the two
// sources it is not (porting feedback 2026-11-03).
func TestLogsSourceJobPointsAtJobLogs(t *testing.T) {
	err := cmdLogs(&env{}, []string{"--app", "demo", "--source", "job"})
	if err == nil || !strings.Contains(err.Error(), "toft job logs") {
		t.Fatalf("err = %v", err)
	}
}

// Two fetches are in flight at once while a deploy echoes its log — the
// follower's and the step boundary's — and the shorter, older answer taking
// the lock second must not rewind the cursor and print the difference twice
// (code review 2026-11-11, M2-4).
func TestAStaleBuildLogFetchDoesNotRewindTheTail(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	text := "A\nB\nC\n"
	tail := &logTail{echo: true, fetch: func() (string, error) { return text, nil }, stop: make(chan struct{})}
	tail.ended = true
	tail.pull(e)
	if buf.String() != "A\nB\nC\n" {
		t.Fatalf("first pull printed %q", buf.String())
	}
	text = "A\nB\n" // the older, shorter answer lands late
	tail.pull(e)
	text = "A\nB\nC\nD\n"
	tail.pull(e)
	if got := buf.String(); got != "A\nB\nC\nD\n" {
		t.Fatalf("printed %q, want C once and then D", got)
	}
}

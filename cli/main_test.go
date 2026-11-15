package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
	"toft/cli/internal/ui"
)

// testEnv is an env that writes into a buffer rather than a terminal.
func testEnv() (*env, *bytes.Buffer) {
	var buf bytes.Buffer
	out := ui.New(false, false, false)
	out.Stderr = &buf
	out.Stdout = &buf
	return &env{out: out}, &buf
}

// A commercial refusal exits 3 like a permission problem, so the extra line is
// the only distinguisher. It is asserted here for the same reason `key_expired`
// is: this is the code a pipeline hits at whatever hour it runs, and a log that
// reads "you are not allowed" sends somebody to the wrong person.
func TestATierLimitSaysWhatItIs(t *testing.T) {
	e, buf := testEnv()
	err := &client.Error{
		Status: http.StatusForbidden, Code: "tier_limit",
		Message: "This install is on the demo tier, which runs up to 10 apps, and it has 10.",
	}
	if got := report(e, err); got != exitAuth {
		t.Fatalf("exit %d, want %d", got, exitAuth)
	}
	out := buf.String()
	if !strings.Contains(out, "This install is on the demo tier") {
		t.Errorf("the install's own sentence was not printed: %q", out)
	}
	if !strings.Contains(out, "license limit") || !strings.Contains(out, "not a permission problem") {
		t.Errorf("the extra line is missing, so this reads as a permission refusal: %q", out)
	}
}

// A viewer who runs `toft create` gets 403 and the word "forbidden", which is all
// the install will say — and on its own it reads as a broken command rather
// than as an account that was never given the publisher rung. The extra line is
// the whole difference between retrying forever and asking an administrator.
func TestAViewerIsToldItIsTheirRoleAndNotTheCommand(t *testing.T) {
	e, buf := testEnv()
	err := &client.Error{
		Status: http.StatusForbidden, Code: "deny_system_role", Message: "forbidden",
	}
	if got := report(e, err); got != exitAuth {
		t.Fatalf("exit %d, want %d", got, exitAuth)
	}
	out := buf.String()
	if !strings.Contains(out, "publisher role") {
		t.Errorf("the refusal never names the role that is missing: %q", out)
	}
	if !strings.Contains(out, "administrator") {
		t.Errorf("the refusal never names who can fix it, so it reads as retryable: %q", out)
	}
}

// A viewer of an app who deploys it, rolls it back or changes its visibility
// is deny_no_action, and an install from before that refusal carried a hint
// answers the bare word (black-box BB-1). The client's own line names the act:
// ask the owner for editor, after the publisher role if the account has none.
func TestAViewerOfAnAppIsToldWhatToAskForOnAnOlderInstall(t *testing.T) {
	e, buf := testEnv()
	err := &client.Error{
		Status: http.StatusForbidden, Code: "deny_no_action", Message: "forbidden",
	}
	if got := report(e, err); got != exitAuth {
		t.Fatalf("exit %d, want %d", got, exitAuth)
	}
	out := buf.String()
	for _, want := range []string{"editor", "access-requests", "promotion-requests"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal never names %q, so it reads as a broken command: %q", want, out)
		}
	}
}

// **Exit codes are a contract**, and this is what makes them one.
//
// The two that earn their keep are 3 and 4: a red pipeline should be triageable
// without reading the log, and "the deploy failed", "the token expired" and "the
// install was unreachable" are three different pages.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, exitOK},

		{"a failed build is the platform doing the thing and failing",
			&deployFailedError{phase: "scan", step: "scan"}, exitFailed},
		{"so is a refusal that is a wait",
			&client.Error{Status: http.StatusConflict, Code: "deploy_queued", Message: "a deployment is already queued"}, exitFailed},
		{"and a task run the overlap policy skipped",
			&client.Error{Status: http.StatusConflict, Code: "already_running"}, exitFailed},

		// classifyRefusal: nothing ran in any of these, so none is 1.
		{"a name the install cannot resolve is usage",
			&client.Error{Status: http.StatusNotFound, Code: "not_found", Message: "deployment not found"}, exitUsage},
		{"a body the install cannot read is usage",
			&client.Error{Status: http.StatusBadRequest, Code: "bad_request"}, exitUsage},
		{"a name already taken is usage",
			&client.Error{Status: http.StatusConflict, Code: "name_taken"}, exitUsage},
		{"and stays usage when the install adds its hint",
			&client.Error{Status: http.StatusConflict, Code: "name_taken", Hint: "choose another name"}, exitUsage},
		{"a disabled task is a refusal",
			&client.Error{Status: http.StatusConflict, Code: "schedule_disabled"}, exitAuth},
		{"the serving deployment is a refusal",
			&client.Error{Status: http.StatusConflict, Code: "deployment_protected"}, exitAuth},
		{"and so is any other state that refuses",
			&client.Error{Status: http.StatusConflict, Code: "conflict"}, exitAuth},
		{"a server error is still a failure",
			&client.Error{Status: http.StatusInternalServerError, Code: "internal"}, exitFailed},

		{"a bad invocation is usage", usageCodef("x", "no app is linked"), exitUsage},
		{"and so is a prompt that could not be asked",
			usageCodef("x", "refusing to roll back without --yes"), exitUsage},

		{"no credential is not a failed deploy", config.ErrNoInstall, exitAuth},
		{"nor is a credential for an install this machine has not signed in to",
			&config.NotSignedInError{Install: "https://toft.corp"}, exitAuth},
		{"a revoked key is 401", &client.Error{Status: http.StatusUnauthorized}, exitAuth},
		{"uploads switched off is 403, and still not a failed deploy",
			&client.Error{Status: http.StatusForbidden, Code: "uploads_not_allowed"}, exitAuth},
		{"a license cap is 403, and shares exit 3 with a permission problem",
			&client.Error{Status: http.StatusForbidden, Code: "tier_limit"}, exitAuth},
		{"and so does a system role too low to publish",
			&client.Error{Status: http.StatusForbidden, Code: "deny_system_role"}, exitAuth},

		// Both of these are statements about how the command was invoked, and
		// neither is exit 3: no install refused anything, because nothing was
		// sent to one. That distinction is the whole reason the token is
		// refused before the first request rather than after it.
		{"an unbound token in the environment is usage, not auth",
			&config.AmbiguousCredentialError{Install: "https://staging.corp", Origin: "--install"}, exitUsage},
		{"and so is an alias that names two installs",
			&config.AmbiguousAliasError{Alias: "prod", URLs: []string{"https://one.corp", "https://two.corp"}}, exitUsage},

		{"an install that did not answer is its own code",
			&client.UnreachableError{URL: "https://toft.corp", Err: errors.New("no route to host")}, exitUnreachable},

		{"Ctrl-C during a watch", context.Canceled, exitFailed},

		{"anything unrecognised fails rather than passing", errors.New("something else"), exitFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testEnv()
			if got := report(e, tc.err); got != tc.want {
				t.Errorf("report(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// A wrapped error is still the error it wraps. Every command returns through
// one path, and a helper that adds context on the way out must not turn a
// credential problem into a generic failure.
func TestWrappedErrorsKeepTheirCode(t *testing.T) {
	e, _ := testEnv()
	wrapped := fmt.Errorf("resolving the app: %w",
		&client.UnreachableError{URL: "https://toft.corp", Err: errors.New("timeout")})
	if got := report(e, wrapped); got != exitUnreachable {
		t.Errorf("a wrapped unreachable error exited %d, want %d", got, exitUnreachable)
	}
}

// A failed deploy has already been narrated in full, phase and step included. A
// second sentence from report() would only repeat it.
func TestAFailedDeployIsNotNarratedTwice(t *testing.T) {
	e, buf := testEnv()
	report(e, &deployFailedError{phase: "deploy", step: "build"})
	if buf.Len() != 0 {
		t.Errorf("report() said %q about a deploy the watcher already explained", buf.String())
	}
}

// **A non-interactive run never blocks on a prompt.** The failure this prevents
// is a CI job hanging for an hour on a confirmation nobody can see, killed by a
// timeout that reports nothing useful.
func TestConfirmRefusesRatherThanHanging(t *testing.T) {
	e, _ := testEnv()

	t.Setenv("CI", "1")
	err := confirm(e, &globalFlags{}, "roll phase-test back")
	if err == nil {
		t.Fatal("confirm() returned nil under CI — it would have blocked on a real terminal")
	}
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Errorf("confirm() = %v, want a usage error (exit 2)", err)
	}
	// It names the flag that would have answered it, which is the difference
	// between a fixable pipeline and a puzzle.
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal does not name --yes: %v", err)
	}

	// And --yes is the answer, in the same run.
	if err := confirm(e, &globalFlags{yes: true}, "roll phase-test back"); err != nil {
		t.Errorf("--yes did not answer the confirmation: %v", err)
	}
}

// **A confirmation is answered from the env's stdin**, so a test can answer
// one: until it was, every confirm() read os.Stdin directly and no test had
// ever said yes or no to one. Only `y` agrees; anything else, nothing at all,
// and a stream that ends are all a no.
func TestConfirmReadsTheAnswer(t *testing.T) {
	t.Setenv("CI", "")
	for _, c := range []struct {
		typed string
		yes   bool
	}{
		{"y\n", true}, {"Y\n", true}, {" y \n", true},
		{"n\n", false}, {"yes\n", false}, {"\n", false}, {"", false},
	} {
		e, _ := testEnv()
		e.stdinTerminal = func() bool { return true }
		e.stdin = strings.NewReader(c.typed)
		err := confirm(e, &globalFlags{}, "roll phase-test back")
		if c.yes && err != nil {
			t.Errorf("typed %q: confirm() = %v, want agreement", c.typed, err)
		}
		if !c.yes {
			var usage *usageError
			if !errors.As(err, &usage) || !strings.Contains(err.Error(), "cancelled") {
				t.Errorf("typed %q: confirm() = %v, want the cancelled usage error", c.typed, err)
			}
		}
	}

	// A piped stdin is unattended however the answer would have read, and the
	// stream is never consulted.
	e, _ := testEnv()
	e.stdinTerminal = func() bool { return false }
	e.stdin = strings.NewReader("y\n")
	if err := confirm(e, &globalFlags{}, "roll phase-test back"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("confirm() with stdin piped = %v, want the refusal naming --yes", err)
	}
}

// Unattended is a mode the CLI detects, not a flag the user remembers.
func TestUnattendedIsDetected(t *testing.T) {
	e, _ := testEnv()
	t.Setenv("CI", "")
	// Under `go test`, stdin is not a terminal — which is itself one of the
	// three signals, and is why this asserts the detection rather than a
	// hardcoded false.
	if !unattended(e, &globalFlags{}) {
		t.Error("a run with no terminal on stdin should be unattended")
	}
	t.Setenv("CI", "true")
	if !unattended(e, &globalFlags{}) {
		t.Error("CI being set should be enough on its own")
	}
	if !unattended(e, &globalFlags{yes: true}) {
		t.Error("--yes should be enough on its own")
	}
}

// An unknown command is a usage error, not a crash and not a silent success.
func TestUnknownCommandIsUsage(t *testing.T) {
	if got := run([]string{"deply"}); got != exitUsage {
		t.Errorf("run(deply) = %d, want %d", got, exitUsage)
	}
	if got := run(nil); got != exitUsage {
		t.Errorf("run() with no arguments = %d, want %d", got, exitUsage)
	}
}

// `--version` and `--help` are answers, not failures. (They write to the real
// stdout, so this test is the noisy one in the package — that noise *is* the
// property: help goes to stdout, where a person can pipe it.)
func TestHelpAndVersionSucceed(t *testing.T) {
	for _, arg := range []string{"--help", "help", "--version", "version"} {
		if got := run([]string{arg}); got != exitOK {
			t.Errorf("run(%s) = %d, want 0", arg, got)
		}
	}
}

// The install-newer-than-the-client comparison, which decides whether one
// dimmed line is printed and nothing else.
func TestVersionComparison(t *testing.T) {
	cases := []struct {
		install, client string
		newer           bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.1.1", "0.1.0", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		// A pre-release marker is not compared: 0.2.0 and 0.2.0-rc1 are the
		// same install as far as this line is concerned.
		{"0.2.0-rc1", "0.2.0", false},
		// Something unparseable gets silence rather than a line about nothing.
		{"nightly", "0.1.0", false},
		{"", "0.1.0", false},
	}
	for _, tc := range cases {
		if got := newerThan(tc.install, tc.client); got != tc.newer {
			t.Errorf("newerThan(%q, %q) = %v, want %v", tc.install, tc.client, got, tc.newer)
		}
	}
}

// **The warning goes to stderr, once, at resolution.**
//
// The piped output of `toft apps` is a contract. A warning in it is a parsing bug
// in somebody's script, which is a worse thing to hand somebody than the expiry
// it was warning about — so this asserts on stdout being untouched as much as on
// the warning existing.
//
// It is emitted where the session is resolved because that is the one place that
// happens: once per invocation rather than once per request.
func TestExpiryWarningGoesToStderrAtResolution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	t.Setenv("TOFT_TOKEN", "")
	t.Setenv("TOFT_TOKEN_FILE", "")

	creds := &config.CredentialsFile{Installs: map[string]config.Credential{}}
	creds.Set(config.Credential{
		URL:       "https://toft.corp",
		Token:     "toftu_nearly_out",
		User:      "ada@corp.com",
		ExpiresAt: time.Now().Add(3 * 24 * time.Hour),
	})
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := captureStd(t)

	e := &env{dir: t.TempDir(), interrupted: make(chan struct{})}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	defer e.cancel()
	if _, err := e.setup(&globalFlags{}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if got := stdout(); got != "" {
		t.Errorf("stdout = %q, want nothing — a pipeline parses this", got)
	}
	said := stderr()
	if !strings.Contains(said, "expires") || !strings.Contains(said, "toft login") {
		t.Errorf("stderr = %q, want the expiry and the fix", said)
	}
}

// captureStd redirects the real descriptors, because setup builds its own Out
// from os.Stdout and os.Stderr and there is nowhere to inject a buffer.
func captureStd(t *testing.T) (stdout, stderr func() string) {
	t.Helper()
	savedOut, savedErr := os.Stdout, os.Stderr
	outFile, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outFile, errFile
	t.Cleanup(func() {
		os.Stdout, os.Stderr = savedOut, savedErr
		outFile.Close()
		errFile.Close()
	})
	read := func(f *os.File) func() string {
		return func() string {
			b, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	return read(outFile), read(errFile)
}

// A bad flag is echoed the way the caller typed it.
//
// `flag` normalizes every name to a single dash, so `--nope` came back as
// "flag provided but not defined: -nope" — a worked example of the one
// convention this CLI does not use, handed to the reader most likely to copy it
// back verbatim.
func TestABadFlagIsEchoedAsTyped(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--nope"}, "--nope"},
		{[]string{"--nope=1"}, "--nope"},
		// Typed with one dash, echoed with one. Only the spelling the caller
		// actually used is reported; nothing is guessed at.
		{[]string{"-nope"}, "-nope"},
	}
	for _, tc := range cases {
		got := echoAsTyped("flag provided but not defined: -nope", tc.args)
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("echoAsTyped(%v) = %q, want it to end %q", tc.args, got, tc.want)
		}
	}
}

// **Both channels say the same thing about the same flag.**
//
// The `--json` half was corrected to echo the flag as typed; the human half was
// not, because `flag` writes its own message straight to the writer it was
// given, before this client's error formatter is ever reached. So stdout said
// `--nope` and stderr said `-nope` — the one message on stderr with no `toft:`
// prefix, and the only refusal followed by a full page of usage.
//
// A person reading a transcript of an agent's failed run sees the stderr half,
// and the single-dash spelling is a convention this CLI does not use.
func TestABadFlagReadsTheSameOnBothChannels(t *testing.T) {
	stdout, stderr := captureStd(t)

	e := &env{dir: t.TempDir(), interrupted: make(chan struct{}), json: true}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	defer e.cancel()
	e.out = ui.New(false, true, false)

	err := cmdStatus(e, []string{"--nope"})
	if err == nil {
		t.Fatal("a flag that does not exist was accepted")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}

	said := stderr()
	if !strings.HasPrefix(said, "toft: ") {
		t.Errorf("stderr = %q, want it to start `toft: ` like every other message here", said)
	}
	if !strings.Contains(said, "--nope") || strings.Contains(said, " -nope") {
		t.Errorf("stderr = %q, want the flag as the caller typed it", said)
	}
	// The sentence and, in a plain run, its hint under it (BB-093) — not a
	// page. A refusal that dumps the usage buries itself.
	lines := strings.Split(strings.TrimSpace(said), "\n")
	if len(lines) > 2 || (len(lines) == 2 && !strings.HasPrefix(lines[1], "    ")) {
		t.Errorf("stderr is %d lines; a typo gets its sentence and one hint line:\n%s", len(lines), said)
	}

	var doc struct {
		Error string `json:"error"`
		Kind  string `json:"kind"`
		Exit  int    `json:"exit"`
	}
	if err := json.Unmarshal([]byte(stdout()), &doc); err != nil {
		t.Fatalf("stdout was not the one document a --json caller parses (%v): %q", err, stdout())
	}
	if doc.Kind != "usage" || doc.Exit != exitUsage {
		t.Errorf("stdout document = %+v, want kind usage / exit %d", doc, exitUsage)
	}
	if !strings.Contains(said, doc.Error) {
		t.Errorf("the two channels disagree:\n  stderr %q\n  stdout %q", said, doc.Error)
	}
}

// `--help` is a request for the usage text, so it still prints it — and exits 0.
func TestHelpStillPrintsTheUsage(t *testing.T) {
	_, stderr := captureStd(t)

	e := &env{dir: t.TempDir(), interrupted: make(chan struct{})}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	defer e.cancel()
	e.out = ui.New(false, true, false)

	if err := cmdStatus(e, []string{"--help"}); err != nil {
		t.Fatalf("--help returned an error: %v", err)
	}
	if said := stderr(); !strings.Contains(said, "toft status") {
		t.Errorf("--help printed %q, want the command's usage", said)
	}
}

// The install's hint replaces this client's own line for the same code: two
// wordings of one rule, printed one above the other, is the thing the envelope
// exists to end. The local line is for an install older than the envelope.
func TestTheInstallsHintWinsOverTheLocalLine(t *testing.T) {
	e, buf := testEnv()
	report(e, &client.Error{Status: http.StatusUnauthorized, Code: "key_expired",
		Message: "that key has expired", Hint: "mint a new key"})
	out := buf.String()
	if !strings.Contains(out, "Mint a new key") || strings.Contains(out, "run `toft login` to replace it") {
		t.Errorf("want the install's hint alone: %q", out)
	}

	e, buf = testEnv()
	report(e, &client.Error{Status: http.StatusUnauthorized, Code: "key_expired",
		Message: "that key has expired"})
	if !strings.Contains(buf.String(), "run `toft login` to replace it") {
		t.Errorf("an older install lost the local line: %q", buf.String())
	}
}

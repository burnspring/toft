package builder

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/events"
)

// recordingStore is the events.Store surface, remembering what was written.
type recordingStore struct {
	events []*database.AppEvent
	// claims counts notification claims: a silent event takes none.
	claims int
}

func (r *recordingStore) InsertAppEvent(e *database.AppEvent) error {
	copied := *e
	copied.ID = "e1"
	e.ID = copied.ID
	r.events = append(r.events, &copied)
	return nil
}

func (r *recordingStore) ClaimEventNotify(eventID, appID, kind, fingerprint string, cooldown time.Duration) (bool, error) {
	r.claims++
	return false, nil // never mail from these tests; the row is what is under test
}
func (r *recordingStore) MarkEventNoticeSent(string) error                     { return nil }
func (r *recordingStore) ResolveAppEvents(string, string, string) (int, error) { return 0, nil }
func (r *recordingStore) AppNotifyFailures(string) (bool, error)               { return true, nil }
func (r *recordingStore) ListAppRecipients(string) ([]database.Recipient, error) {
	return nil, nil
}
func (r *recordingStore) GetApp(string) (*database.App, error) { return nil, nil }

func newRecordingBuilder() (*Builder, *recordingStore) {
	store := &recordingStore{}
	return &Builder{opts: Options{Events: events.NewRecorder(store, nil, nil)}}, store
}

// A start that does not come up says why, once per transition.
//
// Every caller of that path had somewhere for the error to go except the one
// that matters: a restart holds the request for `wakeHold` and then answers
// 202, so a failure arriving four minutes later reached nobody. The row went to
// `crashed`, the audit said a person pressed Restart, and the reason — which
// `waitReady` had in its hand, naming the stage and the cluster's own words —
// was discarded. `crashed` with no explanation is a smaller version of the
// `running` with no explanation the watcher's grace was added to fix.
func TestAFailedStartIsRecordedOncePerTransition(t *testing.T) {
	b, store := newRecordingBuilder()
	app := &database.App{
		ID: "app-1", Slug: "reports", CurrentRelease: "reports-abc12345",
		Status: database.AppStatusRunning,
	}

	b.sayStartFailed(app, errors.New(`kube: workload reports-abc12345 failed at ready: `+
		`persistentvolumeclaim "lpv-drop-read" not found`))

	if len(store.events) != 1 {
		t.Fatalf("a failed start recorded %d events, want 1", len(store.events))
	}
	got := store.events[0]
	if got.Kind != string(events.KindAppCrashed) {
		t.Errorf("kind = %q, want app_crashed", got.Kind)
	}
	if !strings.Contains(got.Detail, "not found") {
		t.Errorf("the detail does not carry the cluster's own sentence: %q", got.Detail)
	}
	if got.Fingerprint != app.CurrentRelease {
		t.Errorf("fingerprint = %q, want the release", got.Fingerprint)
	}

	// The supervisor retrying on a backoff: the row already says crashed, and
	// five identical rows over half an hour would bury the one that explains
	// what happened.
	app.Status = database.AppStatusCrashed
	b.sayStartFailed(app, errors.New("the same failure, one backoff later"))
	if len(store.events) != 1 {
		t.Errorf("a retry against an already-crashed app recorded %d events, want 1", len(store.events))
	}
}

// Every failed deploy is recorded; only an automatic one tells anybody. A
// manual deploy or a rollback has a person watching the build log, so its row
// is silent — but it is written, or `toft events` says nothing went wrong
// about a build that failed an hour ago.
func TestEveryDeployFailureIsRecordedAndOnlyAutomaticOnesTell(t *testing.T) {
	b, store := newRecordingBuilder()
	app := &database.App{ID: "app-1", Slug: "reports"}

	b.recordDeployFailure(app, &database.Deployment{ID: "d1", TriggeredBy: database.DeployTriggerManual},
		StepBuild, "npm run build failed", "log")
	b.recordDeployFailure(app, &database.Deployment{ID: "d2", TriggeredBy: database.DeployTriggerRollback},
		StepStart, "failed to start app", "log")
	if len(store.events) != 2 || store.claims != 0 {
		t.Fatalf("a manual deploy and a rollback recorded %d events and claimed %d notifications, want 2 and 0",
			len(store.events), store.claims)
	}
	if !strings.Contains(store.events[0].Summary, "A deploy did not finish") ||
		!strings.Contains(store.events[1].Summary, "A rollback did not finish") {
		t.Errorf("summaries = %q, %q", store.events[0].Summary, store.events[1].Summary)
	}
	store.events = nil

	b.recordDeployFailure(app, &database.Deployment{ID: "d3", TriggeredBy: database.DeployTriggerAuto},
		StepBuild, "npm run build failed\nsee above", "the build log")
	if len(store.events) != 1 || store.claims != 1 {
		t.Fatalf("an auto deploy recorded %d events and claimed %d notifications, want 1 and 1",
			len(store.events), store.claims)
	}
	e := store.events[0]
	if e.Kind != string(events.KindDeployFailed) {
		t.Errorf("kind = %q", e.Kind)
	}
	if e.Fingerprint != string(StepBuild) {
		t.Errorf("fingerprint = %q, want the failing step", e.Fingerprint)
	}
	if e.DeploymentID != "d3" {
		t.Errorf("deployment id = %q", e.DeploymentID)
	}
	if strings.Contains(e.Summary, "\n") {
		t.Errorf("summary spans lines: %q", e.Summary)
	}
	if !strings.Contains(e.Summary, "npm run build failed") {
		t.Errorf("summary = %q", e.Summary)
	}
	if e.Detail != "the build log" {
		t.Errorf("detail = %q", e.Detail)
	}
}

// A builder with no recorder is the ordinary case for an install that has not
// wired one, and must not panic.
func TestRecordDeployFailureWithoutRecorder(t *testing.T) {
	b := &Builder{}
	b.recordDeployFailure(&database.App{ID: "app-1"},
		&database.Deployment{ID: "d1", TriggeredBy: database.DeployTriggerAuto}, StepBuild, "boom", "")
}

// A failed command carries its own last words back, which is what lets a clone
// failure be classified rather than reported as "exit status 128".
func TestTailBuffer(t *testing.T) {
	var tb tailBuffer
	if tb.len() != 0 || tb.string() != "" {
		t.Fatal("an empty buffer is not empty")
	}

	tb.add("only line")
	if tb.string() != "only line" {
		t.Fatalf("got %q", tb.string())
	}

	var full tailBuffer
	for i := 0; i < outputTailLines*3; i++ {
		full.add(string(rune('a' + i%26)))
	}
	if full.len() != outputTailLines {
		t.Fatalf("len = %d, want %d", full.len(), outputTailLines)
	}
	lines := strings.Split(full.string(), "\n")
	if len(lines) != outputTailLines {
		t.Fatalf("kept %d lines, want %d", len(lines), outputTailLines)
	}
	// The last line added must be the last line kept — the end of a build log
	// is where the failure is.
	want := string(rune('a' + (outputTailLines*3-1)%26))
	if lines[len(lines)-1] != want {
		t.Fatalf("last line = %q, want %q", lines[len(lines)-1], want)
	}
}

// runCommand wraps a failing command's output into its error, so callers get
// something they can classify and show.
func TestRunCommandCarriesOutput(t *testing.T) {
	b := &Builder{}
	err := b.runCommand("d1", "", "sh", []string{"-c", "echo fatal: Authentication failed >&2; exit 128"},
		nil, func(string) {})
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("error does not carry the command's own words: %q", err)
	}
	if ClassifyGitError(err.Error()) != GitFailureAuth {
		t.Fatalf("a wrapped clone failure could not be classified: %q", err)
	}

	// A command that succeeds still returns nil, output or not.
	if err := b.runCommand("d1", "", "sh", []string{"-c", "echo hello"}, nil, func(string) {}); err != nil {
		t.Fatalf("a successful command returned %v", err)
	}
}

// BB-028 (second 2026-11-03 bash): npm ci on an out-of-sync lockfile says why
// in its first error line and then prints forty of usage; the error keeps the
// first line even when the tail has scrolled past it.
func TestRunCommandKeepsTheToolsFirstErrorLine(t *testing.T) {
	b := &Builder{}
	// npm's real opening: a code line and a bare "npm error" come first.
	script := "echo 'npm error code EUSAGE'; echo 'npm error'; " +
		"echo 'npm error `npm ci` " + npmLockOutOfSync + " are in sync.'; " +
		"i=0; while [ $i -lt 40 ]; do echo \"npm error usage line $i\"; i=$((i+1)); done; exit 1"
	err := b.runCommand("d1", "", "sh", []string{"-c", script}, nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), npmLockOutOfSync) || !strings.Contains(err.Error(), "usage line 39") {
		t.Fatalf("error = %v", err)
	}
}

// A failed deploy's event carries the end of the build log, not all of it, and
// each line as a terminal would have shown it: a tool rendering to a fixed width
// padded every line to 200 columns, and the event held the whole log so padded.
func TestDeployFailureDetailIsABoundedUnpaddedTail(t *testing.T) {
	b, store := newRecordingBuilder()
	var log strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&log, "    line %d%s\n", i, strings.Repeat(" ", 190))
	}
	log.WriteString("    Downloading 10%\rDownloading 100%   \n")
	log.WriteString("ERROR: the build failed" + strings.Repeat(" ", 180) + "\n")

	b.recordDeployFailure(&database.App{ID: "app-1"},
		&database.Deployment{ID: "d1", TriggeredBy: database.DeployTriggerAuto}, StepBuild, "boom", log.String())
	if len(store.events) != 1 {
		t.Fatalf("recorded %d events", len(store.events))
	}
	lines := strings.Split(store.events[0].Detail, "\n")
	if len(lines) != eventLogTailLines {
		t.Fatalf("detail has %d lines, want %d", len(lines), eventLogTailLines)
	}
	for _, l := range lines {
		if strings.HasSuffix(l, " ") || strings.Contains(l, "\r") {
			t.Fatalf("line kept its padding or a carriage return: %q", l)
		}
	}
	if lines[0] != "    line 62" {
		t.Errorf("first line = %q, want the 63rd of the log", lines[0])
	}
	if lines[len(lines)-2] != "Downloading 100%" || lines[len(lines)-1] != "ERROR: the build failed" {
		t.Errorf("tail = %q", lines[len(lines)-2:])
	}
}

func TestTerminalLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                  "plain",
		"padded" + "     ":       "padded",
		"  indented  \t":         "  indented",
		"10%\r50%\r100%":         "100%",
		"frame\rlast   \r":       "last",
		strings.Repeat(" ", 200): "",
	} {
		if got := terminalLine(in); got != want {
			t.Errorf("terminalLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// resolvingStore is recordingStore remembering which kinds were resolved.
type resolvingStore struct {
	recordingStore
	resolved []string
}

func (r *resolvingStore) ResolveAppEvents(appID, kind, prefix string) (int, error) {
	r.resolved = append(r.resolved, kind)
	return 0, nil
}

// A successful deploy closes the dependency policy's refusal as well as the
// deploy failure: left open, the next refusal with the same fingerprint
// collapsed into it and nobody was told (docs/bugs 2026-11-15, MAIL.3).
func TestASuccessfulDeployClearsTheDependencyRefusal(t *testing.T) {
	store := &resolvingStore{}
	b := &Builder{opts: Options{Events: events.NewRecorder(store, nil, nil)}}
	b.clearDeployProblems("app-1")
	want := map[string]bool{
		string(events.KindDeployFailed):      false,
		string(events.KindGitAuthFailed):     false,
		string(events.KindDependencyBlocked): false,
	}
	for _, k := range store.resolved {
		want[k] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("a successful deploy did not resolve %s", k)
		}
	}
}

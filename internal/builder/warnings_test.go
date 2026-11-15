package builder

import (
	"strings"
	"testing"

	"toft/internal/database"
)

// **A warning that only reaches the build log reaches the caller least likely
// to read one.**
//
// `toft.toml` deliberately ignores tables its readers do not name, which
// is what lets an older platform run a newer manifest — and the cost is that a
// misspelled `[jbos]` table is silently nothing. The
// warning existed and was well worded, and lived only in the build log; a
// `--json` deploy never starts the log follower, so reading it cost a second
// command after every deploy on the chance that something was ignored.
//
// It travels three ways now: the log line it always had, the column, and the
// terminal progress frame both watchers read.
func TestManifestWarningsTravelBeyondTheBuildLog(t *testing.T) {
	dir := writeManifest(t, `
[jbos.digest]
schedule = "0 9 * * *"
command = "python digest.py"

[jobs.nightly]
command = "python worker.py"
`)
	// A static app, so the manifest is refused before anything is written to a
	// database — which is what makes a zero-value Builder enough here, and
	// which is also the case the ordering exists for: the warnings are said
	// *before* the error check, because a manifest that is about to be refused
	// is when knowing what else is in the file is worth most.
	deploy := &database.Deployment{ID: "d1"}

	var logged []string
	_, err := (&Builder{}).checkManifest(
		&database.App{ID: "app-1", Slug: "demo", Framework: string(FrameworkStatic)},
		deploy, dir, func(line string) { logged = append(logged, line) })
	if err == nil {
		t.Fatal("a static app accepted a declared job")
	}

	if !strings.Contains(strings.Join(logged, "\n"), "jbos") {
		t.Errorf("the build log did not carry the warning: %v", logged)
	}
	// On the caller's copy, which is both what goes to the column and what the
	// terminal frame is assembled from — no second read of a row this function
	// just wrote.
	if !strings.Contains(deploy.Warnings, "jbos") {
		t.Errorf("the warning was not recorded on the deploy: %q", deploy.Warnings)
	}

	// Which is what a reconnecting watcher receives: a snapshot states the
	// deploy in full, and a warning it could not carry would be the one thing
	// a client that dropped its connection lost.
	frame := SnapshotProgress(&database.Deployment{ID: "d1", Status: database.DeployStatusSuccess,
		Warnings: deploy.Warnings})
	if len(frame.Warnings) == 0 || !strings.Contains(frame.Warnings[0], "jbos") {
		t.Errorf("the snapshot carried no warning: %+v", frame.Warnings)
	}
}

// A manifest with nothing unread writes nothing, so "no warnings" costs no
// query and no column value to distinguish from a row that predates the field.
func TestACleanManifestRecordsNoWarnings(t *testing.T) {
	dir := writeManifest(t, `
[jobs.nightly]
command = "python worker.py"
`)
	deploy := &database.Deployment{ID: "d1"}
	if _, err := (&Builder{}).checkManifest(
		&database.App{ID: "app-1", Slug: "demo", Framework: string(FrameworkStatic)},
		deploy, dir, func(string) {}); err == nil {
		t.Fatal("a static app accepted a declared job")
	}
	if deploy.Warnings != "" {
		t.Errorf("deploy.Warnings = %q, want empty", deploy.Warnings)
	}
}

// The two halves of the column, and the rule that a row this build cannot read
// loses its warnings rather than failing the snapshot that carries the deploy's
// status. Nothing branches on a warning.
func TestWarningsSurviveTheColumnAndAnUnreadableOneIsNotFatal(t *testing.T) {
	if got := EncodeWarnings(nil); got != "" {
		t.Errorf("EncodeWarnings(nil) = %q, want empty — no warnings is not an empty array", got)
	}
	round := DecodeWarnings(EncodeWarnings([]string{"one", "two"}))
	if len(round) != 2 || round[0] != "one" || round[1] != "two" {
		t.Errorf("round trip = %v", round)
	}
	if got := DecodeWarnings("{not json"); got != nil {
		t.Errorf("DecodeWarnings of an unreadable row = %v, want nil", got)
	}
}

// A coded warning keeps its code on the row, so the deploy's result carries
// warning_details as toft check does — go_toolchain_fetched was a sentence and
// nothing else (BB-037, fourth 2026-11-03 black-box list). A row of sentences
// alone, written before the objects, reads as it did.
func TestACodedDeployWarningKeepsItsCode(t *testing.T) {
	var b Builder
	deploy := &database.Deployment{ID: "d1"}
	b.addDeployWarnings(deploy, []string{"plain"})
	b.addDeployWarningDetails(deploy, []Warning{{Code: WarningGoToolchainFetched, Message: "go.mod asks"}})
	if got := DecodeWarnings(deploy.Warnings); len(got) != 2 || got[0] != "plain" || got[1] != "go.mod asks" {
		t.Errorf("sentences = %v", got)
	}
	details := DecodeWarningDetails(deploy.Warnings)
	if len(details) != 1 || details[0].Code != WarningGoToolchainFetched || details[0].Message != "go.mod asks" {
		t.Errorf("details = %+v", details)
	}
	ev := SnapshotProgress(&database.Deployment{ID: "d1", Warnings: deploy.Warnings})
	if len(ev.WarningDetails) != 1 || len(ev.Warnings) != 2 {
		t.Errorf("snapshot = %+v", ev)
	}
	if got := DecodeWarningDetails(`["old row"]`); got != nil {
		t.Errorf("an old row's details = %+v, want none", got)
	}
}

// Judging a release's jobs records none of them: the definitions are written at
// the flip, by applyManifestJobs, once the release that declares them serves.
// They were written at detect, so for the length of every build the app's jobs
// were the new toft.toml's while runs executed the old release, and a build
// that then failed left them standing over the old code (drills, 2026-10-23).
// A zero-value Builder has no database: reaching one here would panic.
func TestCheckingAManifestRecordsNoJobs(t *testing.T) {
	dir := writeManifest(t, `
[jobs.nightly]
command = "python worker.py"
schedule = "0 3 * * *"
`)
	decl, err := (&Builder{}).checkManifest(
		&database.App{ID: "app-1", Slug: "demo", Framework: string(FrameworkFlask)},
		&database.Deployment{ID: "d1"}, dir, func(string) {})
	defs := decl.Jobs
	if err != nil {
		t.Fatalf("checkManifest: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "nightly" || defs[0].Schedule != "0 3 * * *" {
		t.Errorf("defs = %+v, want the one declared job, to record at the flip", defs)
	}
}

// The earlier lines a missing-wheel refusal may suggest: distinct, below the
// release's own, newest first (BB-039, fourth 2026-11-03 black-box list).
func TestEarlierSeriesAreBelowTheReleasesNewestFirst(t *testing.T) {
	got := earlierSeries("3.15", []string{"3.13", "3.15", "3.14", "3.13"})
	if strings.Join(got, ",") != "3.14,3.13" {
		t.Errorf("earlierSeries = %v", got)
	}
	if got := earlierSeries("3.13", []string{"3.13", "3.14"}); len(got) != 0 {
		t.Errorf("nothing below the oldest, got %v", got)
	}
}

// BB-038: the failure message's tail carries no terminal escape codes.
func TestTheFailedStartTailHasNoEscapeCodes(t *testing.T) {
	if got := ansiEscape.ReplaceAllString("\x1b[31mError\x1b[0m: boom", ""); got != "Error: boom" {
		t.Errorf("stripped = %q", got)
	}
}

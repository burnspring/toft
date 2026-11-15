package builder

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/deploysteps"
)

// A StepLog continues the row it is handed. Without this an uploaded deploy's
// first two steps — recorded by the API handler that received the bytes —
// would be overwritten by the build worker's first close, and the upload would
// show as the one phase that cost nothing.
func TestNewStepLogContinuesRecordedTimings(t *testing.T) {
	s := NewStepLog(nil, nil, &database.Deployment{
		ID:          "d1",
		StepTimings: `[{"step":"receive","ms":4200},{"step":"unpack","ms":90}]`,
	})
	if len(s.done) != 2 {
		t.Fatalf("carried %d recorded steps, want 2", len(s.done))
	}
	if s.done[0].Step != string(StepReceive) || s.done[0].MS != 4200 {
		t.Errorf("first carried step is %+v", s.done[0])
	}
}

// Timings this binary cannot parse are dropped, not fatal. The row may have
// been written by a newer version mid-upgrade; recording from here loses the
// earlier durations, which were already lost, and refusing to record would
// lose the rest of the deploy as well.
func TestNewStepLogSurvivesUnreadableTimings(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"step":"fetch"}`} {
		s := NewStepLog(nil, nil, &database.Deployment{ID: "d1", StepTimings: raw})
		if len(s.done) != 0 {
			t.Errorf("StepTimings %q carried %d steps, want none", raw, len(s.done))
		}
	}
}

// The step a deploy reports and the phase clients group it under never
// disagree, because the frame derives one from the other rather than taking
// both from its caller.
func TestProgressEventDerivesItsPhase(t *testing.T) {
	e := NewProgressEvent("d1", database.DeployStatusBuilding, StepScan, "")
	if e.Phase != "scan" || e.Step != "scan" {
		t.Errorf("scan step reported as phase %q step %q", e.Phase, e.Step)
	}
	if e.Terminal() {
		t.Error("a building deploy is not terminal")
	}

	done := NewProgressEvent("d1", database.DeployStatusFailed, StepBuild, "npm ci failed")
	if done.Phase != "deploy" {
		t.Errorf("build step reported as phase %q, want deploy", done.Phase)
	}
	if !done.Terminal() {
		t.Error("a failed deploy is terminal — a watcher that misses this waits forever")
	}
}

// Publishing to a deployment nobody is watching, and to a builder that has no
// fabric at all, are both no-ops rather than panics: a deploy must never fail
// because of the machinery that describes it.
func TestPublishProgressIsSafeWithoutWatchers(t *testing.T) {
	(&Builder{watchers: map[string][]chan ProgressEvent{}}).
		PublishProgress("d1", database.DeployStatusBuilding, StepFetch, "")
	var nilBuilder *Builder
	nilBuilder.PublishProgress("d1", database.DeployStatusBuilding, StepFetch, "")
}

// A watcher that has stopped reading is dropped from, never blocked on.
func TestPublishProgressDoesNotBlockOnAFullWatcher(t *testing.T) {
	b := &Builder{watchers: make(map[string][]chan ProgressEvent)}
	ch := b.SubscribeProgress("d1")
	for i := 0; i < cap(ch)+5; i++ {
		b.PublishProgress("d1", database.DeployStatusBuilding, StepBuild, "")
	}
	if len(ch) != cap(ch) {
		t.Errorf("watcher holds %d frames, want it filled to %d and the rest dropped", len(ch), cap(ch))
	}
	b.UnsubscribeProgress("d1", ch)
	if _, ok := b.watchers["d1"]; ok {
		t.Error("the last watcher left but its deployment is still in the map")
	}
}

// A failure frame carries both halves of why, and the message is not clipped.
//
// The clipping is the bug this replaces: a dependency refusal names the
// packages, the advisories and the versions that fix them, and firstLine() sent
// the headline alone. A client whose only view of a failure is the frame — a
// `--json` run, which never reads the build log — was left with a sentence it
// could do nothing with.
func TestFailureEventCarriesTheWholeReason(t *testing.T) {
	msg := "REFUSED: this install does not deploy packages with known high advisories.\n\n" +
		"  lodash 4.17.20  →  GHSA-xxxx  (high)\n     Fixed in 4.17.21."
	e := NewFailureEvent("d1", StepScan, deploysteps.ReasonDependencyBlocked, msg)

	if e.Reason != "dependency_blocked" {
		t.Errorf("reason %q, want dependency_blocked", e.Reason)
	}
	if e.Message != msg {
		t.Errorf("message was clipped to %q", e.Message)
	}
	if !strings.Contains(e.Message, "Fixed in 4.17.21") {
		t.Error("the fixed version did not survive, which is the one thing a caller can act on")
	}
	if e.Phase != "scan" || !e.Terminal() {
		t.Errorf("frame reports phase %q terminal %v", e.Phase, e.Terminal())
	}
}

// A frame that did not fail carries no reason. An empty reason means
// unclassified, and a running deploy must not be able to look like one that
// failed for an unknown cause.
func TestRunningFrameCarriesNoReason(t *testing.T) {
	e := NewProgressEvent("d1", database.DeployStatusBuilding, StepBuild, "")
	if e.Reason != "" {
		t.Errorf("a building deploy reported reason %q", e.Reason)
	}
}

// The snapshot says why, which is the whole point of storing it: a watcher that
// reconnects, attaches with --watch, or arrives after a fast build has already
// finished receives *only* this frame, and it used to report a failed deploy
// with no reason at all.
func TestSnapshotOfAFailedDeploySaysWhy(t *testing.T) {
	e := SnapshotProgress(&database.Deployment{
		ID:             "d1",
		Status:         database.DeployStatusFailed,
		CurrentStep:    string(StepBuild),
		FailureReason:  string(deploysteps.ReasonBuildFailed),
		FailureMessage: "npm ci exited 1",
	})
	if !e.Terminal() {
		t.Fatal("a snapshot of a failed deploy must be terminal")
	}
	if e.Reason != "build_failed" || e.Message != "npm ci exited 1" {
		t.Errorf("snapshot reported reason %q message %q", e.Reason, e.Message)
	}
	if e.Phase != "deploy" {
		t.Errorf("snapshot phase %q, want deploy", e.Phase)
	}
}

// A row written before the columns existed reports nothing rather than
// something wrong. Absent is unclassified, and never internal_error.
func TestSnapshotOfAnOlderRowClassifiesAsNothing(t *testing.T) {
	e := SnapshotProgress(&database.Deployment{
		ID: "d1", Status: database.DeployStatusFailed, CurrentStep: string(StepBuild),
	})
	if e.Reason != "" || e.Message != "" {
		t.Errorf("a pre-columns row reported reason %q message %q", e.Reason, e.Message)
	}
}

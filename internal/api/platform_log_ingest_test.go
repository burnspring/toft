package api

import (
	"strings"
	"testing"
	"time"

	"toft/internal/applog"
	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runner"
)

type capturingStore struct{ lines []database.PlatformLogLine }

func (c *capturingStore) InsertPlatformLog(lines []database.PlatformLogLine) error {
	c.lines = append(c.lines, lines...)
	return nil
}

// A service's batch: the platform's half is kept, the app's is dropped.
//
// An app-labelled line here comes from a pod carrying the old runner, whose
// home is that pod's stdout; an *unlabelled* one is the same runner's, since it
// labelled nothing. Neither is stored, which is the whole reason
// ContractVersion does not move for this.
func TestIngestKeepsOnlyThePlatformsVoice(t *testing.T) {
	store := &capturingStore{}
	sink := applog.New(store)
	defer sink.Close()
	srv := &Server{platformLog: sink}

	srv.storePlatformLines(
		&auth.RunClaims{AppID: "app-1", WorkloadName: "demo-r1"},
		runner.LogBatch{Lines: []runner.LogLine{
			{Seq: 1, At: time.Now(), Source: runner.LogSourcePlatform, Stream: "stdout", Line: "==> Fetching release"},
			{Seq: 2, At: time.Now(), Source: runner.LogSourceApp, Stream: "stdout", Line: "the app printed this"},
			{Seq: 3, At: time.Now(), Stream: "stdout", Line: "an older runner labelled nothing"},
		}})
	sink.Flush()

	if len(store.lines) != 1 {
		t.Fatalf("stored %d lines, want just the platform's: %+v", len(store.lines), store.lines)
	}
	got := store.lines[0]
	if got.Line != "==> Fetching release" {
		t.Errorf("stored %q", got.Line)
	}
	if got.AppID != "app-1" || got.WorkloadName != "demo-r1" {
		t.Errorf("the claims' subject did not survive: %+v", got)
	}
}

// The runner's own drop count is relayed as a line. Truncation counted out
// loud is the reason the runner counts it rather than dropping silently.
func TestIngestRelaysTheRunnersDropCount(t *testing.T) {
	store := &capturingStore{}
	sink := applog.New(store)
	defer sink.Close()
	srv := &Server{platformLog: sink}

	srv.storePlatformLines(
		&auth.RunClaims{AppID: "app-1", WorkloadName: "demo-r1"},
		runner.LogBatch{
			Lines:   []runner.LogLine{{Source: runner.LogSourcePlatform, Stream: "stdout", Line: "==> Started"}},
			Dropped: 17,
		})
	sink.Flush()

	if len(store.lines) != 2 {
		t.Fatalf("stored %d lines, want the line and the count", len(store.lines))
	}
	last := store.lines[1]
	if !strings.Contains(last.Line, "17 line(s) dropped") {
		t.Errorf("the drop count was silent: %q", last.Line)
	}
	if last.Stream != applog.StreamStderr {
		t.Errorf("the drop line is on %q, want stderr", last.Stream)
	}
}

// An install with no sink ingests nothing and does not panic. Absent, not off.
func TestIngestWithNoSinkIsSilent(t *testing.T) {
	srv := &Server{}
	srv.storePlatformLines(
		&auth.RunClaims{AppID: "app-1", WorkloadName: "demo-r1"},
		runner.LogBatch{Lines: []runner.LogLine{
			{Source: runner.LogSourcePlatform, Stream: "stdout", Line: "==> Started"},
		}})
}

// The cap is the platform's count, not the runner's: a compromised runner
// could ignore its own, and this path is the one it would ignore it on.
func TestIngestIsCappedByThePlatform(t *testing.T) {
	store := &capturingStore{}
	sink := applog.New(store)
	defer sink.Close()
	srv := &Server{platformLog: sink}

	var lines []runner.LogLine
	for i := 0; i < 5000; i++ {
		lines = append(lines, runner.LogLine{
			Seq: i, Source: runner.LogSourcePlatform, Stream: "stdout", Line: "chatter",
		})
	}
	srv.storePlatformLines(&auth.RunClaims{AppID: "app-1", WorkloadName: "demo-r1"},
		runner.LogBatch{Lines: lines})
	sink.Flush()

	if len(store.lines) >= 5000 {
		t.Fatalf("a runner ignoring its own cap stored %d lines", len(store.lines))
	}
	last := store.lines[len(store.lines)-1]
	if !strings.Contains(last.Line, "suppressed") {
		t.Errorf("the platform's cap was silent: %q", last.Line)
	}
}

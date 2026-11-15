package api

import (
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/runtime"
)

func at(ms int) *time.Time {
	t := time.Date(2026, 8, 22, 10, 0, 0, ms*int(time.Millisecond), time.UTC)
	return &t
}

func platformLine(t *time.Time, stream, line string) runtime.LogLine {
	return runtime.LogLine{At: t, Source: runtime.LogSourcePlatform, Stream: stream, Line: line}
}

func appLine(t *time.Time, line string) runtime.LogLine {
	return runtime.LogLine{At: t, Source: runtime.LogSourceApp, Stream: runtime.LogStreamStdout, Line: line}
}

func linesOf(resp logsResponse) []string {
	out := make([]string, 0, len(resp.Lines))
	for _, l := range resp.Lines {
		out = append(out, l.Line)
	}
	return out
}

func sharedApp() *database.App   { return &database.App{ID: "a1", RunningMode: "shared"} }
func isolatedApp() *database.App { return &database.App{ID: "a1", RunningMode: "isolated"} }

// TestSharedModeIsAssembledAsBlocks. A tail with unstamped lines — written
// before the release was started with PM2's stamping on — has no clock to
// interleave on, and inventing one would be a claim rather than an absence. The
// platform's account goes above the app's output behind a separator that says
// what the boundary is.
func TestSharedModeIsAssembledAsBlocks(t *testing.T) {
	stored := []runtime.LogLine{
		platformLine(at(1), runtime.LogStreamStdout, "==> Starting release-1"),
		platformLine(at(2), runtime.LogStreamStdout, "==> Started"),
	}
	// As parsePM2Dump produces them from lines written without a stamp.
	backend := []runtime.LogLine{
		{Source: runtime.LogSourceApp, Stream: runtime.LogStreamStdout, Line: "listening on 8081"},
		{Source: runtime.LogSourceApp, Stream: runtime.LogStreamStderr, Line: "boom"},
	}

	got := linesOf(mergeLogs(sharedApp(), stored, backend))
	want := []string{
		"==> Starting release-1",
		"==> Started",
		"── everything below is the app's own output; it was written without timestamps ──",
		"listening on 8081",
		"boom",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSharedModeDoesNotInventTimestamps. The separator and the app's lines
// carry no `At` on the way out; a stamp there would date a line the platform
// did not stamp.
func TestSharedModeDoesNotInventTimestamps(t *testing.T) {
	stored := []runtime.LogLine{platformLine(at(1), runtime.LogStreamStdout, "==> Started")}
	backend := []runtime.LogLine{{Source: runtime.LogSourceApp, Stream: runtime.LogStreamStdout, Line: "hello"}}

	resp := mergeLogs(sharedApp(), stored, backend)
	for _, l := range resp.Lines {
		if l.Source == runtime.LogSourceApp && l.At != nil {
			t.Errorf("an app line was given a timestamp PM2 never supplied: %+v", l)
		}
	}
	if resp.Lines[1].At != nil {
		t.Error("the separator was given a timestamp; it describes a boundary, not a moment")
	}
}

// TestSharedModeInterleavesByTimeWhenStamped. With PM2 stamping every app line,
// the platform's account and the app's output are one stream in time order,
// with no separator.
func TestSharedModeInterleavesByTimeWhenStamped(t *testing.T) {
	stored := []runtime.LogLine{
		platformLine(at(1), runtime.LogStreamStdout, "==> Starting release-1"),
		platformLine(at(5), runtime.LogStreamStdout, "==> Started"),
	}
	backend := []runtime.LogLine{
		appLine(at(3), "listening on 8081"),
		appLine(at(7), "GET /"),
	}
	got := linesOf(mergeLogs(sharedApp(), stored, backend))
	want := []string{"==> Starting release-1", "listening on 8081", "==> Started", "GET /"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSharedModeWithNoStoredLinesIsUnchanged — an install whose sink is absent,
// or an app that predates the table, answers exactly what it answered before.
func TestSharedModeWithNoStoredLinesIsUnchanged(t *testing.T) {
	backend := []runtime.LogLine{{Source: runtime.LogSourceApp, Line: "hello"}}
	got := linesOf(mergeLogs(sharedApp(), nil, backend))
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %q, want just the backend's line", got)
	}
}

// TestStoppedAppShowsTheStoreAlone: no workload, and the account of the start
// that preceded the stop. The pane this feature exists for.
func TestStoppedAppShowsTheStoreAlone(t *testing.T) {
	stored := []runtime.LogLine{platformLine(at(1), runtime.LogStreamStderr, "The platform has stopped restarting this app.")}
	resp := withReason(mergeLogs(sharedApp(), stored, nil), runtime.LogAbsentStopped)

	if len(resp.Lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(resp.Lines))
	}
	if resp.Reason != runtime.LogAbsentStopped {
		t.Errorf("reason is %q, want the absence word to travel with the lines", resp.Reason)
	}
	if resp.Logs == "" {
		t.Error("the flat blob is empty; `toft logs` reads exactly that")
	}
	// No separator: there is no app output to divide it from.
	if resp.Lines[0].Line[:2] == "──" {
		t.Error("a separator was drawn with nothing on the other side of it")
	}
}

// TestIsolatedModeDedupesTheOverlap is §7's same-millisecond collision.
//
// The pod's teed copy and the shipped copy of one platform line come from the
// same record() call over the same variables, so `(at, stream, line)` matches
// exactly — and only one may reach the pane.
func TestIsolatedModeDedupesTheOverlap(t *testing.T) {
	shared := platformLine(at(5), runtime.LogStreamStdout, "==> Repairing the release: venv missing")
	stored := []runtime.LogLine{
		platformLine(at(1), runtime.LogStreamStdout, "==> Fetching release"),
		shared,
	}
	backend := []runtime.LogLine{shared, appLine(at(6), "INFO: started")}

	got := linesOf(mergeLogs(isolatedApp(), stored, backend))
	seen := 0
	for _, l := range got {
		if l == shared.Line {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the repair line appears %d times:\n%q", seen, got)
	}
	if got[0] != "==> Fetching release" {
		t.Errorf("the line the backend could not reach is not first: %q", got)
	}
}

// TestIsolatedModeDoesNotReorderTheBackend. kube.Logs composes its restart
// separator without a stamp — it describes the boundary, not a moment — so a
// merge that sorted by `at` would move it away from the restart it describes.
func TestIsolatedModeDoesNotReorderTheBackend(t *testing.T) {
	separator := runtime.LogLine{
		Source: runtime.LogSourcePlatform, Stream: runtime.LogStreamStdout,
		Line: "── the container restarted here (restart 1) ──",
	}
	backend := []runtime.LogLine{
		appLine(at(10), "first generation"),
		separator,
		appLine(at(20), "second generation"),
	}
	stored := []runtime.LogLine{platformLine(at(1), runtime.LogStreamStdout, "==> Fetching release")}

	got := linesOf(mergeLogs(isolatedApp(), stored, backend))
	want := []string{
		"==> Fetching release",
		"── everything above is from the platform's record; the workload's own log begins here ──",
		"first generation",
		separator.Line,
		"second generation",
	}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestIsolatedModeWithNothingLeftOverDrawsNoSeparator — every stored line is
// one the backend is already showing, so there is no older block and no
// boundary to announce.
func TestIsolatedModeWithNothingLeftOverDrawsNoSeparator(t *testing.T) {
	l := platformLine(at(5), runtime.LogStreamStdout, "==> Fetching release")
	got := linesOf(mergeLogs(isolatedApp(), []runtime.LogLine{l}, []runtime.LogLine{l, appLine(at(6), "up")}))
	want := []string{"==> Fetching release", "up"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestAppLinesAreNeverDeduped. Only platform lines can collide — an app line
// never reaches the store — and two identical app lines in one millisecond are
// two things the app really printed.
func TestAppLinesAreNeverDeduped(t *testing.T) {
	twice := appLine(at(9), "tick")
	got := linesOf(mergeLogs(isolatedApp(),
		[]runtime.LogLine{platformLine(at(1), runtime.LogStreamStdout, "==> Fetching release")},
		[]runtime.LogLine{twice, twice}))
	n := 0
	for _, l := range got {
		if l == "tick" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("an app line was deduped: %q", got)
	}
}

// TestTheMergeNeverAnswersNil. `lines` is always present on a platform that
// has it — absence of the key is what tells `toft` it is talking to one too old
// to label lines, and `[]` is what tells it the log is empty.
func TestTheMergeNeverAnswersNil(t *testing.T) {
	resp := mergeLogs(sharedApp(), nil, nil)
	if resp.Lines == nil {
		t.Fatal("Lines is nil; an empty log and an old platform must stay tellable apart")
	}
}

// TestSubmillisecondStampsStillCollide: the marker carries milliseconds and
// nothing finer, so two copies of one line that differ only below that are the
// same line.
func TestSubmillisecondStampsStillCollide(t *testing.T) {
	base := time.Date(2026, 8, 22, 10, 0, 0, 5*int(time.Millisecond), time.UTC)
	finer := base.Add(400 * time.Microsecond)
	stored := []runtime.LogLine{{At: &finer, Source: runtime.LogSourcePlatform, Stream: runtime.LogStreamStdout, Line: "==> Started"}}
	backend := []runtime.LogLine{{At: &base, Source: runtime.LogSourcePlatform, Stream: runtime.LogStreamStdout, Line: "==> Started"}}

	got := linesOf(mergeLogs(isolatedApp(), stored, backend))
	if len(got) != 1 {
		t.Fatalf("got %q, want one copy", got)
	}
}

// The store has lines and the backend has none — both modes, same answer.
//
// This is the case the whole feature exists for: the workload is gone and the
// platform's account of it is not. The two assemblies differ in how they put
// two halves together, and must not differ when there is only one half.
func TestStoreWithoutABackendAnswersTheSameInBothModes(t *testing.T) {
	stored := []runtime.LogLine{
		platformLine(at(1), runtime.LogStreamStdout, "==> Starting release-1"),
		platformLine(at(2), runtime.LogStreamStderr, "Attempt 5 did not bring the app back up."),
	}
	shared := mergeLogs(sharedApp(), stored, nil)
	isolated := mergeLogs(isolatedApp(), stored, nil)

	if len(shared.Lines) != len(isolated.Lines) {
		t.Fatalf("shared answered %d lines, isolated %d", len(shared.Lines), len(isolated.Lines))
	}
	if shared.Logs != isolated.Logs {
		t.Errorf("the two modes disagree about one app:\nshared:   %q\nisolated: %q", shared.Logs, isolated.Logs)
	}
	for i := range shared.Lines {
		if shared.Lines[i] != isolated.Lines[i] {
			t.Errorf("line %d differs: %+v vs %+v", i, shared.Lines[i], isolated.Lines[i])
		}
	}
	// And no separator: a boundary is only worth drawing when there is
	// something on both sides of it.
	for _, l := range shared.Lines {
		if strings.HasPrefix(l.Line, "──") {
			t.Errorf("a separator was drawn with nothing on the other side: %q", l.Line)
		}
	}
}

// Neither mode invents a workload it does not have: an app with no stored
// lines and no backend answers empty, in both.
func TestNothingAnywhereIsEmptyInBothModes(t *testing.T) {
	for _, app := range []*database.App{sharedApp(), isolatedApp()} {
		resp := mergeLogs(app, nil, nil)
		if len(resp.Lines) != 0 {
			t.Errorf("%s answered %d lines from nothing", app.RunningMode, len(resp.Lines))
		}
		if resp.Lines == nil {
			t.Errorf("%s answered nil; `[]` is what tells toft the log is empty", app.RunningMode)
		}
	}
}

// The boundary carries structure, not only a sentence.
//
// It rode inside the machine-readable stream as a box-drawing line with no
// timestamp, so a caller sorting by `at` moved it to the epoch — to the top of
// the log, in front of everything it exists to divide — and a caller filtering
// the platform's narration out could not tell it from narration. It is stamped
// with the time of the line it sits in front of, and named.
func TestTheWorkloadBoundaryIsStampedAndNamed(t *testing.T) {
	stored := []runtime.LogLine{platformLine(at(1), runtime.LogStreamStdout, "==> Fetching release")}
	backend := []runtime.LogLine{appLine(at(10), "listening")}

	var marker *runtime.LogLine
	for _, l := range mergeLogs(isolatedApp(), stored, backend).Lines {
		if l.Marker != "" {
			line := l
			marker = &line
		}
	}
	if marker == nil {
		t.Fatal("the boundary carries no marker, so nothing can find it without matching its prose")
	}
	if marker.Marker != runtime.LogMarkerWorkloadStart {
		t.Errorf("marker = %q, want %q", marker.Marker, runtime.LogMarkerWorkloadStart)
	}
	if marker.At == nil {
		t.Fatal("the boundary carries no time, so sorting by `at` puts it at the epoch")
	}
	// The line it sits in front of, so a sort leaves it where it is.
	if want := at(10); !marker.At.Equal(*want) {
		t.Errorf("marker stamped %v, want the first backend line's %v", marker.At, want)
	}
}

// With nothing stamped on the far side, the boundary takes the last stored
// line's time rather than `now` — a marker later than the lines beneath it
// would sort past them the moment a caller does the obvious thing.
func TestTheBoundaryFallsBackToTheStoredSide(t *testing.T) {
	stored := []runtime.LogLine{platformLine(at(3), runtime.LogStreamStdout, "==> Fetching release")}
	backend := []runtime.LogLine{appLine(nil, "listening")}

	for _, l := range mergeLogs(isolatedApp(), stored, backend).Lines {
		if l.Marker == "" {
			continue
		}
		if l.At == nil {
			t.Fatal("no time on either side left the boundary unstamped")
		}
		if want := at(3); !l.At.Equal(*want) {
			t.Errorf("marker stamped %v, want the last stored line's %v", l.At, want)
		}
		return
	}
	t.Fatal("no boundary was drawn")
}

// TestLinesIsTheTotal: `lines` reaches three sources — the stored log and
// PM2's two files — and the answer is the newest N across all of them.
func TestLinesIsTheTotal(t *testing.T) {
	stored := []runtime.LogLine{
		platformLine(at(1), runtime.LogStreamStdout, "p1"),
		platformLine(at(4), runtime.LogStreamStdout, "p2"),
	}
	backend := []runtime.LogLine{
		appLine(at(2), "a1"), appLine(at(3), "a2"), appLine(at(5), "a3"),
		{At: at(6), Source: runtime.LogSourceApp, Stream: runtime.LogStreamStderr, Line: "e1"},
	}
	resp := tailLogs(mergeLogs(sharedApp(), stored, backend), 3)
	if got := strings.Join(linesOf(resp), "|"); got != "p2|a3|e1" {
		t.Errorf("lines = %q, want the newest three in time order", got)
	}
	if resp.Logs != "p2\na3\ne1" {
		t.Errorf("logs blob = %q; it must be the same lines", resp.Logs)
	}

	// Fewer than N is untouched.
	if got := tailLogs(mergeLogs(sharedApp(), stored, backend), 50); len(got.Lines) != 6 {
		t.Errorf("an answer under the limit was cut to %d", len(got.Lines))
	}
}

// TestACutAtTheSeparatorDropsIt: a boundary with nothing above it divides
// nothing, in either mode's shape.
func TestACutAtTheSeparatorDropsIt(t *testing.T) {
	stored := []runtime.LogLine{platformLine(at(1), runtime.LogStreamStdout, "==> Started")}
	unstamped := []runtime.LogLine{
		{Source: runtime.LogSourceApp, Stream: runtime.LogStreamStdout, Line: "one"},
		{Source: runtime.LogSourceApp, Stream: runtime.LogStreamStdout, Line: "two"},
	}
	// stored, separator, one, two — the newest three start at the separator.
	if got := strings.Join(linesOf(tailLogs(mergeLogs(sharedApp(), stored, unstamped), 3)), "|"); got != "one|two" {
		t.Errorf("shared = %q", got)
	}
	isolated := []runtime.LogLine{appLine(at(5), "one"), appLine(at(6), "two")}
	if got := strings.Join(linesOf(tailLogs(mergeLogs(isolatedApp(), stored, isolated), 3)), "|"); got != "one|two" {
		t.Errorf("isolated = %q", got)
	}
}

// TestOneVoiceIsChosenBeforeTheTail: `--lines 2 --source platform` is the
// platform's newest two, not whichever platform lines survived a cut made
// across both voices — which was nothing when the app had spoken last.
func TestOneVoiceIsChosenBeforeTheTail(t *testing.T) {
	stored := []runtime.LogLine{
		platformLine(at(1), runtime.LogStreamStdout, "p1"),
		platformLine(at(2), runtime.LogStreamStdout, "p2"),
	}
	backend := []runtime.LogLine{appLine(at(3), "a1"), appLine(at(4), "a2"), appLine(at(5), "a3")}

	resp := tailLogs(onlySource(mergeLogs(sharedApp(), stored, backend), runtime.LogSourcePlatform), 2)
	if got := strings.Join(linesOf(resp), "|"); got != "p1|p2" {
		t.Errorf("platform lines = %q, want p1|p2", got)
	}
	if resp.Logs != "p1\np2" {
		t.Errorf("logs blob = %q; it must be the same lines", resp.Logs)
	}
	resp = tailLogs(onlySource(mergeLogs(sharedApp(), stored, backend), runtime.LogSourceApp), 2)
	if got := strings.Join(linesOf(resp), "|"); got != "a2|a3" {
		t.Errorf("app lines = %q, want a2|a3", got)
	}
}

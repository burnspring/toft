package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plainOut is the non-interactive writer: no terminal, so no repaint and one
// line per completed step.
func plainOut() (*Out, *bytes.Buffer) {
	var buf bytes.Buffer
	out := New(false, false, false)
	out.Stderr = &buf
	out.Stdout = &buf
	return out, &buf
}

// "Done" is the client's own inference, and this is the list of things allowed
// to say so. The frames report where a build *is*; nothing on the wire ever
// says a step is over.
func TestWhatCountsAsDone(t *testing.T) {
	out, _ := plainOut()
	tl := NewTimeline(out)

	// Nothing seen yet: nothing is done, and nothing is invented.
	if tl.isDone("setup") {
		t.Error("setup is done before anything has been seen")
	}

	// The client attaches late — a fast build is past `setup` before the
	// upload's response has been parsed — and the frame it does see is enough
	// to place everything before it.
	tl.Frame("building", "deploy", "build")
	if !tl.isDone("setup") {
		t.Error("a step behind the current one should be done")
	}
	if tl.isDone("publish") {
		t.Error("a step ahead of the current one is not done")
	}
	if tl.isDone("build") {
		t.Error("the step in flight is not done")
	}

	// **Conditional steps are excluded from that inference by name.** A git
	// deploy has no bytes to receive; a later step does not imply `receive` was
	// skipped over, and drawing it as done would be a small lie told on every
	// timeline.
	if tl.isDone("receive") || tl.isDone("unpack") {
		t.Error("a conditional step was inferred done from a later step")
	}

	// Unless it actually ran, which the deployment row's own timings say.
	tl.Seed([]StepTiming{{Step: "receive", Millis: 1200}})
	if !tl.isDone("receive") {
		t.Error("a seeded step has a recorded duration and is done")
	}
	if tl.finished["receive"].Milliseconds() != 1200 {
		t.Errorf("seeded duration = %v, want the server's 1200ms", tl.finished["receive"])
	}
}

// The terminal frame finishes the last step. Without this the tree goes on
// spinning on `flip` after the URL has been printed, which would be the last
// thing anybody saw.
func TestTheTerminalFrameEndsTheLastStep(t *testing.T) {
	out, _ := plainOut()
	tl := NewTimeline(out)
	tl.Frame("building", "deploy", "flip")
	if tl.isDone("flip") {
		t.Fatal("flip is done while it is still running")
	}
	tl.Frame("success", "deploy", "flip")
	if !tl.isDone("flip") {
		t.Error("flip is still not done after the success frame")
	}
}

// A failed deploy does not mark the step it died in as succeeded, and does not
// mark the ones after it at all.
func TestAFailureDoesNotFinishWhatDidNotRun(t *testing.T) {
	out, buf := plainOut()
	tl := NewTimeline(out)
	tl.Frame("building", "deploy", "build")
	tl.Frame("failed", "deploy", "build")

	if tl.isDone("publish") {
		t.Error("a step after the failure was marked done")
	}
	if phase, step := tl.Where(); phase != "deploy" || step != "build" {
		t.Errorf("Where() = %q/%q, want deploy/build", phase, step)
	}
	if !strings.Contains(buf.String(), "✗") {
		t.Errorf("the failed step was not marked as failed:\n%s", buf.String())
	}
}

// Off a terminal: one line per *completed* step, with its own duration, and
// nothing that repaints. This is what a CI log shows.
func TestNonInteractiveWritesOneLinePerStep(t *testing.T) {
	out, buf := plainOut()
	tl := NewTimeline(out)
	tl.Start() // a no-op off a terminal, and it must stay one
	tl.Frame("building", "upload", "setup")
	tl.Frame("building", "upload", "fetch")
	tl.Frame("building", "scan", "scan")
	tl.Stop()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want one per finished step:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "upload") || !strings.Contains(lines[0], "setup") {
		t.Errorf("first line = %q, want the phase and the step", lines[0])
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("the non-interactive rendering emitted ANSI:\n%q", buf.String())
	}
}

// `toft deploy 2>deploy.log` from a terminal: stdout is a terminal, so the
// drawn rendering is on, but the tree repaints stderr, and stderr is a file.
// It used to write a copy of the tree every 120 ms into it, cursor escapes
// between them; the file gets the lines a CI log gets instead.
func TestARedirectedStderrIsNotRepainted(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "deploy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := New(true, false, false)
	out.Stderr = f

	tl := NewTimeline(out)
	tl.Start()
	tl.Frame("building", "upload", "setup")
	time.Sleep(300 * time.Millisecond) // two ticks of a repaint, had there been one
	tl.Frame("building", "scan", "scan")
	tl.Stop()

	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "\033[") {
		t.Fatalf("the tree repainted a redirected stderr:\n%q", got)
	}
	if !strings.Contains(string(got), "setup") {
		t.Errorf("the file did not get the line-per-step rendering:\n%q", got)
	}
}

// A phase carries a duration only when every step in it was measured. A number
// assembled from the two steps this client happened to watch would be
// confidently wrong, and a phase with no number beside it is merely quiet.
func TestAPhaseWithUnmeasuredStepsShowsNoTotal(t *testing.T) {
	out, _ := plainOut()
	tl := NewTimeline(out)
	// Attached at `build`, so `detect` and `configure` are done by inference
	// and have no durations of their own.
	tl.Frame("building", "deploy", "build")
	tl.Frame("success", "deploy", "build")

	rendered := tl.renderPhase("deploy")
	if strings.Contains(rendered, "s\n") && strings.Contains(rendered, "0.0s") {
		t.Errorf("a total was drawn from steps nobody measured: %q", rendered)
	}

	// And when every step is measured, the total is there.
	measured := NewTimeline(out)
	measured.Seed([]StepTiming{{Step: "scan", Millis: 8400}})
	if got := measured.renderPhase("scan"); !strings.Contains(got, "8.4s") {
		t.Errorf("renderPhase(scan) = %q, want the measured total", got)
	}
}

// An unknown step renders as its key rather than vanishing. The step vocabulary
// is designed so adding one is safe, and a laptop's `toft` is older than the
// install more often than not — the client must not be the thing that makes a
// safe server change unsafe.
func TestAnUnknownStepIsDrawnByItsKey(t *testing.T) {
	out, buf := plainOut()
	tl := NewTimeline(out)
	tl.Frame("building", "deploy", "sign")
	tl.Frame("building", "deploy", "flip")

	if !strings.Contains(buf.String(), "sign") {
		t.Errorf("an unknown step was dropped:\n%s", buf.String())
	}
	// And an unknown *phase* gets a row of its own rather than being swallowed.
	tl2 := NewTimeline(out)
	tl2.Frame("building", "verify", "attest")
	found := false
	for _, p := range tl2.phases() {
		if p == "?" {
			found = true
		}
	}
	if !found {
		t.Errorf("a step in no known phase was not given one: %v", tl2.phases())
	}
}

func TestFormatting(t *testing.T) {
	if got := FormatBytes(2202009); got != "2.1 MB" {
		t.Errorf("FormatBytes = %q, want 2.1 MB", got)
	}
	if got := FormatBytes(512); got != "512 B" {
		t.Errorf("FormatBytes = %q, want 512 B", got)
	}
}

// Dim writes a multi-line string as lines, never as a block padded to the
// widest line — a response body under a failed --verify was a rectangle of
// trailing spaces.
func TestDimDoesNotPadLines(t *testing.T) {
	for _, colour := range []bool{false, true} {
		var buf bytes.Buffer
		out := New(true, colour, false)
		out.Stderr = &buf
		out.Dim("    short\n    a much longer line than the first")
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.HasSuffix(line, " ") {
				t.Errorf("colour=%v: padded line %q", colour, line)
			}
		}
	}
}

// The frames are polled once a second, and setup and fetch take milliseconds:
// a deploy routinely goes from one frame on `scan`'s far side to the next. The
// line-per-step rendering is the deploy's whole account in a CI log, so a step
// the frames jumped is still ticked — after the one that was in flight, without
// a duration nobody measured, and never a conditional step that may not have
// applied.
func TestAStepTheFramesJumpedIsStillTicked(t *testing.T) {
	out, buf := plainOut()
	tl := NewTimeline(out)
	tl.Frame("building", "upload", "setup")
	tl.Frame("building", "deploy", "detect")
	tl.Frame("building", "deploy", "configure")

	var ticks []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "✓" {
			ticks = append(ticks, f[2])
		}
	}
	if got, want := strings.Join(ticks, " "), "setup fetch scan detect"; got != want {
		t.Fatalf("ticked %q, want %q:\n%s", got, want, buf.String())
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, " fetch") && !strings.HasSuffix(strings.TrimSpace(line), "fetch") {
			t.Errorf("an inferred step was given a duration: %q", line)
		}
	}
	if strings.Contains(buf.String(), "receive") || strings.Contains(buf.String(), "unpack") {
		t.Errorf("a conditional step was ticked by inference:\n%s", buf.String())
	}
}

package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Off a live run there is no wait line: the caller's own sentence stands.
func TestWaitingIsNilOffALiveRun(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	o := liveOut()
	o.Quiet = true
	r := o.Waiting(context.Background(), "waiting for approval")
	if r != nil {
		t.Fatal("a wait line was drawn under -q")
	}
	r.Stop() // safe on nil, as every Region method is
}

// **The wait line says what and for how long, and is gone when the wait is.**
func TestWaitingShowsTheWaitThenClears(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	o := liveOut()
	var screen syncBuffer
	o.Stderr = &screen
	r := o.Waiting(context.Background(), "waiting for demo to come up")
	if r == nil {
		t.Fatal("no wait line on a live run")
	}
	time.Sleep(300 * time.Millisecond)
	if got := ansi.Strip(screen.String()); !strings.Contains(got, "waiting for demo to come up · 0s") {
		t.Errorf("the line is not on screen:\n%q", got)
	}
	r.Stop()
	if v := (regionModel{func() string { return "" }}).View(); v.Content != "" {
		t.Errorf("an emptied region still draws %q", v.Content)
	}
}

func TestClockIsWholeSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{
		400 * time.Millisecond: "0s",
		9*time.Second + 900e6:  "9s",
		65 * time.Second:       "1m05s",
		12*time.Minute + 3e9:   "12m03s",
	} {
		if got := clock(d); got != want {
			t.Errorf("clock(%v) = %q, want %q", d, got, want)
		}
	}
}

// A line written above a region after its program ended — the first Ctrl-C,
// between a poll's answer and its lines — goes to stderr instead of parking
// the writer for ever, and Stop still returns (security review 2026-11-14,
// CS-9).
func TestALineAfterTheRegionEnded(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	for _, held := range []bool{false, true} {
		o := liveOut()
		var screen syncBuffer
		o.Stderr = &screen
		ctx, cancel := context.WithCancel(context.Background())
		r := o.NewRegion(ctx, func() string { return "following" })
		if r == nil {
			t.Fatal("no region on a live run")
		}
		if held {
			fmt.Fprint(r.Writer(), "half")
		}
		cancel()
		select {
		case <-r.ran:
		case <-time.After(2 * time.Second):
			t.Fatal("the program did not end with its context")
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if !held {
				fmt.Fprint(r.Writer(), "late\n")
			}
			r.Stop()
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("held=%v: a write or Stop after the program ended blocked", held)
		}
		want := "late"
		if held {
			want = "half"
		}
		if !strings.Contains(screen.String(), want) {
			t.Errorf("held=%v: %q never reached stderr: %q", held, want, screen.String())
		}
	}
}

package ui

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// syncBuffer is a buffer a program's renderer and a test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// **--logs keeps the tree live.** The log goes above the tree through the
// program, and when the tree stops it is left on screen in its final state
// with every log line above it — including one the last fetch cut mid-line.
func TestTheLiveTreeCarriesTheLog(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	o := liveOut()
	screen := &syncBuffer{}
	o.Stderr = screen
	built := 0
	prev := SetProgramHook(func(bool) { built++ })
	defer SetProgramHook(prev)

	tl := NewTimeline(o)
	tl.Start()
	if built != 1 {
		t.Fatalf("a live run built %d programs", built)
	}
	w := tl.Writer()
	tl.Frame("building", "scan", "detect")
	fmt.Fprint(w, "==> Detecting framework\nflask")
	tl.Frame("building", "deploy", "build")
	fmt.Fprint(w, " found\n")
	fmt.Fprint(w, "still building")
	tl.Frame("success", "deploy", "flip")
	tl.Stop()
	fmt.Fprint(w, "after\n")

	got := ansi.Strip(screen.String())
	for _, want := range []string{"==> Detecting framework", "flask found", "still building", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	last := strings.LastIndex(got, "deploy ")
	if last < 0 || !strings.Contains(got[last:], "✓") {
		t.Errorf("the tree was not left in its final state:\n%q", got)
	}
	if strings.LastIndex(got, "still building") > last {
		t.Errorf("a log line landed under the final tree:\n%q", got)
	}
}

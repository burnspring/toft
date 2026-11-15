package ui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// liveOut claims a person on both streams; each case below takes one claim away.
func liveOut() *Out {
	o, _ := testOut(true, 0)
	o.StderrTerminal = func() bool { return true }
	return o
}

// **Live needs every one of its conditions.** The positive case is the control:
// without it, every negative below could pass because nothing is ever live.
func TestLiveNeedsEveryCondition(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	if !liveOut().Live() {
		t.Fatal("a terminal on both streams is not live — every negative below is vacuous")
	}
	cases := map[string]func(*Out){
		"stdout piped": func(o *Out) { o.Interactive = false },
		"stderr piped": func(o *Out) { o.StderrTerminal = func() bool { return false } },
		"-q":           func(o *Out) { o.Quiet = true },
		"--json":       func(o *Out) { o.JSON = true },
		"TERM=dumb":    func(o *Out) { t.Setenv("TERM", "dumb") },
		"CI":           func(o *Out) { t.Setenv("CI", "1") },
	}
	for name, take := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("CI", "")
			o := liveOut()
			take(o)
			if o.Live() {
				t.Error("live")
			}
			built := false
			prev := SetProgramHook(func(bool) { built = true })
			defer SetProgramHook(prev)
			if p := o.Display(context.Background(), stub{}); p != nil || built {
				t.Error("a display program was built")
			}
		})
	}
}

// A display program is built on stderr, takes no input and does not start
// until asked; the hook sees it, and sees that it reads nothing.
func TestDisplayIsBuiltWithoutInput(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CI", "")
	var sawInput []bool
	prev := SetProgramHook(func(in bool) { sawInput = append(sawInput, in) })
	defer SetProgramHook(prev)
	if liveOut().Display(context.Background(), stub{}) == nil {
		t.Fatal("no program on a live run")
	}
	liveOut().Input(context.Background(), stub{}, &bytes.Buffer{}, nil)
	if len(sawInput) != 2 || sawInput[0] || !sawInput[1] {
		t.Errorf("hook saw %v, want [false true]", sawInput)
	}
}

type stub struct{}

func (stub) Init() tea.Cmd                       { return nil }
func (stub) Update(tea.Msg) (tea.Model, tea.Cmd) { return stub{}, nil }
func (stub) View() tea.View                      { return tea.NewView("") }

// **Ctrl-C in raw mode is a key, and it still interrupts.** Raw mode turns the
// signal into a byte; Input turns the byte back into the run's interrupt.
func TestCtrlCKeyInterrupts(t *testing.T) {
	interrupted := make(chan struct{})
	o := liveOut()
	var screen bytes.Buffer
	o.Stderr = &screen
	// A pipe left open, as a terminal is: the key arrives and nothing ends.
	keys, typed := io.Pipe()
	defer typed.Close()
	go typed.Write([]byte("\x03"))
	p := o.Input(context.Background(), stub{}, keys, func() { close(interrupted) })
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.Kill()
		t.Fatal("the program did not end on Ctrl-C")
	}
	select {
	case <-interrupted:
	default:
		t.Error("Ctrl-C ended the program without interrupting the run")
	}
}

// `toft deploy 2>deploy.log` from a terminal: stdout is a person's, stderr a
// file. The meter repaints with \r and the clear is an escape, so both ask
// stderr — the file got every repaint and a \x1b[K before the first step line.
// More bytes than the total — a file that grew after it was measured — draws a
// full bar rather than panicking (security review 2026-11-14, CS-10).
func TestTheMeterSurvivesMoreThanItsTotal(t *testing.T) {
	o, buf := testOut(true, 0)
	o.StderrTerminal = func() bool { return true }
	o.Meter("upload", 150, 100)
	if !strings.Contains(buf.String(), "100%") {
		t.Errorf("want a full meter, got %q", buf.String())
	}
}

func TestTheMeterDrawsOnlyWhenStderrIsATerminal(t *testing.T) {
	o, buf := testOut(true, 0)
	o.StderrTerminal = func() bool { return true }
	o.Meter("upload", 1, 2)
	o.ClearLine()
	if buf.Len() == 0 {
		t.Fatal("a terminal on both streams drew no meter — the negative below is vacuous")
	}
	o, buf = testOut(true, 0)
	o.StderrTerminal = func() bool { return false }
	o.Meter("upload", 1, 2)
	o.ClearLine()
	if buf.Len() != 0 {
		t.Errorf("stderr to a file got %q, want nothing", buf.String())
	}
}

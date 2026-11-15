package ui

// The live tier: a Bubble Tea program, drawn inline on stderr, beside the two
// tiers every command already has (the plain lines and the drawn answer).
//
// **A program only draws.** The command that starts one still computes its
// outcome, writes its answer through Answer and returns the error that carries
// its exit code; the program is stopped before any of that. So nothing a script
// or an agent reads — the answer, --json, a refusal — is reached by code that
// imports Bubble Tea, and the plain path stays the first program rather than
// becoming the second.

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

// Live is whether this run may draw a live program: a person is reading both
// streams — stdout because a piped answer means a script is reading, and a
// script's run must look like a pipe's; stderr because that is where the
// program draws. -q has asked for no narration, --json for a document, and
// TERM=dumb has said the cursor cannot move. CI is a runner, and some allocate
// a pty, which put the repaint in their log: the tree exists for a person
// watching. (TOFT_PLAIN already made the run not Interactive.) The drawn tier —
// tables, badges — does not move the cursor and does not ask this.
func (o *Out) Live() bool {
	return o.Interactive && o.NarrationTerminal() && !o.Quiet && !o.JSON &&
		os.Getenv("TERM") != "dumb" && os.Getenv("CI") == ""
}

// programBuilt is the gate's witness: every program this package constructs
// passes through it first. A test sets it to see whether a program would have
// been built under a given run, without running one.
var programBuilt func(input bool)

// SetProgramHook installs f as the witness and returns what it replaced.
func SetProgramHook(f func(input bool)) func(input bool) {
	prev := programBuilt
	programBuilt = f
	return prev
}

// Display builds a program that draws and reads nothing. Nil when the run is
// not Live, so a caller's `if p := out.Display(m); p != nil` is the whole gate.
//
// **No input means the terminal is never put in raw mode**: Ctrl-C stays a
// signal and reaches the client's own handler (newEnv), and nothing a display
// does can leave a terminal raw. It also means no terminal query is sent, since
// no reply could be read.
func (o *Out) Display(ctx context.Context, m tea.Model) *tea.Program {
	if !o.Live() {
		return nil
	}
	return o.program(ctx, m, nil)
}

// Input builds a program that reads keys from in. The caller has already asked
// whether a person is there to press them — main's input(e, g), which is Live
// and not unattended, and the only caller. A Ctrl-C key, which raw mode turned
// from a signal into a key, is handed to interrupt and ends the program, so it
// ends the run the way an interrupt does everywhere else.
//
// **Input that runs out ends the program unanswered.** A closed stdin is
// nobody left to answer, and a question that waited on it would wait forever.
func (o *Out) Input(ctx context.Context, m tea.Model, in io.Reader, interrupt func()) *tea.Program {
	// A terminal is handed over as itself: Bubble Tea puts its input in raw
	// mode only when it is given the file, and a wrapper hid it — keys were
	// line-buffered and echoed. A terminal in raw mode does not end, so the
	// wrapper is only for an input that can.
	var ended *endingReader
	reader := in
	if f, ok := in.(*os.File); !ok || !term.IsTerminal(f.Fd()) {
		ended = &endingReader{r: in}
		reader = ended
	}
	p := o.program(ctx, m, reader, tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "ctrl+c" {
			if interrupt != nil {
				interrupt()
			}
			return tea.QuitMsg{}
		}
		return msg
	}))
	// After a grace: the keys read just before the end travel through Bubble
	// Tea's own reader goroutine, and a quit sent at once overtook them.
	if ended != nil {
		ended.onEnd = func() { time.AfterFunc(inputGrace, p.Quit) }
	}
	return p
}

// inputGrace is how long the keys already read get to land once the input has
// ended. A terminal in raw mode does not end; this is a closed stdin.
const inputGrace = 200 * time.Millisecond

// endingReader calls onEnd once its reader has nothing more to give.
type endingReader struct {
	r     io.Reader
	onEnd func()
	once  sync.Once
}

func (e *endingReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err != nil && e.onEnd != nil {
		e.once.Do(e.onEnd)
	}
	return n, err
}

// Height is the terminal's, on stderr where programs draw; 24 when it will not
// say.
func (o *Out) Height() int {
	if f, ok := o.Stderr.(*os.File); ok {
		if _, h, err := term.GetSize(f.Fd()); err == nil && h > 0 {
			return h
		}
	}
	return 24
}

// program is the one call to tea.NewProgram in this module, so the options
// below cannot be forgotten at a call site: inline (never the alternate screen,
// whose view vanishes and takes what it showed with it), on stderr, and without
// Bubble Tea's own signal handler, so newEnv's stays the one path from a signal
// to an interrupted run.
func (o *Out) program(ctx context.Context, m tea.Model, in io.Reader, extra ...tea.ProgramOption) *tea.Program {
	if programBuilt != nil {
		programBuilt(in != nil)
	}
	opts := append([]tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithOutput(o.Stderr),
		tea.WithInput(in),
		tea.WithoutSignalHandler(),
		// The size the answer is fitted to, so a program on a stream that
		// will not report one still has somewhere to draw; a terminal's own
		// resize messages replace it.
		tea.WithWindowSize(o.Width(), o.Height()),
	}, extra...)
	return tea.NewProgram(m, opts...)
}

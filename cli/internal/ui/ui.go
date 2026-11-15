// Package ui is everything `toft` puts on a terminal, and the only place
// Charm — lipgloss and Bubble Tea — is imported.
//
// **Two renderings, one switch.** On a terminal the deploy is a live tree with
// a spinner, elapsed times and a byte meter. Everywhere else it is one line per
// completed step, no ANSI and no repaint — which is what makes the same binary
// readable in a GitHub Actions log without a CI flag. The switch is stdout not
// being a terminal; colour is suppressed independently by NO_COLOR, TERM=dumb
// and --no-color, because a person piping through `less -R` wants colour and a
// person piping to a file does not.
//
// **stdout is the answer and stderr is the narration.** Under --json and under
// -q that rule is what lets `toft deploy --json | jq -r .url` work without anybody
// thinking about it; it holds everywhere else too, so nothing has to remember
// which mode it is in.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-isatty"
)

// Out is where `toft` writes.
type Out struct {
	// Stdout carries the answer: the thing a script would consume.
	Stdout io.Writer
	// Stderr carries the narration: progress, warnings, the reason something
	// was refused.
	Stderr io.Writer

	// Interactive is stdout being a terminal — the one switch between the live
	// tree and one line per step.
	Interactive bool
	// Quiet suppresses narration entirely. The answer still goes to stdout.
	Quiet bool
	// JSON is the answer being a document (--json): nothing is drawn live.
	JSON bool
	// StderrTerminal, when set, answers NarrationTerminal instead of the file —
	// a test's way to claim a terminal on a buffer.
	StderrTerminal func() bool
	// Columns is the width the answer is fitted to. Zero asks the terminal, and
	// off a terminal nothing is fitted at all.
	Columns int

	// colour is whether colour was asked for at all; answerColour is whether
	// the answer may carry it — stdout being a terminal as well. They differ
	// for `toft apps > file` from a terminal, whose narration is coloured and
	// whose file must not be.
	colour       bool
	answerColour bool
	styles       styles
}

type styles struct {
	dim, bold, ok, fail, warn, running, accent, panel ink
}

// ink is a style and the colour profile of the stream its output is written to.
//
// lipgloss v2 renders every style in full ANSI and leaves the profile to the
// writer; this client assembles lines from styled pieces before it knows which
// line they end up in, so the profile is applied here, as each piece renders.
// NoTTY strips every escape — what v1's Ascii renderer produced — and a colour
// profile downsamples, which for the sixteen colours used here is nothing.
type ink struct {
	style   lipgloss.Style
	profile colorprofile.Profile
}

func (i ink) Render(s string) string { return written(i.profile, i.style.Render(s)) }

// written is s as it reads once written through a stream of profile p.
func written(p colorprofile.Profile, s string) string {
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: p}
	w.WriteString(s)
	return b.String()
}

// stderrProfile is the profile the narration is written at: none at all when
// colour is off, and otherwise what stderr says it can do. A profile below
// ANSI is treated as none, as v1's Ascii renderer was — no bold, no faint.
func stderrProfile(colour bool) colorprofile.Profile {
	if !colour {
		return colorprofile.NoTTY
	}
	if p := colorprofile.Detect(os.Stderr, os.Environ()); p >= colorprofile.ANSI {
		return p
	}
	return colorprofile.NoTTY
}

// New builds the writer for this process.
//
// colour is decided once, here, from three independent signals. NO_COLOR is a
// convention this product has no business arguing with, TERM=dumb is a terminal
// saying it cannot, and --no-color is a person saying so.
func New(interactive, colour, quiet bool) *Out {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		colour = false
	}
	// The profile is stderr's because that is where the narration goes: a run
	// whose stdout is a pipe and whose stderr is a terminal should still be
	// readable.
	profile := stderrProfile(colour)
	style := func(st lipgloss.Style) ink { return ink{st, profile} }

	// The sixteen ANSI colours and nothing finer: they are the ones a
	// terminal's own theme remaps, so green is a green that reads on that
	// person's background, light or dark, with no query to the terminal to
	// guess which it is.
	return &Out{
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		Interactive:  interactive,
		Quiet:        quiet,
		colour:       colour,
		answerColour: colour && interactive,
		styles: styles{
			dim:     style(lipgloss.NewStyle().Faint(true)),
			bold:    style(lipgloss.NewStyle().Bold(true)),
			ok:      style(lipgloss.NewStyle().Foreground(lipgloss.Color("2"))),
			fail:    style(lipgloss.NewStyle().Foreground(lipgloss.Color("1"))),
			warn:    style(lipgloss.NewStyle().Foreground(lipgloss.Color("3"))),
			running: style(lipgloss.NewStyle().Foreground(lipgloss.Color("6"))),
			accent:  style(lipgloss.NewStyle().Foreground(lipgloss.Color("4"))),
			panel:   style(panelStyle()),
		},
	}
}

// NarrationTerminal reports whether stderr — where the narration, and so any
// repaint, is written — is a terminal. Interactive is stdout's answer, and the
// two differ for `toft deploy 2>deploy.log` from a terminal: the tree used to
// repaint into the file, a copy every 120 ms with cursor escapes between them.
// A writer that is not a file (a test's buffer) is not a terminal.
func (o *Out) NarrationTerminal() bool {
	if o.StderrTerminal != nil {
		return o.StderrTerminal()
	}
	f, ok := o.Stderr.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

// Say writes a line of narration. Suppressed by -q.
func (o *Out) Say(format string, args ...any) {
	if o.Quiet {
		return
	}
	fmt.Fprintf(o.Stderr, format+"\n", args...)
}

// Dim writes narration that is supporting detail — the secret-ish file
// warning, the "this install is newer than your toft" line.
//
// Line by line: lipgloss renders a multi-line string as a block, padding every
// line to the widest with trailing spaces, which turned a response body into a
// ragged rectangle of whitespace.
func (o *Out) Dim(format string, args ...any) {
	if o.Quiet {
		return
	}
	for _, line := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		fmt.Fprintln(o.Stderr, o.styles.dim.Render(line))
	}
}

// Warn is narration that something is off but the command carries on.
func (o *Out) Warn(format string, args ...any) {
	if o.Quiet {
		return
	}
	fmt.Fprintln(o.Stderr, o.styles.warn.Render("! ")+fmt.Sprintf(format, args...))
}

// Failed is why one item of a batch failed. Stderr, and **never suppressed**:
// -q drops narration, and the batch's summary says "each is named above" —
// under -q nothing was, and a clear refusal became a count with no reason.
func (o *Out) Failed(format string, args ...any) {
	fmt.Fprintln(o.Stderr, o.styles.fail.Render("! ")+fmt.Sprintf(format, args...))
}

// Error is the sentence a refusal or a failure ends on. Never suppressed. Off a
// terminal it is `toft: <sentence>`, which is what a CI log is grepped for; on
// one it is a red ✗, because a person already knows which program is talking.
func (o *Out) Error(err error) {
	if o.Interactive {
		fmt.Fprintln(o.Stderr, o.styles.fail.Render("✗")+" "+err.Error())
		return
	}
	fmt.Fprintf(o.Stderr, "toft: %v\n", err)
}

// Hint is what follows an Error: the next step, indented under it, dimmed on a
// terminal. Each line of a multi-line hint is indented on its own.
func (o *Out) Hint(format string, args ...any) {
	for _, line := range strings.Split(strings.TrimRight(fmt.Sprintf(format, args...), "\n"), "\n") {
		if o.Interactive {
			line = o.styles.dim.Render(line)
		}
		fmt.Fprintln(o.Stderr, "    "+line)
	}
}

// Ask writes a question awaiting an answer on the same line. Never suppressed:
// -q does not answer for anybody.
func (o *Out) Ask(question, choices string) {
	if o.Interactive {
		fmt.Fprintf(o.Stderr, "%s %s %s ", o.styles.running.Render("?"), o.styles.bold.Render(question),
			o.styles.dim.Render(choices))
		return
	}
	fmt.Fprintf(o.Stderr, "%s %s ", question, choices)
}

// Answer writes to stdout — the thing a pipeline consumes. Never suppressed.
//
// **Escapes are stripped here when stdout is not a terminal.** The styles are
// rendered for stderr, which is where colour is decided, and a line assembled
// with Bold or Dimmed does not know which stream it is bound for — so
// `toft apps > file` from a terminal wrote `\x1b[2m` into the file. This is
// the one place the answer passes through, so it is the one place to stop it.
func (o *Out) Answer(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !o.answerColour {
		line = ansi.Strip(line)
	}
	fmt.Fprintln(o.Stdout, line)
}

// Bold, Dimmed and Accent are for assembling a line rather than writing one.
func (o *Out) Bold(s string) string    { return o.styles.bold.Render(s) }
func (o *Out) Dimmed(s string) string  { return o.styles.dim.Render(s) }
func (o *Out) Accent(s string) string  { return o.styles.accent.Render(s) }
func (o *Out) OK(s string) string      { return o.styles.ok.Render(s) }
func (o *Out) Fail(s string) string    { return o.styles.fail.Render(s) }
func (o *Out) Caution(s string) string { return o.styles.warn.Render(s) }
func (o *Out) Info(s string) string    { return o.styles.running.Render(s) }

// FormatBytes is the size of a bundle as a person recognises it.
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// FormatDuration is an elapsed time at the precision people read it: tenths
// under a minute, then minutes and seconds. A build that took 8.4s and one that
// took 8.412s are the same build.
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%02ds", m, s)
}

// Spinner frames, in the one place they exist. Motion is spent here and in the
// byte meter and nowhere else.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerFrame(i int) string { return spinnerFrames[i%len(spinnerFrames)] }

// Meter is the byte meter shown while a bundle uploads. It repaints stderr with
// \r, so it asks stderr's question, not stdout's: `toft deploy 2>deploy.log`
// from a terminal wrote every repaint into the file.
func (o *Out) Meter(label string, sent, total int64) {
	if !o.Interactive || !o.NarrationTerminal() || o.Quiet {
		return
	}
	// Clamped: a file that grew after it was measured sends more than its
	// total, and a negative repeat count below panicked on the transport's
	// goroutine (security review 2026-11-14, CS-10).
	sent = max(sent, 0)
	pct := 0
	if total > 0 {
		sent = min(sent, total)
		pct = int(sent * 100 / total)
	}
	width := 24
	filled := width * pct / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	fmt.Fprintf(o.Stderr, "\r  %s %s %3d%%  %s / %s   ",
		label, o.styles.accent.Render(bar), pct, FormatBytes(sent), FormatBytes(total))
}

// ClearLine erases the meter so the next thing written starts clean.
func (o *Out) ClearLine() {
	if o.Interactive && o.NarrationTerminal() && !o.Quiet {
		fmt.Fprint(o.Stderr, "\r\033[K")
	}
}

package ui

// The pieces every command draws its answer from: a status badge, a table, a
// field, a panel, a link. **Drawn for a person only on a terminal.** Off one,
// each command keeps the plain lines it has always written — an agent, a grep
// and the drills runbook read those, and nothing here is allowed to move them —
// so a command asks Rich() and branches, rather than every helper carrying a
// second rendering that would have to match the old one byte for byte.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// Rich is whether the answer is drawn for a person: stdout is a terminal.
// Colour is a separate question — `--no-color` on a terminal still gets the
// layout, in one ink.
func (o *Out) Rich() bool { return o.Interactive }

// Width is how many columns the answer may use. A terminal that will not say
// gets a hundred, which is narrower than nearly all of them and wider than a
// table needs.
func (o *Out) Width() int {
	if o.Columns > 0 {
		return o.Columns
	}
	if f, ok := o.Stdout.(*os.File); ok {
		if w, _, err := term.GetSize(f.Fd()); err == nil && w > 0 {
			return w
		}
	}
	return 100
}

// badge is how one status word is drawn: a glyph and a tone.
type badge struct {
	glyph string
	tone  func(*Out, string) string
}

var (
	good    = func(o *Out, s string) string { return o.OK(s) }
	bad     = func(o *Out, s string) string { return o.Fail(s) }
	caution = func(o *Out, s string) string { return o.Caution(s) }
	moving  = func(o *Out, s string) string { return o.Info(s) }
	quiet   = func(o *Out, s string) string { return o.Dimmed(s) }
)

// The vocabulary is every status word the platform sends that a person should
// read at a glance. **A word not here is drawn as itself**, never bucketed: a
// newer install's new status reaches the reader unchanged.
var badges = map[string]badge{
	// Up, or done and fine.
	"running": {"●", good}, "ready": {"●", good}, "success": {"✓", good},
	"succeeded": {"✓", good}, "healthy": {"●", good}, "active": {"●", good},
	"enabled": {"●", good}, "allowed": {"●", good}, "yes": {"●", good},
	"ok": {"✓", good}, "fixed": {"✓", good},
	// Resting: nothing wrong, nothing serving this second.
	"sleeping": {"○", quiet}, "stopped": {"○", quiet}, "idle": {"○", quiet},
	"paused": {"○", quiet}, "disabled": {"○", quiet}, "superseded": {"○", quiet},
	"cancelled": {"○", quiet}, "canceled": {"○", quiet}, "skipped": {"○", quiet},
	"pruned": {"○", quiet}, "info": {"•", quiet}, "none": {"○", quiet},
	"no": {"○", quiet}, "not allowed": {"○", quiet},
	// In flight.
	"building": {"◐", moving}, "deploying": {"◐", moving}, "queued": {"◌", moving},
	"pending": {"◌", moving}, "starting": {"◐", moving}, "waking": {"◐", moving},
	"restarting": {"◐", moving}, "in_progress": {"◐", moving}, "stopping": {"◐", moving},
	// Look at this.
	"warning": {"▲", caution}, "locked": {"■", caution}, "low": {"▲", caution},
	"moderate": {"▲", caution}, "medium": {"▲", caution},
	// Broken.
	"failed": {"✕", bad}, "error": {"✕", bad}, "crashed": {"✕", bad},
	"critical": {"✕", bad}, "high": {"✕", bad}, "timed_out": {"✕", bad},
	"timeout": {"✕", bad}, "lost": {"✕", bad}, "killed": {"✕", bad},
	"rejected": {"✕", bad}, "blocked": {"✕", bad}, "vulnerable": {"✕", bad},
}

// Badge is a status word as a person reads it: `● running` in green. Off a
// terminal it is the word alone, so a caller may use it unconditionally.
func (o *Out) Badge(word string) string {
	if !o.Rich() {
		return word
	}
	b, ok := badges[strings.ToLower(word)]
	if !ok {
		return word
	}
	return b.tone(o, b.glyph+" "+word)
}

// RunBadge is Badge for a job run or a deployment, where "running" means in
// flight rather than up.
func (o *Out) RunBadge(word string) string {
	if o.Rich() && word == "running" {
		return o.Info("◐ " + word)
	}
	return o.Badge(word)
}

// Table is rows in aligned columns under a dimmed header, fitted to the
// terminal by truncating one column — the one a person can most afford to
// lose the end of, which the caller names with Flex.
type Table struct {
	out  *Out
	head []string
	rows [][]string
	flex int
}

// NewTable starts a table with these column headers.
func (o *Out) NewTable(head ...string) *Table {
	return &Table{out: o, head: head, flex: -1}
}

// Flex names the column truncated to fit; the last column when unset.
func (t *Table) Flex(col int) *Table { t.flex = col; return t }

// Row adds one row. Cells may already be styled; widths are measured without
// the escapes.
func (t *Table) Row(cells ...string) { t.rows = append(t.rows, cells) }

// Print writes the table to stdout.
func (t *Table) Print() {
	for _, l := range t.Lines() {
		t.out.Answer("%s", l)
	}
}

// Lines is the table as Print writes it, a line each, for a caller that draws
// it somewhere other than stdout — `toft top`'s view.
func (t *Table) Lines() []string {
	var lines []string
	cols := len(t.head)
	widths := make([]int, cols)
	for i, h := range t.head {
		widths[i] = ansi.StringWidth(h)
	}
	for _, r := range t.rows {
		for i := 0; i < cols && i < len(r); i++ {
			widths[i] = max(widths[i], ansi.StringWidth(r[i]))
		}
	}

	flex := t.flex
	if flex < 0 || flex >= cols {
		flex = cols - 1
	}
	if t.out.Rich() {
		total := 2 * (cols - 1)
		for _, w := range widths {
			total += w
		}
		if over := total - t.out.Width(); over > 0 {
			widths[flex] = max(widths[flex]-over, 8)
		}
	}

	line := func(cells []string, style func(string) string) string {
		var b strings.Builder
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			if ansi.StringWidth(cell) > widths[i] {
				cell = ansi.Truncate(cell, widths[i], "…")
			}
			if i < cols-1 {
				cell += strings.Repeat(" ", widths[i]-ansi.StringWidth(cell)) + "  "
			}
			b.WriteString(style(cell))
		}
		return strings.TrimRight(b.String(), " ")
	}

	if t.out.Rich() {
		head := make([]string, cols)
		for i, h := range t.head {
			head[i] = strings.ToUpper(h)
		}
		lines = append(lines, line(head, t.out.Dimmed))
	}
	for _, r := range t.rows {
		lines = append(lines, line(r, func(s string) string { return s }))
	}
	return lines
}

// Field is one `key  value` line under a title, the key padded to width. Off a
// terminal it is exactly `  %-*s%s`, the line every detail view has always
// written; on one the key is dimmed so the values are what the eye lands on.
func (o *Out) Field(width int, key, value string) {
	padded := fmt.Sprintf("%-*s", width, key)
	if o.Rich() {
		padded = o.Dimmed(padded)
	}
	o.Answer("  %s%s", padded, value)
}

// Panel is a block of narration in a rounded frame: the end of a deploy, the
// one moment a command has something to hand over. Off a terminal, and under
// -q, it is the lines alone, indented as they were before there was a frame.
func (o *Out) Panel(lines ...string) {
	if o.Quiet {
		return
	}
	if !o.Interactive {
		for _, l := range lines {
			fmt.Fprintln(o.Stderr, "    "+l)
		}
		return
	}
	frame := o.styles.panel.Render(strings.Join(lines, "\n"))
	for _, l := range strings.Split(frame, "\n") {
		fmt.Fprintln(o.Stderr, "  "+l)
	}
}

// Next is the command to run next, as narration: `→ toft logs --app x`.
func (o *Out) Next(format string, args ...any) {
	if o.Quiet {
		return
	}
	fmt.Fprintln(o.Stderr, "  "+o.Dimmed("→ ")+o.Bold(fmt.Sprintf(format, args...)))
}

// Link makes text a terminal hyperlink to url (OSC 8), where colour is on and
// stdout is a terminal. Terminals that do not know the sequence ignore it and
// show the text, which is why the text is always the URL itself or a shortening
// of it.
func (o *Out) Link(url, text string) string {
	if !o.answerColour || url == "" || os.Getenv("TERM") == "linux" {
		return text
	}
	return ansi.SetHyperlink(url) + text + ansi.ResetHyperlink()
}

// Wrap folds s to the answer's width less indent, and indents every line after
// the first by it. Off a terminal it returns s: a log line is not reflowed.
func (o *Out) Wrap(s string, indent int) string {
	if !o.Rich() {
		return s
	}
	limit := o.Width() - indent
	if limit < 20 {
		return s
	}
	return strings.ReplaceAll(ansi.Wrap(s, limit, " "), "\n", "\n"+strings.Repeat(" ", indent))
}

// Faint dims s line by line. Dimmed on a multi-line string renders it as a
// block, padding every line to the widest — the ragged rectangle Dim exists to
// avoid — so anything that may hold a newline, a wrapped line included, comes
// through here.
func (o *Out) Faint(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = o.Dimmed(l)
	}
	return strings.Join(lines, "\n")
}

// Ago is a time as a person glances at it in a column: "5m ago", "3h ago",
// "2d ago". The long form for a sentence is the caller's.
func Ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2 Jan 2006")
}

// ShortID is an id at the length a person reads in a column: eight
// characters. A command that takes the id back prints it whole wherever it
// does not accept a prefix.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func panelStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Padding(0, 1)
}

package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

func testOut(rich bool, columns int) (*Out, *bytes.Buffer) {
	var buf bytes.Buffer
	o := New(rich, false, false)
	o.Stdout, o.Stderr, o.Columns = &buf, &buf, columns
	return o, &buf
}

// `toft apps > file` from a terminal wrote escapes into the file: the styles
// are decided from stderr, and the answer has to drop them on its own.
func TestAnswerStripsEscapesOffATerminal(t *testing.T) {
	o, buf := testOut(false, 0)
	o.Answer("%s  %s", "\x1b[1mdemo\x1b[0m", "\x1b[2muploaded\x1b[0m")
	if got := buf.String(); got != "demo  uploaded\n" {
		t.Fatalf("got %q", got)
	}
}

// A badge off a terminal is the word alone, so a caller can use it
// unconditionally; on one it gains its glyph, and an unknown word stays itself.
func TestBadge(t *testing.T) {
	plain, _ := testOut(false, 0)
	if got := plain.Badge("running"); got != "running" {
		t.Errorf("plain badge %q", got)
	}
	rich, _ := testOut(true, 0)
	if got := rich.Badge("running"); got != "● running" {
		t.Errorf("rich badge %q", got)
	}
	if got := rich.RunBadge("running"); got != "◐ running" {
		t.Errorf("run badge %q", got)
	}
	if got := rich.Badge("brand-new-state"); got != "brand-new-state" {
		t.Errorf("unknown word was bucketed: %q", got)
	}
}

// The table fits the terminal by truncating the flex column, under a header.
func TestTableFitsTheWidth(t *testing.T) {
	o, buf := testOut(true, 30)
	tb := o.NewTable("app", "source", "when").Flex(1)
	tb.Row("demo", "github.com/example/a-very-long-repository-name", "5m ago")
	tb.Print()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "APP") {
		t.Fatalf("want a header and a row:\n%s", buf.String())
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 30 {
			t.Errorf("%d columns, over 30: %q", n, l)
		}
	}
	if !strings.Contains(lines[1], "…") || !strings.HasSuffix(lines[1], "5m ago") {
		t.Errorf("want the source truncated and the last column whole: %q", lines[1])
	}
}

// Off a terminal a field is exactly the line detail views always wrote.
func TestFieldPlainIsUnchanged(t *testing.T) {
	o, buf := testOut(false, 0)
	o.Field(12, "status", "running")
	if got := buf.String(); got != "  status      running\n" {
		t.Fatalf("got %q", got)
	}
}

// Faint never pads: every line keeps its own length.
func TestFaintKeepsLineLengths(t *testing.T) {
	o, _ := testOut(true, 0)
	if got := o.Faint("a\nlonger line"); got != "a\nlonger line" {
		t.Fatalf("got %q", got)
	}
}

// The help's words are the same drawn or not: only escapes are added.
func TestStyleHelpKeepsTheWords(t *testing.T) {
	text := "toft status — what is serving\n\nFlags:\n  --app <slug>        Which app\n"
	got := StyleHelp(colorprofile.ANSI, text)
	if got == text {
		t.Fatal("nothing was drawn")
	}
	if ansi.Strip(got) != text {
		t.Fatalf("the words changed:\n%q", ansi.Strip(got))
	}
}

// The deploy panel is framed on a terminal and is the bare lines off one.
func TestPanel(t *testing.T) {
	o, buf := testOut(true, 0)
	o.Panel("✓ demo is live", "  https://apps.example/demo")
	if got := buf.String(); !strings.Contains(got, "╭") || !strings.Contains(got, "│ ✓ demo is live") {
		t.Errorf("want a frame:\n%s", got)
	}
	plain, pbuf := testOut(false, 0)
	plain.Panel("✓ demo is live")
	if got := pbuf.String(); got != "    ✓ demo is live\n" {
		t.Errorf("plain panel %q", got)
	}
}

// **Colour is off for each of its three signals, on a terminal that has it.**
// TTY_FORCE makes stderr a colour terminal to the profile detection, so the
// first case proves the escapes would be there and the rest that each signal
// removes them — bold and faint included, as v1's Ascii renderer did.
func TestEachColourSignalLeavesNoEscapes(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		colour    bool
		wantColor bool
	}{
		{"a colour terminal", nil, true, true},
		{"NO_COLOR", map[string]string{"NO_COLOR": "1"}, true, false},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, true, false},
		{"--no-color", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TTY_FORCE", "1")
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("NO_COLOR", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			o := New(true, tc.colour, false)
			var buf bytes.Buffer
			o.Stderr = &buf
			o.Warn("%s", o.Bold("careful")+" "+o.Dimmed("now"))
			got := buf.String()
			if has := ansi.Strip(got) != got; has != tc.wantColor {
				t.Errorf("escapes = %v, want %v: %q", has, tc.wantColor, got)
			}
		})
	}
}

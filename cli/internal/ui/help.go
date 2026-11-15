package ui

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/mattn/go-isatty"
)

// Help writes a usage text, drawn when w is a terminal: the headings bold, the
// commands and flags in the accent, so the eye runs down the left edge. The
// text is the same words either way — the help is half of what "usable"
// means, and it is written once.
//
// It cannot wait for --no-color: help is printed before the flags that would
// say so have been read, and a refused flag prints none. NO_COLOR and
// TERM=dumb are honoured.
func Help(w *os.File, text string) {
	if !isatty.IsTerminal(w.Fd()) || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		fmt.Fprint(w, text)
		return
	}
	fmt.Fprint(w, StyleHelp(colorprofile.Detect(w, os.Environ()), text))
}

var (
	helpHeading = regexp.MustCompile(`^[A-Z][A-Za-z -]*:$`)
	// An entry: two spaces, a term, at least two spaces, its description.
	helpEntry = regexp.MustCompile(`^(  )(\S(?:\S| \S)*)(\s{2,})(.*)$`)
)

// StyleHelp is Help's drawing at profile p, separate so it can be tested
// without a terminal.
func StyleHelp(p colorprofile.Profile, text string) string {
	bold := ink{lipgloss.NewStyle().Bold(true), p}
	accent := ink{lipgloss.NewStyle().Foreground(lipgloss.Color("6")), p}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case i == 0 && strings.Contains(line, " — "):
			name, rest, _ := strings.Cut(line, " — ")
			lines[i] = bold.Render(name) + " — " + rest
		case helpHeading.MatchString(line):
			lines[i] = bold.Render(line)
		default:
			if m := helpEntry.FindStringSubmatch(line); m != nil {
				lines[i] = m[1] + accent.Render(m[2]) + m[3] + m[4]
			}
		}
	}
	return strings.Join(lines, "\n")
}

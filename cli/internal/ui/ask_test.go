package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// **No is selected**: Enter alone answers no, as `[y/N]` always has. The
// other answers each by its own key, and Escape leaves the question unanswered.
func TestConfirmAnswers(t *testing.T) {
	cases := []struct {
		keys string
		yes  bool
		err  error
	}{
		{"\r", false, nil},
		{"y", true, nil},
		{"n", false, nil},
		{"\x1b[D\r", true, nil}, // left, enter: moved onto Yes
		{"\t\t\r", false, nil},  // there and back
		{"\x1b", false, ErrNotAnswered},
		{"", false, ErrNotAnswered}, // the input closed
	}
	for _, tc := range cases {
		o := liveOut()
		screen := &syncBuffer{}
		o.Stderr = screen
		yes, err := o.Confirm(context.Background(), "Roll demo back?", strings.NewReader(tc.keys), nil)
		if yes != tc.yes || !errors.Is(err, tc.err) {
			t.Errorf("keys %q: yes=%v err=%v, want yes=%v err=%v", tc.keys, yes, err, tc.yes, tc.err)
		}
		if !strings.Contains(ansi.Strip(screen.String()), "Roll demo back?") {
			t.Errorf("keys %q: the question was not shown:\n%q", tc.keys, screen.String())
		}
	}
}

// A typed parameter is the line typed, edited; empty is not an answer.
func TestParamTyped(t *testing.T) {
	o := liveOut()
	o.Stderr = &syncBuffer{}
	got, err := o.Param(context.Background(), "rows", "How many", "int", nil,
		strings.NewReader("\r12\x7f00\r"), nil)
	if err != nil || got != "100" {
		t.Errorf("got %q, %v; want 100", got, err)
	}
}

// The filter keeps labels holding the typed characters in order, case aside.
func TestPickerFilter(t *testing.T) {
	all := []Choice{{Label: "demo"}, {Label: "Drills-Flask"}, {Label: "reports"}}
	got := filterChoices(all, "dmo")
	if len(got) != 1 || got[0].Label != "demo" {
		t.Errorf("dmo: %v", got)
	}
	if got := filterChoices(all, "DF"); len(got) != 1 || got[0].Label != "Drills-Flask" {
		t.Errorf("DF: %v", got)
	}
	if got := filterChoices(all, ""); len(got) != 3 {
		t.Errorf("empty filter: %v", got)
	}
}

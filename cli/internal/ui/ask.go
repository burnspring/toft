package ui

// The questions a person at a terminal is asked — the confirm, a missing job
// parameter — as input programs. Each is built only when the caller's input(e, g)
// said a person is there; each ends with the answer written on its own line, so
// the question stays on screen beside what was chosen.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ErrNotAnswered is a question left without an answer: Escape, or Ctrl-C (which
// has also interrupted the run). The caller refuses as it would have for "no".
var ErrNotAnswered = errors.New("not answered")

// ask runs an input program to its end and returns the model it ended on.
func (o *Out) ask(ctx context.Context, m tea.Model, in io.Reader, interrupt func()) (tea.Model, error) {
	final, err := o.Input(ctx, m, in, interrupt).Run()
	if err != nil {
		return nil, err
	}
	return final, nil
}

// Confirm asks a yes/no question with **No selected**: Enter alone answers no,
// as `[y/N]` does. y and n answer at once; arrows and tab move between the two.
func (o *Out) Confirm(ctx context.Context, question string, in io.Reader, interrupt func()) (bool, error) {
	final, err := o.ask(ctx, confirmModel{o: o, question: question}, in, interrupt)
	if err != nil {
		return false, err
	}
	m, _ := final.(confirmModel)
	if !m.done {
		return false, ErrNotAnswered
	}
	return m.yes, nil
}

type confirmModel struct {
	o        *Out
	question string
	yes      bool
	done     bool
}

func (m confirmModel) Init() tea.Cmd { return nil }

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "y", "Y":
		m.yes, m.done = true, true
	case "n", "N":
		m.yes, m.done = false, true
	case "enter":
		m.done = true
	case "left", "right", "tab", "shift+tab", "h", "l":
		m.yes = !m.yes
		return m, nil
	case "esc":
		return m, tea.Quit
	default:
		return m, nil
	}
	return m, tea.Quit
}

func (m confirmModel) View() tea.View {
	q := fmt.Sprintf("%s %s ", m.o.Info("?"), m.o.Bold(m.question))
	if m.done {
		answer := "no"
		if m.yes {
			answer = "yes"
		}
		return tea.NewView(q + m.o.Dimmed(answer) + "\n")
	}
	yes, no := "  Yes  ", "[ No ]"
	if m.yes {
		yes, no = "[ Yes ]", "  No  "
	}
	return tea.NewView(q + m.o.Accent(yes) + " " + m.o.Dimmed(no) + "\n")
}

// Param asks for one parameter: a list to choose from when the declaration has
// choices, a line to type otherwise. help is the declaration's help, shown
// beside the name; kind is its type, shown when there are no choices.
func (o *Out) Param(ctx context.Context, name, help, kind string, choices []string, in io.Reader, interrupt func()) (string, error) {
	m := paramModel{o: o, name: name, help: help, kind: kind, choices: choices}
	final, err := o.ask(ctx, m, in, interrupt)
	if err != nil {
		return "", err
	}
	got, _ := final.(paramModel)
	if !got.done {
		return "", ErrNotAnswered
	}
	return got.answer, nil
}

type paramModel struct {
	o                *Out
	name, help, kind string
	choices          []string
	cursor           int
	field            []rune
	answer           string
	done             bool
}

func (m paramModel) Init() tea.Cmd { return nil }

func (m paramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if ok && k.String() == "esc" {
		return m, tea.Quit
	}
	if len(m.choices) > 0 {
		if !ok {
			return m, nil
		}
		switch k.String() {
		case "up", "k", "shift+tab":
			m.cursor = (m.cursor + len(m.choices) - 1) % len(m.choices)
		case "down", "j", "tab":
			m.cursor = (m.cursor + 1) % len(m.choices)
		case "enter":
			m.answer, m.done = m.choices[m.cursor], true
			return m, tea.Quit
		}
		return m, nil
	}
	// A line, edited at its end: typing and pasting add, backspace and ctrl+u
	// take away. Nothing finer — one value, typed once.
	switch msg := msg.(type) {
	case tea.PasteMsg:
		m.field = append(m.field, []rune(strings.TrimRight(msg.Content, "\r\n"))...)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			// Empty is not an answer to a required parameter; the line stays.
			if v := strings.TrimSpace(string(m.field)); v != "" {
				m.answer, m.done = v, true
				return m, tea.Quit
			}
		case "backspace":
			if len(m.field) > 0 {
				m.field = m.field[:len(m.field)-1]
			}
		case "ctrl+u":
			m.field = nil
		default:
			if msg.Text != "" {
				m.field = append(m.field, []rune(msg.Text)...)
			}
		}
	}
	return m, nil
}

func (m paramModel) View() tea.View {
	q := m.o.Info("?") + " " + m.o.Bold(m.name)
	if m.help != "" {
		q += m.o.Dimmed(" — " + m.help)
	}
	if m.done {
		return tea.NewView(q + "  " + m.answer + "\n")
	}
	if len(m.choices) == 0 {
		return tea.NewView(q + m.o.Dimmed(" ("+m.kind+")") + "  " + string(m.field) + m.o.Accent("▏") + "\n")
	}
	var b strings.Builder
	b.WriteString(q + "\n")
	for i, c := range m.choices {
		if i == m.cursor {
			b.WriteString("  " + m.o.Accent("› "+c) + "\n")
			continue
		}
		b.WriteString("    " + c + "\n")
	}
	return tea.NewView(b.String())
}

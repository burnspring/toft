package ui

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Choice is one row of a picker: what is chosen, and a word or two about it.
type Choice struct {
	Label  string
	Detail string
}

// pickerRows is how many rows a picker shows at once; the rest scroll.
const pickerRows = 10

// Pick asks a person to choose one of choices: type to filter, arrows to move,
// Enter to choose. Escape, Ctrl-C and a closed stdin choose nothing
// (ErrNotAnswered), and the caller refuses as it would have without asking.
func (o *Out) Pick(ctx context.Context, title string, choices []Choice, in io.Reader, interrupt func()) (Choice, error) {
	final, err := o.ask(ctx, pickerModel{o: o, title: title, all: choices, shown: choices}, in, interrupt)
	if err != nil {
		return Choice{}, err
	}
	m, _ := final.(pickerModel)
	if !m.done {
		return Choice{}, ErrNotAnswered
	}
	return m.chosen, nil
}

type pickerModel struct {
	o          *Out
	title      string
	all, shown []Choice
	filter     []rune
	cursor     int
	chosen     Choice
	done       bool
}

func (m pickerModel) Init() tea.Cmd { return nil }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		m.filter = append(m.filter, []rune(strings.TrimSpace(msg.Content))...)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			return m, tea.Quit
		case "enter":
			if len(m.shown) > 0 {
				m.chosen, m.done = m.shown[m.cursor], true
				return m, tea.Quit
			}
			return m, nil
		case "up", "shift+tab":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "tab":
			if m.cursor < len(m.shown)-1 {
				m.cursor++
			}
			return m, nil
		case "backspace":
			if len(m.filter) > 0 {
				m.filter = m.filter[:len(m.filter)-1]
			}
		case "ctrl+u":
			m.filter = nil
		default:
			if msg.Text == "" {
				return m, nil
			}
			m.filter = append(m.filter, []rune(msg.Text)...)
		}
	default:
		return m, nil
	}
	m.shown = filterChoices(m.all, string(m.filter))
	m.cursor = 0
	return m, nil
}

// filterChoices keeps the choices whose label holds every character of filter
// in order — `dmo` finds `demo` — case aside.
func filterChoices(all []Choice, filter string) []Choice {
	if filter == "" {
		return all
	}
	want := []rune(strings.ToLower(filter))
	var out []Choice
	for _, c := range all {
		i := 0
		for _, r := range strings.ToLower(c.Label) {
			if i < len(want) && r == want[i] {
				i++
			}
		}
		if i == len(want) {
			out = append(out, c)
		}
	}
	return out
}

func (m pickerModel) View() tea.View {
	head := m.o.Info("?") + " " + m.o.Bold(m.title)
	if m.done {
		return tea.NewView(head + "  " + m.chosen.Label + "\n")
	}
	var b strings.Builder
	b.WriteString(head + "  " + string(m.filter) + m.o.Accent("▏") + "\n")
	if len(m.shown) == 0 {
		b.WriteString("    " + m.o.Dimmed("nothing matches") + "\n")
		return tea.NewView(b.String())
	}
	// A window of rows around the cursor, so a long list scrolls rather than
	// pushing the question off the screen.
	start := 0
	if m.cursor >= pickerRows {
		start = m.cursor - pickerRows + 1
	}
	end := min(start+pickerRows, len(m.shown))
	width := 0
	for _, c := range m.shown[start:end] {
		width = max(width, len([]rune(c.Label)))
	}
	for i := start; i < end; i++ {
		c := m.shown[i]
		label := fmt.Sprintf("%-*s", width, c.Label)
		if i == m.cursor {
			b.WriteString("  " + m.o.Accent("› "+label) + "  " + c.Detail + "\n")
			continue
		}
		b.WriteString("    " + label + "  " + m.o.Dimmed(c.Detail) + "\n")
	}
	if more := len(m.shown) - end; more > 0 {
		b.WriteString("    " + m.o.Dimmed(fmt.Sprintf("+%d more", more)) + "\n")
	}
	return tea.NewView(b.String())
}

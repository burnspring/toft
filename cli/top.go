package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"toft/cli/internal/ui"
)

const topUsage = `toft top — the apps you can see, kept current

On a terminal, the apps and their state, read again every few seconds, with
the linked app's job runs beneath. q or Ctrl-C leaves the last frame on screen.
Piped, under --json or TOFT_PLAIN, it is toft apps: the same lines, the same
document.

Flags:
  --app <slug>        Whose job runs to show beneath (otherwise the linked app)
  --mine              Only apps that are yours, as toft apps --mine
  --json              The list as JSON, as toft apps gives it
`

// cmdTop is `toft apps`, live. **Off the live tier it is cmdApps** — the same
// lines and, under --json, the same document — so it adds a command and no
// new shape. Live, one GET /api/v1/apps a tick and, with an app linked or
// --app, that app's runs: one request's worth of estate, never the
// administrator-only estate-wide runs.
func cmdTop(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("top", topUsage)
	g.register(fs)
	mine := fs.Bool("mine", false, "only apps that are yours")
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	e.buildOut(&g)
	// An app named with --app must exist, live or not: every other --app is
	// not_found for a slug the install does not know, and top printing the
	// whole estate with exit 0 told a script its app was there (BB-094).
	if g.app != "" {
		s, err := e.setup(&g)
		if err != nil {
			return err
		}
		if _, err := s.resolveApp(g.app); err != nil {
			return err
		}
	}
	if !e.out.Live() {
		return cmdApps(e, args)
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	if *mine && s.IsDeployKey() {
		return errMineNeedsAPerson
	}
	m := &topModel{e: e, s: s, mine: *mine, every: orInterval(jobPollInterval)}
	// The app is the one named or linked, never picked: top is a glance, and
	// a question before it would be the wrong first frame.
	if v, err := s.app(); err == nil {
		if app, err := s.resolveApp(v.Value); err == nil {
			m.app = app
		}
	}
	var p *tea.Program
	if input(e, &g) {
		p = e.out.Input(e.ctx, m, e.input(), e.interrupt)
	} else {
		p = e.out.Display(e.ctx, m)
	}
	_, err = p.Run()
	if err != nil && e.ctx.Err() != nil {
		return nil // Ctrl-C is how top ends
	}
	return err
}

func orInterval(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return 2 * time.Second
}

// topModel is the estate as of the last read.
type topModel struct {
	e     *env
	s     *session
	app   *appSummary
	mine  bool
	every time.Duration

	apps []topApp
	runs []jobRun
	err  error
	at   time.Time
}

type topApp struct {
	appSummary
	Mine           bool       `json:"mine"`
	LastDeployedAt *time.Time `json:"last_deployed_at"`
}

type topRead struct {
	apps []topApp
	runs []jobRun
	err  error
}

type topTick struct{}

func (m *topModel) read() tea.Msg {
	var r topRead
	r.err = m.s.client.Do(http.MethodGet, "/api/v1/apps", nil, &r.apps)
	if m.mine {
		kept := r.apps[:0]
		for _, a := range r.apps {
			if a.Mine {
				kept = append(kept, a)
			}
		}
		r.apps = kept
	}
	sort.Slice(r.apps, func(i, j int) bool { return r.apps[i].Slug < r.apps[j].Slug })
	if r.err == nil && m.app != nil {
		var body struct {
			Runs []jobRun `json:"runs"`
		}
		path := "/api/v1/apps/" + m.app.ID + "/job-runs?limit=5"
		if err := m.s.client.Do(http.MethodGet, path, nil, &body); err == nil {
			r.runs = body.Runs
			sort.SliceStable(r.runs, func(i, j int) bool { return r.runs[i].QueuedAt.After(r.runs[j].QueuedAt) })
		}
	}
	return r
}

func (m *topModel) Init() tea.Cmd { return m.read }

func (m *topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case topRead:
		// A read that failed keeps the last good frame and says why beneath it.
		m.err = msg.err
		if msg.err == nil {
			m.apps, m.runs, m.at = msg.apps, msg.runs, time.Now()
		}
		return m, tea.Tick(m.every, func(time.Time) tea.Msg { return topTick{} })
	case topTick:
		return m, m.read
	case tea.KeyPressMsg:
		if msg.String() == "q" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *topModel) View() tea.View {
	o := m.e.out
	var lines []string
	head := o.Bold("toft top") + o.Dimmed(fmt.Sprintf(" · %s · every %s", m.s.InstallURL, ui.FormatDuration(m.every)))
	lines = append(lines, head)
	if m.at.IsZero() && m.err == nil {
		lines = append(lines, o.Dimmed("  reading…"))
		return tea.NewView(strings.Join(lines, "\n") + "\n")
	}

	t := o.NewTable("app", "status", "kind", "deployed").Flex(2)
	for _, a := range m.apps {
		deployed := o.Dimmed("never")
		if a.LastDeployedAt != nil {
			deployed = ui.Ago(*a.LastDeployedAt)
		}
		t.Row(o.Bold(a.Slug), o.Badge(a.Status), o.Dimmed(orDefault(a.Framework, "—")), deployed)
	}
	table := t.Lines()
	if len(m.apps) == 0 {
		table = []string{o.Dimmed("  no apps you can see")}
		if m.mine {
			table = []string{o.Dimmed("  no apps of yours")}
		}
	}

	var runs []string
	if m.app != nil {
		runs = append(runs, "", o.Bold(m.app.Slug)+o.Dimmed(" · job runs"))
		if len(m.runs) == 0 {
			runs = append(runs, o.Dimmed("  none yet"))
		}
		for _, r := range m.runs {
			runs = append(runs, fmt.Sprintf("  %s  %-14s %s  %s", shortRunID(r.ID), r.Job,
				o.Badge(r.Status), o.Dimmed(ui.Ago(r.QueuedAt))))
		}
	}
	var foot []string
	if m.err != nil {
		foot = append(foot, o.Caution("! "+m.err.Error()))
	}

	// Cut to the terminal: the header, the runs and the footer keep their
	// lines, and the app table gives up its last rows with a count of them.
	room := o.Height() - len(lines) - len(runs) - len(foot) - 1
	if room < 2 {
		room = 2
	}
	if len(table) > room {
		more := len(table) - (room - 1)
		table = append(table[:room-1], o.Dimmed(fmt.Sprintf("  +%d more", more)))
	}
	lines = append(lines, table...)
	lines = append(lines, runs...)
	lines = append(lines, foot...)
	return tea.NewView(strings.Join(lines, "\n") + "\n")
}

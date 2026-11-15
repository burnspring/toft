package ui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Region is a few lines kept current at the bottom of a live run — the job
// wait's status line, the footer under `toft logs -f` — with whatever the
// command prints going above it. Display only: it reads no keys, so Ctrl-C
// stays the signal it is everywhere else.
//
// The view is a function rather than a value so that a clock in it moves
// between polls: it is asked again on every tick.
type Region struct {
	program *tea.Program
	above   *aboveTree
	ran     chan struct{}
	stop    sync.Once
	// clear, when set, empties the view before the last draw — Waiting's.
	clear func()
}

// regionTick is how often the view is asked again: often enough for an
// elapsed time in whole seconds to read as moving.
const regionTick = time.Second

// NewRegion starts a region drawing view, or returns nil off a live run — the
// caller's plain path is the `nil` branch, and every method is safe on nil.
func (o *Out) NewRegion(ctx context.Context, view func() string) *Region {
	p := o.Display(ctx, regionModel{view})
	if p == nil {
		return nil
	}
	ran := make(chan struct{})
	r := &Region{program: p, above: &aboveTree{program: p, done: ran, stderr: o.Stderr}, ran: ran}
	go func() {
		defer close(r.ran)
		p.Run()
	}()
	return r
}

// Waiting is a region of one dimmed line — what is being waited for, and a
// clock — for a wait with nothing else to show: the browser's approval, an app
// coming up, a path answering.
//
// **It leaves nothing behind.** What the wait ended in is the next thing said,
// and a "waiting · 14s" above it is the same news twice. nil off a live run,
// where the caller says its own line, or none.
func (o *Out) Waiting(ctx context.Context, what string) *Region {
	start := time.Now()
	var over atomic.Bool
	r := o.NewRegion(ctx, func() string {
		if over.Load() {
			return ""
		}
		return o.Dimmed(what + " · " + clock(time.Since(start)))
	})
	if r != nil {
		r.clear = func() { over.Store(true) }
	}
	return r
}

// clock is an elapsed wait in whole seconds, which is what a one-second tick
// can show moving: 9s, 1m05s.
func clock(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Writer prints whole lines above the region; nil off a live run.
func (r *Region) Writer() io.Writer {
	if r == nil {
		return nil
	}
	return r.above
}

// Refresh asks for the view now, rather than at the next tick — after a poll
// changed what it shows.
func (r *Region) Refresh() {
	if r != nil {
		r.program.Send(regionRefresh{})
	}
}

// Stop draws the view a last time and leaves it on screen, the cursor under it
// — or, for Waiting, takes it away.
func (r *Region) Stop() {
	if r == nil {
		return
	}
	r.stop.Do(func() {
		r.above.detach()
		if r.clear != nil {
			r.clear()
		}
		r.program.Send(regionDone{})
		<-r.ran
	})
}

type regionModel struct{ view func() string }

type (
	regionTickMsg struct{}
	regionRefresh struct{}
	regionDone    struct{}
)

func nextRegionTick() tea.Cmd {
	return tea.Tick(regionTick, func(time.Time) tea.Msg { return regionTickMsg{} })
}

func (m regionModel) Init() tea.Cmd { return nextRegionTick() }

func (m regionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case regionTickMsg:
		return m, nextRegionTick()
	case regionDone:
		return m, tea.Quit
	}
	return m, nil
}

func (m regionModel) View() tea.View {
	v := strings.TrimSuffix(m.view(), "\n")
	if v == "" {
		return tea.NewView("")
	}
	return tea.NewView(v + "\n")
}

package ui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"toft/internal/deploysteps"
)

// The deploy, as a terminal draws it.
//
//	  ✓ upload   receive · unpack                              1.2s
//	  ✓ scan     setup · fetch · scan · detect                 8.4s
//	  ⠸ deploy   configure ✓ · build ⠸ 11s · publish · start · ready · flip
//
// Three phases, their steps beneath them, a spinner and running elapsed on the
// step in flight, `✓` on what is done, and everything not yet reached dimmed.
// The default is this and not the build log: the log is hundreds of lines of
// npm output and the wrong default for a command whose answer is usually "it
// worked, in 14 seconds". A failed deploy prints the tail of the log without
// being asked, because at that point it is the only thing that matters.
//
// **An unknown phase or step renders as its key.** The step vocabulary is
// designed so that adding one is safe, and a laptop's `toft` is older than the
// install it talks to more often than not — so the client must not be the thing
// that makes a safe change unsafe. A step this binary has never heard of is
// drawn with the name the server used.

// Timeline accumulates frames and draws them.
//
// It is written to from the frame loop and read by the program's view, so the
// state is behind a mutex — the alternative is a channel and a select in a
// display, which is more machinery than a dozen strings deserve.
type Timeline struct {
	out *Out

	mu       sync.Mutex
	order    []string // steps in the order they were first seen
	entered  map[string]time.Time
	finished map[string]time.Duration
	// inferred is the steps passedOver has already ticked, so a later frame
	// does not tick them twice.
	inferred map[string]bool
	current  string
	phase    string
	status   string
	failed   bool
	// ended is the terminal frame having arrived, which is the only thing that
	// makes the *last* step done — the frames say where a build is, never that
	// something is over.
	ended   bool
	started time.Time

	tick int
	// live is the tree drawn by a program rather than one line per step:
	// Out.Live, decided once. `--logs` no longer switches it off — the log is
	// printed above the tree by the program (Writer), so the two share the
	// terminal instead of repainting over each other.
	live    bool
	program *tea.Program
	above   *aboveTree
	ran     chan struct{}
}

func NewTimeline(out *Out) *Timeline {
	return &Timeline{
		out:      out,
		live:     out.Live(),
		entered:  map[string]time.Time{},
		finished: map[string]time.Duration{},
		inferred: map[string]bool{},
		started:  time.Now(),
	}
}

// Start begins drawing, on a live run. Off one it does nothing: the plain
// rendering writes one line per completed step from Frame, and has no clock of
// its own.
func (t *Timeline) Start() {
	if !t.live {
		return
	}
	t.program = t.out.Display(context.Background(), treeModel{t})
	if t.program == nil {
		t.live = false
		return
	}
	t.ran = make(chan struct{})
	t.above = &aboveTree{program: t.program, done: t.ran, stderr: t.out.Stderr}
	go func() {
		defer close(t.ran)
		t.program.Run()
	}()
}

// Stop ends the program and leaves the finished tree on screen: the last
// frame is drawn from the final state, then the program quits inline. A log
// line still half-written is printed above it first; anything written after
// goes to stderr, under the tree.
func (t *Timeline) Stop() {
	if t.program == nil {
		return
	}
	t.above.detach()
	t.program.Send(treeDone{})
	<-t.ran
	t.program = nil
}

// Writer is where a build log goes while the tree is drawn: each complete line
// printed above it by the program, and off a live run stderr itself.
func (t *Timeline) Writer() io.Writer {
	if t.program == nil {
		return t.out.Stderr
	}
	return t.above
}

// aboveTree turns writes into whole lines for Program.Println, which prints a
// line above the managed region. A write that ends mid-line keeps the rest
// until the next one completes it — a log fetched once a second is cut
// wherever the second fell. Written from the log follower's goroutine, and
// detached by Stop from the command's, so it has a lock of its own.
//
// **Never Program.Println.** It is a bare send on the program's unbuffered
// channel with no exit path, so a line written after the program's context
// ended — the first Ctrl-C, between a poll's answer and its lines — parked
// the writer for ever while holding mu, and Stop then blocked on mu too
// (security review 2026-11-14, CS-9). println goes through Program.Send,
// which returns once the program is done, and once done (the owner's ran
// channel closed) lines go to stderr as they do after detach.
type aboveTree struct {
	mu      sync.Mutex
	program *tea.Program
	// done is closed when the program's Run has returned.
	done    <-chan struct{}
	stderr  io.Writer
	partial string
}

// ended reports whether the program can no longer print: detached, or its
// event loop over.
func (w *aboveTree) ended() bool {
	if w.program == nil {
		return true
	}
	if w.done == nil {
		return false
	}
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// println prints one line above the region through the guarded path.
func (w *aboveTree) println(text string) {
	w.program.Send(tea.Println(text)())
}

func (w *aboveTree) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended() {
		fmt.Fprint(w.stderr, w.partial+string(b))
		w.partial = ""
		return len(b), nil
	}
	text := w.partial + string(b)
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		w.partial = text
		return len(b), nil
	}
	w.partial = text[i+1:]
	w.println(text[:i])
	return len(b), nil
}

// detach prints what is still held and sends everything after to stderr.
func (w *aboveTree) detach() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.partial != "" {
		if w.ended() {
			fmt.Fprintln(w.stderr, w.partial)
		} else {
			w.println(w.partial)
		}
		w.partial = ""
	}
	w.program = nil
}

// treeModel is the tree as a program sees it: a tick that moves the spinner and
// the running clocks, and a view that is render().
type treeModel struct{ t *Timeline }

type (
	treeTick struct{}
	treeDone struct{}
)

func nextTreeTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return treeTick{} })
}

func (m treeModel) Init() tea.Cmd { return nextTreeTick() }

func (m treeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case treeTick:
		m.t.mu.Lock()
		m.t.tick++
		m.t.mu.Unlock()
		return m, nextTreeTick()
	case treeDone:
		return m, tea.Quit
	}
	return m, nil
}

func (m treeModel) View() tea.View {
	m.t.mu.Lock()
	defer m.t.mu.Unlock()
	// The trailing newline is kept: it leaves the cursor under the finished
	// tree, so whatever is written next does not overwrite its last line.
	return tea.NewView(m.t.render())
}

// Seed records steps that finished before this client was watching.
//
// It exists because the first two steps of an uploaded deploy — receiving the
// bytes and unpacking them — happen inside the HTTP request that starts it,
// which is over before there is anything to subscribe to. Without this the tree
// would silently omit the phase that, on a slow connection, took the longest;
// with it, a deploy rejoined half an hour later also draws what it missed.
//
// The timings come off the deployment row, which is the server's record rather
// than this client's guess.
func (t *Timeline) Seed(timings []StepTiming) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range timings {
		if s.Step == "" {
			continue
		}
		if _, seen := t.entered[s.Step]; !seen {
			t.order = append(t.order, s.Step)
		}
		t.entered[s.Step] = time.Now()
		t.finished[s.Step] = time.Duration(s.Millis) * time.Millisecond
	}
}

// StepTiming is one finished step, as the deployment row records it.
type StepTiming struct {
	Step   string `json:"step"`
	Millis int64  `json:"ms"`
}

// Frame records one progress frame.
func (t *Timeline) Frame(status, phase, step string) {
	t.mu.Lock()
	t.status, t.phase = status, phase

	// A step is finished when the deploy moves off it, or when the deploy ends.
	// Nothing else says so — the frames report where a build *is*, and "done"
	// is the client's own inference from having been told about somewhere else.
	var completed []string
	if step != "" && step != t.current {
		if t.current != "" {
			t.finished[t.current] = time.Since(t.entered[t.current])
			completed = append(completed, t.current)
		}
		completed = append(completed, t.passedOver(step)...)
		if _, seen := t.entered[step]; !seen {
			t.order = append(t.order, step)
		}
		t.entered[step] = time.Now()
		t.current = step
	}
	if status == "failed" {
		t.failed = true
	}
	if status == "failed" || status == "success" {
		t.ended = true
	}
	if t.ended && t.current != "" {
		if _, done := t.finished[t.current]; !done {
			t.finished[t.current] = time.Since(t.entered[t.current])
		}
		completed = append(completed, t.current)
	}

	var lines []string
	for _, done := range completed {
		lines = append(lines, t.plainLine(done, status))
	}
	t.mu.Unlock()

	// Off a terminal: one line per *completed* step, with its phase and how
	// long that step itself took, and nothing that repaints. This is the whole
	// non-interactive rendering, and it is what a CI log shows.
	if !t.live && !t.out.Quiet {
		for _, line := range lines {
			if line != "" {
				fmt.Fprintln(t.out.Stderr, line)
			}
		}
	}
}

// passedOver is the steps a frame jumped: behind the new one in Order, after
// the one in flight, and never named by a frame. The frames are polled, so a
// step that starts and ends inside one interval — setup and fetch take
// milliseconds — is never seen, and the line-per-step rendering left it out of
// the deploy's account altogether. They are done by the same inference isDone
// draws them with, and so excluded the same way: a conditional step that no
// frame named may never have applied. Called with the lock held.
func (t *Timeline) passedOver(step string) []string {
	next := deploysteps.Index(step)
	if next < 0 {
		return nil
	}
	var out []string
	for i := deploysteps.Index(t.current) + 1; i < next; i++ {
		s := string(deploysteps.Order[i])
		if deploysteps.Conditional(s) || t.inferred[s] {
			continue
		}
		if _, seen := t.entered[s]; seen {
			continue
		}
		t.inferred[s] = true
		out = append(out, s)
	}
	return out
}

// plainLine is the non-interactive form: one finished step. Called with the
// lock held.
func (t *Timeline) plainLine(step, status string) string {
	if step == "" {
		return ""
	}
	mark := "✓"
	if status == "failed" && step == t.current {
		mark = "✗"
	}
	phase := string(deploysteps.PhaseOf(step))
	if phase == "" {
		phase = "?"
	}
	// A step done by inference has no duration, and nothing invents one.
	d, timed := t.finished[step]
	if !timed {
		return strings.TrimRight(fmt.Sprintf("%s %-7s %s", mark, phase, step), " ")
	}
	return fmt.Sprintf("%s %-7s %-10s %s", mark, phase, step, FormatDuration(d))
}

// render draws the three phases and their steps. Called with the lock held.
func (t *Timeline) render() string {
	var b strings.Builder
	for _, phase := range t.phases() {
		b.WriteString(t.renderPhase(phase))
	}
	return b.String()
}

// phases is the three known phases, plus any phase the server named that this
// binary has never heard of — appended in the order they arrived, so a newer
// install's new phase appears rather than vanishing.
func (t *Timeline) phases() []string {
	known := map[string]bool{}
	var out []string
	for _, p := range deploysteps.Phases() {
		known[string(p)] = true
		out = append(out, string(p))
	}
	var extra []string
	for _, step := range t.order {
		p := string(deploysteps.PhaseOf(step))
		if p == "" {
			p = "?"
		}
		if !known[p] {
			known[p] = true
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func (t *Timeline) renderPhase(phase string) string {
	steps := t.stepsIn(phase)
	if len(steps) == 0 {
		return ""
	}

	mark, done := t.phaseMark(phase, steps)
	var parts []string
	for _, step := range steps {
		parts = append(parts, t.renderStep(step))
	}

	// The phase name carries the phase's state too: bold once it has begun,
	// dimmed while it is still ahead.
	name := t.out.Dimmed(fmt.Sprintf("%-8s", phase))
	for _, s := range steps {
		if _, begun := t.entered[s]; begun || t.isDone(s) {
			name = t.out.Bold(fmt.Sprintf("%-8s", phase))
			break
		}
	}
	line := fmt.Sprintf("  %s %s %s", mark, name, strings.Join(parts, t.out.Dimmed(" · ")))
	if done > 0 {
		line += "  " + t.out.Dimmed(FormatDuration(done))
	}
	return line + "\n"
}

// stepsIn is the phase's steps in Order, plus any step the server named that
// this binary does not place — appended so an unknown step is drawn rather than
// dropped.
func (t *Timeline) stepsIn(phase string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range deploysteps.StepsIn(deploysteps.Phase(phase)) {
		// A conditional step that never ran is not drawn at all: a git deploy
		// has no bytes to receive and nothing to unpack, and showing those as
		// skipped would be a small lie told on every timeline.
		if deploysteps.Conditional(string(s)) {
			if _, ran := t.entered[string(s)]; !ran {
				continue
			}
		}
		out = append(out, string(s))
		seen[string(s)] = true
	}
	for _, step := range t.order {
		p := string(deploysteps.PhaseOf(step))
		if p == "" {
			p = "?"
		}
		if p == phase && !seen[step] {
			out = append(out, step)
			seen[step] = true
		}
	}
	return out
}

// phaseMark is the phase's own state, derived from its steps rather than
// stored: a phase is done when every step in it has finished.
//
// "Finished" is the client's own inference — the frames say where a build *is*,
// never that something is over — and the two ways to learn it are the deploy
// moving on, and the deploy ending. The second is why this reads `finished`
// rather than testing against the current step: on the terminal frame the
// current step is over too, and a tree that went on spinning after "✓ deployed"
// would be the last thing anybody saw.
func (t *Timeline) phaseMark(phase string, steps []string) (string, time.Duration) {
	var total time.Duration
	allDone, allTimed := true, true
	anyStarted := false
	for _, s := range steps {
		if d, ok := t.finished[s]; ok {
			total += d
			anyStarted = true
		} else {
			allTimed = false
		}
		if !t.isDone(s) {
			allDone = false
		}
		if _, started := t.entered[s]; started {
			anyStarted = true
		}
	}
	// No duration unless every step in the phase was actually measured. A
	// number assembled from the two steps this client happened to watch would
	// be confidently wrong, and a phase with no number beside it is merely
	// quiet.
	if !allTimed {
		total = 0
	}
	switch {
	case t.failed && t.stepIn(steps, t.current):
		return t.out.Fail("✗"), total
	case !anyStarted:
		return t.out.Dimmed("·"), 0
	case allDone:
		return t.out.OK("✓"), total
	}
	return t.out.Accent(spinnerFrame(t.tick)), 0
}

// isDone answers "has this step happened", from the three things that can say
// so — and only those three.
//
// A recorded duration is the strongest: either the server's own timing, seeded
// from the deployment row, or this client watching the build move off the step.
// Behind the current step in Order is the second, and it is what covers the
// steps a client never saw because it attached late — a fast build is past
// `setup` before the upload's response has been parsed. **Conditional steps are
// excluded from that inference by name**, because a later step does not imply
// they were skipped over; it may mean they never applied, and drawing `receive`
// as done on a git deploy would be a small lie told on every timeline. The
// third is the deploy having ended successfully, which finishes the last step.
//
// A step that is done by inference has no duration, and nothing invents one.
func (t *Timeline) isDone(step string) bool {
	if _, ok := t.finished[step]; ok {
		return true
	}
	index := deploysteps.Index(step)
	if index < 0 || deploysteps.Conditional(step) {
		return false
	}
	if t.ended && !t.failed {
		return true
	}
	current := deploysteps.Index(t.current)
	return current >= 0 && index < current
}

func (t *Timeline) stepIn(steps []string, step string) bool {
	for _, s := range steps {
		if s == step {
			return true
		}
	}
	return false
}

// renderStep is one step: dimmed if not reached, a spinner and a running clock
// if in flight, a tick if done.
func (t *Timeline) renderStep(step string) string {
	if step == t.current && t.failed {
		return t.out.Fail(step + " ✗")
	}
	if t.isDone(step) {
		return step + " " + t.out.OK("✓")
	}
	if step == t.current {
		elapsed := time.Since(t.entered[step])
		clock := ""
		// A running clock only once the step has been going long enough to be
		// worth watching. A `configure` that takes 200ms does not need a timer.
		if elapsed > 2*time.Second {
			clock = " " + FormatDuration(elapsed)
		}
		return fmt.Sprintf("%s %s%s", step, t.out.Accent(spinnerFrame(t.tick)), clock)
	}
	if _, started := t.entered[step]; started {
		return step
	}
	return t.out.Dimmed(step)
}

// Elapsed is the whole deploy, for the final block.
func (t *Timeline) Elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Since(t.started)
}

// Where reports the phase and step the deploy was in when it stopped — which is
// what turns "the deploy failed" into two different next actions.
func (t *Timeline) Where() (phase, step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	phase = t.phase
	if phase == "" && t.current != "" {
		phase = string(deploysteps.PhaseOf(t.current))
	}
	return phase, t.current
}

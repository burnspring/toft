package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"toft/cli/internal/ui"
)

const logsUsage = `toft logs — the app's runtime log

Flags:
  --app <slug>        Which app
  --follow, -f        Keep printing as new lines arrive
  --lines <n>         How much history to start with (default 100); with
                      --deploy, the last n lines of the build log (default all)
  --source <which>    Only one voice: "app" or "platform"
  --since <t>         Only lines since then: RFC3339, or a duration like 2h
  --deploy <id>       One build's log instead of the app's runtime log
  --json              One JSON object per line: the stamp, the voice and the text

An app's own colour codes reach a terminal as it wrote them — its other escape
sequences (titles, links, the clipboard, cursor moves) never do — and are removed
when stdout is not one (a pipe, a file) or with --no-color.
`

func cmdLogs(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("logs", logsUsage)
	g.register(fs)
	follow := fs.Bool("follow", false, "keep printing as new lines arrive")
	fs.BoolVar(follow, "f", false, "keep printing as new lines arrive")
	lines := fs.Int("lines", 100, "how much history to start with")
	deployID := fs.String("deploy", "", "one build's log")
	source := fs.String("source", "", `only one voice: "app" or "platform"`)
	since := fs.String("since", "", "only lines since then")
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	// Zero and below used to be accepted and sent; the install's answer to
	// `lines=-5` is not a thing anybody asked for.
	if *lines <= 0 {
		return usageCodef("invalid_lines", "--lines takes a count of 1 or more, not %d", *lines)
	}
	linesSet := flagsSet(fs)["lines"]

	sinceTime, err := parseSince(*since)
	if err != nil {
		return err
	}

	switch *source {
	case "", sourceApp, sourcePlatform:
	case "job", "jobs":
		// A job run is a workload of its own and its output is the run's, not
		// the app's — the one wrong guess worth a sentence of its own.
		return usageCodef("invalid_source", "a job run's output is not in the app's log: "+
			"`toft job runs` lists the runs, and `toft job logs <runID>` prints one")
	default:
		return usageCodef("invalid_source", "--source takes %q or %q, not %q", sourceApp, sourcePlatform, *source)
	}
	// A build log is the builder's output and has no app half to separate from.
	// Accepting the flag here would imply a distinction that does not exist.
	if *source != "" && *deployID != "" {
		return usageCodef("conflicting_flags", "--source and --deploy cannot be combined: a build log has one voice")
	}
	// Refused beside --source's, and for the same shape of reason: a build log
	// is one contiguous thing with a beginning, and asking for the part of it
	// after a wall-clock time is a question about a stream that this is not.
	if *since != "" && *deployID != "" {
		return usageCodef("conflicting_flags", "--since and --deploy cannot be combined: a build log is one contiguous thing")
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	e.plainLogs = g.noColor || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" ||
		!writesToTerminal(e.out.Stdout)
	appValue, err := pickApp(e, s)
	if err != nil {
		return err
	}
	app, err := s.resolveApp(appValue.Value)
	if err != nil {
		return err
	}

	// A build's log is a finished thing once the build is over, so there is
	// nothing to follow: it is fetched whole and printed.
	if *deployID != "" {
		if *deployID, err = resolveDeploymentID(s, app.ID, *deployID); err != nil {
			return err
		}
		text, err := s.buildLog(app.ID, *deployID)
		if err != nil {
			return noSuchName(err, "toft deployments list")
		}
		// --lines is honoured here too, as the tail it is everywhere else.
		// Unset, the whole log: a build log is one document with a beginning,
		// and the default of 100 is the runtime log's.
		if linesSet && text != "" {
			text = tailOf(text, *lines) + "\n"
		}
		if g.json {
			// One object, not one per line. A build log is a single document
			// the builder wrote — it has no per-line stamp and no second voice
			// to separate, so splitting it into frames would invent a structure
			// the platform never recorded.
			return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
				"deployment_id": *deployID,
				"app":           app.Slug,
				"log":           text,
			})
		}
		fmt.Fprint(e.out.Stdout, e.logText(text))
		return nil
	}

	printed, err := s.printRuntimeLog(e, app, *lines, *source, sinceTime, nil)
	if err != nil {
		return err
	}
	if !*follow {
		return nil
	}

	// Polling rather than the WebSocket the browser uses.
	//
	// The stream would be live rather than nearly-live, and it would put a
	// WebSocket library in a binary whose entire dependency budget is one
	// terminal styling package. A one-second poll of a route that already
	// exists is the honest trade for a log tail, and `--follow` on a log is not
	// a latency-critical surface.
	footer := newLogFooter(e, s, app, *source)
	defer footer.stop(e)
	for {
		select {
		case <-e.ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		printed, err = s.printRuntimeLog(e, app, *lines, *source, sinceTime, printed)
		if err != nil {
			return err
		}
		footer.polled()
	}
}

// logFooter is the one line under a live `toft logs -f`: the app's state, the
// voice being followed, and how fast lines are arriving. Display only, so
// Ctrl-C ends the follow as it always has. Nil off a live run, where nothing
// about the follow changes.
type logFooter struct {
	region *ui.Region
	s      *session
	app    *appSummary
	source string

	mu    sync.Mutex
	state string
	rate  float64
	count atomic.Int64
	since time.Time
	polls int
}

// footerStateEvery is how many polls pass between reads of the app's state:
// the log is the subject, and a status read a second would double the
// requests for a word that rarely changes.
const footerStateEvery = 10

func newLogFooter(e *env, s *session, app *appSummary, source string) *logFooter {
	f := &logFooter{s: s, app: app, source: source, state: app.Status, since: time.Now()}
	f.region = e.out.NewRegion(e.ctx, func() string { return f.view(e) })
	if f.region == nil {
		return nil
	}
	e.logSink = lineCounter{w: f.region.Writer(), n: &f.count}
	return f
}

func (f *logFooter) view(e *env) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	voice := "both voices"
	if f.source != "" {
		voice = f.source + " only"
	}
	return fmt.Sprintf("%s %s  %s", e.out.Bold(f.app.Slug), e.out.Badge(orDefault(f.state, "unknown")),
		e.out.Dimmed(fmt.Sprintf("· %s · %.1f lines/s · following", voice, f.rate)))
}

// polled is one poll done: the rate over it, and every footerStateEvery polls
// the app's state.
func (f *logFooter) polled() {
	if f == nil {
		return
	}
	n := f.count.Swap(0)
	now := time.Now()
	f.mu.Lock()
	f.rate = float64(n) / now.Sub(f.since).Seconds()
	f.since = now
	f.polls++
	read := f.polls%footerStateEvery == 0
	f.mu.Unlock()
	if read {
		if got, err := f.s.resolveApp(f.app.Slug); err == nil {
			f.mu.Lock()
			f.state = got.Status
			f.mu.Unlock()
		}
	}
	f.region.Refresh()
}

func (f *logFooter) stop(e *env) {
	if f == nil {
		return
	}
	f.region.Stop()
	e.logSink = nil
}

// lineCounter counts the lines written through it.
type lineCounter struct {
	w io.Writer
	n *atomic.Int64
}

func (c lineCounter) Write(b []byte) (int, error) {
	c.n.Add(int64(bytes.Count(b, []byte("\n"))))
	return c.w.Write(b)
}

// The two voices a runtime log carries. Named here rather than imported: the
// CLI is a second module and does not depend on the platform's packages.
const (
	sourceApp      = "app"
	sourcePlatform = "platform"
)

// logLine is one labeled line of the tail.
//
// The flat `logs` blob beside it is what an older `toft` reads and what this one
// falls back to, so a new CLI against an old platform still prints something.
type logLine struct {
	At     string `json:"at"`
	Source string `json:"source"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
	// Marker names a boundary the platform drew rather than a line anything
	// printed — see runtime.LogLine. Carried through so a caller reading this
	// stream can find the workload boundary without matching its prose; empty
	// against an install that predates it, which is the same as every other
	// line.
	Marker string `json:"marker,omitempty"`
	// Raw is the line as the platform recorded it, present only when that
	// held terminal escapes — `line` is then the text alone (BB-016, third
	// 2026-11-03 list): a program reading `line` greps text, as a pipe does.
	Raw string `json:"raw,omitempty"`
}

// jsonLogText is a log line's text for a --json document, and the raw line
// when stripping changed it.
func jsonLogText(s string) (text, raw string) {
	if !strings.Contains(s, "\x1b") {
		return s, ""
	}
	return ansiEscape.ReplaceAllString(s, ""), s
}

// printRuntimeLog fetches the tail and prints whatever is new since the last
// poll, returning what has now been printed.
//
// Two shapes, because the platform answers both. When it sends labeled lines
// the diff is exact — a line is the unit and each carries a stamp — and when it
// does not, the old overlap match over the blob is what is left.
//
// The overlap match is guesswork on a stream that can repeat itself: it is
// exact while the log has not moved by more than N lines between polls, and
// wrong in a way it cannot detect when two identical lines bracket the seam.
// **When the overlap is lost it says so** rather than reprinting the tail,
// because a silent gap is worse than a stated one — and the warning goes to
// stderr, where it does not land in a redirected log.
func (s *session) printRuntimeLog(e *env, app *appSummary, lines int, source string, since time.Time, seen []logLine) ([]logLine, error) {
	var body struct {
		Logs  string    `json:"logs"`
		Lines []logLine `json:"lines"`
	}
	// The source goes to the install too, which chooses the voice before it
	// cuts the tail; the filter below stays for an install older than that,
	// where it is the only filter there is.
	path := fmt.Sprintf("/api/v1/apps/%s/logs?lines=%d", app.ID, lines)
	if source != "" {
		path += "&source=" + url.QueryEscape(source)
	}
	if err := s.client.Do(http.MethodGet, path, nil, &body); err != nil {
		return seen, err
	}

	if body.Lines == nil {
		// An older platform, which has no lines to filter and no labels to
		// mark. Asking for one voice cannot be honoured, and quietly printing
		// both would be the wrong answer to a question that was asked.
		if source != "" {
			return seen, fmt.Errorf("this Toft does not label log lines, so --source cannot be applied")
		}
		// Same reason, same honesty: with no lines there are no stamps, so
		// there is nothing to filter by. Saying the install is too old beats
		// filtering nothing and reporting it as a filter that matched
		// everything.
		if !since.IsZero() {
			return seen, usageCodef("unsupported_by_install", "this Toft does not stamp log lines, so --since cannot be applied")
		}
		// Same honesty a third time. Every field of a log line except the text
		// would be invented here, and an object full of empty stamps is worse
		// than a refusal that names the reason.
		if e.json {
			return seen, usageCodef("unsupported_by_install", "this Toft does not label or stamp log lines, "+
				"so --json has nothing to put in an object — drop it to read the log as text")
		}
		s.printBlob(e, body.Logs, lines, seen)
		return []logLine{{Line: body.Logs}}, nil
	}

	fresh := body.Lines
	if source != "" {
		fresh = filterSource(fresh, source)
	}
	if !since.IsZero() {
		fresh = filterSince(fresh, since)
	}
	// A gap is said, not swallowed: when nothing of the last page survives
	// into this one and the page was full, lines came and went between two
	// polls (code review 2026-11-11, M-5).
	if len(seen) > 0 && len(seen) >= lines && len(fresh) > 0 && len(newSince(fresh, seen)) == len(fresh) {
		e.out.Warn("the app logged faster than this poll could follow — some lines between two polls were not shown; pass a larger --lines")
	}
	encoder := json.NewEncoder(e.out.Stdout)
	for _, l := range newSince(fresh, seen) {
		// **NDJSON, not an array**, for the same reason the deploy's frames
		// are: `--follow` has no end, so a document that has to be closed
		// before it can be parsed is one a follower could never emit. One
		// object per line reads the same whether the command ran for a second
		// or is still running.
		if e.json {
			out := l
			out.Line, out.Raw = jsonLogText(l.Line)
			encoder.Encode(out)
			continue
		}
		line := renderCLILine(l, source)
		if e.out.Rich() && source == "" && l.Source == sourcePlatform {
			// The platform's own voice, told from the app's by colour as well
			// as by the prefix. The prefix stays: it is what --no-color and a
			// pipe still read, and logText strips the colour under either.
			line = e.out.Info("toft") + e.out.Dimmed(" | ") + l.Line
		}
		fmt.Fprintln(e.logOut(), e.logText(line))
	}
	return fresh, nil
}

// filterSince keeps the lines stamped at or after a moment.
//
// Client-side, over the tail the platform already returned: the endpoint takes
// a line count and not a time, and asking it for one would be a second way to
// say how much log to send. A line whose stamp cannot be read is kept — the
// filter narrows what is shown, and dropping a line because this binary could
// not parse its timestamp would lose data to a formatting difference.
func filterSince(lines []logLine, since time.Time) []logLine {
	out := make([]logLine, 0, len(lines))
	for _, l := range lines {
		at, err := time.Parse(time.RFC3339, l.At)
		if err != nil || !at.Before(since) {
			out = append(out, l)
		}
	}
	return out
}

// filterSource keeps one voice.
func filterSource(lines []logLine, source string) []logLine {
	out := make([]logLine, 0, len(lines))
	for _, l := range lines {
		if l.Source == source {
			out = append(out, l)
		}
	}
	return out
}

// renderCLILine marks a platform line so the two voices are tellable apart,
// and leaves an app line as the app wrote it.
//
// **The filtered form is the pipeable form.** With --source the marker is
// dropped entirely: the reason to ask for one voice is usually to pipe it
// somewhere, and a prefix that survives into grep makes the CLI worse at the
// thing it is best at. Asking for one voice has already answered the question
// the marker exists to answer.
func renderCLILine(l logLine, source string) string {
	if source != "" || l.Source != sourcePlatform {
		return l.Line
	}
	return "toft | " + l.Line
}

// newSince returns the lines of fresh that were not in the previous poll.
//
// **Counted per line, not aligned as a sequence.** The tail is two voices
// merged, and the platform's block can move relative to the app's between two
// polls — a new platform line, or a window that is a total across both. An
// overlap match over the sequence then found no overlap and reprinted the whole
// tail every second. Here a line is its source, stream, stamp and text; the
// k-th occurrence of one is new when the previous poll held fewer than k+1 of
// it. With stamps that is exact. Without them (an install that does not stamp
// app lines) identical text is told apart only by count, so a repeated line
// that arrives in the same poll as an older copy leaves the window is missed —
// the best a stampless stream allows, and never a reprint.
func newSince(fresh, seen []logLine) []logLine {
	if len(seen) == 0 {
		return fresh
	}
	before := make(map[logLine]int, len(seen))
	for _, l := range seen {
		before[l]++
	}
	occurrence := make(map[logLine]int, len(fresh))
	var out []logLine
	for _, l := range fresh {
		k := occurrence[l]
		occurrence[l]++
		if k >= before[l] {
			out = append(out, l)
		}
	}
	return out
}

// printBlob is the pre-labels path, kept for an older platform.
func (s *session) printBlob(e *env, fresh string, lines int, seen []logLine) {
	var previous string
	if len(seen) == 1 {
		previous = seen[0].Line
	}
	switch {
	case previous == "":
		fmt.Fprint(e.logOut(), e.logText(fresh))
	case strings.Contains(fresh, previous):
		i := strings.Index(fresh, previous)
		fmt.Fprint(e.logOut(), e.logText(fresh[i+len(previous):]))
	case fresh != "":
		e.out.Dim("… the app logged faster than this poll could follow; try --lines %d", lines*4)
		fmt.Fprint(e.logOut(), e.logText(fresh))
	}
}

func (s *session) buildLog(appID, deployID string) (string, error) {
	var body struct {
		BuildLog string `json:"build_log"`
	}
	path := fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", appID, deployID)
	if err := s.client.Do(http.MethodGet, path, nil, &body); err != nil {
		return "", err
	}
	return body.BuildLog, nil
}

// logTail follows a build's log while it runs.
//
// It is always started, and only *printed* when `--logs` asked for it or when
// the deploy failed — at which point the log is the only thing that matters and
// having to re-run the command to see it would be the wrong answer to the wrong
// question.
type logTail struct {
	mu    sync.Mutex
	text  string
	stop  chan struct{}
	done  sync.WaitGroup
	echo  bool
	fetch func() (string, error)
	// step is the step the deploy is in and ended says it is over: until it
	// is, nothing a later step wrote is printed (pull).
	step  string
	ended bool
	// sink is where an echoed log is written while the deploy runs: stderr,
	// or above the live tree.
	sink io.Writer
}

func followBuildLog(e *env, s *session, app *appSummary, deployID string, echo bool, sink io.Writer) *logTail {
	t := &logTail{
		stop:  make(chan struct{}),
		echo:  echo,
		sink:  sink,
		fetch: func() (string, error) { return s.buildLog(app.ID, deployID) },
	}
	t.done.Add(1)
	go func() {
		defer t.done.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-e.ctx.Done():
				return
			case <-ticker.C:
			}
			t.pull(e)
		}
	}()
	return t
}

// pull fetches the log and prints whatever is new, when the caller asked to see
// it — up to the opening of any step after the current one, which waits for
// that step's ✓ above it (see pullToStep). A log that cannot be read is **not**
// a deploy that failed and must never be reported as one: the progress stream
// is the authority on what happened.
func (t *logTail) pull(e *env) {
	if t == nil {
		return
	}
	t.mu.Lock()
	step := t.step
	t.mu.Unlock()
	t.pullUpTo(e, step)
}

// pullToStep is pull at a step boundary, before the finished step's ✓: it
// prints what the finished step wrote and holds back what the steps after it
// have already written, so the ✓ sits between the two. The build log carries
// no step tags, but the steps that write anything open with a fixed "==> "
// header (stepOpenings), so the first opening of a later step is where the
// hold starts.
//
// **Every print is held this way, not only the boundary's.** On a fast deploy
// one fetch spans several steps, and "==> Detecting framework…" printed above
// "✓ upload fetch" twice over: once because only the last header was held, and
// again because the pull after a ✓ and the once-a-second follower printed
// everything new, later steps included.
func (t *logTail) pullToStep(e *env, finished string) {
	if t == nil {
		return
	}
	t.pullUpTo(e, finished)
}

// setStep records the step the deploy is in, which bounds what pull prints.
func (t *logTail) setStep(step string) {
	if t == nil || step == "" {
		return
	}
	t.mu.Lock()
	t.step = step
	t.mu.Unlock()
}

func (t *logTail) pullUpTo(e *env, step string) {
	text, err := t.fetch()
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.echo && strings.HasPrefix(t.text, text) {
		// Older than what is on screen: two fetches are in flight at once —
		// the follower's and the step boundary's — and the shorter, earlier
		// answer taking the lock second rewound the cursor and printed the
		// difference twice (code review 2026-11-11, M2-4).
		return
	}
	if !t.echo || len(text) <= len(t.text) || !strings.HasPrefix(text, t.text) {
		if !t.echo || !strings.HasPrefix(text, t.text) {
			t.text = text
		}
		return
	}
	fresh := text[len(t.text):]
	cut := len(fresh)
	if !t.ended {
		cut = heldFrom(fresh, step)
	}
	fmt.Fprint(t.sinkOr(e), e.logText(fresh[:cut]))
	t.text += fresh[:cut]
}

func (t *logTail) sinkOr(e *env) io.Writer {
	if t.sink != nil {
		return t.sink
	}
	return e.out.Stderr
}

// stepOrder is the platform's step sequence (toft/internal/deploysteps), and
// stepOpenings the header each step that logs anything opens with
// (internal/builder). Every other "==> " line — the base path, a warning, the
// SDK install — belongs to the step it is written in.
var (
	stepOrder    = []string{"receive", "unpack", "setup", "fetch", "scan", "detect", "configure", "build", "publish", "start", "ready", "flip"}
	stepOpenings = map[string]string{
		"==> Source uploaded":                     "fetch",
		"==> Reusing source from":                 "fetch",
		"==> Cloning repository":                  "fetch",
		"==> Detecting framework":                 "detect",
		"==> This is a batch app":                 "start",
		"==> Starting release":                    "start",
		"==> Waiting for the app to become ready": "ready",
		"==> Stopping previous release":           "flip",
	}
)

// heldFrom is where, in fresh log text, a step after finished opens: the start
// of its header line, or the end when nothing later has started yet.
func heldFrom(fresh, finished string) int {
	after := -1
	for i, s := range stepOrder {
		if s == finished {
			after = i
		}
	}
	at := 0
	for _, line := range strings.SplitAfter(fresh, "\n") {
		for prefix, step := range stepOpenings {
			if strings.HasPrefix(line, prefix) && slices.Index(stepOrder, step) > after {
				return at
			}
		}
		at += len(line)
	}
	return len(fresh)
}

// close stops following, and takes one last look first.
//
// That final fetch is what makes `--logs` honest on a fast build: a deploy that
// takes two seconds is over before a one-second poll has seen the end of its
// log, and printing the phases and then nothing would read as a build that
// produced no output. It is also what fills the tail a *failed* deploy prints
// without being asked.
func (t *logTail) close(e *env) {
	if t == nil {
		return
	}
	close(t.stop)
	t.done.Wait()
	t.mu.Lock()
	t.ended = true
	t.mu.Unlock()
	t.pull(e)
}

// printTail writes the last n lines, which is what a failed deploy is asked
// about. The whole log is rarely the question; the end of it always is.
func (t *logTail) printTail(e *env, n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	text := t.text
	t.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		e.out.Say("")
		e.out.Dim("  … %d earlier lines, `toft logs --deploy <id>` for all of them", len(lines)-n)
		lines = lines[len(lines)-n:]
	}
	e.out.Say("")
	for _, line := range lines {
		fmt.Fprintln(e.out.Stderr, "  "+e.logText(line))
	}
}

// readTOML is the CLI's one-line TOML read, for the `[static] root` lookup.
func readTOML(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = toml.Decode(string(data), into)
	return err
}

// ansiEscape is a terminal control sequence: CSI (colour, cursor), OSC
// (titles, links, the clipboard), and ESC with whatever single character
// follows it — which also takes the opening of an OSC that never ends.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[ -~]?)`)

// sgr is the one sequence a terminal is given: Select Graphic Rendition,
// which is colour and weight and nothing else.
var sgr = regexp.MustCompile(`^\x1b\[[0-9;]*m$`)

// logText is a log's text as this run should print it. **A terminal gets the
// app's colours as the app wrote them** — that is the reader they were for —
// and nothing else the app wrote: an OSC can write the clipboard, retitle the
// window or dress a link as another, and a cursor move can overwrite the
// platform's own lines, so every sequence but SGR is removed. A pipe or a file
// gets the text alone, because `\x1b[35m` in a grep or a CI log is noise.
// `--no-color`, NO_COLOR and TERM=dumb ask for the same. Under --json `line`
// is the text alone and `raw` the platform's record (jsonLogText).
//
// **Nor any other control character.** A `\r` or a run of `\b` let an app's
// line overwrite the `toft |` prefix printed before it and pass as the
// platform's voice, SO switched the terminal's charset for everything after,
// and U+009B/U+009D are CSI and OSC with no ESC at all (security review
// 2026-11-14, CS-6). dropControls removes them, between the kept SGRs.
func (e *env) logText(s string) string {
	if !hasControl(s) {
		return s
	}
	if e.plainLogs {
		return plainText(s)
	}
	s = crLines(s)
	var b strings.Builder
	prev := 0
	for _, loc := range ansiEscape.FindAllStringIndex(s, -1) {
		b.WriteString(stripControlRunes(s[prev:loc[0]]))
		if seq := s[loc[0]:loc[1]]; sgr.MatchString(seq) {
			b.WriteString(seq)
		}
		prev = loc[1]
	}
	b.WriteString(stripControlRunes(s[prev:]))
	return b.String()
}

// isControl is a C0 or C1 control, DEL included, other than the two a log is
// made of: a tab and a newline.
func isControl(r rune) bool {
	if r == '\t' || r == '\n' {
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// hasControl reports whether s holds anything dropControls would change: a
// control rune, or a byte that is not UTF-8 in the C1 range (a terminal that
// reads bytes would take it as one).
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f || c == 0xc2 || (c >= 0x80 && c <= 0x9f) {
			return true
		}
	}
	return false
}

// dropControls is s without a control character but `\t` and `\n`, its
// carriage returns resolved as a terminal would show them: one that ends a
// line is dropped, and of a line with one inside only the text after the last
// is kept — what `terminalLine` does on the server.
func dropControls(s string) string {
	if !hasControl(s) {
		return s
	}
	return stripControlRunes(crLines(s))
}

// crLines resolves each line's carriage returns (dropControls).
func crLines(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if j := strings.LastIndexByte(line, '\r'); j >= 0 {
			line = line[j+1:]
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// stripControlRunes removes every control rune (isControl), and a lone byte
// in the C1 range that is not part of a UTF-8 sequence.
func stripControlRunes(s string) string {
	if !hasControl(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			if c := s[i]; c < 0x80 || c > 0x9f {
				b.WriteByte(c)
			}
		case !isControl(r):
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// writesToTerminal reports whether w is a terminal; a writer that is not a
// file (a test's buffer) is not one.
func writesToTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f)
}

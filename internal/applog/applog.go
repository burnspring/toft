// Package applog is the platform's own voice about one app, kept where its
// owner can read it after the workload is gone.
//
// # Why a package and not a convention
//
// Every per-app diagnostic the platform writes goes to log.Printf, composed for
// somebody with a shell on the box: absolute paths under APPS_DIR, artifact
// store endpoints, wrapped database/sql errors. Those lines are the operator's
// and they stay exactly where they are. This package is the *second* register —
// what the app's owner is owed — and it exists as a stage rather than as a
// habit for the reason internal/telemetry's attribute allow-list does: the set
// of writers becomes a property a test can assert rather than something a code
// review has to keep noticing.
//
// The writing verbs are Say and Sayf and nothing else is one, which is what
// lets callsites_test.go find every one of them by name. A file that calls
// either and is not on that test's list fails the build.
//
// # The rule the writers keep
//
//   - **No error value ever reaches Say.** The signature takes none on purpose.
//     Where the owner needs the cause, the writer composes a sentence out of
//     words the platform controls, the way builder.Repair already does when a
//     framework can no longer be detected.
//   - **Stage marks, never a stream.** Anything that could repeat per line of a
//     subprocess, per request or per sweep tick is disqualified by
//     construction. What a build command prints is the app's voice and goes
//     where the app's output goes — which is what the isolated runner has
//     always done, pumping build output as `app` and narrating only the `==>`
//     lines as `platform`.
//
// Neither is enforced by anything here. Free text has no handle a filter could
// take hold of, and pretending otherwise would be a promise the implementation
// could not keep; the allow-list bounds *who* may write, not what they say.
//
// # Absent, not off
//
// A nil *Sink is a real configuration — a backend standing on its own in a
// test, an install wired without one — and every method on it is a no-op that
// does not error. A log line may never be the reason something else breaks.
package applog

import (
	"fmt"
	"log"
	"sync"
	"time"

	"toft/internal/database"
	"toft/internal/textutil"
)

const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

const (
	// flushInterval is the window between the last line said and the last line
	// stored, and therefore what an abrupt platform stop loses. Shorter than
	// usage.Recorder's ten seconds, because that window is exactly the thing
	// this feature exists to close: the narration of a start that did not
	// finish is what the owner came to read.
	flushInterval = 2 * time.Second

	// maxPerStart bounds one workload's narration between StartOf calls. The
	// 200th line is followed by one line naming what was suppressed and
	// nothing else is stored for that workload until it starts again.
	//
	// Code and not a setting: it exists to stop a loop, and an operator who
	// could raise it could turn a log into an outage.
	maxPerStart = 200

	// maxBuffered bounds what one flush window may hold across every app, so a
	// database outage costs a bounded amount of memory rather than an
	// unbounded one. The newest are dropped rather than the oldest, for the
	// reason the runner's shipper drops that way: the beginning of a failure
	// is what explains it.
	maxBuffered = 5000

	// maxLineBytes truncates a monstrous line rather than storing it, matching
	// the runner's own bound.
	maxLineBytes = 16 * 1024

	// listenerBuffer is one live subscriber's depth, and a slow reader is
	// dropped on rather than waited for — Builder.SubscribeLogs' arrangement,
	// for the same reason: live is never load-bearing, the durable copy is.
	listenerBuffer = 256

	// counterIdle is how long a workload's line count survives without being
	// written to. It only exists so the counter map does not grow with every
	// app the install has ever started.
	counterIdle = 30 * time.Minute
)

// Store is the durable half. An interface rather than *database.DB so the sink
// can be tested without one, and so a test can assert *which goroutine* calls
// it — which is the one property this package must never get wrong.
type Store interface {
	InsertPlatformLog(lines []database.PlatformLogLine) error
}

type counter struct {
	n        int
	said     bool // the suppression line has been emitted
	lastSeen time.Time
}

// Sink buffers the platform's narration and flushes it in the background.
//
// The buffer is the write path, never the store. Five of the six writers are
// nowhere near a request, but the sixth is AgentRuntime.Deploy, which the proxy's
// wake reaches — so a synchronous insert here would put a database write on the
// proxy request path, and the invariant is about the path rather than about a
// caller's good intentions.
type Sink struct {
	store Store
	now   func() time.Time

	mu        sync.Mutex
	buf       []database.PlatformLogLine
	seq       int
	dropped   int
	counters  map[string]*counter
	listeners map[string][]chan database.PlatformLogLine

	once sync.Once
	stop chan struct{}
}

func New(store Store) *Sink {
	return &Sink{
		store:     store,
		now:       func() time.Time { return time.Now().UTC() },
		counters:  map[string]*counter{},
		listeners: map[string][]chan database.PlatformLogLine{},
		stop:      make(chan struct{}),
	}
}

// Writer is the sink bound to one app's workload. The subject is fixed once so
// a call site says what happened rather than repeating who it happened to.
type Writer struct {
	sink         *Sink
	appID        string
	workloadName string
	deploymentID string
}

// For binds the sink to one workload. A nil sink yields a Writer whose verbs
// do nothing, which is what makes an unwired install silent rather than broken.
func (s *Sink) For(appID, workloadName, deploymentID string) Writer {
	return Writer{sink: s, appID: appID, workloadName: workloadName, deploymentID: deploymentID}
}

// Say records one line of the platform's account of this workload.
//
// It returns nothing and cannot fail. Everything after this call is the sink's
// problem, and a workload may never fail to start because its narration could
// not be written down.
func (w Writer) Say(stream, line string) {
	if w.sink == nil || w.appID == "" {
		return
	}
	w.sink.record(w.appID, w.workloadName, w.deploymentID, stream, line)
}

// Sayf is Say with formatting. No argument may be an error value — see the
// package comment for why the rule is stated rather than enforced.
func (w Writer) Sayf(stream, format string, args ...any) {
	if w.sink == nil || w.appID == "" {
		return
	}
	w.sink.record(w.appID, w.workloadName, w.deploymentID, stream, fmt.Sprintf(format, args...))
}

// StartOf resets the per-start line count for one workload.
//
// Called by the two places that know a workload's narration is beginning: the
// shared launcher, at the top of a start, and the runner bootstrap handler,
// which is a pod's first contact with the platform. Nothing else needs it —
// a background actor writing one line about an app is not a start.
func (s *Sink) StartOf(appID, workloadName string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.counters, appID+"\x00"+workloadName)
	s.mu.Unlock()
}

func (s *Sink) record(appID, workloadName, deploymentID, stream, line string) {
	s.once.Do(func() { go s.loop() })

	if len(line) > maxLineBytes {
		line = textutil.Clip(line, maxLineBytes) + " …[truncated]"
	}

	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	key := appID + "\x00" + workloadName
	c := s.counters[key]
	if c == nil {
		c = &counter{}
		s.counters[key] = c
	}
	c.lastSeen = now
	c.n++
	switch {
	case c.n > maxPerStart && c.said:
		return
	case c.n > maxPerStart:
		// One line naming what stopped, then silence until the next start.
		// Truncation counted out loud rather than dropped quietly, which is
		// the whole reason the runner counts its own drops.
		c.said = true
		stream = StreamStderr
		line = fmt.Sprintf("… further lines suppressed: this start said more than %d things", maxPerStart)
	}

	if len(s.buf) >= maxBuffered {
		s.dropped++
		return
	}
	s.seq++
	l := database.PlatformLogLine{
		AppID:        appID,
		WorkloadName: workloadName,
		DeploymentID: deploymentID,
		At:           now,
		Seq:          s.seq,
		Stream:       stream,
		Line:         line,
	}
	s.buf = append(s.buf, l)

	// Live, now, and never from the table. A subscriber that cannot keep up is
	// dropped on: the durable copy is what the next reader gets.
	for _, ch := range s.listeners[appID] {
		select {
		case ch <- l:
		default:
		}
	}
}

func (s *Sink) loop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush()
		case <-s.stop:
			return
		}
	}
}

// Flush writes and clears whatever is buffered.
//
// Called on the ticker, at the end of a start — so the narration of the thing
// the owner is asking about is on disk before the workload is — and once on the
// way down, beside the HTTP server's drain. That last one is the difference
// from usage.Recorder, whose silent loss on restart is an acceptable rounding
// error for a counter and is not acceptable for this.
//
// A failed write drops the batch rather than retrying. Unbounded retry inside a
// database outage grows memory for the length of it, and these lines are
// best-effort by construction.
func (s *Sink) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if len(s.buf) == 0 && s.dropped == 0 {
		s.pruneCountersLocked()
		s.mu.Unlock()
		return
	}
	batch, dropped := s.buf, s.dropped
	s.buf, s.dropped = nil, 0
	s.pruneCountersLocked()
	s.mu.Unlock()

	if dropped > 0 && len(batch) > 0 {
		// Attached to the app whose line was last admitted. The count is
		// install-wide and the row is not, which is imprecise and still better
		// than a silent gap — the reader's question is "is this everything?",
		// and the honest answer is no.
		last := batch[len(batch)-1]
		batch = append(batch, database.PlatformLogLine{
			AppID:        last.AppID,
			WorkloadName: last.WorkloadName,
			At:           last.At,
			Seq:          last.Seq + 1,
			Stream:       StreamStderr,
			Line:         fmt.Sprintf("… %d line(s) dropped: the platform log buffer overflowed", dropped),
		})
	}
	if err := s.store.InsertPlatformLog(batch); err != nil {
		// The operator's log, and only there: this is the platform failing to
		// write down what it said, which is not the app's owner's problem and
		// cannot be reported into the surface that just failed.
		log.Printf("platform log: dropping %d line(s): %v", len(batch), err)
	}
}

func (s *Sink) pruneCountersLocked() {
	cutoff := s.now().Add(-counterIdle)
	for k, c := range s.counters {
		if c.lastSeen.Before(cutoff) {
			delete(s.counters, k)
		}
	}
}

// Close flushes once more and stops the loop. Idempotent, because shutdown
// paths are not always as singular as they look.
func (s *Sink) Close() {
	if s == nil {
		return
	}
	s.Flush()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// Subscribe returns a channel carrying this app's platform lines as they are
// said. The caller must Unsubscribe.
//
// Shared mode only, in practice: an isolated workload's platform lines already
// ride the pod stream the backend follows, and reading them twice is what the
// merge's dedupe would then have to undo.
func (s *Sink) Subscribe(appID string) chan database.PlatformLogLine {
	ch := make(chan database.PlatformLogLine, listenerBuffer)
	if s == nil {
		close(ch)
		return ch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners[appID] = append(s.listeners[appID], ch)
	return ch
}

// Listeners is how many subscriptions an app has open. For tests, which is
// the one reader that needs to know a socket let go of its subscription.
func (s *Sink) Listeners(appID string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners[appID])
}

func (s *Sink) Unsubscribe(appID string, ch chan database.PlatformLogLine) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.listeners[appID] {
		if l == ch {
			s.listeners[appID] = append(s.listeners[appID][:i], s.listeners[appID][i+1:]...)
			close(ch)
			if len(s.listeners[appID]) == 0 {
				delete(s.listeners, appID)
			}
			return
		}
	}
}

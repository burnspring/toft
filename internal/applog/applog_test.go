package applog

import (
	"errors"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"toft/internal/database"
)

type fakeStore struct {
	mu     sync.Mutex
	calls  int
	lines  []database.PlatformLogLine
	stacks []string
	err    error
}

func (f *fakeStore) InsertPlatformLog(lines []database.PlatformLogLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.stacks = append(f.stacks, string(debug.Stack()))
	f.lines = append(f.lines, lines...)
	return f.err
}

func (f *fakeStore) snapshot() (int, []database.PlatformLogLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]database.PlatformLogLine(nil), f.lines...)
}

// TestSayNeverReachesTheStore is the one whose failure would be silent.
//
// Five of the six writers are nowhere near a request path. The sixth is
// PM2Runtime.Deploy, which the proxy's wake reaches — so a Say that inserted
// synchronously would put a database write on the proxy request path, and
// nothing in the product would look different until an install got busy. The
// property is not "it is fast": it is that the store is not touched at all.
func TestSayNeverReachesTheStore(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()
	w := s.For("app1", "app1-r1", "dep1")

	for i := 0; i < 50; i++ {
		w.Say(StreamStdout, "a stage mark")
	}

	if calls, _ := store.snapshot(); calls != 0 {
		t.Fatalf("Say reached the store %d time(s) on the caller's goroutine", calls)
	}

	s.Flush()
	calls, lines := store.snapshot()
	if calls != 1 {
		t.Fatalf("Flush wrote %d batches, want 1", calls)
	}
	if len(lines) != 50 {
		t.Fatalf("stored %d lines, want 50", len(lines))
	}
	if lines[0].AppID != "app1" || lines[0].WorkloadName != "app1-r1" || lines[0].DeploymentID != "dep1" {
		t.Errorf("the subject did not survive: %+v", lines[0])
	}
}

// TestTheStoreIsTouchedOffTheSayingStack is the same property read from the
// other end, and it is what catches a Say that grows a store call *below* it —
// through a helper, say — where a count of zero would no longer notice.
func TestTheStoreIsTouchedOffTheSayingStack(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()
	s.For("app1", "app1-r1", "").Say(StreamStdout, "a stage mark")

	// The ticker rather than an explicit Flush: an explicit one is legitimately
	// on the caller's stack (Close does it on the way down), and the path being
	// asserted here is the background one.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _ := store.snapshot(); calls > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	store.mu.Lock()
	stacks := append([]string(nil), store.stacks...)
	store.mu.Unlock()
	if len(stacks) == 0 {
		t.Fatal("the flush loop never wrote — the sink's ticker is not running")
	}
	for _, st := range stacks {
		// Only this goroutine's own frames. Everything after "created by" is
		// ancestry — the flush loop is *started* by the first record() call,
		// so that line names Say's stack forever and would make this assertion
		// pass for the wrong reason and then fail for it.
		if i := strings.Index(st, "\ncreated by "); i >= 0 {
			st = st[:i]
		}
		if strings.Contains(st, "applog.Writer.Say") || strings.Contains(st, "applog.(*Sink).record") {
			t.Errorf("the store was reached from inside Say:\n%s", st)
		}
	}
}

// TestOneStartIsCapped: the belt on D4's rule that narration is stage marks and
// never a stream. The suppression line is emitted once and nothing follows it.
func TestOneStartIsCapped(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()
	w := s.For("app1", "app1-r1", "")

	for i := 0; i < maxPerStart+500; i++ {
		w.Say(StreamStdout, "chatter")
	}
	s.Flush()

	_, lines := store.snapshot()
	if len(lines) != maxPerStart+1 {
		t.Fatalf("stored %d lines, want %d plus one suppression line", len(lines), maxPerStart)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last.Line, "further lines suppressed") {
		t.Errorf("the cap was silent: last line is %q", last.Line)
	}
	if last.Stream != StreamStderr {
		t.Errorf("the suppression line is on %s, want stderr", last.Stream)
	}
}

// TestStartOfReopensTheBudget: a workload that starts again is a new start, and
// a crash loop must not be silenced by the narration of the start before it.
func TestStartOfReopensTheBudget(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()

	for i := 0; i < maxPerStart+10; i++ {
		s.For("app1", "app1-r1", "").Say(StreamStdout, "chatter")
	}
	s.StartOf("app1", "app1-r1")
	s.For("app1", "app1-r1", "").Say(StreamStdout, "the next start speaks")
	s.Flush()

	_, lines := store.snapshot()
	if lines[len(lines)-1].Line != "the next start speaks" {
		t.Fatalf("the second start was silenced: last line is %q", lines[len(lines)-1].Line)
	}
}

// TestCapIsPerWorkload: two apps do not share a budget, or one chatty app would
// silence the install.
func TestCapIsPerWorkload(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()

	for i := 0; i < maxPerStart+10; i++ {
		s.For("app1", "app1-r1", "").Say(StreamStdout, "chatter")
	}
	s.For("app2", "app2-r1", "").Say(StreamStdout, "app2 still speaks")
	s.Flush()

	_, lines := store.snapshot()
	var seen bool
	for _, l := range lines {
		if l.AppID == "app2" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("app2 was silenced by app1's budget")
	}
}

// TestAFailedFlushDropsTheBatch. Unbounded retry inside a database outage grows
// memory for the length of it, and these lines are best-effort by construction.
func TestAFailedFlushDropsTheBatch(t *testing.T) {
	store := &fakeStore{err: errors.New("database is down")}
	s := New(store)
	defer s.Close()

	s.For("app1", "app1-r1", "").Say(StreamStdout, "lost")
	s.Flush()
	store.err = nil
	s.Flush()

	calls, _ := store.snapshot()
	if calls != 1 {
		t.Fatalf("the failed batch was retried: %d store calls, want 1", calls)
	}
}

// TestANilSinkIsSilentAndDoesNotPanic. Absent, not off: a backend standing on
// its own in a test has nothing to narrate to, and that is a real
// configuration rather than a gap.
func TestANilSinkIsSilentAndDoesNotPanic(t *testing.T) {
	var s *Sink
	s.For("app1", "app1-r1", "").Say(StreamStdout, "into the void")
	s.For("app1", "app1-r1", "").Sayf(StreamStderr, "%s", "also the void")
	s.StartOf("app1", "app1-r1")
	s.Flush()
	ch := s.Subscribe("app1")
	if _, open := <-ch; open {
		t.Error("a nil sink handed out a live channel")
	}
	s.Unsubscribe("app1", ch)
	s.Close()
}

// TestSubscribeSeesLinesAsTheyAreSaid — live never comes from the table.
func TestSubscribeSeesLinesAsTheyAreSaid(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()

	ch := s.Subscribe("app1")
	defer s.Unsubscribe("app1", ch)

	s.For("app1", "app1-r1", "").Say(StreamStdout, "said now")
	s.For("app2", "app2-r1", "").Say(StreamStdout, "somebody else's app")

	select {
	case l := <-ch:
		if l.Line != "said now" {
			t.Fatalf("got %q", l.Line)
		}
	case <-time.After(time.Second):
		t.Fatal("the subscriber saw nothing")
	}
	select {
	case l := <-ch:
		t.Fatalf("the subscriber saw another app's line: %q", l.Line)
	default:
	}
}

// TestASlowSubscriberIsDroppedOn, not waited for. A log line may never be the
// reason a start blocks.
func TestASlowSubscriberIsDroppedOn(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()

	ch := s.Subscribe("app1")
	defer s.Unsubscribe("app1", ch)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < listenerBuffer*3; i++ {
			s.For("app1", "app1-r1", "").Say(StreamStdout, "chatter")
			s.StartOf("app1", "app1-r1") // keep the per-start cap out of it
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full subscriber channel blocked the writer")
	}
}

// A monstrous line is cut, and the cut must not split a rune: invalid UTF-8
// makes Postgres refuse the whole batch, every app's lines with it (JOBS-1).
func TestATruncatedLineIsStillUTF8(t *testing.T) {
	store := &fakeStore{}
	s := New(store)
	defer s.Close()
	s.For("app1", "app1-r1", "").Say(StreamStdout, "xy"+strings.Repeat("€", maxLineBytes)) // the cut lands mid-rune
	s.Flush()

	_, lines := store.snapshot()
	if len(lines) != 1 {
		t.Fatalf("stored %d lines, want 1", len(lines))
	}
	if !utf8.ValidString(lines[0].Line) || !strings.HasSuffix(lines[0].Line, "[truncated]") {
		t.Fatalf("truncated line is not valid UTF-8 or not marked: %q", lines[0].Line[len(lines[0].Line)-40:])
	}
}

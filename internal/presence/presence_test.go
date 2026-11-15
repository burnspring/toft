package presence

import (
	"sync"
	"testing"
	"time"
)

// clock lets a test move time without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newAt(base time.Time) (*Tracker, *clock) {
	c := &clock{t: base}
	tr := New()
	tr.now = c.now
	return tr, c
}

func TestActive_WindowExcludesStaleAndOrdersByRecency(t *testing.T) {
	tr, c := newAt(time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))

	tr.Touch("alice", "app-1")
	c.advance(2 * time.Minute)
	tr.Touch("bob", "")
	c.advance(4 * time.Minute) // alice is now 6m old, bob 4m

	got := tr.Active(5 * time.Minute)
	if len(got) != 1 || got[0].UserID != "bob" {
		t.Fatalf("5m window = %+v, want bob only", got)
	}

	got = tr.Active(10 * time.Minute)
	if len(got) != 2 {
		t.Fatalf("10m window returned %d entries, want 2", len(got))
	}
	if got[0].UserID != "bob" || got[1].UserID != "alice" {
		t.Fatalf("order = %s, %s; want most recent first", got[0].UserID, got[1].UserID)
	}
	if got[1].AppID != "app-1" {
		t.Fatalf("alice's app = %q, want app-1", got[1].AppID)
	}
	if got[0].AppID != "" {
		t.Fatalf("bob only used the platform, but reports app %q", got[0].AppID)
	}
}

// Platform activity after opening an app must not make the app look freshly
// used — that is the whole reason AppSeen is separate from LastSeen.
func TestTouch_PlatformActivityKeepsButDoesNotRefreshTheApp(t *testing.T) {
	tr, c := newAt(time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))

	tr.Touch("alice", "app-1")
	appSeen := tr.Active(time.Minute)[0].AppSeen
	c.advance(3 * time.Minute)
	tr.Touch("alice", "")

	got := tr.Active(time.Minute)
	if len(got) != 1 {
		t.Fatalf("Active = %+v, want alice still online", got)
	}
	if got[0].AppID != "app-1" {
		t.Fatalf("AppID = %q, want the app to be remembered", got[0].AppID)
	}
	if !got[0].AppSeen.Equal(appSeen) {
		t.Fatalf("AppSeen moved to %v; platform activity must not refresh it", got[0].AppSeen)
	}
	if !got[0].LastSeen.After(appSeen) {
		t.Fatal("LastSeen did not advance on platform activity")
	}
}

// Anonymous app traffic has nobody to name, so it must not appear at all.
func TestTouch_IgnoresAnonymous(t *testing.T) {
	tr, _ := newAt(time.Now())
	tr.Touch("", "app-1")
	if n := tr.Count(time.Minute); n != 0 {
		t.Fatalf("Count = %d after anonymous touch, want 0", n)
	}
}

// A tracker nobody queries must not grow forever; a touch past the prune
// interval drops whatever aged out of Retention.
func TestTouch_PrunesBeyondRetention(t *testing.T) {
	tr, c := newAt(time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))

	tr.Touch("alice", "app-1")
	c.advance(Retention + time.Minute)
	tr.Touch("bob", "")

	tr.mu.Lock()
	n := len(tr.seen)
	_, aliceHeld := tr.seen["alice"]
	tr.mu.Unlock()

	if aliceHeld || n != 1 {
		t.Fatalf("after retention: %d entries held, alice present=%v", n, aliceHeld)
	}
}

// A caller asking for more than the tracker remembers gets Retention's worth,
// not a silently empty answer past the memory's edge.
func TestActive_ClampsWindowToRetention(t *testing.T) {
	tr, c := newAt(time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))

	tr.Touch("alice", "")
	c.advance(20 * time.Minute)

	if n := len(tr.Active(24 * time.Hour)); n != 1 {
		t.Fatalf("24h window = %d entries, want the 1 still within retention", n)
	}
}

func TestTracker_ConcurrentTouchAndActive(t *testing.T) {
	tr := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); tr.Touch("user", "app-1") }(i)
		go func() { defer wg.Done(); tr.Active(DefaultWindow) }()
	}
	wg.Wait()
	if n := tr.Count(DefaultWindow); n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}
}

// A nil tracker is what a Server built without one holds; every method must be
// a no-op rather than a panic on the request path.
func TestNilTracker(t *testing.T) {
	var tr *Tracker
	tr.Touch("alice", "app-1")
	if tr.Active(time.Minute) != nil || tr.Count(time.Minute) != 0 {
		t.Fatal("nil tracker did not read as empty")
	}
}

package idle

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

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestIdleSince_UnknownAppIsBusyAndSeeded(t *testing.T) {
	tr, _ := newAt(base)

	// The restart case: an app the tracker has never seen must get a full
	// grace period, not be reaped on the first sweep.
	if tr.IdleSince("a", base, 5*time.Minute) {
		t.Fatal("an app with no entry must not be reported idle")
	}
	// Seeded at the time of the question, so it becomes reapable a full
	// threshold later and not before.
	if tr.IdleSince("a", base.Add(4*time.Minute), 5*time.Minute) {
		t.Fatal("seeded entry went idle before its grace period elapsed")
	}
	if !tr.IdleSince("a", base.Add(5*time.Minute), 5*time.Minute) {
		t.Fatal("seeded entry never went idle")
	}
}

func TestIdleSince_TouchResetsTheClock(t *testing.T) {
	tr, c := newAt(base)
	tr.Touch("a")

	c.advance(4 * time.Minute)
	tr.Touch("a")

	if tr.IdleSince("a", base.Add(8*time.Minute), 5*time.Minute) {
		t.Fatal("a touch four minutes ago must hold the app awake")
	}
	if !tr.IdleSince("a", base.Add(10*time.Minute), 5*time.Minute) {
		t.Fatal("app should be idle five minutes after its last touch")
	}
}

func TestIdleSince_InFlightRequestHoldsAppAwake(t *testing.T) {
	tr, c := newAt(base)

	// The streaming case: an SSE subscription opens, and an hour later it is
	// still open. Its *arrival* is an hour stale; it is still activity.
	done := tr.Begin("a")
	c.advance(time.Hour)
	if tr.IdleSince("a", base.Add(time.Hour), 5*time.Minute) {
		t.Fatal("an app with a request in flight must never be reported idle")
	}

	done()
	// Ending stamps activity, so the app stays awake for a full threshold from
	// the moment the response finished — not from when it started.
	if tr.IdleSince("a", base.Add(time.Hour).Add(4*time.Minute), 5*time.Minute) {
		t.Fatal("a just-finished response must count as activity")
	}
	if !tr.IdleSince("a", base.Add(time.Hour).Add(5*time.Minute), 5*time.Minute) {
		t.Fatal("app should go idle a threshold after the response ended")
	}
}

func TestBegin_NestedRequestsUnwindToZero(t *testing.T) {
	tr, _ := newAt(base)
	first, second := tr.Begin("a"), tr.Begin("a")

	first()
	if tr.IdleSince("a", base.Add(time.Hour), time.Minute) {
		t.Fatal("one of two in-flight requests ended — the app is still busy")
	}
	second()
	if !tr.IdleSince("a", base.Add(time.Hour), time.Minute) {
		t.Fatal("with nothing in flight the app should be idle")
	}
}

func TestRetain_DropsDeletedApps(t *testing.T) {
	tr, _ := newAt(base)
	tr.Touch("keep")
	tr.Touch("gone")

	tr.Retain([]string{"keep"})

	tr.mu.Lock()
	_, kept := tr.apps["keep"]
	_, dropped := tr.apps["gone"]
	tr.mu.Unlock()

	if !kept {
		t.Fatal("Retain dropped a live app")
	}
	if dropped {
		t.Fatal("Retain kept an app that no longer exists")
	}
}

func TestNilTrackerIsInert(t *testing.T) {
	var tr *Tracker
	tr.Touch("a")
	tr.Begin("a")()
	tr.Retain([]string{"a"})
	if tr.IdleSince("a", base, time.Minute) {
		t.Fatal("a nil tracker must never report an app idle — nothing may be reaped on its word")
	}
}

// The proxy calls Touch and Begin from every request goroutine at once; run
// with -race.
func TestTracker_ConcurrentUse(t *testing.T) {
	tr := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tr.Touch("a")
				done := tr.Begin("a")
				tr.IdleSince("a", time.Now(), time.Minute)
				done()
			}
		}()
	}
	wg.Wait()

	tr.mu.Lock()
	active := tr.apps["a"].active
	tr.mu.Unlock()
	if active != 0 {
		t.Fatalf("in-flight count did not unwind: %d", active)
	}
}

// BeginQuiet holds a request in flight without recording that anybody was
// there. It exists for the reachability prober: an anonymous GET from outside,
// arriving every hour forever, whose whole purpose is to be indistinguishable
// from a visitor to the app and which must be entirely distinguishable from
// one here.
func TestBeginQuiet_HoldsTheAppWithoutStampingIt(t *testing.T) {
	tr, c := newAt(base)

	// A real visit, so the app has a last-access to lose.
	tr.Touch("a")
	c.advance(time.Hour)

	done := tr.BeginQuiet("a")
	// In flight: the reaper must not stop the workload underneath the request,
	// however quiet it is.
	if tr.IdleSince("a", c.now(), 5*time.Minute) {
		t.Error("an app with a quiet request in flight was reported idle")
	}
	done()

	// And the hour that passed is still an hour: the request stamped nothing on
	// the way in or on the way out.
	if !tr.IdleSince("a", c.now(), 5*time.Minute) {
		t.Error("a quiet request stamped the app awake — an hourly probe would " +
			"keep every public app awake for as long as the install exists")
	}

	// Begin is the control: the same call that does count.
	done = tr.Begin("a")
	done()
	if tr.IdleSince("a", c.now(), 5*time.Minute) {
		t.Error("Begin stopped counting as activity")
	}
}

// A quiet request on an app the tracker has never seen still leaves it busy,
// which is the answer an unseen app already gets — so a probe cannot make one
// reapable that would not have been.
func TestBeginQuiet_UnknownAppStaysBusy(t *testing.T) {
	tr, c := newAt(base)

	done := tr.BeginQuiet("a")
	if tr.IdleSince("a", c.now(), 5*time.Minute) {
		t.Error("an unseen app with a quiet request in flight was reported idle")
	}
	done()
	c.advance(time.Hour)
	if !tr.IdleSince("a", c.now(), 5*time.Minute) {
		t.Error("an app whose only traffic was a probe is being held awake by it")
	}
}

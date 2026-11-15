package idle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A burst of requests for one sleeping app is one wake.
//
// This is what the type is for: a link in a chat message fetches the page and
// every asset on it, each arrives to find the app asleep, and without this they
// would each call Restart and the app would bounce once per request.
func TestConcurrentWakesOfOneAppAreOneCall(t *testing.T) {
	g := NewWakeGroup()
	var calls int
	var mu sync.Mutex
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Do(context.Background(), "app-1", func() error {
				mu.Lock()
				calls++
				mu.Unlock()
				<-release
				return nil
			})
		}()
	}

	// Let them all arrive, then let the one call finish.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Errorf("%d wakes ran, want 1 — the burst was not collapsed", calls)
	}
}

// **An instruction is not a burst.**
//
// An administrator attaches storage; the developer restarts. Joining the start
// already in flight returned *that* one's success — a pod whose workload
// description was assembled before the mapping existed, serving with no volumes
// and exit 0. A second restart, nothing else changed, brought the mounts. So a
// caller who passes the moment they asked is not answered by older work.
func TestACallerIsNotAnsweredByWorkOlderThanItsRequest(t *testing.T) {
	g := NewWakeGroup()
	var order []string
	var mu sync.Mutex

	first := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = g.Do(context.Background(), "app-1", func() error {
			close(started)
			mu.Lock()
			order = append(order, "the start already in flight")
			mu.Unlock()
			<-first
			return nil
		})
	}()
	<-started

	// The restart arrives *after* that call began.
	asked := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- g.DoAfter(context.Background(), "app-1", asked, func() error {
			mu.Lock()
			order = append(order, "the restart the person asked for")
			mu.Unlock()
			return nil
		})
	}()

	// It must not have been satisfied yet: the only call that has run is the
	// older one, and it has not finished.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	ran := len(order)
	mu.Unlock()
	if ran != 1 {
		t.Fatalf("%d calls ran while the older one was still going; the restart joined it", ran)
	}

	close(first)
	if err := <-done; err != nil {
		t.Fatalf("the restart returned %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[1] != "the restart the person asked for" {
		t.Errorf("order = %v, want the older call then a fresh one", order)
	}
}

// And a call that began *after* the request is still joined: two restarts a
// moment apart are one restart, which is what both callers asked for.
func TestACallerJoinsWorkNewerThanItsRequest(t *testing.T) {
	g := NewWakeGroup()
	asked := time.Now()
	time.Sleep(2 * time.Millisecond)

	var calls int
	var mu sync.Mutex
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.DoAfter(context.Background(), "app-1", asked, func() error {
				mu.Lock()
				calls++
				mu.Unlock()
				<-release
				return nil
			})
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Errorf("%d calls ran, want 1 — work newer than the request is still shared", calls)
	}
}

// A waiter that gives up does not cancel the work; the next one finds it done.
func TestAbandoningTheWaitDoesNotCancelTheWake(t *testing.T) {
	g := NewWakeGroup()
	ran := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = g.Do(context.Background(), "app-1", func() error {
			// Signalled from inside the call, so the waiter below cannot win
			// the race to start the work itself and then find nothing to wait
			// for.
			close(started)
			time.Sleep(30 * time.Millisecond)
			close(ran)
			return errors.New("finished anyway")
		})
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := g.Do(ctx, "app-1", func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a waiter that gave up got %v, want the deadline", err)
	}
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Error("the work was cancelled with its waiter")
	}
}

// In flight for exactly as long as the work, so that a status read while it is
// false is the outcome the work wrote rather than the one it found.
func TestInFlightLastsAsLongAsTheStart(t *testing.T) {
	g := NewWakeGroup()
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = g.Do(ctx, "app", func() error { <-release; return nil })

	if !g.InFlight("app") {
		t.Fatal("a running start is not in flight")
	}
	if g.InFlight("other") {
		t.Error("an app nobody is starting is in flight")
	}
	close(release)
	_ = g.Do(context.Background(), "app", func() error { return nil })
	if g.InFlight("app") {
		t.Error("a finished start is still in flight")
	}
}

// A restart whose hold runs out while an older start is still going is owed
// its own start all the same. Returning on the hold dropped it: the restart
// answered 202, never ran, and `toft restart --wait` settled on the older
// start's outcome (review #4).
func TestARestartThatStopsWaitingStillRunsAfterTheOlderStart(t *testing.T) {
	g := NewWakeGroup()
	first := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = g.Do(context.Background(), "app-1", func() error {
			close(started)
			<-first
			return nil
		})
	}()
	<-started

	ran := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := g.DoAfter(ctx, "app-1", time.Now(), func() error {
		close(ran)
		return nil
	})
	if err == nil {
		t.Fatal("the restart's wait returned nil while the older start was still going")
	}

	if !g.InFlight("app-1") {
		t.Fatal("InFlight is false with the restart's start still owed")
	}
	close(first)
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the restart's own start never ran once the older one finished")
	}
}

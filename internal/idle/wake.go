package idle

import (
	"context"
	"sync"
	"time"
)

// WakeGroup collapses concurrent wakes of the same app into one call.
//
// A sleeping app that someone links to gets a burst of requests — the page and
// every asset on it — and each one arrives to find the app asleep. Without this
// they would all call Restart, and the app would bounce once per request.
//
// This is deliberately a few lines rather than golang.org/x/sync/singleflight:
// that module is not a direct dependency today, and one call site is a thin
// reason to make it one.
//
// The work outlives the waiters. A visitor who gives up (or whose browser
// cancels) does not cancel the wake — their next refresh should find a running
// app, not a wake that was abandoned halfway.
type WakeGroup struct {
	mu       sync.Mutex
	inflight map[string]*wakeCall
	// queued counts callers waiting out an older call before starting their
	// own, so InFlight stays true across the moment between the two.
	queued map[string]int
}

type wakeCall struct {
	done chan struct{}
	// started is when this call began, and it is what lets a caller refuse to
	// be satisfied by work older than their request. See DoAfter.
	started time.Time
	err     error
}

func NewWakeGroup() *WakeGroup {
	return &WakeGroup{inflight: make(map[string]*wakeCall)}
}

// Do runs fn for key unless it is already running, and waits for the result or
// for ctx to end, whichever comes first. Every caller waiting on the same key
// gets the same error.
//
// A ctx that ends first returns ctx.Err() while fn keeps running.
func (g *WakeGroup) Do(ctx context.Context, key string, fn func() error) error {
	return g.do(ctx, key, time.Time{}, fn)
}

// DoAfter is Do for a caller who must not be satisfied by work that began
// before they asked.
//
// **Collapsing is right for a burst and wrong for an instruction.** A sleeping
// app linked to in a chat message is fetched by twenty requests that all mean
// the same thing, and one wake is the correct answer to all of them. A person
// running `toft restart` after an administrator has just attached storage means
// something else entirely: *now*, with what is true now. Joining a start that
// assembled its workload description before the mapping existed answered them
// with a pod that has no volumes — and answered exit 0, because the older call
// genuinely succeeded. The developer then sees an app serving with nothing
// mounted and no way to tell it from an administrator who has not done the
// work yet.
//
// So a caller passes the moment their request arrived, and a call older than
// that is waited out rather than joined. Two restarts a second apart run twice,
// in order, which is what was asked for both times.
func (g *WakeGroup) DoAfter(ctx context.Context, key string, since time.Time, fn func() error) error {
	return g.do(ctx, key, since, fn)
}

// do joins an in-flight call only when it began after `after`. The zero time
// joins anything, which is Do.
func (g *WakeGroup) do(ctx context.Context, key string, after time.Time, fn func() error) error {
	return g.run(ctx, key, after, fn, false)
}

// run is do's loop. `queued` says this caller is already counted in g.queued —
// waiting out an older call — and owes that count back when it stops waiting.
func (g *WakeGroup) run(ctx context.Context, key string, after time.Time, fn func() error, queued bool) error {
	for {
		g.mu.Lock()
		if g.inflight == nil {
			g.inflight = make(map[string]*wakeCall)
		}
		if g.queued == nil {
			g.queued = make(map[string]int)
		}
		c, running := g.inflight[key]
		if running && !c.started.After(after) {
			// Too old to answer this caller. Wait for it to finish and look
			// again rather than starting a second one beside it: two starts of
			// one workload at once is the thing this type exists to prevent,
			// and it stays prevented here.
			if !queued {
				queued = true
				g.queued[key]++
			}
			g.mu.Unlock()
			select {
			case <-c.done:
				continue
			case <-ctx.Done():
				// The caller stops waiting; their start does not stop being
				// owed. Returning here used to drop it — the restart answered
				// 202, its own start never ran, and `--wait` settled on the
				// older start's outcome (review #4). The work outlives the
				// waiter, as it does once it has begun, and the queued count
				// goes with it so InFlight never reads false in between.
				go func() { _ = g.run(context.Background(), key, after, fn, true) }()
				return ctx.Err()
			}
		}
		if queued {
			queued = false
			if g.queued[key]--; g.queued[key] <= 0 {
				delete(g.queued, key)
			}
		}
		if !running {
			c = &wakeCall{done: make(chan struct{}), started: time.Now()}
			g.inflight[key] = c
			go func() {
				defer func() {
					g.mu.Lock()
					delete(g.inflight, key)
					g.mu.Unlock()
					close(c.done)
				}()
				c.err = fn()
			}()
		}
		g.mu.Unlock()

		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// InFlight reports whether a start of key is running now.
//
// It is what lets `toft restart --wait` tell *this restart finished* from *the
// app's row still says what it said before*: a restart past the hold answers
// 202 and leaves the status alone until the start writes its outcome, so a
// running app read `running` the moment the CLI first looked — while the old
// process, on a slow start, was still the one serving.
func (g *WakeGroup) InFlight(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, running := g.inflight[key]
	return running || g.queued[key] > 0
}

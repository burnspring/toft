package schedule

import (
	"sync"
	"time"
)

// TickInterval is how often both schedulers look for work that is due.
//
// One figure, because two were two answers to one question: the task scheduler
// ticked at 30s and the job scheduler at 20s, and neither number was defending
// anything — a 5-field expression has minute granularity, so any interval well
// under a minute fires on time. The finer of the two is kept so that nothing
// got *later* when they were merged (ADR §33).
//
// This bounds lateness, not accuracy: a schedule due at 09:00:00 fires within
// one tick of it, and its *next* firing is computed from now rather than from
// the time it was due, so lateness never accumulates.
const TickInterval = 20 * time.Second

// ClaimLimit is how many schedules one node takes in one pass, for either kind.
//
// One figure for the same reason as the interval: "how much of the estate does
// this node claim before firing any of it" is one question. A node that finds
// more than this due is behind, and the rest go on the next tick rather than in
// one burst that holds locks while it dispatches. The task scheduler's number,
// because the job scheduler's 100 was a ceiling nobody had reached.
const ClaimLimit = 20

// Loop runs fn every `every` until stop is closed, and accounts for itself in
// wg so a caller's Close can wait for the tick in flight.
//
// Both schedulers used to grow their own version of this, and the task one had
// no stop channel at all — it ran for the life of the process, which meant a
// test could start it and never get it back, and a shutdown could not tell a
// tick in flight from one that had finished. A loop that cannot be stopped is
// not a smaller loop; it is one with a missing case.
func Loop(stop <-chan struct{}, wg *sync.WaitGroup, every time.Duration, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				fn()
			case <-stop:
				return
			}
		}
	}()
}

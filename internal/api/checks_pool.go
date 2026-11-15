package api

import (
	"database/sql"
	"sync"
	"time"
)

// poolWindow is how far back the Database pool check looks for waits.
//
// database/sql's wait counters run from process start and never reset, so a
// check that read them raw warned for the rest of the process's life after one
// busy burst — an idle pool reported as a slow install. The check asks about
// the last fifteen minutes instead: long enough that a burst is still on the
// page when somebody opens it to find out why things were slow, short enough
// that it clears by itself once the load has gone.
const poolWindow = 15 * time.Minute

// poolSampleEvery is the sampler's tick. A minute is the window's resolution.
const poolSampleEvery = time.Minute

// poolWatch remembers the pool's wait counters over the last poolWindow, so the
// check can subtract the oldest from the live ones. The zero value is usable,
// and with no sampler running it has no samples and the check falls back to
// the counters since start — which is what a process younger than a minute
// should report anyway.
type poolWatch struct {
	mu      sync.Mutex
	samples []poolSample // oldest first
}

type poolSample struct {
	at    time.Time
	count int64
	wait  time.Duration
}

func (w *poolWatch) record(now time.Time, st sql.DBStats) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, poolSample{now, st.WaitCount, st.WaitDuration})
	cut := 0
	for cut < len(w.samples) && now.Sub(w.samples[cut].at) > poolWindow {
		cut++
	}
	w.samples = w.samples[cut:]
}

// recent is the waits since the oldest sample inside the window, and how long
// ago that sample was taken; with none, the counters since start and a zero
// span.
func (w *poolWatch) recent(now time.Time, st sql.DBStats) (count int64, wait, span time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.samples {
		if now.Sub(s.at) <= poolWindow {
			return st.WaitCount - s.count, st.WaitDuration - s.wait, now.Sub(s.at)
		}
	}
	return st.WaitCount, st.WaitDuration, 0
}

// WatchPool samples the database pool's wait counters every minute, for the
// Database pool check. It runs for the life of the process.
func (s *Server) WatchPool() {
	sample := func() { s.pool.record(time.Now(), s.db.Conn().SQL().Stats()) }
	sample()
	for range time.Tick(poolSampleEvery) {
		sample()
	}
}

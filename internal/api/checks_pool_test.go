package api

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// One busy burst used to leave the Database pool check warning for the rest of
// the process's life: database/sql's wait counters never reset. The check now
// asks about the window, so the same burst warns while it is recent and clears
// once it has aged out.
func TestABurstOfPoolWaitsClearsOnceItLeavesTheWindow(t *testing.T) {
	var w poolWatch
	t0 := time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)
	quiet := sql.DBStats{}
	burst := sql.DBStats{WaitCount: 120, WaitDuration: 14472 * time.Millisecond}

	w.record(t0, quiet)
	w.record(t0.Add(time.Minute), burst) // the e2e run, in the minute after start

	got := recentWaits(w.recent(t0.Add(2*time.Minute), burst))
	if !strings.Contains(got, "120 waits in the last 2 minutes") {
		t.Fatalf("a burst two minutes ago should warn, got %q", got)
	}

	// An hour of quiet sampling: the counters stay where the burst left them.
	for m := 2; m <= 62; m++ {
		w.record(t0.Add(time.Duration(m)*time.Minute), burst)
	}
	if got := recentWaits(w.recent(t0.Add(62*time.Minute), burst)); got != "" {
		t.Errorf("an hour after the burst the pool is idle, and the check still warns: %q", got)
	}
	if len(w.samples) > int(poolWindow/poolSampleEvery)+1 {
		t.Errorf("the watch keeps %d samples; it should keep the window and no more", len(w.samples))
	}
}

// With no sampler (a process younger than a minute, or a test server), the
// counters since start are the honest answer, and the sentence says so.
func TestWithNoSamplesThePoolCheckFallsBackToSinceStart(t *testing.T) {
	var w poolWatch
	st := sql.DBStats{WaitCount: 3, WaitDuration: 2 * time.Second}
	got := recentWaits(w.recent(time.Now(), st))
	if !strings.HasSuffix(got, "since the platform started.") {
		t.Errorf("got %q", got)
	}
	if recentWaits(w.recent(time.Now(), sql.DBStats{WaitCount: 5, WaitDuration: time.Second / 2})) != "" {
		t.Error("half a second of waiting in total is not a warning")
	}
}

// A drills fan-out queued 230 times in fourteen minutes for 2.7 s in all — 12 ms
// a wait — and the check warned for ten minutes that every page was slow. Many
// short waits are a busy pool, not a slow install; the same total in waits a
// person would notice still warns.
func TestManyShortPoolWaitsAreNotAWarning(t *testing.T) {
	var w poolWatch
	short := sql.DBStats{WaitCount: 230, WaitDuration: 2714 * time.Millisecond}
	if got := recentWaits(w.recent(time.Now(), short)); got != "" {
		t.Errorf("230 waits of 12 ms each warned: %q", got)
	}
	long := sql.DBStats{WaitCount: 20, WaitDuration: 2714 * time.Millisecond}
	if got := recentWaits(w.recent(time.Now(), long)); got == "" {
		t.Error("20 waits of 136 ms each should warn")
	}
}

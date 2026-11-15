// Package idle sleeps apps nobody is using and wakes them on the next request.
//
// An app deployed in shared mode holds a process, its port, and its memory for
// as long as it exists, whether or not anyone has opened it since the deploy.
// The reaper here stops such a process after an administrator-set timeout and
// marks the app `sleeping`; the proxy wakes it — a start through the agent,
// roughly a second or two — the next time someone asks for it.
//
// Two properties are load-bearing rather than incidental:
//
//   - Activity lives in memory, never in a row. Stamping a last_active_at on
//     every proxied request would put a database write on the app request path,
//     which is the one thing that path may not do. The cost is that a platform
//     restart forgets who was busy — and the tracker turns that into the safe
//     answer rather than the dangerous one: an app it has never seen is not
//     idle, so a restart hands every app a full fresh grace period instead of
//     reaping the estate on the first sweep.
//
//   - In-flight requests hold an app awake for as long as they last. The proxy
//     streams (FlushInterval: -1) precisely so SSE and long downloads work, and
//     a response that takes an hour is activity even though its last *arrival*
//     was an hour ago. Begin/End counts connections, not just touches.
//
//   - The clock belongs to the workload, not to the app. An entry outlives the
//     process it was measuring — a crash, a restart and a deploy all leave it
//     where it was — so a workload that has just started begins already past
//     every threshold unless something says otherwise. Started is what says
//     otherwise, and the reaper calls it from the transition it can see.
//
// Isolated workloads are deliberately out of scope: waking one is a pod
// schedule and an image pull, so the visitor who triggered it would time out
// rather than get their page. The reaper never touches them.
package idle

import (
	"sync"
	"time"
)

// Tracker records per-app activity for the reaper. Safe for concurrent use;
// every method is O(1) under one mutex except Retain, which the reaper calls
// once a sweep.
//
// A nil *Tracker is usable and inert, so a Server built without one (every
// test that does not care about sleeping) needs no nil checks at the call
// sites.
type Tracker struct {
	// now is time.Now except in tests.
	now func() time.Time

	mu   sync.Mutex
	apps map[string]*entry
}

type entry struct {
	lastAccess time.Time
	// active counts proxied requests currently in flight. A streaming response
	// keeps this above zero for its whole life, which is what stops the reaper
	// killing an app mid-response.
	active int
}

func New() *Tracker {
	return &Tracker{now: time.Now, apps: make(map[string]*entry)}
}

// Touch records that a request for appID just arrived. Called after
// authorization, so probes and refused requests never keep an app awake.
func (t *Tracker) Touch(appID string) {
	if t == nil || appID == "" {
		return
	}
	now := t.now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(appID).lastAccess = now
}

// Started records that a workload for appID has just started, which restarts
// its idle clock.
//
// A workload that has just started has had no chance to be asked for. Its
// entry, though, is whatever the *previous* workload left behind: the tracker
// is keyed by app and survives a crash, a restart and a deploy, so an app
// nobody had visited for an hour before it died comes back up already past
// every threshold and is reaped by the next sweep — seconds after somebody
// pressed Restart, and under a sentence ("after N minutes without a request")
// that is then false about the process it just stopped.
//
// So the clock starts no earlier than the workload does. This is the same safe
// answer IdleSince gives an app it has never seen, for the same reason: no
// evidence either way is not evidence of idleness.
//
// It is the reaper that calls this, from the transition it can see — see
// Reaper.tick.
func (t *Tracker) Started(appID string) {
	if t == nil || appID == "" {
		return
	}
	now := t.now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(appID).lastAccess = now
}

// StartedAt is Started for a start the reaper learns of after the fact: the
// clock moves forward to `at` and never back. A release that took longer to
// come up than a sweep's interval is replaced while the row reads `running`
// throughout, so the transition Started relies on is never seen; the row's
// own timestamp of the flip is what says it happened.
func (t *Tracker) StartedAt(appID string, at time.Time) {
	if t == nil || appID == "" || at.IsZero() {
		return
	}
	at = at.UTC()

	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.entryLocked(appID); e.lastAccess.Before(at) {
		e.lastAccess = at
	}
}

// Begin marks a request as in flight and returns the function that ends it.
// The returned closure is safe to call exactly once, from a defer.
func (t *Tracker) Begin(appID string) func() {
	if t == nil || appID == "" {
		return func() {}
	}

	t.mu.Lock()
	e := t.entryLocked(appID)
	e.active++
	e.lastAccess = t.now().UTC()
	t.mu.Unlock()

	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if e.active > 0 {
			e.active--
		}
		// Stamp on the way out too: a response that streamed for an hour was
		// activity for that hour, not activity an hour ago.
		e.lastAccess = t.now().UTC()
	}
}

// BeginQuiet is Begin for a caller that must not count as activity: the
// request is held in flight, so the reaper cannot stop the workload underneath
// it, and no timestamp is stamped either on the way in or on the way out.
//
// It exists for the reachability prober, whose whole purpose is to make an
// anonymous request from outside and whose presence must therefore leave no
// trace in the counters that decide whether an app is in use. Begin stamps at
// both ends deliberately; a probe every hour, forever, would stamp an app awake
// for as long as the install exists.
//
// An app with no entry yet gets one, so that "in flight" is answerable, and its
// lastAccess is seeded at now for the same reason Begin's entryLocked does —
// an entry that claimed the app had been idle since the zero time would hand
// the reaper an app it had never been told about.
func (t *Tracker) BeginQuiet(appID string) func() {
	if t == nil || appID == "" {
		return func() {}
	}

	t.mu.Lock()
	e, seen := t.apps[appID]
	if !seen {
		// Seeded at now, exactly as IdleSince seeds an app it has never been
		// told about, so that a probe arriving before any real traffic — after
		// a restart, say — cannot make an app reapable that would otherwise
		// have had its full grace period. A probe must leave the answer where
		// it found it, in both directions.
		e = &entry{lastAccess: t.now().UTC()}
		t.apps[appID] = e
	}
	e.active++
	t.mu.Unlock()

	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if e.active > 0 {
			e.active--
		}
	}
}

// IdleSince reports whether appID has been untouched for at least d, with no
// request in flight.
//
// An app with no entry is reported busy and seeded at now. That is what makes a
// restart safe — see the package comment — and it is why this must be the only
// question the reaper asks.
func (t *Tracker) IdleSince(appID string, now time.Time, d time.Duration) bool {
	if t == nil || appID == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.apps[appID]
	if !ok {
		t.apps[appID] = &entry{lastAccess: now.UTC()}
		return false
	}
	if e.active > 0 {
		return false
	}
	return now.UTC().Sub(e.lastAccess) >= d
}

// Retain drops entries for apps that no longer exist. Called by the reaper with
// the app list it already holds, so a deleted app's entry does not outlive it.
func (t *Tracker) Retain(appIDs []string) {
	if t == nil {
		return
	}
	live := make(map[string]struct{}, len(appIDs))
	for _, id := range appIDs {
		live[id] = struct{}{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.apps {
		if _, ok := live[id]; !ok {
			delete(t.apps, id)
		}
	}
}

func (t *Tracker) entryLocked(appID string) *entry {
	e := t.apps[appID]
	if e == nil {
		e = &entry{}
		t.apps[appID] = e
	}
	return e
}

// Package presence answers "who is using this install right now" from memory
// only.
//
// Sessions are stateless JWTs, so there is no session table to count, and the
// only durable record of activity — app_usage_daily — is bucketed by UTC day,
// which can say "active today" but never "online now". This tracker fills that
// gap without breaking the rule it would be easiest to break here: no database
// write on the request path. A touch is a map assignment under a mutex, so both
// the apps gateway and the platform API can stamp every authenticated request.
//
// Two consequences are properties of the design, not bugs to fix later:
//
//   - It is node-local. Each process knows who talked to *it*, exactly like the
//     host CPU and memory readings the admin pages already show. On a
//     multi-replica install a caller sees one node's view, and the API says
//     which node answered so the UI can say so too.
//   - It resets on restart. Presence is a statement about the last few minutes;
//     after a restart the truthful answer is "nobody yet", which is what an
//     empty tracker reports.
//
// Anonymous app traffic is deliberately absent: presence names people, and an
// anonymous viewer has no identity to name. It is still counted as usage.
package presence

import (
	"sort"
	"sync"
	"time"
)

// Retention is how long a touch is remembered. Longer than any window a caller
// is likely to ask for, so the pruner never eats an answer, and short enough
// that the map stays the size of "people who used the platform recently".
const Retention = 30 * time.Minute

// DefaultWindow is what "online now" means when a caller does not say: long
// enough to survive a person reading a page without clicking, short enough that
// someone who closed their laptop drops off while an operator is still looking.
const DefaultWindow = 5 * time.Minute

// pruneInterval bounds how often a touch pays for cleanup. Without it, a
// tracker nobody queries would hold every user id it ever saw.
const pruneInterval = time.Minute

// Entry is one person's last activity, as seen by this node.
type Entry struct {
	UserID   string
	LastSeen time.Time
	// AppID is the app whose proxy this person last went through, and "" for
	// someone who has only used Toft's own UI. AppSeen is when — kept
	// separate from LastSeen so that browsing the platform after opening an app
	// does not make the app look freshly used.
	AppID   string
	AppSeen time.Time
}

// Tracker records the last time each user was seen. Safe for concurrent use;
// every exported method takes the same lock, and each one is O(1) except the
// periodic prune and Active's copy-and-sort.
type Tracker struct {
	// now is time.Now except in tests.
	now func() time.Time

	mu        sync.Mutex
	seen      map[string]*Entry
	lastPrune time.Time
}

func New() *Tracker {
	return &Tracker{now: time.Now, seen: make(map[string]*Entry)}
}

// Touch records that userID was just active. appID names the app they went
// through, or "" for activity against Toft itself. An empty userID is
// ignored: anonymous traffic is usage, not presence.
func (t *Tracker) Touch(userID, appID string) {
	if t == nil || userID == "" {
		return
	}
	now := t.now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()

	e := t.seen[userID]
	if e == nil {
		e = &Entry{UserID: userID}
		t.seen[userID] = e
	}
	e.LastSeen = now
	if appID != "" {
		e.AppID, e.AppSeen = appID, now
	}

	// Piggyback cleanup on a lock we already hold rather than running a
	// goroutine for the whole process lifetime.
	if now.Sub(t.lastPrune) >= pruneInterval {
		t.pruneLocked(now)
	}
}

// Active returns everyone seen within window, most recent first. A window of
// zero or less means DefaultWindow, and anything longer than Retention is
// clamped: reporting a 24-hour window from a 30-minute memory would understate
// it as confidently as it understates itself.
func (t *Tracker) Active(window time.Duration) []Entry {
	if t == nil {
		return nil
	}
	if window <= 0 {
		window = DefaultWindow
	}
	if window > Retention {
		window = Retention
	}

	now := t.now().UTC()
	cutoff := now.Add(-window)

	t.mu.Lock()
	out := make([]Entry, 0, len(t.seen))
	for _, e := range t.seen {
		if e.LastSeen.After(cutoff) {
			out = append(out, *e)
		}
	}
	t.pruneLocked(now)
	t.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].UserID < out[j].UserID
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// Count is Active's length without the copy, for a headcount tile.
func (t *Tracker) Count(window time.Duration) int {
	return len(t.Active(window))
}

func (t *Tracker) pruneLocked(now time.Time) {
	cutoff := now.Add(-Retention)
	for id, e := range t.seen {
		if e.LastSeen.Before(cutoff) {
			delete(t.seen, id)
		}
	}
	t.lastPrune = now
}

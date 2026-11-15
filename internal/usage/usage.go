// Package usage buffers per-request usage counts in memory and flushes them to
// the database on an interval, so the apps gateway never pays a database write
// on the request path.
package usage

import (
	"log"
	"sort"
	"sync"
	"time"

	"github.com/axiomhq/hyperloglog"

	"toft/internal/database"
)

// Store is the flush sink; implemented by *database.DB.
type Store interface {
	BumpAppUsage(rows []database.UsageBump) error
	BumpAPIKeyUsage(rows []database.APIKeyUsageBump) error
	BumpCapabilityUsage(rows []database.CapabilityBump) error
	MergeAudienceSketches(rows []database.AudienceSketch) error
}

const flushInterval = 10 * time.Second

type Recorder struct {
	store Store

	mu     sync.Mutex
	counts map[bumpKey]counter
	// keyCounts is the per-credential counter, kept separate from counts
	// because the two answer different questions of the same request: counts
	// says who used an app, keyCounts says what a token is doing. One request
	// can bump both, and the totals still reconcile because nothing reads them
	// as one number.
	keyCounts map[keyBumpKey]int64
	// capCounts answers "which apps are using what they were granted" — the
	// governance question that would otherwise want an audit row per call, on a
	// route an app hits on every Streamlit rerun. Same reason it lives here as
	// the other two: the answer is worth having and is not worth a database
	// write on the request path.
	capCounts map[capBumpKey]int64
	// sketches is the anonymous audience: per (app, UTC day), the visitor ids
	// whose cookie came back, as a HyperLogLog sketch. The id goes into the
	// sketch and nowhere else — not a map key, not a log line — so what this
	// process holds is already what the database will: an estimate of how
	// many, never which.
	sketches map[sketchKey]*hyperloglog.Sketch
	once     sync.Once
}

type sketchKey struct {
	appID string
	day   string // YYYY-MM-DD, UTC
}

// counter is one (app, viewer, day) row's two figures. Page loads ride the
// same key as requests rather than a key of their own, so classifying a
// request splits no counter and adds no row.
type counter struct {
	requests  int64
	pageLoads int64
}

type bumpKey struct {
	appID  string
	userID string
	// keyOwner is the person a key: row's key belonged to, so the row still
	// names them after the key or the account is gone. Constant per key, so
	// it splits no counter.
	keyOwner string
	day      string // YYYY-MM-DD, UTC
}

type capBumpKey struct {
	appID      string
	capability string
	day        string // YYYY-MM-DD, UTC
}

type keyBumpKey struct {
	keyID       string
	kind        string // database.KeyKindUser | KeyKindApp
	appID       string // "" = a platform API call
	day         string // YYYY-MM-DD, UTC
	statusClass int
}

func NewRecorder(store Store) *Recorder {
	return &Recorder{
		store:     store,
		counts:    make(map[bumpKey]counter),
		keyCounts: make(map[keyBumpKey]int64),
		capCounts: make(map[capBumpKey]int64),
		sketches:  make(map[sketchKey]*hyperloglog.Sketch),
	}
}

// Record counts one request against appID by userID ("" for anonymous). For a
// key, userID is "key:{id}" and keyOwner the account it belongs to; "" for
// anyone else. pageLoad says the request was a top-level document navigation
// (the gateway's isPageLoad), the unit a person reading an app produces once
// per page however chatty its framework is. The flush loop starts lazily on
// first use, so idle Recorders (tests) cost nothing.
func (r *Recorder) Record(appID, userID, keyOwner string, pageLoad bool) {
	r.once.Do(func() { go r.loop() })

	k := bumpKey{appID: appID, userID: userID, keyOwner: keyOwner, day: time.Now().UTC().Format("2006-01-02")}
	r.mu.Lock()
	c := r.counts[k]
	c.requests++
	if pageLoad {
		c.pageLoads++
	}
	r.counts[k] = c
	r.mu.Unlock()
}

// RecordVisitor adds one returning anonymous browser's visitor id to appID's
// sketch for today. Any request counts — the stylesheet a page pulled is the
// return — and the same id any number of times is one browser.
func (r *Recorder) RecordVisitor(appID, visitorID string) {
	r.once.Do(func() { go r.loop() })

	k := sketchKey{appID: appID, day: time.Now().UTC().Format("2006-01-02")}
	r.mu.Lock()
	sk := r.sketches[k]
	if sk == nil {
		sk = database.NewAudienceSketch()
		r.sketches[k] = sk
	}
	sk.Insert([]byte(visitorID))
	r.mu.Unlock()
}

// RecordKey counts one request against an API key, split by outcome. appID is
// "" for a platform API call and the app's id for a gateway call.
//
// status is the full HTTP status; only its leading digit is kept. Storing 2, 4
// and 5 rather than 200, 404 and 503 is what keeps the row count bounded at
// roughly three per key per day per target — enough to answer "is my
// automation failing", cheap enough to keep for a year.
func (r *Recorder) RecordKey(keyID, kind, appID string, status int) {
	r.once.Do(func() { go r.loop() })

	class := status / 100
	if class < 2 || class > 5 {
		class = 2 // 1xx never terminates a request here; anything odd reads as served
	}
	k := keyBumpKey{
		keyID: keyID, kind: kind, appID: appID,
		day:         time.Now().UTC().Format("2006-01-02"),
		statusClass: class,
	}
	r.mu.Lock()
	r.keyCounts[k]++
	r.mu.Unlock()
}

// RecordCapability counts one use of a named capability by an app. No viewer is
// recorded and none is wanted: the question is what an app is doing with what an
// administrator granted it, not who it did it to.
func (r *Recorder) RecordCapability(appID, capability string) {
	r.once.Do(func() { go r.loop() })

	k := capBumpKey{appID: appID, capability: capability, day: time.Now().UTC().Format("2006-01-02")}
	r.mu.Lock()
	r.capCounts[k]++
	r.mu.Unlock()
}

func (r *Recorder) loop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for range t.C {
		r.Flush()
	}
}

// Flush writes and clears the buffered counters. On error the batch is dropped
// rather than retried — usage stats are best-effort, and unbounded retry would
// grow memory for the whole length of a database outage.
func (r *Recorder) Flush() {
	r.mu.Lock()
	if len(r.counts) == 0 && len(r.keyCounts) == 0 && len(r.capCounts) == 0 && len(r.sketches) == 0 {
		r.mu.Unlock()
		return
	}
	counts, keyCounts, capCounts, sketches := r.counts, r.keyCounts, r.capCounts, r.sketches
	r.sketches = make(map[sketchKey]*hyperloglog.Sketch)
	r.counts = make(map[bumpKey]counter)
	r.keyCounts = make(map[keyBumpKey]int64)
	r.capCounts = make(map[capBumpKey]int64)
	r.mu.Unlock()

	rows := make([]database.UsageBump, 0, len(counts))
	for k, c := range counts {
		day, err := time.Parse("2006-01-02", k.day)
		if err != nil {
			continue
		}
		rows = append(rows, database.UsageBump{AppID: k.appID, UserID: k.userID, KeyOwnerID: k.keyOwner, Day: day,
			Requests: c.requests, PageLoads: c.pageLoads})
	}
	if len(rows) > 0 {
		if err := r.store.BumpAppUsage(rows); err != nil {
			log.Printf("usage: dropping %d rows after flush error: %v", len(rows), err)
		}
	}

	// Written second and independently: a failure of one batch must not
	// discard the other, and neither is retried — an unbounded retry queue
	// would grow for the whole length of a database outage.
	keyRows := make([]database.APIKeyUsageBump, 0, len(keyCounts))
	for k, n := range keyCounts {
		day, err := time.Parse("2006-01-02", k.day)
		if err != nil {
			continue
		}
		keyRows = append(keyRows, database.APIKeyUsageBump{
			KeyID: k.keyID, KeyKind: k.kind, Day: day, AppID: k.appID,
			StatusClass: k.statusClass, Requests: n,
		})
	}
	if len(keyRows) > 0 {
		if err := r.store.BumpAPIKeyUsage(keyRows); err != nil {
			log.Printf("usage: dropping %d api-key rows after flush error: %v", len(keyRows), err)
		}
	}

	// Third batch, independent of the first two for the same reason they are
	// independent of each other.
	capRows := make([]database.CapabilityBump, 0, len(capCounts))
	for k, n := range capCounts {
		day, err := time.Parse("2006-01-02", k.day)
		if err != nil {
			continue
		}
		capRows = append(capRows, database.CapabilityBump{
			AppID: k.appID, Capability: k.capability, Day: day, Requests: n,
		})
	}
	if len(capRows) > 0 {
		if err := r.store.BumpCapabilityUsage(capRows); err != nil {
			log.Printf("usage: dropping %d capability rows after flush error: %v", len(capRows), err)
		}
	}

	// Fourth, independent of the rest for the same reason. A sketch cannot be
	// added to a row the way a counter can, so the store merges each one under
	// a row lock; a failure drops the batch and the day undercounts.
	sketchRows := make([]database.AudienceSketch, 0, len(sketches))
	for k, sk := range sketches {
		day, err := time.Parse("2006-01-02", k.day)
		if err != nil {
			continue
		}
		b, err := sk.MarshalBinary()
		if err != nil {
			continue
		}
		sketchRows = append(sketchRows, database.AudienceSketch{AppID: k.appID, Day: day, Sketch: b})
	}
	// In one order everywhere, so two flushes of overlapping keys — the ticker
	// and the shutdown flush, or two replicas — take their row locks in the
	// same sequence and cannot deadlock (code review 2026-11-11, D-2).
	sort.Slice(sketchRows, func(i, j int) bool {
		if sketchRows[i].AppID != sketchRows[j].AppID {
			return sketchRows[i].AppID < sketchRows[j].AppID
		}
		return sketchRows[i].Day.Before(sketchRows[j].Day)
	})
	if len(sketchRows) > 0 {
		if err := r.store.MergeAudienceSketches(sketchRows); err != nil {
			log.Printf("usage: dropping %d audience sketches after flush error: %v", len(sketchRows), err)
		}
	}
}

package telemetry

import (
	"sync"
	"sync/atomic"
)

// OverflowApp is the bucket every app past the cap folds into.
//
// The double underscores are deliberate and match __unmatched__ below: an app id
// is a UUID, so no real value can ever collide with these, and an operator
// reading a dashboard can tell at a glance that a series is a fold rather than
// an app somebody named oddly.
const OverflowApp = "__other__"

// UnmatchedRoute is the http.route value for a request that matched no route.
//
// It is what stops `r.URL.Path` ever reaching a metric attribute, which is the
// single most common way a metrics rollout takes down the monitoring system it
// was supposed to protect: one scanner walking /wp-admin/… for an hour is
// tens of thousands of series that never recur.
const UnmatchedRoute = "__unmatched__"

// appCap bounds the per-app metric dimension.
//
// The first N app ids seen get a series of their own and everything after folds
// into OverflowApp. The fold is *counted* — as toft.telemetry.dropped with
// reason=app_cardinality_cap — because a ceiling that is hit silently is
// indistinguishable from a ceiling that is not, and the first question an
// operator asks about a suspiciously round dashboard is whether they are seeing
// all of it.
//
// N is TOFT_METRIC_APP_CARDINALITY_CAP, 500 by default. See ADR §31 D5 for
// the budget it is part of.
type appCap struct {
	mu    sync.RWMutex
	limit int
	seen  map[string]struct{}
}

// appCardinality is replaced whole by Apply while every request reads it, so
// it is an atomic pointer like the rest of this package's hot state.
var appCardinality atomic.Pointer[appCap]

func init() { appCardinality.Store(newAppCap(0)) }

func newAppCap(limit int) *appCap {
	return &appCap{limit: limit, seen: make(map[string]struct{})}
}

// value returns the attribute value to record for an app id.
//
// The registry never shrinks. An app that is deleted keeps its slot until the
// process restarts, which is the conservative direction: reclaiming a slot
// would let a create-delete loop mint unbounded series over a long uptime, and
// that is precisely the workload a cap exists to survive.
func (c *appCap) value(appID string) string {
	if appID == "" {
		return ""
	}
	c.mu.RLock()
	_, known := c.seen[appID]
	full := len(c.seen) >= c.limit
	c.mu.RUnlock()
	if known {
		return appID
	}
	if full {
		RecordTelemetryDropped(SignalMetrics, DropReasonAppCap)
		return OverflowApp
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-checked under the write lock: two gateway requests for two new apps can
	// both pass the read-lock check, and without this the cap would be exceeded
	// by however many were in flight.
	if _, known := c.seen[appID]; known {
		return appID
	}
	if len(c.seen) >= c.limit {
		RecordTelemetryDropped(SignalMetrics, DropReasonAppCap)
		return OverflowApp
	}
	c.seen[appID] = struct{}{}
	return appID
}

// AppAttr is what every call site outside this package uses to put an app id on
// a metric. Nothing else may construct that attribute: a raw app id on a metric
// is the one unbounded dimension this install actually has.
func AppAttr(appID string) string { return appCardinality.Load().value(appID) }

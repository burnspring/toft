package api

import (
	"encoding/json"
	"net/http"
	"time"

	"toft/internal/builder"
)

// auditActionBuildCacheReclaim follows <subject>.<verb>, the convention every
// existing action uses. Not authorization-relevant, so it is written outside a
// transaction rather than inside the one that made a change: there is no change
// for it to be atomic with, and refusing to reclaim because an audit write
// failed helps nobody.
const auditActionBuildCacheReclaim = "build_cache.reclaim"

// Emptying the shared build cache.
//
// This is a sibling of the checks report, not a check. The distinction is the
// one that keeps that page safe to press during an incident: every check is
// read-only diagnosis, so re-running the report can never reclaim anything, and
// the platform's only destructive maintenance button is a route of its own that
// no check triggers.
//
// What it promises is narrow and true. Nothing that is running is affected —
// releases are self-contained by construction, and no workload spec has ever
// carried CACHE_DIR — and the next build for each app downloads its
// dependencies again. It deliberately does not redeploy anything: a redeploy
// re-resolves, so an app with no lockfile would come back on whatever the index
// serves today rather than the versions its running release was judged against,
// which is the estate's largest blast radius spent on a disk problem.

// buildCacheBlock is what the checks report carries about the cache, and it is
// what the card polls while a reclaim runs.
//
// It rides on the report rather than having a GET of its own because the card
// lives on that page and the page already refetches: a second endpoint would be
// a second thing to keep in step for no fact the first one has not got.
func (s *Server) buildCacheBlock() map[string]any {
	if s.builder == nil || s.builder.CacheDir() == "" {
		return nil
	}
	return buildCacheBlockFor(s.builder.CacheDir(), s.builder.CacheStatus())
}

// buildCacheBlockFor is the wire shape itself, a pure function of the state so
// that every combination it has to keep distinct — never measured, measured and
// empty, reclaiming, reclaimed with skips — can be asserted without a builder
// to hold them.
func buildCacheBlockFor(dir string, st builder.CacheState) map[string]any {
	block := map[string]any{
		"dir": dir,
		// measured is the field every surface has to branch on before it shows
		// a number. Never-measured is not zero.
		"measured":   st.Measured(),
		"reclaiming": st.Reclaiming,
	}
	if st.Measured() {
		block["bytes"] = st.Bytes
		block["measured_at"] = st.MeasuredAt
	}
	if st.Err != "" {
		block["measure_error"] = st.Err
	}
	if st.Reclaiming {
		block["started_at"] = st.StartedAt
	}
	if !st.LastReclaim.IsZero() {
		block["last_freed"] = st.LastFreed
		block["last_reclaim"] = st.LastReclaim
	}
	if st.LastError != "" {
		block["last_error"] = st.LastError
	}
	return block
}

// handleReclaimBuildCache starts a reclaim.
//
// The only thing it refuses is a second reclaim. It does not refuse because a
// build is in flight: the reclaim takes the cache's write side and waits, which
// is safe because every build holds the read side, and bounded because Go's
// RWMutex stops admitting readers once a writer is waiting. "Try again when
// nothing is deploying" is an instruction an administrator on a busy install
// can never satisfy.
func (s *Server) handleReclaimBuildCache(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	if s.builder == nil || s.builder.CacheDir() == "" {
		// Absent, not dormant: an install with no cache directory has nothing
		// to reclaim, and says so rather than pretending to have started.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "This install has no build cache directory configured.", "hint": "nothing to reclaim; the operator sets a build cache directory where this install is configured",
		})
		return
	}

	// The actor is captured here, at request time, and carried into the
	// goroutine. The row is written when the work finishes so that it can say
	// what happened rather than only that somebody asked — an audit row naming
	// a button press and not its outcome is the row you cannot use six weeks
	// later.
	entry := s.auditFor(r, p)

	started, st := s.builder.StartCacheReclaim(func(res builder.ReclaimResult) {
		entry.Action = auditActionBuildCacheReclaim
		entry.TargetType = "system"
		entry.TargetID = "build_cache"
		entry.OccurredAt = time.Now().UTC()
		if b, err := json.Marshal(map[string]any{
			"before_bytes": res.Before,
			"after_bytes":  res.After,
			"freed_bytes":  res.Freed,
			"duration_ms":  res.Duration.Milliseconds(),
			"steps":        res.Steps,
		}); err == nil {
			entry.After = string(b)
		}
		// Outside the request, and its failure must not undo work that has
		// already happened — the same reasoning WriteAuditRow exists for.
		_ = s.db.WriteAuditRow(entry)
	})

	if !started {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "A reclaim is already running.",
			"started_at": st.StartedAt, "hint": "wait for the running reclaim to finish, then try again if there is more to free",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"started": true,
		// Said back so the card can narrate the wait it is about to do rather
		// than only that something is happening.
		"detail": "Reclaiming. This waits for any build in progress to finish.",
	})
}

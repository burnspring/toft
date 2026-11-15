package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
)

// The build cache on the Checks page, and the route that empties it.
//
// Everything here is a pure function of a CacheState or a decision made before
// any database is reached, which is why none of it needs the database-backed
// suite. The reclaim's own semantics — that it waits for an in-flight build,
// that a second one is refused, that a missing tool is skipped — are asserted
// in internal/builder, where they are decided.

// ---------------------------------------------------------------------------
// The size on the checks report.
// ---------------------------------------------------------------------------
//
// The rule these cover is D6's, and it is the one that is easy to break by
// being helpful: the detail reports a measurement *someone else took*. This
// runs inside a five-second check deadline, so the moment it walks a directory
// it becomes a check that fails on installs whose cache is large — precisely
// backwards, and rendered as "Did not answer in 5s" rather than as a number.

func TestTheCacheCheckReportsTheStoredSizeAndItsAge(t *testing.T) {
	got := cacheSizeDetail(builder.CacheState{
		Bytes:      4 << 30,
		MeasuredAt: time.Now().UTC().Add(-4 * time.Minute),
	})
	if !strings.Contains(got, "4.0 GB") {
		t.Errorf("cacheSizeDetail = %q, want it to carry the stored size", got)
	}
	if !strings.Contains(got, "4 minutes ago") {
		t.Errorf("cacheSizeDetail = %q, want it to say how old the reading is. A figure "+
			"with no timestamp invites somebody to empty the cache and conclude it did "+
			"nothing, because the number will not move until the next measurement.", got)
	}
}

func TestTheCacheCheckSaysNotMeasuredRatherThanZero(t *testing.T) {
	got := cacheSizeDetail(builder.CacheState{})
	if !strings.Contains(got, "not measured yet") {
		t.Errorf("cacheSizeDetail of an unmeasured cache = %q, want \"not measured yet\"", got)
	}
	if strings.Contains(got, "Holding") {
		t.Errorf("cacheSizeDetail = %q — an unmeasured cache is being reported as a "+
			"measured one, which is the reading that sends an operator elsewhere", got)
	}
}

func TestAMeasuredEmptyCacheIsNotTheSameAsAnUnmeasuredOne(t *testing.T) {
	empty := cacheSizeDetail(builder.CacheState{Bytes: 0, MeasuredAt: time.Now().UTC()})
	if strings.Contains(empty, "not measured") {
		t.Errorf("a measured, empty cache reads as unmeasured: %q", empty)
	}
	if !strings.Contains(empty, "Holding 0 B") {
		t.Errorf("cacheSizeDetail of a measured empty cache = %q, want it to state 0 B", empty)
	}
}

func TestAFailedMeasurementIsSaidBesideTheStandingFigure(t *testing.T) {
	got := cacheSizeDetail(builder.CacheState{
		Bytes:      1 << 20,
		MeasuredAt: time.Now().UTC().Add(-90 * time.Minute),
		Err:        "context deadline exceeded",
	})
	if !strings.Contains(got, "1.0 MB") || !strings.Contains(got, "an hour ago") {
		t.Errorf("cacheSizeDetail = %q, want the standing figure and its age", got)
	}
	if !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("cacheSizeDetail = %q, want the reason the newest measurement failed", got)
	}
}

func TestOnlyTheCacheDirectoryGetsASize(t *testing.T) {
	s := &Server{}
	for _, id := range []string{"apps_dir", "logs_dir", "artifact_store", "cache_dir"} {
		// No builder is wired, so every id must be silent — including cache_dir,
		// which is the nil-builder guard the checks page depends on.
		if note := s.cacheSizeNote(id); note != "" {
			t.Errorf("cacheSizeNote(%q) with no builder = %q, want empty", id, note)
		}
	}
}

// ---------------------------------------------------------------------------
// The block the card reads.
// ---------------------------------------------------------------------------

func TestTheCacheBlockIsAbsentWithoutACache(t *testing.T) {
	if block := (&Server{}).buildCacheBlock(); block != nil {
		t.Errorf("buildCacheBlock with no builder = %v, want nil so no card is rendered", block)
	}
}

func TestTheCacheBlockCarriesNoSizeUntilItIsMeasured(t *testing.T) {
	block := buildCacheBlockFor("/data/cache", builder.CacheState{})
	if block["measured"] != false {
		t.Errorf("measured = %v, want false", block["measured"])
	}
	if _, ok := block["bytes"]; ok {
		t.Error("an unmeasured cache put a byte count on the wire, so the card has a zero " +
			"to render and no way to know it is meaningless")
	}
	if _, ok := block["measured_at"]; ok {
		t.Error("an unmeasured cache put a measurement time on the wire")
	}
}

func TestTheCacheBlockCarriesTheSizeOnceMeasured(t *testing.T) {
	now := time.Now().UTC()
	block := buildCacheBlockFor("/data/cache", builder.CacheState{Bytes: 2048, MeasuredAt: now})
	if block["measured"] != true || block["bytes"] != int64(2048) {
		t.Errorf("block = %v, want a measured 2048 bytes", block)
	}
	if block["dir"] != "/data/cache" {
		t.Errorf("dir = %v, want the cache directory named", block["dir"])
	}
}

func TestTheCacheBlockSaysWhatAReclaimFreedAndWhatItSkipped(t *testing.T) {
	block := buildCacheBlockFor("/data/cache", builder.CacheState{
		Bytes:       1024,
		MeasuredAt:  time.Now().UTC(),
		LastFreed:   4 << 30,
		LastReclaim: time.Now().UTC(),
		LastError:   "go-mod: Go is not installed on this host.",
	})
	if block["last_freed"] != int64(4<<30) {
		t.Errorf("last_freed = %v, want the figure the reclaim freed", block["last_freed"])
	}
	if block["last_error"] == nil {
		t.Error("a reclaim that skipped a tool said nothing about it, so the card cannot " +
			"explain why less was freed than the administrator expected")
	}
}

func TestAReclaimInFlightIsSaidWithItsStartTime(t *testing.T) {
	started := time.Now().UTC().Add(-2 * time.Minute)
	block := buildCacheBlockFor("/data/cache", builder.CacheState{
		Reclaiming: true, StartedAt: started,
	})
	if block["reclaiming"] != true || block["started_at"] != started {
		t.Errorf("block = %v, want a running reclaim and when it started — the card has to "+
			"say how long the wait has been, not only that there is one", block)
	}
}

// ---------------------------------------------------------------------------
// The route.
// ---------------------------------------------------------------------------

func postReclaim(t *testing.T, s *Server, role string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/system/build-cache/reclaim", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), &auth.Principal{
		ID: "admin-1", SystemRole: role,
	}))
	rr := httptest.NewRecorder()
	s.handleReclaimBuildCache(rr, r)
	return rr
}

// This is the platform's only destructive maintenance verb, and it is refused
// to everyone but an administrator — the same answer its sibling /system/checks
// gives, which is what "like the routes beside it" has to mean here.
func TestReclaimIsAdminOnly(t *testing.T) {
	rr := postReclaim(t, &Server{}, database.SystemRoleViewer)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a non-administrator got %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestReclaimRefusesAnAnonymousCaller(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/system/build-cache/reclaim", nil)
	rr := httptest.NewRecorder()
	(&Server{}).handleReclaimBuildCache(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous caller got %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestReclaimIsAbsentWithoutACacheDirectory(t *testing.T) {
	rr := postReclaim(t, &Server{}, database.SystemRoleAdmin)
	if rr.Code != http.StatusNotFound {
		t.Errorf("an install with no cache directory got %d, want %d — absent, not a "+
			"reclaim that pretends to have started", rr.Code, http.StatusNotFound)
	}
}

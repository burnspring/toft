package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"toft/internal/database"
)

type upcomingBody struct {
	Firings   []upcomingFiring `json:"firings"`
	Recurring []upcomingFiring `json:"recurring"`
	Held      map[string]int   `json:"held"`
}

func upcomingPath(from, to time.Time) string {
	return "/api/v1/system/scheduled/upcoming?from=" + url.QueryEscape(from.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(to.Format(time.RFC3339))
}

func (e *jobEnv) upcoming(t *testing.T, from, to time.Time) upcomingBody {
	t.Helper()
	rec := jobReq(t, e.srv, e.router, "root", http.MethodGet, upcomingPath(from, to), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var b upcomingBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The interval is two instants, the later one at most eight days on, and only
// an administrator may ask: the answer names apps the caller may hold no grant
// on.
func TestUpcomingRefusesABadIntervalAndANonAdmin(t *testing.T) {
	e := newJobEnv(t)
	from := time.Now().UTC().Truncate(time.Hour)
	for _, path := range []string{
		"/api/v1/system/scheduled/upcoming",
		"/api/v1/system/scheduled/upcoming?from=tomorrow&to=later",
		upcomingPath(from, from),
		upcomingPath(from, from.Add(-time.Hour)),
		upcomingPath(from, from.Add(8*24*time.Hour+time.Minute)),
	} {
		if rec := jobReq(t, e.srv, e.router, "root", http.MethodGet, path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, rec.Code)
		}
	}
	if rec := jobReq(t, e.srv, e.router, "root", http.MethodGet, upcomingPath(from, from.Add(8*24*time.Hour)), ""); rec.Code != http.StatusOK {
		t.Errorf("eight days = %d, want 200", rec.Code)
	}
	if rec := jobReq(t, e.srv, e.router, "owner", http.MethodGet, upcomingPath(from, from.Add(time.Hour)), ""); rec.Code != http.StatusForbidden {
		t.Errorf("a publisher = %d, want 403", rec.Code)
	}
}

// Firings in time order, the stored next_run_at first even when it is overdue;
// a frequent expression is one entry; the timeout is the bound.
func TestUpcomingExpandsFromTheStoredFiring(t *testing.T) {
	e := newJobEnv(t)
	now := time.Now().UTC()
	from, to := now.Add(-2*time.Hour), now.Add(22*time.Hour)

	nightly := e.createJob(t, `{"name":"nightly","command":"a","schedule":"0 */6 * * *","timeout_sec":5400}`)
	overdue := now.Add(-time.Hour).Truncate(time.Minute)
	if err := e.srv.db.SetNextRunAt(nightly.ID, &overdue); err != nil {
		t.Fatal(err)
	}
	e.createJob(t, `{"name":"sync","command":"b","schedule":"*/5 * * * *"}`)

	b := e.upcoming(t, from, to)
	if len(b.Firings) < 4 || !b.Firings[0].At.Equal(overdue) || b.Firings[0].Name != "nightly" {
		t.Fatalf("firings = %+v, want the overdue one first", b.Firings)
	}
	for i := 1; i < len(b.Firings); i++ {
		if b.Firings[i].At.Before(b.Firings[i-1].At) {
			t.Fatalf("not in time order: %+v", b.Firings)
		}
	}
	if f := b.Firings[0]; f.UpperSec == nil || *f.UpperSec != 5400 || f.TypicalSec != nil || f.Kind != "job" {
		t.Errorf("first firing = %+v, want no history and a 5400 s bound", f)
	}
	if len(b.Recurring) != 1 || b.Recurring[0].Name != "sync" || b.Recurring[0].GapSec != 300 || b.Recurring[0].Count < 200 {
		t.Errorf("recurring = %+v, want sync every 300 s", b.Recurring)
	}
}

// A paused schedule and one on a locked app fire nothing and are counted.
func TestUpcomingCountsPausedAndLockedSchedules(t *testing.T) {
	e := newJobEnv(t)
	now := time.Now().UTC()
	paused := e.createJob(t, `{"name":"paused","command":"a","schedule":"0 * * * *"}`)
	if err := e.srv.db.SetJobEnabled(e.app.ID, paused.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	other := &database.App{Slug: "held", Name: "Held", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := e.srv.db.CreateAppTx(other, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.db.ReconcileManifestJobs(other.ID, []database.JobDefinition{
		{Name: "locked", Command: "b", Schedule: "0 * * * *"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.db.SetAppLock(other.ID, "root", "review", now, other.Slug, database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	b := e.upcoming(t, now, now.Add(24*time.Hour))
	if len(b.Firings) != 0 || len(b.Recurring) != 0 {
		t.Fatalf("held schedules were drawn: %+v %+v", b.Firings, b.Recurring)
	}
	if b.Held["paused"] != 1 || b.Held["locked"] != 1 {
		t.Fatalf("held = %v, want one of each", b.Held)
	}
}

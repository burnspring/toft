package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"toft/internal/database"
)

// The two readers, and the split between them.
//
// The owner is being asked to fix something and gets the latest reading. The
// administrator is asking a longitudinal question — how long has this been
// true, and did anyone act — which is a statement about response and belongs
// with audit_log and /admin/packages.

func seedScans(t *testing.T, db *database.DB, appID, deployID string, n int) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(n) * time.Hour)
	for i := 0; i < n; i++ {
		scan := &database.DependencyScan{
			AppID: appID, DeploymentID: deployID,
			Trigger:    database.DependencyScanTriggerScheduled,
			OccurredAt: base.Add(time.Duration(i) * time.Hour),
			Scanned:    true, Counted: i, Findings: i,
			Advisories: []string{"GHSA-crit"},
		}
		if err := db.RecordDependencyScan(scan, 30); err != nil {
			t.Fatalf("RecordDependencyScan: %v", err)
		}
	}
}

// **A serialization slip that attached the history to the owner's payload is an
// over-share nothing errors on**, so this asserts the absence by key on one
// response and the presence on the other.
func TestTheOwnerGetsTheLatestReadingAndNeverTheList(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	seedScans(t, f.srv.db, f.app.ID, f.deploy.ID, 4)

	rec := f.send(t, http.MethodGet, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, forbidden := range []string{"scans", "history", "last_scans"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("the owner's payload carries %q; the history is the administrator's", forbidden)
		}
	}
	if _, ok := raw["last_scan"]; !ok {
		t.Fatal("the owner's payload should carry the latest reading")
	}
	var latest database.DependencyScan
	if err := json.Unmarshal(raw["last_scan"], &latest); err != nil {
		t.Fatalf("decoding last_scan: %v", err)
	}
	if latest.Counted != 3 {
		t.Errorf("last_scan.counted = %d, want the newest reading (3)", latest.Counted)
	}
}

// Absent means never checked, which is not zero findings and never renders as
// one.
func TestLastScanIsAbsentBeforeAnythingHasLooked(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	rec := f.send(t, http.MethodGet, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, ok := raw["last_scan"]; ok {
		t.Error("an app nothing has looked at reported a reading")
	}
}

func TestTheHistoryRouteIsTheAdministratorsAlone(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	seedScans(t, f.srv.db, f.app.ID, f.deploy.ID, 3)
	path := "/api/v1/admin/apps/" + f.app.ID + "/dependency-scans"

	// 404 over 403: the existence of a governance history is not a fact an
	// owner needs to discover by probing.
	for _, who := range []string{"owner", "viewer", ""} {
		rec := f.send(t, http.MethodGet, who, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q got %d, want 404", who, rec.Code)
		}
	}

	rec := f.send(t, http.MethodGet, "admin1", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d\n%s", rec.Code, rec.Body.String())
	}
	var out struct {
		AppSlug string                    `json:"app_slug"`
		Kept    int                       `json:"kept"`
		Total   int                       `json:"total"`
		Scans   []database.DependencyScan `json:"scans"`
		LockOn  string                    `json:"lock_on"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Scans) != 3 {
		t.Fatalf("scans = %d, want 3", len(out.Scans))
	}
	// Newest first, so the reader's first line is the current answer.
	if out.Scans[0].Counted != 2 {
		t.Errorf("the list is not newest first: %+v", out.Scans[0])
	}
	if out.Total != 3 || out.Kept == 0 {
		t.Errorf("total=%d kept=%d; the panel has to be able to say 'the last N'", out.Total, out.Kept)
	}
	if out.AppSlug != f.app.Slug {
		t.Errorf("app_slug = %q", out.AppSlug)
	}
	// Malware is the default and the response says so rather than leaving
	// the page to guess what would happen.
	if out.LockOn != database.DependencyLockMalware {
		t.Errorf("lock_on = %q, want malware", out.LockOn)
	}
}

// It takes an id from the URL, so it scopes by that id: an app that is not
// there is a 404 and never somebody else's history.
func TestTheHistoryRouteScopesByTheAppInTheURL(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	rec := f.send(t, http.MethodGet, "admin1",
		"/api/v1/admin/apps/does-not-exist/dependency-scans", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

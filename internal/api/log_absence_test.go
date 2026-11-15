package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/runtime"
)

// decodeLogs reads the tail response, which is the only place the reason is
// carried.
func decodeLogs(t *testing.T, body []byte) logsResponse {
	t.Helper()
	var got logsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode logs response: %v (body %q)", err, string(body))
	}
	return got
}

// A workload that is not there is not a server error, and every situation that
// produces an empty log says which one it is.
//
// The defect this pins down predates the feature it was found in: asked for a
// stopped app's log, the shared backend answered 200 with an empty tail and the
// isolated one answered 500, and the Logs tab rendered both as "No log output
// yet." because it read neither. Three different answers to one question — the
// mock supplied a third — and no test disagreed with any of them.
func TestLogsAbsenceIsATwoHundredWithAReason(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, e *logTestEnv)
		want   string
	}{
		{
			name: "stopped",
			mutate: func(t *testing.T, e *logTestEnv) {
				// UpdateAppStatus, not UpdateApp: status is one of the columns
				// UpdateApp deliberately does not write, so that a build
				// holding a stale App cannot revert a stop somebody made while
				// it was running. Writing it through UpdateApp changes nothing
				// and the app stays running, which is how this case came to
				// assert a reason the row never had.
				if err := e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusStopped); err != nil {
					t.Fatalf("stop app: %v", err)
				}
			},
			want: runtime.LogAbsentStopped,
		},
		{
			// `sleeping` is not `stopped`: one wakes on the next request and
			// the other stays down until a person says otherwise, so the tab
			// must not collapse them into one sentence.
			name: "sleeping",
			mutate: func(t *testing.T, e *logTestEnv) {
				if err := e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusSleeping); err != nil {
					t.Fatalf("sleep app: %v", err)
				}
			},
			want: runtime.LogAbsentSleeping,
		},
		{
			// A lock outranks the workload's own state. The owner asking why
			// their app is quiet is owed the administrator's decision, not
			// "stopped".
			name: "locked",
			mutate: func(t *testing.T, e *logTestEnv) {
				// SetAppLock, not UpdateApp: a lock is deliberately not a field
				// UpdateApp writes, so that an overlapping deploy holding a
				// stale App cannot revert it.
				//
				// The locker is a seeded account rather than a bare string:
				// `apps.locked_by` is a foreign key onto users now, so a lock
				// by nobody-in-particular is refused by the database — which is
				// the point of the column, and was how this fixture broke.
				admin := seedUser(t, e.srv.db, "locking-admin", database.SystemRoleAdmin)
				if err := e.srv.db.SetAppLock(e.app.ID, admin.ID, "", time.Now(), e.app.Slug, database.RequestMeta{}); err != nil {
					t.Fatalf("lock app: %v", err)
				}
			},
			want: runtime.LogAbsentLocked,
		},
		{
			// The backend's answer, not the app row's: the app is running as
			// far as the database is concerned and the process manager has
			// never heard of it.
			name: "no workload under this name",
			mutate: func(t *testing.T, e *logTestEnv) {
				if err := e.srv.rts.Shared().Delete(e.app.ProcessName()); err != nil {
					t.Fatalf("delete mock process: %v", err)
				}
			},
			want: runtime.LogAbsentNoWorkload,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newLogTestEnv(t)
			tc.mutate(t, e)

			rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/logs", e.app.ID))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — an absent workload is not a server error", rec.Code)
			}
			got := decodeLogs(t, rec.Body.Bytes())
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
			if got.Logs != "" {
				t.Errorf("logs = %q, want empty alongside a reason", got.Logs)
			}
		})
	}
}

// An app with a workload answers with output and no reason. The two fields are
// never both set, and this is the case a reason must not appear in: "your app
// is fine" is one of the six situations the old single sentence covered.
func TestLogsWithAWorkloadCarryNoReason(t *testing.T) {
	e := newLogTestEnv(t)

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/logs", e.app.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeLogs(t, rec.Body.Bytes())
	if got.Reason != "" {
		t.Errorf("reason = %q, want empty for a running workload", got.Reason)
	}
	if got.Logs == "" {
		t.Error("logs empty for a registered workload")
	}
}

// An app that has never had a successful deploy says so, rather than borrowing
// the backend's "nothing under this name".
//
// The trap this guards is the shortcut: `apps.current_release` is empty for
// apps written before releases were named, and ProcessName falls back to the
// slug for exactly those. Reading that column as "never deployed" would blank
// the log of the oldest working apps on an install.
func TestLogsNeverDeployedIsAskedOfTheDeploymentHistory(t *testing.T) {
	e := newLogTestEnv(t)

	fresh := &database.App{
		Slug: "undeployed", Name: "Undeployed", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	if err := e.srv.db.CreateApp(fresh); err != nil {
		t.Fatalf("create app: %v", err)
	}

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/logs", fresh.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeLogs(t, rec.Body.Bytes()); got.Reason != runtime.LogAbsentNeverDeployed {
		t.Errorf("reason = %q, want %q", got.Reason, runtime.LogAbsentNeverDeployed)
	}
}

// `lines` is always present on a platform that labels lines — `[]` when there
// is nothing — because its *absence* is what tells a client it is talking to an
// older platform. `toft` reads exactly that to decide whether --source can be
// honoured, so marking the field omitempty made an empty log indistinguishable
// from a platform too old to have the field at all.
func TestLogsAlwaysCarryTheLinesKey(t *testing.T) {
	e := newLogTestEnv(t)
	if err := e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusStopped); err != nil {
		t.Fatalf("stop app: %v", err)
	}

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/logs", e.app.ID))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lines, ok := raw["lines"]
	if !ok {
		t.Fatal(`no "lines" key: a client reads its absence as an older platform`)
	}
	if string(lines) != "[]" {
		t.Errorf(`"lines" = %s, want []`, lines)
	}
}

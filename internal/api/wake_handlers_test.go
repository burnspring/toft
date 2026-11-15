package api

import (
	"net/http"
	"testing"
	"time"

	"toft/internal/database"
)

// The wake ahead of a visit wakes a sleeping app, leaves every other one alone,
// and needs a signed-in reader like the visit it stands in for.
func TestWakeAheadOfAVisitWakesOnlyASleepingApp(t *testing.T) {
	e := newLockEnv(t)
	path := "/api/v1/apps/" + e.app.ID + "/wake"

	if rec := e.do(t, "", http.MethodPost, path, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous wake = %d, want 401", rec.Code)
	}
	if rec := e.do(t, "owner", http.MethodPost, path, ""); rec.Code != http.StatusNoContent {
		t.Errorf("waking a running app = %d, want 204 (nothing to wake)", rec.Code)
	}

	if err := e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusSleeping); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "owner", http.MethodPost, path, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("waking a sleeping app = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		app, err := e.srv.db.GetApp(e.app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if app.Status == database.AppStatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the app is still %q after the wake", app.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

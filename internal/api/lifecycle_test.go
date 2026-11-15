package api

import (
	"encoding/json"
	"testing"
	"time"

	"toft/internal/database"
)

// A scheduled restart, driven through fire against the restart handler's own
// environment: a real builder over a mock backend.

func (e *restartEnv) declareRestart(t *testing.T) database.RestartSchedule {
	t.Helper()
	next := time.Now().UTC().Add(-time.Minute)
	if _, err := e.srv.db.ReconcileManifestSchedules(e.app.ID,
		&database.ManifestSchedule{Schedule: "0 4 * * *", NextRunAt: &next}, nil); err != nil {
		t.Fatal(err)
	}
	sch, err := e.srv.db.GetRestartSchedule(e.app.ID)
	if err != nil || sch == nil {
		t.Fatalf("GetRestartSchedule: %v", err)
	}
	return *sch
}

func (e *restartEnv) setStatus(t *testing.T, status string) {
	t.Helper()
	if err := e.srv.db.UpdateAppStatus(e.app.ID, status); err != nil {
		t.Fatal(err)
	}
}

func (e *restartEnv) outcome(t *testing.T) (string, string) {
	t.Helper()
	sch, _ := e.srv.db.GetRestartSchedule(e.app.ID)
	if sch == nil || sch.LastFiredAt == nil {
		t.Fatalf("no outcome recorded: %+v", sch)
	}
	return sch.LastOutcome, sch.LastReason
}

// A running app is restarted, as the system, and is not forgiven.
func TestAScheduledRestartRestartsARunningAppWithoutForgiving(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	sch := e.declareRestart(t)
	var forgiven []string
	e.srv.forgiven = func(id string) { forgiven = append(forgiven, id) }

	NewRestartScheduler(e.srv, nil).fire(sch)

	if o, r := e.outcome(t); o != database.RestartOutcomeRestarted || r != "" {
		t.Errorf("outcome = %s/%s, want restarted", o, r)
	}
	if len(forgiven) != 0 {
		t.Errorf("a scheduled restart forgave %v; only a person's may", forgiven)
	}
	var actorType, after string
	if err := e.srv.db.Conn().QueryRow(`SELECT actor_type, after FROM audit_log
		WHERE action = 'app.restart' AND target_id = ?`, e.app.ID).Scan(&actorType, &after); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	var a map[string]string
	json.Unmarshal([]byte(after), &a)
	if actorType != "system" || a["via"] != "schedule" {
		t.Errorf("audit = %s %s, want system via schedule", actorType, after)
	}
}

// Every status but running declines, records why, and starts nothing — a
// sleeping app is not woken, a crashed one keeps its verdict.
func TestAScheduledRestartDeclinesEverythingButRunning(t *testing.T) {
	for _, c := range []struct{ status, reason string }{
		{database.AppStatusSleeping, "sleeping"},
		{database.AppStatusStopped, "stopped"},
		{database.AppStatusCrashed, "crashed"},
		{database.AppStatusBuilding, "no_process"},
		{database.AppStatusFailed, "no_process"},
	} {
		t.Run(c.status, func(t *testing.T) {
			e := newRestartEnv(t, startWorks)
			sch := e.declareRestart(t)
			e.setStatus(t, c.status)
			before := e.mockRestarts(t)
			var forgiven int
			e.srv.forgiven = func(string) { forgiven++ }

			NewRestartScheduler(e.srv, nil).fire(sch)

			if o, r := e.outcome(t); o != database.RestartOutcomeSkipped || r != c.reason {
				t.Errorf("outcome = %s/%s, want skipped/%s", o, r, c.reason)
			}
			if got := e.status(t); got != c.status {
				t.Errorf("status = %s, want it left at %s", got, c.status)
			}
			if e.mockRestarts(t) != before || forgiven != 0 {
				t.Errorf("a declined firing started or forgave something")
			}
		})
	}
}

// A lock refuses restarts from anyone, a timetable included.
func TestAScheduledRestartDeclinesALockedApp(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	sch := e.declareRestart(t)
	now := time.Now().UTC()
	if _, err := e.srv.db.Conn().Exec(`UPDATE apps SET locked_at = ? WHERE id = ?`, now, e.app.ID); err != nil {
		t.Fatal(err)
	}
	NewRestartScheduler(e.srv, nil).fire(sch)
	if o, r := e.outcome(t); o != database.RestartOutcomeSkipped || r != "locked" {
		t.Errorf("outcome = %s/%s, want skipped/locked", o, r)
	}
}

// A deploy in flight starts a fresh process anyway.
func TestAScheduledRestartDeclinesWhileADeployIsPending(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	sch := e.declareRestart(t)
	if err := e.srv.db.CreateDeployment(&database.Deployment{AppID: e.app.ID,
		Status: database.DeployStatusPending, TriggeredBy: "manual"}, nil); err != nil {
		t.Fatal(err)
	}
	NewRestartScheduler(e.srv, nil).fire(sch)
	if o, r := e.outcome(t); o != database.RestartOutcomeSkipped || r != "deploy_pending" {
		t.Errorf("outcome = %s/%s, want skipped/deploy_pending", o, r)
	}
}

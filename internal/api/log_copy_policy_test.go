package api

import (
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
)

type fakeOutput struct {
	uncapped bool
	dropped  int64
	lines    *[]runtime.OutputLine
}

func (f fakeOutput) Uncapped() bool                 { return f.uncapped }
func (f fakeOutput) DroppedSinceStart(string) int64 { return f.dropped }
func (f fakeOutput) Line(l runtime.OutputLine) {
	if f.lines != nil {
		*f.lines = append(*f.lines, l)
	}
}

// An app's page says where its output goes: copied and capped for a shared
// app, with what the cap refused; the cluster's for an isolated one; and
// nothing where this process runs no copy.
func TestAnAppsPolicySaysWhereItsOutputGoes(t *testing.T) {
	if database.DefaultPlatformSettings().AppLogCopyUncapped {
		t.Fatal("the cap is lifted by default")
	}
	srv := newTestServer(t)
	shared := &database.App{ID: "a1", ExecutionMode: "shared"}
	isolated := &database.App{ID: "a2", ExecutionMode: "isolated"}

	if p := srv.logCopyPolicy(shared); p != nil {
		t.Errorf("with no copy, log_copy = %+v, want absent", p)
	}
	srv.appOutput = fakeOutput{dropped: 42}
	p := srv.logCopyPolicy(shared)
	if p == nil || p.Mode != "copied" || p.Capped == nil || !*p.Capped || *p.DroppedSinceStart != 42 {
		t.Errorf("shared = %+v, want copied, capped, 42 dropped", p)
	}
	if p := srv.logCopyPolicy(isolated); p == nil || p.Mode != "cluster" || p.Capped != nil {
		t.Errorf("isolated = %+v, want cluster", p)
	}
}

// A shared host job's stored lines are copied, named by run and job; an
// isolated run's never are — they are its pod's stdout already.
func TestASharedRunsLinesAreCopiedAfterTheyAreStored(t *testing.T) {
	srv := newTestServer(t)
	var got []runtime.OutputLine
	srv.appOutput = fakeOutput{lines: &got}
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{Slug: "etl", Name: "ETL", OwnerID: "owner", Status: database.AppStatusRunning}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	lines := []database.JobLogLine{{Seq: 1, At: time.Now().UTC(), Stream: "stdout", Line: "extracted 12 rows"}}
	for _, mode := range []string{"shared", "isolated"} {
		run := &database.JobRun{AppID: app.ID, JobName: "nightly", StartedBy: database.JobStartedByApp, ExecutionMode: mode}
		if err := srv.db.EnqueueJobRun(run, 0); err != nil {
			t.Fatal(err)
		}
		got = nil
		srv.copyRunLines(&auth.RunClaims{RunID: run.ID}, lines)
		if mode == "shared" && (len(got) != 1 || got[0].RunID != run.ID || got[0].Job != "nightly" || got[0].AppID != app.ID) {
			t.Errorf("shared run copied %+v", got)
		}
		if mode == "isolated" && len(got) != 0 {
			t.Errorf("an isolated run's lines were copied: %+v", got)
		}
	}
}

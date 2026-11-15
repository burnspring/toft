package jobs

import (
	"testing"
	"time"

	"toft/internal/runtime"
)

// A run that posted its own result and kept running is stopped once the grace
// is over; one that exited is left alone (security review 2026-11-10, C-1).
func TestAReportedRunThatKeepsRunningIsStopped(t *testing.T) {
	b := newFakeBackend()
	b.statuses["alive"] = runtime.JobStatus{State: runtime.JobRunning}
	b.statuses["gone"] = runtime.JobStatus{State: runtime.JobSucceeded}

	settleReported(b, "run-gone", "gone", 10*time.Millisecond, nil)
	if b.wasCancelled("gone") {
		t.Error("a run that had exited was stopped")
	}
	settleReported(b, "run-alive", "alive", 10*time.Millisecond, nil)
	if !b.wasCancelled("alive") {
		t.Error("a run still alive after its report was not stopped")
	}
}

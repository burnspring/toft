package api

import (
	"testing"

	"toft/internal/jobs"
	"toft/internal/runtime"
)

// BB-010 (second 2026-11-03 bash): a job's timeout carried no enforcement
// beside memory's and CPU's. It is the platform's own clock, so enforced in
// every mode.
func TestAJobsTimeoutIsSaidEnforced(t *testing.T) {
	for _, mode := range []runtime.Mode{runtime.ModeShared, runtime.ModeIsolated} {
		l := withEnforcement(jobs.Limits{}, mode, runtime.Containment{})
		if l.TimeoutSec.Enforcement != runtime.Enforced {
			t.Errorf("%s: timeout enforcement %q", mode, l.TimeoutSec.Enforcement)
		}
	}
}

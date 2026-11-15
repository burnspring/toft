package api

import (
	"context"
	"fmt"
	"time"
)

// Work that was claimed and then abandoned, and schedules that stopped ticking.
// Both are failures that turn an install partly dead with no error anywhere: the
// app is fine, the platform is fine, and the thing simply never happens.

// checkStuckWork reports job runs past their deadline.
//
// A job run past its deadline is a worker that died between claiming the work
// and finishing it. ReleaseStaleClaims only recovers the ones that never
// started, so a run that began and then lost its node stays `running` forever
// with nothing to notice it. Asked on every install: a run is placed on this
// machine when there is no cluster (ADR §106), so "not isolated" is not "no
// jobs".
func (s *Server) checkStuckWork(_ context.Context, rp *checkReport) {
	now := time.Now()
	var findings []string

	overdue, err := s.db.ListOverdueJobRuns(now)
	if err != nil {
		rp.add("stuck_work", "Stuck work", checkWarn, dbReadFailed("stuck_work", "the job runs", err))
		return
	}
	if len(overdue) > 0 {
		oldest := now
		for _, r := range overdue {
			if r.StartedAt != nil && r.StartedAt.Before(oldest) {
				oldest = *r.StartedAt
			}
		}
		findings = append(findings, fmt.Sprintf(
			"%d job run(s) are past their deadline and still claimed, the oldest for %s — a worker "+
				"that died between claiming the work and finishing it",
			len(overdue), humanDuration(now.Sub(oldest))))
	}

	if len(findings) > 0 {
		rp.add("stuck_work", "Stuck work", checkWarn, sentence(findings))
		return
	}
	rp.add("stuck_work", "Stuck work", checkPass, "No job run is past its deadline.")
}

// checkScheduledJobs is about the ticker, not the job.
//
// Read the verdict carefully: an overdue schedule is not a job that failed, it
// is a firing that was never dispatched — the scheduler is not running, or the claim
// is failing, or the row is wedged. A *failed* run is the app's business and
// belongs on the app's own page. This row is the platform's.
//
// One row for all of them, on the git connections' reasoning: an install with
// twelve schedules must not own twelve rows.
func (s *Server) checkScheduledJobs(_ context.Context, rp *checkReport) {
	jobs, err := s.db.ListAllScheduledJobs()
	if err != nil {
		rp.add("scheduled_jobs", "Scheduled jobs", checkWarn, dbReadFailed("scheduled_jobs", "the scheduled jobs", err))
		return
	}

	enabled := 0
	var overdue []string
	cutoff := time.Now().Add(-scheduleOverdueAfter)
	for _, t := range jobs {
		if !t.Enabled {
			continue
		}
		enabled++
		if t.NextRunAt != nil && t.NextRunAt.Before(cutoff) {
			overdue = append(overdue, fmt.Sprintf("%s on %s (due %s ago)",
				t.Name, t.AppSlug, humanDuration(time.Since(*t.NextRunAt))))
		}
	}

	if enabled == 0 {
		rp.add("scheduled_jobs", "Scheduled jobs", checkSkip,
			"No app has an enabled schedule.")
		return
	}
	if len(overdue) > 0 {
		rp.add("scheduled_jobs", "Scheduled jobs", checkWarn,
			fmt.Sprintf("%d schedule(s) are more than %s past due and have not been dispatched: %s. "+
				"This is the scheduler, not the app — a run that failed would be recorded on the app.",
				len(overdue), humanDuration(scheduleOverdueAfter), list(overdue)))
		return
	}
	rp.add("scheduled_jobs", "Scheduled jobs", checkPass,
		fmt.Sprintf("%d enabled schedule(s), all dispatched on time.", enabled))
}

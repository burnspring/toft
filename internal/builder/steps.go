package builder

import (
	"encoding/json"
	"log"
	"time"

	"toft/internal/database"
	"toft/internal/deploysteps"
)

// StepLog is the durable record of which step a deploy is in and what each
// finished step cost — the thing a person watching a deploy reads, and the
// thing a CLI reports.
//
// It sits beside `buildTrace`, which times the same steps, and the duplication
// is deliberate. That one is telemetry: ADR §31 makes export optional and
// droppable by design, so on an install with no collector it produces nothing
// at all, and a deploy view built on it would go blank exactly where a small
// install needs it most. This one is a row. The two agree approximately,
// answer to different owners, and neither is the other's reason to write — the
// same split `internal/usage` and the metrics pipeline already keep.
//
// It is exported because the builder is not the only thing that walks a deploy
// through its steps: an uploaded bundle is received and unpacked by an API
// handler, before any builder has been handed the job, and those two steps are
// as much a part of the deploy as the clone they stand in for.
//
// Every method is safe on a nil receiver and a failed write is logged and
// dropped: a deploy must never fail because the platform could not describe it.
type StepLog struct {
	b  *Builder
	db *database.DB
	// deploy is kept up to date as well as written, because the caller holds
	// this same struct and hands it on: the API's upload handler answers with
	// it, and the build queue receives it. A StepLog that wrote only to the
	// database would leave both of them describing a deploy that had not
	// started — and the build worker, which continues from `StepTimings`,
	// would then write over the upload's steps rather than after them.
	deploy  *database.Deployment
	current BuildStep
	at      time.Time
	done    []stepTiming
}

// stepTiming is one finished step. The JSON shape is what the UI reads; keep
// it stable.
type stepTiming struct {
	Step string `json:"step"`
	MS   int64  `json:"ms"`
}

// NewStepLog begins recording, continuing whatever the deployment has already
// recorded rather than starting from nothing.
//
// The continuation is what makes an uploaded deploy's timeline whole. Its first
// two steps are written by the handler that received the bytes, and its
// remaining ten by a build worker that picks the job up afterwards; a second
// StepLog that started empty would write the build's timings over the upload's
// on its first close, and the phase that actually took the longest on a slow
// connection would be the one with no time against it.
// The row is the argument and the builder is not, because recording a step is a
// database write and telling a watcher about it is a courtesy. A nil builder —
// a server with no build queue — costs the broadcast and nothing else; the row
// is still written, and it is the row every later reader depends on.
func NewStepLog(db *database.DB, watchers *Builder, deploy *database.Deployment) *StepLog {
	s := &StepLog{b: watchers, db: db, deploy: deploy}
	if deploy.StepTimings != "" {
		if err := json.Unmarshal([]byte(deploy.StepTimings), &s.done); err != nil {
			// A row written by a future version, or a truncated one. Recording
			// from here is still better than refusing to record at all; what is
			// lost is the earlier steps' durations, which are already lost.
			log.Printf("Warning: reading recorded deploy timings for %s: %v", deploy.ID, err)
			s.done = nil
		}
	}
	return s
}

// Enter moves the deploy into a step, closing the previous one's clock and
// telling everyone watching.
func (s *StepLog) Enter(step BuildStep) {
	if s == nil {
		return
	}
	if s.current != "" {
		s.done = append(s.done, stepTiming{string(s.current), time.Since(s.at).Milliseconds()})
	}
	s.current = step
	s.at = time.Now()
	s.deploy.CurrentStep = string(step)
	s.deploy.Phase = string(deploysteps.PhaseOf(string(step)))
	startedAt := s.at.UTC()
	s.deploy.StepStartedAt = &startedAt
	if err := s.db.SetDeploymentStep(s.deploy.ID, string(step)); err != nil {
		log.Printf("Warning: recording deploy step %s for %s: %v", step, s.deploy.ID, err)
	}
	s.b.PublishProgress(s.deploy.ID, database.DeployStatusBuilding, step, "")
}

// Close writes the timings, once, whatever the deploy's outcome — and a failed
// deploy's timeline is the more useful of the two, because it shows how far it
// got and what it spent getting there.
//
// Deferred at the top of execute() for the reason buildTrace.finish is: fail()
// is followed by `return` at about thirty sites and a call at the bottom would
// be stepped over by every one of them.
func (s *StepLog) Close() {
	if s == nil {
		return
	}
	if s.current != "" {
		s.done = append(s.done, stepTiming{string(s.current), time.Since(s.at).Milliseconds()})
		s.current = ""
	}
	if len(s.done) == 0 {
		return
	}
	encoded, err := json.Marshal(s.done)
	if err != nil {
		log.Printf("Warning: encoding deploy timings for %s: %v", s.deploy.ID, err)
		return
	}
	s.deploy.StepTimings = string(encoded)
	if err := s.db.SetDeploymentTimings(s.deploy.ID, string(encoded)); err != nil {
		log.Printf("Warning: recording deploy timings for %s: %v", s.deploy.ID, err)
	}
}

package builder

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"toft/internal/database"
	"toft/internal/telemetry"
)

// PhaseTotal is the whole build, recorded once at the end alongside the phases
// it is the sum of.
//
// One instrument rather than two, so "why was that deploy slow" is answered
// from one series family: an operator graphs toft.build.duration by phase
// and the total sits in the same query as its parts.
const PhaseTotal = "total"

// buildTrace times a deploy, phase by phase, and holds the span tree.
//
// The durations are accumulated and emitted at the end rather than recorded as
// each phase closes, and that is not laziness. The framework is not known until
// the detector has run — three phases in — and the execution mode is settled
// before the first phase but is worth carrying on every one of them. Recording
// as we went would label the early phases `unknown`, which is exactly the
// series an operator would then have to explain away on every dashboard.
//
// Every method is safe on a nil receiver and inert when telemetry is off, so
// the call sites in execute() need no branch.
type buildTrace struct {
	ctx    context.Context
	parent telemetry.Span

	phase     telemetry.Span
	phaseName BuildStep
	phaseAt   time.Time
	startedAt time.Time

	durations []phaseDuration
	framework string
	mode      string
}

type phaseDuration struct {
	name string
	dur  time.Duration
}

// startBuildTrace opens the build span. queued is how long the job sat before a
// worker took it — the figure that turns "builds are slow" into "builds are
// queued", which are different problems with different fixes.
func startBuildTrace(app *database.App, deploy *database.Deployment, queued time.Duration, rollback bool) *buildTrace {
	ctx, span := telemetry.Start(context.Background(), "build",
		attribute.String("toft.app.id", telemetry.AppAttr(app.ID)),
		attribute.String("toft.deployment.id", deploy.ID),
		attribute.Bool("toft.rollback", rollback),
	)
	telemetry.RecordBuildQueueWait(ctx, queued)
	telemetry.AddBuildActive(ctx, 1)

	now := time.Now()
	t := &buildTrace{
		ctx:       ctx,
		parent:    span,
		startedAt: now,
		phaseAt:   now,
		phaseName: StepSetup,
		framework: "unknown",
		mode:      "shared",
	}
	t.phase = t.openPhase(StepSetup)
	return t
}

func (t *buildTrace) openPhase(step BuildStep) telemetry.Span {
	_, span := telemetry.Start(t.ctx, "build."+string(step),
		attribute.String("toft.build.phase", string(step)))
	return span
}

// next closes the current phase and opens the next, returning the step so the
// call site reads `step = trace.next(StepFetch)` — one line, in the place the
// assignment already was, and impossible to do by accident on only some paths.
func (t *buildTrace) next(step BuildStep) BuildStep {
	if t == nil {
		return step
	}
	t.closePhase(nil)
	t.phaseName = step
	t.phaseAt = time.Now()
	t.phase = t.openPhase(step)
	return step
}

func (t *buildTrace) closePhase(err error) {
	if t == nil || t.phaseName == "" {
		return
	}
	t.durations = append(t.durations, phaseDuration{string(t.phaseName), time.Since(t.phaseAt)})
	t.phase.End(err)
	t.phaseName = ""
}

// detected records what the detector found, once it has run.
func (t *buildTrace) detected(framework string) {
	if t == nil || framework == "" {
		return
	}
	t.framework = framework
}

// placed records where this release was decided to run.
func (t *buildTrace) placed(mode string) {
	if t == nil || mode == "" {
		return
	}
	t.mode = mode
	t.parent.Set(attribute.String("toft.execution_mode", mode))
}

// finish closes the build. Called exactly once, on both the success and the
// failure path — which is why execute() defers it rather than calling it at the
// bottom, where a `return` inside fail() would step over it.
func (t *buildTrace) finish(outcome string) {
	if t == nil {
		return
	}
	t.closePhase(nil)
	total := time.Since(t.startedAt)
	for _, d := range t.durations {
		// Every phase carries the *build's* outcome, not its own. A clone that
		// succeeded inside a build that failed at `start` is still part of a
		// failed build, and separating the two would make "p95 of a successful
		// build's clone phase" impossible to write.
		telemetry.RecordBuildPhase(t.ctx, d.name, outcome, t.framework, t.mode, d.dur)
	}
	telemetry.RecordBuildPhase(t.ctx, PhaseTotal, outcome, t.framework, t.mode, total)
	telemetry.AddBuildActive(t.ctx, -1)
	t.parent.EndOK(outcome)
}

// deployed records the flip itself: the moment the backend was asked to place
// the workload, separately from the build that produced it.
//
// Split out because a rollback is exactly this phase and none of the others,
// and averaging the two hides the thing rollback exists to be — fast.
func (t *buildTrace) deployed(mode string, rollback bool, dur time.Duration, err error) {
	ctx := context.Background()
	if t != nil {
		ctx = t.ctx
	}
	telemetry.RecordDeploy(ctx, telemetry.Outcome(err), mode, rollback, dur)
}

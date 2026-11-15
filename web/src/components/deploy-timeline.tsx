import { useEffect, useState } from "react";
import { Check, Loader2, Minus, X } from "lucide-react";
import { cn } from "@/lib/utils";
import { deployPhases, formatStepMs, type PhaseRow, type StepState, type TimelineRow } from "@/lib/deploy-steps";
import { formatElapsed } from "@/lib/time";
import type { Deployment } from "@/lib/api";

/**
 * What a deploy is actually doing — three phases, and the steps inside them.
 *
 * This is the one thing this product exists to do, and until recently it was a
 * spinner and the word "Building" for however long it took. The builder has
 * named and timed its steps the whole time; putting them on screen was the
 * first half, and grouping them is the second: a dozen equal rows answers a
 * question nobody asked, while "upload, scan, deploy" is how a person waiting
 * on their own code describes what they are waiting for.
 *
 * It is information rather than decoration, which is what makes it the right
 * place to spend this product's entire motion budget: one spinner on the step
 * that is running, and a counter beside it. Nothing else animates.
 *
 * Four rules the drawing has to keep:
 *
 * - **A finished build never spins.** A failed one is drawn as failed at the
 *   step it stopped in, not as forever-in-progress — a spinner on something
 *   that ended half an hour ago is a worse lie than showing nothing.
 * - **Absent means absent.** A deployment that recorded no step gets no
 *   timeline (`deployPhases` returns null) and the caller shows what it showed
 *   before. A dozen confident rows under a build nobody measured would be
 *   fabrication.
 * - **A step that never applied is never done.** A git deploy did not receive
 *   an upload, and drawing a tick beside one because the build moved past it is
 *   a small lie told on every timeline.
 * - **The durations are the point.** "Building" for four minutes is a fact
 *   about the product; "installing dependencies, 3m 50s" is a fact somebody can
 *   act on.
 */

const DOT: Record<StepState, string> = {
  done: "border-tone-ok-border bg-tone-ok-surface text-tone-ok-fg",
  active: "border-primary/30 bg-primary/10 text-primary",
  failed: "border-tone-danger-border bg-tone-danger-surface text-tone-danger-fg",
  pending: "border-border bg-muted text-muted-foreground",
  // Neutral, not muted-into-nothing: a step that did not apply is a recorded
  // fact with no valence, the same as "stopped" or "skipped" elsewhere.
  skipped: "border-tone-neutral-border bg-tone-neutral-surface text-tone-neutral-fg",
};

function StepDot({ state, size = "sm" }: { state: StepState; size?: "sm" | "md" }) {
  const box = size === "md" ? "h-5 w-5" : "h-4 w-4";
  const glyph = size === "md" ? "h-3 w-3" : "h-2.5 w-2.5";
  return (
    <span
      className={cn("flex shrink-0 items-center justify-center rounded-full border", box, DOT[state])}
      aria-hidden
    >
      {state === "done" && <Check className={glyph} />}
      {state === "active" && <Loader2 className={cn(glyph, "motion-safe:animate-spin")} />}
      {state === "failed" && <X className={glyph} />}
      {state === "skipped" && <Minus className={glyph} />}
      {state === "pending" && <span className="h-1 w-1 rounded-full bg-current" />}
    </span>
  );
}

/** The word a screen reader gets, since the dot is a shape and a colour. */
const SPOKEN: Record<StepState, string> = {
  done: "done",
  active: "in progress",
  failed: "failed",
  pending: "not started",
  skipped: "not applicable",
};

export function DeployTimeline({
  deployment,
  className,
}: {
  deployment: Deployment;
  className?: string;
}) {
  const phases = deployPhases(deployment);
  if (!phases) return null;
  return (
    <ol className={cn("space-y-3", className)}>
      {phases.map((phase) => (
        <PhaseGroup key={phase.key} phase={phase} startedAt={deployment.step_started_at} />
      ))}
    </ol>
  );
}

function PhaseGroup({ phase, startedAt }: { phase: PhaseRow; startedAt?: string }) {
  // A step that never applied is not drawn at all. Its absence is the honest
  // rendering — the phase's own state already says whether it happened, and a
  // row of struck-through steps on every git deploy would be noise about a
  // path that app never takes.
  const steps = phase.steps.filter((s) => s.state !== "skipped");
  return (
    <li>
      <div className="flex items-center gap-2.5 text-xs">
        <StepDot state={phase.state} size="md" />
        <span
          className={cn(
            "font-semibold",
            phase.state === "pending" ? "text-muted-foreground" : "text-foreground",
          )}
        >
          {phase.label}
        </span>
        <span className="sr-only">{SPOKEN[phase.state]}</span>
        <span className="truncate text-muted-foreground">{phase.detail}</span>
        <span className="ml-auto shrink-0 font-mono text-2xs text-muted-foreground">
          {phase.ms !== undefined ? formatStepMs(phase.ms) : null}
        </span>
      </div>
      {steps.length > 0 && (
        <ol className="mt-1.5 space-y-1 border-l border-border/60 pl-4 ml-2">
          {steps.map((step) => (
            <StepRow key={step.key} row={step} startedAt={startedAt} />
          ))}
        </ol>
      )}
    </li>
  );
}

function StepRow({ row, startedAt }: { row: TimelineRow; startedAt?: string }) {
  return (
    <li className="flex items-center gap-2 text-xs">
      <StepDot state={row.state} />
      <span className={row.state === "pending" ? "text-muted-foreground" : "text-foreground"}>
        {row.label}
      </span>
      <span className="sr-only">{SPOKEN[row.state]}</span>
      {row.state === "active" && (
        <span className="truncate text-muted-foreground">{row.detail}</span>
      )}
      <span className="ml-auto shrink-0 font-mono text-2xs text-muted-foreground">
        {row.ms !== undefined
          ? formatStepMs(row.ms)
          : row.state === "active" && startedAt
            ? <Elapsed since={startedAt} />
            : null}
      </span>
    </li>
  );
}

/**
 * A counter for the step that is running.
 *
 * The server sends when the step began and this counts up from it, rather than
 * the page polling for a number that is a subtraction. One interval for the one
 * row that is live.
 */
function Elapsed({ since }: { since: string }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, []);
  return <>{formatElapsed(since)}</>;
}

/**
 * The same three phases as a position rather than a list.
 *
 * A person waiting on a deploy wants one glance to answer *is it still moving*,
 * and a bar answers it at a size the timeline cannot: three segments, sized to
 * nothing in particular, because this is a position and not a prediction — the
 * platform does not know how long a build takes and a segment scaled to a guess
 * would be a promise. It is drawable at all only because §40 made the phases
 * contiguous in the step order.
 *
 * This is where the motion budget is spent now: the running segment, and
 * nothing else on the page (ADR §87).
 */
const SEGMENT: Record<StepState, string> = {
  done: "bg-tone-ok-solid",
  active: "bg-primary/20",
  failed: "bg-tone-danger-solid",
  pending: "bg-border",
  skipped: "bg-tone-neutral-solid",
};

export function PhaseBar({
  deployment,
  className,
}: {
  deployment: Deployment;
  className?: string;
}) {
  const phases = deployPhases(deployment);
  if (!phases) return null;
  return (
    <ol className={cn("flex items-stretch gap-1.5", className)}>
      {phases.map((phase) => (
        <li key={phase.key} className="min-w-0 flex-1">
          <div className={cn("h-1.5 overflow-hidden rounded-full", SEGMENT[phase.state])}>
            {phase.state === "active" && (
              <div className="h-full w-1/2 rounded-full bg-primary motion-safe:animate-pulse" />
            )}
          </div>
          <div className="mt-1 flex items-baseline gap-1.5">
            <span
              className={cn(
                "truncate text-2xs font-medium",
                phase.state === "pending" ? "text-muted-foreground" : "text-foreground",
              )}
            >
              {phase.label}
            </span>
            <span className="sr-only">{SPOKEN[phase.state]}</span>
            {phase.ms !== undefined && (
              <span className="shrink-0 font-mono text-2xs text-muted-foreground">
                {formatStepMs(phase.ms)}
              </span>
            )}
          </div>
        </li>
      ))}
    </ol>
  );
}

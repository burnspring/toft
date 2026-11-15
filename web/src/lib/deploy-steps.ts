import type { Deployment } from "@/lib/api";

/**
 * The three phases of a deploy, and the steps inside them.
 *
 * The builder has named its steps since it was written (`internal/deploysteps`)
 * and used them as the failure fingerprint, so the vocabulary is settled. What
 * changed is that a dozen equal rows is not how anyone asks about a deploy:
 * they ask whether their code arrived, whether it was allowed, and whether it
 * is live. That is the phase, and the steps are the detail underneath it.
 *
 * The keys here mirror the server's exactly — they arrive on the deployment row
 * and in the progress stream — but the labels are this surface's own. `flip` is
 * a real and useful distinction to the builder and means nothing to an app's
 * owner, who wants to know that traffic moved.
 *
 * Order is the specification. A timeline drawn in a different order from the
 * one the build runs in would be worse than none, and the phases are contiguous
 * in it (a server-side test holds that property), so grouping is a scan rather
 * than a sort.
 */
export type DeployPhaseKey = "upload" | "scan" | "deploy";

/** One step of a deploy, as this surface draws it. */
export interface DeployStepDef {
  key: string;
  phase: DeployPhaseKey;
  label: string;
  detail: string;
  /**
   * Conditional steps happen on some deploys and not others, so a later step
   * does not mean they were skipped — it may mean they never applied. Only a
   * bundle upload receives and unpacks; a git deploy clones instead, and
   * drawing those two as done because the build moved past them would be a
   * small lie told on every timeline.
   */
  conditional?: boolean;
}

export const DEPLOY_STEPS: readonly DeployStepDef[] = [
  {
    key: "receive",
    phase: "upload",
    label: "Receiving",
    detail: "Taking the uploaded archive",
    conditional: true,
  },
  { key: "unpack", phase: "upload", label: "Unpacking", detail: "Expanding the archive and checking it", conditional: true },
  { key: "setup", phase: "upload", label: "Preparing", detail: "Deciding where this deployment will run" },
  { key: "fetch", phase: "upload", label: "Fetching code", detail: "Cloning the repository, or staging the upload" },
  { key: "scan", phase: "scan", label: "Checking dependencies", detail: "Judging what this deployment wants to install" },
  { key: "detect", phase: "deploy", label: "Detecting", detail: "Working out the framework and how to build it" },
  { key: "configure", phase: "deploy", label: "Configuring", detail: "Writing the base path and the app’s environment" },
  { key: "build", phase: "deploy", label: "Building", detail: "Installing dependencies and running the build" },
  { key: "publish", phase: "deploy", label: "Packaging", detail: "Packing the deployment into the artifact store" },
  { key: "start", phase: "deploy", label: "Starting", detail: "Bringing the new deployment up on its own address", conditional: true },
  { key: "ready", phase: "deploy", label: "Health check", detail: "Waiting for the app to answer a request", conditional: true },
  { key: "flip", phase: "deploy", label: "Going live", detail: "Moving traffic over, then retiring the old deployment" },
];

/**
 * What each phase is called, and what it means.
 *
 * The first phase is named for its source. "Upload" is what a person who
 * pushed an archive did; for a git-backed app the platform did the fetching and
 * calling it an upload would describe an action nobody took. Same phase, same
 * key on the wire, two words for two truths.
 */
export const DEPLOY_PHASES: {
  key: DeployPhaseKey;
  label: string;
  uploadedLabel?: string;
  detail: string;
}[] = [
  {
    key: "upload",
    label: "Source",
    uploadedLabel: "Upload",
    detail: "Getting the code onto the platform",
  },
  { key: "scan", label: "Scan", detail: "Judging it before anything is built" },
  { key: "deploy", label: "Deploy", detail: "Building it, starting it, moving traffic" },
];

/** One step's cost, as the builder recorded it. */
export interface StepTiming {
  step: string;
  ms: number;
}

/**
 * What the timeline should draw for one step.
 *
 * `state` is per step and there are four: `done` has a duration, `active` is
 * where the deploy is now, `pending` has not happened, and `skipped` never
 * applied to this deploy. A **failed** build's current step is where it
 * stopped, and is drawn as failed rather than active — a spinner on a build
 * that ended half an hour ago is a worse lie than no timeline.
 */
export type StepState = "done" | "active" | "failed" | "pending" | "skipped";

export interface TimelineRow {
  key: string;
  phase: DeployPhaseKey;
  label: string;
  detail: string;
  state: StepState;
  /** Milliseconds, for a finished step. */
  ms?: number;
}

export interface PhaseRow {
  key: DeployPhaseKey;
  label: string;
  detail: string;
  state: StepState;
  steps: TimelineRow[];
  /** The phase's total, when every step in it has been timed. */
  ms?: number;
}

function parseTimings(raw?: string): Map<string, number> {
  if (!raw) return new Map();
  try {
    const parsed: StepTiming[] = JSON.parse(raw);
    return new Map(parsed.map((t) => [t.step, t.ms]));
  } catch {
    // A row written by a future version, or a truncated one. The timeline
    // still draws from `current_step`; only the durations are missing.
    return new Map();
  }
}

/**
 * Build the timeline, or return null when there is nothing honest to draw.
 *
 * Null for any deployment that recorded no step at all — every row older than
 * the columns. The caller falls back to what it showed before, which is the
 * right answer: history that predates the feature genuinely has no timeline,
 * and inventing one for it would put a dozen confident rows under a build
 * nobody measured.
 */
export function deployTimeline(deployment: Deployment): TimelineRow[] | null {
  const current = deployment.current_step;
  if (!current) return null;

  const timings = parseTimings(deployment.step_timings);
  const currentIndex = DEPLOY_STEPS.findIndex((s) => s.key === current);
  if (currentIndex === -1) return null;

  const inFlight = deployment.status === "pending" || deployment.status === "building";
  const failed = deployment.status === "failed";

  return DEPLOY_STEPS.map((step, i) => {
    let state: StepState;
    if (i < currentIndex) {
      // A conditional step is only done if it actually ran, and a recorded
      // duration is the proof. Without this every git deploy would claim to
      // have received and unpacked an upload it never had.
      state = !step.conditional || timings.has(step.key) ? "done" : "skipped";
    } else if (i > currentIndex) {
      state = "pending";
    } else if (inFlight) {
      state = "active";
    } else if (failed) {
      state = "failed";
    } else {
      state = "done";
    }
    return {
      key: step.key,
      phase: step.phase,
      label: step.label,
      detail: step.detail,
      state,
      ms: timings.get(step.key),
    };
  });
}

/**
 * The same timeline at the altitude people ask in: three phases, each carrying
 * its steps and the state of the whole.
 *
 * A phase is failed if any step in it failed, active if any is running, done
 * once every step that applied to this deploy is done, and pending otherwise.
 * A phase whose every step was skipped is skipped — which is not a state any
 * phase reaches today, and is the honest answer if one ever does.
 */
export function deployPhases(deployment: Deployment): PhaseRow[] | null {
  const rows = deployTimeline(deployment);
  if (!rows) return null;

  const uploaded = rows.some((r) => r.key === "receive" && r.state !== "skipped");

  return DEPLOY_PHASES.map((phase) => {
    const steps = rows.filter((r) => r.phase === phase.key);
    const ran = steps.filter((s) => s.state !== "skipped");
    let state: StepState;
    if (steps.some((s) => s.state === "failed")) state = "failed";
    else if (steps.some((s) => s.state === "active")) state = "active";
    else if (ran.length === 0) state = "skipped";
    else if (ran.every((s) => s.state === "done")) state = "done";
    else if (ran.some((s) => s.state === "done")) state = "active";
    else state = "pending";

    const timed = ran.filter((s) => s.ms !== undefined);
    return {
      key: phase.key,
      label: phase.key === "upload" && uploaded ? (phase.uploadedLabel ?? phase.label) : phase.label,
      detail: phase.detail,
      state,
      steps,
      ms: timed.length === ran.length && timed.length > 0
        ? timed.reduce((total, s) => total + (s.ms ?? 0), 0)
        : undefined,
    };
  });
}

/** A step's duration, at the precision a person reading a build cares about. */
export function formatStepMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const m = Math.floor(ms / 60_000);
  const s = Math.round((ms % 60_000) / 1000);
  return s ? `${m}m ${s}s` : `${m}m`;
}

/** What the last successful deploy of this app cost, as a guide to this one. */
export interface DeployExpectation {
  /** The whole deploy, start to finish, in milliseconds. */
  totalMs: number;
  /** The step that took longest, when it was most of the wait. */
  slowest?: { label: string; ms: number };
}

/**
 * How long the deploy now running is likely to take: the most recent
 * *successful* deploy of the same app other than this one, read off its own
 * row (ADR §123).
 *
 * A guide and never a promise, so it is said as what happened last time rather
 * than as a countdown — a countdown that reaches nought with the build still
 * going is a progress bar that lied. A failed deploy is no guide (it stopped
 * somewhere), and neither is a rollback, which builds nothing; a row with no
 * finish time is still running. The slowest step is named only when it was at
 * least half the wait, because that is when knowing it changes what somebody
 * expects to see.
 */
export function expectedDeploy(deployments: Deployment[], currentId?: string): DeployExpectation | null {
  const last = deployments.find(
    (d) => d.id !== currentId && d.status === "success" && !!d.started_at && !!d.finished_at,
  );
  if (!last) return null;
  const totalMs = new Date(last.finished_at).getTime() - new Date(last.started_at).getTime();
  if (!(totalMs > 0)) return null;
  let slowest: DeployExpectation["slowest"];
  for (const [step, ms] of parseTimings(last.step_timings)) {
    if (!slowest || ms > slowest.ms) {
      slowest = { label: DEPLOY_STEPS.find((s) => s.key === step)?.label ?? step, ms };
    }
  }
  return { totalMs, slowest: slowest && slowest.ms * 2 >= totalMs ? slowest : undefined };
}

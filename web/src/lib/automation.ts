/**
 * How jobs are spoken about, in one place.
 *
 * Toft has one automation concept — the job, declared in toft.toml — and two
 * surfaces render it: the app's Automation tab and Admin → Automation. Two
 * copies of this vocabulary would drift into telling an owner and an operator
 * different things about the same row.
 */

import { ApiError, type JobDefinition, type JobLimit } from "@/lib/api";

/** What runs: the command line, or the binary's argv written out. */
export function jobWhatRuns(job: Pick<JobDefinition, "command" | "args">): string {
  if (job.command) return job.command;
  return (job.args ?? []).join(" ");
}

/** A duration in seconds the way toft.toml would say it: "45m", "1h30m", "2d". */
export function formatJobDuration(sec: number): string {
  if (!Number.isFinite(sec) || sec <= 0) return "—";
  if (sec % 86400 === 0) return `${sec / 86400}d`;
  if (sec < 60) return `${sec}s`;
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  return `${h ? `${h}h` : ""}${m ? `${m}m` : ""}${s ? `${s}s` : ""}`;
}

/** A job's CPU limit in cores; "no CPU limit" for an install that set none. */
export function formatJobCPU(millicores: number): string {
  if (millicores <= 0) return "no CPU limit";
  return millicores === 1000 ? "1 core" : `${millicores / 1000} cores`;
}

/** Megabytes, said in gigabytes from a gigabyte up when it is a whole number of them. */
export function formatJobMemory(mb: number): string {
  if (mb >= 1024 && mb % 1024 === 0) return `${mb / 1024} GB`;
  return `${mb} MB`;
}

/** Where an effective limit came from, in words. A clamp names what was asked for. */
export function limitSourceLabel(limit: JobLimit, format: (n: number) => string): string {
  switch (limit.source) {
    case "declared": return "from toft.toml";
    case "install default": return "install default";
    case "override": return "set for this job by an administrator";
    case "install ceiling": return "the install’s ceiling for a job";
    case "clamped":
      return limit.declared
        ? `lowered from ${format(limit.declared)} to the install’s ceiling`
        : "lowered to the install’s ceiling";
    default: return limit.source;
  }
}

/** Where runs go, as a sentence. Empty when the platform gave no answer. */
export function placementSentence(placement: string | undefined): string {
  if (placement === "cluster") return "Runs go to the cluster.";
  if (placement === "this machine") return "Runs go to this machine.";
  return "";
}

/** The parameters, short enough for a table cell. */
export function paramsSummary(params: Record<string, unknown> | null | undefined): string {
  const entries = Object.entries(params ?? {});
  if (entries.length === 0) return "";
  return entries
    .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
    .join(" ");
}

/**
 * Where a run came from.
 *
 * `unknown` is a real case and is not a person: a start this build has no word
 * for, or a person-shaped start whose account could not be resolved, renders
 * as a gap rather than as somebody's name. Drawn by
 * `components/run-provenance.tsx`.
 */
export type RunOrigin =
  | { kind: "person"; label: string; via: "viewer" | "cli" | "ui" }
  | { kind: "schedule" }
  | { kind: "app" }
  | { kind: "unknown" };

/**
 *   a schedule fired                 → started_by=schedule
 *   the app started it itself        → started_by=app
 *   a viewer pressed the app's button→ started_by=viewer + started_by_user
 *   `toft job run`                   → started_by=cli + started_by_user
 *   Run now on the Automation tab    → started_by=ui + started_by_user
 */
export function jobRunOrigin(run: { started_by: string; started_by_user?: string }): RunOrigin {
  switch (run.started_by) {
    case "schedule": return { kind: "schedule" };
    case "app": return { kind: "app" };
    case "viewer":
    case "cli":
    case "ui":
      return run.started_by_user
        ? { kind: "person", label: run.started_by_user, via: run.started_by }
        : { kind: "unknown" };
    default: return { kind: "unknown" };
  }
}

/**
 * A refusal in the install's own words, with its hint when it carries one —
 * `queue_full` says where to look, `run_still_open` names the command to wait.
 */
export function refusalText(err: unknown, fallback: string): string {
  if (!(err instanceof Error)) return fallback;
  const hint = err instanceof ApiError ? err.body["hint"] : undefined;
  const msg = err.message || fallback;
  return typeof hint === "string" && hint !== "" ? `${msg} — ${hint}` : msg;
}

import { toneText } from "@/lib/tone";
import { StatusBadge } from "@/components/status-badge";
import { RunProvenance } from "@/components/run-provenance";
import { jobRunStatusLabel, jobRunStatusTone } from "@/lib/job-runs";
import { jobRunOrigin } from "@/lib/automation";
import type { JobRun } from "@/lib/api";

/**
 * The components that render a job run, shared by the app's Automation tab and
 * Admin → Automation. The vocabulary they draw from is in `lib/job-runs.ts`.
 */

export function JobRunStatusBadge({ status }: { status: string }) {
  return <StatusBadge status={status} tone={jobRunStatusTone(status)} label={jobRunStatusLabel(status)} />;
}

/** Who started this run. The drawing is `RunProvenance`, the mapping `jobRunOrigin`. */
export function JobRunProvenance({ run }: { run: Pick<JobRun, "started_by" | "started_by_user"> }) {
  return <RunProvenance origin={jobRunOrigin(run)} />;
}

/**
 * How a run ended, or how far it has got, in one line.
 *
 * The reason is the platform's own words — `exit 3: connection refused`,
 * `timed out after 45m`, `stopped at 1024 MB` — which is the sentence that says
 * which of three fixes applies. An output that has expired is not offered.
 */
export function JobRunResult({ run }: { run: JobRun }) {
  if (run.failure_reason) {
    return <span className={toneText.danger}>{run.failure_reason}</span>;
  }
  if (run.status === "running") {
    if (run.progress_message) {
      return <span>{run.progress > 0 ? `${run.progress}% · ` : ""}{run.progress_message}</span>;
    }
    return <span>{run.progress > 0 ? `${run.progress}%` : "Running"}</span>;
  }
  if (run.status === "succeeded") {
    return <span>{run.has_output ? "Wrote an output file" : "Finished"}</span>;
  }
  return <span>—</span>;
}

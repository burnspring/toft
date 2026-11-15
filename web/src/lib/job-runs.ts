import type { Tone } from "@/lib/tone";

/**
 * How a job run is spoken about, in one place.
 *
 * Two surfaces render runs — the app's Automation tab and Admin → Automation —
 * and they are read by different people about the same rows. Two copies of this
 * vocabulary would drift, and the drift would be somebody's incident: an
 * operator and an owner disagreeing about whether a run "failed" or "ran out of
 * time" is the difference between debugging the code and raising the memory
 * limit.
 *
 * The components that render these live in `components/job-run-status.tsx`.
 */

/** The platform's status vocabulary, in the words a developer would use.
 *
 * `timed_out` in a table cell reads as a bug in our copy rather than a fact
 * about their job — and the distinctions are kept because the fix differs:
 * a bigger limit, less work, or a code change. */
export function jobRunStatusLabel(status: string): string {
  switch (status) {
    case "queued": return "Queued";
    case "running": return "Running";
    case "succeeded": return "Succeeded";
    case "failed": return "Failed";
    case "timed_out": return "Ran out of time";
    case "out_of_memory": return "Out of memory";
    case "cancelled": return "Cancelled";
    case "skipped": return "Skipped";
    default: return status;
  }
}

/** What a run status looks like. A job's vocabulary is its own — "ran out of
 * time" is not "failed" — so it names a tone rather than borrowing a status
 * word that means something else on another page. */
export function jobRunStatusTone(status: string): Tone {
  switch (status) {
    case "queued": return "attention";
    case "running": return "ok";
    case "succeeded": return "ok";
    // Neutral, never an error colour: a cancel is somebody's decision and a
    // skip is the overlap policy doing what it was set to do.
    case "cancelled":
    case "skipped": return "neutral";
    default: return "danger";
  }
}

/** Whether a run is still open — waiting or going. Everything else is final. */
export function jobRunInFlight(status: string): boolean {
  return status === "queued" || status === "running";
}

/** "3rd in line" — a queued run's place in *its own app's* queue. The limit
 * that holds it there is per app (`job_concurrency_per_app`), so this number is
 * never an install-wide position. */
export function ordinal(n: number): string {
  const suffix = n % 100 >= 11 && n % 100 <= 13 ? "th"
    : n % 10 === 1 ? "st"
    : n % 10 === 2 ? "nd"
    : n % 10 === 3 ? "rd"
    : "th";
  return `${n}${suffix}`;
}

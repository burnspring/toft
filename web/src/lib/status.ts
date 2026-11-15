import type { Tone } from "@/lib/tone";

/**
 * What a status word means and how it should look — one table, read by both
 * `StatusBadge` and `StatusDot`.
 *
 * They each held their own before this, and had drifted in the way two copies
 * always do: `sleeping` was a lighter grey as a dot than as a badge, the dot
 * knew about `online` and `errored` and the badge did not, and the dot
 * lowercased its labels and re-capitalized them in CSS. None of that was a
 * decision anybody made.
 *
 * The `tone` column is where the reasoning lives; `lib/tone.ts` says what each
 * tone is for. The two rules worth restating here, because they are the ones a
 * new status gets wrong:
 *
 * - **`sleeping` and `skipped` are neutral, never a warning.** Both did exactly
 *   what they were configured to do. A slept app wakes on the next request and
 *   the visitor never knows; a skipped run is the overlap policy working.
 * - **`skip` — a check for a capability this install does not offer — is
 *   neutral too.** An optional capability that is absent is not a degraded
 *   install, and amber here sends an administrator hunting for a fault.
 */
export interface StatusMeaning {
  label: string;
  tone: Tone;
}

const STATUS: Record<string, StatusMeaning> = {
  // App and process lifecycle.
  running: { label: "Running", tone: "ok" },
  online: { label: "Running", tone: "ok" },
  building: { label: "Building", tone: "attention" },
  deploying: { label: "Deploying", tone: "attention" },
  pending: { label: "Pending", tone: "attention" },
  stopped: { label: "Stopped", tone: "neutral" },
  sleeping: { label: "Sleeping", tone: "neutral" },
  /** A batch app with a release: live, and serving nothing because it has no
   * service (ADR §107). A fact, not a state that wants attention. */
  ready: { label: "Ready", tone: "neutral" },
  crashed: { label: "Crashed", tone: "danger" },
  errored: { label: "Crashed", tone: "danger" },

  // Outcomes.
  success: { label: "Success", tone: "ok" },
  failed: { label: "Failed", tone: "danger" },
  error: { label: "Error", tone: "danger" },
  skipped: { label: "Skipped", tone: "neutral" },
  queued: { label: "Queued", tone: "attention" },

  // Accounts, keys and connections.
  active: { label: "Active", tone: "ok" },
  inactive: { label: "Inactive", tone: "neutral" },
  paused: { label: "Paused", tone: "neutral" },
  locked: { label: "Locked", tone: "attention" },
  /** An account that exists and cannot sign in until an administrator
   * activates it. One word for it, where People said "Pending" and the
   * account's own page "Pending activation". */
  pending_activation: { label: "Pending activation", tone: "attention" },
  connected: { label: "Connected", tone: "ok" },
  disconnected: { label: "Disconnected", tone: "neutral" },

  // System checks.
  pass: { label: "Passed", tone: "ok" },
  warn: { label: "Warning", tone: "attention" },
  fail: { label: "Failed", tone: "danger" },
  skip: { label: "Not applicable", tone: "neutral" },

  // Requests an administrator answers.
  approved: { label: "Approved", tone: "ok" },
  // An administrator's recorded decision, not a failure: red beside a
  // person's own request history read as something wrong with them.
  denied: { label: "Declined", tone: "neutral" },
  withdrawn: { label: "Withdrawn", tone: "neutral" },

  // Who caused a deploy. `info` is the platform acting on its own.
  auto: { label: "Auto", tone: "info" },
  rollback: { label: "Rollback", tone: "info" },
  // A notebook's scheduled re-render. `info`, like the two above it: the
  // platform acting rather than a person.
  render: { label: "Re-render", tone: "info" },
};

/** An unknown status renders its own word, neutrally, rather than disappearing. */
export function statusMeaning(status: string): StatusMeaning {
  return STATUS[status] ?? { label: status, tone: "neutral" };
}

/** Whether the dot for this status should pulse: work is happening right now. */
export function statusIsLive(status: string): boolean {
  return (
    status === "building" || status === "deploying" || status === "pending"
  );
}

/**
 * A row's severity, as a tone. `app_events` and `audit_log` are merged at read
 * time (ADR §21) and rendered by two pages, which each held their own copy of
 * this three-line map.
 *
 * Anything unrecognised is neutral rather than loud: `audit_log` records
 * ordinary administration as `info`, and most of what is in it is somebody
 * doing their job.
 */
export function severityTone(severity: string): Tone {
  switch (severity) {
    case "error":
      return "danger";
    case "warning":
      return "attention";
    default:
      return "neutral";
  }
}

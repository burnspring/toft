/**
 * Which rows of Admin → Settings this install puts on screen.
 *
 * A row is on screen only while it governs something — the job figures go with
 * background jobs, hidden rather than disabled, because three retention numbers
 * for a queue that is off read as a policy in force. An install that can never
 * isolate an app (one machine, no cluster) keeps the Isolated execution row,
 * which says why, and drops the rows that only govern isolated apps.
 *
 * **Background jobs are not one of those rows.** With no cluster a run goes to
 * this server (ADR §106), so the switch is hidden only when there is nowhere at
 * all to run one. Hiding it with the isolated rows left an ECS install with jobs
 * off and no way to turn them on — the settings route refuses an API key.
 */

/** Settings that mean something only on an install that can isolate an app. */
const ISOLATED_ONLY = new Set([
  "default_execution_mode",
  "app_cpu_request_millicores",
  "isolated_idle_sleep_minutes",
  "isolated_sleep_window_enabled",
  "isolated_sleep_window_start",
  "isolated_sleep_window_end",
]);

/** The window's bounds, which govern nothing while the window is off. */
const SLEEP_WINDOW_BOUNDS = new Set(["isolated_sleep_window_start", "isolated_sleep_window_end"]);

export interface SettingsVisibilityFacts {
  background_jobs_enabled: boolean;
  isolated_possible: boolean;
  host_jobs_possible: boolean;
  isolated_sleep_window_enabled?: boolean;
}

/** The job limits whose names do not start with `job_`. */
const JOBS_ONLY = new Set([
  "default_job_timeout_seconds",
  "max_job_timeout_seconds",
  "max_queued_runs_per_app",
]);

export function settingHidden(id: string, facts: SettingsVisibilityFacts | undefined): boolean {
  if (!(facts?.background_jobs_enabled ?? false) && (id.startsWith("job_") || JOBS_ONLY.has(id))) {
    return true;
  }
  if (!facts) return false;
  if (!facts.isolated_possible && ISOLATED_ONLY.has(id)) return true;
  if (SLEEP_WINDOW_BOUNDS.has(id) && !facts.isolated_sleep_window_enabled) return true;
  if (id === "background_jobs_enabled") {
    return !facts.isolated_possible && !facts.host_jobs_possible;
  }
  return false;
}

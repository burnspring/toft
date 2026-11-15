import { describe, expect, it } from "vitest";
import { settingHidden } from "./settings-visibility";

const facts = (over: Partial<Parameters<typeof settingHidden>[1] & object> = {}) => ({
  background_jobs_enabled: false,
  isolated_possible: false,
  host_jobs_possible: true,
  ...over,
});

describe("settingHidden", () => {
  it("keeps the jobs switch on an install that runs jobs on this server", () => {
    // ECS, the one-box bundle: no cluster, and runs go to the host (ADR §106).
    expect(settingHidden("background_jobs_enabled", facts())).toBe(false);
  });

  it("hides the jobs switch only when a run has nowhere to go", () => {
    expect(settingHidden("background_jobs_enabled", facts({ host_jobs_possible: false }))).toBe(true);
    expect(
      settingHidden("background_jobs_enabled", facts({ host_jobs_possible: false, isolated_possible: true })),
    ).toBe(false);
  });

  it("still drops the rows that only govern isolated apps", () => {
    for (const id of ["default_execution_mode", "app_cpu_request_millicores"]) {
      expect(settingHidden(id, facts())).toBe(true);
      expect(settingHidden(id, facts({ isolated_possible: true }))).toBe(false);
    }
    // The CPU limit is held by the shared backend's cgroup too.
    expect(settingHidden("app_cpu_limit_millicores", facts())).toBe(false);
  });

  it("shows isolated idle sleep only where apps can be isolated, and the window's hours only while it is on", () => {
    for (const id of ["isolated_idle_sleep_minutes", "isolated_sleep_window_enabled"]) {
      expect(settingHidden(id, facts())).toBe(true);
      expect(settingHidden(id, facts({ isolated_possible: true }))).toBe(false);
    }
    for (const id of ["isolated_sleep_window_start", "isolated_sleep_window_end"]) {
      expect(settingHidden(id, facts({ isolated_possible: true }))).toBe(true);
      expect(settingHidden(id, facts({ isolated_possible: true, isolated_sleep_window_enabled: true }))).toBe(false);
    }
  });

  it("hides the job figures while jobs are off", () => {
    expect(settingHidden("job_log_retention_days", facts())).toBe(true);
    expect(settingHidden("job_log_retention_days", facts({ background_jobs_enabled: true }))).toBe(false);
    for (const id of ["max_job_timeout_seconds", "job_memory_limit_mb", "job_cpu_limit_millicores", "max_queued_runs_per_app"]) {
      expect(settingHidden(id, facts())).toBe(true);
      expect(settingHidden(id, facts({ background_jobs_enabled: true }))).toBe(false);
    }
  });
});

import type { Enforcement } from "@/lib/api";

/**
 * How a limit is held, in the words beside it (ADR §128). The three are the
 * server's — `runtime.Enforcement` — and the page only says them: a limit the
 * kernel holds, one a watchdog samples after the fact, and one nothing holds.
 */
export function enforcementPhrase(e: Enforcement | undefined): string | undefined {
  switch (e) {
    case "enforced":
      return "Held by the kernel.";
    case "sampled":
      return "Checked by sampling, so it can be passed briefly before the app is stopped.";
    case "not_enforced":
      return "Not enforced on this install.";
    default:
      return undefined;
  }
}

/** The last hour at the CPU limit, in minutes, or undefined when nothing was
 *  measured: `held` of `work` minutes, and the share. */
export function throttledHour(
  t: { periods: number; throttled_periods: number } | undefined,
): { held: number; work: number; percent: number } | undefined {
  if (!t || t.periods <= 0) return undefined;
  // One enforcement period is 100 ms.
  return {
    held: Math.round((t.throttled_periods * 0.1) / 60),
    work: Math.round((t.periods * 0.1) / 60),
    percent: Math.round((t.throttled_periods / t.periods) * 100),
  };
}

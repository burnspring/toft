import { DEPLOY_IN_PROGRESS } from "@/pages/app/state";
import type { Deployment } from "@/lib/api";

/**
 * Which of an app's two logs the Logs tab opens on, and which build the build
 * log is showing.
 *
 * An app has two logs and they answer two different questions. The run log is
 * *this release's* output and goes with the workload. The build log is the
 * deploy that made the release and is kept with the build. Nothing in the
 * product ever said which was which, and the two lived on different tabs — one
 * of them behind a modal with no address — so "which log do I want" was a thing
 * every person had to work out once, alone, usually while something was broken.
 *
 * The switch is a **time** seam rather than a subject one, which is why it is a
 * segmented control inside the tab rather than a second tab: the run log's own
 * App/Platform split already refused the tab bar for the same reason (ADR §87).
 *
 * The rules live here rather than in the tab because they are the part worth
 * asserting, and `npm test` runs without a DOM (ADR §50).
 */
export type LogSource = "run" | "build";

/** The two facts the default reads, and no others. */
export interface LogSourceFacts {
  /** A build is in flight for this app right now. */
  building: boolean;
  /**
   * The most recent build failed and is not the release being served — which
   * is the app that looks healthy and is running last week's code.
   */
  lastFailed: boolean;
}

/**
 * Which log to open on, decided by the product rather than by the reader.
 *
 * This is the whole *when do I use what* question, answered from evidence the
 * page already has. A build in flight and a build that just failed are the two
 * moments the build log is the only log that can say anything; every other
 * moment the app is running and its own output is the answer.
 */
export function defaultLogSource(facts: LogSourceFacts): LogSource {
  return facts.building || facts.lastFailed ? "build" : "run";
}

/**
 * The source named in the URL, or the default when it names nothing.
 *
 * An unrecognised value is the default rather than an error: this is a link
 * somebody may have typed or truncated, and the page it addresses exists either
 * way.
 */
export function readLogSource(
  param: string | null | undefined,
  fallback: LogSource,
): LogSource {
  return param === "run" || param === "build" ? param : fallback;
}

/**
 * Which build the build log is showing.
 *
 * The requested one when the URL names a build this caller can see — which is
 * what makes a build log a thing you can send to somebody (ADR §87) — the one
 * in flight when there is one, and the newest otherwise. A build id that names
 * nothing in the list falls through rather than emptying the pane: the list is
 * already filtered by what this install still holds, and a pruned build is not
 * an error state.
 */
export function pickBuild(
  deployments: Deployment[],
  requested?: string | null,
): Deployment | undefined {
  if (requested) {
    const named = deployments.find((d) => d.id === requested);
    if (named) return named;
  }
  return deployments.find((d) => DEPLOY_IN_PROGRESS.has(d.status)) ?? deployments[0];
}

/**
 * Where an app stands between its newest build and the release it is serving.
 *
 * Two derivations on the app page folded "nothing is serving" into a branch
 * whose copy asserts that something is:
 *
 *   lastDeployFailed = latest?.status === "failed" && (!serving || serving.id !== latest.id)
 *   current          = !latest || !serving || latest.id === serving.id
 *
 * The `!serving` disjunct is the **never-served** case, and in both places it
 * came out as the reassuring answer. On an app whose first deploy failed, the
 * banner read *"the app is still serving the previous release … Nothing is
 * down, so nothing else reports a problem: the app kept serving"* three inches
 * above a Health card reading SERVING *"Nothing deployed yet"* and LIVE SINCE
 * *"—"*; and the Up-to-date fact rendered a green tick and *"Serving the
 * latest build"* for an app that has never served anything.
 *
 * So the three cases the product actually has get a name each, and both
 * readers ask this rather than re-deriving. The table test beside this file is
 * the guard: on screen the wrong answer is a green tick nobody questions.
 */

/** The parts of a deployment row this needs. Structural, so the tests can
 *  tabulate it without building a whole `Deployment`. */
export interface DeployRow {
  id: string;
  status: string;
}

export type DeployStanding =
  /** Nothing has ever served. There may still be builds — every one of them
   *  failed, or the first is in flight. */
  | "never_served"
  /** Serving something, and it is the newest build. */
  | "serving_latest"
  /** Serving something older than the newest build. */
  | "serving_older";

/**
 * @param latest  the newest build row, or undefined when there are none
 * @param serving the row that owns the code actually executing, or undefined
 */
export function deployStanding(
  latest: DeployRow | undefined,
  serving: DeployRow | undefined,
): DeployStanding {
  if (!serving) return "never_served";
  if (!latest || latest.id === serving.id) return "serving_latest";
  return "serving_older";
}

/**
 * Whether the banner about a failed deploy applies — the one whose copy says
 * the app kept serving.
 *
 * It needs the newest build to have failed **and** something to still be
 * serving. A first deploy that failed is a different sentence, and
 * `firstDeployFailed` is that one.
 */
export function lastDeployFailedOverServing(
  latest: DeployRow | undefined,
  serving: DeployRow | undefined,
): boolean {
  if (latest?.status !== "failed") return false;
  return deployStanding(latest, serving) === "serving_older";
}

/** A failed newest build on an app that has never served anything. */
export function firstDeployFailed(
  latest: DeployRow | undefined,
  serving: DeployRow | undefined,
): boolean {
  if (latest?.status !== "failed") return false;
  return deployStanding(latest, serving) === "never_served";
}

/**
 * Whether the Up-to-date fact may claim the app is serving the latest build.
 *
 * Never true for an app that has served nothing: "up to date" about no running
 * code is a fact true only by omission, which is the rule the `!latest` branch
 * already followed and the `!serving` disjunct broke.
 */
export function servingLatest(
  latest: DeployRow | undefined,
  serving: DeployRow | undefined,
): boolean {
  if (!latest) return false;
  return deployStanding(latest, serving) === "serving_latest";
}

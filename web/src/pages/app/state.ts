/**
 * Two facts about an app's lifecycle that more than one tab has to agree on.
 *
 * They lived beside the page shell while every tab lived in the same file. Now
 * that the tabs are their own modules, a second copy of either would be a
 * second definition of "is this app still deploying" — which is exactly the
 * drift the status table in `lib/status.ts` exists to prevent.
 */

/** The deploy statuses that mean *still happening*, and so keep the poll fast. */
export const DEPLOY_IN_PROGRESS = new Set(["pending", "building"]);

/**
 * How often the app page re-reads its deployments.
 *
 * Fast while a row is in flight *or* the app row says `building` — a build
 * started by auto-deploy, `toft deploy` or another editor is not in a list read
 * before it began, so the list alone would never notice it. Otherwise the app
 * query's own idle cadence, never off: a build that started and ended between
 * two app polls changes no status this page sees, and without this its
 * failure banner never appeared.
 */
export function deploymentsPollMs(
  deployments: { status: string }[] | undefined,
  appStatus: string | undefined,
): number {
  if (appStatus === "building") return 3000;
  if ((deployments ?? []).some((d) => DEPLOY_IN_PROGRESS.has(d.status))) return 3000;
  return 15000;
}

/**
 * What a 202 start is told to say.
 *
 * The server answers 202 when a start outlived its hold, so nothing here may
 * say the app is back — an isolated start is a pod schedule and an image pull,
 * and the status pill is what reports the outcome when it arrives.
 */
export const RESTART_IN_FLIGHT =
  "The app is starting. This can take a minute in isolated mode, while the pod is scheduled and its image pulled.";

/**
 * The one act the app page's header leads with — the filled button.
 *
 * It was **Open** whatever the app was doing, so a stopped app's page offered
 * to open something that would answer with an interstitial while Start sat in
 * the ⋯ menu. The primary is what the state asks for next:
 *
 * - `start` — stopped, and this caller may start it (not locked).
 * - `restart` — crashed, for the same caller. The logs say why; this is the act.
 * - `build_log` — the first deploy failed, so there is nothing to open and the
 *   build log is where the answer is.
 * - `open` — anything else that serves pages. Sleeping included: opening it is
 *   what wakes it.
 * - `none` — a batch app (ADR §107) with nothing above to do.
 *
 * Open stays in the header as an outlined secondary whenever it is not the
 * primary, so the address is never more than one click away.
 */
export type HeaderPrimary = "start" | "restart" | "build_log" | "open" | "none";

export function headerPrimary({
  status,
  canEdit,
  locked,
  batch,
  firstDeployFailed,
}: {
  status: string;
  canEdit: boolean;
  locked: boolean;
  batch: boolean;
  firstDeployFailed: boolean;
}): HeaderPrimary {
  if (canEdit && firstDeployFailed) return "build_log";
  if (batch) return "none";
  if (canEdit && !locked && status === "stopped") return "start";
  if (canEdit && !locked && status === "crashed") return "restart";
  return "open";
}

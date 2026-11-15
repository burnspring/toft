/**
 * The app detail page's tab vocabulary, and which of them a caller actually
 * has.
 *
 * Here rather than inside the page because this is the one statement of *who
 * sees what* on the product's busiest surface, and a rule that lives inside a
 * 5,000-line component is a rule nobody can test. Both consumers are in
 * `app-detail.tsx` — the strip that renders the triggers, and the redirect that
 * bounces a URL naming a tab this caller does not have — and they must agree,
 * because the two disagreeing is exactly what ADR §53 D5 was: a strip with
 * nothing selected over an empty page.
 *
 * **The rule is what the server will answer, never what the tab looks like.**
 * A tab whose endpoints refuse this caller is not a tab that renders empty; it
 * is a tab that does not exist for them. Three gates, and each one is a
 * server-side fact this page has already fetched:
 *
 *   - `can_edit` — Settings, Automation, Integrations, Dependencies and
 *     **Reach**, whose writes and reads are all `ActionEditContent`; and
 *     **Logs and Deployments**, whose endpoints are gated by `logsAction`,
 *     which is deliberately stricter than read: build logs are the raw stdout
 *     of the build environment and runtime logs are whatever the app printed,
 *     so a viewer on a shared or public app must not see them.
 *
 *     Reach is on that gate for a sharper version of Dependencies' reason: a
 *     viewer keeps reading `/apps/{id}/storage`, which is deliberately open to
 *     anyone who can read the app, and what they do not get is the *assembly*
 *     — public, writable storage, three credentials and it may act as the
 *     viewer is a sentence about how to attack the install, and no part of it
 *     alone is.
 *   - a dependency verdict exists — the policy can be off install-wide.
 *   - `can_manage_grants || can_change_visibility` — Access.
 *
 * What is left for a viewer is Overview and Usage, and both are the app's own
 * read rule on the server: what the app is, who owns it, where it lives, and
 * aggregate counts that name nobody.
 */
export const APP_TABS = [
  "overview",
  "usage",
  "automation",
  "logs",
  "deployments",
  "dependencies",
  "integrations",
  "reach",
  "audit",
  "access",
  "settings",
] as const;

export type AppTab = (typeof APP_TABS)[number];

/**
 * The Access tab was called Sharing, and Automation was two tabs — Scheduled
 * (`cron`) and Jobs — before ADR §33 merged them. Preview folded into Overview
 * and Events into Logs. Old links, including the ones Admin → Automation and
 * every past email point to, keep working.
 */
export const LEGACY_APP_TABS: Record<string, AppTab> = {
  sharing: "access",
  cron: "automation",
  jobs: "automation",
  preview: "overview",
  events: "logs",
};

/** The permission facts a tab decision reads, and no others. */
export type AppTabPermissions = {
  can_edit: boolean;
  can_manage_grants: boolean;
  can_change_visibility: boolean;
};

/**
 * Which tabs this caller has, given their permissions and whether this install
 * produced a dependency verdict for the app.
 *
 * `perms` undefined means the answer has not landed yet, and the reply is the
 * two tabs *everybody* who reached this page has: reading it at all is the app's
 * read rule, so Overview and Usage cannot be wrong. The strip therefore paints
 * once and gains tabs, rather than appearing a round trip late. Nothing
 * redirects on this value — the guard in the page waits for the real answer.
 */
export function appTabsFor(
  perms: AppTabPermissions | undefined,
  hasDependencyVerdict: boolean,
): Set<AppTab> {
  const has = new Set<AppTab>(["overview", "usage"]);
  if (!perms) return has;

  if (perms.can_edit) {
    has
      .add("logs")
      .add("deployments")
      .add("settings")
      .add("automation")
      .add("integrations")
      .add("reach")
      // The app's audit file: reach's assembly plus its history, on reach's
      // gate. Sections an editor may not have are shown refused, never hidden.
      .add("audit");
    if (hasDependencyVerdict) has.add("dependencies");
  }
  if (perms.can_manage_grants || perms.can_change_visibility) has.add("access");
  return has;
}

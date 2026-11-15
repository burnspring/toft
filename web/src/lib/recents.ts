/**
 * The apps you were just looking at.
 *
 * Kept in `localStorage` and nowhere else, deliberately. It is a convenience
 * for one person on one machine, and the alternative — a column, an endpoint,
 * a write on every app page load — would put "which apps has this person been
 * reading" into the database, which is a surveillance record nobody asked for
 * and the platform would then have to defend. The same reasoning `internal/presence`
 * gives for never persisting: some things are worth less than the questions
 * storing them invites.
 *
 * Ids only. A name or a slug held here would go stale, and rendering a stale
 * name for an app that has been renamed is worse than looking it up.
 */

const KEY = "toft.recent-apps";
const MAX = 5;

export function recentAppIds(): string[] {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

/** Most recent first, deduped, capped. Silent on a storage that refuses. */
export function rememberApp(id: string) {
  if (!id) return;
  try {
    const next = [id, ...recentAppIds().filter((x) => x !== id)].slice(0, MAX);
    localStorage.setItem(KEY, JSON.stringify(next));
  } catch {
    // Private mode, a full quota, a locked-down browser. A convenience that
    // cannot be stored is a convenience that is absent, not an error.
  }
}

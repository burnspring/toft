/* ------------------------------------------------------------------ *
 * The app Settings tab's form contract
 *
 * Which fields on one app batch into the pending-changes bar, and the
 * bounds each is held to — mirrored from `internal/api/app_handlers.go`.
 * Beside `settings-index.ts` for the same reason its install-wide twin
 * is: two things need it and one of them is a test.
 *
 * The switches are deliberately absent. Each saves the moment it is
 * thrown, because the server's answer belongs to the act that caused it
 * — "the release currently serving was started shared", "failure email
 * is off for everyone on this app, not just you" — and a warning that
 * arrives attached to a save of five unrelated things has lost the one
 * thing that made it useful. Environment variables are absent too, and
 * for the opposite reason: they are one decision saved as a set, on the
 * same ground `source-policy-editor.tsx` earns its own Save.
 * ------------------------------------------------------------------ */

/** The fields that collect in a draft and go out as one request. */
export type AppFieldKey = "name" | "slug" | "branch" | "repo_subdir" | "git_token" | "releases_kept";

export const APP_FIELD_KEYS: AppFieldKey[] = [
  "name",
  "slug",
  "branch",
  "repo_subdir",
  "git_token",
  "releases_kept",
];

/**
 * The `id` each field's input carries.
 *
 * The same string the settings index claims as that setting's `control`, and
 * the guard test holds the two against each other — a field renamed here and
 * not there would leave `/apps/{id}/settings#source` pointing at an input that
 * no longer exists, which is exactly the failure the index is for.
 */
export const APP_FIELD_CONTROL: Record<AppFieldKey, string> = {
  name: "app-name",
  slug: "app-slug",
  branch: "app-branch",
  repo_subdir: "app-repo-subdir",
  git_token: "git-token",
  releases_kept: "releases-kept",
};

/** Mirrors `maxAppNameLen`. Names render in HTML and in mail subjects. */
export const MAX_APP_NAME = 100;

/**
 * Mirrors `sanitizeSlug` server-side.
 *
 * The API does not *refuse* a slug — it transforms one, silently, and stores
 * the result. So the page's job here is not validation but a preview: what you
 * typed is not always what this app's URL becomes, and finding that out from
 * the address bar afterwards is how somebody comes to believe the rename
 * failed.
 */
export function sanitizeSlug(raw: string): string {
  const s = raw
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/^-+|-+$/g, "");
  return s === "" ? "app" : s;
}

/** What a field shows when what is typed cannot be saved as it stands. */
export function appFieldError(key: AppFieldKey, raw: string): string | null {
  if (key === "name") {
    const v = raw.trim();
    if (v === "") return "An app needs a name.";
    if (v.length > MAX_APP_NAME) return `At most ${MAX_APP_NAME} characters.`;
    // Mirroring validateAppName, which refuses exactly these: a name renders
    // into mail subjects and into HTML.
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f]/.test(v)) return "No control characters.";
    return null;
  }
  if (key === "slug") {
    return raw.trim() === "" ? "An app needs a slug — it is the last part of its address." : null;
  }
  if (key === "branch") {
    return raw.trim() === "" ? "Name the branch to build the tip of." : null;
  }
  if (key === "repo_subdir") {
    // A mirror of builder.ValidateSubdir's refusals that a person can make by
    // typing; the server is the authority and says the rest in its own words.
    const v = raw.trim().replace(/^\/+|\/+$/g, "");
    if (v === "") return null; // blank is the repository's root
    const segs = v.split("/");
    if (v.includes("\\")) return "Use / between folders.";
    if (segs.some((s) => s === "" || s === "." || s === "..")) return "A folder inside the repository, like apps/web.";
    if (segs.some((s) => s.toLowerCase() === ".git")) return "Not a folder inside .git.";
    return null;
  }
  if (key === "releases_kept") {
    const v = raw.trim();
    if (v === "") return null; // blank means "follow the platform default"
    const n = Number(v);
    return Number.isInteger(n) && n >= 1
      ? null
      : "Must be a whole number of 1 or more, or blank for the install default.";
  }
  return null;
}

/**
 * What a field says under itself when this app's value has something to add.
 *
 * Only the slug has one, and it is the transform rather than a rule: a note is
 * what a silent normalization needs, where a refusal would need an error.
 */
export function appFieldNote(key: AppFieldKey, raw: string, stored: string): string | undefined {
  if (key !== "slug") return undefined;
  const v = raw.trim();
  if (v === "" || v === stored) return undefined;
  const clean = sanitizeSlug(v);
  return clean === v ? undefined : `Saved as ${clean} — a slug is lowercase letters, digits and dashes.`;
}

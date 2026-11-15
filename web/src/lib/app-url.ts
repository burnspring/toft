/**
 * Deployed apps are served from their own origin (same host, different port) so
 * that code inside an app cannot reach the platform API as the viewer. The SPA
 * therefore cannot build app links from window.location — it asks the server.
 *
 * Until the config lands, links stay relative: the platform redirects
 * /apps/{slug} to the gateway, so they work either way.
 */

let appsBaseUrl = "";

/** What `GET /api/v1/config` answers, as far as the SPA reads it. */
export interface PublicConfig {
  apps_base_url: string;
  /** The manuals' folder for this install's minor; "" under DOCS_URL=off. */
  docs_url: string;
}

let configPromise: Promise<PublicConfig | null> | null = null;

/**
 * The public config, fetched once per page load and shared: `App` starts it at
 * boot and the landing page reads the same answer rather than asking again.
 * Null when the read fails.
 */
export function publicConfig(): Promise<PublicConfig | null> {
  configPromise ??= readPublicConfig();
  return configPromise;
}

async function readPublicConfig(): Promise<PublicConfig | null> {
  try {
    const res = await fetch("/api/v1/config", { credentials: "include" });
    return res.ok ? ((await res.json()) as PublicConfig) : null;
  } catch {
    // Callers fall back: relative app links, and no documentation link.
    return null;
  }
}

export async function loadAppUrlConfig(): Promise<void> {
  const cfg = await publicConfig();
  // Left relative on a failed read; the platform-side redirect covers it.
  appsBaseUrl = cfg?.apps_base_url || "";
}

/**
 * An app as these functions read it: a slug, or the app itself. Given the app,
 * the server's `public_url` decides — an app moved to a hostname of its own is
 * served at that origin's root, which the SPA cannot work out from the slug and
 * must never compose itself.
 */
export type AppAddress = string | { slug: string; public_url?: string };

/** The app's own origin when it lives at the root of a hostname, else null. */
function hostOrigin(app: AppAddress): string | null {
  if (typeof app === "string" || !app.public_url) return null;
  try {
    const u = new URL(app.public_url);
    return u.pathname === "/" || u.pathname === "" ? u.origin : null;
  } catch {
    return null;
  }
}

/** Absolute URL of a deployed app, optionally with a sub-path. */
export function appUrl(app: AppAddress, path = "/"): string {
  const suffix = path.startsWith("/") ? path : `/${path}`;
  const host = hostOrigin(app);
  if (host) return `${host}${suffix}`;
  const slug = typeof app === "string" ? app : app.slug;
  return `${appsBaseUrl}/apps/${slug}${suffix}`;
}

/** The address shown to users: the hostname when the app has one, else its path. */
export function appDisplayPath(app: AppAddress): string {
  const host = hostOrigin(app);
  if (host) return new URL(host).host;
  return `/apps/${typeof app === "string" ? app : app.slug}`;
}

/**
 * The address to hand to somebody else — absolute, always.
 *
 * `appUrl` is deliberately relative on a single-origin install, which is right
 * for an `href` and useless the moment the string leaves the page: `/apps/foo/`
 * pasted into a ticket, a chat message or a spreadsheet cell names nothing, and
 * the person who receives it cannot tell which install it meant. So anything
 * that copies, exports or mails an app's address takes this instead.
 *
 * On a split-origin install `appUrl` is already absolute and the base is
 * ignored; on a single-origin one the apps are served from the platform's own
 * origin, which is exactly what the page is loaded from.
 */
export function appShareUrl(app: AppAddress, path = "/"): string {
  const relative = appUrl(app, path);
  try {
    return new URL(relative, window.location.origin).toString();
  } catch {
    return relative;
  }
}

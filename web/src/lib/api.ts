// The one type in this file that is declared elsewhere. `removed.ts` renders it
// and is tested under the node project, which has no `lib.dom` — so the type
// lives there and is re-exported here, where every other API type lives.
import type { RemovedEntry } from "./removed";
import { noteFetchError, noteResponse } from "./connection";
export type { RemovedEntry } from "./removed";

const BASE = "/api/v1";

/**
 * Whether this tab has ever held a signed-in user.
 *
 * It is the one fact that tells a 401 meaning "your session ended" apart from a
 * 401 meaning "you were never signed in", and without it every expiry reads as
 * a bounce to the login page for no reason. `sessionStorage` rather than a
 * module variable because the ordinary shape of an expiry is a laptop that
 * slept and a page that was then reloaded — a variable is gone by the moment
 * the answer is wanted. Per-tab, and cleared on sign-out, so a deliberate exit
 * never reads as an expiry.
 *
 * Both accesses are guarded: storage is unavailable in some privacy modes, and
 * a page that cannot say *why* somebody is signing in again must still let them
 * sign in.
 */
const SESSION_HELD_KEY = "toft.session_held";

function setSessionHeld(held: boolean) {
  try {
    if (held) sessionStorage.setItem(SESSION_HELD_KEY, "1");
    else sessionStorage.removeItem(SESSION_HELD_KEY);
  } catch {
    /* storage unavailable — the notice is the only thing lost */
  }
}

function sessionHeld(): boolean {
  try {
    return sessionStorage.getItem(SESSION_HELD_KEY) === "1";
  } catch {
    return false;
  }
}

/**
 * A refusal, carrying the thing every page needed and none of them had.
 *
 * Errors from this module used to be a bare `Error` with a message, so no page
 * could tell a 403 from a 500 — and thirteen admin pages resolved that by
 * asserting "Administrators only" for every failure, which tells an
 * administrator watching their own install fall over that they lack a
 * permission they hold. The status is what makes those three sentences
 * possible, and `body` carries the rest of the server's own answer (a `reason`,
 * a retry-after) for the handful of places that read it.
 *
 * `message` stays the server's `error` string, so every existing call site —
 * `err instanceof Error ? err.message : …` — keeps working unchanged.
 */
/** One mapping a user or group delete was refused over (409
 *  `holds_storage_mappings`): the subject's delegation attaches it. */
export interface HeldStorageMapping {
  resource_id: string;
  resource_name: string;
  app_id: string;
  app_slug: string;
  app_name: string;
  path: string;
}

export class ApiError extends Error {
  readonly status: number;
  readonly body: Record<string, unknown>;

  constructor(
    status: number,
    message: string,
    body: Record<string, unknown> = {},
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

/** The status behind a failure, or `undefined` where it was not an API refusal
 *  — a network error, an aborted request, a bug in a handler. The distinction
 *  matters: "we could not ask" and "it said no" are different sentences. */
export function statusOf(error: unknown): number | undefined {
  return error instanceof ApiError ? error.status : undefined;
}

/** The refusal code the API puts on a 403 body, or undefined.
 *
 *  It is how a page tells "your system role is too low" from "you have no grant
 *  on this app" — and, since the `list` rung, how it tells a refusal it should
 *  render an error for from one it should render the access gate for. */
export function refusalCodeOf(error: unknown): string | undefined {
  if (!(error instanceof ApiError)) return undefined;
  const code = error.body["code"];
  return typeof code === "string" ? code : undefined;
}

/** Whether a failure is this caller being refused rather than something
 *  breaking. 401 never reaches a page — `send` bounces it. */
export function isRefusal(error: unknown): boolean {
  const status = statusOf(error);
  return status === 401 || status === 403 || status === 404;
}

/**
 * Every call's status handling, whatever its body: the connection reading, the
 * 401 bounce, the locked-account bounce and the `ApiError`. `request` is JSON;
 * the uploads and the CSV download build their own request and come through
 * here too, because a session that ends during an upload is discovered by the
 * upload — and was rendered as an upload failure while they bypassed it.
 */
async function send(path: string, init: RequestInit): Promise<Response> {
  let res: Response;
  try {
    res = await fetch(`${BASE}${path}`, { credentials: "include", ...init });
  } catch (err) {
    noteFetchError(err);
    throw err;
  }
  noteResponse(res);

  // The local sign-in and password routes answer a wrong password with 401 and
  // a sentence; that is the caller's answer, not a session that ended. Bounced,
  // a mistyped current password on Me › Security sent a signed-in person to
  // the login page as "expired", form and all (code review 2026-11-11, O-2).
  if (res.status === 401 && path.startsWith("/auth/local/")) {
    const body = await res.json().catch(() => ({ error: "incorrect username or password" }));
    throw new ApiError(401, body.error ?? "incorrect username or password", body);
  }

  if (res.status === 401) {
    // `/` answers an anonymous visitor with the landing page, and the router is
    // the only thing that decides that — so this handler has to stay out of the
    // way of the 401 that discovers it. The `GET /me` on first paint is exactly
    // that 401, and bouncing it here would put the login page back at `/` with
    // no route in sight to explain why.
    //
    // Narrow on purpose: a session that ends while somebody is *using* the
    // dashboard still lands on the login page, because `sessionHeld()` is the
    // difference between "your session ended" and "you were never signed in" —
    // the same fact `?expired=1` is already keyed on.
    const anonymousAtRoot = window.location.pathname === "/" && !sessionHeld();
    if (window.location.pathname !== "/login" && !anonymousAtRoot) {
      // Keep where they were, so a session that expires mid-use returns them
      // there rather than to the dashboard. Site-relative only — the server
      // validates the target, and there is no reason to hand it an origin.
      const target = window.location.pathname + window.location.search;
      const params = new URLSearchParams();
      if (target !== "/") params.set("redirect", target);
      // Only when this tab was signed in. On an install that forwards to a
      // single provider the person may never see the login page at all, which
      // is the intended outcome — the sentence is for whoever does.
      if (sessionHeld()) params.set("expired", "1");
      const query = params.toString();
      window.location.href = query ? `/login?${query}` : "/login";
    }
    throw new ApiError(401, "Unauthorized");
  }

  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    // A locked account is a refusal the person can do something about, and the
    // login page is where the administrator's explanation is rendered. Handled
    // here rather than at the call site because the session dies mid-use: any
    // request can be the one that discovers it.
    if (res.status === 403 && body.reason === "locked") {
      if (window.location.pathname !== "/login") {
        window.location.href = "/login?denied=locked";
      }
    }
    throw new ApiError(res.status, body.error || res.statusText, body);
  }
  return res;
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const res = await send(path, {
    headers: { "Content-Type": "application/json", ...options?.headers },
    ...options,
  });
  if (res.status === 204) return undefined as T;
  return res.json();
}

// Auth
export const api = {
  getMe: async () => {
    const user = await request<User>("/auth/me");
    setSessionHeld(true);
    return user;
  },
  // Cleared whether or not the request succeeded: the person is leaving either
  // way, and a stale marker would greet their next visit with "your session
  // ended" for a session they closed themselves.
  logout: async () => {
    try {
      await request<void>("/auth/session", { method: "DELETE" });
    } finally {
      setSessionHeld(false);
    }
  },

  /** The zone this person reads times in: "", "browser", "UTC" or an IANA name. */
  setMyTimeZone: (time_zone: string) =>
    request<TimeZonePreference>("/me/time-zone", {
      method: "PUT",
      body: JSON.stringify({ time_zone }),
    }),

  // Slug check
  checkSlug: (slug: string) =>
    request<{ available: boolean }>(`/slugs/${slug}/available`),

  // Dependency policy (ADR §39).
  //
  // The app-scoped reads require can_edit and the waiver writes require an
  // administrator; both are enforced server-side, and the UI hides rather than
  // disables what a viewer may not reach.
  getAppDependencies: (id: string) =>
    request<DependencyState>(`/apps/${id}/dependencies`),
  /** Re-derives findings from the *stored* inventory: no registry, no
   *  resolver, and deliberately not a re-read of the manifest — that needs a
   *  build, because a floating range resolves differently on a different day. */
  recheckAppDependencies: (id: string) =>
    request<DependencyState>(`/apps/${id}/dependency-recheck`, {
      method: "POST",
    }),
  grantDependencyWaiver: (
    id: string,
    body: {
      advisory_id: string;
      package?: string;
      reason: string;
      days: number;
    },
  ) =>
    request<DependencyWaiver>(`/apps/${id}/dependency-waivers`, {
      method: "POST",
      body: JSON.stringify(body),
    }),
  revokeDependencyWaiver: (id: string, waiverID: string) =>
    request<void>(`/apps/${id}/dependency-waivers/${waiverID}`, {
      method: "DELETE",
    }),

  /** One app's scan history — the governance read, and administrator-only.
   *  The owner sees the latest reading on their own tab; a stack of readings
   *  is a statement about response, which is a different question. */
  getAppDependencyScans: (id: string, limit?: number) =>
    request<DependencyScanHistory>(
      `/admin/apps/${id}/dependency-scans${limit ? `?limit=${limit}` : ""}`,
    ),

  getAdminDependencies: () =>
    request<AdminDependencyState>("/admin/dependencies"),
  /** By advisory id **or** package name — the second is the one that gets
   *  used, because the news arrives as "there's something going round in
   *  event-stream" hours before anybody has an identifier. */
  searchDependencies: (q: string) =>
    request<DependencySearchResult>(
      `/admin/dependencies/search?q=${encodeURIComponent(q)}`,
    ),
  /** The inventory rather than the advisories: every app carrying the package,
   *  affected or not. A sibling of searchDependencies, never a flag on it —
   *  there a row is an app with findings, here it is an occurrence. */
  searchPackages: (q: string, version?: PackageVersionFilter) => {
    const params = new URLSearchParams({ q });
    for (const [k, v] of Object.entries(version ?? {})) {
      if (v) params.set(k, v);
    }
    return request<PackageSearchResult>(`/admin/packages?${params}`);
  },
  /** No `q` is the estate's package list — the question a search box cannot be
   *  used for, because you have to know the name to type it. */
  getPackageCatalog: () => request<PackageSearchResult>("/admin/packages"),

  /** The estate by language. Administrator-only; the route 404s otherwise. */
  getEstateLanguages: () => request<EstateCensus>("/admin/estate/languages"),
  /** The same estate as addresses, for whoever is pointing a scanner at it.
   *  A third route rather than a flag on the census: a row there is a language
   *  and a count, a row here is an app to visit. */
  getEstateTargets: () => request<EstateTargets>("/admin/estate/targets"),
  recheckAllDependencies: () =>
    request<void>("/admin/dependency-recheck", { method: "POST" }),
  syncAdvisoryFeed: () =>
    request<void>("/admin/dependency-feed/sync", { method: "POST" }),
  setAdvisoryFeedCredential: (credential: string) =>
    request<void>("/admin/dependency-feed/credential", {
      method: "PUT",
      body: JSON.stringify({ credential }),
    }),

  // Apps
  /** Optionally narrowed to one tag, and one of its values. Filtering happens inside the same visibility-scoped query as the unfiltered listing. */
  listApps: (filter?: { tag?: string; value?: string }) => {
    const params = new URLSearchParams();
    if (filter?.tag) params.set("tag", filter.tag);
    if (filter?.value) params.set("value", filter.value);
    const qs = params.toString();
    return request<AppWithMetrics[]>(`/apps${qs ? `?${qs}` : ""}`);
  },
  createApp: (data: CreateAppRequest) =>
    request<{ app: App; deployment: Deployment }>("/apps", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  uploadApp: async (
    file: File,
    opts: {
      name?: string;
      slug?: string;
      env_vars?: Record<string, string>;
      env_sensitive?: Record<string, boolean>;
      execution_mode?: ExecutionMode;
    },
  ) => {
    const form = new FormData();
    form.append("file", file);
    if (opts.name) form.append("name", opts.name);
    if (opts.slug) form.append("slug", opts.slug);
    if (opts.env_vars) form.append("env_vars", JSON.stringify(opts.env_vars));
    if (opts.env_sensitive)
      form.append("env_sensitive", JSON.stringify(opts.env_sensitive));
    // A form value beside the rest: this route reads a multipart form, not a
    // JSON body. Same field name, same rules.
    if (opts.execution_mode) form.append("execution_mode", opts.execution_mode);
    const res = await send(`/apps/upload`, { method: "POST", body: form });
    return res.json() as Promise<{
      app: App;
      deployment: Deployment;
      removed?: RemovedEntry[];
    }>;
  },
  getApp: (id: string) => request<AppDetail>(`/apps/${id}`),

  getPlatformSettings: () => request<PlatformSettings>("/system/settings"),

  // Where this install will deploy from. The first four are the
  // administrator's and 404 to everyone else; the fifth is the narrow view
  // every signed-in user may read, so a refusal can name what would work.
  getSourcePolicy: () => request<SourcePolicy>("/system/source-policy"),
  updateSourcePolicy: (data: SourcePolicyInput) =>
    request<{ policy: SourcePolicy; warning: string }>(
      "/system/source-policy",
      {
        method: "PUT",
        body: JSON.stringify(data),
      },
    ),
  previewSourcePolicy: (data: SourcePolicyInput) =>
    request<SourcePolicyPreview>("/system/source-policy/preview", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  getObservedSources: () =>
    request<{ sources: ObservedSource[] | null }>(
      "/system/source-policy/observed",
    ),
  getEffectiveSourcePolicy: () =>
    request<EffectiveSourcePolicy>("/system/source-policy/effective"),

  // The gallery. Every one of these 404s while the mode is off, which is what
  // the nav and the page read to decide whether the gallery exists at all.
  getGallery: () => request<GalleryResponse>("/gallery"),
  syncGallery: () => request<GallerySync>("/gallery/sync", { method: "POST" }),
  deployFromGallery: (itemID: string, data: GalleryDeployRequest) =>
    request<{ app: App; deployment: Deployment }>(`/gallery/${itemID}/deploy`, {
      method: "POST",
      body: JSON.stringify(data),
    }),
  updateFromGallery: (appID: string) =>
    request<Deployment>(`/apps/${appID}/gallery/update`, { method: "POST" }),
  detachFromGallery: (appID: string) =>
    request<{ status: string; message: string }>(`/apps/${appID}/gallery`, {
      method: "DELETE",
    }),
  updatePlatformSettings: (data: UpdatePlatformSettings) =>
    request<{ settings: PlatformSettings; warning: string }>(
      "/system/settings",
      {
        method: "PUT",
        body: JSON.stringify(data),
      },
    ),
  updateApp: (id: string, data: UpdateAppRequest) =>
    request<App>(`/apps/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  updateAppEnv: (id: string, data: UpdateAppEnvRequest) =>
    request<UpdateAppEnvResult>(`/apps/${id}/env`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  deleteApp: (id: string) => request<void>(`/apps/${id}`, { method: "DELETE" }),
  /** Stop offering Open in Garth until a Garth session deploys the app again (ADR §158). */
  unlinkGarth: (id: string) => request<void>(`/apps/${id}/garth-link`, { method: "DELETE" }),

  // App actions
  deploy: (id: string) =>
    request<Deployment>(`/apps/${id}/deploy`, { method: "POST" }),
  uploadAppVersion: async (id: string, file: File) => {
    const form = new FormData();
    form.append("file", file);
    const res = await send(`/apps/${id}/upload`, { method: "POST", body: form });
    return res.json() as Promise<Deployment>;
  },
  rollbackDeployment: (id: string, deployId: string) =>
    request<Deployment>(`/apps/${id}/deployments/${deployId}/rollback`, {
      method: "POST",
    }),

  /**
   * Delete one build's release — its directory on the host and its artifact in
   * the object store — and the deployment row with it. Irreversible: a deleted
   * build can never be made current again.
   */
  deleteDeployment: (id: string, deployId: string) =>
    request<ReleaseReclaim>(`/apps/${id}/deployments/${deployId}`, {
      method: "DELETE",
    }),
  /** The same, for every build except the one being served and any in flight. */
  deleteNonCurrentDeployments: (id: string) =>
    request<ReleaseReclaim>(`/apps/${id}/deployments`, { method: "DELETE" }),

  // Permissions & sharing
  getAppPermissions: (id: string) =>
    request<AppPermissions>(`/apps/${id}/permissions`),
  /** Hand an app to a user or a group. The type defaults server-side to "user",
   *  so an install with no groups never sends the word. */
  transferOwnership: (
    id: string,
    newOwnerId: string,
    newOwnerType: OwnerPrincipalType = "user",
  ) =>
    request<void>(`/apps/${id}/transfer`, {
      method: "POST",
      body: JSON.stringify({
        new_owner_id: newOwnerId,
        new_owner_type: newOwnerType,
      }),
    }),
  /** Move every app somebody owns *personally* to one new owner. The
   *  offboarding action, and the migration path onto group ownership. */
  transferUserApps: (
    userId: string,
    toId: string,
    toType: OwnerPrincipalType,
  ) =>
    request<TransferAppsResult>(`/users/${userId}/transfer-apps`, {
      method: "POST",
      body: JSON.stringify({ to_id: toId, to_type: toType }),
    }),
  searchDirectoryUsers: (q: string) =>
    request<DirectoryUser[]>(`/directory/users?q=${encodeURIComponent(q)}`),
  searchDirectoryGroups: (q: string) =>
    request<DirectoryGroup[]>(`/directory/groups?q=${encodeURIComponent(q)}`),
  listGrants: (id: string) => request<Grant[]>(`/apps/${id}/grants`),
  /**
   * Add a grant, change a role, or both.
   *
   * `notify` is opt-in per share and honoured only for a **person on a new
   * share** — a group is not an actor, and "gave you access" is not true the
   * second time. Every refusal comes back named in `notify_skipped` rather than
   * being dropped, because the person who ticked the box would otherwise assume
   * a message went out.
   */
  putGrant: (
    id: string,
    principalType: "user" | "group",
    principalId: string,
    role: string,
    notify?: { notify: boolean; note?: string },
  ) =>
    request<PutGrantResult>(
      `/apps/${id}/grants/${principalType}/${principalId}`,
      {
        method: "PUT",
        body: JSON.stringify({ role, ...(notify ?? {}) }),
      },
    ),
  deleteGrant: (
    id: string,
    principalType: "user" | "group",
    principalId: string,
  ) =>
    request<void>(`/apps/${id}/grants/${principalType}/${principalId}`, {
      method: "DELETE",
    }),
  /** Start or restart an app. `status: "restarting"` comes back with 202 when
   *  the start outlived the server's hold — an isolated start is a pod
   *  schedule and an image pull — and means it is still running, not that it
   *  failed. Callers must not say the app is back on that answer. */
  /** Wake a sleeping app ahead of the visit an Open button is about to make
   * (ADR §123). 202 started, 204 nothing to wake; never audited. */
  wakeAhead: (id: string) => request<void>(`/apps/${id}/wake`, { method: "POST" }),
  restart: (id: string) =>
    request<{ status: string; detail?: string }>(`/apps/${id}/restart`, {
      method: "POST",
    }),
  /** Lock an app: stops the workload and refuses every visitor with `message`
   * (or the install default when blank). system_admin only — an owner may stop
   * and restart their app, which is exactly why a lock is not a status. */
  lockApp: (id: string, message: string) =>
    request<App>(`/apps/${id}/lock`, {
      method: "POST",
      body: JSON.stringify({ message }),
    }),
  /** Unlock. Does not start the app — it comes back stopped, and somebody
   * presses Start. */
  unlockApp: (id: string) =>
    request<App>(`/apps/${id}/lock`, { method: "DELETE" }),
  stop: (id: string) =>
    request<{ status: string }>(`/apps/${id}/stop`, { method: "POST" }),

  // Usage stats
  getAppUsage: (id: string, days: number = 30) =>
    request<AppUsage>(`/apps/${id}/usage?days=${days}`),
  /** Who produced the traffic. Needs manage_grants on the app — a plain
   * reader gets 403, so only request it when permissions allow. */
  getAppUsageViewers: (id: string, days: number = 30) =>
    request<AppUsageViewers>(`/apps/${id}/usage/viewers?days=${days}`),
  /** The viewers at the stored grain — one line per principal per day — over
   * UTC dates `since`..`until` inclusive, as a CSV file. Same gate as the
   * table. */
  downloadAppUsageViewers: async (id: string, since: string, until: string): Promise<Blob> => {
    const res = await send(`/apps/${id}/usage/viewers?since=${since}&until=${until}`, {
      headers: { Accept: "text/csv" },
    });
    return res.blob();
  },

  /** Personal keys — a key that *is* the person. Role, groups and grants are
   * read live, so a demotion or a lock applies on the next call. */
  listMyApiKeys: () =>
    // `max_lifetime_days` is the install's cap on a new key; 0 is none.
    request<{ keys: UserApiKey[]; days: number; max_lifetime_days: number }>(`/me/api-keys`),
  createMyApiKey: (name: string, expiresInDays: number, scope: UserKeyScope) =>
    request<{ key: UserApiKey; secret: string }>(`/me/api-keys`, {
      method: "POST",
      body: JSON.stringify({ name, expires_in_days: expiresInDays, scope }),
    }),
  updateMyApiKey: (
    keyId: string,
    data: { name?: string; disabled?: boolean },
  ) =>
    request<UserApiKey>(`/me/api-keys/${keyId}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  deleteMyApiKey: (keyId: string) =>
    request<void>(`/me/api-keys/${keyId}`, { method: "DELETE" }),
  getMyApiKeyUsage: (keyId: string, bucket: KeyUsageBucket, days: number) =>
    request<KeyUsage>(
      `/me/api-keys/${keyId}/usage?bucket=${bucket}&days=${days}`,
    ),

  /** `toft login` — the browser half of the device flow.
   *
   * Both calls carry the session cookie and nothing else, which is the whole
   * point: the mint is authorized by a signed-in person, so a stolen key
   * cannot mint its replacement. The device code never reaches this page. */
  getCliSignIn: (userCode: string) =>
    request<CliSignInRequest>(
      `/auth/cli/device?user_code=${encodeURIComponent(userCode)}`,
    ),
  answerCliSignIn: (userCode: string, approve: boolean) =>
    request<{ status: string }>(`/auth/cli/device/approve`, {
      method: "POST",
      body: JSON.stringify({ user_code: userCode, approve }),
    }),

  /** The install-wide listing. Keeps working while keys are switched off —
   * that is exactly when a leaked one must be found. */
  listAllApiKeys: (days: number = 30) =>
    request<{ keys: UserApiKey[]; days: number }>(
      `/admin/api-keys?days=${days}`,
    ),
  adminUpdateApiKey: (
    keyId: string,
    data: { name?: string; disabled?: boolean },
  ) =>
    request<UserApiKey>(`/admin/api-keys/${keyId}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  adminDeleteApiKey: (keyId: string) =>
    request<void>(`/admin/api-keys/${keyId}`, { method: "DELETE" }),

  /** One account's keys, for its record — the same inventory narrowed, not a
   * different one. Admin-only and, like the install-wide listing, still
   * answering while personal keys are switched off: deciding whether an
   * account is safe to lock means seeing what would stop working. */
  listUserApiKeys: (userId: string, days: number = 30) =>
    request<{ keys: UserApiKey[]; days: number }>(
      `/users/${userId}/api-keys?days=${days}`,
    ),
  getUserApiKeyUsage: (
    userId: string,
    keyId: string,
    bucket: KeyUsageBucket,
    days: number,
  ) =>
    request<KeyUsage>(
      `/users/${userId}/api-keys/${keyId}/usage?bucket=${bucket}&days=${days}`,
    ),

  /* A document's re-render schedule, declared as `[render]` in toft.toml:
   * one per app, and `null` rather than a 404 for an app that has none —
   * "this app has no schedule" and "there is no such app" are different facts. */
  getRenderSchedule: (id: string) =>
    request<RenderSchedule | null>(`/apps/${id}/render-schedule`),
  /** Pause or resume — the one field the API writes; the schedule is the
   *  app's toft.toml `[render]`. 409 `no_render_schedule` when it declares none. */
  pauseRenderSchedule: (id: string, enabled: boolean) =>
    request<RenderSchedule>(`/apps/${id}/render-schedule`, {
      method: "PATCH",
      body: JSON.stringify({ enabled }),
    }),
  /** Replaces the list; an empty array clears it. */
  setRenderScheduleRecipients: (id: string, recipients: string[]) =>
    request<RenderSchedule>(`/apps/${id}/render-schedule/recipients`, {
      method: "PUT",
      body: JSON.stringify({ recipients }),
    }),
  /** The app's `[restart]` from toft.toml with its last firing, or `null`. */
  getRestartSchedule: (id: string) =>
    request<RestartSchedule | null>(`/apps/${id}/restart-schedule`),
  /** Pause or resume until the next deploy. 409 `no_restart_schedule` when
   *  toft.toml declares none. */
  pauseRestartSchedule: (id: string, enabled: boolean) =>
    request<RestartSchedule>(`/apps/${id}/restart-schedule`, {
      method: "PATCH",
      body: JSON.stringify({ enabled }),
    }),
  listReportDeliveries: (id: string) =>
    request<{ deliveries: ReportDelivery[] }>(
      `/apps/${id}/render-schedule/deliveries`,
    ),

  // Background jobs. Declared in toft.toml only: the one edit here is pausing
  // a job's schedule until the next deploy, and the one act is starting a run.
  listJobDefinitions: (id: string) =>
    request<JobDefinitionList>(`/apps/${id}/jobs`),
  /** Pause (`enabled: false`) or resume a job's schedule. Lasts until the next
   *  deploy, which re-applies toft.toml. */
  pauseJob: (id: string, jobId: string, enabled: boolean) =>
    request<JobDefinition>(`/apps/${id}/jobs/${encodeURIComponent(jobId)}`, {
      method: "PATCH",
      body: JSON.stringify({ enabled }),
    }),
  /** Admin-only: a job's own limits. `null` clears one back to the install's
   *  job default; a field left out is left as it is. */
  setJobLimits: (
    id: string,
    jobId: string,
    limits: { memory_limit_mb?: number | null; cpu_limit_millicores?: number | null },
  ) =>
    request<JobDefinition>(`/apps/${id}/jobs/${encodeURIComponent(jobId)}`, {
      method: "PATCH",
      body: JSON.stringify(limits),
    }),
  listJobRuns: (id: string, opts?: { job?: string; status?: string; limit?: number }) => {
    const params = new URLSearchParams();
    if (opts?.job) params.set("job", opts.job);
    if (opts?.status) params.set("status", opts.status);
    params.set("limit", String(opts?.limit ?? 100));
    return request<{ runs: JobRun[] }>(`/apps/${id}/job-runs?${params.toString()}`);
  },
  /** Start a run of a declared job. `scheduled_for` names the instant the run is
   *  for (RFC 3339); absent is a run for nothing in particular, never "now".
   *  Owners and editors only. 202 with the run, or 200 with `deduplicated`. */
  startJobRun: (
    id: string,
    job: string,
    body: { params?: Record<string, unknown>; scheduled_for?: string } = {},
  ) =>
    request<JobRun>(`/apps/${id}/jobs/${encodeURIComponent(job)}/runs`, {
      method: "POST",
      body: JSON.stringify(body),
    }),
  /** A new run with the same params and instant, recording `rerun_of`. Refused
   *  with 409 `run_still_open` while the original has not finished. */
  rerunJobRun: (id: string, runId: string) =>
    request<JobRun>(`/apps/${id}/job-runs/${runId}/rerun`, { method: "POST" }),
  cancelJobRun: (id: string, runId: string) =>
    request<JobRun & { changed?: boolean }>(`/apps/${id}/job-runs/${runId}/cancel`, {
      method: "POST",
    }),
  getJobRunLogs: (id: string, runId: string, after: number = 0) =>
    request<{ lines: JobLogLine[]; status: string }>(
      `/apps/${id}/job-runs/${runId}/logs?after=${after}`,
    ),

  // Logs & deployments
  getLogs: (id: string, lines?: number) =>
    request<{
      logs: string;
      lines?: WorkloadLogLine[];
      reason?: LogAbsenceReason;
    }>(`/apps/${id}/logs?lines=${lines || 100}`),
  listDeployments: (id: string) =>
    request<Deployment[]>(`/apps/${id}/deployments`),
  getDeploymentLogs: (appId: string, deployId: string) =>
    request<{ build_log: string; line_times?: BuildLineTimes }>(
      `/apps/${appId}/deployments/${deployId}/logs`,
    ),

  /** What broke on this app. The record, not the notification, is what a
   * workflow may rely on — either notification switch may be off. */
  listAppEvents: (
    id: string,
    opts?: { kind?: string; severity?: string; limit?: number },
  ) => {
    const params = new URLSearchParams();
    if (opts?.kind) params.set("kind", opts.kind);
    if (opts?.severity) params.set("severity", opts.severity);
    if (opts?.limit) params.set("limit", String(opts.limit));
    const qs = params.toString();
    return request<AppEvent[]>(`/apps/${id}/events${qs ? `?${qs}` : ""}`);
  },

  /** The merged audit + events feed. `before`/`before_id` are the keyset
   * cursor: offset paging would skip or repeat rows as new ones arrive. */
  listActivity: (opts?: {
    q?: string;
    category?: ActivityCategory;
    /** `info`, `warning`, `error`, or several separated by commas. */
    severity?: string;
    limit?: number;
    before?: string;
    before_id?: string;
    /** One app's history, by id. */
    app?: string;
    /** The rows one actor wrote, by the actor's id. */
    actor?: string;
    /** One action, or a family on a trailing `*`. */
    action?: string;
    /** RFC 3339; rows at or after it. */
    since?: string;
    /** RFC 3339; rows at or before it. */
    until?: string;
  }) => {
    const params = new URLSearchParams();
    if (opts?.q) params.set("q", opts.q);
    if (opts?.since) params.set("since", opts.since);
    if (opts?.until) params.set("until", opts.until);
    if (opts?.app) params.set("app", opts.app);
    if (opts?.actor) params.set("actor", opts.actor);
    if (opts?.action) params.set("action", opts.action);
    if (opts?.category && opts.category !== "all")
      params.set("category", opts.category);
    if (opts?.severity) params.set("severity", opts.severity);
    if (opts?.limit) params.set("limit", String(opts.limit));
    if (opts?.before) params.set("before", opts.before);
    if (opts?.before_id) params.set("before_id", opts.before_id);
    const qs = params.toString();
    return request<{
      entries: ActivityEntry[];
      /** The next page's cursor, absent on the last page. */
      next_before?: string;
      next_before_id?: string;
      /** On a narrowed read: when attribution began on this install. Older
       * rows carry an app only when the app was their target. */
      app_scope_started?: string;
      /** On a read narrowed to an actor: the name the Who column prints. */
      actor_label?: string;
    }>(`/admin/audit${qs ? `?${qs}` : ""}`);
  },

  // Users (system_admin only)
  listUsers: () => request<User[]>("/users"),
  getUser: (id: string) => request<UserDetail>(`/users/${id}`),
  updateUserRole: (id: string, role: "system_admin" | "publisher" | "viewer") =>
    request<User>(`/users/${id}/role`, {
      method: "PATCH",
      body: JSON.stringify({ system_role: role }),
    }),
  /** Only for an account that owns nothing and whose storage delegations
   * attach nothing — the API 409s otherwise, naming how many apps have to move
   * first, or (`holds_storage_mappings`) which mappings to revoke. */
  deleteUser: (id: string) =>
    request<void>(`/users/${id}`, { method: "DELETE" }),
  lockUser: (id: string) =>
    request<User>(`/users/${id}/lock`, { method: "POST" }),
  unlockUser: (id: string) =>
    request<User>(`/users/${id}/unlock`, { method: "POST" }),
  /** The scanner mark: its scan traffic is not app usage and it may read
   * Scan targets. It grants no access to any app. */
  setScanner: (id: string, on: boolean) =>
    request<User>(`/users/${id}/scanner`, { method: on ? "POST" : "DELETE" }),
  /**
   * Lets in an account a provider created but did not finish, and chooses the
   * role in the same act. Viewer or publisher only — the API refuses
   * system_admin, so promote afterwards rather than here.
   */
  activateUser: (id: string, role: "publisher" | "viewer") =>
    request<User>(`/users/${id}/activate`, {
      method: "POST",
      body: JSON.stringify({ system_role: role }),
    }),
  getUserAccess: (id: string) =>
    request<UserAccessReport>(`/users/${id}/access`),
  /** The apps a person has been opening over the last `days` UTC days, most
   *  recent first — existing apps only, their own traffic only. Admin only. */
  getUserAppUsage: (id: string, days: number = 90) =>
    request<UserAppUsage>(`/users/${id}/app-usage?days=${days}`),

  /**
   * The account's view of itself — the self-scoped halves of the two reads
   * above. They take no id: the subject is whoever is calling, which is what
   * keeps `/users/{id}` unconditionally administrator-only rather than
   * conditionally so.
   *
   * The response types are shared with the admin reads deliberately. Both
   * pairs of routes are backed by one query and one access-report builder on
   * the server, so a second type here would only be a place for the two
   * renderings of one record to drift apart.
   */
  getMyAccount: () => request<MyAccount>("/me/account"),
  getMyAccess: () => request<UserAccessReport>("/me/access"),

  /**
   * Your own sign-in history — the second read path into the audit log, and a
   * deliberately narrow one. The window and the cap come back with the rows so
   * the page can say what it is showing rather than implying it is everything.
   */
  getMySignins: () => request<MySigninsResponse>("/me/sign-ins"),

  /**
   * End every session this account has, everywhere — including the one making
   * this call, whose cookies the server clears on the way out. Personal API
   * keys are not sessions and keep working.
   */
  revokeMySessions: () =>
    request<void>("/me/sessions/revoke", { method: "POST" }),

  // Groups (list/read: any signed-in user; create/membership: admin+publisher; delete: admin)
  listGroups: () => request<Group[]>("/groups"),
  createGroup: (name: string) =>
    request<Group>("/groups", {
      method: "POST",
      body: JSON.stringify({ name }),
    }),
  getGroup: (id: string) => request<GroupDetail>(`/groups/${id}`),
  deleteGroup: (id: string) =>
    request<void>(`/groups/${id}`, { method: "DELETE" }),
  addGroupMember: (groupId: string, userId: string) =>
    request<void>(`/groups/${groupId}/members/${userId}`, { method: "PUT" }),
  removeGroupMember: (groupId: string, userId: string) =>
    request<void>(`/groups/${groupId}/members/${userId}`, { method: "DELETE" }),
  /** Where a group's membership may come from a directory. Admin only — a
   *  mapping decides who is in the group on every future sign-in. */
  listGroupClaimMappings: (groupId: string) =>
    request<{ mappings: GroupClaimMapping[] }>(`/groups/${groupId}/claim-mappings`),
  addGroupClaimMapping: (groupId: string, providerId: string, claimValue: string) =>
    request<void>(`/groups/${groupId}/claim-mappings`, {
      method: "PUT",
      body: JSON.stringify({ provider_id: providerId, claim_value: claimValue }),
    }),
  // Credentials an administrator holds and attaches to apps. No route here
  // returns a value: they are write-only in a stronger sense than a sensitive
  // env var, which its owner at least typed.
  listCredentials: () => request<{ credentials: Credential[] }>("/admin/credentials"),
  /** Which apps hold one, and who decided. A separate read from the inventory:
   *  the inventory carries a count, and this is what the credential's own page
   *  needs to act on the apps behind it. */
  listCredentialApps: (id: string) =>
    request<{ apps: CredentialApp[] }>(`/admin/credentials/${id}/apps`),
  createCredential: (data: { name: string; description?: string; values: Record<string, string> }) =>
    request<Credential>("/admin/credentials", { method: "POST", body: JSON.stringify(data) }),
  /** An omitted field is one you are not changing — which is what lets a rename
   *  and a rotation be two forms against one route without each clearing what
   *  the other owns. Supplying `values` replaces the whole set, except the
   *  names in `keep`, which hold their stored value: every stored name is
   *  sent, kept, or dropped. */
  updateCredential: (
    id: string,
    data: { name?: string; description?: string; values?: Record<string, string>; keep?: string[] },
  ) => request<void>(`/admin/credentials/${id}`, { method: "PATCH", body: JSON.stringify(data) }),
  deleteCredential: (id: string) =>
    request<void>(`/admin/credentials/${id}`, { method: "DELETE" }),
  attachCredential: (appId: string, id: string) =>
    request<void>(`/admin/apps/${appId}/credentials/${id}`, { method: "PUT" }),
  /** Detaching restarts the app: the value is in a live process's environment
   *  and only a start replaces that. */
  detachCredential: (appId: string, id: string) =>
    request<{ restarting: string[] }>(`/admin/apps/${appId}/credentials/${id}`, { method: "DELETE" }),

  /** Mint the token a directory presents to /scim/v2. Returned once and never
   *  readable again; minting again rotates and invalidates the previous one. */
  mintSCIMToken: (providerId: string) =>
    request<{ token: string; base_url: string }>(
      `/admin/auth-providers/${providerId}/scim-token`, { method: "POST" }),
  revokeSCIMToken: (providerId: string) =>
    request<void>(`/admin/auth-providers/${providerId}/scim-token`, { method: "DELETE" }),
  removeGroupClaimMapping: (groupId: string, providerId: string, claimValue: string) =>
    request<void>(
      `/groups/${groupId}/claim-mappings?provider_id=${encodeURIComponent(providerId)}` +
        `&claim_value=${encodeURIComponent(claimValue)}`,
      { method: "DELETE" },
    ),
  /** Set or clear the address failure mail reaches this group at. Empty clears
   *  it, which puts the group back on member expansion rather than silencing
   *  it. Admin only. */
  setGroupEmail: (groupId: string, email: string) =>
    request<Group>(`/groups/${groupId}`, {
      method: "PATCH",
      body: JSON.stringify({ email }),
    }),
  /** Rename a group. Its id is what every grant and owned app points at, so
   *  nothing else moves. 409 when another group already has the name. Admin
   *  only. */
  renameGroup: (groupId: string, name: string) =>
    request<Group>(`/groups/${groupId}`, {
      method: "PATCH",
      body: JSON.stringify({ name }),
    }),

  // Deploy keys — the credential that belongs to nobody. Admin only, and no
  // key can reach these routes: no key mints a key.
  listDeployKeys: () => request<DeployKey[]>("/deploy-keys"),
  createDeployKey: (name: string, expiresInDays: number | null, scope: DeployKeyScope, mayAttach = false) =>
    request<DeployKeyMint>("/deploy-keys", {
      method: "POST",
      body: JSON.stringify({ name, expires_in_days: expiresInDays, scope, may_attach: mayAttach }),
    }),
  setDeployKeyMayAttach: (id: string, mayAttach: boolean) =>
    request<DeployKey>(`/deploy-keys/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ may_attach: mayAttach }),
    }),
  setDeployKeyDisabled: (id: string, disabled: boolean) =>
    request<DeployKey>(`/deploy-keys/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ disabled }),
    }),
  revokeDeployKey: (id: string) =>
    request<void>(`/deploy-keys/${id}`, { method: "DELETE" }),

  /** Toft's own manuals — the apps on the reserved `_` namespace, which
   * `listApps` drops and this gathers. The complement of that list, read
   * through the same definition of who may read an app, so the two pages
   * together show each readable app exactly once. */
  listDocumentation: () => request<DocumentationResponse>("/documentation"),

  // Features — what this build offers, what the install is licensed for, and
  // what an administrator has switched on. Admin-only and 404 to everyone else,
  // like the telemetry and email settings.
  listFeatures: () => request<Feature[]>("/admin/features"),
  /** Gate 2 only. It never touches the license and never touches an app's
   * grant, so switching a feature off leaves its app installed and serving —
   * and switching it back on restores what was granted rather than asking
   * somebody to remember. */
  setFeatureEnabled: (name: string, enabled: boolean) =>
    request<Feature>(`/admin/features/${name}`, {
      method: "PUT",
      body: JSON.stringify({ enabled }),
    }),

  // System
  getSystemHealth: () => request<SystemHealth>("/system/health"),
  /** Where this install exports its telemetry, and whether that is working.
   * Admin-only and 404 to everyone else, like the email settings. */
  getTelemetrySettings: () =>
    request<TelemetrySettings>("/admin/telemetry-settings"),
  /** Saves and applies in one call — the pipeline is rebuilt in place, so the
   * change takes effect before this resolves rather than at the next restart. */
  updateTelemetrySettings: (body: UpdateTelemetrySettings) =>
    request<{ settings: TelemetrySettings; warning?: string }>(
      "/admin/telemetry-settings",
      {
        method: "PUT",
        body: JSON.stringify(body),
      },
    ),
  /** Exports one empty batch through the saved configuration and returns the
   * collector's own error. `ok: false` is a working request with a failing
   * collector — the page renders `error`; a thrown error is the platform. */
  testTelemetry: () =>
    request<{ ok: boolean; error?: string }>("/admin/telemetry-settings/test", {
      method: "POST",
    }),
  getActiveUsers: () => request<ActiveUserCounts>("/system/active-users"),
  /** Every live workload, plus anything recorded as running that the backend
   * has never heard of. Admin-only. */
  getProcesses: () => request<ProcessList>("/system/processes"),
  /** Who is using this node right now. Admin-only; `window` is in seconds and
   * the server accepts 60–1800. */
  getPresence: (windowSeconds?: number) =>
    request<Presence>(
      `/system/presence${windowSeconds ? `?window=${windowSeconds}` : ""}`,
    ),
  getConsumptionSummary: (days = 7) =>
    request<ConsumptionSummary>(`/system/consumption?days=${days}`),
  /** What one app actually consumed. Authorized by the app's own read rule —
   *  consumption is no more sensitive than the app's existence, and no less. */
  getAppConsumption: (appId: string, days = 30) =>
    request<AppConsumption>(`/apps/${appId}/consumption?days=${days}`),
  /** Unfinished builds, in the order the worker will take them. Admin-only. */
  getQueue: () => request<BuildQueue>("/system/queue"),
  /** Every job run on the install, whichever app owns it — the only cross-app
   * view of the job system. Admin-only. `status` takes a stored status or the
   * pseudo-status `in_flight`; anything else is refused by name. */
  getAllJobRuns: (opts?: {
    status?: string;
    days?: number;
    limit?: number;
  }) => {
    const params = new URLSearchParams();
    if (opts?.status) params.set("status", opts.status);
    if (opts?.days) params.set("days", String(opts.days));
    if (opts?.limit) params.set("limit", String(opts.limit));
    const qs = params.toString();
    return request<AdminJobRunList>(`/system/job-runs${qs ? `?${qs}` : ""}`);
  },
  /** Every app's traffic over the window (1–90 days), busiest first. Counts
   * only — who visited is on the app's own Usage panel. Admin-only. */
  getUsageOverview: (days = 30) =>
    request<UsageOverview>(`/system/usage?days=${days}`),
  /** The install's audience — people signed in, anonymous browsers, page
   *  loads — bucketed by day, ISO week or month. Admin-only. */
  getSystemAudience: (since: string, until: string, grain: AudienceGrain) =>
    request<SystemAudience>(
      `/system/audience?since=${since}&until=${until}&grain=${grain}`,
    ),
  /** The other end of the same question: apps nobody has opened in at least
   * `days` days (1–3650), longest-quiet first. A separate call because it needs
   * an *unwindowed* last visit, which getUsageOverview cannot give — that one
   * caps its last-visit at the window it charts. Admin-only. */
  getQuietApps: (days = 30) =>
    request<QuietAppList>(`/system/quiet-apps?days=${days}`),
  /** Every schedule on the install, whichever app owns it. Admin-only. */
  getScheduledOverview: () => request<ScheduledOverview>("/system/scheduled"),
  /** Every firing on the install in [from, to), at most eight days, expanded
   *  by the server's own scheduler arithmetic (ADR §156). Admin-only. */
  getUpcomingSchedules: (from: Date, to: Date) =>
    request<UpcomingSchedules>(
      `/system/scheduled/upcoming?from=${encodeURIComponent(from.toISOString().replace(/\.\d{3}Z$/, "Z"))}` +
        `&to=${encodeURIComponent(to.toISOString().replace(/\.\d{3}Z$/, "Z"))}`,
    ),
  /** The install's self-diagnosis, computed when asked. Admin-only. */
  getSystemChecks: () => request<SystemCheckReport>("/system/checks"),
  /**
   * Empty the shared build cache. 202 and it starts; 409 if one is already
   * running, which is the only thing it refuses — a build in flight is not a
   * refusal, because the reclaim waits for it.
   */
  reclaimBuildCache: () =>
    request<{ started: boolean; detail: string }>("/system/build-cache/reclaim", {
      method: "POST",
    }),

  // Announcement banner
  getAnnouncement: () => request<Announcement>("/announcement"),
  setAnnouncement: (data: { message: string; level: AnnouncementLevel }) =>
    request<Announcement>("/admin/announcement", {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  clearAnnouncement: () =>
    request<void>("/admin/announcement", { method: "DELETE" }),

  // The announcement an administrator *sends*, which is the banner's neighbour
  // and nothing like it. The banner is one live string that DELETE takes back;
  // this reaches every person on the install, cannot be taken back, and leaves
  // a frozen row saying who was told what. Admin-only, and every one of these
  // is refused to an API key.
  listAnnouncementEmails: () =>
    request<AnnouncementEmailList>("/admin/announcement-emails"),
  getAnnouncementEmail: (id: string) =>
    request<AnnouncementEmail>(`/admin/announcement-emails/${id}`),
  /** About how many people an audience resolves to *today*. The word is
   *  "about": a draft composed on Monday and sent on Thursday is sent to
   *  Thursday's directory, and only the frozen row carries what happened. */
  estimateAnnouncementAudience: (audience: AnnouncementAudience) =>
    request<AnnouncementEstimate>(
      `/admin/announcement-emails/estimate?audience=${audience}`,
    ),
  createAnnouncementEmail: (data: AnnouncementEmailInput) =>
    request<AnnouncementEmail>("/admin/announcement-emails", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  updateAnnouncementEmail: (id: string, data: AnnouncementEmailInput) =>
    request<AnnouncementEmail>(`/admin/announcement-emails/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /** A draft or a completed record, recipient rows with it. A run in flight is
   *  409 `send_in_flight` — the runner is still writing into it, and it is
   *  deletable the moment it finishes. What survives a deleted record is the
   *  audit line, carrying the subject, the audience, the date and the counts. */
  deleteAnnouncementEmail: (id: string) =>
    request<void>(`/admin/announcement-emails/${id}`, { method: "DELETE" }),
  cloneAnnouncementEmail: (id: string) =>
    request<AnnouncementEmail>(`/admin/announcement-emails/${id}/clone`, {
      method: "POST",
    }),
  /** The real template with the real content, to your own address. Freezes
   *  nothing and records no recipient. */
  testAnnouncementEmail: (id: string) =>
    request<{ sent_to: string }>(`/admin/announcement-emails/${id}/test`, {
      method: "POST",
    }),
  /** The same frozen message, to the people this one's run did not reach —
   *  a new row, created already sending. 409 `already_resent` names the resend
   *  in `resent_as`; `nothing_to_resend` once nobody is left on record. */
  resendAnnouncementEmail: (id: string) =>
    request<AnnouncementEmail>(`/admin/announcement-emails/${id}/resend`, {
      method: "POST",
    }),
  /** The freeze, then the run. There is no un-send. */
  sendAnnouncementEmail: (id: string) =>
    request<AnnouncementEmail>(`/admin/announcement-emails/${id}/send`, {
      method: "POST",
    }),

  // The install's own knowledge base — one app an administrator points
  // everybody at, reached from the header rather than from the apps list.
  //
  // Two reads, and they answer different questions on purpose. `getKnowledgeBase`
  // resolves the pointer **for this caller**: `{}` when nothing is picked, when
  // the picked app no longer exists, and when this person may not read it — the
  // same bytes for all three, because a distinguishable answer would disclose a
  // private app to the whole install. `getAdminKnowledgeBase` answers with what
  // is *stored*, which is the only way the page that owns the pick can tell
  // "nothing is set" from "this names an app that is gone".
  getKnowledgeBase: () => request<KnowledgeBase>("/knowledge-base"),
  getAdminKnowledgeBase: () => request<KnowledgeBasePick>("/admin/knowledge-base"),
  setKnowledgeBase: (data: { app_id: string; label: string; description: string }) =>
    request<KnowledgeBasePick>("/admin/knowledge-base", {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  clearKnowledgeBase: () =>
    request<void>("/admin/knowledge-base", { method: "DELETE" }),

  // Tags — a tag is a key and an app holds one value for it. The vocabulary is
  // readable by anyone; only system_admin can change it.
  listTags: () => request<Tag[]>("/tags"),
  /** Values already in use, for the picker's suggestions. Scoped to apps the caller can read. */
  listTagValues: (tagSlug: string, q?: string) =>
    request<TagValue[]>(
      `/tags/${tagSlug}/values${q ? `?q=${encodeURIComponent(q)}` : ""}`,
    ),
  createTag: (data: { name: string; color?: string; description?: string }) =>
    request<Tag>("/admin/tags", { method: "POST", body: JSON.stringify(data) }),
  updateTag: (
    id: string,
    data: Partial<{
      name: string;
      slug: string;
      color: string;
      description: string;
    }>,
  ) =>
    request<Tag>(`/admin/tags/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /** Deleting a tag in use answers 409 unless force is set; the cascade then clears it from every app. */
  deleteTag: (id: string, force = false) =>
    request<void>(`/admin/tags/${id}${force ? "?force=true" : ""}`, {
      method: "DELETE",
    }),
  /** Sets an app's values. Keys are tag slugs; an empty value clears the tag. Omitted tags are untouched. */
  setAppTags: (appId: string, tags: Record<string, string>) =>
    request<AppTag[]>(`/apps/${appId}/tags`, {
      method: "PUT",
      body: JSON.stringify({ tags }),
    }),

  // Sign-in providers.
  //
  // The public read is what the login page renders from — it must work before
  // anyone is signed in. Everything else is admin-only: whoever can add a
  // provider can add a door they control.
  getLoginOptions: () => request<LoginOptions>("/auth/providers"),
  listAuthProviders: () => request<AuthProviderList>("/admin/auth-providers"),
  createAuthProvider: (data: AuthProviderInput) =>
    request<{ id: string; redirect_uri: string }>("/admin/auth-providers", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  /** Omit client_secret to keep the stored one — it is never sent back to the form. */
  /** Answers `login_ready` and `secret_unreadable` as well as the id: *switched
   *  on* and *on the login page* are two facts, and a toast that reports the
   *  first over a row that shows the second is what this is for. */
  updateAuthProvider: (id: string, data: Partial<AuthProviderInput>) =>
    request<{ id: string; login_ready: boolean; secret_unreadable: boolean }>(`/admin/auth-providers/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /** Users provisioned through it keep their accounts; re-adding the same id restores their sign-in. */
  deleteAuthProvider: (id: string) =>
    request<void>(`/admin/auth-providers/${id}`, { method: "DELETE" }),
  /** Issuers whose identity tokens buy a short-lived deploy key (ADR §157). */
  listTrustedIssuers: () => request<TrustedIssuer[]>("/admin/trusted-issuers"),
  createTrustedIssuer: (data: TrustedIssuerInput) =>
    request<TrustedIssuer>("/admin/trusted-issuers", { method: "POST", body: JSON.stringify(data) }),
  updateTrustedIssuer: (id: string, data: Partial<TrustedIssuerInput> & { enabled?: boolean }) =>
    request<TrustedIssuer>(`/admin/trusted-issuers/${id}`, { method: "PATCH", body: JSON.stringify(data) }),
  /** Every key it vouched for ends with it. */
  deleteTrustedIssuer: (id: string) =>
    request<void>(`/admin/trusted-issuers/${id}`, { method: "DELETE" }),
  /** The live ones: neither expired nor revoked. */
  listExchangedKeys: () => request<ExchangedKey[]>("/admin/exchanged-keys"),
  revokeExchangedKey: (id: string) =>
    request<void>(`/admin/exchanged-keys/${id}`, { method: "DELETE" }),
  /** The connections a developer may point an app at. Never the auth method. */
  listMyGitConnections: () =>
    request<{ connections: DeveloperGitConnection[] }>("/git/connections"),
  /**
   * The repositories this person can see on a connection.
   *
   * `needs_authorize` is the ordinary first state, not a failure: they have not
   * signed in to the host yet, and `authorize_url` is where to send them.
   */
  listConnectionRepos: (connectionId: string, q: string) =>
    request<{
      repositories: PickerRepository[];
      /** The list was cut short: the 100 most recent, or a search's first 100 matches. */
      has_more?: boolean;
      needs_authorize?: boolean;
      authorize_url?: string;
      connection_name: string;
    }>(`/git/connections/${connectionId}/repos?q=${encodeURIComponent(q)}`),
  listGitConnections: () =>
    request<{ connections: GitConnection[] }>("/admin/git-connections"),
  /** The one response that carries the webhook secret, beside the rotate's. */
  createGitConnection: (data: GitConnectionInput) =>
    request<GitConnection>("/admin/git-connections", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  /** Turns the webhook on, or replaces its secret: a new secret, returned once;
   *  any old one stops verifying. A connection starts with no webhook. */
  rotateGitConnectionWebhookSecret: (id: string) =>
    request<GitConnectionWithSecret>(`/admin/git-connections/${id}/webhook-secret`, {
      method: "POST",
    }),
  /** Turns the webhook off: no delivery verifies, and pushes deploy on the poll. */
  disableGitConnectionWebhook: (id: string) =>
    request<GitConnection>(`/admin/git-connections/${id}/webhook-secret`, {
      method: "DELETE",
    }),
  /** Omit a secret to keep the stored one — none is ever sent back to the form. */
  updateGitConnection: (id: string, data: Partial<GitConnectionInput>) =>
    request<GitConnection>(`/admin/git-connections/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /** Refused with 409 while apps still deploy through it, naming every one. */
  deleteGitConnection: (id: string) =>
    request<void>(`/admin/git-connections/${id}`, { method: "DELETE" }),
  /** What a removal would be refused on, read before an administrator tries. */
  listGitConnectionApps: (id: string) =>
    request<{ apps: GitConnectionApp[] }>(`/admin/git-connections/${id}/apps`),
  /** One authenticated call to the host, against the *stored* credential — the
   *  same rule the email test follows. A refused credential comes back as
   *  `ok: false` with the sentence to act on, not as a thrown error. */
  testGitConnection: (id: string) =>
    request<GitConnectionTestResult>(`/admin/git-connections/${id}/test`, {
      method: "POST",
    }),

  /** Probes an issuer's discovery document, so a wrong URL fails at the form rather than at someone's first sign-in. */
  discoverIssuer: (issuer: string) =>
    request<DiscoveryDoc>("/admin/auth-providers/discover", {
      method: "POST",
      body: JSON.stringify({ issuer }),
    }),

  // Durable app storage. Admin-only, except the per-app read: an owner needs to
  // see what their app mounts without being able to grant it anything.
  // Every one of these 404s on an install that offers no storage.
  listStorageResources: () => request<StorageResourceList>("/admin/storage"),
  createStorageResource: (data: {
    name: string;
    kind: StorageKind;
    /** Absent means the one this kind has always had. */
    backing?: StorageBacking;
    storage_class?: string;
    storage_root?: string;
    bucket?: string;
    prefix?: string;
    description?: string;
    partitioned?: boolean;
  }) =>
    request<StorageResource>("/admin/storage", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  /** What a resource is *called* — its label and its description. The slug is
   *  not editable and sending one is refused by name: every mounted binding is
   *  derived from it, so moving it would strand live data. Nothing here touches
   *  the cluster, and no app is restarted. */
  renameStorageResource: (
    id: string,
    data: { description: string },
  ) =>
    request<StorageResource>(`/admin/storage/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /** 409s while any app still mounts it. Deleting releases the binding; the data survives. */
  /** Sets what a drive is called; an empty label clears it back to the drive's name. */
  updateStorageDriveLabel: (key: string, data: { label: string; description: string }) =>
    request<StorageDrive>(`/admin/storage/drives/${encodeURIComponent(key)}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /**
   * Moves every resource on a drive this install no longer declares onto one it
   * does, all or none, after proving each one's data is there. `dry_run`
   * answers the proof without writing; a refusal is a 409 whose body carries
   * the same `checks`.
   */
  repointStorageDrive: (key: string, data: { to: string; dry_run?: boolean }) =>
    request<StorageRepoint>(`/admin/storage/drives/${encodeURIComponent(key)}/repoint`, {
      method: "POST",
      body: JSON.stringify(data),
    }),
  /** 204, or 200 naming a directory left on a drive this install no longer declares. */
  deleteStorageResource: (id: string) =>
    request<StorageDeleteResult>(`/admin/storage/${id}`, { method: "DELETE" }),
  /** Frees a name a finished delete holds because its volume's files were kept. */
  releaseKeptStorageName: (slug: string) =>
    request<void>(`/admin/storage/deleting/${encodeURIComponent(slug)}`, { method: "DELETE" }),
  listStorageMappings: (id: string) =>
    request<StorageMapping[]>(`/admin/storage/${id}/mappings`),
  /** Creates or moves the mapping of one resource onto one app. Takes effect on that app's next deploy. */
  putStorageMapping: (
    id: string,
    appId: string,
    data: { path: string; access: StorageAccess; folder?: string },
  ) =>
    request<{ mapping: StorageMapping; note?: string }>(
      `/admin/storage/${id}/mappings/${appId}`,
      { method: "PUT", body: JSON.stringify(data) },
    ),
  /**
   * Detaches one folder, or — with no folder named — everything this app mounts
   * of this resource, which is what the call meant before folders existed.
   */
  /** `note` is set when the mapping was declared in the app's toft.toml: the
   *  next deploy restores it while the owner can attach it. */
  deleteStorageMapping: (id: string, appId: string, folder?: string) =>
    request<{ removed: number; note?: string }>(
      `/admin/storage/${id}/mappings/${appId}${folder === undefined ? "" : `?folder=${encodeURIComponent(folder)}`}`,
      { method: "DELETE" },
    ),

  // Who may attach this resource without being an administrator. The revoke is
  // the one call in the product that restarts somebody else's app as a
  // consequence of a permission change, which is why it answers with the list
  // rather than 204.
  // The install's default delegations (Admin › Storage): who every new
  // resource is delegated to when it is created.
  listStorageDefaults: () => request<StorageDefault[]>("/admin/storage/defaults"),
  putStorageDefault: (body: {
    subject_type: StoreSubjectType; subject_id: string; max_access: StorageDefaultLevel; apply_existing: boolean;
  }) =>
    request<{
      default: StorageDefault; applied?: string[]; differing?: { resource: string; max_access: StorageAccess }[];
    }>("/admin/storage/defaults", { method: "PUT", body: JSON.stringify(body) }),
  deleteStorageDefault: (subject: string, revokeExisting: boolean) =>
    request<{ revoked: number; note?: string }>(
      `/admin/storage/defaults/${encodeURIComponent(subject)}${revokeExisting ? "?revoke_existing=true" : ""}`,
      { method: "DELETE" },
    ),
  listStorageManagers: (id: string) =>
    request<StorageManagerList>(`/admin/storage/${id}/managers`),
  putStorageManager: (
    id: string,
    data: {
      subject_type: StoreSubjectType;
      subject_id: string;
      max_access: StorageAccess;
    },
  ) =>
    request<StorageManager>(`/admin/storage/${id}/managers`, {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  deleteStorageManager: (
    id: string,
    subjectType: StoreSubjectType,
    subjectId: string,
  ) =>
    request<StorageRevocation>(
      `/admin/storage/${id}/managers/${subjectType}:${subjectId}`,
      {
        method: "DELETE",
      },
    ),

  /** Which resources this caller may attach, and at what ceiling. */
  listMyAttachableStorage: () => request<AttachableStorageList>("/me/storage"),
  attachAppStorage: (
    appId: string,
    resourceId: string,
    data: { path: string; access: StorageAccess; folder?: string },
  ) =>
    request<{ mapping: StorageMapping; note?: string }>(
      `/apps/${appId}/storage/${resourceId}`,
      {
        method: "PUT",
        body: JSON.stringify(data),
      },
    ),
  detachAppStorage: (appId: string, resourceId: string, folder?: string) =>
    request<{ removed: number }>(
      `/apps/${appId}/storage/${resourceId}${folder === undefined ? "" : `?folder=${encodeURIComponent(folder)}`}`,
      { method: "DELETE" },
    ),
  /** What one app mounts. Readable by anyone who can read the app. */
  getAppStorage: (appId: string) =>
    request<AppStorage>(`/apps/${appId}/storage`),

  // What an app can reach: everything the platform has given it, in one place.
  //
  // Two calls and two audiences. The per-app read needs `can_edit` — the same
  // gate as Dependencies, because the assembly says more than the sum of its
  // parts — and the estate read is administrator-only. Neither reports a value
  // and neither reports history.
  /** One app's whole assembly: who may open it, what it holds, what it may
   *  send, and who can act as it. */
  getAppReach: (appId: string) => request<AppReach>(`/apps/${appId}/reach`),
  /** The app's audit file, read now. Each section carries its own outcome. */
  getAppAuditFile: (appId: string) => request<AppAuditFile>(`/apps/${appId}/audit-file`),
  /** Every app, as what it holds. One query on the server; the client filters
   *  and sorts, as the scan-target list does. */
  getEstateReach: () => request<EstateReach>("/admin/estate/reach"),

  // What this install tells an assessor (ADR §93). Three reads, and the split
  // between the first two is the feature: the control document is a property of
  // the BUILD and identical on every install of a version, while the report is
  // a property of THIS install, read now.
  //
  // All of them are administrator-only and refused to every API key class, so
  // the only caller is this page.
  /** Framework, families and coverage — enough to render the landing tiles. */
  getCompliance: () => request<ComplianceSummary>("/admin/compliance"),
  /** The control document as data: 97 requirements with the mapping joined on. */
  getComplianceControls: () => request<ComplianceControls>("/admin/compliance/controls"),
  /** The live reading. Computed per request and filed nowhere. */
  getComplianceReport: () => request<ComplianceReport>("/admin/compliance/report"),

  // Who may reach a store from *outside* — a person with a grant, no app and no
  // mount. A mapping is an app's and a grant is a person's; neither implies the
  // other, which is why these are three calls beside the three above.
  //
  // Stores only: a volume is an EFS access point with nothing to presign, and
  // the API refuses one by kind.
  listStoreGrants: (id: string) =>
    request<StoreGrantList>(`/admin/storage/${id}/grants`),
  putStoreGrant: (
    id: string,
    data: {
      subject_type: StoreSubjectType;
      subject_id: string;
      access: StorageAccess;
      folder?: string;
    },
  ) =>
    request<StoreGrant>(`/admin/storage/${id}/grants`, {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  /** Revokes one scope. A folder grant and a root grant are separate rows, so revoking one leaves the other. */
  deleteStoreGrant: (
    id: string,
    subjectType: StoreSubjectType,
    subjectId: string,
    folder?: string,
  ) =>
    request<void>(
      `/admin/storage/${id}/grants/${subjectType}:${subjectId}${folder ? `?folder=${encodeURIComponent(folder)}` : ""}`,
      { method: "DELETE" },
    ),

  // --- Using a store, as the person a grant was created for ---
  //
  // The other half of the feature above: those three calls are an
  // administrator deciding who may, these five are somebody doing it. Every one
  // is self-scoped — no id in the URL names anybody else — and the gate is the
  // grant, resolved server-side, union-of-max, with no admin bypass.
  //
  // The bucket and the prefix are deliberately not in any of these shapes. The
  // server builds every key from a name or a path the caller supplies, which is
  // why `path` below is a folder and never a key.

  /** The stores this caller reaches, and at what level. 404 where the install
   * offers no store uploads at all — absent, not dormant. */
  listStores: () => request<{ stores: StoreSummary[] }>(`/store`),

  /** One level of a store, or everything beneath a path when `flat`. */
  listStoreObjects: (
    slug: string,
    opts: { path?: string; flat?: boolean; cursor?: string } = {},
  ) => {
    const q = new URLSearchParams();
    if (opts.path) q.set("path", opts.path);
    if (opts.flat) q.set("flat", "1");
    if (opts.cursor) q.set("cursor", opts.cursor);
    const query = q.toString();
    return request<StoreObjectPage>(
      `/store/${slug}/objects${query ? `?${query}` : ""}`,
    );
  },

  /** Mint a presigned PUT. The digests are computed *before* this is called:
   * they are signed into the URL, and the URL starts its one hour here. */
  signStoreUpload: (
    slug: string,
    body: { name: string; size: number; sha256: string; md5: string },
  ) =>
    request<SignedStoreRequest>(`/store/${slug}/objects`, {
      method: "POST",
      body: JSON.stringify(body),
    }),

  /** Mint a presigned GET. `attachment` asks the object store to send a
   * Content-Disposition naming the object, so the browser saves it under its
   * own name instead of rendering it or naming it after the URL. */
  signStoreDownload: (slug: string, name: string, attachment = false) =>
    request<SignedStoreRequest>(
      `/store/${slug}/objects/${encodeStoreName(name)}${attachment ? "?disposition=attachment" : ""}`,
    ),

  /** Mint a presigned DELETE. `full` only, and the platform never spends it. */
  signStoreDelete: (slug: string, name: string) =>
    request<SignedStoreRequest>(
      `/store/${slug}/objects/${encodeStoreName(name)}`,
      {
        method: "DELETE",
      },
    ),

  // --- The app's own saved document (ADR §36) ---
  //
  // Metadata and a reset, and deliberately no reader: the app is the only thing
  // that writes this document, and a screen that displayed it would be a second,
  // unversioned editor for state that has one writer.

  /** Whether this app has saved anything, how big it is, and when it changed. */
  getAppData: (appId: string) => request<AppDataMeta>(`/apps/${appId}/data`),
  /** Throw that document away. Irreversible — nothing keeps a copy. */
  resetAppData: (appId: string) =>
    request<AppDataMeta>(`/apps/${appId}/data`, { method: "DELETE" }),

  // --- Local sign-in (the built-in administrator) ---
  //
  // Both routes 404 when an administrator has switched local sign-in off, which
  // is why the login page decides what to render from the provider list rather
  // than by probing them.

  /**
   * Verifies a password. A session cookie comes back set — unless the account
   * still holds its initial password, in which case `must_change_password` is
   * true and no session is issued: the caller must go through `changePassword`
   * first. That is what makes shipping a published default password safe.
   */
  passwordLogin: (username: string, password: string, redirect?: string) =>
    request<PasswordLoginResult>("/auth/local/login", {
      method: "POST",
      body: JSON.stringify({ username, password, redirect }),
    }),

  /**
   * Replaces a password, authenticated by the current one, and signs in. Used
   * for the forced first change and for voluntary rotation afterwards — there is
   * no session yet on the first, so the old password is the credential.
   */
  changePassword: (username: string, password: string, newPassword: string, redirect?: string) =>
    request<{ ok: boolean; redirect?: string }>("/auth/local/password", {
      method: "POST",
      body: JSON.stringify({ username, password, new_password: newPassword, redirect }),
    }),

  /** What this principal may do, independent of any particular app. */
  getCapabilities: () => request<Capabilities>("/me/capabilities"),

  // Publisher access requests. The requester's half is under /me; deciding is
  // admin-only and lives under /admin.
  /** The requester's own app access requests, newest first. The decider's
   *  list is getAccessRequests below, and needs a different permission. */
  getMyAccessRequests: () => request<AccessRequest[]>("/me/access-requests"),
  createAccessRequest: (appId: string, role: string, note?: string) =>
    request<AccessRequest>(`/apps/${appId}/access-requests`, {
      method: "POST",
      body: JSON.stringify({ role, ...(note ? { note } : {}) }),
    }),
  withdrawAccessRequest: (appId: string, id: string) =>
    request<void>(`/apps/${appId}/access-requests/${id}`, { method: "DELETE" }),
  /** The decider's queue for one app. */
  getAccessRequests: (appId: string) =>
    request<AccessRequest[]>(`/apps/${appId}/access-requests`),
  approveAccessRequest: (appId: string, id: string) =>
    request<void>(`/apps/${appId}/access-requests/${id}/approve`, { method: "POST" }),
  denyAccessRequest: (appId: string, id: string, reason: string) =>
    request<void>(`/apps/${appId}/access-requests/${id}/deny`, {
      method: "POST",
      body: JSON.stringify({ reason }),
    }),

  getMyPromotionRequests: () =>
    request<PromotionRequest[]>("/me/promotion-requests"),
  createPromotionRequest: () =>
    request<PromotionRequest>("/me/promotion-requests", { method: "POST" }),
  withdrawPromotionRequest: (id: string) =>
    request<void>(`/me/promotion-requests/${id}`, { method: "DELETE" }),
  /** Defaults to the pending queue; pass "all" for the full history. */
  adminListPromotionRequests: (
    status: PromotionRequestStatus | "all" = "pending",
  ) =>
    request<PromotionRequest[]>(`/admin/promotion-requests?status=${status}`),
  adminApprovePromotionRequest: (id: string) =>
    request<void>(`/admin/promotion-requests/${id}/approve`, {
      method: "POST",
    }),
  adminDenyPromotionRequest: (id: string, reason: string) =>
    request<void>(`/admin/promotion-requests/${id}/deny`, {
      method: "POST",
      body: JSON.stringify({ reason }),
    }),

  getEmailSettings: () => request<EmailSettings>("/admin/email-settings"),
  /** Omit password to keep the stored one — it is never sent back to the form. */
  updateEmailSettings: (data: UpdateEmailSettings) =>
    request<EmailSettings>("/admin/email-settings", {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  /** Sends to the caller's own address using the saved settings. */
  sendTestEmail: () =>
    request<{ sent_to: string }>("/admin/email-settings/test", {
      method: "POST",
    }),

  // The integration catalog — connections, and what each app is attached to.
  //
  // A connection is a credential, so every one of these is admin-only and every
  // one is denied to API keys. The credential goes in and never comes out.
  listIntegrationConnections: () =>
    request<{
      catalog: IntegrationKind[];
      connections: IntegrationConnection[];
    }>("/admin/integrations/connections"),
  createIntegrationConnection: (data: NewIntegrationConnection) =>
    request<IntegrationConnection>("/admin/integrations/connections", {
      method: "POST",
      body: JSON.stringify(data),
    }),
  updateIntegrationConnection: (
    id: string,
    data: UpdateIntegrationConnection,
  ) =>
    request<IntegrationConnection>(`/admin/integrations/connections/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    }),
  /** Refused while apps are attached, naming them. Switching `for_apps` off is
   * the immediate and unrefusable half. */
  deleteIntegrationConnection: (id: string) =>
    request<{ deleted: boolean }>(`/admin/integrations/connections/${id}`, {
      method: "DELETE",
    }),
  testIntegrationConnection: (id: string) =>
    request<{ sent: boolean }>(`/admin/integrations/connections/${id}/test`, {
      method: "POST",
    }),
  /** 404 for a kind that does not enumerate — never an empty list, which would
   * read as "none configured yet". */
  listConnectionDestinations: (id: string) =>
    request<{ label: string; destinations: IntegrationDestination[] }>(
      `/admin/integrations/connections/${id}/destinations`,
    ),
  /** What this connection has left in somebody else's system. Detaching reaches
   * none of it; this list is what makes that something a person can act on. */
  /** A Teams bot's pasted teams and chats; `reachable` is asked of Teams now. */
  listIntegrationPlaces: (id: string) =>
    request<{ places: IntegrationPlace[] }>(`/admin/integrations/connections/${id}/places`),
  /** Paste a team's or chat's link; saved only where the Toft app is installed. */
  addIntegrationPlace: (id: string, link: string, name?: string) =>
    request<IntegrationPlace>(`/admin/integrations/connections/${id}/places`, {
      method: "POST",
      body: JSON.stringify({ link, name }),
    }),
  deleteIntegrationPlace: (id: string, placeId: string) =>
    request<{ deleted: boolean }>(
      `/admin/integrations/connections/${id}/places/${placeId}`,
      { method: "DELETE" },
    ),
  /** The Teams app package, as a download link — the browser owns the save. */
  teamsAppPackageUrl: (id: string) => `${BASE}/admin/integrations/connections/${id}/teams-app`,
  listConnectionPlacements: (id: string, limit = 200) =>
    request<{ placements: IntegrationPlacement[] }>(
      `/admin/integrations/connections/${id}/placements?limit=${limit}`,
    ),

  // App integrations — what an app sends itself.
  getAppIntegrations: (id: string) =>
    request<AppIntegrations>(`/apps/${id}/integrations`),
  listAppSends: (id: string, limit: number = 20) =>
    request<{ sends: IntegrationSend[] }>(`/apps/${id}/sends?limit=${limit}`),
  testAppIntegration: (id: string, kind: string, to?: string) =>
    request<{ send_id: string; status: string }>(
      `/apps/${id}/integrations/test`,
      {
        method: "POST",
        body: JSON.stringify({ kind, to }),
      },
    ),
  /** Attaching is an administrator's decision — an app has no integration of a
   * kind until one is made. Detaching forgets the destination; disabling keeps
   * it. */
  attachAppIntegration: (
    id: string,
    kind: string,
    data: Partial<{ connection_id: string; enabled: boolean }> = {},
  ) =>
    request<AppIntegrations>(`/apps/${id}/integrations/${kind}`, {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  detachAppIntegration: (id: string, kind: string) =>
    request<{ attached: boolean; kind: string }>(
      `/apps/${id}/integrations/${kind}`,
      {
        method: "DELETE",
      },
    ),
  /** An empty id clears the choice and returns to the connection's default. A
   * chosen destination that stops working fails rather than falling back —
   * silently rerouting an app's alerts somewhere nobody chose is a disclosure. */
  setAppIntegrationDestination: (
    id: string,
    kind: string,
    destinationId: string,
  ) =>
    request<AppIntegrations>(`/apps/${id}/integrations/${kind}/destination`, {
      method: "PUT",
      body: JSON.stringify({ id: destinationId }),
    }),
  listAppIntegrationDestinations: (id: string, kind: string) =>
    request<{ label: string; destinations: IntegrationDestination[] }>(
      `/apps/${id}/integrations/${kind}/destinations`,
    ),

  getIntegrationSettings: () =>
    request<IntegrationSettings>("/admin/integration-settings"),
  updateIntegrationSettings: (data: Partial<IntegrationSettings>) =>
    request<IntegrationSettings>("/admin/integration-settings", {
      method: "PUT",
      body: JSON.stringify(data),
    }),
  /** The ledger, install-wide. Undefined and empty filters are dropped rather
   * than sent as blanks. */
  listIntegrationSends: (filter: {
    kind?: string;
    /** "token" narrows to access-token issuances. */
    action?: string;
    status?: string;
    recipient?: string;
    days?: number;
    limit?: number;
    app_id?: string;
  }) => {
    const params = new URLSearchParams();
    Object.entries(filter).forEach(([k, v]) => {
      if (v !== undefined && v !== "") params.set(k, String(v));
    });
    return request<{
      sends: IntegrationSend[];
      kinds: string[];
      /** Each kind's display name — "Google Drive", never "Gdrive". */
      kind_titles?: Record<string, string>;
      retention_days: number;
    }>(`/admin/integration-sends?${params.toString()}`);
  },
};

/**
 * The knowledge base pointer, resolved for the caller who asked. Every field is
 * optional because the negative answer is `{}` — and it is `{}` for "nothing is
 * picked", "the app is gone" and "not for you" alike.
 *
 * `slug` rather than a URL: `appUrl()` in `lib/app-url.ts` is the one client-side
 * construction of an app's address and is already correct on a single-origin
 * install, where the server deliberately reports no apps origin.
 */
export interface KnowledgeBase {
  app_id?: string;
  slug?: string;
  label?: string;
  /** The administrator's own sentence about it. Absent when they have not
   *  written one, and the card renders the product's default instead. */
  description?: string;
}

/** What is stored, for the administrator's page. `app_id` may name an app that
 *  no longer exists — that is the case this shape exists to make visible. */
export interface KnowledgeBasePick {
  app_id: string;
  label: string;
  /** Always present on this shape, empty string included: the page that owns
   *  the pick seeds a form from it and has to tell empty from absent. */
  description: string;
}

export type AnnouncementLevel = "info" | "warning" | "critical";
export interface Announcement {
  message: string;
  level?: AnnouncementLevel;
  updated_at?: string;
  updated_by?: string;
}

/** `publishers` includes administrators — an administrator is a publisher, and
 *  an announcement about the build queue that reached every publisher except
 *  the people who run the queue would be the one failure this switch can make.
 *  The label says so; the enum stays two values. */
export type AnnouncementAudience = "all" | "publishers";

/** `sent` is terminal whatever the outcomes were. An announcement where twelve
 *  sends failed is *sent, with twelve failures*, and the counts say so. */
export type AnnouncementStatus = "draft" | "sending" | "sent";

export type AnnouncementRecipientStatus =
  | "pending"
  | "delivered"
  | "failed"
  | "skipped"
  | "abandoned";

export interface AnnouncementRecipient {
  id: string;
  announcement_id: string;
  user_id?: string;
  login: string;
  email?: string;
  status: AnnouncementRecipientStatus;
  /** `locked` or `no_address` for a skip; the relay's own words for a failure. */
  reason?: string;
  attempts: number;
  created_at: string;
  completed_at?: string;
}

export interface AnnouncementEmail {
  id: string;
  subject: string;
  body: string;
  link_url?: string;
  link_text?: string;
  audience: AnnouncementAudience;
  status: AnnouncementStatus;

  created_by?: string;
  created_at: string;
  updated_at: string;
  cloned_from?: string;
  /** Set on a resend: the announcement whose failed and abandoned people it
   *  went to. The original may have been deleted since. */
  resend_of?: string;
  /** Set on an original that has been resent — at most once. */
  resent_as?: string;
  sent_by?: string;
  sent_at?: string;
  completed_at?: string;

  /** The size of the audience at send time. It never moves again. */
  resolved: number;
  delivered: number;
  failed: number;
  skipped: number;
  abandoned: number;

  created_by_login?: string;
  sent_by_login?: string;

  /** Present on the single read: every resolved person and what happened to
   *  them, worst news first. A count cannot answer "why did Bob not get it". */
  recipients?: AnnouncementRecipient[];
  /** Whether this install has a relay at all. Not a permission. */
  can_send?: boolean;
}

export interface AnnouncementEmailList {
  announcements: AnnouncementEmail[];
  can_send: boolean;
}

export interface AnnouncementEstimate {
  audience: AnnouncementAudience;
  total: number;
  deliverable: number;
  skipped_locked: number;
  skipped_no_address: number;
  resolved_at: string;
}

export interface AnnouncementEmailInput {
  subject?: string;
  body?: string;
  link_url?: string;
  link_text?: string;
  audience?: AnnouncementAudience;
}

export interface PasswordLoginResult {
  /**
   * The password was right, and it is the initial one. No session was issued —
   * only the right to replace it.
   */
  must_change_password?: boolean;
  /** Why the change is being asked for, in the server's words. */
  reason?: string;
  ok?: boolean;
  /** Where to land, as the server validated it. */
  redirect?: string;
}

// Types
/**
 * When each line of a build log was written (ADR §155): line `i`'s time is
 * `start_ms + offsets_ms[i]`, Unix milliseconds. Shorter than the log when
 * lines were added after the build; absent for a build that recorded none.
 */
export interface BuildLineTimes {
  start_ms: number;
  offsets_ms: number[];
}

/** The two layers of the display zone, as GET /auth/me carries them (ADR §154).
 *  `resolveDisplayTimeZone` in lib/time.ts is the only reader. */
export interface TimeZonePreference {
  /** "" follows `install`; "browser" is this person’s browser; else a zone. */
  user: string;
  /** "" is each viewer’s browser; else "UTC" or an IANA name. */
  install: string;
}

export interface User {
  id: string;
  /** Present on GET /auth/me only. */
  time_zone?: TimeZonePreference;
  login: string;
  /** Display name as the provider last asserted it, refreshed on every
   * sign-in. Empty when the provider does not send one — show `login` then,
   * never a fabricated name. */
  name?: string;
  email: string;
  avatar_url: string;
  created_at: string;
  system_role?: "system_admin" | "publisher" | "viewer";
  /** Set when the account is locked (admin action); absent for active users. */
  locked_at?: string;
  /** Last sign-in, not last request. Absent for an account that has never
   * signed in since the platform started recording it. */
  last_login_at?: string;
  /** When an administrator last reinstated the account. Absent for one that
   * has never been locked. */
  unlocked_at?: string;
  /**
   * Set when a sign-in provider created the account under a `pending` policy
   * and nobody has activated it yet. Such an account exists and is refused at
   * every sign-in until an administrator lets it in and picks its role.
   *
   * Not a lock: `locked_at` is something taken away, this is something never
   * given. An account can carry both.
   */
  pending_activation_at?: string;
  /** Set when an administrator marked this as the scanner account. */
  scanner_since?: string;
  /**
   * The dormancy clock: the newest of created_at, last_login_at, unlocked_at
   * and the newest use of any of this person's personal API keys. Always
   * present on the admin list, whether or not dormancy locking is enabled.
   *
   * Show this, never `last_login_at`, when the question is "is this account in
   * use" — a key authenticates its owner without minting a session, so an
   * account running a nightly job has one sign-in and looks abandoned.
   */
  last_active_at?: string;
  /** When one of this person's personal API keys was last used. Absent if
   * never. It is what makes "Never signed in" beside a fresh clock legible
   * instead of contradictory. */
  last_key_use_at?: string;
}

/** One group the user belongs to. `added_at` is absent for memberships made
 * before the platform recorded it — unknown, not "just now". */
export interface UserGroupMembership {
  id: string;
  name: string;
  added_at?: string;
}

/** One external account that signs in as this user. The provider's subject is
 * deliberately not exposed. */
export interface UserIdentity {
  provider_id: string;
  provider_name: string;
  login: string;
  name?: string;
  email: string;
  created_at: string;
  last_login_at?: string;
}

/** Admin-only account record — GET /users/{id}. Apps the user can reach come
 * from getUserAccess, which computes the effective role per app. */
export interface UserDetail {
  user: User;
  groups: UserGroupMembership[];
  identities: UserIdentity[];
  /** Whether this person asked to publish, when, and who decided. Newest
   *  first, and on this response rather than a second fetch of the queue: an
   *  administrator promotes from a `<Select>` on this page. */
  promotion_requests: PromotionRequest[];
}

/**
 * What the idle sweep (ADR §48) will do to this account, and when.
 *
 * The server decides whether this is present at all: absent when the sweep is
 * off, and absent for an administrator, whom it never locks. So the client
 * renders it when it is there and never asks whether it should — a notice that
 * cannot come true is a lie with a date on it, and that judgement is not the
 * page's to make. The threshold itself lives in platform settings, which an
 * ordinary account may not read.
 */
export interface MeDormancy {
  threshold_days: number;
  locks_on: string;
}

/** The account's own record — GET /me/account. `UserDetail` plus the facts
 * only the subject is owed. */
export interface MyAccount extends UserDetail {
  dormancy?: MeDormancy;
  /** Whether a password form should exist at all. Exactly one account on any
   * install can change a password — the built-in administrator — and only
   * while local sign-in is on. False means render nothing, not a disabled form
   * explaining that your identity provider owns your password. */
  can_change_password: boolean;
}

/** One session event on your own account. `action` carries the outcome; the
 * audit log's `before`/`after` documents are never part of this. */
export interface MySignin {
  occurred_at: string;
  action: string;
  ip: string;
  user_agent: string;
}

export interface MySigninsResponse {
  sign_ins: MySignin[];
  /** The window and cap the server applied — both fixed, neither a parameter. */
  days: number;
  limit: number;
}

/**
 * Where an app runs. Never a backend name: an app is "isolated", not
 * "fargate" or "pod", so an install that changes cluster technology does not
 * change what a developer sees.
 */
export type ExecutionMode = "shared" | "isolated";

/**
 * Why a log tail is empty. A closed set from the server, and the reason the
 * Logs tab can stop saying "No log output yet." to six different situations —
 * one of which is "your app is fine".
 */
/**
 * One line of a workload's output, labeled by the backend that read it.
 *
 * The labels are the backend's to produce: the runner's marker is a private
 * wire format, and the browser once held a parser for the shared backend's —
 * which is why the Logs tab could serve exactly one execution mode.
 */
export interface WorkloadLogLine {
  /** Absent when nothing stamped the line — an unmarked line carries no time. */
  at?: string;
  source: "app" | "platform";
  stream: "stdout" | "stderr";
  line: string;
}

export type LogAbsenceReason =
  | "never_deployed"
  | "stopped"
  | "sleeping"
  | "locked"
  | "launching"
  | "no_workload";

export interface App {
  id: string;
  slug: string;
  name: string;
  /** A DNS name of the app's own, set by an administrator; absent when none. */
  hostname?: string;
  /** Whether that hostname is served. */
  hostname_enabled?: boolean;
  /** Where a visitor reaches the app now — read it through `appUrl(app)`. */
  public_url?: string;
  /** The owning **user**, and empty when a group owns the app. Exactly one of
   *  this and owner_group_id is set — the pair is a database constraint, not a
   *  convention. Render them through <AppOwner>, which knows about both. */
  owner_id: string;
  owner_group_id?: string;
  repo_url: string;
  branch: string;
  /** The folder of `repo_url` this app is built from; empty is the
   *  repository's root. */
  repo_subdir: string;
  framework?: string;
  port: number;
  /**
   * The process-manager name of the release currently serving, and **empty for
   * an app that has never had a successful build**.
   *
   * That emptiness is the only thing the SPA reads it for: "never deployed" is
   * a different fact from every `status` value, since a new app and one a
   * person switched off are both `stopped`. It is not an address and nothing
   * here may take it apart — the platform builds an app's URL server-side.
   */
  current_release: string;
  /**
   * `sleeping` is an app the platform stopped after an idle timeout, and is
   * deliberately not `stopped`: it wakes itself on the next visit and still
   * picks up a git push, while a stopped app was switched off by a person.
   */
  status:
    "building" | "running" | "crashed" | "stopped" | "failed" | "sleeping" | "ready";
  /**
   * The platform lock — an administrator's decision that nobody may reach this
   * app. Absent means unlocked.
   *
   * Deliberately *not* a `status` value. Status describes the workload and this
   * describes a decision about the app, and the two are orthogonal: a locked app
   * is usually `stopped`, but it can be `crashed` or `sleeping` too and both
   * facts still hold. Read them together, never one instead of the other.
   *
   * `locked_message` is what the administrator typed for this app; empty means
   * the install-wide default applies, so never render it raw as the reason —
   * the API resolves the fallback chain everywhere it matters.
   */
  locked_at?: string;
  locked_by?: string;
  locked_message?: string;
  /**
   * Who placed it: `admin`, or `dependency` for a lock the dependency policy
   * placed by itself. It exists because `locked_by` cannot answer that — it is
   * empty both for a lock nobody has and for one whose administrator has since
   * been deleted. Absent reads as `admin`, which is what every lock meant
   * before the platform could place one.
   */
  locked_source?: "admin" | "dependency";
  visibility: AppVisibility;
  /**
   * Whether this app appears in the list of somebody who holds nothing on it,
   * while the install-wide `list` rung is on.
   *
   * The owner's half of that rung — the install says whether *anything* is
   * listed, this says whether *this one* is — and it only ever subtracts from
   * what the install gave: a grant always outranks it, so un-listing can never
   * hide the app from somebody it was shared with.
   *
   * **Inert above `private`**, because a public or signed-in-visible app is
   * already listable to everyone who could open it. The control is not drawn at
   * those visibilities, and the stored value is kept so it comes back if the app
   * returns to private.
   */
  listable: boolean;
  /** Origins allowed to put this app in an iframe, beyond the install itself.
   *  Always a list — empty means this Toft and nothing else, which is not
   *  the same as anyone. */
  frame_ancestors: string[];
  /**
   * Whether *this install* publishes to people who are not signed in at all.
   *
   * It is true on every app here — including private ones, which is
   * what lets the Access tab grey out Public before anyone chooses it. When it
   * is true a `public` app is authorized as `authenticated`; the column above
   * is still the owner's decision and is still what the page shows.
   */
  public_withheld?: boolean;
  /** The mode the *next* deployment will run under. An admin may change it freely. */
  execution_mode: ExecutionMode;
  /**
   * The mode the app's *current release* is actually running under. It only
   * changes on a deployment, so it can lag execution_mode — which is exactly
   * what "takes effect on the next deploy" looks like from here.
   */
  running_mode?: ExecutionMode;
  auto_deploy: boolean;
  /** Prune old releases automatically. Only an app with this **off** can ever
   * be blocked from deploying by the version cap; rollback never is. */
  auto_delete_releases: boolean;
  /** The owner's figure, stored unclamped: the conflict with the platform cap
   * is shown, never refused, so raising the cap restores their intent. */
  releases_kept: number;
  /** An administrator's exemption from idle sleep. */
  never_sleep: boolean;
  /** This app's own memory ceiling in MB, when an administrator has set one.
   * 0 means the install's number, never "uncapped". */
  memory_limit_mb: number;
  /** This app's own CPU limit in millicores, by the same rule. */
  cpu_limit_millicores: number;
  /** Whether this app's people are told when it breaks. It governs the
   * notification only — every occurrence is recorded either way. */
  notify_failures: boolean;
  created_at: string;
  /** When this **row** last changed, which is not a deploy date: a status
   *  transition, a doze, a lock and the git poller all write it. Do not label
   *  a column over it "Last deployed" — that is what `last_deployed_at` is. */
  updated_at: string;
  /** When the *serving* release arrived. Absent means never deployed, which is
   *  a third thing from a date and from unknown: the server computes it on
   *  every app row, so absent is the answer. */
  last_deployed_at?: string;
}

/**
 * Durable storage an administrator has provisioned.
 *
 * A volume is a real POSIX filesystem for state — SQLite, caches, working
 * files. A store is an object-store prefix for blobs, with whole-object
 * semantics that the mount itself enforces: no rename, no append, and
 * overwrite and delete only where they were granted.
 */
export type StorageKind = "volume" | "store";

/**
 * What is *underneath* a resource, which the kind deliberately does not say.
 *
 * A file store is a file system to the app that mounts it whichever of these
 * it is; the difference is the operator's — which class provisions it, which
 * prerequisites a failure names, and whether there are objects behind it.
 *
 * - `efs` — an EFS access point. No bucket, so no grants and no Files page.
 * - `s3files` — an Amazon S3 Files file system: a bucket mounted with NFS
 *   semantics, so it is a file system *and* has a data plane.
 * - `host` — a directory under a root the operator mounted, on an install with
 *   no cluster. Toft cannot see through the mount, so this says what is
 *   known and nothing more.
 * - `azurefiles` — an Azure Files share. Same class variable as `efs`, and the
 *   same limit as `host`: the platform cannot see whether the operator mounted
 *   SMB or NFS 4.1, which is why the embedded-database condition is copy on the
 *   create form rather than a fourth backing.
 * - `mountpoint` — an object-store prefix mounted with Mountpoint for Amazon S3.
 * - `blob` — an Azure Blob container prefix mounted with blobfuse2. The first
 *   store that is not a Mountpoint mount, and the only backing whose mount
 *   ladder is narrower than its kind's: see `mountableAccessLevels`.
 */
export type StorageBacking =
  | "efs" | "s3files" | "host" | "azurefiles"
  | "mountpoint" | "blob";

/** A host directory an operator mounted and declared, for shared-mode storage. */
/**
 * One declared place volumes live — a host root or a cluster storage class.
 * The key and name are the operator's; the label is what an administrator
 * calls it, and the name when nobody has.
 */
export interface StorageDrive {
  key: string;
  kind: "root" | "class";
  name: string;
  label: string;
  description?: string;
  path?: string;
  class?: string;
  backing?: StorageBacking;
  read_only?: boolean;
  /** A class volumes naming no class use. */
  default?: boolean;
  /** False for a name resources still record that this install no longer declares. */
  declared: boolean;
  resource_count: number;
  /**
   * Whether deleting a resource on this drive removes its files: a writable
   * host root always, a class when its driver is known to (ADR §12a).
   */
  delete_removes_data?: boolean;
}

/** A deleted resource whose volumes the cluster has not finished deleting; its name is held until then. */
export interface StorageDeleting {
  slug: string;
  kind: StorageKind;
  started_at: string;
  /**
   * Finished, but the volume's directory was kept on the file system; the
   * name is held until an administrator releases it, because a new volume of
   * that name would be handed those files.
   */
  kept?: boolean;
}

/** One thing a delete did not remove, and why. */
export interface StorageLeftBehind {
  what: string;
  reason: string;
}

/** What a delete did: `deleting` while the cluster finishes and the name is held. */
export interface StorageDeleteResult {
  state: "deleted" | "deleting";
  removes_data?: boolean;
  removed?: string[];
  left_behind?: StorageLeftBehind[];
}

/** One resource's evidence in a re-point: what was found, or what was not. */
export interface StorageRepointCheck {
  slug: string;
  evidence?: string;
  problem?: string;
}

export interface StorageRepoint {
  from: string;
  to: string;
  checks: StorageRepointCheck[];
  moved: boolean;
}

export interface StorageRoot {
  name: string;
  path: string;
  /** What an administrator calls it; the name when nobody has. */
  label?: string;
  /**
   * A level in shared mode is which mount the resource is under, never a
   * permission Toft applies — so a read-only root serves `read` mappings
   * and a writable one serves `write`.
   */
  readOnly?: boolean;
}

/**
 * A ladder — each level is the previous plus one mount flag. Volumes offer
 * read and write; only a store has the two rungs above, because only a store
 * has flags to gate them.
 */
export type StorageAccess = "read" | "write" | "full";

export interface StorageResource {
  id: string;
  slug: string;
  name: string;
  kind: StorageKind;
  backing: StorageBacking;
  /** Empty means the backing's default class. */
  storage_class?: string;
  /** Which of the operator's host mounts this lives under, on a shared install. */
  storage_root?: string;
  /**
   * Mapped only by folder, no mapped folder inside another, and on a host root
   * each folder its own group. Fixed at creation.
   */
  partitioned: boolean;
  bucket?: string;
  prefix?: string;
  claim_name: string;
  description: string;
  /** How many apps mount this. */
  mapping_count: number;
  /**
   * Who can reach this from *outside*, counted as the grants dialog lists them
   * — subjects, not resolved people, so a group of forty is one group here and
   * one row there. Always 0 where the resource names no bucket, which is what
   * a grant needs: there is nothing to sign for.
   */
  grant_users: number;
  grant_groups: number;
  /**
   * Who may *attach* this without being an administrator, counted the same way.
   * Unlike the grant counts, these are meaningful on a volume: a volume takes
   * no grant because it has no data plane, and is very much attachable.
   */
  manager_users: number;
  manager_groups: number;
  created_at: string;
  updated_at: string;
}

/** One resource on one app's filesystem, at one path, at one access level. */
export interface StorageMapping {
  id: string;
  resource_id: string;
  app_id: string;
  path: string;
  access: StorageAccess;
  /**
   * Which directory of the resource is mounted, and absent is the whole
   * resource. Part of the mapping's identity, so one app may mount two folders
   * of one resource at two paths and two levels.
   */
  folder?: string;
  resource_slug?: string;
  resource_name?: string;
  resource_kind?: StorageKind;
  resource_partitioned?: boolean;
  app_slug?: string;
  app_name?: string;
  /**
   * Which delegation wrote this row — `admin`, `user:<id>` or `group:<id>`.
   * It is what a revocation collects by: an administrator's write stamps
   * `admin` and adopts whatever it touches, so a mapping an administrator has
   * edited never disappears with somebody else's delegation.
   */
  authority: string;
  /**
   * `manifest` when the app's toft.toml declares this mapping — written and
   * removed by its deploys, read-only in the console — and `console` when a
   * person attached it. Absent from an install older than the field.
   */
  source?: "console" | "manifest";
  /** The delegate's or group's name behind `authority`, empty on `admin`. */
  authority_name?: string;
  created_at: string;
  updated_at: string;
}

/**
 * One subject's delegation on one resource: they may attach it to an app they
 * can already edit, up to `max_access`.
 *
 * On a **store** it also confers the data plane at that ceiling — the holder
 * browses, downloads and uploads through /storage exactly as a grant at that
 * level would let them. On a **volume** it confers nothing but the ability to
 * attach, because a volume has no data plane to confer.
 */
export interface StorageManager {
  id: string;
  resource_id: string;
  subject_type: StoreSubjectType;
  subject_id: string;
  max_access: StorageAccess;
  subject_name?: string;
  subject_email?: string;
  /**
   * The apps this delegation's mappings mount — by name, not a count, because
   * revoking restarts them and a number does not let an administrator warn
   * anybody.
   */
  apps: StorageManagedApp[];
  created_at: string;
  updated_at: string;
}

export interface StorageManagedApp {
  id: string;
  slug: string;
  name: string;
  path: string;
  access: StorageAccess;
}

export interface StorageManagerList {
  managers: StorageManager[];
  kind: StorageKind;
  /**
   * What "manage" promises here. Where there are objects behind the resource a
   * delegation also confers the data plane at its ceiling; where there are none
   * it confers nothing but the ability to attach. The kind alone stopped being
   * able to answer this the moment a file store could be a bucket.
   */
  has_data_plane: boolean;
}

/** What a revocation did, and what it is now restarting. */
export interface StorageRevocation {
  restarting: StorageManagedApp[];
  note: string;
}

/**
 * One resource this caller may attach, from `GET /me/storage`.
 *
 * It carries no bucket and no prefix: those are the containment boundary and
 * belong to an administrator. A delegate picks a resource and a mount path, and
 * neither requires knowing which bucket it is.
 */
export interface AttachableResource {
  id: string;
  slug: string;
  name: string;
  kind: StorageKind;
  description: string;
  max_access: StorageAccess;
  /** A folder is required to attach it. */
  partitioned: boolean;
  authority: string;
}

export interface AttachableStorageList {
  resources: AttachableResource[];
  /**
   * Whether a shared-mode app can hold storage at all here — that is, whether
   * the operator declared a host root. The attach dialog warns from this rather
   * than from the mode alone, because the refusal it is warning about is the
   * one the deploy will actually make.
   */
  shared_mode: boolean;
}

export interface StorageResourceList {
  resources: StorageResource[];
  /** What this install can actually provision. A kind absent here has no backing configured. */
  kinds: StorageKind[];
  /** Which backings each kind can have here — a backing with no class or root configured is absent. */
  backings: Partial<Record<StorageKind, StorageBacking[]>>;
  /**
   * Every drive: the operator's host roots and a cluster's declared classes,
   * labelled, plus any name resources still record that nothing declares.
   */
  drives: StorageDrive[];
  /** Deletes the cluster has not finished; each holds its name until it has. */
  deleting: StorageDeleting[];
  /** False with no cluster: a store is never mounted and is reached over signed URLs. */
  stores_mounted?: boolean;
  /** The install's cap on resources, and an app's on mappings — the numbers the server refuses at. */
  max_resources: number;
  max_app_mappings: number;
  /** Whether a partitioned resource may be created under a host mount here. */
  partitioned_roots: boolean;
}

/** A grant's subject. A group is a set of humans, never an actor. */
export type StoreSubjectType = "user" | "group";

/**
 * One subject's reach into one store, from outside the platform.
 *
 * The level resolves union-of-max: a person holds the highest of their own
 * grant and the grants of every group they belong to, so adding a narrow direct
 * grant never silently revokes a wider one they had through a group.
 */
export interface StoreGrant {
  id: string;
  resource_id: string;
  subject_type: StoreSubjectType;
  subject_id: string;
  access: StorageAccess;
  /** Which directory of the store the grant is on; absent is the whole store. */
  folder?: string;
  subject_name?: string;
  subject_email?: string;
  created_at: string;
  updated_at: string;
}

export interface StoreGrantList {
  grants: StoreGrant[];
  /**
   * Whether this install can actually mint URLs. Grants can be recorded either
   * way — false means the operator has not switched uploads on, so the panel
   * says the grants are not yet usable rather than letting somebody discover it
   * from a 404 in the CLI.
   */
  uploads_enabled: boolean;
}

/**
 * One store this caller reaches, as `GET /store` answers it.
 *
 * No bucket and no prefix, on purpose: a person who may upload has no business
 * knowing which bucket the platform signs against, and a client that could see
 * the prefix would start building keys out of it. `access` is the highest of
 * this person's own grant and their groups' — one number, never the rows it
 * came from.
 */
export interface StoreSummary {
  slug: string;
  name: string;
  description?: string;
  /** How app code reaches it; never where it lives. */
  kind: StorageKind;
  /**
   * Which directory of the store this row is about; absent is the whole store.
   * **A slug can appear more than once in this list**: reach is per scope, so
   * somebody granted two folders of one store has two rows, each of which is
   * somewhere the browser can be rooted.
   */
  folder?: string;
  access: StorageAccess;
}

/** One object, named relative to the store's prefix. */
export interface StoreObjectSummary {
  name: string;
  size: number;
  last_modified: string;
  etag?: string;
}

/**
 * One page of a listing.
 *
 * `prefixes` are the folders at this level — relative, slash-terminated, and
 * absent from a flat listing, which has no levels. `next_cursor` is S3's own
 * continuation token passed through and never an offset: an offset over a
 * prefix being written to skips and repeats.
 */
export interface StoreObjectPage {
  objects: StoreObjectSummary[];
  prefixes?: string[];
  /**
   * The level this caller holds on the folder that was listed — not on the
   * store. Somebody granted `read` on one folder and `full` on another has two
   * different sets of controls inside one store, and the page draws them from
   * this rather than from a store-wide number that would offer a delete button
   * which always refuses. Absent at a root the caller does not reach, where the
   * listing is their granted folders and there is nothing to do in it.
   */
  access?: StorageAccess;
  next_cursor?: string;
}

/** A URL the platform signed, the headers it signed into it, and when it dies. */
export interface SignedStoreRequest {
  url: string;
  method: string;
  headers: Record<string, string>;
  /** The object's name within the store — what was asked for, echoed back. */
  key: string;
  expires_at: string;
}

/**
 * A name, encoded for the wildcard segment of the object routes.
 *
 * Per segment, because the slashes in `2026/08/sales.parquet` are the key's own
 * structure and have to survive; everything else inside a segment is escaped,
 * because a `#` or a `?` in a filename would otherwise cut the name short at
 * the server.
 */
function encodeStoreName(name: string): string {
  return name.split("/").map(encodeURIComponent).join("/");
}

export interface AppStorage {
  mappings: StorageMapping[];
  /** True when the running release predates these mappings — a deploy is owed. */
  pending: boolean;
  note?: string;
}

/**
 * What the platform knows about an app's saved JSON document — never its
 * contents. `version` is 0 and `updated_at` null when the app has never written
 * one, which is the same state as having cleared it.
 */
export interface AppDataMeta {
  version: number;
  bytes: number;
  updated_at: string | null;
  /** Present on a reset: whether there was anything to remove. */
  deleted?: boolean;
}

/** Apps deployed from an archive have an upload:// pseudo-URL instead of a git remote. */
export function isGitSource(app: Pick<App, "repo_url">): boolean {
  return !app.repo_url.startsWith("upload://");
}

/** The branch a deployment records when its code arrived as a bundle rather
 * than from a remote — an archive through the browser, or `toft deploy` from a laptop.
 * It is the value the row already carried for an uploaded app; a wire contract,
 * not a display string. */
export const UPLOAD_BRANCH = "upload";

/**
 * Whether this app is serving code that did not come from the repository its
 * page names.
 *
 * True only when both halves hold: the running release came from a bundle, and
 * there is a repository for it to differ from. An app with no remote has
 * nothing to diverge from, and its rows say `upload` on every deploy — showing
 * a notice there would be telling somebody their app is not what it is.
 *
 * The consequence is real and is the reason this exists: the repository wins
 * next. Anyone pressing Redeploy, and any push the auto-deploy poller sees,
 * rebuilds from the remote and replaces what a laptop pushed.
 */
export function servingUploadedCode(
  app: Pick<App, "repo_url">,
  serving?: Pick<Deployment, "branch">,
): boolean {
  return isGitSource(app) && serving?.branch === UPLOAD_BRANCH;
}

export interface AppMetrics {
  /** Percent. Absent when the backend running the app takes no reading — an
   *  isolated workload reports status, uptime and restarts and nothing else.
   *  Render "not available"; there is no zero to fall back on, deliberately. */
  cpu?: number;
  /** MB — already converted by the backend. Absent on the same terms as cpu. */
  memory?: number;
  /** Elapsed seconds. */
  uptime: number;
  restarts: number;
  status: string;
  /** How the workload last ended: a code, or the signal and no code. Absent
   *  when the backend did not say, which is not 0. */
  exit_code?: number;
  exit_signal?: string;
  /** The backend's own verdicts: backing off a crash loop; stopped at its
   *  memory limit. */
  crash_looping?: boolean;
  oom_killed?: boolean;
}

/** An app's owner as an app's readers get it: who to ask, and nothing about
 *  the account. An administrator's read carries the whole `User`; nothing
 *  outside the admin pages may count on more than this. */
export interface AppOwnerIdentity {
  id: string;
  login: string;
  name?: string;
  avatar_url: string;
}

/**
 * One row of `GET /apps`.
 *
 * **A row with `can_open: false` — held at the `list` rung — carries only
 * `id`, `name`, `slug`, `framework`, `owner_id`, `owner_group_id`, `owner` and
 * the flags** (`can_edit`, `can_delete`, `can_open`, `mine`, `listable`). The
 * rung discloses the listing's row and not one field more (ADR §90), so
 * `status`, `visibility`, the release and everything else are absent from it
 * at runtime whatever this type says: read them only behind `can_open`.
 */
export interface AppWithMetrics extends App {
  metrics?: AppMetrics;
  owner?: AppOwnerIdentity;
  /** Stored values plus the derived Framework pair. */
  tags?: AppTag[];
  /** The dependency glyph, absent unless this viewer may edit the app *and*
   *  one of the two conditions that earn a notice on its own page holds. Not a
   *  column and not a status: the app is running, and putting a finding in the
   *  status filter would make somebody choose between "crashed" and
   *  "vulnerable" for an app that is neither. */
  dependency_alert?: {
    malware: boolean;
    blocking: boolean;
    /** The worst counted, unwaived advisory serving — the set the tab badge
     *  counts. Red when critical, amber otherwise. */
    severity?: string;
  };
  /** The release line this app is on, folded by the server so the facet and
   *  the census agree. **Administrator-only**, for the reason the census is:
   *  a viewer sees more apps than they may edit, and a version per row is a
   *  partial copy of what /admin/estate/languages is gated to withhold. */
  runtime?: AppRuntime;
  /** Whether the app declares a job, for `/?kind=jobs`. **Administrator-only**,
   *  like `runtime`: it is a census fact. */
  has_jobs?: boolean;
  /** What this caller may do with the row — answered per row by the same
   *  `auth.Authorize` every handler uses, rather than re-derived here. The
   *  list's `⋯` menu offered Redeploy, Restart, Stop and Delete on every app a
   *  viewer could *read*, and the server refused each one. */
  can_edit: boolean;
  can_delete: boolean;
  /** Whether following this row leads anywhere. False exactly for a row held at
   *  the `list` rung: the app is in the list and opening it meets the gate. */
  can_open: boolean;
  /** Whether this app is *yours* — you own it, a group you belong to owns it,
   *  or somebody granted it to you. The apps list's Mine / All apps split, and
   *  deliberately not `can_open`: a public app belonging to a team you have
   *  never met is openable by everybody, and on an install with a published
   *  intranet that is most of the list. Mine has to mean a relationship
   *  somebody wrote down, or it stops meaning anything. */
  mine: boolean;
  /** The status of this app's unfinished build — `"pending"` or `"building"`
   *  — and **absent when there is none**.
   *
   *  Not the app's status and never to be merged into it. `status` describes
   *  the workload: an app that failed its last build is still failed while a
   *  new one runs, because what a viewer would reach is still not there. This
   *  is the second fact, rendered beside the first the way the pending
   *  execution mode already is. */
  deploying?: string;
}

/** One app's release line, in the census's own vocabulary. */
export interface AppRuntime {
  language: string;
  label: string;
  /** Empty unless `state` is "resolved". */
  series: string;
  state: EstateVersionState;
  /** The recorded version in full — `3.13.7` where `series` is `3.13`. Absent
   *  unless `state` is "resolved".
   *
   *  A count folds patch away or it fragments across two 3.13 patches; an app's
   *  own page is asking which interpreter is running *its* code, where the
   *  rounded answer would hide the difference between two of its releases. Both
   *  travel and each surface renders the one its question needs. */
  version?: string;
}

/** One env var as the API returns it — value present only when the var is visible. */
export interface AppEnvVar {
  key: string;
  value?: string;
  sensitive: boolean;
  /** A sensitive variable holding nothing: shown as “not set”. */
  unset?: boolean;
}

/**
 * The body of `PATCH /apps/{id}/env`. A key it does not name is left alone;
 * `value: ""` clears; `sensitive` absent keeps a stored flag and makes a new
 * row visible.
 */
export interface UpdateAppEnvRequest {
  set?: Record<string, { value?: string; sensitive?: boolean }>;
  unset?: string[];
  only_missing?: boolean;
}

/** What `PATCH /apps/{id}/env` did, per key. Never a value. */
export interface UpdateAppEnvResult {
  set: string[];
  /** Sent with the value and flag it already had: not written. */
  unchanged: string[];
  kept: string[];
  cleared: string[];
  unset: string[];
  not_present: string[];
}

export interface AppDetail {
  app: App;
  /** The owning **user**, in its public shape unless an administrator is
   *  reading. Absent when a group owns the app — then owner_group is the one
   *  that is set. Exactly one of the two, always. */
  owner?: AppOwnerIdentity;
  owner_group?: Group;
  metrics?: AppMetrics;
  env_keys?: string[];
  /** Only present for callers who may edit the app; sensitive entries never carry a value. */
  env_vars?: AppEnvVar[];
  /** Credentials an administrator attached: names and the key names they
   *  supply, never values — for the owner and an administrator alike, because
   *  values are read at the credential and not through an app. */
  credentials?: Credential[];
  /** Stored values plus the derived Framework pair. */
  tags?: AppTag[];
  /** Whether a private-repo credential is configured; the token itself never leaves the server. */
  has_git_token?: boolean;
  /** The connected git host this app clones through, or null for its own token
   *  (or none). Null rather than an empty object, so an install that never
   *  adopted connections renders the token field it always had. */
  git_connection?: DeveloperGitConnection | null;
  /** The platform's decisions about *this app*, read-only to its owner. An
   * administrator's setting is otherwise met only as a deploy that refuses. */
  policy?: AppPolicy;
  /** Which interpreter the serving release was built against, for the Runtime
   *  section of Settings. Absent for a viewer — a per-app version is a partial
   *  copy of the map the estate census is administrator-gated to withhold, and
   *  the exception that gate names is an editor reading their *own* app's.
   *
   *  Absent is never "the default": a missing value is a fact the page could
   *  not get, and the three states carry the rest. */
  runtime?: AppRuntime;
  /** The facts Overview's tiles print. Absent when they could not be read —
   *  the tiles are the least load-bearing thing on that page, so a count that
   *  did not come back costs the tiles and not the app. */
  summary?: AppSummary;
  /** How many saved variable changes the running workload was not started
   *  with. 0 for a viewer, and 0 when there is no workload to restart. Derived
   *  on the server from the app row, so it survives a page reload — which a
   *  toast does not, and which is the whole reason it is here. */
  unapplied_env_changes?: number;
  /** The Open in Garth link, built by the server. Present only for someone who
   *  may edit the app, on an install with a Garth address (ADR §158). */
  open_in_garth?: string;
}

/**
 * One fact per destination, travelling with the app.
 *
 * ADR §53 D4's rule, inherited from ADR §50's admin hub: **no tile makes a
 * request of its own.** A figure worth a round trip is worth a field on
 * `GET /apps/{id}`, which is one request either way; a destination whose fact
 * would cost more than a single indexed read gets no tile.
 */
export interface AppSummary {
  /** Principals with a grant, the owner's own row included. */
  grant_users: number;
  grant_groups: number;
  /** Jobs with a schedule, and how many of those are not paused. Both, because
   *  "3 schedules, all paused" and "3 schedules" are different answers. */
  schedules: number;
  schedules_enabled: number;
  /** The same 30-day window the Usage tab opens on, so the two cannot disagree. */
  /** How long since anybody wanted this app, and which of three facts the clock
   *  runs from — `visit`, `deploy` or `created`. Unwindowed, unlike the counts
   *  below: past thirty days those stop telling "quiet since 2022" apart from
   *  "deployed last week". Never a verdict — a quarter-end report opened four
   *  times a year is quiet by every measure here and doing its job. */
  quiet_days: number;
  quiet_basis: "visit" | "deploy" | "created" | "retention";
  quiet_since: string;
  requests_30d: number;
  viewers_30d: number;
  /** Absent means nothing has ever been recorded — which is not the same as
   *  nothing having gone wrong, and the copy has to keep them apart. */
  last_failure_at?: string;
}

/** One attached kind, as the app's own page reports it. */
export interface AppPolicyIntegration {
  kind: string;
  title: string;
  enabled: boolean;
  /** Where messages actually land — the app's own choice when its people made
   * one, and the connection's default otherwise. Naming the default rather than
   * reporting an absence is deliberate: "posting to #admin" is actionable. */
  destination?: string;
  /** How many objects this app has left in that system, for a kind that leaves
   * any. What the delete confirmation reads. */
  placements?: number;
}

/** Every field is read-only. The alternative to showing these is an owner
 * meeting them as a deploy that refuses for no visible reason, or an app that
 * keeps going to sleep. */
export interface AppPolicy {
  max_versions_per_app: number; // 0 when there is no cap
  /** The install's own memory ceiling in MB, which a per-app one is set
   *  against. 0 when there is none. `/system/settings` 404s to an owner, so
   *  this is the only place the app's page can read it. */
  platform_memory_limit_mb: number;
  /** The install's CPU limit per app in millicores, resolved; 0 is none. */
  platform_cpu_limit_millicores: number;
  effective_releases_kept: number;
  requested_releases_kept: number;
  releases_kept_conflict: boolean;
  /** How many builds can still be made current, absent when nothing counted
   * them (automatic cleanup on means the count cannot reach the cap). */
  versions?: number;
  deploys_blocked: boolean;
  blocked_reason?: string;

  /** Whether this app's repository is still one the install deploys from, and
   * the sentence a deploy would refuse with. Always true for an uploaded app.
   * The app keeps running either way — what stops is its next deploy. */
  source_allowed: boolean;
  source_reason?: string;

  idle_sleep_minutes: number; // this app's mode's timeout, 0 when that mode is off
  sleeps: boolean;
  /** Why this app is not slept: "disabled", "isolated" or "admin".
   * Distinguished rather than collapsed — they have different owners. */
  sleep_exemption?: string;
  /** The isolated sleep window as "19:00–07:00 Europe/London", present only
   * for an isolated app on an install whose window is on. */
  sleep_window?: string;

  /** Every kind attached to this app, whether an administrator has since
   * switched it off, and where its messages land.
   *
   * Here for the reason the sleep exemption is: a platform decision about *this
   * app* is one its owner is owed sight of. Without it, "why did nothing appear
   * in our channel" is unanswerable from the app's own page. Read-only. */
  integrations: AppPolicyIntegration[];
  /** Where its notifications land — the app's own channel, or the install
   * default while its people have not chosen one. */
  slack_channel?: string;

  email_attached: boolean;
  email_enabled: boolean;

  /** Whether this app may ask the platform who is looking at it.
   *
   * Absent for a viewer — the same gate the dependency block uses. Undefined
   * therefore means "you may not see this", not "off"; the switch renders only
   * when the field is present. */
  viewer_identity?: boolean;

  /** Where this app came from, when it came from the gallery *and* the gallery
   *  is switched on. Absent otherwise — off means the gallery is not part of
   *  this install, and a page describing something the viewer cannot act on is
   *  worse than one that says nothing. */
  gallery?: AppGalleryPolicy;

  /** What the dependency policy has decided about this app (ADR §39). Absent
   *  while the feature is off, and absent for a viewer — a package-and-version
   *  manifest of a running internal tool is a target map, which is the same
   *  reasoning that keeps this app's failure events off a viewer's Logs tab. */
  dependencies?: AppDependencyPolicy;
}

/**
 * The app's own view of the dependency policy.
 *
 * Two pairs have to stay apart here, and collapsing either is a wrong answer.
 * `mode` is what the install says and `enforcing` is whether a finding can
 * actually stop a deploy *right now* — a `block` install whose corpus has
 * never loaded is not enforcing. And `scanned` false is **not** zero findings:
 * it is the state most of an estate is in on the first day, with `scan_reason`
 * saying which of the two causes it is, because a redeploy fixes one and would
 * change nothing about the other.
 */
export interface AppDependencyPolicy {
  mode: DependencyMode;
  enforcing: boolean;

  scanned: boolean;
  /** "never_scanned" — a redeploy records its packages. "not_scanned_by_policy"
   *  — this install does not resolve dependencies at deploy, so a redeploy
   *  would not help, and the copy names the setting instead. "no_dependencies"
   *  — a static app, which is neither of the other two and is never blocked.
   *  "not_enumerable" — the app has dependencies and nothing can name them (an
   *  R app with no lockfile); the copy says what to commit. "not_covered" — the
   *  packages *are* recorded and shown, and no corpus checks them (an R app
   *  with a lockfile), so the inventory renders and the findings block says
   *  nothing was checked. Like "no_dependencies", "not_enumerable" and
   *  "not_covered" are never blocked. "unresolved" — the build could not list
   *  what it installs (a package with no wheel for this Python), so a redeploy
   *  of the same tree would not help either. "no_corpus" — the packages are
   *  recorded and shown, in a covered ecosystem whose advisory data has never
   *  loaded on this install, so nothing checked them. */
  scan_reason?: string;
  scanned_at?: string;
  /** Every number here is scoped to this, and the surfaces say so. */
  feed_as_of?: string;
  feed_stale: boolean;

  /** Which release the findings describe. */
  release?: string;
  counts: DependencyCounts;

  deploys_blocked: boolean;
  /** The same sentence the deploy would return — one wording, read before
   *  somebody presses Redeploy rather than after. */
  blocked_reason?: string;

  /** A malicious package in the release that is serving. Earns a notice in
   *  *every* mode, including one whose threshold would not block it: there is
   *  no low-severity credential stealer. */
  malware_serving: boolean;

  waivers: DependencyWaiver[];
}

/**
 * An app's own view of its catalog attachment, with five states kept apart on
 * purpose: up to date, an update is available, the item is no longer listed,
 * the versions cannot be compared, and — for an example — there is no update
 * channel at all. None of them may render as any other.
 */
export interface AppGalleryPolicy {
  item_id: string;
  kind: GalleryKind;
  /** From the catalog, so absent once the item is retired. */
  name?: string;
  source?: string;
  /** What is running. Empty until the first build succeeds. */
  installed_version?: string;
  catalog_version?: string;
  retired: boolean;
  version_unknown: boolean;
  /** Why, in the commonest case: nothing was recorded at install, rather than
   *  something recorded that will not parse. A different sentence — "cannot be
   *  compared" reads as a fault in an app that is working. */
  version_not_recorded: boolean;
  update_available: boolean;
  /** False for every example, by design. */
  updatable: boolean;
}

export interface AppUsageDay {
  day: string; // YYYY-MM-DD
  requests: number;
  viewers: number; // distinct signed-in viewers that day
  /** Top-level page loads, signed-in and anonymous alike. */
  page_loads: number;
  /** Distinct anonymous browsers that day. Absent on a day nothing was
   *  counted (the switch was off) — a gap, never a zero. Recorded days are
   *  read whatever the switch says now. */
  anonymous_browsers?: number;
}

export interface AppUsage {
  days: number;
  series: AppUsageDay[]; // days without traffic are absent
  /** `anonymous_browsers` is one union across the window, never a sum. */
  totals: { requests: number; viewers: number; page_loads: number; anonymous_browsers?: number };
  /** Whether the install counts anonymous browsers now. */
  audience_measurement?: boolean;
}

/** One bucket of the estate's audience (`GET /system/audience`). */
export interface AudienceBucket {
  start: string; // YYYY-MM-DD, the bucket's first day (a Monday for weeks)
  people: number;
  /** Null for a bucket with no count (the switch was off, or nobody came). */
  anonymous_browsers?: number | null;
  page_loads: number;
}

export type AudienceGrain = "day" | "week" | "month";

export interface SystemAudience {
  since: string;
  until: string;
  grain: AudienceGrain;
  audience_measurement: boolean;
  buckets: AudienceBucket[];
  /** People and browsers are distinct across the window, not sums. */
  totals: { people: number; anonymous_browsers?: number | null; page_loads: number };
}

/** One principal's traffic against an app. `anonymous` is the pool of
 * unidentified visitors to a public app, not a person; `removed` is traffic
 * whose account no longer exists — usage rows outlive users on purpose. */
export interface AppUsageViewer {
  kind: "user" | "anonymous" | "api_key" | "removed";
  id?: string;
  display_name: string;
  avatar_url?: string;
  requests: number;
  days: number; // distinct days with traffic
  first_day: string;
  last_day: string;
}

/** Distinct signed-in people who opened any app in each window. Anonymous
 * visitors and API keys are excluded — they are not headcount. */
export interface AppUsageViewers {
  days?: number;
  since: string;
  until: string;
  /** The oldest day this install still keeps, "" when it keeps everything. */
  retained_from: string;
  viewers: AppUsageViewer[];
}

/** One of Toft's three guides, at the folder for this install's version. */
export interface DocumentationSection {
  id: "user" | "admin" | "cli";
  name: string;
  summary: string;
  url: string;
}

/** `GET /documentation`. `url` is null under DOCS_URL=off, when there are no
 *  sections either; `version` is the minor the links describe either way. */
export interface DocumentationResponse {
  url: string | null;
  version: string;
  sections: DocumentationSection[];
  /** The site's page on the two coding-agent skills; null under DOCS_URL=off. */
  agent_skills_url: string | null;
}

/**
 * One feature, as Admin → Features renders it.
 *
 * It merges three things that are deliberately separate on the server: the
 * catalogue of what this binary offers, the license answer, and the
 * administrator's switch. `licensed` and `enabled` are two gates, not one —
 * somebody must be able to switch off a feature they have paid for without
 * surrendering the entitlement.
 */
export interface Feature {
  name: string;
  title: string;
  summary: string;

  /** Gate 1. `unlicensed` means the install has no license key at all, which is
   * the permissive default rather than a grant somebody was given — render it
   * as such rather than as a tick. */
  licensed: boolean;
  unlicensed: boolean;
  in_grace: boolean;
  license_note?: string;

  /** What this install is on, and what the feature asks for. Together they are
   * what makes a row render as **locked** — visible, labelled, with one line of
   * what it does and one button — rather than absent. That is the opposite of
   * *absent, not dormant*, and it is the one stated exception to it: a feature
   * nobody can see sells nothing. */
  tier: Tier;
  min_tier: Tier;

  /** Whether gate 2 exists for this feature at all. False renders the license
   * state and no switch: turning off the providers your colleagues sign in with
   * is not a thing this page offers, because there is nothing to switch — a
   * license decides whether *new* configuration may be written and never
   * whether existing configuration is honoured. */
  admin_switch: boolean;

  /** Gate 2. `ever_enabled` is what keeps a switched-off feature on the page:
   * absent and off are different states, and an administrator whose app stopped
   * answering needs to see why rather than find the row gone. */
  enabled: boolean;
  ever_enabled: boolean;
  enabled_at?: string;
  enabled_by?: string;
}

/**
 * What this install has bought. Derived on the server from the stored key and
 * the cached answer on every read, and stored nowhere — a key removed at 10:00
 * puts the install on the demo tier at 10:00.
 *
 * `demo` is the absence of a key, and it is the only tier that makes no
 * outbound request: the two are the same fact. `trial` is an ordinary license
 * with a short term rather than a concept of its own.
 */
export type Tier = "demo" | "free" | "trial" | "paid";

/** What a tier is called in a sentence. One place, so a refusal on one page and
 * a banner on another cannot word it differently. */
export const TIER_LABEL: Record<Tier, string> = {
  demo: "the demo tier",
  free: "the free tier",
  trial: "a trial license",
  paid: "a paid license",
};

/** What a tier is called as a value beside a label — "Tier: Paid", never the
 * sentence fragment "Tier: a paid license". */
export const TIER_NAME: Record<Tier, string> = {
  demo: "Demo",
  free: "Free",
  trial: "Trial",
  paid: "Paid",
};

/** Where this install exports traces and metrics.
 *
 * `configured: false` means no administrator has saved this document and the
 * OTEL_* environment is the whole configuration — the state every install
 * starts in. `source` says which of the two produced what is actually running.
 */
export interface TelemetrySettings {
  configured: boolean;
  source: "settings" | "environment" | "";

  enabled: boolean;
  /** The collector's base URL. The exporters append /v1/traces and /v1/metrics. */
  endpoint: string;
  /** Whether a credential is stored. The credential itself never leaves the server. */
  headers_set: boolean;
  /** …and whether that stored value can still be decrypted. `set` with
   * `unreadable` is a changed or lost ENCRYPTION_KEY: the form's "leave blank
   * to keep" would keep something unusable, so the field has to say so. */
  headers_unreadable: boolean;
  traces: boolean;
  metrics: boolean;
  /** Head sampling ratio, 0–1. */
  sample_ratio: number;
  temporality: "cumulative" | "delta";

  /** What the environment would give if this document were cleared. */
  env_endpoint: string;
  env_present: boolean;

  /** Deployment configuration — read-only here, because it opens a listener. */
  scrape_enabled: boolean;
  scrape_addr: string;
  scrape_token_set: boolean;

  health?: TelemetryHealth;
}

export interface TelemetryHealth {
  spans_exported: number;
  spans_dropped: number;
  export_failures: number;
  metric_exports: number;
  metric_failures: number;
  last_error?: string;
  last_error_at?: string;
  /** When the running configuration took effect — not process start. */
  since: string;
}

export interface UpdateTelemetrySettings {
  enabled: boolean;
  endpoint: string;
  traces: boolean;
  metrics: boolean;
  sample_ratio: number;
  temporality: "cumulative" | "delta";
  /** Omit to keep the stored credential; send "" to clear it. */
  headers?: string;
  /** Abandons the document and hands the install back to OTEL_*. */
  use_environment?: boolean;
}

export interface ActiveUserCounts {
  today: number;
  week: number;
  month: number;
  year: number;
}

/** One vocabulary for both credential classes. Computed by the server rather
 * than stored, so the UI never re-derives "is this expired" from a timestamp
 * and a clock it does not share. */
/** Shared vocabulary across both credential classes. `revoked` is the deploy
 *  key's alone, and is the only one that cannot be undone. */
export type KeyState = "active" | "disabled" | "expired" | "revoked";

/** A personal key *is* the person: it authenticates its owner, normal
 * authorization then applies, and it dies with them. */
/** Which routes a personal key may call. Chosen at creation and never edited —
 *  a scope that could be widened in place would be a label rather than a fence,
 *  so no PATCH carries it. `full` is what every key created before scopes
 *  existed has, and what a key is given when none is named.
 *
 *  It bounds Toft's own routes only. A read-scoped key still calls the
 *  reader's apps with any method, because Toft has no view into an app's
 *  routes and a bound claimed there would be a promise about somebody else's
 *  code. */
export type UserKeyScope = "read" | "full";

export interface UserApiKey {
  id: string;
  user_id: string;
  name: string;
  key_prefix: string;
  scope: UserKeyScope;
  state: KeyState;
  created_at: string;
  expires_at?: string;
  disabled_at?: string;
  last_used_at?: string;
  last_used_ip?: string;
  requests: number;
  /** Filled only by the install-wide admin listing, where the question is
   * whose key this is. */
  owner_login?: string;
}

/** One pending `toft login`, as the approval screen is allowed to see it: which
 * machine asked, when the request dies, and whether it is still open. The
 * device code is the terminal's secret and never appears here. */
export interface CliSignInRequest {
  user_code: string;
  hostname: string;
  client_version: string;
  /** Which program asked — `toft login`, or RStudio's Add account dialog
   * through rsconnect (ADR §102) — and the address it asked from. */
  client: SignInClient;
  requested_from: string;
  expires_at: string;
  status: "pending" | "approved" | "denied" | "expired";
}

export type SignInClient = "toft" | "rsconnect";

export type KeyUsageBucket = "day" | "week" | "month";

export interface KeyUsagePoint {
  bucket: string; // YYYY-MM-DD, the first day of the bucket
  requests: number;
  success: number;
  client_error: number;
  server_error: number;
}

/** What a key spent its calls on. An empty `app_id` is the platform API; an app
 * that has since been deleted keeps its row with an empty name. */
export interface KeyUsageTarget {
  app_id: string;
  app_name?: string;
  requests: number;
}

export interface KeyUsage {
  days: number;
  bucket: KeyUsageBucket;
  series: KeyUsagePoint[];
  totals: KeyUsagePoint;
  targets: KeyUsageTarget[];
}

/** One app's re-render schedule. At most one, because an app has one output. */
/** A credential an administrator holds and attaches to apps. There is
 *  deliberately no field for a value: no route returns one, to anybody. */
export interface Credential {
  id: string;
  name: string;
  description: string;
  /** The environment variable names it supplies. */
  keys: string[];
  app_count: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

/** One app holding one credential. The `app_id` is what a detach is addressed
 *  by — the reason this read exists rather than the count alone. */
export interface CredentialApp {
  app_id: string;
  app_name: string;
  app_slug: string;
  /** Who attached it, resolved to a name. Absent when that account is gone —
   *  the row is still the fact that it happened. */
  attached_by?: string;
  attached_at: string;
}

/** An app's `[restart]`, and what its last firing did. */
export interface RestartSchedule {
  id: string;
  app_id: string;
  name: string;
  /** The cron expression, a `CRON_TZ=` zone joined in. */
  schedule: string;
  enabled: boolean;
  next_run_at?: string;
  created_at: string;
  updated_at: string;
  /** Absent before the first firing. */
  last_fired_at?: string;
  last_outcome?: "restarted" | "skipped" | "failed";
  /** Why it was skipped (`sleeping`, `stopped`, `crashed`, `deploy_pending`,
   *  `locked`, `no_process`), or the failed start's sentence. */
  last_reason?: string;
}

export interface RenderSchedule {
  id: string;
  app_id: string;
  /** The platform's word, not a person's — there is one per app, so there is
   * nothing to tell apart. */
  name: string;
  /** A 5-field cron expression. */
  schedule: string;
  enabled: boolean;
  next_run_at?: string;
  created_at: string;
  updated_at: string;
  /** The people told when a render lands. Omitted when nobody is on the list,
   *  so a schedule with no report reads exactly as it did before. */
  recipients?: ScheduleRecipient[];
}

/** Somebody the report is sent to. The list is the owner's *intent*; what
 *  actually happened on a run is a `ReportDeliveryRecipient`. */
export interface ScheduleRecipient {
  user_id: string;
  login: string;
  name?: string;
  email?: string;
}

/** One run of a report's delivery, sent or not — a run suppressed by the
 *  install's floor still gets a row, because "did Monday's report go out" has
 *  to be answerable. */
export interface ReportDelivery {
  id: string;
  app_id: string;
  deployment_id: string;
  created_at: string;
  delivered: number;
  skipped: number;
  failed: number;
  recipients?: ReportDeliveryRecipient[];
}

export interface ReportDeliveryRecipient {
  user_id?: string;
  login: string;
  email?: string;
  status: "delivered" | "skipped" | "failed";
  /** Why, in a sentence. A skip with no reason is an absence, not a record. */
  reason?: string;
}

/** Per-action allow flags for the caller on one app — GET /apps/{id}/permissions.
 * UI decisions must come from this endpoint, never re-derived client-side. */
export interface AppPermissions {
  effective_role: string;
  can_read: boolean;
  can_edit: boolean;
  can_delete: boolean;
  can_manage_grants: boolean;
  can_change_visibility: boolean;
  can_transfer_ownership: boolean;
  can_approve_access_requests: boolean;
  can_publish: boolean;
  /** The caller may create a request for this app and it would not be refused
   *  on the spot — not merely "is not the owner". */
  can_request_access: boolean;
  /** The caller's own open request, or null. The object rather than a boolean,
   *  so the page resolves in one round trip and the gate does not flash
   *  between "you may ask" and "you already did". */
  pending_access_request: AccessRequest | null;
  /** Whether the sharing panel may offer to email somebody about a share — a
   *  capability, not a permission: it says a message would actually go out
   *  (email configured, on, and the app audience not silenced). A checkbox that
   *  promises mail an administrator has switched off is a control making a
   *  claim the copy is not allowed to make. */
  can_notify_on_share: boolean;
}

/** What a grant write did, and whether anybody was told about it.
 *
 * `notify_skipped` is absent when nobody asked to be told — which is a
 * different fact from asked-and-skipped, and the toast says something
 * different for each. */
export interface PutGrantResult {
  status: string;
  notified: boolean;
  notify_skipped?:
    "not_a_person" | "not_new" | "self" | "no_address" | "email_off";
}

/** Why a user can reach an app: ownership, membership of the group that *owns*
 *  it, a direct grant, or a grant to one of their groups.
 *
 *  `owner_via_group` is separate from `group` on purpose. One is a team being
 *  the owner; the other is somebody sharing an app with a team. An access
 *  review that could not tell them apart would under-report the first, which is
 *  a compliance defect rather than a cosmetic gap. */
export interface AccessSource {
  kind: "owner" | "owner_via_group" | "direct" | "group";
  role: string;
  group_id?: string;
  group_name?: string;
}

export interface AccessEntry {
  app: { id: string; slug: string; name: string; status: string };
  effective_role: string;
  sources: AccessSource[];
}

/** Admin-only access review — GET /users/{id}/access. Read-only; revocation
 * lives on each app's sharing panel. */
export interface UserAccessReport {
  user: { id: string; login: string; system_role: string; locked: boolean };
  explicit: AccessEntry[];
  visibility_access: { public_apps: number; authenticated_apps: number };
}

/** Minimal identity from the directory search — no email by design. */
export interface DirectoryUser {
  id: string;
  login: string;
  name?: string;
  avatar_url: string;
  system_role: string;
}

export interface DirectoryGroup {
  id: string;
  name: string;
  /** Members who could actually act as owner — neither locked nor a system
   *  viewer. Zero means the group is shown and unselectable: a transfer to it
   *  would be refused, and a write-bearing grant to it would confer nothing. */
  eligible_members: number;
}

/** Who owns an app. Exactly one of the two, always. */
export type OwnerPrincipalType = "user" | "group";

export interface TransferAppsResult {
  transferred: number;
  apps: string[];
}

export interface Group {
  id: string;
  name: string;
  /** Where failure mail for this group's apps goes. Empty means its members
   *  are told instead — a group that has not thought about it still gets told,
   *  which is the safe direction. Not a credential: it reads back in the clear. */
  email: string;
  created_at: string;
  /** Present on the list endpoint. */
  member_count?: number;
  /** How many apps this group owns. Non-zero means it cannot be deleted and
   *  cannot lose its last eligible member. */
  owns_apps?: number;
}

/** A deploy key as the admin page reads it. The secret exists once, in
 *  DeployKeyMint, and is never returned again. */
/** Which route list bounds a deploy key. Chosen at mint and never edited — a
 *  scope that could be widened in place would be a label rather than a fence,
 *  so there is no PATCH that carries it. */
export type DeployKeyScope = "read" | "deploy";

export interface DeployKey {
  id: string;
  name: string;
  key_prefix: string;
  scope: DeployKeyScope;
  created_by: string;
  created_by_login?: string;
  created_at: string;
  expires_at?: string;
  last_used_at?: string;
  disabled_at?: string;
  revoked_at?: string;
  state: KeyState;
  /** The account that minted it has been deleted. The key still works — that is
   *  the point of the class — and this is the prompt to rotate it. */
  minter_left: boolean;
  /** Deploys carrying this key may add storage toft.toml declares, on the app
   *  owner's delegation alone. Deploy scope only; an administrator sets it. */
  may_attach?: boolean;
}

/** A default's level: the two every kind of storage has. */
export type StorageDefaultLevel = "read" | "full";

/** One of the install's default delegations. */
export interface StorageDefault {
  id: string;
  subject_type: StoreSubjectType;
  subject_id: string;
  subject_name?: string;
  max_access: StorageDefaultLevel;
  created_at: string;
  /** Resources carrying a delegation a default wrote for this subject. */
  delegations: number;
  /** The apps those delegations attach storage to — what a revoke restarts. */
  apps: { id: string; slug: string; name: string }[];
}

export interface DeployKeyMint {
  key: DeployKey;
  secret: string;
}

/** A person, and the membership that put them in the group. The provenance is
 *  on the membership rather than on the user: the same person is a manual
 *  member of one group and a directory member of another. */
export interface GroupMember extends User {
  source: "manual" | "directory";
  /** Which provider wrote it. Absent on a manual membership. */
  source_provider_id?: string;
}

/** GET /groups/{id} — `members` is only present for admins and the group's
 * own members; email is blanked for non-admins server-side. */
export interface GroupDetail {
  group: Group;
  members?: GroupMember[];
}

/** One claim value attached to a group, for one sign-in provider. A group with
 *  none is an ordinary group whose membership an administrator writes. */
export interface GroupClaimMapping {
  group_id: string;
  provider_id: string;
  claim_value: string;
  created_at: string;
  created_by?: string;
}

export type AppVisibility = "public" | "authenticated" | "private";

/** One sharing-panel row: a grant plus the principal's display identity. */
export interface Grant {
  object_id: string;
  principal_type: "user" | "group" | "service_account";
  principal_id: string;
  role: "owner" | "editor" | "viewer" | "list";
  granted_by: string;
  granted_at: string;
  display_name: string;
  avatar_url?: string;
}

export interface Deployment {
  id: string;
  app_id: string;
  commit_sha: string;
  branch: string;
  status: "pending" | "building" | "success" | "failed";
  triggered_by: "manual" | "auto" | "rollback" | "render" | "move";
  /**
   * What this build's release costs: the unpacked tree on the host, and the
   * packed artifact in the object store. Null means unmeasured — a build older
   * than size accounting, or one whose release is gone from both places — and
   * is deliberately not zero.
   *
   * A rollback owns no release of its own (it serves the build it rolled back
   * to), so both are null on those rows and the same bytes are never counted
   * twice.
   */
  disk_bytes: number | null;
  artifact_bytes: number | null;
  /**
   * Which phase the build is in, or the one it ended at, and when that phase
   * began. Absent on rows older than the columns — which the timeline treats
   * as "no timeline recorded" rather than "starts at setup", because drawing
   * nine phases for a deployment that never reported one would be fabricating
   * them.
   */
  current_step?: string;
  step_started_at?: string;
  /** What each finished phase cost: JSON `[{"step","ms"}]`, in order. */
  step_timings?: string;
  /**
   * The gallery version this build is *trying* to reach, present only on a
   * deployment that has something to do with the catalog — an extension's
   * update, or a redeploy of one that followed the catalog. The app's recorded
   * version moves when the build succeeds, never at this point.
   */
  gallery_version?: string;
  started_at: string;
  finished_at: string;
  /**
   * Which build is running, decided by the server. `current` is the row that
   * records the running deploy; `serving` is the row that owns the release
   * actually executing. They are the same row except after a rollback, which
   * keeps serving the release it rolled back to.
   *
   * Only present on the deployments listing — a deployment returned from a
   * deploy or rollback has not run yet.
   */
  current?: boolean;
  serving?: boolean;
  /** Why this build cannot be deleted, or "" if it can. */
  protected_reason?: string;
  /**
   * What the platform took out of the uploaded archive before installing it
   * (ADR §60), largest first — dependency trees, VCS metadata, OS litter.
   *
   * Present only on the response to an upload, and only when something was
   * removed: the platform changed what somebody shipped, so it says so on the
   * spot rather than only in the build log.
   */
  removed?: RemovedEntry[];
}

/**
 * One frame of a build's progress, from the SSE stream.
 *
 * **Every frame is a snapshot**, so a watcher that connects late or misses one
 * is correct again on the next; the terminal frame — `success` or `failed` — is
 * the end signal, and the server closes the stream after it. `phase` is derived
 * from `step` server-side and sent anyway, so no client carries the mapping.
 *
 * The deploy console is the SPA's only consumer, and its only per-deployment
 * question (ADR §87). The deployment list stays the list's source.
 */
export interface DeployProgress {
  deployment_id: string;
  status: "pending" | "building" | "success" | "failed";
  phase: string;
  step: string;
  at: string;
  /** Why, on a terminal frame that failed. Empty otherwise. */
  message?: string;
}

/** What a release deletion actually reclaimed. */
export interface ReleaseReclaim {
  deleted: number;
  freed_disk_bytes: number;
  freed_artifact_bytes: number;
}

export interface CreateAppRequest {
  repo_url: string;
  branch?: string;
  /** A folder of the repository to build, for one that holds several apps. */
  repo_subdir?: string;
  name?: string;
  slug?: string;
  env_vars?: Record<string, string>;
  /** Marks which env_vars keys are write-only secrets; unlisted keys are visible configuration. */
  env_sensitive?: Record<string, boolean>;
  /** Clone credential for private repos (HTTPS token). Stored encrypted, never returned. */
  git_token?: string;
  /**
   * Deploy through a connected git host instead, so the app carries no
   * credential of its own. Refused for an API key, and refused with 403 when
   * the connection does not cover the repository.
   */
  git_connection_id?: string;
  /**
   * Where this app runs, from its very first build — not a promotion
   * afterwards. Optional: absent means the install's default.
   *
   * Administrator-only, and refused with 403 for anyone else *even when the
   * value they sent is the one they would have been given*, so send it only
   * when somebody actually chose. Refused with 422 when the install does not
   * offer the mode.
   */
  execution_mode?: ExecutionMode;
}

export interface UpdateAppRequest {
  name?: string;
  slug?: string;
  branch?: string;
  /** Another folder of the repository; "" is the root. */
  repo_subdir?: string;
  auto_deploy?: boolean;
  /**
   * Admin-only: point the app at a different repository. The target must pass
   * the install's source policy. The release being served keeps serving until
   * the next deploy succeeds, and the stored git token is left alone — it was
   * issued for the old host, so the response says to replace it.
   */
  repo_url?: string;
  /** Gated by can_change_visibility server-side. */
  visibility?: AppVisibility;
  /** Whether the app shows up in the list of somebody who holds nothing on it.
   *  Gated by can_change_visibility too — it is the weaker half of the same
   *  question, not a separate permission. Re-sending the current value is
   *  accepted and records nothing. */
  listable?: boolean;
  /** Owner-only. Empty string clears the stored credential. */
  git_token?: string;
  /**
   * Point the app at a connected git host, or at none with `""`. Refused for an
   * API key, and refused with 403 when the connection does not cover the
   * repository — including when only `repo_url` is moving.
   */
  git_connection_id?: string;
  /**
   * Admin-only. Rejected with 422 and an explanation when the platform does
   * not offer isolated execution — show that message rather than a generic
   * failure, because it tells the reader what would change it.
   */
  execution_mode?: ExecutionMode;
  /** Retention. The requested figure is stored unclamped — the platform cap is
   * applied at read time, so raising it restores the owner's intent. */
  auto_delete_releases?: boolean;
  releases_kept?: number;
  /** Admin-only: exempt this app from idle sleep. */
  never_sleep?: boolean;
  /** Admin-only: a hostname of the app's own ("" clears it), and whether it
   * is served. Either change that moves the app rebuilds it at the new address. */
  hostname?: string;
  hostname_enabled?: boolean;
  /** Admin-only: this app's own memory ceiling. Send one of the two — both is
   * refused, because they are the same number in two units. 0 hands the number
   * back to the install's dial rather than uncapping the workload. */
  memory_limit_mb?: number;
  memory_limit_gb?: number;
  /** Admin-only: this app's own CPU limit in millicores; 0 is the install's. */
  cpu_limit_millicores?: number;
  /** Admin-only: let this app learn who is looking at it. Naming a person to a
   * program is a grant rather than a preference, and the person named is
   * neither the app nor its owner — which is why the owner cannot set it. */
  viewer_identity?: boolean;
  /** Silence failure notifications for this app. The events record is kept
   * regardless — this governs telling, not recording. */
  notify_failures?: boolean;
  /** Replace the whole list of origins allowed to embed this app. Sending `[]`
   * clears it; omitting the field is a different request and changes nothing. */
  frame_ancestors?: string[];
}

/**
 * The platform's capability switches, plus the read-only facts about what the
 * install is *able* to do. Both halves are needed: the UI has to explain why a
 * switch is unavailable, and "no cluster configured" and "an admin turned it
 * off" are different problems with different fixes.
 */
// --- Sign-in providers ---

/**
 * `password` is local sign-in against the built-in administrator's password. It
 * is a provider like any other to every list in this file, and the one that is
 * not an identity provider: no client id, no secret, no redirect URI, and a form
 * on the login page instead of a button that leaves.
 */
export type AuthProviderKind = "oidc" | "github" | "password";

/** What the login page renders. Public — it has to work before anyone is signed in. */
export interface LoginOptions {
  /**
   * Every way in. A provider with `kind: "password"` means render the username
   * and password form; there is no separate flag for it, so the page cannot
   * disagree with the provider row that actually decides.
   */
  providers: Array<{
    id: string;
    kind: AuthProviderKind;
    display_name: string;
  }>;
  /**
   * Set when there is exactly one way in, and the page should go straight
   * there. The server decides this so the rule has one home — a second
   * provider appearing stops the redirect with no frontend change.
   */
  auto_select?: string;
  /**
   * What to tell someone who has been turned away, in the administrator's
   * words. Served to anonymous callers because that is exactly who reads
   * them — `?denied=locked` and `?denied=no_account` select which one.
   */
  locked_message: string;
  no_account_message: string;
  /** The third: the account exists and is waiting to be activated. */
  pending_activation_message: string;
}

export interface AuthProvider {
  id: string;
  kind: AuthProviderKind;
  kind_label: string;
  display_name: string;
  enabled: boolean;
  client_id: string;
  /** Secrets go in and never come out; this is all the API will say about one. */
  secret_set: boolean;
  /** …and whether that stored value can still be decrypted. `set` with
   * `unreadable` is a changed or lost ENCRYPTION_KEY: the form's "leave blank
   * to keep" would keep something unusable, so the field has to say so. */
  secret_unreadable: boolean;

  issuer?: string;
  auth_url?: string;
  token_url?: string;
  userinfo_url?: string;
  scopes?: string;

  allowed_domains: string;
  default_role: string;
  link_by_verified_email: boolean;
  /** Off means the provider only authenticates people who already have an account. */
  auto_provision: boolean;
  /** The claim carrying directory group membership. Empty means `groups`. */
  groups_claim: string;
  /** Claim values of which a person must hold one to sign in. Empty means every
   *  group is permitted. A non-empty list refuses a sign-in carrying no group
   *  claim at all, which is what makes it a gate rather than a preference. */
  permitted_login_groups: string;
  /** Individual addresses permitted alongside the domains, in union with them. */
  allowed_emails: string;
  /** The delimiter a group claim delivered as one string is split on. Empty
   *  means no split. */
  groups_separator: string;
  /** When a SCIM token was minted for this provider. Absent means no feed —
   *  the token itself is never returned. */
  scim_token_set_at?: string;
  /** When the directory last called the SCIM endpoint. Absent with a token set
   *  is the shape of every SCIM misconfiguration. */
  scim_last_call_at?: string;

  /** False for an environment-configured provider: the operator owns it, so the UI shows it read-only. */
  managed: boolean;
  /** Register this at the provider, exactly as given. */
  redirect_uri: string;
  user_count: number;
  /** Whether it would appear on the login page as things stand. */
  login_ready: boolean;
}

/** A known vendor's shape — a form filler, never required. */
export interface AuthProviderPreset {
  id: string;
  name: string;
  kind: AuthProviderKind;
  category: "enterprise" | "social";
  issuer?: string;
  issuer_template?: string;
  scopes?: string[];
  /** Authenticates accounts the organization does not control, so a domain restriction matters. */
  multi_tenant?: boolean;
  hint?: string;
  docs_url?: string;
}

/**
 * An issuer an administrator trusts to vouch for a person: its identity token,
 * carrying `audience`, is exchanged for a short-lived deploy key (`toftx_`)
 * for the account it names — by `sub` on `provider_id`, or by verified email.
 */
export interface TrustedIssuer {
  id: string;
  display_name: string;
  issuer: string;
  audience: string;
  subject_claim: "sub" | "email";
  provider_id: string;
  key_lifetime_minutes: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
  active_keys: number;
}

export type TrustedIssuerInput = Pick<
  TrustedIssuer,
  "display_name" | "issuer" | "audience" | "subject_claim" | "provider_id" | "key_lifetime_minutes"
>;

/** A live short-lived key an exchange created. */
export interface ExchangedKey {
  id: string;
  user_id: string;
  user_login: string;
  issuer_id: string;
  issuer_name: string;
  key_prefix: string;
  token_subject: string;
  created_at: string;
  expires_at: string;
  last_used_at?: string;
  last_used_ip?: string;
}

export interface AuthProviderList {
  providers: AuthProvider[];
  presets: AuthProviderPreset[];
}

/**
 * A connected git host, as Admin → Sources sees it.
 *
 * Every secret is write-only: the API takes one, encrypts it, and thereafter
 * reports only whether one is stored. `reach` is the sentence the server
 * derives from the credential's own prefix, so the page never asks anybody what
 * kind of token they pasted.
 */
export interface GitConnection {
  id: string;
  kind: "github" | "gitlab";
  display_name: string;
  base_url: string;
  auth_method: "app" | "token";
  enabled: boolean;
  app_id?: string;
  installation_id?: string;
  client_id?: string;
  clone_secret_set: boolean;
  client_secret_set: boolean;
  webhook_secret_set: boolean;
  clone_secret_unreadable: boolean;
  reach: string;
  credential_kind?: string;
  credential_owner?: string;
  credential_expires_at?: string;
  coverage: string[];
  /** Whether a developer choosing this connection gets a repository list or a URL field. */
  picker: boolean;
  last_error?: string;
  last_error_at?: string;
  last_ok_at?: string;
  app_count: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
  webhook_url: string;
}

export interface GitConnectionInput {
  kind: "github" | "gitlab";
  display_name: string;
  base_url?: string;
  auth_method: "app" | "token";
  enabled?: boolean;
  app_id?: string;
  installation_id?: string;
  client_id?: string;
  client_secret?: string;
  clone_secret?: string;
  credential_owner?: string;
  credential_expires_at?: string | null;
  coverage?: string[];
}

/**
 * A connection as a *developer* sees it.
 *
 * There is no auth method here and there never will be: whether the
 * organization's connection is backed by a GitHub App or by a pasted token is
 * an administrator's fact about a credential. `picker` is the only part of that
 * difference a developer sees.
 */
/** A connection with the webhook secret just minted for it — create and rotate only. */
export interface GitConnectionWithSecret extends GitConnection {
  webhook_secret: string;
}

export interface DeveloperGitConnection {
  id: string;
  kind: "github" | "gitlab";
  display_name: string;
  picker: boolean;
  coverage: string[];
}

/**
 * One entry of the picker's list.
 *
 * `allowed` and `covered` are markings, never enforcement: a refused repository
 * renders disabled with the reason rather than being hidden, because a
 * developer who can see why does not open a ticket — and because a repository
 * the picker never lists can still be typed into the URL field beside it, which
 * is why the picker cannot be an enforcement point.
 */
export interface PickerRepository {
  full_name: string;
  clone_url: string;
  private: boolean;
  archived: boolean;
  default_branch: string;
  description: string;
  allowed: boolean;
  refusal?: string;
  covered: boolean;
}

/**
 * What one connection test learned.
 *
 * `ok` is whether the host accepted this install's credential; `summary` is the
 * sentence shown either way, and on a failure it is also what the server wrote
 * to the connection's health, so the row and the toast cannot disagree.
 * `repositories` is absent unless the host counts them — only the App method
 * has such a count, and an absent one is not a count of none.
 */
export interface GitConnectionTestResult {
  ok: boolean;
  summary: string;
  identity?: string;
  repositories?: number;
  disabled: boolean;
}

/** One app that deploys through a connection — what a removal is refused on. */
export interface GitConnectionApp {
  id: string;
  slug: string;
  name: string;
}

export interface AuthProviderInput {
  id: string;
  kind: AuthProviderKind;
  display_name: string;
  enabled?: boolean;
  client_id: string;
  client_secret?: string;
  issuer?: string;
  auth_url?: string;
  token_url?: string;
  userinfo_url?: string;
  scopes?: string;
  allowed_domains?: string;
  default_role?: string;
  link_by_verified_email?: boolean;
  auto_provision?: boolean;
  groups_claim?: string;
  permitted_login_groups?: string;
  allowed_emails?: string;
  groups_separator?: string;
}

export interface DiscoveryDoc {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  userinfo_endpoint: string;
  scopes_supported?: string[];
  end_session_endpoint?: string;
  jwks_uri?: string;
}

/** The shared-mode agent's tier (ADR §128): `process`, `cgroup` or `namespace`. */
export interface SharedContainment {
  tier: "process" | "cgroup" | "namespace" | string;
  reason?: string;
  bridge?: string;
}

/** Whether the kernel holds a shared workload's memory limit at this tier. */
export function kernelHoldsMemory(c: SharedContainment | undefined): boolean {
  return c?.tier === "cgroup" || c?.tier === "namespace";
}

export interface PlatformSettings {
  /** When the document last changed. Sent back with a write; a write from a
   *  page that read an older document is refused with 409. */
  updated_at: string;
  isolated_execution_enabled: boolean;
  /**
   * What a *new* app is born as when whoever creates it names no mode.
   *
   * A seed, never an override: it is read once, at insert, and never again, so
   * changing it moves no existing app in either direction. Reads back `shared`
   * on an install that has never set it.
   */
  default_execution_mode: ExecutionMode;
  background_jobs_enabled: boolean;
  job_concurrency_per_app: number;
  app_memory_limit_mb: number;
  app_memory_limit_gb: number;
  /** The rest of the operating dials. 0, or empty, means the install's own
   * figure — never "off" and never "unlimited". */
  app_cpu_request_millicores: number;
  app_cpu_request_cores: number;
  app_cpu_limit_millicores: number;
  app_cpu_limit_cores: number;
  max_concurrent_builds: number;
  auto_deploy_poll_seconds: number;
  r_package_repository: string;
  log_level: string;

  /** Shown to a locked user, and to someone with no account, respectively. Never empty as read. */
  locked_account_message: string;
  /** What a visitor to a *locked app* is told, when the lock on that app
   * carries no message of its own. The third of the refusal-message family and
   * the first that refuses somebody at an app rather than at the door. */
  app_locked_message: string;
  no_account_message: string;
  /** The fourth refusal: the account exists and no administrator has activated
   * it. Never empty as read, and its default promises no email. */
  pending_activation_message: string;
  /** The one message in the family somebody is glad to read — the sentence the
   * activation notice carries. Never empty as read. */
  account_activated_message: string;

  /** Which environment this install is, as telemetry reports it; empty names none. */
  environment_tag: string;

  /** What emails and notifications call this install; empty means “Toft”. */
  install_name: string;

  /** The zone times are shown in for anyone who has not chosen one; empty is
   *  each viewer’s own browser (ADR §154). */
  display_time_zone: string;

  /**
   * How long a shared-mode app may go untouched before the platform stops it
   * and marks it sleeping. 0 means idle sleep is off, which is the default.
   */
  idle_sleep_minutes: number;
  /** The same for isolated apps — a setting of its own, 0 (off) by default. */
  isolated_idle_sleep_minutes: number;
  /** Whether isolated apps sleep only inside a daily window, woken one at a
   *  time when it closes. The bounds are "HH:MM" in the display zone (UTC
   *  when the install has none). */
  isolated_sleep_window_enabled: boolean;
  isolated_sleep_window_start: string;
  isolated_sleep_window_end: string;

  topology: "vm" | "ecs" | "kubernetes";
  isolated_possible: boolean;
  /** Whether job runs can be placed on this machine, so jobs need no cluster. */
  host_jobs_possible: boolean;
  /** Where runs go now: "cluster", "this machine", or "" when jobs cannot run.
   *  Read-only, like `topology`. */
  job_placement: string;
  /** How the shared-mode agent holds this machine's workloads: its tier, why
   *  the next one up was refused, and the bridge range at the namespace tier.
   *  Absent where nothing answers for it. */
  shared_containment?: SharedContainment;
  /** Empty when isolated execution can be enabled. */
  isolated_blocked_reason?: string;
  env_app_memory_limit_mb: number;
  env_app_memory_limit_gb: number;
  env_app_cpu_request_millicores: number;
  env_app_cpu_request_cores: number;
  env_app_cpu_limit_millicores: number;
  env_app_cpu_limit_cores: number;
  env_max_concurrent_builds: number;
  env_auto_deploy_poll_seconds: number;
  env_r_package_repository: string;
  env_log_level: string;
  /** The pool this platform's build workers live in. The dial cannot exceed it,
   * and a field that cannot pass a number has to name it. */
  max_concurrent_builds_ceiling: number;
  /** False on an install started with AUTO_DEPLOY_POLL_INTERVAL=0, where no
   * poller exists and storing a number starts none. */
  auto_deploy_poller_running: boolean;
  /** The half of the logging pair that stays in the environment, and what this
   * process is saying now. */
  log_format: string;
  effective_log_level: string;
  /** Whether npm lifecycle scripts run during a build. Environment-only, like
   * the log format, and for the sharper reason: it is not a quantity but what
   * code a build may execute. */
  build_allow_scripts: boolean;
  isolated_app_count: number;
  runner_version: string;
  runner_images: string[] | null;

  /** What is protecting this install's secrets at rest — `local` (the key is in
   * this install's environment) or `managed` (the key that encrypts the data key
   * never leaves a KMS). Derived from configuration and read-only, so an
   * operator cannot mislabel their own install. */
  encryption_backend: "local" | "managed";
  /** The key-encrypting key, when there is one. Carries no credential, by
   * construction. */
  encryption_kek?: string;
  /** The key new records are written under — how a rotation is confirmed to
   * have taken. */
  encryption_key_id: string;

  /** Which OS identity a shared-mode app's processes run as. `per-app` is the
   * shipped posture and the only one that keeps one app out of another's
   * files; `shared` separates apps from the platform but not from each other;
   * `platform` is no separation at all. Derived from configuration and
   * read-only. */
  app_accounts: "per-app" | "shared" | "platform";

  /** Whether `BLOCK_AWS_METADATA` is set on this process. Read-only and derived
   * from configuration. It is not a verdict: the isolated half is a
   * NetworkPolicy nothing verifies, so `true` here means the rule was written,
   * not that a CNI is enforcing it. */
  metadata_blocked: boolean;

  /** Machine credentials. Off means absent and off means *now*: it stops keys
   * that already exist, not merely new ones. */
  api_keys_enabled: boolean;
  /** The longest a new personal key may last, in days; 0 is no cap. Keys
   * created by `toft login` and RStudio then never expire. */
  api_key_max_lifetime_days: number;

  /** Whether an app here may be opened without signing in. Off withdraws
   * anonymity from every app that already has it and refuses the change to
   * Public; it edits no app, so turning it back on republishes them. */
  public_apps_enabled: boolean;
  /** Whether the gateway counts distinct anonymous browsers with a 13-month
   *  first-party cookie, into sketches that name nobody. Off by default; off
   *  stores nothing and expires a cookie that comes back. */
  audience_measurement_enabled: boolean;
  /** Whether the apps gateway fills nosniff and Referrer-Policy where an app
   *  sent neither. On by default; HSTS is not behind it. */
  app_security_headers: boolean;
  /** Whether a shared app's output reaches the platform's log with no per-app
   *  cap. Off by default. */
  app_log_copy_uncapped: boolean;
  /** Whether every signed-in person sees every app in their list without being
   *  able to open any of it — the `list` rung granted install-wide. */
  apps_listable_to_authenticated: boolean;

  /** How many apps their owners have chosen to publish — the blast radius of
   * turning the switch above off, and the owners' column rather than what is in
   * effect, so it stays truthful once it is already off. */
  public_app_count: number;

  /**
   * Dormancy locking. While `dormant_lock_enabled` is off nothing is swept and
   * the threshold means nothing, so the surface says so rather than showing a
   * figure for a sweep that is not running.
   *
   * Administrators are never locked by the sweep whatever these say — it is not
   * a setting, and no request changes it.
   */
  dormant_lock_enabled: boolean;
  dormant_lock_days: number;

  /**
   * The session policy. Two numbers because every standard names them apart,
   * and no switch above them: a session always has a lifetime, and
   * `session_idle_minutes: 0` is what turns the idle half off.
   *
   * Both hold sessions that are already open, not only ones minted afterwards.
   */
  session_absolute_hours: number;
  session_idle_minutes: number;

  /** The cap on releases kept per app, 0 for none, and how many apps currently
   * ask for more than it — the conflict is shown rather than enforced. */
  max_versions_per_app: number;
  apps_over_version_cap: number;

  /**
   * The job limits. A job's own `timeout` in toft.toml is held to the
   * ceiling; a job that declares none gets the default. Its memory and CPU
   * are the install's job defaults unless an administrator overrides them on
   * the job. 0 in a default means the environment's figure.
   */
  default_job_timeout_seconds: number;
  max_job_timeout_seconds: number;
  job_memory_limit_mb: number;
  job_memory_limit_gb: number;
  job_cpu_limit_millicores: number;
  job_cpu_limit_cores: number;
  env_job_memory_limit_mb: number;
  env_job_cpu_limit_millicores: number;
  env_job_cpu_limit_cores: number;
  /** How many runs one app may have waiting before a start is refused with
   *  `queue_full`. */
  max_queued_runs_per_app: number;
  /** How long a notebook's cells may run during a deploy. The one build step
   * whose length is a property of the app's own work rather than of package
   * downloads, which is why it is a setting and the other ten minutes is a
   * constant. */
  notebook_render_timeout_seconds: number;
  report_delivery_min_interval_minutes: number;
  /** How long any one build command may run. Was a fixed ten minutes chosen
   *  for `npm ci`; an app may be given more without moving this figure. */
  build_timeout_seconds: number;

  /** How long a background job's logs and run rows are kept. Rows must outlive
   * logs: a run's output object dies with its row, so the other order would
   * leave a succeeded run with nothing behind it. */
  job_log_retention_days: number;
  job_run_retention_days: number;

  /** How long app failure events are kept — the system half of Admin →
   * Activity. The audit half of that page is not pruned at all, so this is a
   * dial over one of the two tables behind it. */
  app_event_retention_days: number;
  platform_log_retention_days: number;

  /** How long the per-person list under a sent announcement is kept. It prunes
   * who was mailed, never the announcement or its counts — those are the
   * install's own record and are never deleted. */
  announcement_recipient_retention_days: number;
  /** 0 keeps everything; otherwise 365–3650. */
  access_record_retention_days: number;

  /** Which third-party packages this install will run (ADR §39). `warn` is the
   *  mode `block` is unadoptable without: it records and shows everything and
   *  fails no build. Its controls live on Admin → Dependencies, beside the
   *  estate they act on and the credential they need. */
  dependency_policy: DependencyMode;
  dependency_scan_on_deploy: boolean;
  dependency_watch_enabled: boolean;
  dependency_block_malware: boolean;
  /** One of the four levels, or "off" to stop blocking on severity at all —
   *  which is not the same as an advisory's own empty severity. */
  dependency_block_severity: string;
  dependency_block_only_fixable: boolean;
  dependency_feed_endpoint: string;
  dependency_feed_interval_hours: number;
  dependency_feed_max_age_hours: number;
  /** 0 turns waivers off entirely, which changes what a refused deploy tells
   *  the developer: with no waiver to ask for, the threshold is the only thing
   *  that can move. */
  dependency_waiver_max_days: number;
  /** The watch's own dials: how often it looks, how many readings each app
   *  keeps, and the one threshold at which looking has a consequence. */
  dependency_scan_interval_hours: number;
  dependency_scan_history_kept: number;
  dependency_lock_on: DependencyLockOn;

  /**
   * Where links in email point. The one address that is configured rather than
   * worked out, because no request exists when a notification is written —
   * nothing inbound may consult it.
   *
   * `effective_base_url` is what is actually used, `env_base_url` what BASE_URL
   * set, and `observed_base_url` what this request arrived as — shown so an
   * administrator can see the three agree before saving.
   */
  public_base_url: string;
  /** Garth's address; empty means no app page offers Open in Garth (ADR §158). */
  garth_url: string;
  effective_base_url: string;
  env_base_url: string;
  observed_base_url?: string;
  /**
   * Where deployed apps are served — APPS_BASE_URL, or derived from BASE_URL
   * and APPS_PORT. Read-only, and not moved by `public_base_url`: it is the
   * origin the SDKs' `url()` builds an app's own links from.
   */
  apps_base_url: string;

  /**
   * The four security.txt fields (RFC 9116). All empty on an install that has
   * never filled them in, which is a complete state and not a missing one:
   * `/.well-known/security.txt` 404s on both origins until there is a contact,
   * because a file with a blank contact is a promise nobody is keeping.
   */
  security_txt_contact: string;
  /** `YYYY-MM-DD`, required whenever there is a contact. Served as written
   *  even once it has passed — the platform will not quietly renew it. */
  security_txt_expires: string;
  security_txt_policy: string;
  security_txt_languages: string;

  /**
   * What the gallery offers. `off` is the default and means absent: the routes
   * 404, the tab disappears, and — the part that matters on an on-premise
   * install — nothing is fetched from the catalog's host at all.
   *
   * The rest of this group is read-only. Where the catalog lives is part of how
   * the install was built, not a decision an administrator revisits; sending
   * `gallery_catalog_url` in an update is refused by name.
   */
  gallery_deploy_mode: GalleryMode;
  gallery_available: boolean;
  gallery_catalog_url?: string;
  gallery_catalog_source?: "built-in" | "environment";
  gallery_sync?: GallerySync;
}

/** What the signed-in principal may do without reference to a specific app. */
export interface Capabilities {
  system_role: string;
  principal_type: string;
  can_create_objects: boolean;
  can_request_publisher_role: boolean;
  can_create_service_account: boolean;
  /**
   * What an app *this caller* creates is born as when they name no mode.
   *
   * Not the install's setting — that is on `/system/settings` and
   * administrator-only. This is the answer to "what am I about to make", which
   * anybody who may create an app is entitled to, and it is derived through
   * the same rule the create path applies: an install whose default says
   * isolated but whose capability has gone away reads `shared` here.
   */
  new_app_execution_mode: ExecutionMode;
  /** What this install will do for anybody, as the refusing code paths read it. */
  install: InstallCapabilities;
  /** Whether anonymous browsers are counted, so a Usage tab knows to draw them. */
  audience_measurement: boolean;
}

/** The install half of `GET /me/capabilities` (`installCapabilities`). */
export interface InstallCapabilities {
  version: string;
  uploads_allowed: boolean;
  public_apps_allowed: boolean;
  jobs_available: boolean;
  /** Whether the storage routes exist here — the same test as their 404. */
  storage_available: boolean;
  sdk_languages: string[];
  runtimes: Record<string, string[]>;
}

export type AccessRequestStatus =
  "pending" | "approved" | "denied" | "withdrawn";

/** One person's request for a grant on one app — the object half of
 *  access_requests, the twin of PromotionRequest. */
export interface AccessRequest {
  id: string;
  requester_id: string;
  request_type: string;
  object_id: string;
  requested_role: string;
  status: AccessRequestStatus;
  /** Who decided, not who was asked. Empty while pending. */
  approver_id: string;
  approver_group_id?: string;
  decided_at?: string | null;
  /** The requester's own words on the ask, replaced by the decider's on a
   *  denial. Empty when neither wrote one. */
  reason: string;
  created_at: string;
  /** Empty when the app has since been deleted: the ask happened, and the
   *  thing it was about is gone. */
  app_slug: string;
  app_name: string;
  /** The requester, as their colleagues know them. `requester_name` is what
   *  their provider asserted and may be empty; fall back to the login. */
  requester_login: string;
  requester_name: string;
  /** Empty while pending — nobody has decided it. */
  approver_login: string;
}

export type PromotionRequestStatus =
  "pending" | "approved" | "denied" | "withdrawn";

export interface PromotionRequest {
  id: string;
  requester_id: string;
  request_type: string;
  requested_role: string;
  status: PromotionRequestStatus;
  approver_id: string;
  decided_at?: string | null;
  /** The administrator's own words on a denial. Empty otherwise. */
  reason: string;
  created_at: string;
  requester_login: string;
  requester_name: string;
  requester_email: string;
  /** Empty while pending — nobody has decided it. */
  approver_login: string;
}

export type EmailTransport = "smtp" | "api";

export interface EmailSettings {
  enabled: boolean;
  /** Two transports, one Message. `api` is an HTTPS POST to one provider, for
   * the install where outbound 587 is closed and 443 is not. */
  transport: EmailTransport;
  host: string;
  port: number;
  security: "starttls" | "implicit_tls" | "none";
  username: string;
  /** Which sending API, when `transport` is "api". */
  api_provider: string;
  from_address: string;
  from_name: string;
  reply_to: string;
  skip_verify: boolean;
  /** Whether a password is stored. The password itself never comes back. */
  password_set: boolean;
  /** …and whether that stored value can still be decrypted. `set` with
   * `unreadable` is a changed or lost ENCRYPTION_KEY: the form's "leave blank
   * to keep" would keep something unusable, so the field has to say so. */
  password_unreadable: boolean;
  /** The same, for the sending-API key. Separate slots: a transport switch must
   * never offer an SMTP password as a bearer token. */
  api_key_set: boolean;
  /** The same fact for the API key. See `password_unreadable`. */
  api_key_unreadable: boolean;
  /** The two audience switches, independent and both defaulting on. Copy may
   * not promise mail — either may be off. */
  notify_admin: boolean;
  notify_app: boolean;
  /** Sends admin mail to one distribution list instead of to each
   * administrator, so role membership stops doubling as a mailing list. Not a
   * credential: read back in the clear, cleared with "". */
  admin_group_address: string;
  /** Where the test button would send — the caller's own address, empty if they have none. */
  test_recipient: string;
}

export interface UpdateEmailSettings {
  enabled?: boolean;
  transport?: EmailTransport;
  host?: string;
  port?: number;
  security?: EmailSettings["security"];
  username?: string;
  /** Omit or send empty to keep the stored password. */
  password?: string;
  api_provider?: string;
  /** Write-only, and its own slot rather than a second use of `password`. */
  api_key?: string;
  from_address?: string;
  from_name?: string;
  reply_to?: string;
  skip_verify?: boolean;
  notify_admin?: boolean;
  notify_app?: boolean;
  /** "" clears it and restores per-administrator addressing. */
  admin_group_address?: string;
}

export interface UpdatePlatformSettings {
  /** The `updated_at` the page read, so a stale page cannot overwrite what
   *  somebody else saved since. */
  updated_at?: string;
  /** Dependency policy (ADR §39). Every field optional, and the server refuses
   *  the two combinations that would read as a control and not be one: `block`
   *  with the deploy scan off, and `block` with BUILD_ALLOW_SCRIPTS set. */
  dependency_policy?: DependencyMode;
  dependency_scan_on_deploy?: boolean;
  dependency_watch_enabled?: boolean;
  dependency_block_malware?: boolean;
  dependency_block_severity?: string;
  dependency_block_only_fixable?: boolean;
  dependency_feed_endpoint?: string;
  dependency_feed_interval_hours?: number;
  dependency_feed_max_age_hours?: number;
  dependency_waiver_max_days?: number;
  dependency_scan_interval_hours?: number;
  dependency_scan_history_kept?: number;
  /** The one setting here that can take a running app off the air. The server
   *  refuses it while the watch is off — locking happens on a scheduled pass,
   *  and with no passes it would read as a control and never fire. */
  dependency_lock_on?: DependencyLockOn;

  isolated_execution_enabled?: boolean;
  /** What a new app is born as. Refused with 422 if this install cannot offer
   *  the mode — including in the same request that turns it on, which is
   *  allowed, because the check reads the settings as this request leaves them. */
  default_execution_mode?: ExecutionMode;
  background_jobs_enabled?: boolean;
  job_concurrency_per_app?: number;
  app_memory_limit_mb?: number;
  app_memory_limit_gb?: number;
  app_cpu_request_millicores?: number;
  app_cpu_request_cores?: number;
  app_cpu_limit_millicores?: number;
  app_cpu_limit_cores?: number;
  max_concurrent_builds?: number;
  auto_deploy_poll_seconds?: number;
  r_package_repository?: string;
  log_level?: string;
  /** Send empty to restore the default wording; the server never stores a blank message. */
  locked_account_message?: string;
  no_account_message?: string;
  /** The default shown to a visitor of a locked app, when the lock on that app
   * carries no wording of its own. Same rule: empty restores the default. */
  app_locked_message?: string;
  /** What an account awaiting activation is told at the door, and what the
   * activation notice says once somebody lets it in. Same rule. */
  pending_activation_message?: string;
  account_activated_message?: string;
  /** Which environment this install is, as telemetry reports it. Send empty to name none. */
  environment_tag?: string;
  /** Send empty to go back to “Toft” as the sender’s name. */
  install_name?: string;
  /** "UTC" or an IANA name; empty shows each viewer their browser’s zone. */
  display_time_zone?: string;
  /** 0 turns idle sleep off; otherwise 5 to 10080 minutes. */
  idle_sleep_minutes?: number;
  /** The isolated timeout: 0 keeps isolated apps awake; otherwise 5 to 10080. */
  isolated_idle_sleep_minutes?: number;
  isolated_sleep_window_enabled?: boolean;
  /** "HH:MM", 24-hour; start and end must differ. */
  isolated_sleep_window_start?: string;
  isolated_sleep_window_end?: string;
  /** 0 for no limit. Apps already over it are shown, not refused. */
  max_versions_per_app?: number;
  /** Off stops keys that already exist, not merely new ones. */
  api_keys_enabled?: boolean;
  /** 0 for no cap, otherwise 1 to 3650 days. Bounds new keys only. */
  api_key_max_lifetime_days?: number;
  /** Off withdraws anonymous access from every app that has it, immediately,
   * and refuses the change to Public. No app's visibility is written. */
  public_apps_enabled?: boolean;
  audience_measurement_enabled?: boolean;
  app_security_headers?: boolean;
  app_log_copy_uncapped?: boolean;
  apps_listable_to_authenticated?: boolean;
  /** Dormancy locking. The switch is what turns the sweep on — there is no
   * value of the threshold that means off, and 0 is refused rather than read
   * as "never lock anyone". */
  dormant_lock_enabled?: boolean;
  /** 30 to 730 days. Refused out of range with a message naming it. */
  dormant_lock_days?: number;
  /** 1 to 24 hours. No zero-means-off: there is no value of it that turns
   *  sessions off. Shortening it signs out sessions that are already open. */
  session_absolute_hours?: number;
  /** 0 to turn the idle timeout off, or 5 to 480 minutes. */
  session_idle_minutes?: number;
  /** Where links in email point. Send empty to fall back to BASE_URL. */
  public_base_url?: string;
  /** Garth's address. Send empty to take the Open in Garth link away. */
  garth_url?: string;

  /** The security.txt fields. The contact is the switch — sent empty, this
   *  install stops publishing the file at all. An expiry is required beside a
   *  contact and a date in the past is refused, because that field is what
   *  makes a stale file visible. */
  security_txt_contact?: string;
  security_txt_expires?: string;
  security_txt_policy?: string;
  security_txt_languages?: string;
  /** What the gallery offers. Refused by name if it is not one of the three —
   *  never clamped, because an administrator who typed something meant it. */
  gallery_deploy_mode?: GalleryMode;

  /** The job limits. Each is refused out of range with a message naming the
   * range — none of them clamps silently. */
  default_job_timeout_seconds?: number;
  max_job_timeout_seconds?: number;
  job_memory_limit_mb?: number;
  job_cpu_limit_millicores?: number;
  max_queued_runs_per_app?: number;
  notebook_render_timeout_seconds?: number;
  report_delivery_min_interval_minutes?: number;
  build_timeout_seconds?: number;
  /** Job history. Rows must outlive logs, and the inversion is refused. */
  job_log_retention_days?: number;
  job_run_retention_days?: number;
  /** Failure-event history. Read by the sweep per pass, so a change lands on
   * the next sweep rather than the next restart. */
  app_event_retention_days?: number;
  platform_log_retention_days?: number;
  /** Who each announcement reached. The announcement and its counts are never
   * pruned; this is the clock on the rows naming people. */
  announcement_recipient_retention_days?: number;
  access_record_retention_days?: number;
}

export interface SystemHealth {
  /** Admin-only; null for everyone else, and when the install has no shared host to report on. */
  system: {
    cpu_percent: number;
    memory_total: number;
    memory_used: number;
    memory_percent: number;
    disk_total: number;
    disk_used: number;
    disk_percent: number;
  } | null;
  /** Distinguishes "no host to report on" from "the read failed". */
  system_available: boolean;
  /** Admin-only; absent for everyone else. */
  server?: {
    version: string;
    /** Commit the binary was built from; "" when the build had no VCS stamp. */
    revision: string;
    go_version: string;
    /** Toolchains on the shared host; version is "" when a binary is absent. */
    runtimes: Array<{ name: string; version: string }>;
    /**
     * The three languages as a *set* rather than a row: every version this
     * host has, which one an app with no pin gets, and what the isolated
     * runner image is declared to carry. Both halves, because a pin is judged
     * against the mode the app runs in.
     */
    toolchains: Array<{
      language: string;
      label: string;
      installed: Array<{
        language: string;
        version: string;
        /** Absent for a declared version — a runner image's, which is not on this host. */
        path?: string;
        /** The one an app with no [runtime] pin builds against. */
        default: boolean;
      }>;
      /** "" on an install with no declaration, and always for Go. */
      runner?: string;
      /** Go's GOTOOLCHAIN, and only Go's. "" when nobody has set it. */
      posture?: string;
    }>;
    hostname: string;
    os: string;
    started_at: string;
    uptime_seconds: number;
  };
  summary: {
    total: number;
    running: number;
    crashed: number;
    /** A build that never produced a release. Counted apart from crashed: the
     *  estate list filters on each word separately, so folding the two together
     *  sent a reader to a Crashed filter holding nothing. */
    failed: number;
    building: number;
    stopped: number;
    /** Idle apps the platform stopped. Counted apart from stopped on purpose. */
    sleeping: number;
  };
  apps: Array<{
    id: string;
    name: string;
    slug: string;
    /** Absent, with `metrics`, for an app held at the `list` rung. */
    status?: string;
    metrics?: AppMetrics;
  }> | null;
}

/** One live workload. App-shaped rather than process-shaped: the backend owns
 * paths, ports, and pids, so a row names the app, its release, and the readings. */
export interface SystemProcess {
  app_id: string;
  app_name: string;
  app_slug: string;
  framework?: string;
  /** The process name the backend knows this workload by — the release id. */
  release: string;
  /** The mode the workload is *running* under, not the next deploy's mode. */
  mode: "shared" | "isolated";
  owner_id?: string;
  owner_login?: string;
  owner_name?: string;
  /** The owning group's name, when a group owns the app rather than a person.
   *  Exactly one of this and owner_id is set. */
  owner_group?: string;
  /** The app's recorded status. */
  status: string;
  /** What the backend says right now, in the four words both backends share
   * (`online | launching | stopped | errored`) plus "missing" for a name no
   * backend holds at all. A row that is not up is here for exactly one reason:
   * the app is recorded running and there is no process behind it. */
  process_status: string;
  /** Absent when this workload's backend takes no reading — the same terms as
   *  host below. Such rows sort after every measured one. */
  cpu?: number;
  /** Resident memory, in MB. Absent on the same terms as cpu. */
  memory?: number;
  /** Seconds since the process started, and zero for a workload that is not
   * up — a stopped backend entry still remembers when the process that is gone
   * began, and reporting that is a figure counting up for nothing. */
  uptime: number;
  restarts: number;
  /** This machine for a shared workload; absent for an isolated one. */
  host?: string;
}

export interface ProcessList {
  /** The node that answered — these are its processes, not the install's. */
  node: string;
  count: number;
  /** Set when an execution backend did not answer, so the list may be short. */
  degraded?: string;
  /** Apps with no workload, counted rather than listed: they have nothing to
   *  say in any column this page has. They are here so the table can account
   *  for them in a sentence rather than have an operator conclude their app
   *  has left the estate. */
  sleeping: number;
  stopped: number;
  processes: SystemProcess[];
}

/** Someone active within the window, as seen by the node that answered. */
export interface PresenceUser {
  id: string;
  login: string;
  name?: string;
  avatar_url?: string;
  system_role?: string;
  last_seen: string;
  /** The app they last went through; absent for platform-only activity. */
  app_id?: string;
  app_name?: string;
  app_slug?: string;
  /** True when that app request is itself inside the window — they are in the
   * app now, as opposed to having opened it earlier and moved on. */
  in_app: boolean;
}

/** Live presence, held in memory per node: it counts the last few minutes and
 * resets when the process restarts. */
export interface Presence {
  window_seconds: number;
  node: string;
  count: number;
  users: PresenceUser[];
}

export interface ConsumptionRow {
  app_id: string;
  job_run_id?: string;
  cpu_seconds: number;
  average_memory_mb: number;
  peak_memory_mb: number;
  memory_byte_hours: number;
  observed_seconds: number;
  /** Part of the period was not observed — the figures are a lower bound. */
  incomplete: boolean;
  incomplete_message?: string;
}

/**
 * One app's own consumption, over the window the Usage tab is already drawing
 * traffic on — demand and cost over **one** period, which is the whole
 * argument ADR §50 D7 made for the estate table and the reason this is on that
 * tab rather than on a page of its own (ADR §53 D7).
 */
export interface AppConsumption {
  from: string;
  to: string;
  total: ConsumptionRow;
  /** Every background job run inside the window, each with its own figures. */
  job_runs: ConsumptionRow[];
  /** The app's configured memory cap, so peak reads against a limit rather
   *  than against nothing. */
  memory_limit_mb: number;
  /** Both limits as a start resolves them, each with its source and how this
   *  install holds it. */
  memory_limit: { value_mb: number; source: string; enforcement: Enforcement };
  cpu_limit: { value_millicores: number; source: string; enforcement: Enforcement };
  /** What the scheduler reserves for this app, in millicores — absent on a
   *  shared workload, which has no scheduler to reserve from. */
  cpu_request_millicores?: number;
  /** The last hour held at the CPU limit; absent where nothing measures it. */
  cpu_throttled_last_hour?: {
    periods: number;
    throttled_periods: number;
    throttled_seconds: number;
    measured_seconds: number;
  };
}

/** How a limit is held: by the kernel, by a watchdog's sample, or not at all. */
export type Enforcement = "enforced" | "sampled" | "not_enforced";

export interface ConsumptionSummary {
  from: string;
  to: string;
  /** Ranked by consumption, narrowed to apps the caller can read. */
  apps: ConsumptionRow[];
  /** Admin-only; null for everyone else, and when the install has no shared host. */
  host_capacity: { memory_total_mb: number; memory_used_mb: number } | null;
}

/**
 * A log as a file, from the server: the whole of it, whatever the page holds
 * (ADR §87). A plain `<a download>` target, so the browser owns the transfer.
 */
export function runLogFileUrl(appId: string): string {
  return `${BASE}/apps/${appId}/logs?download=1`;
}

/** A finished run's output file. The server answers with a redirect to the
 *  object itself, 404 when the run wrote none and 410 once it has expired. */
export function jobRunOutputUrl(appId: string, runId: string): string {
  return `${BASE}/apps/${appId}/job-runs/${runId}/output`;
}

export function buildLogFileUrl(appId: string, deployId: string): string {
  return `${BASE}/apps/${appId}/deployments/${deployId}/logs?download=1`;
}

/** The same text shown in a tab of its own, for the browser's own find and save. */
export function runLogRawUrl(appId: string): string {
  return `${BASE}/apps/${appId}/logs?raw=1`;
}

export function buildLogRawUrl(appId: string, deployId: string): string {
  return `${BASE}/apps/${appId}/deployments/${deployId}/logs?raw=1`;
}

export function wsUrl(path: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${BASE}${path}`;
}

/**
 * A server-sent-events endpoint, as an absolute URL.
 *
 * `EventSource` takes a URL rather than going through `request`, so the one
 * place the API prefix is written down has to be reachable from outside it —
 * the same reason `wsUrl` exists above.
 */
export function sseUrl(path: string): string {
  const { protocol, host } = window.location;
  return `${protocol}//${host}${BASE}${path}`;
}

// --- New types for UI rebuild ---

export interface AuditLogEntry {
  id: string;
  timestamp: string;
  user: string;
  action: string;
  target: string;
  details: string;
}

/**
 * A tag is a key; an app holds at most one value for it.
 *
 * `system` tags are platform-owned — their values are derived from the app row
 * (today: Framework, from the detected framework) and cannot be renamed,
 * deleted, or set. `custom` tags are defined by admins and valued by users.
 */
export interface Tag {
  id: string;
  name: string;
  slug: string;
  kind: "system" | "custom";
  color: TagColor;
  description: string;
  created_by?: string;
  /** Distinct values in use, counted only over apps the caller can read. */
  value_count: number;
  /** Apps carrying any value, counted only over apps the caller can read. */
  app_count: number;
  created_at: string;
  updated_at: string;
}

export type TagColor =
  | "indigo"
  | "cyan"
  | "fuchsia"
  | "green"
  | "amber"
  | "red"
  | "violet"
  | "orange"
  | "slate";

/** One key/value pair on an app. */
export interface AppTag {
  tag_id: string;
  tag_name: string;
  tag_slug: string;
  kind: "system" | "custom";
  color: TagColor;
  value: string;
  value_slug: string;
}

/** One value in use for a tag, with how many readable apps carry it. */
export interface TagValue {
  value: string;
  slug: string;
  app_count: number;
}

/** One app's traffic over the window. Every app the install has appears,
 * including one with no traffic at all. */
export interface AppUsageSummary {
  app_id: string;
  name: string;
  slug: string;
  framework?: string;
  status: string;
  owner_id?: string;
  owner_login?: string;
  /** The owning group, when a group owns the app rather than a person. */
  owner_group_id?: string;
  owner_group?: string;
  requests: number;
  page_loads: number;
  /** Distinct signed-in people across the whole window — not the sum of the
   * daily counts. */
  viewers: number;
  /** The most recent day with traffic (YYYY-MM-DD), absent when there is none.
   * A day rather than a timestamp: usage is counted per day. */
  last_day?: string;
  series: AppUsageDay[];
}

export interface UsageOverview {
  days: number;
  apps: AppUsageSummary[];
}

/**
 * Which fact the quiet clock is running from. The three are a wire contract
 * with database.QuietBasis, and the page explains each in its own words —
 * "quiet for 240 days" means something different when nobody ever opened the
 * app than when somebody used it daily until March.
 */
export type QuietBasis = "visit" | "deploy" | "created" | "retention";

/** One app nobody has opened lately, with the evidence for saying so. */
export interface QuietApp {
  app_id: string;
  name: string;
  slug: string;
  status: string;
  owner_id?: string;
  owner_login?: string;
  owner_group?: string;
  /** Lifetime requests, not the window's — the app this list is for is the one
   * whose forty thousand requests all happened last year. */
  requests: number;
  /** Already locked. The list is where locking gets decided, so it has to say
   * what was decided last time — nine rows that never change read as nine
   * outstanding decisions. */
  locked: boolean;
  /** Most recent day with any traffic, absent for an app never opened. */
  last_day?: string;
  quiet_since: string;
  quiet_basis: QuietBasis;
  quiet_days: number;
}

export interface QuietAppList {
  days: number;
  /** Every app on the install, so the page can say "9 of 84" rather than
   * letting a list of nine read as the whole estate. */
  total: number;
  apps: QuietApp[];
}

/** One build that has not finished. */
export interface QueuedBuild {
  id: string;
  app_id: string;
  app_name: string;
  app_slug: string;
  owner_login?: string;
  /** The owning principal. `AppOwner` renders either from these — without them
   *  a group-owned app read as having no owner at all. */
  owner_id?: string;
  owner_group_id?: string;
  status: "pending" | "building";
  triggered_by: string;
  commit_sha?: string;
  branch?: string;
  enqueued_at: string | null;
  /** 1 is the build the worker is on. Derived from the order, not stored. */
  position: number;
}

export interface BuildQueue {
  count: number;
  /** How many builds this install runs at once — operator configuration, not a
   * setting. Without it the page cannot say whether a waiting build is waiting
   * on anything. */
  concurrency: number;
  items: QueuedBuild[];
}

/** One scheduled job, with the app that owns it and how its last run went. */
export interface ScheduledJob extends JobDefinition {
  app_name: string;
  app_slug: string;
  owner_login?: string;
  /** The owning principal. `AppOwner` renders either from these — without them
   *  a group-owned app read as having no owner at all. */
  owner_id?: string;
  owner_group_id?: string;
  /** Absent for a schedule that has never fired — which is a different fact
   * from one whose last run failed. */
  last_run_at?: string;
  last_status?: string;
  /** The app's lock holds every firing without pausing the schedule. */
  app_locked?: boolean;
}

/** One firing on Admin → Automation's Upcoming view — or, with `count` set,
 *  one entry standing for an expression firing more often than hourly. */
export interface UpcomingFiring {
  kind: "job" | "render" | "restart";
  schedule_id: string;
  app_id: string;
  app_name: string;
  app_slug: string;
  /** The job's; a re-render and a restart have none. */
  name?: string;
  /** UTC; a recurrence's first firing. */
  at: string;
  /** The schedule's own zone, "" for UTC, and its expression without it. */
  zone: string;
  schedule: string;
  /** Median of the last `basis` scheduled runs that started; null with none. */
  typical_sec: number | null;
  /** The firing's bound — a job's timeout, a restart's three minutes. */
  upper_sec: number | null;
  basis: number;
  last?: string;
  count?: number;
  gap_sec?: number;
}

export interface UpcomingSchedules {
  from: string;
  to: string;
  firings: UpcomingFiring[];
  recurring: UpcomingFiring[];
  /** Schedules that would otherwise fire in the interval. */
  held: { paused: number; locked: number };
}

export interface ScheduledOverview {
  /** Only jobs that carry a schedule: one that is started and never fires by
   * itself has no answer to "when", so it belongs to the run list. */
  jobs: ScheduledJob[];
  /** One per app that declares one. Absent from an older server. */
  renders?: EstateSchedule[];
  /** The same for `[restart]`; `last_status` is the last firing's outcome. */
  restarts?: EstateSchedule[];
}

/** An app's re-render or restart schedule on Admin → Automation: one per app,
 *  no command, one outcome per firing. */
export interface EstateSchedule {
  id: string;
  app_id: string;
  app_name: string;
  app_slug: string;
  owner_login?: string;
  owner_id?: string;
  owner_group_id?: string;
  schedule: string;
  enabled: boolean;
  next_run_at?: string;
  /** Absent for a schedule that has never fired. */
  last_run_at?: string;
  last_status?: string;
  /** The app's lock holds every firing without pausing the schedule. */
  app_locked?: boolean;
}

/** One line of the system check report. `skip` is a capability this install
 * does not offer — absent, not degraded. */
export interface SystemCheck {
  id: string;
  name: string;
  category: string;
  status: "pass" | "warn" | "fail" | "skip";
  detail: string;
  /**
   * Where this finding is fixed. At most one of the two, and never on a `pass`
   * or a `skip` — a fact links nowhere, which is the quiet second signal of
   * which rows are worth acting on.
   *
   * `href` is an admin route. `setting` is a settings *key*, resolved through
   * SETTINGS_INDEX rather than sent as a URL, because the route and anchor of
   * every setting are already a guarded table on this side (ADR §50) and a
   * backend spelling one by hand would be a second copy of it. Resolve both
   * with `checkDestination` in `@/lib/system-checks`.
   */
  href?: string;
  setting?: string;
}

/** The install's self-diagnosis. Computed when asked and stored nowhere, so
 * there is no history to page through — only the answer for right now. */
export interface SystemCheckReport {
  ran_at: string;
  hostname: string;
  duration_ms: number;
  status: "pass" | "warn" | "fail";
  passed: number;
  warned: number;
  failed: number;
  skipped: number;
  checks: SystemCheck[];
  /** The shared build cache. Absent on an install with no cache directory,
   *  which is what keeps the card off a page that has nothing to put in it. */
  build_cache?: BuildCacheState | null;
  /** Every filesystem the install writes to, measured when the report was
   *  asked for and never otherwise. Absent where nothing can measure. */
  disks?: DisksBlock | null;
}

/** One folder the install writes to, and what it holds. */
export interface DiskFolder {
  path: string;
  what: string;
}

/** One filesystem, fullest first in the list. `used_percent` is df’s: used
 *  over used plus available, so root’s reserve does not read as free. */
export interface DiskReport {
  folders: DiskFolder[];
  total_bytes: number;
  used_bytes: number;
  free_bytes: number;
  used_percent: number;
  /** A network filesystem reporting a size it does not have (EFS says 8 EiB);
   *  there is no fullness to draw. */
  unbounded?: boolean;
  /** The release disk’s watermarks applied here: below the soft one is
   *  `low`, below the hard one `full`. */
  level: "ok" | "low" | "full";
}

export interface DisksBlock {
  disks: DiskReport[];
  /** Folders whose filesystem did not answer within a second. */
  unanswered?: (DiskFolder & { error: string })[];
}

/**
 * The shared npm/uv/Go build cache.
 *
 * `measured` is the field to branch on before showing any number, and it is
 * load-bearing: the size is walked on a fifteen-minute ticker rather than per
 * request, so an install in the first minutes after a restart has an
 * *unmeasured* cache and not an empty one. `bytes` is absent until then — never
 * a zero standing in for "we have not looked yet".
 */
export interface BuildCacheState {
  dir: string;
  measured: boolean;
  bytes?: number;
  measured_at?: string;
  /** Why the newest measurement did not land. The figure beside it still
   *  stands — a stale number with an honest age beats none. */
  measure_error?: string;
  reclaiming: boolean;
  started_at?: string;
  last_freed?: number;
  last_reclaim?: string;
  /** Tools that skipped or failed, one line. Not a failed reclaim: a run in
   *  which npm was absent still reclaimed whatever uv and Go had. */
  last_error?: string;
}

/**
 * The gallery: a catalog the Toft team publishes, fetched from a fixed
 * address compiled into the binary.
 *
 * Two kinds, and they behave differently on purpose. An **extension** is
 * administrator-only, comes from a git repository, and can be updated in place
 * when the catalog names a newer version. An **example** is a ZIP snapshot
 * anyone who may publish can take, and is never updated — it is a copy its
 * owner owns from the moment it lands.
 */
export type GalleryKind = "extension" | "example";

export interface GalleryEnvField {
  name: string;
  required: boolean;
  default: string;
  description: string;
  /** Collected as a password and never rendered back. Every env var is
   *  encrypted at rest either way; this decides what the interface shows. */
  sensitive?: boolean;
}

/** One installed copy. Derived from live app rows, never a stored flag — an
 *  app that is deleted stops being installed the moment it is gone. */
export interface GalleryInstalled {
  app_id: string;
  slug: string;
  name: string;
  /** What that copy is running. Empty until its first build succeeds. */
  version: string;
  /** Whether it is the viewer's own copy, which for an example is the whole
   *  question being asked. */
  mine: boolean;
  update_available?: boolean;
  /** The versions could not be compared — which is not the same as current. */
  version_unknown?: boolean;
  /** Nothing was recorded at install, which is why. Never set while the first
   *  build is still to land: an empty version means "building" then. */
  version_not_recorded?: boolean;
}

export interface GalleryItem {
  id: string;
  kind: GalleryKind;
  name: string;
  summary: string;
  description: string;
  author: string;
  language: string;
  tags: string[] | null;
  icon: string;
  version: string;
  min_platform_version: string;
  /** Extensions only. */
  repo_url: string;
  ref: string;
  /** Examples only. */
  zip_url: string;
  /** The folder of the source this item is, when items share a repository. */
  path?: string;
  /** Where a person may go and read the code. Informational, on either kind,
   *  and never fetched — `repo_url` is the update channel, and an example has
   *  none, so this is an example's only readable address. */
  source_url: string;
  suggested_slug: string;
  env: GalleryEnvField[] | null;
  notes: string;

  /** Whether this install will deploy it, and the sentence explaining why not. */
  deployable: boolean;
  blocked_reason?: string;
  installed: GalleryInstalled[] | null;
  update_available: boolean;
}

/** The last refresh. An empty catalog and an unreachable one must never look
 *  alike, which is what `status` is for. */
export interface GallerySync {
  status: "" | "ok" | "unreachable" | "unparseable" | "too_new";
  error?: string;
  item_count: number;
  dropped_count: number;
  fetched_at?: string | null;
}

export interface GalleryResponse {
  mode: GalleryMode;
  items: GalleryItem[] | null;
  sync: GallerySync;
  admin: boolean;
  /** One banner above the whole catalog when this install's source policy is
   * restricted: every Deploy button is off, and the templates stay browsable
   * for reference. Empty otherwise. */
  source_blocked?: string;
}

export type GalleryMode = "off" | "extensions_only" | "all";

/* --- Where apps may come from --- */

/**
 * `any` is the default and today's behaviour: any https repository. `allowlist`
 * deploys only from a listed pattern. `none` is no remote git at all, and is a
 * separate value rather than an empty allowlist because "not configured yet"
 * and "deliberately nothing" want different copy.
 */
export type GitSourceMode = "any" | "allowlist" | "none";

/** One entry on the approved-source list, in the canonical form the server
 *  stores: lowercase, and an explicit trailing `/*` for a subtree. */
export interface AllowedGitSource {
  id: string;
  pattern: string;
  /** Why this entry exists, in the administrator's own words. */
  note?: string;
  created_at: string;
  created_by?: string;
  /** How many apps currently match it — what makes an entry added for one
   *  migration and long since irrelevant findable. */
  apps: number;
}

export interface SourcePolicy {
  mode: GitSourceMode;
  /** A separate switch, because upload-only is a legitimate install and is the
   *  exact inverse of restricting git. */
  uploads_enabled: boolean;
  patterns: AllowedGitSource[];
  /** How many apps the *stored* policy already blocks. They keep running. */
  non_compliant: number;
}

/** A PUT or a preview. `patterns` is the whole list, not a delta — the server
 *  diffs it against what it had and audits each addition and removal by name. */
export interface SourcePolicyInput {
  mode?: GitSourceMode;
  uploads_enabled?: boolean;
  patterns?: { pattern: string; note?: string }[];
}

/** One app, reduced to where its code comes from. */
export interface AppSource {
  app_id: string;
  slug: string;
  name: string;
  owner_id: string;
  owner_name?: string;
  repo_url: string;
}

/** What a proposed policy would do. Nothing is saved until this has been seen —
 *  on an install with forty apps it is the difference between adopting the
 *  feature and abandoning it. */
export interface SourcePolicyPreview {
  mode: GitSourceMode;
  patterns: string[];
  non_compliant: AppSource[] | null;
  count: number;
  /** Entries that will not parse, reported beside the answer rather than
   *  replacing it: the administrator is mid-edit. */
  invalid?: { pattern: string; error: string }[];
  /** Entries that will be stored differently from how they were typed — a bare
   *  host gaining its `/*`, a mixed-case path. Shown before saving. */
  canonicalized?: { typed: string; stored: string }[];
  uploads_enabled: boolean;
}

/** A host, organization or repository already in use, offered so that a
 *  starting list is a few clicks rather than transcription. */
export interface ObservedSource {
  pattern: string;
  kind: "host" | "org" | "repo";
  apps: number;
}

/** What every signed-in user may know: enough for New App to say what would
 *  work instead of refusing blindly. */
export interface EffectiveSourcePolicy {
  mode: GitSourceMode;
  patterns: string[] | null;
  uploads_enabled: boolean;
  /** False under `none` — the form offers upload only. */
  git_allowed: boolean;
}

export interface GalleryDeployRequest {
  name?: string;
  slug?: string;
  env_vars?: Record<string, string>;
  env_sensitive?: Record<string, boolean>;
}

// ---------------------------------------------------------------- Background jobs

/** A job is a different entrypoint of the app itself — same repo, same release,
 * a different command — declared in toft.toml as `[jobs.<name>]`. Nothing in
 * the UI creates, edits or deletes one; the one write is pausing its schedule
 * until the next deploy. */
export interface JobDefinition {
  id: string;
  app_id: string;
  name: string;
  /** A shell command line, or `args` — the argv of a single binary. One of the two. */
  command?: string;
  args?: string[];
  /** 5-field cron, with the zone as a `CRON_TZ=` prefix (`lib/schedule.ts`). */
  schedule?: string;
  /** The declared parameters, keyed by name. Empty or null when there are none. */
  params_spec: Record<string, JobParamSpec> | null;
  timeout_sec: number;
  /** An administrator's limits for this job; absent where the install's job
   *  defaults apply. */
  override_memory_mb?: number;
  override_cpu_millicores?: number;
  /** What a scheduled firing does while the last run is still going. */
  overlap: "queue" | "skip" | "";
  /** Who hears about a run's outcome. */
  notify: "none" | "failures" | "always" | "";
  /** False while the schedule is paused. */
  enabled: boolean;
  next_run_at?: string;
  /** How long it may go without a success before it is reported overdue. */
  expect_within_sec?: number;
  enabled_at: string;
  created_at: string;
  updated_at: string;
}

export type JobParamType = "string" | "int" | "float" | "bool";

/** One declared parameter. `required` and `default` are mutually exclusive. */
export interface JobParamSpec {
  type: JobParamType;
  required?: boolean;
  default?: string | number | boolean;
  help?: string;
  choices?: (string | number | boolean)[];
}

/** Where an effective limit came from. `clamped` means toft.toml asked for more
 *  than the install allows, and `declared` then carries what it asked for. */
export type JobLimitSource = "declared" | "install default" | "override" | "clamped" | "install ceiling";

export interface JobLimit {
  /** Seconds for a timeout, MB for memory, millicores for CPU. */
  value: number;
  source: JobLimitSource | string;
  declared?: number;
  /** Memory and CPU only: how the install holds it where the job runs. */
  enforcement?: Enforcement;
}

/** One job as the Automation tab sees it: the definition, its effective limits,
 *  and whether its schedule is paused. */
export interface JobListEntry extends JobDefinition {
  limits: { timeout: JobLimit; memory: JobLimit; cpu: JobLimit };
  /** The schedule is switched off until the next deploy. */
  paused: boolean;
}

/** The capability state travels with the list, so the tab can explain why it is
 * inert rather than showing an empty page that looks broken. */
export interface JobDefinitionList {
  jobs: JobListEntry[];
  jobs_available: boolean;
  /** Why not, when unavailable — install-level refusals only. */
  unavailable_bec?: string;
  /** Where runs go: "cluster", "this machine", or "" when they cannot run. */
  placement: string;
  execution_mode: string;
  concurrency_limit: number;
  /** How many runs this app may have waiting. */
  max_queued: number;
}

export type JobRunStatus =
  | "queued" | "running" | "succeeded" | "failed" | "timed_out"
  | "out_of_memory" | "cancelled" | "skipped";

export type JobStartedBy = "app" | "viewer" | "cli" | "schedule" | "ui";

export interface JobRun {
  id: string;
  definition_id?: string;
  app_id: string;
  job: string;
  status: JobRunStatus | string;
  /** Who or what started it. */
  started_by: JobStartedBy | string;
  /** The person behind a viewer, CLI or UI start, resolved at read time. Empty
   * is meaningful rather than missing: a run with no person behind it must
   * never render as one. */
  started_by_user?: string;
  /** The run this one was started again from. */
  rerun_of?: string;
  params: Record<string, unknown> | null;
  execution_mode: string;
  queued_at: string;
  started_at?: string;
  finished_at?: string;
  /** When the run will be stopped. */
  deadline?: string;
  exit_code?: number;
  failure_stage?: string;
  /** Words: `exit 3: connection refused`, `timed out after 45m`, `stopped at 1024 MB`. */
  failure_reason?: string;
  peak_memory_mb?: number;
  /** Seconds the run was held at its CPU limit; absent where not measured. */
  cpu_throttled_seconds?: number;
  progress: number;
  progress_message?: string;
  /** Whether the run wrote an output object that is still there. */
  has_output: boolean;
  /** 1-based while queued, absent otherwise — its place in its own app's line. */
  queue_position?: number;
  /** The instant this run is for: its schedule's tick, or one a person named. */
  scheduled_for?: string;
  /** The live status page on the app's origin. */
  status_url?: string;
  logs_url?: string;
  /** Only on a start that matched a run already open. */
  deduplicated?: boolean;
  created_at: string;
}

/** One run with enough of its app to render a row on the install-wide page. */
export interface AdminJobRun extends JobRun {
  app_name: string;
  app_slug: string;
  owner_login?: string;
}

export interface AdminJobRunList {
  runs: AdminJobRun[];
  /** Whether this install offers background jobs at all. An empty list on an
   * install that never turned them on is not the same page as a quiet one. */
  jobs_available: boolean;
  /** Why not, in the platform's own words. Empty when they are available. */
  unavailable_bec: string;
  /** Runs in flight allowed **per app** — there is no install-wide queue. */
  concurrency_limit: number;
  /** Runs waiting allowed per app. */
  max_queued?: number;
}

export interface JobLogLine {
  seq: number;
  at: string;
  stream: string; // stdout | stderr
  line: string;
}

// ------------------------------------------------------------ Events & activity

/** What broke, and when. Every occurrence is recorded; only some are notified —
 * an owner can silence the noise without erasing the platform's memory. */
export interface AppEvent {
  id: string;
  app_id: string;
  kind: string;
  severity: string;
  source: string;
  summary: string;
  detail: string;
  /** May name a deployment retention has since pruned — events outlive
   * deployments on purpose, so a reader must tolerate a dangling id. */
  deployment_id?: string;
  fingerprint: string;
  occurred_at: string;
  /** The cooldown claim — this occurrence won the right to be the one people
   * are told about. `notice_sent` is whether anyone actually was. */
  notified_at?: string;
  notice_sent: boolean;
  resolved_at?: string;
}

/** The chips on the activity page. "system" is the events half of the union;
 * the rest select audit actions. Kept in step with `activityCategories` in
 * `event_handlers.go` — an unknown one is a 400, not an empty list. */
export type ActivityCategory =
  "all" | "deploys" | "access" | "signins" | "settings" | "system";

/** The merged feed. `audit_log` and `app_events` gain no shared writer — the
 * two are joined only at read time. */
export interface ActivityEntry {
  id: string;
  source: "audit" | "event";
  /** A user's login, "cli" for an operator action, or "system:<source>" for a
   * machine observation. */
  actor_label: string;
  /** The id behind the label, where the actor has one — what the feed is
   * narrowed to an actor by. Absent on an `event` and for "cli". */
  actor_id?: string;
  action: string;
  target_type: string;
  target_id: string;
  target_label: string;
  /** The target is an app that has since been deleted: name it, never link it. */
  target_gone?: boolean;
  /** The app this row is part of the history of, which is not always its
   * target: a denied access request targets the request. */
  app_id?: string;
  severity: string;
  summary: string;
  detail: string;
  occurred_at: string;
  /** Where the call came from and which credential made it — the request-scoped
   * half of an audit row. All five are absent on an `event`, which no request
   * produced. `ip` is the client the platform resolved, so behind a load
   * balancer it is the person rather than the balancer. `api_key_name` is
   * resolved at read time and absent for a key that has since been deleted;
   * `api_key_id` survives it. */
  ip?: string;
  user_agent?: string;
  api_key_id?: string;
  api_key_name?: string;
  request_id?: string;
}

// --------------------------------------------------- The integration catalog

/** One integration this build offers, exactly as `appint.Known` describes it.
 *
 * The catalog is the specification: a kind absent from it is not an
 * integration, and every field below is read by something — the connection
 * form, the destination picker, the attach dialog's copy, the ledger's filter. */
export interface IntegrationKind {
  name: string;
  title: string;
  summary: string;
  /** What a connection of this kind is welded to — "workspace", "site". The
   * scope is fixed when the connection is made and is never editable: changing
   * it under a live attachment re-points every app on it silently. */
  scope_label: string;
  scope_hint?: string;
  /** What is worth writing in a connection's description, for this kind. */
  description_hint?: string;
  /** Set only where the far side cannot be read from Toft: what a connection's
   * card says while its description is empty. */
  description_missing?: string;
  credentials: IntegrationCredentialField[];
  /** Whether a connection of this kind can list what it may reach. Where it is
   * false the destinations route 404s rather than returning an empty list —
   * an empty list reads as "none configured yet". */
  enumerates: boolean;
  destination_label?: string;
  /** `nothing` is the token action: the app asks and is handed something. */
  actions: { name: string; carries: "text" | "file" | "nothing"; summary: string }[];
  text_limit: number;
  file_limit: number;
  /** This kind puts objects in somebody else's system that outlive the send,
   * the attachment and the app. What the attach dialog's copy turns on. */
  leaves_residue: boolean;
  platform_audiences: string[];
  refusals: {
    not_connected: string;
    not_attached: string;
    disabled: string;
    /** Set on the kinds that can hand an app the connection's access token. */
    tokens_off?: string;
  };
}

export interface IntegrationCredentialField {
  key: string;
  label: string;
  help?: string;
  /** Secret fields are stored encrypted and are write-only: they go in and
   * never come back. */
  secret: boolean;
  required: boolean;
}

/** Where a send lands. `id` routes; `label` is what a person reads and nothing
 * routes on it. */
export interface IntegrationDestination {
  id: string;
  label: string;
}

/** One credential welded to one scope. */
export interface IntegrationConnection {
  id: string;
  kind: string;
  title: string;
  name: string;
  /** The administrator's own note; "" when none was written. */
  description: string;
  scope_id: string;
  scope_label: string;
  config: Record<string, string>;
  /** Where a send goes when nobody chose. Unset falls back; broken fails. */
  default_destination: IntegrationDestination;
  for_platform: boolean;
  for_apps: boolean;
  platform_audiences: string[];
  /** Apps attached here may get the connection's own access token, which
   * reaches the connection's whole grant — not only their folder. */
  issue_tokens: boolean;
  /** While `issue_tokens` is on: why an app asking now would be refused (a
   * Graph token carrying permissions beyond files). Checked on every read. */
  token_problem?: string;
  /** All a client learns about the stored credential: whether there is one… */
  secret_set: boolean;
  /** …and whether that stored value can still be decrypted. `set` with
   * `unreadable` is a changed or lost ENCRYPTION_KEY: the form's "leave blank
   * to keep" would keep something unusable, so the field has to say so. */
  secret_unreadable: boolean;
  /** How many apps are attached — what the delete refusal is about. */
  apps: number;
  /** Their names, for the confirm in front of the token switch. */
  app_names: string[];
  /** How many objects this connection has out in somebody else's system. */
  placements: number;
  created_at: string;
  updated_at: string;
}

export interface NewIntegrationConnection {
  kind: string;
  name: string;
  /** Optional; on an update, absent keeps the stored one and "" clears it. */
  description?: string;
  scope_id?: string;
  config?: Record<string, string>;
  /** Write-only: sent to set the stored credential, never read back. */
  credentials?: Record<string, string>;
  for_platform?: boolean;
  for_apps?: boolean;
  platform_audiences?: string[];
  /** Accepted only for a kind that issues tokens. */
  issue_tokens?: boolean;
  default_destination_id?: string;
  default_destination_label?: string;
}

/** The scope is absent on purpose: a new scope is a new connection. */
export type UpdateIntegrationConnection = Omit<
  NewIntegrationConnection,
  "kind" | "scope_id"
>;

/** A Teams team or chat an administrator pasted onto a bot connection. */
export interface IntegrationPlace {
  id: string;
  connection_id: string;
  kind: "team" | "chat";
  conversation_id: string;
  name: string;
  added_at: string;
  /** Whether Teams still answers for it — false once the app is removed there. */
  reachable: boolean;
  problem?: string;
}

/** One object an app left in somebody else's system.
 *
 * It carries no foreign key to the app or the connection, and both names are
 * stamped in at the time: the row outlives them, which is the whole reason it
 * exists. Detaching reaches none of this, and the platform will not delete it
 * on anybody's behalf. */
export interface IntegrationPlacement {
  id: string;
  connection_id: string;
  connection_name: string;
  kind: string;
  destination: IntegrationDestination;
  item_id: string;
  item_url: string;
  name: string;
  bytes: number;
  content_type: string;
  sha256: string;
  app_id: string;
  app_name: string;
  send_id: string;
  placed_at: string;
}

// ------------------------------------------------------------- App integrations

/** One kind as one app sees it. The app supplies the message; the platform
 * supplies the identity. */
export interface AppIntegrationRow {
  kind: string;
  title: string;
  summary: string;
  /** Whether the platform offers this kind at all — "your administrator has not
   * attached this" and "this install does not do that" are different asks, and
   * a card cannot tell them apart without this. */
  connected: boolean;
  attached: boolean;
  enabled: boolean;
  connection_name?: string;
  scope_label?: string;
  destination: IntegrationDestination;
  destination_label?: string;
  /** Where sends go until the app's people choose. Named rather than reported
   * as an absence: "posting to #admin" is actionable, "no channel set" reads
   * like a broken integration. */
  default_destination: IntegrationDestination;
  enumerates: boolean;
  /** `nothing` is the token action: the app asks and is handed something. */
  actions: { name: string; carries: "text" | "file" | "nothing"; summary: string }[];
  leaves_residue: boolean;
  /** The attached connection lets this app get its own access token. */
  issues_tokens: boolean;
}

export interface AppIntegrations {
  integrations: AppIntegrationRow[];
  /** The same list `/app/self` gives the app, so the tab and the running code
   * cannot disagree about what is switched on. */
  capabilities: string[];
  /** Where mail replies go while nobody has chosen — the app owner's own
   * address, so the placeholder can say what leaving it blank means. */
  reply_to_fallback: string;
  reply_to_fallback_kind: "owner" | "install" | "";
}

export interface SendAttachment {
  name: string;
  bytes: number;
  type: string;
  sha256: string;
}

/** The ledger is its own table, not `app_events` — that feed is *what broke*,
 * and successful sends would drown it. The row is written before the send is
 * attempted: no status is ever a lie. */
export interface IntegrationSend {
  id: string;
  app_id: string;
  /** The catalog's name for the integration. The column was `integration`; the
   * one place that word survives is `/app/sends/{id}`, which is published. */
  kind: string;
  action: string;
  status: string; // queued | sent | failed | blocked | abandoned
  destination?: string;
  recipients: string[];
  subject?: string;
  body_bytes: number;
  body_sha256?: string;
  /** Stored only when the install retains bodies; the hash alone is enough for
   * almost every install. */
  body?: string;
  attachments: SendAttachment[];
  idempotency_key?: string;
  deployment_id?: string;
  error?: string;
  /** The object this send left behind, where it left one. A row that names one
   * is exempt from the ledger's retention delete: the message is chatter and
   * expires, the fact that this app placed this object is not. */
  placement_id?: string;
  created_at: string;
  completed_at?: string;
  /** Filled by the install-wide listing, where the question is whose send it is. */
  app_slug?: string;
  app_name?: string;
}

/** The ledger's own settings, and only the ledger's. The per-kind switches are
 * the connections themselves: one that exists with `for_apps` set *is* the
 * install switch for its kind. */
export interface IntegrationSettings {
  /** Stores the message body beside its hash. Off by default. */
  retain_bodies: boolean;
  retention_days: number;
}

/* ── Dependency policy (ADR §39) ─────────────────────────────────────────── */

/**
 * Which third-party packages this install will run.
 *
 * `warn` is the mode that makes `block` adoptable: it resolves, records and
 * shows everything and fails no build. An empty middle state would be
 * ambiguous, so the middle state has a name.
 */
export type DependencyMode = "off" | "warn" | "block";

/** The four levels, plus the empty string for an advisory nobody scored. */
export type DependencySeverity = "critical" | "high" | "moderate" | "low" | "";

/**
 * One advisory matched against one installed package.
 *
 * `would_block` is named for what it means: it is true in `warn` mode, where
 * nothing is refused. That is the point — it is what lets the tab and the
 * preview say "this would block" before anybody switches the mode. The
 * install-level answer is `enforcing`, which travels beside it, and rendering
 * `would_block` as "blocked" without checking it is the mistake this name
 * exists to prevent.
 */
export interface DependencyFinding {
  advisory_id: string;
  kind: "vuln" | "malware";
  severity: DependencySeverity;
  summary?: string;
  url?: string;

  ecosystem: string;
  package: string;
  version: string;
  /** Empty when no fix has been published, which is what makes "update the
   *  dependency" the advice that wastes an afternoon. */
  fixed_in?: string;
  /** "(this app) > webpack > lodash". Not decoration: it is the only thing
   *  that says which of *their* dependencies an owner can actually bump. */
  path?: string;
  direct: boolean;
  dev: boolean;

  would_block: boolean;
  waived: boolean;
  waived_by?: string;
  waived_reason?: string;
  waiver_expires_at?: string;
}

/**
 * The tone-bearing summary.
 *
 * `counted` is the one number a badge shows — malware plus anything at or above
 * the install's threshold, minus waived. Never `total`: a tab reading 31 on
 * every app conveys nothing but that Node exists.
 */
export interface DependencyCounts {
  total: number;
  malware: number;
  blocking: number;
  waived: number;
  counted: number;
  /** Advisories nobody scored, counted apart so a feed degrading into unscored
   *  records is visible rather than read as an improvement. */
  unknown: number;
  by_severity: Record<string, number>;
}

export interface DependencyPackage {
  ecosystem: string;
  package: string;
  version: string;
  direct: boolean;
  dev: boolean;
  path?: string;
}

/** An accepted risk, with an expiry and somebody's name on it. */
export interface DependencyWaiver {
  id: string;
  app_id: string;
  app_slug?: string;
  advisory_id: string;
  package?: string;
  reason: string;
  expires_at: string;
  created_by?: string;
  created_at: string;
  created_by_name?: string;
  severity?: string;
  summary?: string;
}

export interface DependencyRelease {
  deployment_id: string;
  commit_sha?: string;
  at?: string;
  findings: DependencyFinding[];
  counts: DependencyCounts;
  inventory: DependencyPackage[];
  ecosystems?: string[];
  direct: number;
}

/** A deploy this policy turned away. Kept apart from what is serving: after a
 *  blocked deploy the app is still on release 46 while 47 was refused, and
 *  conflating them has the developer fix the wrong package. */
export interface DependencyRefusedRelease {
  deployment_id: string;
  commit_sha?: string;
  at?: string;
  reason: string;
  findings: DependencyFinding[];
}

/**
 * One reading of one app: what the sweep saw, and when.
 *
 * A summary plus the notable advisory ids, never a copy of the findings. It is
 * written whether or not anything changed, because "checked, unchanged for
 * thirty days" and "not checked for thirty days" are different answers and a
 * table that only recorded changes could not tell them apart.
 */
export interface DependencyScan {
  id: string;
  app_id: string;
  deployment_id?: string;
  /** Why the reading was taken. Only a scheduled one can lock. */
  trigger: "deploy" | "scheduled" | "manual";
  occurred_at: string;
  /** Coverage, not "an inventory exists". False is *unknown* and must never
   *  render as "no findings". */
  scanned: boolean;
  feed_as_of?: string;
  findings: number;
  counted: number;
  malware: number;
  critical: number;
  high: number;
  moderate: number;
  low: number;
  unknown: number;
  waived: number;
  packages: number;
  advisories: string[];
}

/** One app's readings, newest first. Administrator-only: the owner gets the
 *  latest reading on their own tab, and a *stack* of readings answers a
 *  different question — how long this sat, and whether anyone acted. */
export interface DependencyScanHistory {
  app_id: string;
  app_slug: string;
  /** The install's retention count, so the panel can say "the last 30" rather
   *  than implying the list is everything that ever happened. */
  kept: number;
  total: number;
  scans: DependencyScan[];
  lock_on: DependencyLockOn;
  locked_by_policy: boolean;
}

/** The threshold at which this install takes a serving app off the air by
 *  itself. There is no moderate and no low. */
export type DependencyLockOn = "off" | "malware" | "critical" | "high";

export interface DependencyState {
  mode: DependencyMode;
  enforcing: boolean;
  available: boolean;
  /** Which of the four gates refused, when one did. */
  reason?: string;

  feed_as_of?: string;
  feed_stale: boolean;
  has_data: boolean;

  scanned: boolean;
  /** See AppDependencyPolicy.scan_reason: "never_scanned",
   *  "not_scanned_by_policy", "no_dependencies", "not_enumerable",
   *  "not_covered", "no_corpus", or "unresolved". */
  scan_reason?: string;
  scanned_at?: string;
  /** False means these figures will not move on their own, and the tab says so
   *  beside Check again rather than showing a number that quietly stopped. */
  watch_enabled: boolean;

  serving?: DependencyRelease;
  refused?: DependencyRefusedRelease;

  waivers: DependencyWaiver[];
  can_waive: boolean;
  /** False when the install's cap is 0 — waivers off. The copy must not send a
   *  developer to an administrator who has no button. */
  waivers_available: boolean;
  /** The cap itself, so the grant dialog refuses an out-of-range expiry in the
   *  same words the server would and names the number. */
  waiver_max_days: number;

  /** The most recent reading, and **one object, never a list**. Absent means
   *  never checked, which is not zero findings. */
  last_scan?: DependencyScan;
  /** This app is being held down by the policy itself rather than by an
   *  administrator, so the way back is a waiver rather than an unlock. */
  locked_by_policy: boolean;
  lock_on: DependencyLockOn;
}

export interface AdvisoryFeedState {
  ecosystem: string;
  endpoint?: string;
  last_sync_at?: string;
  last_success_at?: string;
  last_error?: string;
  advisory_count: number;
  /** "sync" or "import". An air-gapped install carries snapshots in by hand,
   *  and the page names the command rather than offering a dead button. */
  origin: string;
  /** The snapshot's own age, never when it was loaded. */
  snapshot_at?: string;
  /** Whether an *absence* of findings in this ecosystem is a verdict. An
   *  uncovered corpus is matched and displayed and never counted as scanned,
   *  and its age is left out of the install's staleness figure. */
  covered: boolean;
}

export interface AdminDependencyApp {
  app_id: string;
  slug: string;
  name: string;
  owner_id: string;
  owner_name?: string;
  status: string;
  deployment_id?: string;
  scanned: boolean;
  scanned_at?: string;
  packages: number;
  baseline_at?: string;
  /** When the sweep last took a reading. Absent is *never checked*, which is
   *  not the same fact as checked and found nothing. Distinct from
   *  `scanned_at`, which is when the *release* was judged at deploy. */
  last_scan_at?: string;
  locked_at?: string;
  locked_source?: "admin" | "dependency";
  counts: DependencyCounts;
  /** False for a stopped app. It is not exposure, and folding it into the
   *  headline count inflates the thing a security review reads. A *sleeping*
   *  app is exposure — it wakes on the next request — and counts normally. */
  serving: boolean;
}

export interface AdminDependencyState {
  mode: DependencyMode;
  enforcing: boolean;
  available: boolean;
  reason?: string;

  feeds: AdvisoryFeedState[];
  feed_as_of?: string;
  feed_stale: boolean;
  /** False on an air-gapped install, where the answer is
   *  `toft advisory-import` rather than a button that cannot work. */
  can_sync: boolean;
  /** The *corpus*, not this estate: how many advisories the install holds. */
  corpus_size: number;
  malware_records: number;

  apps: AdminDependencyApp[];
  /** The threshold this install takes a serving app off the air at, and what
   *  that has actually done. The estate-wide consequence of one switch. */
  lock_on: DependencyLockOn;
  locked_apps: AdminDependencyApp[];
  /** The same findings grouped the other way, and the page's default axis:
   *  "how bad is this news" is the question an administrator arrives with,
   *  while the by-app table is already forty rows on day one. */
  advisories: AdvisoryGroup[];
  waivers: DependencyWaiver[];
  credential_set: boolean;
  /** …and whether that stored value can still be decrypted. `set` with
   * `unreadable` is a changed or lost ENCRYPTION_KEY: the form's "leave blank
   * to keep" would keep something unusable, so the field has to say so. */
  credential_unreadable: boolean;
}

/** One advisory and the apps under it. */
export interface AdvisoryGroup {
  advisory_id: string;
  kind: "vuln" | "malware";
  severity: DependencySeverity;
  summary?: string;
  url?: string;
  apps: {
    app_id: string;
    slug: string;
    name: string;
    package: string;
    version: string;
    fixed_in?: string;
    waived: boolean;
    serving: boolean;
    /** At or above this install's threshold, unwaived — what puts the pair on
     *  the page's *Needs a decision* list. The policy's own answer. */
    counted: boolean;
    /** Refuses the next deploy under Refuse (mode-independent, unwaived). */
    would_block: boolean;
  }[];
  /** Counted apart, never summed: a stopped app is not exposure. */
  serving: number;
  stopped: number;
  waived: number;
}

export interface DependencySearchResult {
  query: string;
  apps: {
    app_id: string;
    slug: string;
    name: string;
    status: string;
    deployment_id: string;
    findings: DependencyFinding[];
  }[];
  /** Counted apart, never summed. */
  serving: number;
  stopped: number;
}

/**
 * One (app, package, version) the estate is carrying, affected or not.
 *
 * A row is an occurrence rather than an app, because one app can carry the same
 * package at two versions through two paths and a governance answer has to show
 * both.
 */
export interface PackageOccurrence {
  app_id: string;
  slug: string;
  name: string;
  status: string;
  deployment_id: string;
  ecosystem: string;
  package: string;
  version: string;
  /** "app > webpack > lodash" — what settles "we don't depend on that". */
  path: string;
  direct: boolean;
  dev: boolean;
}

/** One (ecosystem, package) across the estate, with both counts. */
export interface PackageCatalogEntry {
  ecosystem: string;
  package: string;
  /** Apps is the exposure number; occurrences is the fragmentation one. */
  apps: number;
  occurrences: number;
  versions: string[];
  newest?: string;
  oldest?: string;
  /** Versions no ordering can place, so an unknown spread is not an absent one. */
  incomparable: number;
}

/**
 * The governance read: what this estate has, rather than what is wrong with it.
 *
 * `apps_total` / `apps_scanned` are on every response and the UI leads with
 * them — a search result feels exhaustive, and unscanned is not zero.
 */
export interface PackageSearchResult {
  mode: "search" | "catalog";
  query: string;
  available: boolean;
  reason?: string;
  occurrences: PackageOccurrence[];
  /** Never dropped and never counted as matches: git URLs, file: paths, local tags. */
  incomparable: PackageOccurrence[];
  packages: PackageCatalogEntry[];
  apps_total: number;
  apps_scanned: number;
}

/** The version half of the predicate, in advisory.Range's own field names. */
export interface PackageVersionFilter {
  version?: string;
  introduced?: string;
  fixed?: string;
  last_affected?: string;
}

/**
 * The estate census: what this install's apps are written in.
 *
 * A different question from the framework count on `/tags/framework`, which is
 * viewer-scoped and answers "show me the Flask apps I can see". This is the
 * governance reader's number, and `apps_total` / `apps_versioned` ride on it
 * for the reason `apps_scanned` rides on a package search: a census returns a
 * total, and a total feels exhaustive.
 */
export interface EstateCensus {
  languages: EstateLanguage[];
  apps_total: number;
  /** How many have a recorded runtime version. Never the same as the total. */
  apps_versioned: number;
  /** Locked and retired apps, counted in every total above and said once here. */
  apps_locked: number;
  /** The estate by what answers a request; the three sum to `apps_total`. */
  kinds: { serving: number; rendered: number; batch: number };
  /** Apps of any kind that declare at least one job — overlaps `kinds`. */
  apps_with_jobs: number;
  /** Whether the mode columns mean anything on this install. */
  show_modes: boolean;
}

/**
 * The estate's scan targets — every app as something to visit.
 *
 * The coverage rides on it for the reason the census's does: a list of
 * addresses feels exhaustive. **Nothing is ever omitted** — an app that has
 * never been deployed is a row with a status and no `url` — so what the
 * coverage reports is how many rows have an address, never how many apps were
 * skipped.
 */
export interface EstateTargets {
  targets: EstateTarget[];
  apps_total: number;
  /** How many carry a `url`. Never larger than `apps_total`. */
  apps_addressable: number;
  /** Locked apps. Listed among the targets and counted here — the reader is
   *  complete, and skipping what it is told to skip is the scanner's job. */
  apps_locked: number;
  /** Where deployed apps are served from, said once so an install with no
   *  apps yet still says what the addresses would look like. */
  apps_origin: string;
}

/* ── What an app can reach ────────────────────────────────────────────────
 *
 * The assembly, and the estate's transpose of it. Names, never values: no
 * credential value crosses this boundary, and the reader is deliberately
 * incapable of carrying one.
 */

/** One principal who may open the app, at one rung. The owner is not here —
 *  ownership is a column, not a grant. */
export interface ReachGrant {
  principal_type: string;
  principal_id: string;
  name: string;
  role: string;
}

/** A bag of values an app's owner never holds. `keys` are the value *names*
 *  inside it, which is what says whether a rotation touches this app. */
export interface ReachCredential {
  id: string;
  name: string;
  keys: string[];
}

/** An environment variable the owner set, by name. A sensitive value is
 *  write-only and a plain one is on the Settings tab; neither is here. */
export interface ReachEnvKey {
  key: string;
  sensitive: boolean;
}

/** One durable location the app has, at one level. */
export interface ReachMount {
  resource_id: string;
  resource_slug: string;
  resource_name: string;
  /** `volume` or `store`. The access ladder is not symmetric between the two,
   *  so the same word means different things and a label without the kind
   *  would be wrong for one of them. */
  resource_kind: string;
  path: string;
  access: string;
  folder?: string;
  /** Which delegation wrote the mapping — `admin`, `user:<id>` or
   *  `group:<id>`. "Who gave this app write access to the finance share" is
   *  the second question anybody asks after seeing the first. */
  authority: string;
  resource_missing?: boolean;
}

/** One channel the app may send on, and the three booleans that decide whether
 *  it actually can. Present-and-not-effective is never reported as absent: an
 *  attachment an install has switched off is one an administrator has to
 *  re-examine when they switch it back on. */
export interface ReachIntegration {
  kind: string;
  destination?: string;
  connected: boolean;
  attached: boolean;
  enabled: boolean;
}

/** The app's own JSON document, described and never read. */
export interface ReachData {
  bytes: number;
  version: number;
  updated_at: string;
}

/** Somebody the app's re-render mails on a timer. `enabled` is the schedule's
 *  switch: a suspended schedule still names who it would mail. */
export interface ReachRecipient {
  schedule_id: string;
  user_id: string;
  name: string;
  email: string;
  enabled: boolean;
}

export interface AppReach {
  app_id: string;
  slug: string;
  name: string;
  /** The **effective** visibility: `public` becomes `authenticated` when this
   *  install has withdrawn anonymous access, and `visibility_withheld` says
   *  so. */
  visibility: string;
  visibility_withheld?: boolean;
  listable: boolean;
  grants: ReachGrant[];
  /** Who may frame it. Empty is the install's own answer, which is not
   *  "anyone". */
  frame_ancestors: string;
  viewer_identity: boolean;
  owner?: { kind: string; id: string; name: string };
  credentials: ReachCredential[];
  env_keys: ReachEnvKey[];
  mounts: ReachMount[];
  integrations: ReachIntegration[];
  data?: ReachData;
  report_recipients: ReachRecipient[];
  /** An install that has provisioned no storage, or offered no integration to
   *  apps, gets no section rather than a section of zeros. Absent, not off. */
  show_storage: boolean;
  show_integrations: boolean;
  /** The sentence this page is not allowed to omit, served from the API so
   *  both surfaces say the same thing and neither can quietly drop it. */
  cannot_see: string;
}

/** One app on the estate page: what it holds, as names and counts. The full
 *  assembly stays on the app — a payload carrying every fact about every app is
 *  one nobody renders and somebody eventually leaks. */
export interface EstateReachRow {
  app_id: string;
  slug: string;
  name: string;
  visibility: string;
  visibility_withheld?: boolean;
  listable: boolean;
  locked: boolean;
  owner?: string;
  owner_kind?: "user" | "group";
  owner_id?: string;
  owner_group_id?: string;
  frame_ancestors?: string;
  viewer_identity: boolean;
  credentials: { id: string; name: string }[];
  mounts: {
    resource_id: string;
    resource_slug: string;
    resource_name: string;
    resource_kind: string;
    path: string;
    access: string;
  }[];
  integrations: ReachIntegration[];
  env_keys: number;
  grants: number;
  report_recipients: number;
  has_data: boolean;
}

/** ADR §93. The catalog's own account of itself, summed from the rows rather
 *  than written down — a coverage figure a human maintains is one that goes
 *  stale, and this one is quoted at an assessor. */
export interface ComplianceCoverage {
  requirements: number;
  families: number;
  /** platform / shared / inherited / customer. The last two are what make the
   *  first one credible, and the page leads with all four for that reason. */
  by_responsibility: Record<string, number>;
  evidence: number;
  evidence_by_credential: Record<string, number>;
  odps: number;
  odps_bound: number;
  odps_noted: number;
  /** Shown, never hidden: a coverage figure you cannot see is one you cannot
   *  trust. */
  odps_unbound: number;
}

export interface ComplianceFamily {
  code: string;
  title: string;
}

export interface ComplianceSummary {
  catalog: string;
  oscal_version: string;
  families: ComplianceFamily[];
  coverage: ComplianceCoverage;
}

export interface ComplianceStatement {
  label?: string;
  prose: string;
  depth: number;
}

export interface ComplianceODP {
  id: string;
  label?: string;
  usage?: string;
  guideline?: string;
}

export interface ComplianceRequirement {
  id: string;
  family: string;
  family_title: string;
  title: string;
  statement: ComplianceStatement[];
  odps: ComplianceODP[];
  guidance?: string;
  objectives?: string[];
  /** platform | shared | inherited | customer */
  responsibility: string;
  summary: string;
  mechanism?: string;
  customer?: string;
  /** References into this repository's own documentation. A citation that stops
   *  resolving fails the build, which is the only half of staleness a test can
   *  catch. */
  refs?: string[];
  evidence?: string[];
}

export interface ComplianceEvidenceDef {
  id: string;
  label: string;
  credential: string;
  source: { kind?: string; route?: string; fields?: string[] };
  screen: { path?: string; anchor?: string; label?: string };
  reading?: string;
}

export interface ComplianceControls {
  catalog: string;
  oscal_version: string;
  families: ComplianceFamily[];
  requirements: ComplianceRequirement[];
  evidence: ComplianceEvidenceDef[];
  coverage: ComplianceCoverage;
}

/** One evidence item, read. `absent` is a capability this install does not
 *  offer and is never a failure; `refused` and `error` stay apart because an
 *  access boundary and an outage are different facts. */
export interface ComplianceResult {
  id: string;
  label: string;
  credential: string;
  route?: string;
  screen?: string;
  reading?: string;
  outcome: "ok" | "absent" | "refused" | "error";
  reason?: string;
  value?: unknown;
  taken_at: string;
}

export interface ComplianceReport {
  collected_at: string;
  install_id?: string;
  version?: string;
  hostname?: string;
  collected_by?: string;
  catalog: string;
  oscal_version: string;
  results: ComplianceResult[];
  by_outcome: Record<string, number>;
  coverage: ComplianceCoverage;
}

export interface EstateReach {
  rows: EstateReachRow[];
  apps_total: number;
  /** How many hold anything at all beyond who may open them. Sharing and
   *  framing are excluded: every app has an answer to those, so counting them
   *  would make this equal `apps_total` and say nothing. */
  apps_with_reach: number;
  apps_public: number;
  apps_writable_storage: number;
  show_storage: boolean;
  show_integrations: boolean;
  cannot_see: string;
}

export interface EstateTarget {
  app_id: string;
  slug: string;
  name: string;
  /** Empty for an app that has never been deployed: there is nothing at that
   *  address to scan, and the row exists to say so. */
  url?: string;
  /** The **effective** visibility. `public` becomes `authenticated` when this
   *  install has withdrawn anonymous access, and `visibility_withheld` says
   *  so. */
  visibility: string;
  visibility_withheld?: boolean;
  /** A display name, for a reader that is not this console. The ids beside it
   *  are what `AppOwner` needs to draw either kind of owner. */
  owner?: string;
  /** A person owner's login, which is how the console names an owner. */
  owner_login?: string;
  owner_kind?: "user" | "group";
  owner_id?: string;
  owner_group_id?: string;
  framework: string;
  /** The mode the serving release was actually started under; empty for an app
   *  that has never been deployed. */
  execution_mode?: string;
  status: string;
  current_release?: string;
  last_deployed_at?: string;
  locked: boolean;
}

export interface EstateLanguage {
  /** "python" | "node" | "go" | "static", or "" for an app the detector could
   *  not place. Read `label` for the word. */
  language: string;
  label: string;
  apps: number;
  shared: number;
  isolated: number;
  series: EstateSeries[];
}

/** One release line — 3.13, 22, 1.25 — or one of the two wordless states. */
export interface EstateSeries {
  /** Empty unless `state` is "resolved". */
  series: string;
  state: EstateVersionState;
  apps: number;
  shared: number;
  isolated: number;
  frameworks: EstateFramework[];
}

/**
 * Absent is a state with a name, and it never reads as zero.
 *
 * `not_applicable` is a static app, which resolves no interpreter because it
 * runs none. `unresolved` is a language with no recorded version — Go left to
 * the go command, and a release predating the columns, which are
 * indistinguishable in the data and so are not pretended apart here.
 */
export type EstateVersionState = "resolved" | "unresolved" | "not_applicable";

export interface EstateFramework {
  framework: string;
  apps: number;
}

/** One section of an app's audit file, as the install report's evidence rows are. */
export interface AppAuditFileSection {
  id: string;
  label: string;
  outcome: "ok" | "absent" | "refused" | "error";
  reason?: string;
  value?: unknown;
  taken_at: string;
}

/** An app's audit file. */
export interface AppAuditFile {
  collected_at: string;
  app: { id: string; slug: string; name: string };
  producer: { principal_id: string; credential: string; key_id?: string };
  sections: AppAuditFileSection[];
  by_outcome: Record<string, number>;
}

/** GET /users/{id}/app-usage. */
export interface UserAppUsage {
  days: number;
  since: string;
  until: string;
  apps: {
    app_id: string;
    slug: string;
    name: string;
    requests: number;
    /** Distinct UTC days with traffic. */
    days: number;
    first_day: string;
    last_day: string;
  }[];
  /** The oldest day the install still keeps, "" when it keeps everything. */
  retained_from: string;
}

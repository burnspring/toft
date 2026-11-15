/* ------------------------------------------------------------------ *
 * The Settings page's form contract
 *
 * Which keys that page renders, as what control, and the bounds each is
 * held to — mirrored from `internal/database/settings.go` and
 * `settings_handlers.go`. A form that accepts what the API refuses is a
 * round trip spent learning a rule the page already knew, so the message
 * a field shows is the message the server would have sent.
 *
 * Beside `settings-index.ts` rather than inside the page, because two
 * things need it and one of them is not a component: the page draws the
 * rows from it, and the index's guard test holds `SETTINGS_PAGE_KEYS`
 * against the entries the index says that page owns. A setting in one
 * and not the other fails the test.
 * ------------------------------------------------------------------ */

import { zoneFormattable } from "@/lib/time";

/** The numbers, each saved through the pending-changes bar. */
export type NumberFieldKey =
  | "app_memory_limit_mb"
  | "app_cpu_request_millicores"
  | "app_cpu_limit_millicores"
  | "max_concurrent_builds"
  | "auto_deploy_poll_seconds"
  | "job_concurrency_per_app"
  | "job_log_retention_days"
  | "job_run_retention_days"
  | "idle_sleep_minutes"
  | "isolated_idle_sleep_minutes"
  | "max_versions_per_app"
  | "default_job_timeout_seconds"
  | "max_job_timeout_seconds"
  | "job_memory_limit_mb"
  | "job_cpu_limit_millicores"
  | "max_queued_runs_per_app"
  | "notebook_render_timeout_seconds"
  | "report_delivery_min_interval_minutes"
  | "build_timeout_seconds"
  | "app_event_retention_days"
  | "platform_log_retention_days"
  | "announcement_recipient_retention_days"
  | "access_record_retention_days"
  | "dormant_lock_days"
  | "api_key_max_lifetime_days"
  | "session_absolute_hours"
  | "session_idle_minutes";

/** The free-text ones, saved the same way. */
export type TextFieldKey =
  | "public_base_url"
  | "garth_url"
  | "environment_tag"
  | "install_name"
  | "display_time_zone"
  | "security_txt_contact"
  | "security_txt_expires"
  | "security_txt_policy"
  | "security_txt_languages"
  | "r_package_repository"
  | "isolated_sleep_window_start"
  | "isolated_sleep_window_end";

export type FieldKey = NumberFieldKey | TextFieldKey;

/** The switches, wherever they are drawn. Never part of a pending set: each
 *  saves the moment it is thrown. */
export type SwitchKey =
  | "isolated_execution_enabled"
  | "background_jobs_enabled"
  | "api_keys_enabled"
  | "dormant_lock_enabled"
  | "app_security_headers"
  | "app_log_copy_uncapped"
  | "isolated_sleep_window_enabled";

/** The settings that are a choice rather than a switch or a number. They save
 *  like a switch: a single deliberate act whose consequence the server answers
 *  with, not one of six edits batched behind a bar. */
export type ChoiceKey =
  "gallery_deploy_mode" | "dependency_policy" | "default_execution_mode" | "log_level";

/** Every platform setting this UI draws, wherever it draws it. */
export type SettingId = FieldKey | SwitchKey | ChoiceKey;

/**
 * The subset the Settings page itself renders.
 *
 * Narrower than `SettingId` since ADR §50 D2 sent six sections to the pages
 * that show what they govern: the dormancy pair is on Authentication, the
 * personal-keys switch is on API keys, the dependency mode is on Dependencies.
 * Their bounds and validation stay here, because the editors that took them
 * still need the numbers to agree with the server's.
 */
export type SettingsPageKey =
  | Exclude<
      FieldKey,
      | "dormant_lock_days"
      | "api_key_max_lifetime_days"
      | "session_absolute_hours"
      | "session_idle_minutes"
    >
  | Exclude<SwitchKey, "api_keys_enabled" | "dormant_lock_enabled">
  | Exclude<ChoiceKey, "dependency_policy">;

/**
 * Which control each setting on this page is drawn as.
 *
 * Exhaustive by type — a `Record<SettingId, …>` cannot be missing a key — and
 * it is what the rows are generated from, so a setting cannot be in the index,
 * carry its copy, appear in the filter's count, and then be rendered by
 * nothing. That was not hypothetical: a retries ceiling was once in the index
 * and in the Execution section's list, and no row for it was ever drawn. The guard
 * test found it the first time it ran, which is the argument for both.
 */
/** "memory" is a number with a unit beside it. One setting is drawn that way,
 *  because megabytes is the right unit for a 256 MB cap and gigabytes for a
 *  16 GB one, and a page that offered only one would be asking an
 *  administrator to do the arithmetic in their head. */
/** "cpu" is a number of millicores — what the row holds and the backends count
 *  in — drawn and typed as cores, the unit an app's own Settings tab uses, so
 *  one install-wide figure and its per-app override are read the same way. */
export type ControlKind = "switch" | "number" | "memory" | "cpu" | "text" | "choice";

export const CONTROL_KIND: Record<SettingsPageKey, ControlKind> = {
  public_base_url: "text",
  garth_url: "text",
  environment_tag: "text",
  install_name: "text",
  display_time_zone: "text",

  security_txt_contact: "text",
  security_txt_expires: "text",
  security_txt_policy: "text",
  security_txt_languages: "text",
  app_security_headers: "switch",
  app_log_copy_uncapped: "switch",

  isolated_execution_enabled: "switch",
  default_execution_mode: "choice",
  background_jobs_enabled: "switch",

  gallery_deploy_mode: "choice",

  app_memory_limit_mb: "memory",
  app_cpu_request_millicores: "cpu",
  app_cpu_limit_millicores: "cpu",
  max_concurrent_builds: "number",
  auto_deploy_poll_seconds: "number",
  r_package_repository: "text",
  log_level: "choice",
  job_concurrency_per_app: "number",
  default_job_timeout_seconds: "number",
  max_job_timeout_seconds: "number",
  job_memory_limit_mb: "memory",
  job_cpu_limit_millicores: "cpu",
  max_queued_runs_per_app: "number",
  notebook_render_timeout_seconds: "number",
  report_delivery_min_interval_minutes: "number",
  build_timeout_seconds: "number",
  job_log_retention_days: "number",
  job_run_retention_days: "number",
  idle_sleep_minutes: "number",
  isolated_idle_sleep_minutes: "number",
  isolated_sleep_window_enabled: "switch",
  isolated_sleep_window_start: "text",
  isolated_sleep_window_end: "text",
  max_versions_per_app: "number",
  app_event_retention_days: "number",
  platform_log_retention_days: "number",
  announcement_recipient_retention_days: "number",
  access_record_retention_days: "number",
};

/** What that page renders, for the guard test to hold against the index. */
export const SETTINGS_PAGE_KEYS = Object.keys(
  CONTROL_KIND,
) as SettingsPageKey[];

export const NUMBER_FIELDS: Record<
  NumberFieldKey,
  {
    unit?: string;
    width?: string;
    min: number;
    max: number;
    zeroAllowed?: boolean;
  }
> = {
  // zeroAllowed, and 0 does not mean "no limit" here — it hands the number
  // back to the install's own APP_MEMORY_LIMIT_MB / APP_MEMORY_LIMIT_GB. The
  // field's own copy has to say so, because every other zeroAllowed field on
  // this page means "off".
  //
  // Stated in megabytes because that is what the row holds and what both
  // backends count in; the control draws a unit beside it and converts, so an
  // administrator may type 16 GB without working out 16384.
  app_memory_limit_mb: {
    unit: "MB",
    min: 64,
    max: 1048576,
    zeroAllowed: true,
  },
  // Millicores, and zeroAllowed for the reason the ceiling above is: 0 hands
  // the number back to the install's own figure rather than reserving nothing.
  // The request's floor is 1m because a reservation the scheduler rounds to
  // nothing is a reservation nobody made.
  app_cpu_request_millicores: { unit: "cores", min: 1, max: 64000, zeroAllowed: true },
  app_cpu_limit_millicores: { unit: "cores", min: 50, max: 64000, zeroAllowed: true },
  // Bounded above by the pool this platform's workers were started as, which
  // the page reads from the settings document rather than assuming.
  max_concurrent_builds: { unit: "builds", min: 1, max: 64, zeroAllowed: true },
  auto_deploy_poll_seconds: { unit: "seconds", min: 10, max: 86400, zeroAllowed: true },
  job_concurrency_per_app: { min: 1, max: 100 },
  // The job limits. None of them is ever off: a job always has a deadline and
  // a memory limit, and an app's queue always has a bottom. The two job
  // defaults take 0 for the environment's figure, like the app dials.
  default_job_timeout_seconds: { unit: "seconds", min: 60, max: 604800 },
  max_job_timeout_seconds: { unit: "seconds", min: 60, max: 604800 },
  job_memory_limit_mb: { unit: "MB", min: 64, max: 1048576, zeroAllowed: true },
  job_cpu_limit_millicores: { unit: "cores", min: 50, max: 64000, zeroAllowed: true },
  max_queued_runs_per_app: { unit: "runs", min: 1, max: 10000 },
  // No zeroAllowed: there is no value of this that means "no limit". A render
  // with no clock on it is a build queue with no clock on it.
  notebook_render_timeout_seconds: { unit: "seconds", min: 60, max: 3600 },
  // 0 is no floor at all — an install that has decided its relay can take a
  // minutely report — which the range already holds, so no zeroAllowed.
  report_delivery_min_interval_minutes: { unit: "minutes", min: 0, max: 1440 },
  build_timeout_seconds: { unit: "seconds", min: 60, max: 3600 },
  job_log_retention_days: { unit: "days", min: 1, max: 365 },
  job_run_retention_days: { unit: "days", min: 1, max: 3650 },
  idle_sleep_minutes: {
    unit: "minutes",
    min: 5,
    max: 10080,
    zeroAllowed: true,
  },
  isolated_idle_sleep_minutes: {
    unit: "minutes",
    min: 5,
    max: 10080,
    zeroAllowed: true,
  },
  max_versions_per_app: {
    unit: "deployments",
    min: 2,
    max: 100,
    zeroAllowed: true,
  },
  app_event_retention_days: { unit: "days", min: 7, max: 3650 },
  platform_log_retention_days: { unit: "days", min: 7, max: 3650 },
  // No zeroAllowed: there is no value of this that means "keep nothing". The
  // announcement and its counts are never pruned either way, so a zero would
  // only be a way of asking for a list nobody can read.
  announcement_recipient_retention_days: { unit: "days", min: 30, max: 3650 },
  // Zero keeps everything and is the default: the one dial here that deletes
  // the audit trail, so its resting state is the one that deletes nothing.
  access_record_retention_days: { unit: "days", min: 365, max: 3650, zeroAllowed: true },
  // No zeroAllowed, unlike idle sleep and the version cap: off is the switch
  // above it, and a control that locks accounts should not be reachable by
  // clearing a field.
  dormant_lock_days: { unit: "days", min: 30, max: 730 },
  // On API keys beside the switch, not on Settings. 0 is no cap — the default,
  // and what `toft login` and RStudio's keys then get: no end date.
  api_key_max_lifetime_days: { unit: "days", min: 1, max: 3650, zeroAllowed: true },
  // The session policy, and the pair is the argument for the two flags being
  // separate. There is no value of the lifetime that turns sessions off, so no
  // zeroAllowed; the idle timeout's zero is the whole escape hatch, so it has
  // one.
  session_absolute_hours: { unit: "hours", min: 1, max: 24 },
  session_idle_minutes: {
    unit: "minutes",
    min: 5,
    max: 480,
    zeroAllowed: true,
  },
};

export const NUMBER_KEYS = Object.keys(NUMBER_FIELDS) as NumberFieldKey[];
export const TEXT_KEYS: TextFieldKey[] = [
  "public_base_url",
  "garth_url",
  "environment_tag",
  "install_name",
  "display_time_zone",
  "security_txt_contact",
  "security_txt_expires",
  "security_txt_policy",
  "security_txt_languages",
  "r_package_repository",
];

// A set rather than a chain of `||`, which is what the two-key version was and
// what six of them would have turned into.
const TEXT_KEY_SET = new Set<string>(TEXT_KEYS);

export function isTextKey(key: FieldKey): key is TextFieldKey {
  return TEXT_KEY_SET.has(key);
}

// Mirrors NormalizePublicBaseURL on the server: an address only — scheme and
// host, nothing after it. Every caller appends its own path, so a stored one
// would be doubled in the link.
const PUBLIC_BASE_URL_RE = /^https?:\/\/[^/?#\s]+$/i;
const GARTH_URL_RE = /^https:\/\/[^/?#\s@]+(\/[^?#\s]*)?$/i;

// Mirrors the server's rule: 1–32 lowercase letters, digits, dashes, or
// underscores, starting and ending alphanumeric — safe as a filename component.
const ENV_NAME_RE = /^[a-z0-9]([a-z0-9_-]{0,30}[a-z0-9])?$/;

// Mirrors NormalizeInstallName: 60 characters, one line, no < or >, and nothing
// that reads as an address — a scheme, www., or a dotted host with a TLD-like
// suffix — since every footer prints the name beside the real one.
export const INSTALL_NAME_LIMIT = 60;
// eslint-disable-next-line no-control-regex
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/;
const URLISH_RE = /(^|[^a-z0-9-])[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,63}($|[^a-z0-9.-])/i;

/** The install's display zone (ADR §154): empty for each viewer's browser, or a
 *  zone. The server's `NormalizeTimeZone` is the authority; this catches what
 *  is plainly not one before the round trip. */
export function displayTimeZoneError(raw: string): string | null {
  const v = raw.trim();
  if (v === "" || /^utc$/i.test(v)) return null;
  if (/^(local|browser)$/i.test(v)) return "Leave it blank for each person’s own browser zone.";
  return zoneFormattable(v) ? null : "Not a time zone name — try one like America/New_York.";
}

export function installNameError(raw: string): string | null {
  const v = raw.trim();
  if (v === "") return null;
  if ([...v].length > INSTALL_NAME_LIMIT) return `Use ${INSTALL_NAME_LIMIT} characters or fewer.`;
  if (CONTROL_RE.test(v)) return "Use one line, without control characters.";
  if (/[<>]/.test(v)) return "Leave out < and >.";
  if (v.includes("://") || /^www\./i.test(v) || URLISH_RE.test(v)) {
    return "Use a name, not something that looks like an address.";
  }
  return null;
}

/* ---------------------------------------------------------------- *
 * security.txt (RFC 9116)
 *
 * Four fields, mirroring `internal/api/security_txt_settings.go`. The
 * rule between them is the one worth stating here rather than only on
 * the server: the contact is what publishes the file, and a published
 * file needs an expiry. So the expiry's message is checked against the
 * contact's *draft* — an administrator filling both boxes in one pass
 * must not be refused by a field they are in the middle of typing.
 * ---------------------------------------------------------------- */

const SECURITY_TXT_KEYS = new Set<string>([
  "security_txt_contact",
  "security_txt_expires",
  "security_txt_policy",
  "security_txt_languages",
]);

type SecurityTxtKey =
  | "security_txt_contact"
  | "security_txt_expires"
  | "security_txt_policy"
  | "security_txt_languages";

function isSecurityTxtKey(key: FieldKey): key is SecurityTxtKey {
  return SECURITY_TXT_KEYS.has(key);
}

const CONTACT_RE = /^(mailto:\S+@\S+|tel:\S+|https:\/\/[^/?#\s]+\S*)$/i;
const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
const LANGUAGE_TAG_RE = /^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$/;

/** The ceiling the server holds, in the same number of days. */
export const MAX_SECURITY_TXT_EXPIRY_DAYS = 800;

function securityTxtError(
  key: SecurityTxtKey,
  raw: string,
  draftOf: (k: FieldKey) => string,
): string | null {
  const v = raw.trim();

  if (key === "security_txt_contact") {
    if (v === "") return null;
    return CONTACT_RE.test(v)
      ? null
      : "Write it as a URI — mailto:security@example.com, https://example.com/security, or tel:+1-201-555-0123. A bare email address is not one.";
  }

  if (key === "security_txt_policy") {
    if (v === "") return null;
    return /^https:\/\/[^/?#\s]+/i.test(v)
      ? null
      : "Must be an https:// address. It is published to whoever fetches the file, so it has to be somewhere they can reach.";
  }

  if (key === "security_txt_languages") {
    if (v === "") return null;
    const bad = v
      .split(",")
      .map((t) => t.trim())
      .filter((t) => t !== "" && !LANGUAGE_TAG_RE.test(t));
    return bad.length === 0
      ? null
      : `“${bad[0]}” is not a language tag. Write them separated by commas — en, fr, de-CH.`;
  }

  // The expiry, and the one cross-field rule.
  const hasContact = draftOf("security_txt_contact").trim() !== "";
  if (v === "") {
    return hasContact
      ? "A published security.txt needs an expiry date. RFC 9116 requires the field, and it is what stops this file quietly naming a contact nobody reads."
      : null;
  }
  if (!ISO_DATE_RE.test(v)) return "Write the date as YYYY-MM-DD.";
  const when = Date.parse(v + "T00:00:00Z");
  if (Number.isNaN(when)) return "Write the date as YYYY-MM-DD.";
  const today = Date.parse(new Date().toISOString().slice(0, 10) + "T00:00:00Z");
  if (when <= today) {
    return "Must be a date in the future. This is the field that makes a stale security.txt visible, and one that has already passed publishes a contact nobody is promising to read.";
  }
  if (when - today > MAX_SECURITY_TXT_EXPIRY_DAYS * 86_400_000) {
    return `Must be within ${MAX_SECURITY_TXT_EXPIRY_DAYS} days. RFC 9116 asks for less than a year, and a date further out than that is a way of declining to be reminded — which is the one thing the field is for.`;
  }
  return null;
}

/** How many days are left on an expiry, or null when there is not one to read.
 *  The settings page says this out loud: the whole point of the field is that
 *  somebody sees it going stale before a researcher does. */
export function securityTxtDaysLeft(stored: string): number | null {
  const v = stored.trim();
  if (!ISO_DATE_RE.test(v)) return null;
  const when = Date.parse(v + "T00:00:00Z");
  if (Number.isNaN(when)) return null;
  const today = Date.parse(new Date().toISOString().slice(0, 10) + "T00:00:00Z");
  return Math.round((when - today) / 86_400_000);
}

/** What gets sent, and what a value is compared against to know it changed. */
export function normalize(key: FieldKey, raw: string): string {
  if (key === "public_base_url" || key === "garth_url") return raw.trim().replace(/\/+$/, "");
  return raw.trim();
}

/**
 * The message a field shows when what is typed cannot be saved — including
 * the one cross-field rule, which is checked against the *other field's
 * draft* rather than its stored value. Raising both retention figures at
 * once used to be impossible for exactly that reason.
 */
/** "HH:MM", 24-hour — the server's `ParseClock`. */
const CLOCK_RE = /^([01]\d|2[0-3]):[0-5]\d$/;

export function fieldError(
  key: FieldKey,
  raw: string,
  draftOf: (k: FieldKey) => string,
): string | null {
  if (key === "public_base_url") {
    const v = normalize(key, raw);
    return v === "" || PUBLIC_BASE_URL_RE.test(v)
      ? null
      : "Write it as an address only — https://toft.example — with no path after the hostname.";
  }
  if (key === "garth_url") {
    // The server's rule (NormalizeGarthURL): https, a host, no user name,
    // query or fragment. A path is allowed — Garth may live under one.
    const v = normalize(key, raw);
    return v === "" || GARTH_URL_RE.test(v)
      ? null
      : "Write it as an https address — https://garth.example — with no query after it.";
  }
  if (key === "install_name") return installNameError(raw);
  if (key === "display_time_zone") return displayTimeZoneError(raw);
  if (key === "isolated_sleep_window_start" || key === "isolated_sleep_window_end") {
    const v = raw.trim();
    if (!CLOCK_RE.test(v)) return "Write a 24-hour time, such as 19:00.";
    const other =
      key === "isolated_sleep_window_start" ? "isolated_sleep_window_end" : "isolated_sleep_window_start";
    return draftOf(other).trim() === v
      ? "The window’s start and end can’t be the same time — it would never open."
      : null;
  }
  if (key === "environment_tag") {
    const v = normalize(key, raw);
    return v === "" || ENV_NAME_RE.test(v)
      ? null
      : "Use 1–32 lowercase letters, digits, dashes, or underscores.";
  }

  if (isSecurityTxtKey(key)) return securityTxtError(key, raw, draftOf);

  // The one text field with nothing to check. It is a URL somebody typed, and
  // it is deliberately unvalidated on both sides: the legitimate value is
  // routinely a private mirror, and nothing here or on the server dials it —
  // Rscript is handed a repository option and no socket.
  if (key === "r_package_repository") return null;

  const spec = NUMBER_FIELDS[key];
  const v = raw.trim();
  const n = Number(v);
  const isWhole = v !== "" && Number.isInteger(n);

  if (key === "job_run_retention_days") {
    const floor = Number(draftOf("job_log_retention_days").trim());
    const lo = Number.isInteger(floor) && floor >= 1 ? floor : spec.min;
    if (isWhole && n >= lo && n <= spec.max) return null;
    return `Must be a whole number from ${lo} to ${spec.max} — at least as long as the ${lo}-day log retention above. A run deleted while its logs remain is a log nobody can attribute.`;
  }

  // The default may not exceed the ceiling, checked against the ceiling's
  // *draft*: an administrator raising both at once must not be refused by a
  // number they are in the middle of replacing.
  if (key === "default_job_timeout_seconds") {
    const ceiling = Number(draftOf("max_job_timeout_seconds").trim());
    const hi =
      Number.isInteger(ceiling) && ceiling >= spec.min ? Math.min(ceiling, spec.max) : spec.max;
    if (isWhole && n >= spec.min && n <= hi) return null;
    return `Must be a whole number from ${spec.min} to ${hi} seconds — no longer than the longest a job may run, below.`;
  }

  if (spec.zeroAllowed && n === 0 && isWhole) return null;
  if (isWhole && n >= spec.min && n <= spec.max) return null;

  if (key === "app_cpu_request_millicores") {
    return "Must be 0 to use this install’s own figure, or from 0.001 to 64 cores.";
  }
  if (key === "app_cpu_limit_millicores" || key === "job_cpu_limit_millicores") {
    return "Must be 0 to use this install’s own figure, or from 0.05 to 64 cores.";
  }
  if (key === "job_memory_limit_mb") {
    return "Must be 0 to use this install’s own figure, or at least 64 MB (0.0625 GB).";
  }
  if (key === "max_concurrent_builds") {
    return "Must be 0 to use this install’s MAX_CONCURRENT_BUILDS, or a whole number no larger than the pool this install’s build workers were started as.";
  }
  if (key === "auto_deploy_poll_seconds") {
    return "Must be 0 to use this install’s AUTO_DEPLOY_POLL_INTERVAL, or a whole number from 10 to 86400 seconds. Turning polling off is a change to the install’s configuration, not a value here.";
  }
  if (key === "app_memory_limit_mb") {
    return "Must be 0 to use this install’s own figure, or at least 64 MB (0.0625 GB) — a cap below that kills an interpreter before it has finished starting.";
  }

  if (key === "idle_sleep_minutes") {
    return "Must be 0 to turn idle sleep off, or a whole number from 5 to 10080 minutes.";
  }
  if (key === "isolated_idle_sleep_minutes") {
    return "Must be 0 to keep isolated apps awake, or a whole number from 5 to 10080 minutes.";
  }
  if (key === "api_key_max_lifetime_days") {
    return "Must be 0 for no limit, or a whole number from 1 to 3650 days.";
  }
  if (key === "max_versions_per_app") {
    return "Must be 0 for no limit, or a whole number from 2 to 100. A limit of 1 would leave no earlier release to roll back to.";
  }
  if (key === "max_job_timeout_seconds") {
    return "Must be a whole number from 60 to 604800 seconds — a minute to a week.";
  }
  if (key === "max_queued_runs_per_app") {
    return "Must be a whole number from 1 to 10000.";
  }
  if (key === "report_delivery_min_interval_minutes") {
    return "Must be 0 for no floor, or a whole number up to 1440 minutes — a day.";
  }
  if (key === "app_event_retention_days") {
    return "Must be a whole number from 7 to 3650 days. A week is the floor because anything shorter cannot answer whether an app failed over a weekend.";
  }
  if (key === "platform_log_retention_days") {
    return "Must be a whole number from 7 to 3650 days. Keep it at least as long as failure events above: an event’s detail points at this log, so a shorter figure here leaves the pointer with nowhere to land.";
  }
  if (key === "announcement_recipient_retention_days") {
    return "Must be a whole number from 30 to 3650 days. A month is the floor because “was this person told, and if not why not” stops being answerable inside the window in which anybody asks it.";
  }
  if (key === "access_record_retention_days") {
    return "Must be 0 to keep everything, or a whole number from 365 to 3650 days. A year is the floor because it is the common retention requirement.";
  }
  if (key === "session_absolute_hours") {
    return "Must be a whole number from 1 to 24 hours. There is no value that turns sessions off, and a day is the longest anything can reasonably be called a session.";
  }
  if (key === "session_idle_minutes") {
    return "Must be 0 to turn the idle timeout off, or a whole number from 5 to 480 minutes. A shorter timeout signs people out while they are reading a page.";
  }
  return `Must be a whole number from ${spec.min} to ${spec.max}.`;
}

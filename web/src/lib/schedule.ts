/**
 * A schedule's time zone travels inside its cron expression, as
 * `CRON_TZ=Europe/London 0 6 * * *` — the server's `internal/schedule` decides
 * that and this mirrors it. Every screen that shows or edits a schedule goes
 * through these, so the prefix is never what somebody reads.
 */

const PREFIXES = ["CRON_TZ=", "TZ="];

/** The zone ("" for UTC) and the bare expression. */
export function splitSchedule(s: string | null | undefined): { zone: string; expr: string } {
  const t = (s ?? "").trim();
  for (const p of PREFIXES) {
    if (!t.startsWith(p)) continue;
    const rest = t.slice(p.length);
    const i = rest.search(/\s/);
    if (i < 0) return { zone: rest, expr: "" };
    return { zone: rest.slice(0, i), expr: rest.slice(i).trim() };
  }
  return { zone: "", expr: t };
}

/** The stored form. No zone, or UTC, stores the bare expression. */
export function joinSchedule(zone: string, expr: string): string {
  const z = zone.trim();
  const e = expr.trim();
  if (!z || z === "UTC" || !e) return e;
  return `CRON_TZ=${z} ${e}`;
}

/** What a list prints: the expression, and the zone only when it is not UTC. */
export function scheduleLabel(s: string | null | undefined): string {
  const { zone, expr } = splitSchedule(s);
  return zone && expr ? `${expr} · ${zone}` : expr;
}

/** The IANA zones this browser knows, for the picker's suggestions. The server
 * holds the authoritative list and refuses a name it does not know. */
export function knownTimeZones(): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: (key: string) => string[] };
  try {
    return intl.supportedValuesOf?.("timeZone") ?? [];
  } catch {
    return [];
  }
}

/** Whether a typed zone is plausibly one: empty (UTC), or a name the browser
 * knows. A browser with no list accepts anything and leaves it to the server. */
export function zoneLooksValid(zone: string, known: string[]): boolean {
  const z = zone.trim();
  return z === "" || z === "UTC" || known.length === 0 || known.includes(z);
}

let known: string[] | undefined;

/** zoneLooksValid against this browser's list, read once. */
export function timeZoneValid(zone: string): boolean {
  known ??= knownTimeZones();
  return zoneLooksValid(zone, known);
}

/** The UTC offset, in minutes, that `zone` has at instant `at`. */
function offsetMinutes(zone: string, at: Date): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: zone, hourCycle: "h23",
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  }).formatToParts(at);
  const n = (t: string) => Number(parts.find((p) => p.type === t)?.value);
  const asUTC = Date.UTC(n("year"), n("month") - 1, n("day"), n("hour"), n("minute"), n("second"));
  return Math.round((asUTC - at.getTime()) / 60000);
}

/**
 * A wall-clock time typed into a `datetime-local` field, read in `zone` ("" is
 * UTC), as the instant it names — or null when the field is not a time. Two
 * passes, because the offset to subtract is the one in force at the answer, not
 * at the guess, and the two differ across a changeover.
 */
export function zonedWallToInstant(wall: string, zone: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(wall.trim());
  if (!m) return null;
  const guess = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +(m[6] ?? 0));
  const z = zone.trim();
  if (!z || z === "UTC") return new Date(guess);
  try {
    let at = guess - offsetMinutes(z, new Date(guess)) * 60000;
    at = guess - offsetMinutes(z, new Date(at)) * 60000;
    return new Date(at);
  } catch {
    return null;
  }
}

/** expect_within_sec back into the shape somebody typed: whole days as "7d",
 * otherwise hours. */
export function formatExpectWithin(sec: number | undefined): string {
  if (!sec) return "";
  if (sec % 86400 === 0 && sec >= 2 * 86400) return `${sec / 86400}d`;
  if (sec % 3600 === 0) return `${sec / 3600}h`;
  return `${Math.round(sec / 60)}m`;
}

/** Whether a typed window is one the server will take: a number and a unit of
 * m, h or d, between an hour and 31 days. Empty is "no expectation". */
export function expectWithinValid(v: string): boolean {
  const t = v.trim();
  if (t === "") return true;
  const m = /^(\d+)(m|h|d)$/.exec(t);
  if (!m) return false;
  const sec = Number(m[1]) * { m: 60, h: 3600, d: 86400 }[m[2] as "m" | "h" | "d"];
  return sec >= 3600 && sec <= 31 * 86400;
}

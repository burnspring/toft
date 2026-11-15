// Shared time formatting. Convention: relative times in tables ("3m ago")
// with the absolute timestamp in a title tooltip; absolute short form where
// exact times matter (deploy history).
//
// **Every moment is shown in one zone, and it is never the server's** (ADR
// §154). The API speaks UTC; the zone a person reads is theirs, chosen in this
// order: their own choice on /me, else the install's default an administrator
// set, else this browser's own zone. `setDisplayTimeZone` is called once the
// session is known, and every formatter below reads it. A UTC *day* — a usage
// bucket — is the exception and stays UTC, because that is what it counts.

/** The zone moments are shown in; undefined is this browser's own. */
let displayZone: string | undefined;

/** Whether this browser can format in `zone`. A zone the server knows and an
 *  old browser does not falls back to the browser's own, never to an error. */
export function zoneFormattable(zone: string): boolean {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

export function setDisplayTimeZone(zone: string | undefined): void {
  displayZone = zone && zoneFormattable(zone) ? zone : undefined;
}

/** The zone in force: a name, or undefined for the browser's. */
export function displayTimeZone(): string | undefined {
  return displayZone;
}

/** This browser's own zone, by name — what "browser" resolves to. */
export function browserTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
}

/**
 * The two layers /auth/me hands over, resolved. `user` is "" (follow the
 * install), "browser", or a zone; `install` is "" (each browser) or a zone.
 * Undefined is the browser's own zone.
 */
export function resolveDisplayTimeZone(tz?: { user: string; install: string }): string | undefined {
  if (!tz || tz.user === "browser") return undefined;
  return tz.user || tz.install || undefined;
}

export function timeAgo(iso: string): string {
  if (!iso) return "--";
  const seconds = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  // A future timestamp is not "just now". Without this the negative branch
  // falls through to the under-a-minute case and a date three months out reads
  // as having already happened. A moment less than a minute ahead is the
  // server's clock running ahead of this browser's — a last-seen stamped by the
  // very request that read it — and is "just now", not timeUntil's "expired".
  if (seconds < -60) return timeUntil(iso);
  if (seconds < 0) return "just now";
  if (seconds < 60) return "just now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(iso).toLocaleDateString("en-US", {
    year: "numeric", month: "short", day: "numeric", timeZone: displayZone,
  });
}

/**
 * How long until a future moment — the mirror of timeAgo, for deadlines rather
 * than events. A moment that has already passed reads as "expired", because
 * every current caller is asking about an expiry and "0m" would be worse.
 */
export function timeUntil(iso: string): string {
  if (!iso) return "--";
  const seconds = Math.floor((new Date(iso).getTime() - Date.now()) / 1000);
  if (seconds <= 0) return "expired";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `in ${Math.max(1, minutes)}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `in ${hours}h`;
  const days = Math.floor(hours / 24);
  if (days < 60) return `in ${days} day${days === 1 ? "" : "s"}`;
  return new Date(iso).toLocaleDateString("en-US", {
    year: "numeric", month: "short", day: "numeric", timeZone: displayZone,
  });
}

/**
 * A **day**, with its year, and no time of day.
 *
 * For a fact that is a date rather than a moment — a license term, an expiry,
 * a billing period. Rendering 03:02:58 beside one invites somebody to wonder
 * about the timezone of a thing that has no time; leaving the year off is
 * worse, because the two dates on a term are usually twelve months apart and
 * "Aug 21" beside "Aug 21" is not a term.
 *
 * Here rather than in the page that wanted it, which had grown its own pair of
 * `en-GB` formatters while the rest of the product formats `en-US` — so one
 * screen spelled a date a different way round from every other.
 */
export function formatDay(iso: string): string {
  if (!iso) return "--";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  // A bare `YYYY-MM-DD` parses as UTC midnight, which the browser's zone puts
  // on the previous day anywhere west of Greenwich. A day named without a time
  // is shown as that day; a moment is shown in the reader's zone.
  const dateOnly = /^\d{4}-\d{2}-\d{2}$/.test(iso);
  return d.toLocaleDateString("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
    timeZone: dateOnly ? "UTC" : displayZone,
  });
}

/** A moment, to the minute, with its zone named — "Nov 7, 2026, 02:03 PM EST".
 *  The zone is there because an unlabelled time on a hosted install is a
 *  question: whose clock? */
export function formatDateTime(iso: string): string {
  if (!iso) return "--";
  return new Date(iso).toLocaleString("en-US", {
    year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
    timeZone: displayZone, timeZoneName: "short",
  });
}

export function formatTimestamp(iso: string): string {
  if (!iso) return "--";
  return new Date(iso).toLocaleString("en-US", {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
    timeZone: displayZone,
  });
}

/**
 * A moment to the second, 24-hour, with its zone — "Nov 7, 2026, 14:03:22 EST" —
 * for where *exactly when* is the question (an audit row). `zone` overrides the
 * display zone; pass "UTC" for the line that is compared with server logs.
 */
export function formatExactTime(iso: string, zone?: string): string {
  if (!iso) return "--";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
    timeZone: zone ?? displayZone, timeZoneName: "short",
  });
}

/** Whether the zone in force already is UTC, so a second UTC line would only repeat it. */
export function displayIsUTC(): boolean {
  const z = displayZone ?? browserTimeZone();
  return z === "UTC" || z === "Etc/UTC";
}

export function formatDuration(startedAt: string, finishedAt: string): string {
  if (!startedAt || !finishedAt) return "--";
  const ms = new Date(finishedAt).getTime() - new Date(startedAt).getTime();
  if (ms < 0) return "--";
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

export function formatElapsed(sinceIso: string): string {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(sinceIso).getTime()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

export function formatUptime(seconds: number): string {
  if (seconds <= 0) return "0m";
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);
  const days = Math.floor(hours / 24);
  if (days > 0) return `${days}d ${hours % 24}h`;
  if (hours > 0) return `${hours}h ${minutes % 60}m`;
  if (minutes > 0) return `${minutes}m`;
  return `${seconds}s`;
}

/**
 * CPU-seconds, in the units the estate table already uses.
 *
 * Under a second reads as "—" rather than "0.4s": the figure is a lower bound
 * whenever part of the window went unobserved, and a sub-second total is
 * indistinguishable from a gap.
 */
export function formatCpuSeconds(s: number): string {
  if (s < 1) return "—";
  if (s < 60) return `${s.toFixed(1)}s`;
  return formatUptime(Math.round(s));
}

/**
 * A CPU reservation, in the units an operator typed and a cluster stores.
 *
 * Millicores below a core, because that is the spelling everywhere else this
 * number appears — `APP_CPU_REQUEST_MILLICORES`, `kubectl describe`, a pod
 * spec — and a reader comparing the page against any of them should not have
 * to convert. A whole number of cores reads as cores, because "2000m" is a
 * spelling nobody chooses when they mean two.
 */
export function formatMillicores(m: number): string {
  if (m >= 1000 && m % 1000 === 0) {
    const cores = m / 1000;
    return `${cores} ${cores === 1 ? "core" : "cores"}`;
  }
  return `${m}m`;
}

/**
 * A **UTC calendar day**, for the axis of anything bucketed by one.
 *
 * The platform buckets traffic by UTC day, so a label rendered in the reader's
 * own zone slides by one for anybody west of Greenwich — a bar labelled Tuesday
 * holding Monday's requests. Every chart and table over a daily bucket reads
 * this rather than each formatting a `Date` for itself: three of them did, in
 * three shapes, one of which had drifted onto `en-US` while the others used the
 * browser's locale.
 *
 * `day` is the API's own `YYYY-MM-DD`.
 */
export function formatUTCDay(day: string): string {
  if (!day) return "--";
  return new Date(`${day}T00:00:00Z`).toLocaleDateString("en-US", {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  });
}

/**
 * The `days` UTC day keys (`YYYY-MM-DD`) ending today's, oldest first — the
 * shape the daily usage buckets are keyed on. Stepped in UTC: walking back
 * local days and keying each with `toISOString` read one bucket twice and
 * another never whenever the window crossed a DST change near midnight.
 */
export function utcDayKeys(days: number, now: Date = new Date()): string[] {
  const today = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
  const out: string[] = [];
  for (let i = days - 1; i >= 0; i--) {
    out.push(new Date(today - i * 86_400_000).toISOString().slice(0, 10));
  }
  return out;
}

/** The same bucket a month wide — "Aug 26" rather than "Aug 1". */
export function formatUTCMonth(day: string): string {
  if (!day) return "--";
  return new Date(`${day}T00:00:00Z`).toLocaleDateString("en-US", {
    month: "short",
    year: "2-digit",
    timeZone: "UTC",
  });
}

/** A month in full, for a sentence rather than an axis — "March 2025". */
export function formatUTCMonthLong(day: string): string {
  if (!day) return "--";
  return new Date(`${day}T00:00:00Z`).toLocaleDateString("en-US", {
    month: "long",
    year: "numeric",
    timeZone: "UTC",
  });
}

/**
 * Just the clock, 24-hour — for a log's time gutter.
 *
 * The date belongs to the pane's window rather than to every line in it, and a
 * log read during an incident is read by the second. `en-US` like every other
 * formatter here: one of these reached for `en-GB` and `conventions.test.ts`
 * caught it, which is the same drift that had one screen spelling a date the
 * other way round from the rest of the product.
 */
export function formatClock(iso: string): string | undefined {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return undefined;
  return d.toLocaleTimeString("en-US", {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false, timeZone: displayZone,
  });
}

// --- A day in the display zone (ADR §156) ---------------------------------
//
// Admin → Automation's Upcoming view asks the server for one day's firings,
// and "a day" is the reader's: midnight to midnight in the display zone, which
// only this page knows (§154 — the server never resolves "browser"). The page
// sends the two instants; on a changeover day they are 23 or 25 hours apart,
// and that is right.

/** The zone in force, by name — the display zone, else this browser's. */
export function displayZoneName(): string {
  return displayZone ?? browserTimeZone();
}

/** How far `zone`'s wall clock is ahead of UTC at instant `ms`, in ms. */
function zoneOffsetMs(ms: number, zone: string): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: zone, hourCycle: "h23",
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  }).formatToParts(new Date(ms));
  const n = (t: string) => Number(parts.find((p) => p.type === t)?.value ?? 0);
  const wall = Date.UTC(n("year"), n("month") - 1, n("day"), n("hour"), n("minute"), n("second"));
  return wall - Math.floor(ms / 1000) * 1000;
}

/** The instant a `YYYY-MM-DD` day's midnight falls at in `zone`. A midnight a
 *  changeover skips is the first moment of the day that exists. */
function zoneMidnight(day: string, zone: string): number {
  const [y, m, d] = day.split("-").map(Number);
  const wall = Date.UTC(y, m - 1, d);
  let guess = wall - zoneOffsetMs(wall, zone);
  // Twice is enough: the second pass corrects a guess that landed across a
  // changeover from the instant it was looking for. When local midnight does
  // not exist the two passes alternate between the instants either side of
  // the gap; the later one is the first moment of the day that exists — the
  // earlier is an hour inside the previous day, which is where Cairo's and
  // Beirut's spring-forward day used to start (code review 2026-11-11, O-3).
  for (let i = 0; i < 2; i++) {
    const next = wall - zoneOffsetMs(guess, zone);
    if (next === guess) break;
    if (i === 1) return Math.max(guess, next);
    guess = next;
  }
  return guess;
}

/** Midnight to midnight of `day` in the display zone, as two instants. */
export function displayDayBounds(day: string, zone: string = displayZoneName()): { from: Date; to: Date } {
  return { from: new Date(zoneMidnight(day, zone)), to: new Date(zoneMidnight(shiftDay(day, 1), zone)) };
}

/** Today, as `YYYY-MM-DD`, in the display zone. */
export function displayToday(now: Date = new Date(), zone: string = displayZoneName()): string {
  return new Date(now.getTime() + zoneOffsetMs(now.getTime(), zone)).toISOString().slice(0, 10);
}

/** An instant as the wall clock reads it in the display zone —
 *  `YYYY-MM-DDTHH:MM`, the shape a `datetime-local` field holds. The way back
 *  is `zonedWallToInstant` (`lib/schedule.ts`). */
export function displayWallClock(at: Date = new Date(), zone: string = displayZoneName()): string {
  return new Date(at.getTime() + zoneOffsetMs(at.getTime(), zone)).toISOString().slice(0, 16);
}

/** `day` moved by `n` calendar days. Pure date arithmetic, no zone. */
export function shiftDay(day: string, n: number): string {
  const [y, m, d] = day.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d + n)).toISOString().slice(0, 10);
}

/** Whether `s` is a real `YYYY-MM-DD` date. */
export function isDayKey(s: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(s) && shiftDay(s, 0) === s;
}

/** A day named for a heading — "Tue, Nov 10, 2026". */
export function formatDayHeading(day: string): string {
  const [y, m, d] = day.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d)).toLocaleDateString("en-US", {
    weekday: "short", month: "short", day: "numeric", year: "numeric", timeZone: "UTC",
  });
}

/** Hours and minutes, 24-hour, in the display zone — "02:00". */
export function formatHourMinute(iso: string | number): string {
  return new Date(iso).toLocaleTimeString("en-US", {
    hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: displayZone,
  });
}

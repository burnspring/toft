// Admin → Automation's Upcoming view, as arithmetic (ADR §156).
//
// The server expands schedules; this module only lays out what it returned:
// which lane a bar sits in, where nothing is scheduled, and the words for a
// duration. Pure, so the decisions are tested here rather than in a page.

import type { UpcomingFiring, UpcomingSchedules } from "./api";

const MIN = 60_000;

/** A gap shorter than this is not listed: half an hour is the smallest window
 *  an administrator plans an upgrade around. */
export const QUIET_MIN_MS = 30 * MIN;

/** A firing with neither a median nor a bound occupies this long. */
const POINT_MS = MIN;

/** One firing on the strip. `end` is `at + typical`; a firing with no history
 *  is a point (`end === start`) with `upper` its faint bound. */
export interface StripBar {
  firing: UpcomingFiring;
  start: number;
  end: number;
  upper?: number;
  lane: number;
}

/**
 * Stacks firings into lanes so an overlap reads as height. Greedy, in time
 * order: each bar takes the lowest lane already free at its start. A bar is
 * given at least `minVisualMs` for the test, so two points a minute apart do
 * not sit on top of each other where the eye cannot separate them; the faint
 * bound is not counted, since a day-long timeout would push every later bar
 * into a lane of its own.
 */
export function stackLanes(firings: UpcomingFiring[], minVisualMs: number): { bars: StripBar[]; lanes: number } {
  const laneEnds: number[] = [];
  const bars: StripBar[] = [...firings]
    .sort((a, b) => Date.parse(a.at) - Date.parse(b.at))
    .map((f) => {
      const start = Date.parse(f.at);
      const end = f.typical_sec != null ? start + f.typical_sec * 1000 : start;
      const upper = f.typical_sec == null && f.upper_sec != null ? start + f.upper_sec * 1000 : undefined;
      const occupied = Math.max(end, start + minVisualMs);
      let lane = laneEnds.findIndex((e) => e <= start);
      if (lane < 0) {
        lane = laneEnds.length;
        laneEnds.push(occupied);
      } else {
        laneEnds[lane] = occupied;
      }
      return { firing: f, start, end, upper, lane };
    });
  return { bars, lanes: laneEnds.length };
}

/** How long a firing is taken to occupy the install, for the quiet line: the
 *  median, else the bound, else a minute — the conservative reading, since a
 *  gap that is not really there is the expensive mistake. */
function occupiedMs(f: UpcomingFiring): number {
  if (f.typical_sec != null) return Math.max(f.typical_sec * 1000, POINT_MS);
  if (f.upper_sec != null) return f.upper_sec * 1000;
  return POINT_MS;
}

/**
 * The stretches of at least `minMs` with nothing scheduled in [start, end),
 * frequent schedules' bands included. `start` is the later of the interval's
 * start and now: a gap already past is no use to anybody planning.
 */
export function quietStretches(
  data: Pick<UpcomingSchedules, "firings" | "recurring">,
  start: number,
  end: number,
  minMs: number = QUIET_MIN_MS,
): { start: number; end: number }[] {
  const busy: [number, number][] = [
    ...data.firings.map((f): [number, number] => {
      const at = Date.parse(f.at);
      return [at, at + occupiedMs(f)];
    }),
    ...data.recurring.map((r): [number, number] => [
      Date.parse(r.at),
      Date.parse(r.last ?? r.at) + occupiedMs(r),
    ]),
  ].sort((a, b) => a[0] - b[0]);

  const out: { start: number; end: number }[] = [];
  let cursor = start;
  for (const [s, e] of busy) {
    if (s - cursor >= minMs) out.push({ start: cursor, end: Math.min(s, end) });
    cursor = Math.max(cursor, e);
    if (cursor >= end) break;
  }
  if (end - cursor >= minMs) out.push({ start: cursor, end });
  return out.filter((g) => g.end - g.start >= minMs);
}

/** "1 h 30 m", "4 m", "45 s". */
export function formatSpan(sec: number): string {
  if (sec < 60) return `${Math.max(0, Math.round(sec))} s`;
  const h = Math.floor(sec / 3600);
  const m = Math.round((sec % 3600) / 60);
  if (h === 0) return `${m} m`;
  return m ? `${h} h ${m} m` : `${h} h`;
}

/** What the agenda says about a firing's length, short. */
export function durationShort(f: UpcomingFiring): string {
  if (f.typical_sec != null) return `usually ${formatSpan(f.typical_sec)}`;
  if (f.upper_sec != null) return `up to ${formatSpan(f.upper_sec)}`;
  return "no runs yet";
}

/** The same with what the median is over — for the hover. */
export function durationLong(f: UpcomingFiring): string {
  if (f.typical_sec != null) {
    return `usually ${formatSpan(f.typical_sec)} (last ${f.basis} run${f.basis === 1 ? "" : "s"})`;
  }
  if (f.kind === "restart") return `up to ${formatSpan(f.upper_sec ?? 180)}`;
  return f.upper_sec != null ? `no runs yet — up to ${formatSpan(f.upper_sec)}` : "no runs yet";
}

/** "every 5 minutes", "every minute", or "as often as every 5 minutes" when
 *  the firings are not evenly spaced. */
export function recurrencePhrase(r: UpcomingFiring): string {
  const gap = r.gap_sec ?? 0;
  const mins = Math.round(gap / 60);
  const unit = gap < 60 ? `${gap} seconds` : mins === 1 ? "minute" : `${mins} minutes`;
  const count = r.count ?? 0;
  const even = count > 1 && r.last != null &&
    (Date.parse(r.last) - Date.parse(r.at)) / 1000 === gap * (count - 1);
  return `${even ? "every" : "as often as every"} ${unit}, ${count} time${count === 1 ? "" : "s"}`;
}

/** What a firing is called on the strip and in the agenda. */
export function firingName(f: UpcomingFiring): string {
  if (f.kind === "render") return "Re-render";
  if (f.kind === "restart") return "Restart";
  return f.name ?? "Job";
}

export const KIND_LABEL: Record<UpcomingFiring["kind"], string> = {
  job: "job",
  render: "re-render",
  restart: "restart",
};

/** Where on its app a schedule is shown and paused — the inventory's links. */
export function firingHref(f: UpcomingFiring): string {
  if (f.kind === "job") return `/apps/${f.app_id}/automation`;
  if (f.kind === "render") return `/apps/${f.app_id}/settings`;
  return `/apps/${f.app_id}/settings#runtime`;
}

/** "2 paused schedules and 1 on a locked app would otherwise run today." —
 *  empty when nothing is held. */
export function heldLine(held: UpcomingSchedules["held"], when: string): string {
  const parts: string[] = [];
  if (held.paused) parts.push(`${held.paused} paused schedule${held.paused === 1 ? "" : "s"}`);
  if (held.locked) {
    const noun = held.paused ? "" : held.locked === 1 ? " schedule" : " schedules";
    parts.push(`${held.locked}${noun} on ${held.locked === 1 ? "a locked app" : "locked apps"}`);
  }
  return parts.length ? `${parts.join(" and ")} would otherwise run ${when}.` : "";
}

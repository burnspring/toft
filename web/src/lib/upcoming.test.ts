import { describe, expect, it } from "vitest";
import type { UpcomingFiring } from "./api";
import {
  durationLong, durationShort, formatSpan, heldLine, quietStretches, recurrencePhrase, stackLanes,
} from "./upcoming";

const DAY = Date.parse("2026-11-10T00:00:00Z");
const H = 3_600_000;
const iso = (ms: number) => new Date(ms).toISOString();

function firing(atH: number, typical: number | null, extra: Partial<UpcomingFiring> = {}): UpcomingFiring {
  return {
    kind: "job", schedule_id: `s${atH}`, app_id: "a", app_name: "App", app_slug: "app",
    name: `job${atH}`, at: iso(DAY + atH * H), zone: "", schedule: "0 2 * * *",
    typical_sec: typical, upper_sec: 3600, basis: typical == null ? 0 : 10, ...extra,
  };
}

describe("stackLanes", () => {
  it("puts overlapping firings in separate lanes and reuses a freed one", () => {
    const { bars, lanes } = stackLanes(
      [firing(2, 2 * 3600), firing(3, 600), firing(5, 600)],
      10 * 60_000,
    );
    expect(lanes).toBe(2);
    expect(bars.map((b) => b.lane)).toEqual([0, 1, 0]);
  });

  it("separates two points a minute apart by their visual width, not their length", () => {
    const a = firing(1, null);
    const b = { ...firing(1, null), at: iso(DAY + H + 60_000) };
    const { bars, lanes } = stackLanes([a, b], 10 * 60_000);
    expect(lanes).toBe(2);
    expect(bars[0].end).toBe(bars[0].start);
    expect(bars[0].upper).toBe(bars[0].start + H);
  });

  it("does not let a faint bound push later firings into new lanes", () => {
    const long = { ...firing(0, null), upper_sec: 86400 };
    const { lanes } = stackLanes([long, firing(4, 60)], 10 * 60_000);
    expect(lanes).toBe(1);
  });
});

describe("quietStretches", () => {
  it("lists every gap of half an hour or more, frequent bands included", () => {
    const data = {
      firings: [firing(2, 90 * 60), firing(6, 60)],
      recurring: [{ ...firing(12, 60), at: iso(DAY + 12 * H), last: iso(DAY + 17 * H), count: 61, gap_sec: 300 }],
    };
    const gaps = quietStretches(data, DAY, DAY + 24 * H);
    expect(gaps.map((g) => [(g.start - DAY) / H, (g.end - DAY) / H])).toEqual([
      [0, 2],
      [3.5, 6],
      [6 + 1 / 60, 12],
      [17 + 1 / 60, 24],
    ]);
  });

  it("starts at now, and ignores gaps under half an hour", () => {
    const gaps = quietStretches({ firings: [firing(10.25, 60)], recurring: [] }, DAY + 10 * H, DAY + 11 * H);
    expect(gaps.map((g) => (g.start - DAY) / H)).toEqual([10.25 + 1 / 60]);
  });

  it("counts a firing with no history for its whole bound", () => {
    const gaps = quietStretches({ firings: [firing(1, null)], recurring: [] }, DAY, DAY + 3 * H);
    expect(gaps.map((g) => [(g.start - DAY) / H, (g.end - DAY) / H])).toEqual([[0, 1], [2, 3]]);
  });

  it("draws a 25-hour day to its end", () => {
    const gaps = quietStretches({ firings: [], recurring: [] }, DAY, DAY + 25 * H);
    expect(gaps).toEqual([{ start: DAY, end: DAY + 25 * H }]);
  });
});

describe("the words", () => {
  it("says what a median is over, and never dresses a bound as a measurement", () => {
    expect(formatSpan(5400)).toBe("1 h 30 m");
    expect(formatSpan(240)).toBe("4 m");
    expect(durationShort(firing(2, 5400))).toBe("usually 1 h 30 m");
    expect(durationLong(firing(2, 5400))).toBe("usually 1 h 30 m (last 10 runs)");
    expect(durationLong(firing(2, null))).toBe("no runs yet — up to 1 h");
    expect(durationShort({ ...firing(4, null), kind: "restart", upper_sec: 180 })).toBe("up to 3 m");
  });

  it("describes a recurrence by its gap and count", () => {
    const r = { ...firing(0, null), last: iso(DAY + 24 * H - 300_000), count: 288, gap_sec: 300 };
    expect(recurrencePhrase(r)).toBe("every 5 minutes, 288 times");
    expect(recurrencePhrase({ ...r, count: 100 })).toBe("as often as every 5 minutes, 100 times");
  });

  it("counts held schedules in one sentence, and says nothing when none are", () => {
    expect(heldLine({ paused: 2, locked: 1 }, "today")).toBe(
      "2 paused schedules and 1 on a locked app would otherwise run today.",
    );
    expect(heldLine({ paused: 0, locked: 2 }, "on this day")).toBe(
      "2 schedules on locked apps would otherwise run on this day.",
    );
    expect(heldLine({ paused: 0, locked: 0 }, "today")).toBe("");
  });
});

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  displayTimeZone, formatClock, formatDateTime, formatDay, formatExactTime,
  displayDayBounds, displayToday, isDayKey, resolveDisplayTimeZone, setDisplayTimeZone, shiftDay,
  timeAgo, utcDayKeys,
} from "./time";

// Both traps are a reader's zone leaking into a UTC day, so the tests run in
// zones where it shows.
let saved: string | undefined;
beforeEach(() => {
  saved = process.env.TZ;
});
afterEach(() => {
  if (saved === undefined) delete process.env.TZ;
  else process.env.TZ = saved;
});

describe("formatDay", () => {
  it("shows a bare date as that day west of Greenwich", () => {
    process.env.TZ = "America/New_York";
    expect(formatDay("2026-10-20")).toBe("October 20, 2026");
  });

  it("shows a moment in the reader's zone", () => {
    process.env.TZ = "America/New_York";
    expect(formatDay("2026-10-20T02:00:00Z")).toBe("October 19, 2026");
  });
});

describe("utcDayKeys", () => {
  it("names each UTC day once across a DST change near midnight", () => {
    process.env.TZ = "Europe/Berlin";
    // 01:00 CET on Nov 2 is Nov 1, 00:00 UTC; the window reaches back over the
    // Oct 25 change from CEST to CET.
    const keys = utcDayKeys(10, new Date("2026-11-01T00:00:00Z"));
    expect(keys).toEqual([
      "2026-10-23", "2026-10-24", "2026-10-25", "2026-10-26", "2026-10-27",
      "2026-10-28", "2026-10-29", "2026-10-30", "2026-10-31", "2026-11-01",
    ]);
  });
});

describe("timeAgo", () => {
  it("reads a moment a little ahead of this clock as just now, not expired", () => {
    // The server stamps last-seen with its own clock; a browser a few hundred
    // milliseconds behind sees its own presence in the future.
    expect(timeAgo(new Date(Date.now() + 800).toISOString())).toBe("just now");
    expect(timeAgo(new Date(Date.now() + 30_000).toISOString())).toBe("just now");
  });

  it("still reads a real future moment as a deadline", () => {
    expect(timeAgo(new Date(Date.now() + 3 * 3600_000 + 60_000).toISOString())).toBe("in 3h");
  });
});

describe("the display zone (ADR §154)", () => {
  afterEach(() => setDisplayTimeZone(undefined));

  it("resolves a person's choice over the install's default over the browser", () => {
    expect(resolveDisplayTimeZone(undefined)).toBeUndefined();
    expect(resolveDisplayTimeZone({ user: "", install: "" })).toBeUndefined();
    expect(resolveDisplayTimeZone({ user: "", install: "America/Chicago" })).toBe("America/Chicago");
    expect(resolveDisplayTimeZone({ user: "UTC", install: "America/Chicago" })).toBe("UTC");
    // "browser" beats the install default: it is a choice, not an absence of one.
    expect(resolveDisplayTimeZone({ user: "browser", install: "America/Chicago" })).toBeUndefined();
  });

  it("formats every moment in the chosen zone, with the zone named", () => {
    const iso = "2026-11-07T19:03:22Z";
    setDisplayTimeZone("Asia/Kolkata");
    expect(formatExactTime(iso)).toBe("Nov 8, 2026, 00:33:22 GMT+5:30");
    expect(formatExactTime(iso, "UTC")).toBe("Nov 7, 2026, 19:03:22 UTC");
    expect(formatDateTime(iso)).toContain("GMT+5:30");
    expect(formatClock(iso)).toBe("00:33:22");
    // A UTC day stays a UTC day whatever the zone.
    expect(formatDay("2026-10-20")).toBe("October 20, 2026");
  });

  it("falls back to the browser for a zone this browser cannot format", () => {
    setDisplayTimeZone("Mars/Olympus");
    expect(displayTimeZone()).toBeUndefined();
  });
});

describe("a day in the display zone", () => {
  afterEach(() => setDisplayTimeZone(undefined));

  it("is 23 and 25 hours long on London's changeover days", () => {
    const spring = displayDayBounds("2026-03-29", "Europe/London");
    expect(spring.from.toISOString()).toBe("2026-03-29T00:00:00.000Z");
    expect((spring.to.getTime() - spring.from.getTime()) / 3_600_000).toBe(23);
    const autumn = displayDayBounds("2026-10-25", "Europe/London");
    expect(autumn.from.toISOString()).toBe("2026-10-24T23:00:00.000Z");
    expect((autumn.to.getTime() - autumn.from.getTime()) / 3_600_000).toBe(25);
  });

  it("starts at the first instant that exists when a changeover skips midnight", () => {
    // Beirut and Cairo spring forward at 00:00, so the day begins at 01:00
    // local -- never at 23:00 the evening before (code review 2026-11-11, O-3).
    const beirut = displayDayBounds("2026-03-29", "Asia/Beirut");
    expect(beirut.from.toISOString()).toBe("2026-03-28T22:00:00.000Z");
    expect((beirut.to.getTime() - beirut.from.getTime()) / 3_600_000).toBe(23);
    expect(displayDayBounds("2026-04-24", "Africa/Cairo").from.toISOString()).toBe("2026-04-23T22:00:00.000Z");
  });

  it("starts at the zone's midnight, west and east of Greenwich", () => {
    expect(displayDayBounds("2026-11-10", "America/New_York").from.toISOString()).toBe("2026-11-10T05:00:00.000Z");
    expect(displayDayBounds("2026-11-10", "Asia/Kolkata").from.toISOString()).toBe("2026-11-09T18:30:00.000Z");
  });

  it("names today in the zone, not in UTC", () => {
    const late = new Date("2026-11-10T03:00:00Z");
    expect(displayToday(late, "America/New_York")).toBe("2026-11-09");
    expect(displayToday(late, "Asia/Tokyo")).toBe("2026-11-10");
  });

  it("moves by calendar days and refuses a date that does not exist", () => {
    expect(shiftDay("2026-12-31", 1)).toBe("2027-01-01");
    expect(isDayKey("2026-02-30")).toBe(false);
    expect(isDayKey("2026-11-10")).toBe(true);
  });
});

import { describe, expect, it } from "vitest";
import {
  expectWithinValid, formatExpectWithin, joinSchedule, scheduleLabel, splitSchedule,
  zonedWallToInstant, zoneLooksValid,
} from "./schedule";

describe("schedule zones", () => {
  it("round-trips a zoned expression", () => {
    const stored = joinSchedule("Europe/London", "0 6 * * *");
    expect(stored).toBe("CRON_TZ=Europe/London 0 6 * * *");
    expect(splitSchedule(stored)).toEqual({ zone: "Europe/London", expr: "0 6 * * *" });
  });

  it("stores an unzoned or UTC expression bare", () => {
    expect(joinSchedule("", "0 3 * * *")).toBe("0 3 * * *");
    expect(joinSchedule("UTC", "0 3 * * *")).toBe("0 3 * * *");
    expect(joinSchedule("Europe/London", "")).toBe("");
  });

  it("reads TZ= as well as CRON_TZ=", () => {
    expect(splitSchedule("TZ=Asia/Tokyo */5 * * * *")).toEqual({ zone: "Asia/Tokyo", expr: "*/5 * * * *" });
  });

  it("never shows the prefix in a label", () => {
    expect(scheduleLabel("CRON_TZ=Europe/London 0 6 * * *")).toBe("0 6 * * * · Europe/London");
    expect(scheduleLabel("0 3 * * *")).toBe("0 3 * * *");
    expect(scheduleLabel(null)).toBe("");
  });

  it("checks a zone against the known list", () => {
    expect(zoneLooksValid("", ["Europe/London"])).toBe(true);
    expect(zoneLooksValid("Europe/London", ["Europe/London"])).toBe(true);
    expect(zoneLooksValid("Nowhere/Bad", ["Europe/London"])).toBe(false);
    expect(zoneLooksValid("Nowhere/Bad", [])).toBe(true);
  });
});

describe("zonedWallToInstant", () => {
  it("reads a wall-clock time in its zone, on both sides of daylight saving", () => {
    expect(zonedWallToInstant("2026-07-06T03:00", "Europe/London")?.toISOString()).toBe("2026-07-06T02:00:00.000Z");
    expect(zonedWallToInstant("2026-01-06T03:00", "Europe/London")?.toISOString()).toBe("2026-01-06T03:00:00.000Z");
    expect(zonedWallToInstant("2026-07-06T03:00", "America/New_York")?.toISOString()).toBe("2026-07-06T07:00:00.000Z");
  });

  it("reads no zone as UTC and refuses what is not a time", () => {
    expect(zonedWallToInstant("2026-10-06T03:00", "")?.toISOString()).toBe("2026-10-06T03:00:00.000Z");
    expect(zonedWallToInstant("tuesday", "Europe/London")).toBeNull();
  });
});

describe("expect within", () => {
  it("formats what was stored the way it was typed", () => {
    expect(formatExpectWithin(26 * 3600)).toBe("26h");
    expect(formatExpectWithin(7 * 86400)).toBe("7d");
    expect(formatExpectWithin(undefined)).toBe("");
  });

  it("checks the server's bounds", () => {
    expect(expectWithinValid("")).toBe(true);
    expect(expectWithinValid("26h")).toBe(true);
    expect(expectWithinValid("31d")).toBe(true);
    expect(expectWithinValid("30m")).toBe(false);
    expect(expectWithinValid("32d")).toBe(false);
    expect(expectWithinValid("daily")).toBe(false);
  });
});

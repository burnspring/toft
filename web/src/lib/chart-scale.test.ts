import { describe, expect, it } from "vitest";
import { niceCeiling, niceScale } from "./chart-scale";

describe("niceCeiling", () => {
  it("rounds up to a number somebody would have chosen", () => {
    expect(niceCeiling(58)).toBe(60);
    expect(niceCeiling(62)).toBe(70);
    expect(niceCeiling(235)).toBe(250);
    expect(niceCeiling(1_100)).toBe(1_500);
  });

  it("never lands below the value it has to hold", () => {
    for (const n of [1, 2, 3, 7, 19, 58, 62, 99, 101, 999, 1234, 87_654]) {
      expect(niceCeiling(n)).toBeGreaterThanOrEqual(n);
    }
  });

  it("keeps the midpoint whole for small counts", () => {
    // A ceiling of 5 would label the middle gridline 2.5, and there is no such
    // thing as half a request.
    for (const n of [1, 2, 3, 4, 5, 6, 7, 8, 9]) {
      const { ticks } = niceScale(n);
      for (const t of ticks) expect(Number.isInteger(t)).toBe(true);
    }
  });

  it("holds a floor, so an empty series still draws an axis", () => {
    expect(niceCeiling(0)).toBe(2);
    expect(niceCeiling(-5)).toBe(2);
  });
});

describe("niceScale", () => {
  it("puts the top tick on the ceiling", () => {
    const { top, ticks } = niceScale(58);
    expect(top).toBe(60);
    expect(ticks).toEqual([30, 60]);
  });
});

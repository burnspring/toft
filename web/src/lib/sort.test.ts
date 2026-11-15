import { describe, expect, it } from "vitest";
import { compareDatesMissingLast } from "./sort";

describe("compareDatesMissingLast", () => {
  const dates = [undefined, "2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z", null];
  const sorted = (dir: "asc" | "desc") =>
    [...dates].sort((a, b) => compareDatesMissingLast(a, b, dir));

  // Array.prototype.sort moves `undefined` to the end without asking the
  // comparator, so `null` is the case that exercises the pin.
  it("puts an undated entry last in both directions", () => {
    for (const dir of ["asc", "desc"] as const) {
      const tail = sorted(dir).slice(2);
      expect(tail).toHaveLength(2);
      expect(tail).toEqual(expect.arrayContaining([undefined, null]));
    }
  });

  it("orders the dated entries by direction", () => {
    expect(sorted("asc").slice(0, 2)).toEqual(["2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z"]);
    expect(sorted("desc").slice(0, 2)).toEqual(["2026-03-01T00:00:00Z", "2026-01-02T00:00:00Z"]);
  });
});

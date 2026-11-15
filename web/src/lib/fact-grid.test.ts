import { describe, it, expect } from "vitest";
import { statColumns } from "./fact-grid";

/**
 * A table over the counts the product actually renders. The defect this
 * replaces was a grid that declared four columns and drew three tiles, which
 * type-checks, lints and renders — so the only thing that can catch it is a
 * case saying what three tiles are supposed to get.
 */
describe("statColumns", () => {
  const cases: Array<[number, string]> = [
    [1, ""],
    [2, "sm:grid-cols-2"],
    [3, "sm:grid-cols-2 lg:grid-cols-3"],
    [4, "sm:grid-cols-2 lg:grid-cols-4"],
  ];

  for (const [n, expected] of cases) {
    it(`${n} tile${n === 1 ? "" : "s"} → ${expected || "one column"}`, () => {
      expect(statColumns(n)).toBe(expected);
    });
  }

  // A lone tile takes the grid's own single column rather than a third of a
  // row; zero tiles never reaches here, and would be the same answer if it did.
  it("never promises a column it has no tile for", () => {
    for (let n = 1; n <= 4; n++) {
      const widest = Number(statColumns(n).match(/grid-cols-(\d)(?!.*grid-cols-)/)?.[1] ?? 1);
      expect(widest).toBeLessThanOrEqual(n);
    }
  });
});

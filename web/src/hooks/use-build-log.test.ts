import { describe, expect, it } from "vitest";
import { parseStamped } from "./use-build-log";

describe("parseStamped (ADR §155)", () => {
  it("splits each line into its time and its text", () => {
    const { lines, at } = parseStamped("1762542202000\t==> Fetch\n-\tafter the build\n");
    expect(lines).toEqual(["==> Fetch", "after the build"]);
    expect(at).toEqual(["2025-11-07T19:03:22.000Z", undefined]);
  });

  it("keeps a tab inside the line's own text", () => {
    const { lines } = parseStamped("5\tcol1\tcol2\n");
    expect(lines).toEqual(["col1\tcol2"]);
  });

  it("takes an unstamped line whole, unmarked", () => {
    const { lines, at } = parseStamped("plain\tline\n");
    expect(lines).toEqual(["plain\tline"]);
    expect(at).toEqual([undefined]);
  });
});

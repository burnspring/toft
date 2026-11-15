import { describe, expect, it } from "vitest";

import { readWaiverDays } from "./waiver-days";

describe("readWaiverDays", () => {
  // The defect this exists for: `Number("")` is 0, the server reads a
  // non-positive `days` as "use the install's cap", and the toast said "for 0
  // days". Clearing the box granted the *longest* waiver the install allows.
  it("refuses a cleared field rather than sending a nought", () => {
    expect(readWaiverDays("", 90).days).toBeUndefined();
    expect(readWaiverDays("", 90).error).toBeTruthy();
    expect(readWaiverDays("   ", 90).days).toBeUndefined();
  });

  it("refuses zero and anything below a day", () => {
    expect(readWaiverDays("0", 90).days).toBeUndefined();
    expect(readWaiverDays("-5", 90).days).toBeUndefined();
  });

  it("refuses what is not a whole number of days", () => {
    expect(readWaiverDays("7.5", 90).days).toBeUndefined();
    expect(readWaiverDays("thirty", 90).days).toBeUndefined();
  });

  it("names the install's cap when the draft is over it", () => {
    const r = readWaiverDays("120", 90);
    expect(r.days).toBeUndefined();
    expect(r.error).toContain("90");
  });

  it("passes a value inside the bound through unchanged", () => {
    expect(readWaiverDays("30", 90)).toEqual({ days: 30 });
    expect(readWaiverDays("90", 90)).toEqual({ days: 90 });
    expect(readWaiverDays(" 1 ", 90)).toEqual({ days: 1 });
  });

  it("applies no ceiling when the cap is unknown", () => {
    // The dialog is not reachable on an install whose cap is 0 — that is
    // waivers switched off — but a state read that has not answered yet must
    // not invent a bound of zero and refuse every value.
    expect(readWaiverDays("400", 0)).toEqual({ days: 400 });
  });
});

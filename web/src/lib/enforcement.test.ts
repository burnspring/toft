import { describe, expect, it } from "vitest";
import { enforcementPhrase, throttledHour } from "@/lib/enforcement";

describe("enforcement", () => {
  it("says each of the three words, and nothing for none", () => {
    expect(enforcementPhrase("enforced")).toMatch(/kernel/);
    expect(enforcementPhrase("sampled")).toMatch(/sampling/);
    expect(enforcementPhrase("not_enforced")).toMatch(/Not enforced/);
    expect(enforcementPhrase(undefined)).toBeUndefined();
  });

  it("reads an hour of periods as minutes", () => {
    expect(throttledHour({ periods: 36000, throttled_periods: 11160 })).toEqual({ held: 19, work: 60, percent: 31 });
    expect(throttledHour({ periods: 0, throttled_periods: 0 })).toBeUndefined();
    expect(throttledHour(undefined)).toBeUndefined();
  });
});

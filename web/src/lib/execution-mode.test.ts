import { describe, expect, it } from "vitest";
import { anyIsolated, pendingMode, runningMode } from "@/lib/execution-mode";

/** The apps list only ever reads these two fields to decide a mode. */
const app = (execution_mode: "shared" | "isolated", running_mode?: "shared" | "isolated") =>
  ({ execution_mode, running_mode });

describe("runningMode", () => {
  it("reads the serving release, not the setting", () => {
    expect(runningMode(app("isolated", "shared"))).toBe("shared");
  });

  it("treats a row written before per-deployment modes as shared", () => {
    expect(runningMode(app("isolated"))).toBe("shared");
  });
});

describe("pendingMode", () => {
  it("is the setting when it differs from what is serving", () => {
    expect(pendingMode(app("isolated", "shared"))).toBe("isolated");
  });

  it("is undefined once the app is already there", () => {
    expect(pendingMode(app("isolated", "isolated"))).toBeUndefined();
  });
});

describe("anyIsolated", () => {
  // The regression this exists for: three isolated apps and nothing else used
  // to count as one distinct mode, which hid the Mode column on exactly the
  // install whose operator most wants it.
  it("is true when every app is isolated", () => {
    expect(anyIsolated([
      app("isolated", "isolated"),
      app("isolated", "isolated"),
      app("isolated", "isolated"),
    ])).toBe(true);
  });

  it("is false when everything is shared", () => {
    expect(anyIsolated([app("shared", "shared"), app("shared")])).toBe(false);
  });

  it("is true on a mixed install", () => {
    expect(anyIsolated([app("shared", "shared"), app("isolated", "isolated")])).toBe(true);
  });

  // A promotion that has not been redeployed yet is still a reason to show the
  // column: the row has something to say that shared rows do not.
  it("counts an app only set to isolated", () => {
    expect(anyIsolated([app("isolated", "shared")])).toBe(true);
  });

  it("is false for an empty list", () => {
    expect(anyIsolated([])).toBe(false);
  });
});

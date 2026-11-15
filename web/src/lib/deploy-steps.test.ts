import { describe, expect, it } from "vitest";

import type { Deployment } from "@/lib/api";
import { expectedDeploy } from "@/lib/deploy-steps";

function dep(over: Partial<Deployment>): Deployment {
  return {
    id: "d",
    status: "success",
    started_at: "2026-10-23T10:00:00Z",
    finished_at: "2026-10-23T10:00:40Z",
    ...over,
  } as Deployment;
}

describe("expectedDeploy", () => {
  it("reads the last successful deploy other than the one running", () => {
    const got = expectedDeploy(
      [
        dep({ id: "now", status: "building", finished_at: "" }),
        dep({ id: "failed", status: "failed" }),
        dep({ id: "ok", step_timings: JSON.stringify([{ step: "build", ms: 30_000 }, { step: "start", ms: 2_000 }]) }),
      ],
      "now",
    );
    expect(got).toEqual({ totalMs: 40_000, slowest: { label: "Building", ms: 30_000 } });
  });

  it("names the slowest step only when it was at least half the wait", () => {
    const got = expectedDeploy([dep({ step_timings: JSON.stringify([{ step: "build", ms: 10_000 }]) })]);
    expect(got).toEqual({ totalMs: 40_000, slowest: undefined });
  });

  it("is nothing when no deploy has succeeded", () => {
    expect(expectedDeploy([dep({ status: "failed" })])).toBeNull();
    expect(expectedDeploy([])).toBeNull();
  });
});

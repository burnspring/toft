import { describe, expect, it } from "vitest";
import { deploymentsPollMs, headerPrimary } from "./state";

const base = { status: "running", canEdit: true, locked: false, batch: false, firstDeployFailed: false };

describe("headerPrimary", () => {
  it.each([
    [{}, "open"],
    [{ status: "sleeping" }, "open"],
    [{ status: "stopped" }, "start"],
    [{ status: "crashed" }, "restart"],
    [{ status: "stopped", locked: true }, "open"],
    [{ status: "stopped", canEdit: false }, "open"],
    [{ status: "crashed", canEdit: false }, "open"],
    [{ firstDeployFailed: true, status: "failed" }, "build_log"],
    [{ firstDeployFailed: true, canEdit: false }, "open"],
    [{ batch: true }, "none"],
    [{ batch: true, status: "stopped" }, "none"],
    [{ batch: true, firstDeployFailed: true }, "build_log"],
  ] as const)("%o → %s", (over, want) => {
    expect(headerPrimary({ ...base, ...over })).toBe(want);
  });
});

describe("deploymentsPollMs", () => {
  it("polls fast while the list or the app row says a build is running, and never stops", () => {
    expect(deploymentsPollMs([], "building")).toBe(3000);
    expect(deploymentsPollMs([{ status: "building" }], "running")).toBe(3000);
    expect(deploymentsPollMs([{ status: "pending" }], undefined)).toBe(3000);
    expect(deploymentsPollMs([{ status: "success" }], "running")).toBe(15000);
    expect(deploymentsPollMs(undefined, undefined)).toBe(15000);
  });
});

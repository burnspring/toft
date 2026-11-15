import { describe, expect, it } from "vitest";

import {
  defaultLogSource,
  pickBuild,
  readLogSource,
} from "./log-source";
import type { Deployment } from "./api";

function build(over: Partial<Deployment> = {}): Deployment {
  return {
    id: "d1",
    app_id: "a1",
    commit_sha: "",
    branch: "main",
    status: "success",
    triggered_by: "manual",
    build_log: "",
    disk_bytes: null,
    artifact_bytes: null,
    started_at: "2026-08-30T10:00:00Z",
    ...over,
  } as Deployment;
}

describe("which log the tab opens on", () => {
  it("opens on the build log while a deploy is in flight", () => {
    expect(defaultLogSource({ building: true, lastFailed: false })).toBe("build");
  });

  it("opens on the build log when the last deploy failed", () => {
    expect(defaultLogSource({ building: false, lastFailed: true })).toBe("build");
  });

  it("opens on the run log otherwise", () => {
    expect(defaultLogSource({ building: false, lastFailed: false })).toBe("run");
  });
});

describe("the source named in the URL", () => {
  it("takes either recognised value", () => {
    expect(readLogSource("build", "run")).toBe("build");
    expect(readLogSource("run", "build")).toBe("run");
  });

  it("falls back on anything else, including nothing", () => {
    expect(readLogSource(null, "build")).toBe("build");
    expect(readLogSource(undefined, "run")).toBe("run");
    expect(readLogSource("", "run")).toBe("run");
    expect(readLogSource("BUILD", "run")).toBe("run");
    expect(readLogSource("workload", "build")).toBe("build");
  });
});

describe("which build the build log shows", () => {
  const inFlight = build({ id: "running", status: "building" });
  const newest = build({ id: "newest" });
  const older = build({ id: "older" });

  it("shows the build the URL names", () => {
    expect(pickBuild([newest, older], "older")?.id).toBe("older");
  });

  it("shows the one in flight when the URL names none", () => {
    expect(pickBuild([inFlight, newest, older])?.id).toBe("running");
  });

  it("shows the newest when nothing is in flight", () => {
    expect(pickBuild([newest, older])?.id).toBe("newest");
  });

  it("falls through a build id the list does not hold", () => {
    expect(pickBuild([newest, older], "pruned")?.id).toBe("newest");
  });

  it("has nothing to show for an app that has never built", () => {
    expect(pickBuild([], "anything")).toBeUndefined();
  });
});

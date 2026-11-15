import { describe, expect, it } from "vitest";
import type { DiskReport } from "@/lib/api";
import { disksBeyondTheRows, diskTone, fullnessLabel } from "@/lib/disks";

const disk = (over: Partial<DiskReport>): DiskReport => ({
  folders: [{ path: "/var/lib/toft", what: "App releases" }],
  total_bytes: 100,
  used_bytes: 60,
  free_bytes: 40,
  used_percent: 60,
  level: "ok",
  ...over,
});

describe("disks", () => {
  it("colours only a disk past a watermark", () => {
    expect(diskTone(disk({}))).toBeUndefined();
    expect(diskTone(disk({ level: "low" }))).toBe("attention");
    expect(diskTone(disk({ level: "full" }))).toBe("danger");
  });

  it("never says 0% of a disk with something on it", () => {
    expect(fullnessLabel(disk({ used_percent: 61.6 }))).toBe("62% full");
    expect(fullnessLabel(disk({ used_percent: 0.3 }))).toBe("Under 1% full");
    expect(fullnessLabel(disk({ used_percent: 0 }))).toBe("0% full");
  });

  it("keeps only what the checks table has no row for", () => {
    const releases = disk({});
    const drive = disk({ folders: [{ path: "/mnt/team", what: "Storage drive “Team”" }] });
    expect(disksBeyondTheRows({ disks: [releases] })).toBeNull();
    expect(disksBeyondTheRows({ disks: [releases, drive] })?.disks).toEqual([drive]);
    const hung = { path: "/mnt/x", what: "Logs", error: "timed out" };
    expect(disksBeyondTheRows({ disks: [releases], unanswered: [hung] })).toEqual({
      disks: [],
      unanswered: [hung],
    });
  });
});

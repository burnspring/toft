import { describe, it, expect } from "vitest";
import { removedSummary } from "./removed";
import { formatBytes } from "./bytes";

describe("removedSummary", () => {
  it("says nothing when nothing was removed", () => {
    expect(removedSummary(undefined)).toBe("");
    expect(removedSummary([])).toBe("");
  });

  it("names the largest thing that went", () => {
    expect(removedSummary([{ path: "node_modules", files: 12431, bytes: 193164902 }])).toBe(
      "184.2 MB of node_modules was removed before installing."
    );
  });

  // Two is worth naming; more than two is a count, because a toast that lists
  // four directories is a toast nobody finishes reading.
  it("names a second, and counts the rest", () => {
    const two = removedSummary([
      { path: "node_modules", files: 12431, bytes: 193164902 },
      { path: ".git", files: 892, bytes: 23803821 },
    ]);
    expect(two).toContain("and .git");

    const many = removedSummary([
      { path: "node_modules", files: 1, bytes: 3 },
      { path: ".git", files: 1, bytes: 2 },
      { path: ".DS_Store", files: 1, bytes: 1 },
    ]);
    expect(many).toContain("and 2 more");
  });
});

// The same shape the build log writes, so the two reports of one fact do not
// disagree in front of somebody comparing them.
describe("formatBytes", () => {
  it("matches the server's rendering", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(193164902)).toBe("184.2 MB");
    expect(formatBytes(2 * 1024 ** 3)).toBe("2.0 GB");
    expect(formatBytes(-1)).toBe("--");
  });
});

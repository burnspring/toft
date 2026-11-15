import { describe, expect, it } from "vitest";

import {
  defaultFolded,
  firstErrorLine,
  isErrorLine,
  lineHash,
  logSections,
  parseLineHash,
  sectionOf,
} from "./log-view";

const LOG = [
  "Removed from the upload: .git/",
  "==> Cloning…",
  "    Cloning into 'x'",
  "==> Installing dependencies…",
  "    added 412 packages",
  "    npm ERR! code ERESOLVE",
  "ERROR: dependency install failed",
];

describe("a build log's steps", () => {
  it("starts a step at each ==> line and runs it to the next", () => {
    expect(logSections(LOG)).toEqual([
      { start: 1, end: 3 },
      { start: 3, end: 7 },
    ]);
  });

  it("leaves the lines before the first step outside every step", () => {
    expect(sectionOf(logSections(LOG), 0)).toBe(-1);
    expect(sectionOf(logSections(LOG), 2)).toBe(1);
    expect(sectionOf(logSections(LOG), 6)).toBe(3);
  });

  it("has no steps when nothing narrated one", () => {
    expect(logSections(["a", "b"])).toEqual([]);
  });
});

describe("which steps open folded", () => {
  const sections = logSections(LOG);

  it("folds every step but the one a failed build died in", () => {
    expect([...defaultFolded(sections, true)]).toEqual([1]);
  });

  it("folds nothing for a build that worked or is still working", () => {
    expect(defaultFolded(sections, false).size).toBe(0);
  });

  it("folds nothing when there is only one step", () => {
    expect(defaultFolded([{ start: 0, end: 4 }], true).size).toBe(0);
  });
});

describe("what an error line is", () => {
  it("finds the first failure, past the builder's indent", () => {
    expect(firstErrorLine(LOG)).toBe(5);
  });

  it.each([
    "ERROR: build failed",
    "    error: could not compile `app`",
    "    error[E0425]: cannot find value",
    "    Traceback (most recent call last):",
    "    fatal: repository not found",
    "    Execution halted",
  ])("counts %j", (line) => {
    expect(isErrorLine(line)).toBe(true);
  });

  it.each([
    "    found 0 errors",
    "    + error-stack-parser@2.1.4",
    "    Compiled with warnings",
  ])("does not count %j", (line) => {
    expect(isErrorLine(line)).toBe(false);
  });

  it("answers -1 for a log with no failure", () => {
    expect(firstErrorLine(["ok", "fine"])).toBe(-1);
  });
});

describe("a line's address", () => {
  it("is one-based, as people count", () => {
    expect(lineHash(0)).toBe("#L1");
    expect(parseLineHash("#L1")).toBe(0);
    expect(parseLineHash("#L412")).toBe(411);
  });

  it("names nothing for any other fragment", () => {
    expect(parseLineHash("")).toBeNull();
    expect(parseLineHash("#L0")).toBeNull();
    expect(parseLineHash("#settings")).toBeNull();
    expect(parseLineHash("#L12x")).toBeNull();
  });
});

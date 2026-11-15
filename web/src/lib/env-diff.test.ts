import { describe, expect, it } from "vitest";
import { envChanges, envSnapshotGrown, hiddenRenames } from "./env-diff";

const stored = [
  { key: "SECRET", sensitive: true },
  { key: "EMPTY_SECRET", sensitive: true, unset: true },
  { key: "MODE", value: "prod", sensitive: false },
  { key: "GONE", value: "x", sensitive: false },
];
const asStored = [
  { key: "SECRET", value: "", sensitive: true },
  { key: "EMPTY_SECRET", value: "", sensitive: true },
  { key: "MODE", value: "prod", sensitive: false },
  { key: "GONE", value: "x", sensitive: false },
];

describe("the Settings form's environment diff", () => {
  it("sends nothing for an untouched form", () => {
    expect(envChanges(stored, asStored)).toBeNull();
  });

  it("maps removed, added, edited, flipped and blank-sensitive rows", () => {
    const rows = [
      { key: "SECRET", value: "", sensitive: true }, // blank sensitive: not mentioned
      { key: "EMPTY_SECRET", value: "filled", sensitive: true }, // typed: set
      { key: "MODE", value: "dev", sensitive: false }, // edited
      { key: "NEW", value: "n", sensitive: false }, // added
      // GONE removed
    ];
    expect(envChanges(stored, rows)).toEqual({
      set: {
        EMPTY_SECRET: { value: "filled", sensitive: true },
        MODE: { value: "dev", sensitive: false },
        NEW: { value: "n", sensitive: false },
      },
      unset: ["GONE"],
    });
  });

  it("sends a visible row locked as a flag change with its value", () => {
    const rows = asStored.map((r) => (r.key === "MODE" ? { ...r, sensitive: true } : r));
    expect(envChanges(stored, rows)?.set).toEqual({ MODE: { value: "prod", sensitive: true } });
  });

  it("never reveals a stored secret by its flag", () => {
    const rows = asStored.map((r) => (r.key === "SECRET" ? { ...r, sensitive: false } : r));
    // No value: the install refuses it rather than storing "" visibly.
    expect(envChanges(stored, rows)?.set).toEqual({ SECRET: { sensitive: false } });
  });

  it("treats a renamed row as an unset and a set", () => {
    const rows = asStored.map((r) => (r.key === "GONE" ? { ...r, key: "RENAMED" } : r));
    expect(envChanges(stored, rows)).toEqual({
      set: { RENAMED: { value: "x", sensitive: false } },
      unset: ["GONE"],
    });
  });

  it("diffs against the snapshot, so an untouched row is never sent back stale", () => {
    const seed = [
      { key: "API_URL", value: "old", sensitive: false },
      { key: "LOG_LEVEL", value: "info", sensitive: false },
    ];
    const rows = [
      { key: "API_URL", value: "old", sensitive: false },
      { key: "LOG_LEVEL", value: "debug", sensitive: false },
    ];
    expect(envChanges(seed, rows)).toEqual({
      set: { LOG_LEVEL: { value: "debug", sensitive: false } },
      unset: [],
    });
    const live = [
      { key: "API_URL", value: "new", sensitive: false },
      { key: "LOG_LEVEL", value: "info", sensitive: false },
      { key: "ADDED", value: "a", sensitive: false },
    ];
    expect(envSnapshotGrown(seed, live)).toEqual([...seed, { key: "ADDED", value: "a", sensitive: false }]);
  });

  it("keeps a key the server dropped in the snapshot, so its untouched row is not re-created", () => {
    const seed = [
      { key: "KEEP", value: "k", sensitive: false },
      { key: "DROPPED", sensitive: true },
    ];
    const live = [{ key: "KEEP", value: "k", sensitive: false }];
    const snapshot = envSnapshotGrown(seed, live);
    expect(snapshot).toEqual(seed);
    const rows = [
      { key: "KEEP", value: "k", sensitive: false },
      { key: "DROPPED", value: "", sensitive: true },
    ];
    expect(envChanges(snapshot, rows)).toBeNull();
  });

  it("names a hidden variable renamed with no value typed", () => {
    const seeded = asStored.map((r) => ({ ...r, seededKey: r.key }));
    const rename = (key: string, to: string, value = "") =>
      seeded.map((r) => (r.key === key ? { ...r, key: to, value: value || r.value } : r));
    expect(hiddenRenames(stored, seeded)).toEqual([]);
    expect(hiddenRenames(stored, rename("SECRET", " RENAMED "))).toEqual(["RENAMED"]);
    expect(hiddenRenames(stored, rename("SECRET", "RENAMED", "typed"))).toEqual([]);
    expect(hiddenRenames(stored, rename("MODE", "RENAMED"))).toEqual([]);
    expect(hiddenRenames(stored, rename("EMPTY_SECRET", "RENAMED"))).toEqual([]);
    // An added row is never one, whatever its name.
    expect(hiddenRenames(stored, [...seeded, { key: "NEW", value: "", sensitive: true }])).toEqual([]);
  });
});

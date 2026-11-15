import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { mounts, storage, store } from "../app.js";
import { ToftError } from "../transport.js";

// The store accessor and Mount.put — sugar over the mount, and no second write
// path. These are the same scenarios the Python SDK passes, so an app written
// in one language cannot observe a platform the other cannot.

async function fixture() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "toft-store-"));
  const mounted = path.join(root, "mounted");
  await fs.mkdir(mounted);
  const source = path.join(root, "nightly.parquet");
  await fs.writeFile(source, "the nightly file");

  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "reports", kind: "store", path: mounted, access: "write" },
    { name: "cache", kind: "volume", path: mounted, access: "write" },
    { name: "archive", kind: "store", path: mounted, access: "read" },
  ]);
  return { root, mounted, source };
}

test("put writes to the mounted path", async () => {
  const { mounted, source } = await fixture();
  const landed = await store("reports").put(source, "2026-08.parquet");
  assert.equal(landed, path.join(mounted, "2026-08.parquet"));
  assert.equal(await fs.readFile(landed, "utf8"), "the nightly file");
});

test("put defaults to the file's own name", async () => {
  const { source } = await fixture();
  const landed = await store("reports").put(source);
  assert.equal(path.basename(landed), "nightly.parquet");
});

test("put creates intermediate folders", async () => {
  const { mounted, source } = await fixture();
  const landed = await store("reports").put(source, "2026/08/17.parquet");
  assert.equal(path.dirname(landed), path.join(mounted, "2026", "08"));
  assert.equal(await fs.readFile(landed, "utf8"), "the nightly file");
});

// The name is the caller's and the location is not. A traversal segment here
// would write into the app's own filesystem — over its code, in the worst case.
test("a name cannot escape the mount", async () => {
  const { source } = await fixture();
  for (const name of ["../escape", "a/../../escape", "/absolute", "a//b", "a/./b", "a/", ".."]) {
    await assert.rejects(() => store("reports").put(source, name), ToftError, name);
  }
});

// A courtesy, not the enforcement — the mount is the enforcement.
test("a read-only mount says so", async () => {
  const { source } = await fixture();
  await assert.rejects(() => store("archive").put(source), (err) => {
    assert.match(err.message, /read-only/);
    return true;
  });
});

test("store refuses a volume by kind", async () => {
  await fixture();
  assert.throws(() => store("cache"), /volume/);
});

// App code that has a volume mounted has done nothing wrong.
test("storage still answers for both kinds", async () => {
  await fixture();
  assert.equal(storage("cache").kind, "volume");
  assert.equal(storage("reports").kind, "store");
});

test("a miss names what is mounted", async () => {
  await fixture();
  assert.throws(() => store("nope"), /reports/);
});

// Every rung above `read` is writable, and a check that only knew about
// `write` would refuse `full` — the only level above read a volume has.
test("writable covers every rung above read", async () => {
  for (const [access, want] of [
    ["read", false],
    ["write", true],
    ["full", true],
  ]) {
    process.env.TOFT_STORAGE = JSON.stringify([
      { name: "r", kind: "store", path: "/tmp", access },
    ]);
    assert.equal(mounts()[0].writable, want, access);
    assert.equal(mounts()[0].readOnly, !want, access);
  }
  // A volume's only level above read is `full`.
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "v", kind: "volume", path: "/tmp", access: "full" },
  ]);
  assert.equal(mounts()[0].writable, true, "volume at full");
});

// One resource, mapped twice. A mapping's identity is (resource, app, folder),
// so this is a shape an administrator can make on purpose — a read corpus and a
// write drop, at two paths in one workload — and both arrive under one name.
// The same scenarios the Python SDK passes.

function twoMappings() {
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "drop", kind: "store", path: "/data/backup", access: "write", folder: "backup" },
    { name: "drop", kind: "store", path: "/data/drop", access: "read" },
  ]);
}

test("the folder reaches app code", () => {
  twoMappings();
  const byFolder = Object.fromEntries(mounts().map((m) => [m.folder, m]));
  assert.deepEqual(Object.keys(byFolder).sort(), ["", "backup"]);
  assert.equal(byFolder.backup.path, "/data/backup");
  assert.equal(byFolder.backup.access, "write");
});

// Returning the first match made the choice by the alphabetical order of paths
// an administrator chose — the coupling TOFT_STORAGE exists to prevent,
// arriving through the accessor. So the refusal names both folders and both
// levels, the level being usually why there are two.
test("an ambiguous name is refused rather than resolved", () => {
  twoMappings();
  assert.throws(() => storage("drop"), (err) => {
    assert.ok(err instanceof ToftError);
    assert.match(err.message, /folder "backup" at write/);
    assert.match(err.message, /folder "" at read/);
    assert.match(err.message, /folder:/);
    return true;
  });
});

test("naming the folder answers", () => {
  twoMappings();
  assert.equal(storage("drop", { folder: "backup" }).path, "/data/backup");
  assert.equal(storage("drop", { folder: "" }).path, "/data/drop");
  assert.equal(store("drop", { folder: "backup" }).path, "/data/backup");
});

test("a folder that is not mounted names the ones that are", () => {
  twoMappings();
  assert.throws(() => storage("drop", { folder: "archive" }), (err) => {
    assert.match(err.message, /not at folder "archive"/);
    assert.match(err.message, /folder "backup" at write/);
    return true;
  });
});

test("an install that names no folder is unchanged", () => {
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "reports", kind: "volume", path: "/data/r", access: "write" },
  ]);
  const mount = storage("reports");
  assert.equal(mount.folder, "");
  assert.equal(mount.path, "/data/r");
});

// The SDK's own refusals carry the envelope: a code to branch on, and
// retryable false — a wrong name or a missing mount is not a blip.
// store("x") was told to call toft.storage(...), which is not what a store
// wants (drills D17): the refusal names the accessor the caller used.
test("ambiguous_mount names the accessor the caller used", () => {
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "drop", kind: "store", path: "", access: "read" },
    { name: "drop", kind: "store", path: "", access: "write", folder: "backup" },
  ]);
  assert.throws(() => store("drop"), (err) => err.message.includes('toft.store("drop", { folder: '));
  assert.throws(() => storage("drop"), (err) => err.message.includes('toft.storage("drop", { folder: '));
});

test("an SDK-made storage refusal is coded and not retryable", async () => {
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "drop", kind: "store", path: "", access: "read" },
    { name: "drop", kind: "store", path: "", access: "write", folder: "backup" },
  ]);
  const cases = [
    [() => store("drop", { folder: "" }).path, "not_mounted"],
    [() => store("drop"), "ambiguous_mount"],
    [() => storage("nope"), "not_mapped"],
    [() => storage(""), "invalid_name"],
    [() => storage(), "invalid_name"],
    [() => store("drop", { folder: "elsewhere" }), "not_mapped"],
  ];
  for (const [fn, code] of cases) {
    try {
      fn();
      assert.fail(`no refusal for ${code}`);
    } catch (err) {
      assert.ok(err instanceof ToftError, code);
      assert.equal(err.code, code);
      assert.equal(err.retryable, false, code);
      // Each one has a next step, so it names one (drills 2026-10-28 #10).
      assert.ok(err.hint, `${code} has no hint`);
    }
  }
});

test("ambiguous_mount's hint names the folder option", () => {
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "drop", kind: "store", path: "", access: "read" },
    { name: "drop", kind: "store", path: "", access: "write", folder: "backup" },
  ]);
  assert.throws(() => store("drop"), (err) => err.hint.includes('toft.store("drop", { folder: '));
});

test("a refused put() names the next step", async () => {
  process.env.TOFT_STORAGE = JSON.stringify([{ name: "drop", kind: "store", path: "", access: "read" }]);
  await assert.rejects(store("drop").put("/etc/hostname", "../x"),
    (err) => err.code === "invalid_name" && err.hint.includes("relative name"));
  await assert.rejects(store("drop").put("/etc/hostname"),
    (err) => err.code === "not_mounted" && err.hint.includes("upload()"));
});

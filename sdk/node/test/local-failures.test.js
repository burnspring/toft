/**
 * Failures the SDK finds locally — a path, an argument, the environment — are
 * coded and never retryable. Only a real transport failure may leave `code`
 * empty and `retryable` true; anything else is an agent's loop retrying a
 * missing file forever.
 *
 * `node --test` from `sdk/node`, with nothing installed. Each regression names
 * the bug-bash finding it closes.
 */

import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { mkdirSync, mkdtempSync, rmSync, statSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";

import toft, { ACT_FOR_HEADER, JobRun, Run, ToftError, mounts, storage, store } from "../index.js";

const PLATFORM_ENV = [
  "TOFT_API_URL", "TOFT_APP_ID", "TOFT_APP_SLUG", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR",
  "TOFT_STORAGE", "TOFT_APP_TOKEN", "TOFT_JOB_TOKEN", "TOFT_ROLE_KEY",
];
let saved;

beforeEach(() => {
  saved = Object.fromEntries(PLATFORM_ENV.map((k) => [k, process.env[k]]));
  for (const k of PLATFORM_ENV) delete process.env[k];
});

afterEach(() => {
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

const coded = (code) => (err) => err instanceof ToftError && err.code === code && err.retryable === false;

describe("scratchDir stays inside its root (BB-006)", () => {
  let root;
  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), "toft-scratch-"));
    process.env.TOFT_SCRATCH_DIR = root;
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  it("refuses parts that climb out", () => {
    assert.throws(() => toft.scratchDir("..", "..", "etc"), coded("bad_request"));
    assert.throws(() => toft.scratchDir("a", "..", "..", "b"), coded("bad_request"));
  });

  it("refuses an absolute part", () => {
    assert.throws(() => toft.scratchDir("/etc"), coded("bad_request"));
    assert.throws(() => toft.scratchDir("a", "/etc"), coded("bad_request"));
  });

  it("refuses a part that is not a string", () => {
    assert.throws(() => toft.scratchDir(42), coded("bad_request"));
  });

  it("still allows a '..' that stays inside, and names like '..x'", () => {
    assert.equal(toft.scratchDir("a", "..", "b"), join(root, "b"));
    assert.equal(toft.scratchDir("..x"), join(root, "..x"));
    assert.equal(toft.scratchDir(), root);
  });
});

describe("off the platform (BB-034)", () => {
  it("scratch is a private per-user directory, never the shared temp dir", () => {
    const dir = toft.scratchDir("a");
    assert.notEqual(dir, join(tmpdir(), "a"));
    const userRoot = join(tmpdir(), `toft-scratch-${process.getuid()}`);
    assert.ok(dir.startsWith(userRoot + "/"), dir);
    assert.equal(statSync(userRoot).mode & 0o777, 0o700);
  });

  it("is keyed by a hash of the working directory, not its name or TOFT_APP_SLUG (TOFT-BB-003)", () => {
    const a = mkdtempSync(join(tmpdir(), "toft-cwd-a-"));
    const b = mkdtempSync(join(tmpdir(), "toft-cwd-b-"));
    mkdirSync(join(a, "proj"));
    mkdirSync(join(b, "proj"));
    const prevCwd = process.cwd();
    const prevSlug = process.env.TOFT_APP_SLUG;
    try {
      process.chdir(join(a, "proj"));
      const first = toft.scratchDir();
      assert.match(basename(first), /^[0-9a-f]{12}$/);
      process.env.TOFT_APP_SLUG = "other";
      assert.equal(toft.scratchDir(), first);
      process.chdir(join(b, "proj"));
      assert.notEqual(toft.scratchDir(), first);
    } finally {
      process.chdir(prevCwd);
      if (prevSlug === undefined) delete process.env.TOFT_APP_SLUG;
      else process.env.TOFT_APP_SLUG = prevSlug;
      rmSync(a, { recursive: true, force: true });
      rmSync(b, { recursive: true, force: true });
    }
  });

  it("refuses a per-user root that is a symlink somebody planted", () => {
    const fakeTmp = mkdtempSync(join(tmpdir(), "toft-fake-tmp-"));
    const target = mkdtempSync(join(tmpdir(), "toft-elsewhere-"));
    const prev = process.env.TMPDIR;
    process.env.TMPDIR = fakeTmp;
    try {
      symlinkSync(target, join(fakeTmp, `toft-scratch-${process.getuid()}`));
      assert.throws(() => toft.scratchDir("a"), coded("bad_request"));
    } finally {
      // Assigning undefined stores the string "undefined", which every later
      // tmpdir() in this process then answers.
      if (prev === undefined) delete process.env.TMPDIR;
      else process.env.TMPDIR = prev;
      rmSync(fakeTmp, { recursive: true, force: true });
      rmSync(target, { recursive: true, force: true });
    }
  });

  it("storage() and store() are off_platform, not not_mapped", () => {
    assert.throws(() => storage("x"), coded("off_platform"));
    assert.throws(() => store("x"), coded("off_platform"));
  });

  it("on the platform with nothing mapped is still not_mapped", () => {
    process.env.TOFT_API_URL = "http://127.0.0.1:1";
    assert.throws(() => storage("x"), coded("not_mapped"));
  });

  it("app.id and app.slug are off_platform rather than empty", () => {
    assert.throws(() => toft.app.id, coded("off_platform"));
    assert.throws(() => toft.app.slug, coded("off_platform"));
  });

  it("new Run() outside a job is off_platform", () => {
    assert.throws(() => new Run(), coded("off_platform"));
  });
});

describe("a malformed TOFT_STORAGE is storage_env_invalid (BB-016)", () => {
  for (const raw of ["not json", '{"name":"x"}', "null", "42"]) {
    it(`refuses ${raw}`, () => {
      process.env.TOFT_STORAGE = raw;
      assert.throws(() => mounts(), coded("storage_env_invalid"));
      assert.throws(() => storage("x"), coded("storage_env_invalid"));
    });
  }
  it("an empty value is still no mounts", () => {
    process.env.TOFT_STORAGE = "  ";
    assert.deepEqual(mounts(), []);
  });
});

describe("files the SDK is asked to read (BB-007)", () => {
  it("notify.slackFile of a missing path is not_found", () =>
    assert.rejects(() => toft.notify.slackFile("/nonexistent/report.csv"), coded("not_found")));
  it("an email attachment that is a directory is bad_request", () =>
    assert.rejects(
      () => toft.notify.email({ to: ["a@b.example"], subject: "s", text: "t", attachments: [tmpdir()] }),
      coded("bad_request"),
    ));
  it("an attachment of the wrong shape is bad_request", () =>
    assert.rejects(() => toft.notify.slackFile(42), coded("bad_request")));
  it("an attachment whose content is not bytes is bad_request", () =>
    assert.rejects(() => toft.notify.slackFile({ name: "x.txt", content: { a: 1 } }), coded("bad_request")));
  it("notify.send('email', …) is bad_request", () =>
    assert.rejects(() => toft.notify.send("email", "hi"), coded("bad_request")));

  it("store put/upload of a missing source is not_found", async () => {
    const dir = mkdtempSync(join(tmpdir(), "toft-mount-"));
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "reports", kind: "store", path: dir, access: "full" }]);
    try {
      await assert.rejects(() => store("reports").put("/nonexistent/x.csv"), coded("not_found"));
      await assert.rejects(() => store("reports").upload("/nonexistent/x.csv"), coded("not_found"));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("writeOutput of an object is bad_request", async () => {
    const run = new Run({ baseUrl: "http://127.0.0.1:1", token: "t", id: "r", outputUri: "http://127.0.0.1:1/out" });
    await assert.rejects(() => run.writeOutput({ rows: 1 }), coded("bad_request"));
  });
});

describe("arguments checked before any request", () => {
  it("jobs.list with an unknown status is bad_request", () =>
    assert.rejects(() => toft.jobs.list({ status: "bogus" }), coded("bad_request")));
  it("app.can('') is bad_request", () => assert.rejects(() => toft.app.can(""), coded("bad_request")));
  it("app.can() with no argument is bad_request", () => assert.rejects(() => toft.app.can(), coded("bad_request")));
});

describe("a Viewer's string (BB-028)", () => {
  const key = "role-key";
  function roleBlob(role) {
    const payload = `v1.${role}.${Buffer.from("app").toString("base64url")}.${Math.floor(Date.now() / 1000) + 120}`;
    const mac = createHmac("sha256", key)
      .update(Buffer.concat([Buffer.from("toft-role"), Buffer.from([0])]))
      .update(payload)
      .digest("base64url");
    return `${payload}.${mac}`;
  }

  it("does not call an anonymous visitor signed in", () => {
    process.env.TOFT_ROLE_KEY = key;
    const v = toft.viewer({ [ACT_FOR_HEADER]: "anon-blob", "x-toft-role": roleBlob("anonymous") });
    assert.match(String(v), /anonymous/);
    assert.doesNotMatch(String(v), /signed-in/);
  });

  it("still says signed-in for a person on the ladder", () => {
    process.env.TOFT_ROLE_KEY = key;
    const v = toft.viewer({ [ACT_FOR_HEADER]: "blob", "x-toft-role": roleBlob("editor") });
    assert.match(String(v), /a signed-in person/);
  });

  it("claims nothing when the role cannot be verified", () => {
    assert.doesNotMatch(String(toft.viewer({ [ACT_FOR_HEADER]: "blob" })), /signed-in/);
  });
});

describe("JobRun output without a platform", () => {
  it("output() off-platform is off_platform, not a network failure", () =>
    assert.rejects(() => new JobRun({ id: "r" }).output(), coded("off_platform")));
});

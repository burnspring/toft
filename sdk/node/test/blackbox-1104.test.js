/**
 * The 2026-11-04 black-box list, Node's half: jobs.start names idempotencyKey
 * and gives the camelCase for idempotency_key (BB-093); a positional value past
 * a call's arguments is not called an option, and logs(NaN) says NaN (BB-096).
 */

import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, JobRun, ToftError, resetTransport } from "../index.js";

beforeEach(() => resetTransport(new AppTransport({ baseUrl: "http://127.0.0.1:1", token: "tofta_test" })));
afterEach(() => resetTransport());

async function refused(call) {
  try {
    await call();
  } catch (err) {
    assert.ok(err instanceof ToftError, String(err));
    return err;
  }
  assert.fail("no refusal");
}

describe("BB-093: jobs.start's options", () => {
  it("names idempotencyKey for idempotency_key", async () => {
    const err = await refused(() => toft.jobs.start("probe", { idempotency_key: "k" }));
    assert.equal(err.code, "bad_request");
    assert.match(err.message, /it takes params and idempotencyKey/);
    assert.match(err.hint, /^idempotencyKey, not idempotency_key: /);
  });
});

describe("BB-096: wording", () => {
  it("a positional value is not an option", async () => {
    const saved = process.env.TOFT_STORAGE;
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "x", kind: "store", path: "", access: "write" }]);
    let err;
    try {
      err = await refused(() => toft.store("x").download("a.txt", "/tmp/x"));
    } finally {
      if (saved === undefined) delete process.env.TOFT_STORAGE; else process.env.TOFT_STORAGE = saved;
    }
    assert.match(err.message, /it takes one argument/);
    assert.doesNotMatch(err.message, /option/);
  });
  it("logs(NaN) says NaN", async () => {
    const run = JobRun.fromRaw ? JobRun.fromRaw({ id: "r1", status: "running" }) : new JobRun({ id: "r1", status: "running" });
    const err = await refused(() => run.logs(NaN));
    assert.match(err.message, /got NaN/);
  });
});

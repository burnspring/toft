/**
 * The 2026-11-04 black-box list, second half, Node's side: a refusal the SDK
 * makes itself is coded, never a native TypeError (BB-008); run.wait() refuses
 * a timeout that is not one (BB-009); toft.run hands back no presigned output
 * URL (BB-013); TOFT_STORAGE='[]' is not_mapped, not off_platform (BB-023).
 */

import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, JobRun, Run, ToftError, resetTransport } from "../index.js";

let saved;
beforeEach(() => {
  saved = { ...process.env };
  resetTransport(new AppTransport({ baseUrl: "http://127.0.0.1:1", token: "tofta_test" }));
});
afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
  resetTransport();
});

async function refused(call, code) {
  try {
    await call();
  } catch (err) {
    assert.ok(err instanceof ToftError, `not a ToftError: ${err}`);
    assert.equal(err.code, code, err.message);
    assert.equal(err.retryable, false);
    assert.ok(err.hint, "no hint");
    return err;
  }
  assert.fail("no refusal");
}

describe("BB-008: SDK-made refusals are coded", () => {
  let dir;
  let file;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "bb008-"));
    file = join(dir, "a.txt");
    writeFileSync(file, "a");
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  for (const env of [
    { label: "on-platform", vars: { TOFT_API_URL: "http://127.0.0.1:1", TOFT_APP_ID: "a1" } },
    { label: "off-platform", vars: {} },
  ]) {
    it(`upload(file, {}) is bad_request, ${env.label}`, async () => {
      Object.assign(process.env, env.vars);
      process.env.TOFT_STORAGE = JSON.stringify([{ name: "st", kind: "store", access: "read" }]);
      await refused(() => toft.store("st").upload(file, {}), "bad_request");
    });
  }

  it("every name-taking store call refuses a name that is not a string", async () => {
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "st", kind: "store", path: dir, access: "full" }]);
    const s = toft.store("st");
    await refused(() => s.put(file, 5), "bad_request");
    await refused(() => s.download({}), "bad_request");
    await refused(() => s.url(5), "bad_request");
    await refused(() => s.remove(["a"]), "bad_request");
    await refused(() => s.list(5), "bad_request");
  });

  it("a source that is not a path is bad_request", async () => {
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "st", kind: "store", path: dir, access: "full" }]);
    const s = toft.store("st");
    await refused(() => s.upload(5), "bad_request");
    await refused(() => s.upload({}), "bad_request");
    await refused(() => s.put(undefined, "x.txt"), "bad_request");
  });

  it("data.update() with no function is bad_request before any request", async () => {
    process.env.TOFT_API_URL = "http://127.0.0.1:1";
    process.env.TOFT_APP_ID = "a1";
    await refused(() => toft.data.update("notfn"), "bad_request");
    await refused(() => toft.data.update(() => {}, { attempts: 0 }), "bad_request");
    await refused(() => toft.data.update(() => {}, { attempts: "3" }), "bad_request");
  });
});

describe("BB-009: run.wait() timeouts", () => {
  const run = () => new JobRun({ id: "r1", status: "running" });
  for (const bad of [-1, NaN, Infinity, "10"]) {
    it(`timeoutMs ${String(bad)} is bad_request`, async () => {
      await refused(() => run().wait({ timeoutMs: bad }), "bad_request");
    });
  }
  it("a finished run still refuses a negative timeout", async () => {
    await refused(() => new JobRun({ id: "r1", status: "succeeded" }).wait({ timeoutMs: -1 }), "bad_request");
  });
  it("pollIntervalMs 0 is bad_request", async () => {
    await refused(() => run().wait({ timeoutMs: 10, pollIntervalMs: 0 }), "bad_request");
  });
  it("0 is a timeout, not a refusal", async () => {
    const r = new JobRun({ id: "r1", status: "succeeded" });
    assert.equal(await r.wait({ timeoutMs: 0 }), r);
  });
});

describe("BB-013: toft.run keeps its output URL private", () => {
  it("no property, getter or prototype member hands it back", () => {
    const PRESIGNED = "https://bucket.s3.amazonaws.com/out?X-Amz-Credential=AKIA";
    const run = new Run({ baseUrl: "http://127.0.0.1:1", token: "t", id: "r1", outputUri: PRESIGNED });
    assert.equal(run.outputUri, undefined);
    for (const key of Object.getOwnPropertyNames(Run.prototype)) {
      if (key === "constructor") continue;
      const desc = Object.getOwnPropertyDescriptor(Run.prototype, key);
      if (desc.get) assert.notEqual(String(desc.get.call(run)), PRESIGNED, key);
    }
    assert.equal(JSON.parse(JSON.stringify(run)).has_output_location, true);
  });
});

describe("BB-023: TOFT_STORAGE='[]' is not off-platform", () => {
  it("[] is not_mapped", async () => {
    for (const k of ["TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR"]) delete process.env[k];
    process.env.TOFT_STORAGE = "[]";
    await refused(() => toft.storage("x"), "not_mapped");
    await refused(() => toft.store("x"), "not_mapped");
  });
  it("no TOFT_STORAGE at all is off_platform", async () => {
    for (const k of ["TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR", "TOFT_STORAGE"]) {
      delete process.env[k];
    }
    await refused(() => toft.storage("x"), "off_platform");
  });
});

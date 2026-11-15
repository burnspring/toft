/**
 * The shared TOFT_STORAGE and respond vectors (sdk/testdata), Node's half:
 * every SDK reads the same file, so a language that drifts — dropping an entry
 * that is not an object, taking a kind the platform never writes, answering
 * 202 for a run start() did not return — fails here rather than in a
 * black-box run.
 */

import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, describe, it } from "node:test";

import toft, { JobRun, ToftError } from "../index.js";

const testdata = new URL("../../testdata/", import.meta.url);
const read = (name) => JSON.parse(fs.readFileSync(new URL(name, testdata), "utf8"));

function hintedRefusal(fn, code) {
  let caught;
  try {
    fn();
  } catch (err) {
    caught = err;
  }
  assert.ok(caught instanceof ToftError, `want ${code}, got ${caught ?? "no error"}`);
  assert.equal(caught.code, code, caught.message);
  assert.ok(caught.hint, `${code} (${caught.message}) has no hint`);
}

describe("the shared storage-env vector", () => {
  const saved = process.env.TOFT_STORAGE;
  afterEach(() => {
    if (saved === undefined) delete process.env.TOFT_STORAGE;
    else process.env.TOFT_STORAGE = saved;
  });

  const { cases } = read("storage-env-vector.json");
  it("has cases", () => assert.ok(cases.length > 0));
  for (const tc of cases) {
    it(tc.name, () => {
      if (tc.raw === null) delete process.env.TOFT_STORAGE;
      else process.env.TOFT_STORAGE = tc.raw;
      if (tc.error) {
        hintedRefusal(() => toft.mounts(), tc.error);
        return;
      }
      assert.deepEqual(JSON.parse(JSON.stringify(toft.mounts())), tc.mounts);
    });
  }
});

describe("the shared respond vector", () => {
  const { cases } = read("respond-vector.json");
  for (const tc of cases) {
    it(tc.name, () => {
      const run = "string" in tc ? tc.string : new JobRun(tc.run, {});
      for (const [accept, want] of [["application/json", tc.json], ["text/html", tc.html]]) {
        const req = new Request("http://app.test/x", { method: "POST", headers: { Accept: accept } });
        if (tc.error) {
          hintedRefusal(() => toft.jobs.respond(run, req), tc.error);
          continue;
        }
        const res = toft.jobs.respond(run, req);
        assert.equal(res.status, want.status, accept);
        assert.equal(res.headers.get("Location"), want.location, accept);
      }
    });
  }
});

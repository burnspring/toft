/**
 * What a job reads about itself, and what it reports — the scenarios
 * `sdk/python/tests/test_app_sdk.py` runs in `TestRunInsideAJob`, plus the
 * progress message `test_run_errors.py` checks.
 *
 * Params are the only input now, so the env the runner exports is the whole of
 * what a job is given; the deadline and rerunOf are new in it.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { afterEach, beforeEach, describe, it } from "node:test";

import { Run } from "../index.js";

const ENV = {
  TOFT_JOB_NAME: "nightly-report",
  TOFT_PARAMS: '{"region": "emea", "days": 7}',
  TOFT_RERUN_OF: "run_0",
  TOFT_DEADLINE: "2026-10-22T03:45:00Z",
};

const newRun = (baseUrl = "http://platform.invalid") => new Run({ baseUrl, token: "t", id: "run-1" });

describe("a run inside a job", () => {
  let saved;
  beforeEach(() => {
    saved = Object.fromEntries(Object.keys(ENV).map((k) => [k, process.env[k]]));
    Object.assign(process.env, ENV);
  });
  afterEach(() => {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  });

  it("reads its job, typed params, rerunOf and deadline", () => {
    const r = newRun();
    assert.equal(r.job, "nightly-report");
    assert.deepEqual(r.params, { region: "emea", days: 7 });
    assert.equal(r.rerunOf, "run_0");
    assert.ok(r.deadline instanceof Date);
    assert.equal(r.deadline.toISOString(), "2026-10-22T03:45:00.000Z");
  });

  it("reads the absent ones as null", () => {
    delete process.env.TOFT_RERUN_OF;
    delete process.env.TOFT_DEADLINE;
    const r = newRun();
    assert.equal(r.rerunOf, null);
    assert.equal(r.deadline, null);
  });

  it("has no input channels left", () => {
    const r = newRun();
    for (const name of ["setResult", "readInput", "readInputJson", "inputUri"]) {
      assert.equal(name in r, false, name);
    }
  });

  it("logs to stdout, and to stderr when it is an error", () => {
    const written = [];
    const out = process.stdout.write;
    const err = process.stderr.write;
    process.stdout.write = (s) => written.push(["out", s]);
    process.stderr.write = (s) => written.push(["err", s]);
    try {
      newRun().log("12,044 rows");
      newRun().log("ledger missing", { error: true });
    } finally {
      process.stdout.write = out;
      process.stderr.write = err;
    }
    assert.deepEqual(written, [["out", "12,044 rows\n"], ["err", "ledger missing\n"]]);
  });

  it("sends a progress message beside the percent", async () => {
    const seen = [];
    const server = createServer((req, res) => {
      const chunks = [];
      req.on("data", (c) => chunks.push(c));
      req.on("end", () => {
        seen.push([req.url, JSON.parse(Buffer.concat(chunks).toString())]);
        res.writeHead(204);
        res.end();
      });
    });
    await new Promise((r) => server.listen(0, "127.0.0.1", r));
    try {
      const r = newRun(`http://127.0.0.1:${server.address().port}`);
      await r.progress(40, "reading ledgers");
      await r.progress(50);
    } finally {
      server.close();
    }
    assert.equal(seen[0][0], "/api/v1/runner/progress");
    assert.equal(seen[0][1].percent, 40);
    assert.equal(seen[0][1].message, "reading ledgers");
    // No message, no key: the wire stays what a percent-only report was.
    assert.equal("message" in seen[1][1], false);
  });
});

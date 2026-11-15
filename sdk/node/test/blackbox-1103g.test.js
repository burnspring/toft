/**
 * The seventh 2026-11-03 black-box list, Node's half: email's options object
 * is read, not dropped (BB-075); a run's log cursor is a whole number and never
 * NaN, and the calls that take no options — or fewer — refuse one (BB-078);
 * jobs.get and sessionScratchDir refuse under their own names (BB-083); put()
 * on a missing local file says which file (BB-082); and a run carries
 * cpuThrottledSeconds, executionMode and definitionId (BB-076).
 */

import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, JobRun, Run, ToftError, newSessionId, resetTransport, sessionScratchDir, store } from "../index.js";

let server;
let base;
let requests = [];

before(async () => {
  server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      requests.push({ method: req.method, path: req.url, headers: req.headers, body });
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end('{"id":"r1","send_id":"s1","status":"running","lines":[]}');
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

let saved;
beforeEach(() => {
  saved = { ...process.env };
  requests = [];
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
});

const badRequest = (pattern) => (err) =>
  err instanceof ToftError && err.code === "bad_request" && !err.retryable && Boolean(err.hint) &&
  (!pattern || pattern.test(err.message));

const msg = { to: "a@example.com", subject: "s", text: "t" };

describe("email() reads its options object (BB-075)", () => {
  it("sends the key given in the options", async () => {
    await toft.notify.email(msg, { idempotencyKey: "k3" });
    assert.equal(requests.at(-1).headers["idempotency-key"], "k3");
  });

  it("still sends the key given in the message", async () => {
    await toft.notify.email({ ...msg, idempotencyKey: "k4" });
    assert.equal(requests.at(-1).headers["idempotency-key"], "k4");
  });

  it("takes the same key twice", async () => {
    await toft.notify.email({ ...msg, idempotencyKey: "k5" }, { idempotencyKey: "k5" });
    assert.equal(requests.at(-1).headers["idempotency-key"], "k5");
  });

  it("refuses two different keys", async () => {
    await assert.rejects(toft.notify.email({ ...msg, idempotencyKey: "a" }, { idempotencyKey: "b" }),
      badRequest(/two idempotency keys/));
    assert.equal(requests.length, 0);
  });

  it("refuses an unknown option", async () => {
    await assert.rejects(toft.notify.email(msg, { bogus: 1 }), badRequest(/bogus/));
    assert.equal(requests.length, 0);
  });
});

describe("a run's logs cursor (BB-078)", () => {
  const run = () => new JobRun({ id: "r1", status: "running" }, {});

  for (const [label, arg] of [
    ["an options object", { bogus: 1 }],
    ["an after option", { after: 1 }],
    ["a string", "x"],
    ["NaN", NaN],
    ["a fraction", 1.5],
    ["a negative number", -1],
  ]) {
    it(`refuses ${label}`, async () => {
      await assert.rejects(run().logs(arg), badRequest(/logs\(\)/));
      assert.equal(requests.length, 0, "nothing is sent");
    });
  }

  it("sends a whole-number cursor, and 0 by default", async () => {
    await run().logs(7);
    await run().logs();
    assert.match(requests[0].path, /\/logs\?after=7$/);
    assert.match(requests[1].path, /\/logs\?after=0$/);
  });
});

describe("calls that take no options, or fewer, refuse one (BB-078)", () => {
  const current = () => new Run({ baseUrl: base, token: "t", id: "r1", outputUri: `${base}/out` });
  for (const [label, call] of [
    ["run.writeOutput", () => current().writeOutput("x", { bogus: 1 })],
    ["newSessionId", async () => newSessionId({ bogus: 1 })],
    ["app.policy", () => toft.app.policy({ bogus: 1 })],
    ["app.name", () => toft.app.name({ bogus: 1 })],
    ["app.mode", () => toft.app.mode({ bogus: 1 })],
    ["app.basePath", () => toft.app.basePath({ bogus: 1 })],
    ["app.capabilities", () => toft.app.capabilities({ bogus: 1 })],
    ["app.can", () => toft.app.can("email", { bogus: 1 })],
  ]) {
    it(label, async () => {
      await assert.rejects(call, badRequest(/bogus/));
      assert.equal(requests.length, 0, "nothing is sent");
    });
  }
});

describe("refusals name the call made (BB-083)", () => {
  it("jobs.get('') asks for a run id, not a job's name", async () => {
    await assert.rejects(toft.jobs.get(""), (err) =>
      badRequest(/jobs\.get\(\) needs a non-empty run id/)(err) && /id of a run/.test(err.hint) &&
      !/toft\.toml/.test(err.hint));
  });

  it("jobs.start('') still asks for the job's name", async () => {
    await assert.rejects(toft.jobs.start(""), (err) => badRequest(/jobs\.start\(\)/)(err) && /toft\.toml/.test(err.hint));
  });

  it("sessionScratchDir names itself", () => {
    process.env.TOFT_SCRATCH_DIR = os.tmpdir();
    assert.throws(() => sessionScratchDir("abc", { x: 1 }),
      (err) => badRequest(/^sessionScratchDir\(\) takes relative path parts/)(err) && /sessionScratchDir/.test(err.hint));
  });
});

describe("put() on a missing local file (BB-082)", () => {
  it("is not_found with a hint naming the file", async () => {
    const root = await fs.mkdtemp(path.join(os.tmpdir(), "toft-1103g-"));
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "x", kind: "store", path: root, access: "write" }]);
    await assert.rejects(store("x").put("missing.txt", "x"), (err) =>
      err instanceof ToftError && err.code === "not_found" && /missing\.txt/.test(err.hint ?? "") &&
      /local file/.test(err.hint));
  });
});

describe("a run's new fields (BB-076)", () => {
  it("reads cpu_throttled_seconds, execution_mode and definition_id", () => {
    const run = new JobRun({ id: "r1", cpu_throttled_seconds: 2.5, execution_mode: "isolated", definition_id: "d1" });
    assert.equal(run.cpuThrottledSeconds, 2.5);
    assert.equal(run.executionMode, "isolated");
    assert.equal(run.definitionId, "d1");
  });

  it("is null where the platform sent none", () => {
    const run = new JobRun({ id: "r1" });
    assert.equal(run.cpuThrottledSeconds, null);
    assert.equal(run.executionMode, null);
    assert.equal(run.definitionId, null);
  });
});

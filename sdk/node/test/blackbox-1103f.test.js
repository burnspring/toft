/**
 * The sixth 2026-11-03 black-box list, Node's half: a call that takes no
 * options object refuses one, before any request, instead of JavaScript
 * dropping it without a word (BB-066).
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, JobRun, Run, ToftError, caller, mounts, resetTransport } from "../index.js";

let server;
let base;
let requests = [];

before(async () => {
  server = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      requests.push({ method: req.method, path: req.url });
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end('{"id":"r1","status":"running","data":{},"version":1,"lines":[]}');
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

const refused = (err) =>
  err instanceof ToftError && err.code === "bad_request" && !err.retryable && Boolean(err.hint) && /bogus/.test(err.message);

describe("a call that takes no options refuses one (BB-066)", () => {
  const run = () => new JobRun({ id: "r1", status: "running" }, {});
  const current = () => new Run({ baseUrl: base, token: "t", id: "r1" });
  for (const [label, call] of [
    ["data.get", () => toft.data.get({ bogus: 1 })],
    ["data.read", () => toft.data.read({ bogus: 1 })],
    ["jobs.get", () => toft.jobs.get("r1", { bogus: 1 })],
    ["caller", async () => caller({}, { bogus: 1 })],
    ["mounts", async () => mounts({ bogus: 1 })],
    ["notify.status", () => toft.notify.status("snd_1", { bogus: 1 })],
    ["run.cancel", () => run().cancel({ bogus: 1 })],
    ["run.refresh", () => run().refresh({ bogus: 1 })],
    ["run.logs", () => run().logs(0, { bogus: 1 })],
    ["run.output", () => run().output({ bogus: 1 })],
    ["toft.run.progress", () => current().progress(10, "m", { bogus: 1 })],
    ["toft.run.writeOutputJson", () => current().writeOutputJson({}, { bogus: 1 })],
  ]) {
    it(label, async () => {
      await assert.rejects(call, refused);
      assert.equal(requests.length, 0, "nothing is sent");
    });
  }

  it("undefined past the end is no argument", async () => {
    await toft.data.get(undefined);
    assert.equal(requests.length, 1);
  });
});

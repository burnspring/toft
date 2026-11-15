/**
 * The fifth 2026-11-03 black-box list, Node's half: every call refuses an
 * option it does not take, before any request (BB-049); no own-property
 * reflection reaches toft.run's job token or output URL (BB-057); the
 * queue-full hint names --app, and every storage_env_invalid carries a hint
 * that does not call path required (BB-053, BB-061); a destination field is
 * refused naming the integration it was sent to (BB-060).
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { inspect } from "node:util";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, JobRun, QueueFull, Run, ToftError, mounts, resetTransport, storage, store, who } from "../index.js";

let server;
let base;
let requests = [];
let queueFull = false;

before(async () => {
  server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      requests.push({ method: req.method, path: req.url, body });
      if (queueFull) {
        res.writeHead(429, { "Content-Type": "application/json" });
        return res.end(JSON.stringify({
          error: "this app's queue is full", code: "queue_full", retryable: true,
          in_flight: 3, limit: 3, hint: "toft job runs to see what is queued",
        }));
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end('{"runs":[],"version":1}');
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
  queueFull = false;
  process.env.TOFT_STORAGE = JSON.stringify([{ name: "x", kind: "volume", access: "read", path: "/data/x", folder: "" }]);
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
});

const refused = (code, re) => (err) =>
  err instanceof ToftError && err.code === code && !err.retryable && Boolean(err.hint) && (!re || re.test(`${err.message} ${err.hint}`));

describe("every call refuses an option it does not take (BB-049)", () => {
  const cases = [
    ["data.set if_version", () => toft.data.set({ a: 1 }, { if_version: 3 }), /ifVersion, not if_version/],
    ["data.update rate_limit_timeout", () => toft.data.update(() => {}, { rate_limit_timeout: 5 }), /rateLimitTimeout/],
    ["data.clear", () => toft.data.clear({ version: 1 }), null],
    ["storage fold", async () => storage("x", { fold: "a" }), null],
    ["store fold", async () => store("x", { fold: "a" }), null],
    ["jobs.list jobb", () => toft.jobs.list({ jobb: "j" }), null],
    ["jobs.list string job", () => toft.jobs.list("probe", { status: "bogus" }), null],
    ["jobs.list bogus status", () => toft.jobs.list({ status: "bogus" }), null],
    ["accessToken options", () => toft.accessToken("sharepoint", { x: 1 }), null],
    ["azureCredential options", async () => toft.azureCredential("sharepoint", { x: 1 }), null],
    ["who options", () => who({}, { refreh: true }), null],
    ["run.wait", () => new JobRun({ id: "r1", status: "running" }, {}).wait({ timeout: 5 }), /timeoutMs/],
    ["run.tail", () => new JobRun({ id: "r1", status: "running" }, {}).tail({ interval: 1 }).next(), null],
    ["storage non-object", async () => storage("x", "a"), null],
  ];
  for (const [label, call, re] of cases) {
    it(label, async () => {
      await assert.rejects(call, refused("bad_request", re));
      assert.equal(requests.length, 0, "nothing reached the platform");
    });
  }

  it("Run.log refuses an unknown option", () => {
    process.env.TOFT_API_URL = base;
    const run = new Run({ baseUrl: base, token: "t", id: "r1" });
    assert.throws(() => run.log("x", { stderr: true }), refused("bad_request"));
  });

  it("the options a call does take still work", async () => {
    await toft.data.set({ a: 1 }, { ifVersion: 3 });
    assert.equal(requests.length, 1);
    assert.equal(storage("x", { folder: "" }).name, "x");
    assert.equal(storage("x", { folder: undefined }).name, "x");
    assert.equal(storage("x", null).name, "x");
    await toft.jobs.list({ job: "probe", status: "failed" });
    assert.match(requests.at(-1).path, /job=probe&status=failed/);
  });
});

describe("toft.run's secrets are reachable by no reflection (BB-057)", () => {
  const SECRET = "tofjob_supersecret";
  const PRESIGNED = "https://bucket.s3.amazonaws.com/out?X-Amz-Credential=AKIA";

  it("own-property reflection shows neither", () => {
    process.env.TOFT_OUTPUT_URI = PRESIGNED;
    const run = new Run({ baseUrl: base, token: SECRET, id: "r1" });
    const reflected = [
      inspect(Object.getOwnPropertyDescriptors(run), { showHidden: true, depth: 5 }),
      inspect(run, { showHidden: true, depth: 5 }),
      JSON.stringify(run),
      Reflect.ownKeys(run).map(String).join(","),
    ].join("\n");
    assert.doesNotMatch(reflected, /supersecret|X-Amz|_token|outputUri/);
    // No public accessor hands it back (BB-013).
    assert.equal(run.outputUri, undefined);
    assert.equal(JSON.parse(JSON.stringify(run)).has_output_location, true);
  });
});

describe("hints that name the next step (BB-053, BB-061)", () => {
  it("queue_full names toft job runs --app <slug>", async () => {
    process.env.TOFT_APP_SLUG = "ledger";
    queueFull = true;
    await assert.rejects(toft.jobs.start("nightly"),
      (err) => err instanceof QueueFull && err.hint === "toft job runs --app ledger to see what is queued");
  });

  it("queue_full without a slug names a placeholder", async () => {
    delete process.env.TOFT_APP_SLUG;
    queueFull = true;
    await assert.rejects(toft.jobs.start("nightly"), (err) => err.hint.includes("--app <app>"));
  });

  for (const [label, raw] of [
    ["not JSON", "{nope"],
    ["not an array", '{"name":"x"}'],
    ["missing access", '[{"name":"x","kind":"volume"}]'],
  ]) {
    it(`storage_env_invalid for ${label} carries a hint that does not require path`, () => {
      process.env.TOFT_STORAGE = raw;
      assert.throws(() => mounts(), (err) => {
        assert.equal(err.code, "storage_env_invalid");
        assert.ok(err.hint);
        assert.match(err.hint, /name, kind \(volume or store\) and access/);
        assert.match(err.hint, /has none/);
        return true;
      });
    });
  }

  it("a store entry without a path is accepted", () => {
    process.env.TOFT_STORAGE = '[{"name":"s","kind":"store","access":"write"}]';
    assert.equal(mounts()[0].name, "s");
  });
});

describe("a destination field is refused naming its integration (BB-060)", () => {
  for (const [label, call, name] of [
    ["teams channel", () => toft.notify.send("teams", "hi", { channel: "#x" }), /Teams/],
    ["email channel", () => toft.notify.email({ to: "a@b.c", subject: "s", text: "t", channel: "#x" }), /email/],
    ["slack channel", () => toft.notify.slack("hi", { channel: "#x" }), /Slack/],
  ]) {
    it(label, async () => {
      await assert.rejects(call, (err) => {
        assert.equal(err.code, "destination_not_allowed");
        assert.match(err.message, name);
        assert.match(err.message, /"channel"/);
        if (label !== "slack channel") assert.doesNotMatch(err.message, /Slack/);
        return true;
      });
      assert.equal(requests.length, 0);
    });
  }
});

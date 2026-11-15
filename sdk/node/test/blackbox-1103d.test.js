/**
 * The fourth 2026-11-03 black-box list, Node's half: toft.run prints no
 * credential (BB-026), notify refuses a destination or sender field rather
 * than dropping it, and a severity that is not a word (BB-029), a run's empty
 * fields are null on the wire (BB-030), mounts() serialize with the documented
 * fields (BB-044), a JobRun keeps the scheduled-for text it was given (BB-045),
 * and the SDK's own refusals carry a hint (BB-042).
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { inspect } from "node:util";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, JobRun, Run, ToftError, mounts, resetTransport, storage, store } from "../index.js";

let server;
let base;
let requests = [];

before(async () => {
  server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      requests.push({ method: req.method, path: req.url, body });
      if (req.url === "/store/forbidden") return res.writeHead(403).end();
      if (req.url.includes("/runs/") || req.url.includes("/job-runs/")) {
        res.writeHead(200, { "Content-Type": "application/json" });
        return res.end(JSON.stringify({ id: "run_9", job: "probe", status: "running" }));
      }
      res.writeHead(202, { "Content-Type": "application/json" });
      res.end('{"send_id":"snd_1","status":"queued"}');
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  requests = [];
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

const coded = (code) => (err) => err instanceof ToftError && err.code === code && Boolean(err.hint);

describe("toft.run prints no credential (BB-026)", () => {
  const SECRET = "tofjob_supersecret";
  const PRESIGNED = "https://bucket.s3.amazonaws.com/out?X-Amz-Credential=AKIA&X-Amz-Security-Token=zzz";
  let saved;
  beforeEach(() => {
    saved = { ...process.env };
    process.env.TOFT_OUTPUT_URI = PRESIGNED;
    process.env.TOFT_JOB_NAME = "probe";
  });
  afterEach(() => {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
  });

  it("JSON.stringify and util.inspect hide the job token and the output URL", () => {
    const run = new Run({ baseUrl: base, token: SECRET, id: "r1" });
    for (const printed of [JSON.stringify(run), inspect(run), inspect(run, { showHidden: false, depth: 5 })]) {
      assert.doesNotMatch(printed, /supersecret/);
      assert.doesNotMatch(printed, /X-Amz/);
      assert.match(printed, /r1/);
    }
    assert.equal(Object.keys(run).includes("_token"), false);
    assert.equal(Object.keys(run).includes("outputUri"), false);
    assert.equal(JSON.parse(JSON.stringify(run)).has_output_location, true);
    // Not readable at all: no public accessor hands the presigned URL back (BB-013).
    assert.equal(run.outputUri, undefined);
  });
});

describe("notify refuses what it would have dropped (BB-029)", () => {
  const cases = [
    ["slack channel", () => toft.notify.slack("hi", { channel: "#general" }), "destination_not_allowed"],
    ["send channel", () => toft.notify.send("teams", "hi", { channel: "x" }), "destination_not_allowed"],
    ["send folder", () => toft.notify.send("teams", "hi", { folder: "x" }), "destination_not_allowed"],
    ["send url", () => toft.notify.send("webhook", "hi", { url: "https://x" }), "destination_not_allowed"],
    ["send routingKey", () => toft.notify.send("pagerduty", "hi", { routingKey: "k" }), "destination_not_allowed"],
    ["deliver folder", () => toft.notify.deliver("sharepoint", ["a.txt", "x"], { folder: "/x" }), "destination_not_allowed"],
    ["slackFile channel", () => toft.notify.slackFile(["a.txt", "x"], { channel: "#x" }), "destination_not_allowed"],
    ["email replyTo", () => toft.notify.email({ to: "a@b.c", subject: "s", text: "t", replyTo: "r@b.c" }), "sender_not_allowed"],
    ["email reply_to", () => toft.notify.email({ to: "a@b.c", subject: "s", text: "t", reply_to: "r@b.c" }), "sender_not_allowed"],
    ["email from", () => toft.notify.email({ to: "a@b.c", subject: "s", text: "t", from: "f@b.c" }), "sender_not_allowed"],
    ["email headers", () => toft.notify.email({ to: "a@b.c", subject: "s", text: "t", headers: { A: "1" } }), "sender_not_allowed"],
    ["send unknown option", () => toft.notify.send("teams", "hi", { colour: "red" }), "bad_request"],
    ["severity NaN", () => toft.notify.send("pagerduty", "t", { severity: NaN }), "bad_request"],
    ["severity number", () => toft.notify.send("pagerduty", "t", { severity: 3 }), "bad_request"],
  ];
  for (const [name, call, code] of cases) {
    it(`${name} is ${code}, before any request`, async () => {
      await assert.rejects(call, coded(code));
      assert.equal(requests.length, 0);
    });
  }

  it("a severity word still goes", async () => {
    await toft.notify.send("pagerduty", "t", { severity: "critical" });
    assert.equal(JSON.parse(requests[0].body).severity, "critical");
  });
});

describe("a run's empty fields are null on the wire (BB-030)", () => {
  const RAW = { id: "run_1", job: "nightly", status: "queued", status_url: "/s" };
  it("JSON.stringify(run) carries them", () => {
    const out = JSON.parse(JSON.stringify(new JobRun(RAW)));
    assert.equal(out.rerun_of, null);
    assert.equal(out.scheduled_for, null);
    assert.equal(out.failure_reason, null);
  });
  it("respond()'s 202 body carries them", async () => {
    const out = await toft.jobs.respond(new JobRun(RAW)).json();
    for (const k of ["rerun_of", "scheduled_for", "failure_reason"]) assert.ok(k in out && out[k] === null, k);
  });
  it("a set value passes through", () => {
    const out = JSON.parse(JSON.stringify(new JobRun({ ...RAW, rerun_of: "run_0", failure_reason: "exit 3" })));
    assert.equal(out.rerun_of, "run_0");
    assert.equal(out.failure_reason, "exit 3");
  });
});

describe("mounts() serialize with the documented fields (BB-044)", () => {
  let saved;
  beforeEach(() => (saved = process.env.TOFT_STORAGE));
  afterEach(() => {
    if (saved === undefined) delete process.env.TOFT_STORAGE;
    else process.env.TOFT_STORAGE = saved;
  });
  it("name, kind, path, access, folder, read_only, mounted", () => {
    process.env.TOFT_STORAGE = JSON.stringify([
      { name: "v", kind: "volume", path: "/storage/v", access: "read", folder: "a" },
      { name: "s", kind: "store", path: "", access: "write", folder: "" },
    ]);
    const out = JSON.parse(JSON.stringify(mounts()));
    assert.deepEqual(out, [
      { name: "v", kind: "volume", path: "/storage/v", access: "read", folder: "a", read_only: true, mounted: true },
      { name: "s", kind: "store", path: "", access: "write", folder: "", read_only: false, mounted: false },
    ]);
    const [m] = mounts();
    assert.equal(m.mountPath, "/storage/v");
    assert.equal(m.readOnly, true);
    assert.match(inspect(m), /read_only: true/);
  });
});

describe("a JobRun keeps the scheduled-for text it was given (BB-045)", () => {
  it("scheduledFor and scheduledForText are the sent text, offset included", () => {
    const run = new JobRun({ id: "r", scheduled_for: "2026-11-03T09:00:00-05:00" });
    assert.equal(run.scheduledFor, "2026-11-03T09:00:00-05:00");
    assert.equal(run.scheduledForText, "2026-11-03T09:00:00-05:00");
    assert.equal(new JobRun({ id: "r" }).scheduledForText, null);
  });
});

describe("the SDK's own refusals carry a hint (BB-042)", () => {
  let saved;
  beforeEach(() => (saved = { ...process.env }));
  afterEach(() => {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
  });

  it("blank Slack text", () => assert.rejects(() => toft.notify.slack("  "), coded("bad_request")));
  it("blank send text", () => assert.rejects(() => toft.notify.send("teams", ""), coded("bad_request")));
  it("blank email subject", () =>
    assert.rejects(() => toft.notify.email({ to: "a@b.c", subject: " ", text: "t" }), coded("bad_request")));
  it("blank email text", () =>
    assert.rejects(() => toft.notify.email({ to: "a@b.c", subject: "s", text: "" }), coded("bad_request")));
  it("app.id inside a job is off_platform with a hint", () => {
    delete process.env.TOFT_APP_ID;
    process.env.TOFT_RUN_ID = "run_1";
    assert.throws(() => toft.app.id, (err) => coded("off_platform")(err) && /param/.test(err.hint));
  });
  it("app.id off the platform is off_platform with a hint", () => {
    delete process.env.TOFT_APP_ID;
    delete process.env.TOFT_RUN_ID;
    assert.throws(() => toft.app.id, coded("off_platform"));
  });
  it("wait_timeout", async () => {
    const run = new JobRun({ id: "run_9", status: "running" });
    await assert.rejects(() => run.wait({ timeoutMs: 1, pollIntervalMs: 1 }), coded("wait_timeout"));
  });
  it("output_unwritable", async () => {
    const run = new Run({ baseUrl: base, token: "t", id: "r1", outputUri: `${base}/store/forbidden` });
    await assert.rejects(() => run.writeOutput("x"), coded("output_unwritable"));
  });
  it("no_output", async () => {
    const run = new Run({ baseUrl: base, token: "t", id: "r1", outputUri: "" });
    await assert.rejects(() => run.writeOutput("x"), coded("no_output"));
  });
});

describe("TOFT_STORAGE entries and folders are checked, as in Python, Go and R", () => {
  let saved;
  beforeEach(() => (saved = process.env.TOFT_STORAGE));
  afterEach(() => {
    if (saved === undefined) delete process.env.TOFT_STORAGE;
    else process.env.TOFT_STORAGE = saved;
  });

  for (const missing of ["name", "kind", "access"]) {
    it(`an entry with no ${missing} is storage_env_invalid`, () => {
      const entry = { name: "x", kind: "volume", path: "/storage/x", access: "full" };
      delete entry[missing];
      process.env.TOFT_STORAGE = JSON.stringify([entry]);
      assert.throws(() => mounts(), coded("storage_env_invalid"));
      assert.throws(() => storage("x"), coded("storage_env_invalid"));
    });
  }

  it("a missing path stays accepted — a store over signed URLs has none", () => {
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "s", kind: "store", access: "write" }]);
    assert.equal(store("s").mounted, false);
  });

  for (const folder of ["/abs", "a/", "..", "a/../b", "./a", "a//b", 3]) {
    it(`folder ${JSON.stringify(folder)} is invalid_name`, () => {
      process.env.TOFT_STORAGE = JSON.stringify([{ name: "v", kind: "volume", path: "/s/v", access: "full", folder: "a" }]);
      assert.throws(() => storage("v", { folder }), coded("invalid_name"));
      assert.throws(() => store("v", { folder }), coded("invalid_name"));
    });
  }

  it("a good folder, the root and null still resolve", () => {
    process.env.TOFT_STORAGE = JSON.stringify([
      { name: "v", kind: "volume", path: "/s/v", access: "full", folder: "a/b" },
      { name: "w", kind: "volume", path: "/s/w", access: "full", folder: "" },
    ]);
    assert.equal(storage("v", { folder: "a/b" }).folder, "a/b");
    assert.equal(storage("w", { folder: "" }).folder, "");
    assert.equal(storage("w", { folder: null }).name, "w");
  });
});

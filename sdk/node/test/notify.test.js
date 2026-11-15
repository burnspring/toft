/**
 * Contract tests for the notify surface, against a fake platform.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * These mirror the Slack and email scenarios in `sdk/python/tests` — same
 * routes, same idempotency rules, same refusals — because the two libraries have
 * to behave identically or an app written in one observes a platform the other
 * cannot. When one side gains a scenario, the other gains it too.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, Run, ToftError, Send, resetTransport } from "../index.js";

let server;
let base;
let routes = {};
let requests = [];

function route(key, status, payload, headers) {
  routes[key] = { status, payload, headers };
}

const last = () => requests[requests.length - 1];

before(async () => {
  server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const raw = Buffer.concat(chunks);
      requests.push({
        method: req.method,
        path: req.url,
        headers: req.headers,
        body: raw.toString(),
        bytes: raw,
      });

      const entry = routes[`${req.method} ${req.url.split("?")[0]}`];
      if (!entry) {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end('{"error":"no such route"}');
        return;
      }
      res.writeHead(entry.status, {
        "Content-Type": "application/json",
        ...(entry.headers ?? {}),
      });
      res.end(JSON.stringify(entry.payload ?? {}));
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  routes = {};
  requests = [];
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

describe("posting to Slack", () => {
  it("sends the text and returns the record, not a delivery", async () => {
    route("POST /api/v1/app/notify/slack", 202, { send_id: "snd_1", status: "queued" });

    const send = await toft.notify.slack("Ingest finished: 1,204 rows");

    assert.equal(send.id, "snd_1");
    // Accepted is not delivered, and the SDK must never blur the two.
    assert.equal(send.delivered, false);
    assert.equal(send.done, false);
    assert.deepEqual(JSON.parse(last().body), { text: "Ingest finished: 1,204 rows" });
    assert.equal(last().headers["x-toft-app-contract"], "1");
  });

  it("mints no idempotency key by default — a duplicate post is cheap", async () => {
    route("POST /api/v1/app/notify/slack", 202, { send_id: "snd_2" });
    await toft.notify.slack("hello");
    assert.equal(last().headers["idempotency-key"], undefined);
  });

  it("carries the caller's key when a duplicate would matter", async () => {
    route("POST /api/v1/app/notify/slack", 202, { send_id: "snd_3" });
    await toft.notify.slack("hello", { idempotencyKey: "weekly-2026-W32" });
    assert.equal(last().headers["idempotency-key"], "weekly-2026-W32");
  });

  it("refuses an empty message before it reaches the platform", async () => {
    await assert.rejects(() => toft.notify.slack("   "), ToftError);
    assert.equal(requests.length, 0);
  });

  it("has no channel to choose — one passed anyway is destination_not_allowed (BB-029)", async () => {
    // The app's owners choose the channel in the UI, from the channels somebody
    // invited the bot into. Dropping the field left a caller who believed they
    // chose; the platform's own code says so, with a hint, before any request.
    const before = requests.length;
    await assert.rejects(() => toft.notify.slack("hello", { channel: "#finance" }),
      (err) => err.code === "destination_not_allowed" && /Integrations tab/.test(err.hint));
    assert.equal(requests.length, before);
  });
});

describe("uploading a file to Slack", () => {
  it("sends multipart with the file and its metadata, and defaults a key", async () => {
    route("POST /api/v1/app/notify/slack/file", 202, { send_id: "snd_4", status: "queued" });

    const send = await toft.notify.slackFile(
      { name: "recon.xlsx", content: "binary-ish" },
      { title: "Weekly reconciliation", comment: "1,204 rows" },
    );

    assert.equal(send.id, "snd_4");
    const req = last();
    assert.match(req.headers["content-type"], /^multipart\/form-data; boundary=/);
    assert.match(req.body, /name="title"[\s\S]*Weekly reconciliation/);
    assert.match(req.body, /name="comment"[\s\S]*1,204 rows/);
    assert.match(req.body, /name="file"; filename="recon.xlsx"/);
    assert.match(req.body, /binary-ish/);
    // Unlike a text post: re-uploading a large report is not cheap.
    assert.ok(req.headers["idempotency-key"]);
  });

  it("reduces a path to its basename, so no directory layout is disclosed", async () => {
    route("POST /api/v1/app/notify/slack/file", 202, { send_id: "snd_5" });
    await toft.notify.slackFile({ name: "/home/priya/private/recon.xlsx", content: "x" });
    assert.match(last().body, /filename="recon.xlsx"/);
    assert.ok(!last().body.includes("/home/priya"));
  });
});

describe("sending email", () => {
  it("accepts one address or many, and requires the plain-text half", async () => {
    route("POST /api/v1/app/notify/email", 202, { send_id: "snd_6", status: "queued" });

    await toft.notify.email({
      to: "finance@example.com",
      subject: "Weekly report",
      text: "1,204 rows",
      html: "<p>1,204 rows</p>",
      cc: ["ops@example.com", "  "],
    });

    const body = JSON.parse(last().body);
    assert.deepEqual(body.to, ["finance@example.com"]);
    assert.deepEqual(body.cc, ["ops@example.com"]);
    assert.equal(body.html, "<p>1,204 rows</p>");
    // A key by default: a slow response must not become two copies of the
    // report in somebody's inbox.
    assert.ok(last().headers["idempotency-key"]);
  });

  it("refuses with no recipient, and with no text", async () => {
    await assert.rejects(() => toft.notify.email({ to: [], subject: "s", text: "t" }), ToftError);
    await assert.rejects(
      () => toft.notify.email({ to: "a@b.c", subject: "s", text: "" }),
      ToftError,
    );
    assert.equal(requests.length, 0);
  });

  it("sends attachments as multipart with the message as one JSON field", async () => {
    route("POST /api/v1/app/notify/email", 202, { send_id: "snd_7" });

    await toft.notify.email({
      to: "finance@example.com",
      subject: "Weekly report",
      text: "attached",
      attachments: [["recon.csv", "a,b\n1,2\n"]],
    });

    const req = last();
    assert.match(req.headers["content-type"], /^multipart\/form-data/);
    assert.match(req.body, /name="payload"/);
    assert.match(req.body, /name="attachment"; filename="recon.csv"/);
    assert.match(req.body, /text\/csv/);
    // The message itself is still JSON, just carried as a field.
    const payload = JSON.parse(req.body.match(/name="payload"\r?\n\r?\n([\s\S]*?)\r?\n---/)[1]);
    assert.equal(payload.subject, "Weekly report");
  });

  it("has no from or reply_to to set — one passed anyway is sender_not_allowed (BB-029)", async () => {
    for (const extra of [{ from: "spoof@example.com" }, { replyTo: "x@example.com" },
      { reply_to: "x@example.com" }, { headers: { "X-A": "1" } }]) {
      await assert.rejects(
        () => toft.notify.email({ to: "a@b.c", subject: "s", text: "t", ...extra }),
        (err) => err.code === "sender_not_allowed" && Boolean(err.hint),
        JSON.stringify(extra));
    }
  });
});

describe("reading a send back", () => {
  it("reports a blocked send as its own outcome, not a failure", async () => {
    route("GET /api/v1/app/sends/snd_9", 200, {
      id: "snd_9",
      status: "blocked",
      integration: "slack",
      error: "an administrator turned Slack off for this app",
    });

    const send = await toft.notify.status("snd_9");

    assert.equal(send.blocked, true);
    assert.equal(send.delivered, false);
    // Terminal: retrying will not help until somebody re-attaches it.
    assert.equal(send.done, true);
    assert.equal(send.integration, "slack");
  });

  it("accepts the Send it was given, not only its id", async () => {
    route("POST /api/v1/app/notify/slack", 202, { send_id: "snd_10" });
    const send = await toft.notify.slack("hi");
    route("GET /api/v1/app/sends/snd_10", 200, { id: "snd_10", status: "sent" });

    const read = await toft.notify.status(send);
    assert.ok(read instanceof Send);
    assert.equal(read.delivered, true);
  });

  it("refuses an empty id rather than fetching a listing", async () => {
    await assert.rejects(() => toft.notify.status(""), ToftError);
    assert.equal(requests.length, 0);
  });
});

describe("an ordinary call refuses a redirect", () => {
  // fetch would replay the act-for blob and the idempotency key to whatever
  // host the Location names; only follow() deals with a redirect, by hand.
  let elsewhere;
  let seen;
  before(async () => {
    elsewhere = createServer((req, res) => {
      seen.push(req.headers);
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end("{}");
    });
    await new Promise((r) => elsewhere.listen(0, "127.0.0.1", r));
  });
  after(() => elsewhere.close());
  beforeEach(() => {
    seen = [];
  });
  const target = () => `http://127.0.0.1:${elsewhere.address().port}/steal`;

  it("on the app transport", async () => {
    route("GET /api/v1/app/sends/s1", 302, {}, { Location: target() });
    await assert.rejects(toft.notify.status("s1"), (e) => e instanceof ToftError && /redirect/.test(e.message));
    route("POST /api/v1/app/notify/slack", 307, {}, { Location: target() });
    await assert.rejects(toft.notify.slack("hi"), ToftError);
    assert.deepEqual(seen, [], "the redirect was followed");
  });

  it("on the runner transport", async () => {
    route("POST /api/v1/runner/progress", 307, {}, { Location: target() });
    const r = new Run({ baseUrl: base, token: "run-token", id: "run-1" });
    await assert.rejects(r.progress(10), ToftError);
    assert.deepEqual(seen, [], "the redirect was followed");
  });
});

describe("caller-supplied segments", () => {
  it("are escaped into one segment", async () => {
    route("GET /api/v1/app/sends/a%2F..%2Fdata%3Fx%3D1", 200, { id: "x", status: "sent" });
    await toft.notify.status("a/../data?x=1");
    assert.equal(last().path, "/api/v1/app/sends/a%2F..%2Fdata%3Fx%3D1");
  });
});

describe("deliver", () => {
  it("sends the name it was given, separator and all", async () => {
    // The platform refuses a separator by name; the SDK must not hide it.
    route("POST /api/v1/app/deliver/sharepoint", 400, { error: "the name contains '/'", code: "bad_name" });
    await assert.rejects(
      toft.notify.deliver("sharepoint", { name: "report.xlsx", content: Buffer.from("x") }, { name: "2026/Week 32.xlsx" }),
      ToftError,
    );
    assert.match(last().body, /name="name"\r\n\r\n2026\/Week 32\.xlsx\r\n/);
  });
});

/**
 * Code review 2026-11-11: I-1 (tail reads every page), I-3 (bytes are an
 * attachment's content), I-4 (an anonymous role answers who() locally).
 *
 * `node --test` from `sdk/node`, with nothing installed.
 */

import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { createServer } from "node:http";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import toft, {
  ACT_FOR_HEADER,
  AppTransport,
  ENV_ROLE_KEY,
  JobRun,
  ROLE_HEADER,
  resetIdentityCache,
  resetTransport,
} from "../index.js";

let server;
let base;
let handler;
let requests;

before(async () => {
  server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks);
      requests.push({ method: req.method, url: req.url, headers: req.headers, body });
      handler(req, res, body);
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});
after(() => server.close());

const saved = { ...process.env };
beforeEach(() => {
  requests = [];
  handler = (_req, res) => { res.writeHead(404); res.end("{}"); };
  resetIdentityCache();
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});
afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
});

const json = (res, status, payload) => {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(payload));
};

describe("I-1: tail() reads every page of a finished run's log", () => {
  it("yields past the first thousand lines, to the end", async () => {
    const total = 2500;
    handler = (req, res) => {
      const u = new URL(req.url, base);
      if (u.pathname.endsWith("/logs")) {
        const after = Number(u.searchParams.get("after") ?? 0);
        const lines = [];
        for (let seq = after + 1; seq <= total && lines.length < 1000; seq++) {
          lines.push({ seq, stream: "stdout", line: `line ${seq}` });
        }
        return json(res, 200, { lines });
      }
      return json(res, 200, { id: "r1", job: "nightly", status: "failed" });
    };
    const got = [];
    for await (const l of new JobRun({ id: "r1", status: "running" }, {}).tail({ pollIntervalMs: 1 })) got.push(l.seq);
    assert.equal(got.length, total);
    assert.equal(got.at(-1), total);
  });
});

describe("I-3: bytes are an attachment's content, not a zero-byte file", () => {
  it("sends a Uint8Array whole, and refuses an object with no content", async () => {
    handler = (req, res) => json(res, 202, { id: "s1", status: "queued" });
    const bytes = new Uint8Array([104, 101, 108, 108, 111]); // "hello"
    await toft.notify.slackFile(bytes, { filename: "report.csv" });
    const sent = requests.at(-1).body.toString("latin1");
    assert.ok(sent.includes("hello"), "the bytes did not reach the platform");
    assert.ok(/filename="report\.csv"/.test(sent) || /filename="attachment"/.test(sent));

    await assert.rejects(
      () => toft.notify.slackFile({ name: "report.csv", data: bytes }),
      (err) => err.code === "bad_request" && /content/.test(err.message),
    );
  });
});

describe("I-4: who() answers an anonymous visitor without a request", () => {
  it("returns null on a verified anonymous role, and asks for a signed-in one", async () => {
    const key = "test-role-key";
    process.env[ENV_ROLE_KEY] = key;
    const mint = (role) => {
      const appId = Buffer.from("app-a").toString("base64url");
      const payload = `v1.${role}.${appId}.${Math.floor(Date.now() / 1000) + 60}`;
      const mac = createHmac("sha256", key)
        .update(Buffer.concat([Buffer.from("toft-role"), Buffer.from([0])]))
        .update(payload).digest("base64url");
      return `${payload}.${mac}`;
    };
    handler = (_req, res) => json(res, 200, { id: "u_1", name: "Alice", role: "editor", groups: [] });

    const anonymous = { [ACT_FOR_HEADER.toLowerCase()]: "a2.fresh-every-request", [ROLE_HEADER.toLowerCase()]: mint("anonymous") };
    assert.equal(await toft.who(anonymous), null);
    assert.equal(requests.length, 0, "an anonymous visitor cost a platform call");

    const signedIn = { [ACT_FOR_HEADER.toLowerCase()]: "a1.stable", [ROLE_HEADER.toLowerCase()]: mint("editor") };
    const me = await toft.who(signedIn);
    assert.equal(me.id, "u_1");
    assert.equal(requests.length, 1);
  });
});

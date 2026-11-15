/**
 * The third 2026-11-03 black-box list, Node's half: an object store's refusal
 * is coded (BB-008), an output written empty is not_json (BB-022), the status
 * sets cannot be changed from outside and rateLimitTimeout is checked (BB-023),
 * a word that is not a role is bad_request (BB-024), and send("") names the
 * kind (BB-020).
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, {
  AppTransport,
  Caller,
  JobRun,
  RUN_STATUSES,
  Run,
  TERMINAL_STATUSES,
  ToftError,
  resetTransport,
} from "../index.js";

let server;
let base;
let requests = [];

before(async () => {
  server = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      requests.push({ method: req.method, path: req.url });
      if (req.url === "/store/forbidden") return res.writeHead(403).end();
      if (req.url === "/store/down") return res.writeHead(503).end();
      if (req.url.endsWith("/redirected/output")) {
        return res.writeHead(302, { Location: `${base}/store/forbidden` }).end();
      }
      if (req.url.endsWith("/empty/output")) return res.writeHead(200).end();
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end('{"runs":[]}');
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

const coded = (code, retryable) => (err) =>
  err instanceof ToftError && err.code === code && err.retryable === retryable;

describe("an object store's refusal is coded (BB-008)", () => {
  const run = (uri) => {
    return new Run({ baseUrl: base, token: "t", id: "r1", outputUri: uri });
  };
  it("writeOutput answered 403 is output_unwritable, not retryable", () =>
    assert.rejects(() => run(`${base}/store/forbidden`).writeOutput("x"), coded("output_unwritable", false)));
  it("writeOutput answered 503 is output_unwritable, retryable", () =>
    assert.rejects(() => run(`${base}/store/down`).writeOutput("x"), coded("output_unwritable", true)));
  it("a store that cannot be reached is uncoded and retryable", () =>
    assert.rejects(() => run("http://127.0.0.1:1/out").writeOutput("x"), coded("", true)));
  it("output() from a store answering 403 is output_unreadable", () =>
    assert.rejects(() => new JobRun({ id: "redirected" }).output(), coded("output_unreadable", false)));
});

describe("an output written empty (BB-022)", () => {
  it("outputJson() is not_json, not null", () =>
    assert.rejects(() => new JobRun({ id: "empty" }).outputJson(), coded("not_json", false)));
});

describe("argument checks (BB-023)", () => {
  it("RUN_STATUSES and TERMINAL_STATUSES are read-only", async () => {
    assert.throws(() => RUN_STATUSES.add("bogus"), TypeError);
    assert.throws(() => TERMINAL_STATUSES.delete("failed"), TypeError);
    Set.prototype.add.call(RUN_STATUSES, "sneaky");
    await assert.rejects(() => toft.jobs.list({ status: "sneaky" }), coded("bad_request", false));
    assert.equal(requests.length, 0);
    assert.equal(new JobRun({ id: "r", status: "running" }).done, false);
  });
  it("a negative rateLimitTimeout is bad_request", async () => {
    await assert.rejects(() => toft.data.update(() => {}, { rateLimitTimeout: -5 }), coded("bad_request", false));
    await assert.rejects(() => toft.data.update(() => {}, { rateLimitTimeout: NaN }), coded("bad_request", false));
    assert.equal(requests.length, 0);
  });
});

describe("a word that is not a role (BB-024)", () => {
  it("atLeast refuses it", () => {
    for (const word of ["superuser", "", "Editor", undefined]) {
      assert.throws(() => new Caller("owner").atLeast(word), coded("bad_request", false));
    }
  });
  it("roles off the ladder answer false", () => {
    for (const word of ["system", "app", "unknown"]) assert.equal(new Caller("owner").atLeast(word), false);
  });
});

describe("send with no kind (BB-020)", () => {
  it("names the kind, not the text", async () => {
    await assert.rejects(() => toft.notify.send("", "hi"),
      (err) => coded("bad_request", false)(err) && /kind/.test(err.message));
    assert.equal(requests.length, 0);
  });
});

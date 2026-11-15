/**
 * The second 2026-11-03 black-box list, Node's half: arguments the SDK refuses
 * itself, coded `bad_request` and before any request — the fake platform below
 * counts what reached it, and every refusal here must leave that count at 0.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, {
  AppTransport,
  RUN_STATUSES,
  Run,
  ToftError,
  azureCredential,
  resetTokenCache,
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
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end('{"version":1}');
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  requests = [];
  resetTokenCache();
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

async function refusedLocally(fn) {
  await assert.rejects(
    async () => fn(),
    (err) => err instanceof ToftError && err.code === "bad_request" && err.retryable === false,
  );
  assert.equal(requests.length, 0, "nothing reached the platform");
}

describe("NaN and Infinity in a body (BB-001)", () => {
  it("data.set of NaN is bad_request and writes nothing", () =>
    refusedLocally(() => toft.data.set({ x: NaN })));
  it("data.set of Infinity, nested, is bad_request", () =>
    refusedLocally(() => toft.data.set({ a: { b: [1, -Infinity] } })));
  it("data.update writing NaN is bad_request", async () => {
    await assert.rejects(
      () => toft.data.update((d) => { d.x = NaN; }),
      (err) => err instanceof ToftError && err.code === "bad_request",
    );
    assert.ok(requests.every((r) => r.method === "GET"), "the read only, no write");
  });
  it("a finite document still goes", async () => {
    await toft.data.set({ x: 1.5 });
    assert.equal(requests.length, 1);
  });
});

describe("empty identifiers (BB-003)", () => {
  it("accessToken('') is bad_request", () => refusedLocally(() => toft.accessToken("")));
  it("jobs.start('') is bad_request", () => refusedLocally(() => toft.jobs.start("")));
  it("jobs.get('') is bad_request", () => refusedLocally(() => toft.jobs.get("")));
});

describe("local argument checks (BB-004)", () => {
  for (const limit of [-1, 0, 201, 1.5, NaN]) {
    it(`jobs.list limit ${limit} is bad_request`, () => refusedLocally(() => toft.jobs.list({ limit })));
  }
  it("jobs.list limit 200 is sent", async () => {
    await toft.jobs.list({ limit: 200 });
    assert.match(requests[0].path, /limit=200/);
  });
  for (const percent of [150, -1, NaN, "40"]) {
    it(`run.progress(${String(percent)}) is bad_request`, () => {
      const run = new Run({ baseUrl: base, token: "t", id: "run-1" });
      return refusedLocally(() => run.progress(percent, "x"));
    });
  }
});

describe("an email's to (BB-012)", () => {
  for (const to of [null, 5, { a: 1 }, [5]]) {
    it(`to ${JSON.stringify(to)} is bad_request`, () =>
      refusedLocally(() => toft.notify.email({ to, subject: "s", text: "t" })));
  }
  it("an empty list is still no_recipient", async () => {
    await assert.rejects(
      () => toft.notify.email({ to: [], subject: "s", text: "t" }),
      (err) => err.code === "no_recipient",
    );
  });
});

describe("azureCredential (DOC-7)", () => {
  for (const kind of ["gdrive", "", undefined]) {
    it(`azureCredential(${JSON.stringify(kind)}) is bad_request`, () =>
      assert.throws(() => azureCredential(kind), (err) => err.code === "bad_request"));
  }
  it("sharepoint and onedrive are credentials", () => {
    assert.equal(typeof toft.azureCredential("sharepoint").getToken, "function");
    assert.equal(typeof toft.azureCredential("onedrive").getToken, "function");
  });
});

describe("RUN_STATUSES (DOC-8)", () => {
  it("is on the default export as well as named", () => {
    assert.equal(toft.RUN_STATUSES, RUN_STATUSES);
    assert.ok(toft.RUN_STATUSES.has("cancelled"));
  });
});

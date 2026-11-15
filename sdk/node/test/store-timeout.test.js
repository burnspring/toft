import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import { storage } from "../app.js";
import { AppTransport, Run, ToftError, resetTransport } from "../index.js";
import { JobRun } from "../jobs.js";
import { UPLOAD_TIMEOUT_MS } from "../transport.js";

// Every object-store transfer carries the five-minute bound (SDK-6): a
// presigned download, a signed store call and a run's output upload. Before,
// these three were bare fetches bounded only by undici's idle timeouts. The
// platform half is a real fake; the store half is a fetch that records the
// signal it was handed and then times out, so no test waits five minutes.

let server;
let base;
let routes = {};
let storeCalls = [];
const realFetch = globalThis.fetch;

before(async () => {
  server = createServer((req, res) => {
    const entry = routes[`${req.method} ${req.url.split("?")[0]}`];
    req.resume();
    req.on("end", () => {
      if (!entry) {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end('{"error":"no such route"}');
        return;
      }
      res.writeHead(entry.status, { "Content-Type": "application/json", ...(entry.headers ?? {}) });
      res.end(JSON.stringify(entry.payload ?? {}));
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  routes = {};
  storeCalls = [];
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
  process.env.TOFT_STORAGE = JSON.stringify([
    { name: "uploads", kind: "store", path: "", access: "write", folder: "" },
  ]);
  // The store lives at /bucket on the fake; anything else is the platform.
  globalThis.fetch = async (url, init = {}) => {
    if (!String(url).includes("/bucket/")) return realFetch(url, init);
    storeCalls.push({ url: String(url), signal: init.signal });
    throw new DOMException("The operation was aborted due to timeout", "TimeoutError");
  };
});

afterEach(() => {
  globalThis.fetch = realFetch;
  delete process.env.TOFT_OUTPUT_URI;
});

function assertBounded(err) {
  assert.ok(err instanceof ToftError, `want a ToftError, got ${err}`);
  assert.match(err.message, new RegExp(`within ${UPLOAD_TIMEOUT_MS}ms`));
  assert.equal(storeCalls.length, 1);
  assert.ok(storeCalls[0].signal instanceof AbortSignal, "the store fetch had no signal");
  return true;
}

describe("an object-store transfer is bounded", () => {
  it("at five minutes, as in the other three SDKs", () => {
    assert.equal(UPLOAD_TIMEOUT_MS, 300_000);
  });

  it("on a signed store call", async () => {
    routes["GET /api/v1/app/store/uploads/objects/report.csv"] = {
      status: 200,
      payload: { url: `${base}/bucket/report.csv`, method: "GET", headers: {} },
    };
    await assert.rejects(() => storage("uploads").download("report.csv"), assertBounded);
  });

  it("on the presigned hop of a run's output", async () => {
    routes["GET /api/v1/app/job-runs/run_1/output"] = {
      status: 302,
      headers: { Location: `${base}/bucket/run_1` },
    };
    await assert.rejects(() => new JobRun({ id: "run_1" }).output(), assertBounded);
  });

  it("on a run's output upload", async () => {
    process.env.TOFT_OUTPUT_URI = `${base}/bucket/out`;
    const run = new Run({ baseUrl: base, token: "run-token", id: "run-1" });
    await assert.rejects(() => run.writeOutput("rows"), assertBounded);
  });
});

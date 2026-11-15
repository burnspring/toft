import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import { after, before, beforeEach, describe, it } from "node:test";

import { store, storage } from "../app.js";
import { AppTransport, ToftError, resetTransport } from "../index.js";

// A store reached over the platform's signed URLs — the scenarios the Python
// and Go SDKs pass. The signed URLs point back at the fake, so the object-store
// half of each call is recorded: signed headers verbatim, and no Toft token.

let server;
let base;
let routes = {};
let requests = [];

before(async () => {
  server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      requests.push({ method: req.method, url: req.url, headers: req.headers, body: Buffer.concat(chunks) });
      const entry = routes[`${req.method} ${req.url.split("?")[0]}`];
      if (!entry) {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end('{"error":"no such route"}');
        return;
      }
      res.writeHead(entry.status, { "Content-Type": "application/json" });
      res.end(typeof entry.payload === "string" ? entry.payload : JSON.stringify(entry.payload ?? {}));
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
  process.env.TOFT_STORAGE = JSON.stringify([
    // Shared mode: a store is not mounted, and says so with no path.
    { name: "uploads", kind: "store", path: "", access: "write", folder: "in" },
    { name: "cache", kind: "volume", path: "/data/cache", access: "write" },
  ]);
});

const signed = (method, headers = {}) => ({
  status: 200,
  payload: { url: `${base}/bucket/obj`, method, headers, key: "report.csv" },
});

async function source() {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "toft-urls-"));
  const file = path.join(dir, "report.csv");
  await fs.writeFile(file, "a,b\n");
  return file;
}

describe("a store over signed URLs", () => {
  it("has no path to write into when it is not mounted", async () => {
    const handle = store("uploads");
    assert.equal(handle.mounted, false);
    assert.throws(() => handle.path, /upload\(\)/);
    await assert.rejects(() => handle.put("x"), /upload\(\)/);
  });

  it("signs, then sends the bytes with the signed headers and no token", async () => {
    routes["POST /api/v1/app/store/uploads/objects"] = signed("PUT", { "If-None-Match": "*" });
    routes["PUT /bucket/obj"] = { status: 200 };
    assert.equal(await store("uploads").upload(await source()), "report.csv");

    const [mint, put] = requests;
    assert.match(mint.url, /folder=in/);
    const sent = JSON.parse(mint.body.toString());
    assert.equal(sent.sha256, createHash("sha256").update("a,b\n").digest("hex"));
    assert.equal(put.headers["if-none-match"], "*");
    assert.equal(put.headers.authorization, undefined);
    assert.equal(put.body.toString(), "a,b\n");
  });

  it("streams a large upload with its length declared, never chunked or read whole", async (t) => {
    // Security review 2026-11-14, CS-11.
    const payload = Buffer.alloc(3 * 1024 * 1024 + 17);
    for (let i = 0; i < payload.length; i++) payload[i] = (i * 31) & 0xff;
    const file = await source();
    await fs.writeFile(file, payload);
    const whole = t.mock.method(fs, "readFile");
    routes["POST /api/v1/app/store/uploads/objects"] = signed("PUT");
    routes["PUT /bucket/obj"] = { status: 200 };
    await store("uploads").upload(file);

    assert.equal(whole.mock.callCount(), 0, "the file was read whole");
    const [mint, put] = requests;
    const sent = JSON.parse(mint.body.toString());
    assert.equal(sent.size, payload.length);
    assert.equal(sent.sha256, createHash("sha256").update(payload).digest("hex"));
    assert.equal(sent.md5, createHash("md5").update(payload).digest("hex"));
    assert.equal(put.headers["content-length"], String(payload.length));
    assert.equal(put.headers["transfer-encoding"], undefined);
    assert.ok(put.body.equals(payload), "the body arrived changed");
  });

  it("says so when write lands on an existing name", async () => {
    routes["POST /api/v1/app/store/uploads/objects"] = signed("PUT", { "If-None-Match": "*" });
    routes["PUT /bucket/obj"] = { status: 412 };
    await assert.rejects(async () => store("uploads").upload(await source()), /already exists/);
  });

  it("says not_found when a download names nothing", async () => {
    routes["GET /api/v1/app/store/uploads/objects/sales.db"] = signed("GET");
    routes["GET /bucket/obj"] = { status: 404 };
    await assert.rejects(async () => store("uploads").download("sales.db"), (err) => {
      assert.equal(err.code, "not_found");
      assert.equal(err.retryable, false);
      assert.match(err.message, /"sales\.db" is not in "uploads"/);
      return true;
    });
  });

  it("signs a GET for a link and for a download", async () => {
    routes["GET /api/v1/app/store/uploads/objects/2026/report.csv"] = signed("GET");
    routes["GET /bucket/obj"] = { status: 200, payload: "the bytes" };
    const link = await store("uploads").url("2026/report.csv", { attachment: true });
    assert.equal(link, `${base}/bucket/obj`);
    assert.match(requests[0].url, /disposition=attachment/);
    assert.equal((await store("uploads").download("2026/report.csv")).toString(), "the bytes");
  });

  it("asks the platform for a lasting link, carrying the folder", async () => {
    routes["GET /api/v1/app/store/uploads/link/2026/q1%20report.csv"] = {
      status: 200, payload: { link: "https://toft.example/files/uploads/in/2026/q1%20report.csv" } };
    const link = await store("uploads").link("2026/q1 report.csv");
    assert.equal(link, "https://toft.example/files/uploads/in/2026/q1%20report.csv");
    assert.match(requests[0].url, /folder=in/);
    for (const name of ["../escape", "/abs", "a//b"]) {
      await assert.rejects(() => store("uploads").link(name), ToftError, name);
    }
  });

  it("refuses a name that leaves the store, and a volume, before asking", async () => {
    for (const name of ["../escape", "/abs", "a//b"]) {
      await assert.rejects(() => store("uploads").url(name), ToftError, name);
    }
    await assert.rejects(() => storage("cache").list(), ToftError);
    assert.deepEqual(requests, []);
  });
});

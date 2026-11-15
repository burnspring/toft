import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import { Run } from "../index.js";

// writeOutput mints its URL at the moment it writes and falls back to
// TOFT_OUTPUT_URI; every output PUT carries Azure Blob's x-ms-blob-type, which
// S3 ignores (security review 2026-11-14, AS-2 and AS-4).

let server;
let base;
let mint = true;
let seen = [];

before(async () => {
  server = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      seen.push({ method: req.method, path: req.url, headers: req.headers });
      if (req.method === "POST" && req.url === "/api/v1/runner/output-url") {
        if (!mint || req.headers.authorization !== "Bearer run-token") {
          res.writeHead(404, { "Content-Type": "application/json" });
          res.end('{"error":"no such route"}');
          return;
        }
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ url: `${base}/fresh` }));
        return;
      }
      res.writeHead(req.method === "PUT" ? 201 : 418);
      res.end();
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  seen = [];
  process.env.TOFT_OUTPUT_URI = `${base}/stale`;
});

describe("writeOutput", () => {
  it("uses the URL minted at write time", async () => {
    mint = true;
    await new Run({ baseUrl: base, token: "run-token", id: "run-1" }).writeOutput("rows");
    const put = seen.at(-1);
    assert.equal(put.method, "PUT");
    assert.equal(put.path, "/fresh");
    assert.equal(put.headers["x-ms-blob-type"], "BlockBlob");
  });

  it("falls back to TOFT_OUTPUT_URI", async () => {
    mint = false;
    await new Run({ baseUrl: base, token: "run-token", id: "run-1" }).writeOutput("rows");
    const put = seen.at(-1);
    assert.equal(put.path, "/stale");
    assert.equal(put.headers["x-ms-blob-type"], "BlockBlob");
  });
});

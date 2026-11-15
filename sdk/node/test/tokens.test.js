/**
 * Contract tests for a file connection's access token, against a fake platform.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * These are the same scenarios `sdk/python/tests/test_app_sdk.py` runs in
 * `TestAccessTokens` — same route, the same cache and the same adapter shape — so that an app written in one language cannot observe a
 * platform the other cannot. When one side gains a scenario, the other gains it
 * too; that is the whole reason the table is duplicated rather than shared.
 */

import assert from "node:assert/strict";
import { inspect } from "node:util";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, ToftError, resetTokenCache, resetTransport } from "../index.js";

let server;
let base;
let routes = {};
let requests = [];

/** Script one route. An array means "answer these in order". */
function route(key, entries) {
  routes[key] = entries;
}


before(async () => {
  server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks).toString();
      requests.push({ method: req.method, path: req.url, headers: req.headers, body });

      const script = routes[`${req.method} ${req.url.split("?")[0]}`];
      if (!script) {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end('{"error":"no such route"}');
        return;
      }
      const entry = script.length > 1 ? script.shift() : script[0];
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
  requests = [];
  resetTokenCache();
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

function issue(minutes) {
  route("POST /api/v1/app/tokens/sharepoint", [{
    status: 200,
    payload: {
      access_token: "eyJ.tok", token_type: "Bearer",
      expires_at: new Date(Date.now() + minutes * 60_000).toISOString(),
      resource: "https://graph.microsoft.com",
      folder: { drive_id: "b!d", item_id: "01R", label: "reports" },
    },
  }]);
}

describe("access tokens", () => {
  it("fetches once and caches", async () => {
    issue(60);
    const first = await toft.accessToken("sharepoint");
    const again = await toft.accessToken("sharepoint");
    assert.equal(first.value, "eyJ.tok");
    assert.equal(first.folder.driveId, "b!d");
    assert.equal(first, again);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].method, "POST");
    assert.ok(!JSON.stringify(first).includes("eyJ"));
    // console.log uses util.inspect, which never calls toJSON.
    assert.ok(!inspect(first).includes("eyJ"), inspect(first));
    assert.ok(!Object.keys(first).includes("value"));
  });

  it("escapes the kind into one segment", async () => {
    await assert.rejects(toft.accessToken("sharepoint/../../data"), ToftError);
    assert.equal(requests[requests.length - 1].path, "/api/v1/app/tokens/sharepoint%2F..%2F..%2Fdata");
  });

  it("replaces a token near expiry", async () => {
    issue(4);
    await toft.accessToken("sharepoint");
    await toft.accessToken("sharepoint");
    assert.equal(requests.length, 2);
  });

  it("adapts to the shape @azure/identity calls", async () => {
    issue(60);
    const got = await toft.azureCredential("sharepoint").getToken(["https://graph.microsoft.com/.default"]);
    assert.equal(got.token, "eyJ.tok");
    assert.equal(typeof got.expiresOnTimestamp, "number");
  });

  it("carries the platform's words on a refusal", async () => {
    route("POST /api/v1/app/tokens/sharepoint", [{
      status: 403,
      payload: { error: "this SharePoint connection does not hand out access tokens", code: "sharepoint_tokens_off" },
    }]);
    await assert.rejects(toft.accessToken("sharepoint"), (e) =>
      e instanceof ToftError && /does not hand out access tokens/.test(e.message));
  });
});

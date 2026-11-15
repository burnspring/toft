/**
 * Contract tests for the app's view of itself, against a fake platform.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * The same scenarios `sdk/python/tests/test_app_sdk.py` runs in `TestIdentity`,
 * so an app written in one language cannot observe a platform the other cannot.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, ToftError, resetTransport } from "../index.js";

let server;
let base;
let view = {};

before(async () => {
  server = createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(view));
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  view = {};
  delete process.env.BASE_PATH;
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
  toft.app.refresh();
});

/**
 * `basePath()` is the one value an app is told never to get wrong: links and
 * form actions carry it, and a root-absolute URL asks the platform rather than
 * the app and 404s. Reporting `""` as fact is how it got it wrong — and `""` is
 * also the honest answer on a per-app-origin install, so an app had nothing it
 * could check to tell the two apart.
 */
describe("base path", () => {
  it("is what the platform sent", async () => {
    view = { id: "a", slug: "reporter", base_path: "/apps/reporter" };
    process.env.BASE_PATH = "/apps/ignored";
    assert.equal(await toft.app.basePath(), "/apps/reporter");
  });

  it("falls back to the injected variable on an install older than the field", async () => {
    view = { id: "a", slug: "reporter" };
    process.env.BASE_PATH = "/apps/reporter";
    assert.equal(await toft.app.basePath(), "/apps/reporter");
  });

  it("is empty when neither half says otherwise", async () => {
    view = { id: "a", slug: "reporter" };
    assert.equal(await toft.app.basePath(), "");
  });
});

/**
 * `slug` and `id` are plain facts the platform puts in the environment, so they
 * are synchronous properties: as `async slug()` they were a Promise, and a
 * template literal rendered `[object Promise]` into every link.
 */
describe("plain facts", () => {
  it("slug and id are synchronous, from the environment", () => {
    process.env.TOFT_APP_SLUG = "reporter";
    process.env.TOFT_APP_ID = "app_1";
    try {
      assert.equal(`/apps/${toft.app.slug}/`, "/apps/reporter/");
      assert.equal(toft.app.id, "app_1");
    } finally {
      delete process.env.TOFT_APP_SLUG;
      delete process.env.TOFT_APP_ID;
    }
  });

  it("fall back to a view already fetched, and are off_platform with neither (BB-034)", async () => {
    // "" built `/apps//` into links and passed for a value; Python and Go ask
    // the platform for both, which is off_platform here.
    for (const get of [() => toft.app.slug, () => toft.app.id]) {
      assert.throws(get, (err) => err instanceof ToftError && err.code === "off_platform" && err.retryable === false);
    }
    view = { id: "app_2", slug: "ledger", name: "Ledger" };
    assert.equal(await toft.app.name(), "Ledger");
    assert.equal(toft.app.slug, "ledger");
    assert.equal(await toft.app.slug, "ledger");
  });
});

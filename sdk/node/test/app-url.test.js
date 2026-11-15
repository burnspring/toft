/**
 * `toft.app.url(path)`: the shared join vector (sdk/testdata/app-url-vector.json,
 * read by all four SDKs) and Node's half of the fetch rules — asked of
 * `/api/v1/app/url`, reused briefly and then asked again, the last answer used
 * when the platform cannot be reached, a refusal thrown.
 */

import assert from "node:assert/strict";
import fs from "node:fs";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it, mock } from "node:test";

import toft, { AppTransport, ToftError, resetTransport } from "../index.js";
import { URL_TTL_MS, joinAppUrl } from "../app.js";

const vector = JSON.parse(fs.readFileSync(new URL("../../testdata/app-url-vector.json", import.meta.url), "utf8"));

describe("the shared app-url vector", () => {
  it("has cases", () => assert.ok(vector.cases.length > 0));
  for (const tc of vector.cases) {
    it(tc.name, () => {
      if (tc.error) {
        assert.throws(() => joinAppUrl(tc.public_url, tc.base_path, tc.path), (err) => {
          assert.ok(err instanceof ToftError);
          assert.equal(err.code, tc.error);
          assert.ok(err.hint);
          return true;
        });
      } else {
        assert.equal(joinAppUrl(tc.public_url, tc.base_path, tc.path), tc.want);
      }
    });
  }
});

let server;
let base;
let answer;
let asked;

before(async () => {
  server = createServer((req, res) => {
    if (req.url === "/api/v1/app/url") asked += 1;
    const [status, body] = answer;
    res.writeHead(status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

beforeEach(() => {
  asked = 0;
  answer = [200, { public_url: "https://apps.example.com/apps/reporter", base_path: "/apps/reporter" }];
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
  toft.app.refresh();
});

describe("toft.app.url", () => {
  it("is built from the platform's answer", async () => {
    assert.equal(await toft.app.url("/reports/42"), "https://apps.example.com/apps/reporter/reports/42");
    assert.equal(asked, 1);
  });

  it("is reused briefly, then asked again", async () => {
    let now = 1_000_000;
    const clock = mock.method(Date, "now", () => now);
    try {
      await toft.app.url();
      await toft.app.url("/a");
      assert.equal(asked, 1);
      answer = [200, { public_url: "https://applications.example.org/apps/reporter", base_path: "/apps/reporter" }];
      now += URL_TTL_MS + 1;
      assert.equal(await toft.app.url("/a"), "https://applications.example.org/apps/reporter/a");
      assert.equal(asked, 2);
    } finally {
      clock.mock.restore();
    }
  });

  it("uses the last answer when the platform cannot be reached", async () => {
    let now = 1_000_000;
    const clock = mock.method(Date, "now", () => now);
    try {
      await toft.app.url();
      now += URL_TTL_MS + 1;
      resetTransport(new AppTransport({ baseUrl: "http://127.0.0.1:1", token: "tofta_test" }));
      assert.equal(await toft.app.url("/a"), "https://apps.example.com/apps/reporter/a");
    } finally {
      clock.mock.restore();
    }
  });

  it("throws the platform's refusal when there is no address", async () => {
    answer = [503, { error: "no address", code: "no_public_address", retryable: false, hint: "set APPS_BASE_URL" }];
    await assert.rejects(toft.app.url(), (err) => err instanceof ToftError && err.code === "no_public_address" && !!err.hint);
  });

  it("refuses a non-string before asking", async () => {
    await assert.rejects(toft.app.url(42), (err) => err.code === "bad_request");
    assert.equal(asked, 0);
  });
});

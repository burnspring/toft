/**
 * Whether to try again is the platform's call.
 *
 * Every error body the platform writes carries `retryable`, because only the
 * handler knows that a 503 means "this install is configured without it" or
 * that a 409 means "wait"; the status rule is the fallback for answers that
 * never reached a handler. The same table runs in Go, Python and R.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, ToftError, resetTransport } from "../index.js";

describe("the error envelope", () => {
  it("the body's retryable wins over the status", () => {
    const cases = [
      [503, { code: "jobs_unavailable", retryable: false }, false],
      [409, { code: "queue_full", retryable: true }, true],
      [503, { error: "x" }, true],
      [409, { error: "x" }, false],
      [429, {}, true],
      [undefined, {}, true],
      [400, { retryable: "yes" }, false],
    ];
    for (const [status, body, want] of cases) {
      assert.equal(new ToftError("x", { status, body }).retryable, want, JSON.stringify({ status, body }));
    }
  });

  it("an off-platform refusal stays unretryable whatever a body says", () => {
    const err = new ToftError("x", { offPlatform: true, body: { retryable: true } });
    assert.equal(err.retryable, false);
    assert.equal(err.autoRetry, false);
  });

  it("hint is the platform's next step", () => {
    const err = new ToftError("x", { status: 401, body: { code: "key_expired", hint: "mint a new key" } });
    assert.equal(err.code, "key_expired");
    assert.equal(err.hint, "mint a new key");
    assert.equal(new ToftError("x", { status: 404 }).hint, undefined);
  });
});

describe("the built-in retry", () => {
  let server;
  let reply;
  let calls = 0;

  before(async () => {
    server = createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        calls++;
        res.writeHead(reply.status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(reply.body));
      });
    });
    await new Promise((r) => server.listen(0, "127.0.0.1", r));
    resetTransport(
      new AppTransport({ baseUrl: `http://127.0.0.1:${server.address().port}`, token: "tofta_test" }),
    );
  });
  after(() => {
    server.close();
    resetTransport();
  });
  beforeEach(() => {
    calls = 0;
  });

  it("honours the platform's veto on a 5xx", async () => {
    reply = {
      status: 503,
      body: { error: "not configured", code: "unavailable", retryable: false, hint: "an administrator configures it" },
    };
    await assert.rejects(
      () => toft.notify.slack("hello"),
      (err) => {
        assert.equal(err.retryable, false);
        assert.equal(err.hint, "an administrator configures it");
        return true;
      },
    );
    assert.equal(calls, 1);
  });

  it("leaves a retryable 4xx to the caller", async () => {
    reply = { status: 409, body: { error: "wait", code: "busy", retryable: true } };
    await assert.rejects(
      () => toft.notify.slack("hello"),
      (err) => err.retryable === true,
    );
    assert.equal(calls, 1);
  });
});

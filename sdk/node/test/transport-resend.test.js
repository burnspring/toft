/**
 * The transport's single resend, and a success that is not Toft's API.
 *
 * BB-002: a 5xx may come from a handler that already did the work, so a write
 * without an Idempotency-Key is never sent twice. BB-003: a timeout is not
 * resent, and the whole call is held to one bound. BB-014: a 200 web page is
 * `not_toft`, coded and not retryable, with a hint. The same cases run in Go,
 * Python and R.
 */

import assert from "node:assert/strict";
import { createServer } from "node:net";
import { createServer as createHttp } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, ToftError, resetTransport } from "../index.js";

describe("the built-in resend", () => {
  let server;
  let answer;
  let calls;

  const use = (timeoutMs) => resetTransport(new AppTransport({
    baseUrl: `http://127.0.0.1:${server.address().port}`, token: "t", timeoutMs,
  }));

  before(async () => {
    server = createHttp((req, res) => {
      req.resume();
      req.on("end", () => {
        calls.push({ method: req.method, key: req.headers["idempotency-key"] });
        answer(res, calls.length);
      });
    });
    await new Promise((r) => server.listen(0, "127.0.0.1", r));
  });
  after(() => {
    server.closeAllConnections?.();
    server.close();
    resetTransport();
  });
  beforeEach(() => {
    calls = [];
    use(30_000);
  });

  const send = (res, status, body, type = "application/json", headers = {}) => {
    res.writeHead(status, { "Content-Type": type, ...headers });
    res.end(typeof body === "string" ? body : JSON.stringify(body));
  };

  it("does not resend an unkeyed write after a 5xx", async () => {
    answer = (res) => send(res, 500, { error: "boom" }, "application/json", { "Retry-After": "0" });
    for (const [name, call] of [
      ["jobs.start", () => toft.jobs.start("echo")],
      ["data.set", () => toft.data.set({ a: 1 })],
      ["notify.slack", () => toft.notify.slack("x")],
    ]) {
      calls = [];
      await assert.rejects(call, (err) => {
        assert.ok(err instanceof ToftError, name);
        assert.deepEqual([err.status, err.code, err.retryable], [500, "", true], name);
        return true;
      });
      assert.equal(calls.length, 1, `${name} was sent twice`);
    }
  });

  it("resends a keyed write once after a 5xx, with the same key", async () => {
    answer = (res, n) => n === 1
      ? send(res, 502, { error: "gateway" }, "application/json", { "Retry-After": "0" })
      : send(res, 202, { run_id: "run_1", status: "queued" });
    await toft.jobs.start("echo", { idempotencyKey: "k1" });
    assert.equal(calls.length, 2);
    assert.equal(calls[0].key, calls[1].key);
  });

  it("resends a rate-limited unkeyed write once", async () => {
    answer = (res, n) => n === 1
      ? send(res, 429, { error: "slow", code: "rate_limited", retryable: true }, "application/json", { "Retry-After": "0" })
      : send(res, 200, { version: 1, data: { a: 1 } });
    await toft.data.set({ a: 1 });
    assert.equal(calls.length, 2);
  });

  it("waits a rate limit out rather than resending once (TOFT-BB-002)", async () => {
    answer = (res, n) => n <= 3
      ? send(res, 429, { error: "slow", code: "rate_limited", retryable: true }, "application/json", { "Retry-After": "0" })
      : send(res, 200, { version: 1, data: { a: 1 } });
    await toft.data.set({ a: 1 });
    assert.equal(calls.length, 4);
  });

  it("does not resend a timeout, and holds the call to one bound", async () => {
    use(300);
    answer = () => {}; // never answers
    const start = Date.now();
    await assert.rejects(() => toft.jobs.start("echo"), (err) => {
      assert.deepEqual([err.code, err.retryable], ["", true]);
      return true;
    });
    assert.ok(Date.now() - start < 1500, `took ${Date.now() - start}ms`);
    assert.equal(calls.length, 1);
  });

  it("names the call's whole bound when the resend times out (BB-015)", async () => {
    use(1500);
    // The first answer is a quick 5xx; the resend of the GET then never answers,
    // and times out on what was left of the bound — the message names all of it.
    answer = (res, n) => {
      if (n === 1) send(res, 500, { error: "boom" }, "application/json", { "Retry-After": "0" });
    };
    await assert.rejects(() => toft.data.get(), (err) => {
      assert.deepEqual([err.code, err.retryable], ["", true]);
      assert.match(err.message, /did not answer within 1\.5s$/);
      return true;
    });
    assert.equal(calls.length, 2);
  });

  it("a 200 web page is not_toft", async () => {
    answer = (res) => send(res, 200, "<!doctype html><html>Sign in</html>", "text/html");
    await assert.rejects(() => toft.data.get(), (err) => {
      assert.deepEqual([err.code, err.retryable, err.status], ["not_toft", false, 200]);
      assert.ok(err.hint);
      assert.match(err.message, /web page/);
      return true;
    });
    assert.equal(calls.length, 1);
  });
});

describe("a connection that never opened", () => {
  it("is resent once, even for an unkeyed write", async () => {
    const probe = createServer();
    await new Promise((r) => probe.listen(0, "127.0.0.1", r));
    const port = probe.address().port;
    await new Promise((r) => probe.close(r));
    const t = new AppTransport({ baseUrl: `http://127.0.0.1:${port}`, token: "t", timeoutMs: 10_000 });
    const start = Date.now();
    await assert.rejects(() => t.postJson("/api/v1/app/jobs/echo/runs", {}), (err) => {
      assert.deepEqual([err.code, err.retryable, err.sent], ["", true, false]);
      return true;
    });
    // The resend waits the default two seconds before trying again.
    assert.ok(Date.now() - start >= 1900, "no resend happened");
  });

  // BB-004: fetch refuses a port on its blocked list (1 among them) before it
  // opens anything, with an uncoded "bad port" — which read as "sent", so an
  // unkeyed write was tried once while a GET was tried twice.
  it("is resent once on a port fetch will not open, for every method", async () => {
    const realFetch = globalThis.fetch;
    let attempts = 0;
    globalThis.fetch = (...a) => { attempts++; return realFetch(...a); };
    try {
      resetTransport(new AppTransport({ baseUrl: "http://127.0.0.1:1", token: "t", timeoutMs: 10_000 }));
      for (const [name, call] of [
        ["data.set", () => toft.data.set({ a: 1 })],
        ["jobs.start", () => toft.jobs.start("fast")],
        ["data.get", () => toft.data.get()],
      ]) {
        attempts = 0;
        await assert.rejects(call, (err) => {
          assert.deepEqual([err.code, err.retryable, err.sent], ["", true, false], name);
          return true;
        });
        assert.equal(attempts, 2, `${name} made ${attempts} attempts`);
      }
    } finally {
      globalThis.fetch = realFetch;
      resetTransport();
    }
  });
});

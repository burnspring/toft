/**
 * Contract tests for the app's one JSON document, against a fake platform.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * These are the same scenarios `sdk/python/tests/test_app_sdk.py` runs in
 * `TestAppData` — same route, same body shape, same precondition header, same
 * typed conflict — so that an app written in one language cannot observe a
 * platform the other cannot. When one side gains a scenario, the other gains it
 * too; that is the whole reason the table is duplicated rather than shared.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, { AppTransport, ToftError, VersionConflict, resetTransport } from "../index.js";

let server;
let base;
let routes = {};
let requests = [];

/** Script one route. An array means "answer these in order". */
function route(key, entries) {
  routes[key] = entries;
}

const puts = () => requests.filter((r) => r.method === "PUT");
const last = () => requests[requests.length - 1];

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
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

describe("reading", () => {
  it("reads never-written as null", async () => {
    route("GET /api/v1/app/data", [
      { status: 200, payload: { data: null, version: 0, updated_at: null, bytes: 0 } },
    ]);
    assert.equal(await toft.data.get(), null);
    assert.equal((await toft.data.read()).empty, true);
  });

  it("carries the version a conditional write is built on", async () => {
    route("GET /api/v1/app/data", [
      { status: 200, payload: { data: { period: "2026-03" }, version: 7, bytes: 21 } },
    ]);
    const doc = await toft.data.read();
    assert.deepEqual(doc.data, { period: "2026-03" });
    assert.equal(doc.version, 7);
    assert.equal(doc.bytes, 21);
    assert.equal(doc.empty, false);
  });
});

describe("writing", () => {
  it("sends the document as the body, with no precondition by default", async () => {
    route("PUT /api/v1/app/data", [{ status: 200, payload: { version: 1, bytes: 21 } }]);
    const saved = await toft.data.set({ period: "2026-03" });

    // The body *is* the document — not a wrapper with it inside a field.
    assert.deepEqual(JSON.parse(last().body), { period: "2026-03" });
    assert.equal(last().headers["if-match"], undefined);
    assert.equal(saved.version, 1);
  });

  // The platform's reply to a PUT is metadata only — version, timestamp, size.
  // Without filling it in, `const doc = await toft.data.set(...)` came back with
  // `data: null` — the value this API reserves for "never written" — for a
  // document that had just been written and is not empty.
  it("returns a document carrying what was written", async () => {
    route("PUT /api/v1/app/data", [{ status: 200, payload: { version: 2, bytes: 21 } }]);
    const saved = await toft.data.set({ period: "2026-03" });
    assert.deepEqual(saved.data, { period: "2026-03" });
    assert.equal(saved.empty, false);

    route("GET /api/v1/app/data", [{ status: 200, payload: { data: { runs: 1 }, version: 1 } }]);
    route("PUT /api/v1/app/data", [{ status: 200, payload: { version: 2, bytes: 12 } }]);
    const updated = await toft.data.update((s) => {
      s.runs = (s.runs ?? 0) + 1;
    });
    assert.deepEqual(updated.data, { runs: 2 });
  });

  it("sends the precondition when one is named", async () => {
    route("PUT /api/v1/app/data", [{ status: 200, payload: { version: 4 } }]);
    await toft.data.set({ a: 1 }, { ifVersion: 3 });
    assert.equal(last().headers["if-match"], '"3"');
  });

  it("types a failed precondition and carries the stored version", async () => {
    route("PUT /api/v1/app/data", [
      {
        status: 412,
        payload: { error: "the document changed", code: "version_conflict", version: 9 },
      },
    ]);
    await assert.rejects(() => toft.data.set({ a: 1 }, { ifVersion: 3 }), (err) => {
      assert.ok(err instanceof VersionConflict);
      assert.equal(err.version, 9);
      return true;
    });
  });

  it("refuses anything but an object, locally", async () => {
    // Not a duplicated platform limit: `undefined` here would be serialized as
    // `{}` and quietly wipe the app's settings.
    for (const value of [undefined, null, [1, 2], "hello", 3]) {
      await assert.rejects(() => toft.data.set(value), ToftError);
    }
    assert.equal(requests.length, 0);
  });
});

describe("update", () => {
  it("re-reads and re-applies after a conflict", async () => {
    route("GET /api/v1/app/data", [
      { status: 200, payload: { data: { runs: 1 }, version: 1 } },
      { status: 200, payload: { data: { runs: 5 }, version: 2 } },
    ]);
    route("PUT /api/v1/app/data", [
      { status: 412, payload: { code: "version_conflict", version: 2, error: "changed" } },
      { status: 200, payload: { version: 3 } },
    ]);

    const out = await toft.data.update((s) => {
      s.runs = (s.runs ?? 0) + 1;
    });

    assert.equal(out.version, 3);
    assert.equal(puts().length, 2);
    // The second attempt is built on the *second* read: the other writer's
    // change survives instead of being overwritten.
    assert.deepEqual(JSON.parse(puts()[0].body), { runs: 2 });
    assert.equal(puts()[0].headers["if-match"], '"1"');
    assert.deepEqual(JSON.parse(puts()[1].body), { runs: 6 });
    assert.equal(puts()[1].headers["if-match"], '"2"');
  });

  it("hands a mutator {} when nothing is stored", async () => {
    route("GET /api/v1/app/data", [{ status: 200, payload: { data: null, version: 0 } }]);
    route("PUT /api/v1/app/data", [{ status: 200, payload: { version: 1 } }]);

    await toft.data.update((s) => ({ ...s, started: true }));

    assert.deepEqual(JSON.parse(last().body), { started: true });
    // Version 0 is a real precondition — "only if nothing is stored" — so two
    // processes racing their first write cannot both think they won.
    assert.equal(last().headers["if-match"], '"0"');
  });

  it("waits out the rate limit rather than losing the write", async () => {
    // Several writers at once spend the app's burst between them; the update
    // is the write that must not be lost to it.
    // More refusals than attempts: a drained limiter is waited out, and the
    // waits do not spend them (trytoft rerun #3).
    const limited = {
      status: 429,
      payload: { code: "rate_limited", retryable: true, error: "slow down" },
      headers: { "Retry-After": "0" },
    };
    route("GET /api/v1/app/data", [{ status: 200, payload: { data: { runs: 1 }, version: 1 } }]);
    route("PUT /api/v1/app/data", [limited, limited, limited, { status: 200, payload: { version: 2 } }]);

    const out = await toft.data.update(
      (s) => {
        s.runs = (s.runs ?? 0) + 1;
      },
      { attempts: 2 },
    );

    assert.equal(out.version, 2);
  });

  it("waits out more than thirty refusals, up to its deadline", async () => {
    // A count of waits gave up after 30 however long the limiter asked for;
    // the bound is a deadline, as Python's is.
    const limited = {
      status: 429,
      payload: { code: "rate_limited", retryable: true, error: "slow down" },
      headers: { "Retry-After": "0" },
    };
    route("GET /api/v1/app/data", [{ status: 200, payload: { data: { runs: 1 }, version: 1 } }]);
    route("PUT /api/v1/app/data", [...Array(35).fill(limited), { status: 200, payload: { version: 2 } }]);
    const out = await toft.data.update((s) => ({ ...s, runs: 2 }));
    assert.equal(out.version, 2);
  });

  it("throws the rate limit itself once the deadline has passed", async () => {
    const limited = {
      status: 429,
      payload: { code: "rate_limited", retryable: true, error: "slow down" },
      headers: { "Retry-After": "0" },
    };
    route("GET /api/v1/app/data", [{ status: 200, payload: { data: {}, version: 1 } }]);
    route("PUT /api/v1/app/data", [limited]);
    await assert.rejects(
      () => toft.data.update((s) => ({ ...s, a: 1 }), { rateLimitTimeout: 0.2 }),
      (err) => err.code === "rate_limited",
    );
  });

  it("gives up rather than looping forever", async () => {
    route("GET /api/v1/app/data", [{ status: 200, payload: { data: {}, version: 1 } }]);
    route("PUT /api/v1/app/data", [
      { status: 412, payload: { code: "version_conflict", version: 2, error: "changed" } },
    ]);
    await assert.rejects(() => toft.data.update((s) => ({ ...s, a: 1 }), { attempts: 2 }));
    assert.equal(puts().length, 2);
  });
});

describe("clearing", () => {
  it("deletes and reports what it removed", async () => {
    route("DELETE /api/v1/app/data", [{ status: 200, payload: { version: 0, deleted: true } }]);
    assert.equal(await toft.data.clear(), true);
    assert.equal(last().method, "DELETE");
  });
});

/**
 * Contract tests for `toft.who()` against a fake platform.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * These are the same scenarios `sdk/python/tests/test_app_sdk.py` runs in
 * `TestWho` — same route, same three answers, same cache — so that an app
 * written in one language cannot observe a platform the other cannot. When one
 * side gains a scenario, the other gains it too.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, {
  ACT_FOR_HEADER,
  AppTransport,
  ToftError,
  resetIdentityCache,
  resetTransport,
} from "../index.js";

let server;
let base;
let routes = {};
let requests = [];

function route(key, entries) {
  routes[key] = entries;
}

const blob = "act-for-blob";
const headers = { [ACT_FOR_HEADER.toLowerCase()]: blob };

const identity = {
  id: "u_1",
  email: "alice@corp.example",
  name: "Alice Chen",
  role: "editor",
  is_admin: false,
  groups: [
    { id: "g_1", name: "finance" },
    { id: "g_2", name: "nyc-office" },
  ],
};

before(async () => {
  server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      requests.push({ method: req.method, path: req.url, headers: req.headers });
      const script = routes[`${req.method} ${req.url.split("?")[0]}`];
      if (!script) {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end('{"error":"no such route"}');
        return;
      }
      const entry = script.length > 1 ? script.shift() : script[0];
      if (entry.status === 204) {
        res.writeHead(204);
        res.end();
        return;
      }
      res.writeHead(entry.status, { "Content-Type": "application/json" });
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
  resetIdentityCache();
  resetTransport(new AppTransport({ baseUrl: base, token: "tofta_test" }));
});

describe("the three answers", () => {
  it("returns the person, with names on their groups", async () => {
    route("GET /api/v1/app/viewer", [{ status: 200, payload: identity }]);

    const me = await toft.who(headers);
    assert.equal(me.id, "u_1");
    assert.equal(me.email, "alice@corp.example");
    assert.equal(me.role, "editor");
    assert.equal(me.isAdmin, false);
    assert.deepEqual(me.groupNames, ["finance", "nyc-office"]);
    // The blob is relayed, not the app's own claim about who is looking.
    assert.equal(requests[0].headers[ACT_FOR_HEADER.toLowerCase()], blob);
  });

  // An anonymous visitor is an answer, not a failure — a public app rendering
  // "sign in to see your reports" should not need a try/catch around it.
  it("resolves to null with no act-for header, without calling the platform", async () => {
    assert.equal(await toft.who({}), null);
    assert.equal(requests.length, 0);
  });

  // 204: the app's credential was valid and the question was answerable. The
  // answer is that there is nobody there — an anonymous viewer, or one whose
  // access was revoked while their page stayed open.
  it("resolves to null on 204", async () => {
    route("GET /api/v1/app/viewer", [{ status: 204 }]);
    assert.equal(await toft.who(headers), null);
  });

  // The one that must never be mistaken for the one above: 403 is *there is a
  // viewer and this app may not be told who*. An app told "nobody" would render
  // its signed-out state to a signed-in person.
  it("throws on 403, naming the setting", async () => {
    route("GET /api/v1/app/viewer", [
      {
        status: 403,
        payload: {
          error: "viewer identity is not enabled for this app — an administrator can enable it",
          code: "viewer_identity_disabled",
        },
      },
    ]);

    await assert.rejects(
      () => toft.who(headers),
      (err) => {
        assert.ok(err instanceof ToftError);
        assert.equal(err.code, "viewer_identity_disabled");
        assert.match(err.message, /administrator/);
        return true;
      },
    );
  });
});

describe("the email rule", () => {
  it("is null when the platform withheld it", async () => {
    route("GET /api/v1/app/viewer", [
      { status: 200, payload: { ...identity, email: undefined } },
    ]);
    const me = await toft.who(headers);
    assert.equal(me.email, null);
    // Everything else still answers in full.
    assert.equal(me.id, "u_1");
    assert.equal(me.role, "editor");
  });
});

describe("the cache", () => {
  it("resolves once per blob within the TTL", async () => {
    route("GET /api/v1/app/viewer", [{ status: 200, payload: identity }]);

    await toft.who(headers);
    await toft.who(headers);
    await toft.who(headers);

    assert.equal(requests.length, 1, "a per-render call became a request per render");
  });

  it("refresh: true goes back to the platform", async () => {
    route("GET /api/v1/app/viewer", [
      { status: 200, payload: identity },
      { status: 200, payload: { ...identity, role: "viewer" } },
    ]);

    assert.equal((await toft.who(headers)).role, "editor");
    const after = await toft.who(headers, { refresh: true });
    assert.equal(after.role, "viewer");
    assert.equal(requests.length, 2);
  });

  it("keeps two viewers apart", async () => {
    route("GET /api/v1/app/viewer", [
      { status: 200, payload: identity },
      { status: 200, payload: { ...identity, id: "u_2", name: "Sam" } },
    ]);

    const first = await toft.who({ [ACT_FOR_HEADER.toLowerCase()]: "blob-a" });
    const second = await toft.who({ [ACT_FOR_HEADER.toLowerCase()]: "blob-b" });
    assert.equal(first.id, "u_1");
    assert.equal(second.id, "u_2");
  });
});

describe("the two entrypoints", () => {
  // viewer() is *act as this person* and still throws on an absent header: a
  // silent downgrade would move a run out of somebody's name and into nobody's.
  it("viewer() still throws where who() returns null", async () => {
    assert.equal(await toft.who({}), null);
    assert.throws(() => toft.viewer({}), ToftError);
  });

  it("identity() on a Viewer is the same call", async () => {
    route("GET /api/v1/app/viewer", [{ status: 200, payload: identity }]);
    const me = toft.viewer(headers);
    assert.equal((await me.identity()).id, "u_1");
  });
});

/**
 * The 2026-11-04 black-box list, third round, Node's side: a Viewer prints the
 * person once identity resolved, and an inspect or JSON of it shows nothing
 * internal (BB-107); run.tail() refuses a poll interval that is not positive
 * (BB-108's sweep); a store call's not-a-string name is bad_request with its
 * own call in the hint (BB-109's sweep); a redirect is coded
 * unexpected_redirect with a hint (BB-113); accessToken(kind, {}) is refused
 * (BB-114).
 */

import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { createServer } from "node:http";
import { inspect } from "node:util";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import toft, { ACT_FOR_HEADER, AppTransport, JobRun, ToftError, resetIdentityCache, resetTransport } from "../index.js";

let saved;
let server;
let handler;
let requests;
before(async () => {
  server = createServer((req, res) => {
    requests.push(`${req.method} ${req.url}`);
    handler(req, res);
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
});
after(() => server.close());
beforeEach(() => {
  saved = { ...process.env };
  requests = [];
  handler = (req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end("{}");
  };
  resetTransport(new AppTransport({ baseUrl: `http://127.0.0.1:${server.address().port}`, token: "tofta_test" }));
  resetIdentityCache();
});
afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
  resetTransport();
  resetIdentityCache();
});

async function refused(call, code) {
  try {
    await call();
  } catch (err) {
    assert.ok(err instanceof ToftError, `not a ToftError: ${err}`);
    assert.equal(err.code, code, err.message);
    assert.equal(err.retryable, false);
    assert.ok(err.hint, "no hint");
    return err;
  }
  assert.fail("no refusal");
}

const key = "role-key";
function roleBlob(role) {
  const payload = `v1.${role}.${Buffer.from("app").toString("base64url")}.${Math.floor(Date.now() / 1000) + 120}`;
  const mac = createHmac("sha256", key)
    .update(Buffer.concat([Buffer.from("toft-role"), Buffer.from([0])]))
    .update(payload)
    .digest("base64url");
  return `${payload}.${mac}`;
}

describe("BB-107: a Viewer describes the person once identity is visible", () => {
  it("names who() resolved, and claims nothing before", async () => {
    process.env.TOFT_ROLE_KEY = key;
    handler = (req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ id: "u1", name: "Ada Lovelace", role: "editor" }));
    };
    const headers = { [ACT_FOR_HEADER]: "blob-107", "x-toft-role": roleBlob("editor") };
    const v = toft.viewer(headers);
    assert.doesNotMatch(String(v), /cannot see who/);
    assert.match(String(v), /a signed-in person/);
    assert.equal((await toft.who(headers)).name, "Ada Lovelace");
    assert.equal(String(v), "[Viewer: Ada Lovelace (editor)]");
  });

  it("an inspect or JSON of a Viewer shows no internals", () => {
    const v = toft.viewer({ [ACT_FOR_HEADER]: "secret-blob", "x-toft-request-deadline": "2099-01-01T00:00:00Z" });
    for (const text of [inspect(v), JSON.stringify(v), inspect(v.jobs), JSON.stringify(v.jobs), inspect(toft.jobs)]) {
      assert.doesNotMatch(text, /_requestDeadline|secret-blob/, text);
    }
  });
});

describe("BB-108's sweep: tail() refuses a poll interval that is not positive", () => {
  for (const ms of [0, -1, Number.NaN]) {
    it(`pollIntervalMs ${ms}`, async () => {
      await refused(async () => {
        for await (const _ of new JobRun({ id: "r1" }).tail({ pollIntervalMs: ms })) break;
      }, "bad_request");
      assert.deepEqual(requests, []);
    });
  }
});

describe("BB-109's sweep: a store name that is not a string", () => {
  beforeEach(() => {
    process.env.TOFT_STORAGE = JSON.stringify([{ name: "st", kind: "store", access: "full" }]);
  });
  for (const call of ["download", "remove", "url", "link"]) {
    for (const name of [null, 5]) {
      it(`${call}(${name}) is bad_request naming ${call}()`, async () => {
        const err = await refused(() => toft.store("st")[call](name), "bad_request");
        assert.match(err.hint, new RegExp(`^pass the name as a string: ${call}\\(`));
        assert.deepEqual(requests, []);
      });
    }
  }
});

describe("BB-113: a redirect is coded and hinted", () => {
  it("on data.get()", async () => {
    handler = (req, res) => {
      res.writeHead(302, { Location: "https://sign-in.example/" });
      res.end();
    };
    const err = await refused(() => toft.data.get(), "unexpected_redirect");
    assert.equal(err.status, 302);
    assert.match(err.hint, /TOFT_API_URL/);
  });
});

describe("BB-114: accessToken() refuses any options object, {} included", () => {
  for (const options of [{}, { bogus: 1 }]) {
    it(JSON.stringify(options), async () => {
      await refused(() => toft.accessToken("sharepoint", options), "bad_request");
      assert.throws(() => toft.azureCredential("sharepoint", options), (e) => e.code === "bad_request");
      assert.deepEqual(requests, []);
    });
  }
});

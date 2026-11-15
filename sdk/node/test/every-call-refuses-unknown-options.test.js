/**
 * The class guard for the skill's promise that every Node call refuses an
 * option it does not take: "any option a call does not take … and an options
 * object handed to a call that takes none … is `bad_request`", with a hint,
 * before any request is sent.
 *
 * Black-box runs kept finding calls that broke it one batch at a time (BB-066,
 * BB-078). This walks the SDK's public surface by reflection — the default
 * export, every named export, every namespace object on them, every exported
 * class's methods and statics, and the methods of the plain objects calls hand
 * back — and fails when it finds a callable the table below does not name. So a
 * new call cannot ship without being put under the rule, or exempted here with
 * the reason written down.
 *
 * Each entry calls its function with valid arguments plus `{ __notAnOption: 1 }`
 * — merged into the options object where the call takes one, appended where it
 * takes none — with `fetch` counting requests, and the environment of a
 * deployed app, so the refusal is the option's and not `off_platform`. A spy on
 * the reflected function proves the entry reached the call it is named after.
 */

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";

import * as sdk from "../index.js";

const toft = sdk.default;
const { JobRun, Run, ToftError } = sdk;

const JUNK = Object.freeze({ __notAnOption: 1 });
const junk = () => ({ ...JUNK });

// --- Discovery ---

// Where the walk stops: past these is the language's surface, not the SDK's.
const BUILTIN_PROTOTYPES = new Set([
  Object.prototype, Function.prototype, Error.prototype, Set.prototype, Map.prototype,
  Array.prototype, Promise.prototype,
]);
const FUNCTION_STATICS = new Set(["length", "name", "prototype"]);

const isClass = (fn) => /^class\b/.test(Function.prototype.toString.call(fn));

/**
 * Every callable reachable from the SDK's entry, as name → { fn, owner, key }.
 * The first name a function is found under wins, and the default export is
 * walked first, so `jobs.start` rather than `Jobs.start`.
 */
function discover(samples) {
  const found = new Map(); // name -> { fn, owner, key }
  const seen = new Map(); // fn -> name
  const walkedClasses = new Set();
  const walkedObjects = new Set();

  const record = (name, fn, owner, key) => {
    if (seen.has(fn)) return;
    assert.ok(!found.has(name), `two callables reflect to the name ${name}`);
    seen.set(fn, name);
    found.set(name, { fn, owner, key });
  };

  // Methods on an object and its prototype chain, up to a built-in prototype.
  const methods = (prefix, obj) => {
    for (let o = obj; o && !BUILTIN_PROTOTYPES.has(o); o = Object.getPrototypeOf(o)) {
      for (const key of Object.getOwnPropertyNames(o)) {
        if (key === "constructor") continue;
        const d = Object.getOwnPropertyDescriptor(o, key);
        // Accessors are properties, not calls; reading one can need the platform.
        if (!("value" in d) || typeof d.value !== "function") continue;
        if (isClass(d.value)) walkClass(d.value);
        else record(`${prefix}.${key}`, d.value, o, key);
      }
    }
  };

  const walkClass = (cls) => {
    if (walkedClasses.has(cls)) return;
    walkedClasses.add(cls);
    for (const key of Object.getOwnPropertyNames(cls)) {
      if (FUNCTION_STATICS.has(key)) continue;
      const d = Object.getOwnPropertyDescriptor(cls, key);
      if ("value" in d && typeof d.value === "function") record(`${cls.name}.${key}`, d.value, cls, key);
    }
    methods(cls.name, cls.prototype);
  };

  const walkObject = (name, obj, depth = 0) => {
    if (walkedObjects.has(obj) || depth > 2) return;
    walkedObjects.add(obj);
    methods(name, obj);
    for (const key of Object.keys(obj)) {
      const v = obj[key];
      if (!v || typeof v !== "object" || Array.isArray(v)) continue;
      // Into plain objects and SDK instances only: a Date's methods are the
      // language's, not the SDK's.
      const proto = Object.getPrototypeOf(v);
      if (proto === null || proto === Object.prototype || isClass(proto.constructor)) {
        walkObject(`${name}.${key}`, v, depth + 1);
      }
    }
  };

  const walkExport = (name, value, owner) => {
    if (typeof value === "function") {
      if (isClass(value)) walkClass(value);
      else record(name, value, owner, name);
    } else if (value && typeof value === "object") {
      walkObject(name, value);
    }
  };

  for (const key of Object.keys(toft).sort()) walkExport(key, toft[key], toft);
  for (const key of Object.keys(sdk).sort()) if (key !== "default") walkExport(key, sdk[key], null);
  // What calls hand back: instances of the classes above, walked for own
  // function properties, and the plain objects no class describes.
  for (const [name, value] of Object.entries(samples)) walkObject(name, value);
  return found;
}

// --- Fixtures ---

let requests = 0;
let realFetch;
let scratch;
const saved = { ...process.env };

const CANNED = JSON.stringify({
  id: "r1", send_id: "s1", status: "running", lines: [], runs: [], objects: [], prefixes: [],
  url: "http://store.invalid/o", method: "GET", headers: {},
});

const VIEWER_HEADERS = { "x-toft-act-for": "blob" };
const jobRun = () => new JobRun({ id: "r1", job: "report", status: "running" }, {});
const run = () => {
  return new Run({ baseUrl: "http://platform.invalid", token: "tofttj_test", id: "r1", outputUri: "http://store.invalid/out" });
};
const storeMount = () => toft.store("drop");
const volumeMount = () => toft.storage("vol");
const localFile = () => path.join(scratch, "nightly.txt");

before(() => {
  scratch = fs.mkdtempSync(path.join(os.tmpdir(), "toft-every-call-"));
  fs.mkdirSync(path.join(scratch, "vol"));
  fs.writeFileSync(path.join(scratch, "nightly.txt"), "rows");
  realFetch = globalThis.fetch;
  globalThis.fetch = async () => {
    requests++;
    return new Response(CANNED, { status: 200, headers: { "Content-Type": "application/json" } });
  };
});

after(() => {
  globalThis.fetch = realFetch;
  fs.rmSync(scratch, { recursive: true, force: true });
});

beforeEach(() => {
  // A deployed app's environment: the refusal must be the option's.
  Object.assign(process.env, {
    TOFT_API_URL: "http://platform.invalid",
    TOFT_APP_TOKEN: "tofta_test",
    TOFT_APP_ID: "app-1",
    TOFT_APP_SLUG: "reporter",
    TOFT_SCRATCH_DIR: path.join(scratch, "scratch"),
    TOFT_ROLE_KEY: "k",
    TOFT_STORAGE: JSON.stringify([
      { name: "drop", kind: "store", path: "", access: "full" },
      { name: "vol", kind: "volume", path: path.join(scratch, "vol"), access: "write" },
    ]),
  });
  sdk.resetTransport();
  sdk.resetIdentityCache();
  sdk.resetTokenCache();
  toft.app.refresh();
  requests = 0;
});

afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
});

// --- The table ---
//
// Keyed by the reflected name. A function is the call, given the junk object to
// merge or append; a string is an exemption, and the string is why.

const PROTOCOL = "a language protocol, called by JSON.stringify / String() with arguments the SDK does not choose";

const CALLS = {
  // The default export's functions.
  accessToken: (j) => toft.accessToken("sharepoint", j),
  azureCredential: (j) => toft.azureCredential("sharepoint", j),
  caller: (j) => toft.caller(VIEWER_HEADERS, j),
  mounts: (j) => toft.mounts(j),
  newSessionId: (j) => toft.newSessionId(j),
  scratchDir: (j) => toft.scratchDir("uploads", j),
  sessionScratchDir: (j) => toft.sessionScratchDir("sid", "uploads", j),
  storage: (j) => toft.storage("vol", j),
  store: (j) => toft.store("drop", j),
  viewer: (j) => toft.viewer(VIEWER_HEADERS, j),
  who: (j) => toft.who(VIEWER_HEADERS, j),

  // Named exports the default does not carry.
  respond: (j) => sdk.respond(jobRun(), { headers: {} }, j),
  transport: (j) => sdk.transport(j),
  resetTransport: (j) => sdk.resetTransport(undefined, j),
  resetIdentityCache: (j) => sdk.resetIdentityCache(j),
  resetTokenCache: (j) => sdk.resetTokenCache(j),

  // toft.app
  "app.refresh": (j) => toft.app.refresh(j),
  "app.name": (j) => toft.app.name(j),
  "app.mode": (j) => toft.app.mode(j),
  "app.basePath": (j) => toft.app.basePath(j),
  "app.policy": (j) => toft.app.policy(j),
  "app.capabilities": (j) => toft.app.capabilities(j),
  "app.can": (j) => toft.app.can("email", j),
  "app.url": (j) => toft.app.url("/a", j),

  // toft.data
  "data.get": (j) => toft.data.get(j),
  "data.read": (j) => toft.data.read(j),
  "data.set": (j) => toft.data.set({ period: "monthly" }, j),
  "data.update": (j) => toft.data.update((s) => s, j),
  "data.clear": (j) => toft.data.clear(j),

  // toft.jobs (and toft.viewer(h).jobs, the same functions)
  "jobs.start": (j) => toft.jobs.start("report", { params: {}, ...j }),
  "jobs.respond": (j) => toft.jobs.respond(jobRun(), { headers: {} }, j),
  "jobs.get": (j) => toft.jobs.get("r1", j),
  "jobs.list": (j) => toft.jobs.list({ job: "report", ...j }),
  "jobs.mine": (j) => toft.viewer(VIEWER_HEADERS).jobs.mine({ job: "report", ...j }),

  // toft.notify
  "notify.send": (j) => toft.notify.send("teams", "Nightly load complete", { title: "t", ...j }),
  "notify.resolve": (j) => toft.notify.resolve("pagerduty", "key-1", { reason: "fixed", ...j }),
  "notify.deliver": (j) => toft.notify.deliver("sharepoint", ["r.txt", "rows"], { name: "r.txt", ...j }),
  "notify.slack": (j) => toft.notify.slack("Nightly load complete", j),
  "notify.slackFile": (j) => toft.notify.slackFile(["r.txt", "rows"], { title: "t", ...j }),
  "notify.email": (j) => toft.notify.email({ to: "a@example.com", subject: "s", text: "t" }, j),
  "notify.status": (j) => toft.notify.status("s1", j),

  // RUN_STATUSES / TERMINAL_STATUSES are read-only Sets.
  "RUN_STATUSES.add": "the Set interface, read-only: it always throws TypeError, whatever it is given",
  "RUN_STATUSES.delete": "the Set interface, read-only: it always throws TypeError, whatever it is given",
  "RUN_STATUSES.clear": "the Set interface, read-only: it always throws TypeError, whatever it is given",

  // A run handle, from start/get/list/mine.
  "JobRun.toJSON": PROTOCOL,
  "JobRun.refresh": (j) => jobRun().refresh(j),
  "JobRun.wait": (j) => jobRun().wait({ timeoutMs: 1000, ...j }),
  "JobRun.output": (j) => jobRun().output(j),
  "JobRun.outputJson": (j) => jobRun().outputJson(j),
  "JobRun.logs": (j) => jobRun().logs(0, j),
  // An async generator: its body, takeOptions included, runs on the first next().
  "JobRun.tail": (j) => jobRun().tail({ pollIntervalMs: 1, ...j }).next(),
  "JobRun.cancel": (j) => jobRun().cancel(j),

  // toft.run, the job's view of itself.
  "Run.fromEnv": (j) => Run.fromEnv(j),
  "Run.toJSON": PROTOCOL,
  "Run.progress": (j) => run().progress(40, "reading ledgers", j),
  "Run.log": (j) => run().log("12,044 rows", { error: false, ...j }),
  "Run.writeOutput": (j) => run().writeOutput("rows", j),
  "Run.writeOutputJson": (j) => run().writeOutputJson({ rows: 1 }, j),

  // A storage mount, from storage()/store()/mounts().
  "Mount.toJSON": PROTOCOL,
  "Mount.put": (j) => volumeMount().put(localFile(), "landed.txt", j),
  "Mount.list": (j) => storeMount().list("reports/", { cursor: "", ...j }),
  "Mount.upload": (j) => storeMount().upload(localFile(), "landed.txt", j),
  "Mount.download": (j) => storeMount().download("landed.txt", j),
  "Mount.url": (j) => storeMount().url("landed.txt", { attachment: true, ...j }),
  "Mount.remove": (j) => storeMount().remove("landed.txt", j),
  "Mount.link": (j) => storeMount().link("landed.txt", j),

  // toft.viewer(h) and toft.caller(h).
  "Viewer.identity": (j) => toft.viewer(VIEWER_HEADERS).identity({ refresh: true, ...j }),
  "Viewer.toString": PROTOCOL,
  "Viewer.toJSON": PROTOCOL,
  "Caller.atLeast": (j) => toft.caller(VIEWER_HEADERS).atLeast(toft.ROLE_EDITOR, j),
  "Caller.toString": PROTOCOL,

  // accessToken()'s answer.
  "Token.toJSON": PROTOCOL,

  // azureCredential()'s answer.
  "azureCredential().getToken":
    "@azure/identity's TokenCredential interface: Microsoft's libraries call getToken(scopes, options) " +
    "with their own GetTokenOptions, which refusing would break",

  // The low-level transport, exported as a test seam and documented nowhere
  // as an app call: its arguments are a route, a request body and a header
  // map, all free-form by nature.
  "AppTransport.get": "low-level HTTP: (path, headers) — a free-form header map",
  "AppTransport.putJson": "low-level HTTP: (path, body, { headers }) — a free-form body and header map",
  "AppTransport.del": "low-level HTTP: (path, headers) — a free-form header map",
  "AppTransport.postJson": "low-level HTTP: (path, body, { idempotencyKey, headers }) — a free-form body",
  "AppTransport.postMultipart": "low-level HTTP: (path, fields, parts, …) — free-form form fields",
  "AppTransport.follow": "low-level HTTP: (path, headers) — a free-form header map",
};

// --- The guard ---

function surface() {
  return discover({
    // Instances of every class a call returns, walked for own function
    // properties; and the plain objects no exported class describes.
    "JobRun()": jobRun(),
    "Run()": run(),
    "Mount()": volumeMount(),
    "Viewer()": toft.viewer(VIEWER_HEADERS),
    "Caller()": toft.caller(VIEWER_HEADERS),
    "Document()": new sdk.Document({}),
    "Send()": new sdk.Send({}),
    "Token()": new sdk.Token({}),
    "azureCredential()": toft.azureCredential("sharepoint"),
  });
}

describe("every call refuses an option it does not take", () => {
  it("the table names every callable reflection finds, and nothing else", () => {
    const found = surface();
    const missing = [...found.keys()].filter((n) => !(n in CALLS));
    assert.deepEqual(missing, [],
      `callables with no entry — give each valid arguments in CALLS, or an exemption saying why: ${missing.join(", ")}`);
    const stale = Object.keys(CALLS).filter((n) => !found.has(n));
    assert.deepEqual(stale, [], `entries for callables reflection no longer finds: ${stale.join(", ")}`);
  });

  it("covers a plausible surface", () => {
    // A walk that silently found nothing would pass everything above.
    const entries = Object.values(CALLS);
    assert.ok(entries.filter((e) => typeof e === "function").length >= 60);
  });

  for (const [name, entry] of Object.entries(CALLS)) {
    if (typeof entry === "string") continue;
    it(`${name} refuses { __notAnOption: 1 } as bad_request, before any request`, async () => {
      const target = surface().get(name);
      assert.ok(target, `${name} is not on the surface`);

      // Prove the entry reaches the call it is named after, where the owner
      // can be patched (an ES module namespace cannot).
      let reached = target.owner === null;
      if (target.owner) {
        const original = target.owner[target.key];
        target.owner[target.key] = function (...args) {
          reached = true;
          return original.apply(this, args);
        };
        try {
          await check(name, entry);
        } finally {
          target.owner[target.key] = original;
        }
      } else {
        await check(name, entry);
      }
      assert.ok(reached, `${name}'s entry never called ${name}`);
    });
  }
});

async function check(name, entry) {
  requests = 0;
  let outcome;
  try {
    outcome = { value: await entry(junk()) };
  } catch (err) {
    outcome = { err };
  }
  assert.equal(requests, 0, `${name} sent ${requests} request(s) before refusing`);
  assert.ok(outcome.err, `${name} took { __notAnOption: 1 } without a word`);
  const { err } = outcome;
  assert.ok(err instanceof ToftError, `${name} threw ${err?.name}: ${err?.message}, not a ToftError`);
  assert.equal(err.code, "bad_request", `${name}: ${err.message}`);
  assert.equal(err.retryable, false, `${name}'s refusal is retryable`);
  assert.ok(typeof err.hint === "string" && err.hint.trim(), `${name}'s refusal has no hint`);
  assert.match(err.message, /__notAnOption/, `${name}'s refusal does not name the option: ${err.message}`);
}

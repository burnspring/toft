/**
 * Contract tests for the job surface, against a fake platform.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * These are the same scenarios `sdk/python/tests/test_app_sdk.py` runs — same
 * routes, same headers, same reported deduplication, same typed refusal — so
 * that an app written in one language cannot observe a platform the other
 * cannot. When one side gains a scenario, the other gains it too; that is the
 * whole reason the table is duplicated rather than shared.
 */

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, beforeEach, describe, it } from "node:test";

import toft, {
  AppTransport,
  JobRun,
  ToftError,
  QueueFull,
  resetTransport,
  respond,
} from "../index.js";

const ACT_FOR = "signed-blob-for-priya";

let server;
let base;
let routes = {};
let requests = [];

/** Script one route: `"POST /api/v1/app/jobs/x/runs"` → status, body, headers. */
function route(key, status, payload, headers) {
  routes[key] = { status, payload, headers };
}

const last = () => requests[requests.length - 1];

before(async () => {
  server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks).toString();
      requests.push({ method: req.method, path: req.url, headers: req.headers, body });

      let entry = routes[`${req.method} ${req.url.split("?")[0]}`];
      // A scripted route answers its entries in order, the last one forever.
      if (Array.isArray(entry)) entry = entry.length > 1 ? entry.shift() : entry[0];
      if (!entry) {
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end('{"error":"no such route"}');
        return;
      }
      res.writeHead(entry.status, {
        "Content-Type": "application/json",
        ...(entry.headers ?? {}),
      });
      // A Buffer payload is sent as is, for an object that is not JSON.
      res.end(Buffer.isBuffer(entry.payload) ? entry.payload : JSON.stringify(entry.payload ?? {}));
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

const me = () => toft.viewer({ "x-toft-act-for": ACT_FOR });

describe("starting a job", () => {
  it("the app starts a run as itself, with no viewer attached", async () => {
    route("POST /api/v1/app/jobs/reconcile/runs", 202, {
      id: "run_1",
      status: "queued",
      job: "reconcile",
      started_by: "app",
      queue_position: 2,
      params: { month: "2026-03" },
      status_url: "/apps/books/__toft/runs/run_1",
    });

    const run = await toft.jobs.start("reconcile", { params: { month: "2026-03" } });

    assert.equal(run.id, "run_1");
    assert.equal(run.job, "reconcile");
    assert.equal(run.queuePosition, 2);
    assert.deepEqual(run.params, { month: "2026-03" });
    assert.equal(run.statusUrl, "/apps/books/__toft/runs/run_1");
    assert.equal(run.done, false);
    assert.equal(run.startedByViewer, false);
    // No act-for header: the app is acting as itself, deliberately, and nothing
    // about that call may look like a person did it.
    assert.equal(last().headers["x-toft-act-for"], undefined);
    assert.deepEqual(JSON.parse(last().body), { params: { month: "2026-03" } });
  });

  it("a viewer start relays the header verbatim", async () => {
    route("POST /api/v1/app/jobs/reconcile/runs", 202, {
      id: "run_2",
      status: "queued",
      started_by: "viewer",
    });

    const run = await me().jobs.start("reconcile", { params: { month: "2026-03" } });

    assert.equal(run.startedByViewer, true);
    assert.equal(last().headers["x-toft-act-for"], ACT_FOR);
    // The contract version rides on every app call, as it does everywhere else
    // on this surface.
    assert.equal(last().headers["x-toft-app-contract"], "1");
  });

  it("a viewer without the header throws rather than downgrading", () => {
    // A silent fall back to the app acting as itself would move a run out of a
    // person's name and into nobody's, and nobody would notice until an audit.
    assert.throws(
      () => toft.viewer({ "x-toft-session": "abc" }),
      (err) => err instanceof ToftError && /X-Toft-Act-For/.test(err.message),
    );
    assert.equal(requests.length, 0);
  });

  it("a deduplicated start is reported, never silent", async () => {
    route("POST /api/v1/app/jobs/reconcile/runs", 200, {
      id: "run_1",
      status: "running",
      deduplicated: true,
      progress: 40,
    });

    const run = await me().jobs.start("reconcile");

    assert.equal(run.deduplicated, true);
    assert.equal(run.id, "run_1");
    assert.equal(run.progress, 40);
  });

  it("an explicit idempotency key is sent", async () => {
    route("POST /api/v1/app/jobs/reconcile/runs", 202, { id: "run_3" });
    await me().jobs.start("reconcile", { idempotencyKey: "mine" });
    assert.equal(last().headers["idempotency-key"], "mine");
  });

  it("a full queue is a typed refusal carrying the numbers", async () => {
    route(
      "POST /api/v1/app/jobs/reconcile/runs",
      429,
      {
        error: "too many runs already in flight",
        code: "queue_full",
        in_flight: 3,
        limit: 3,
        retryable: true,
      },
      { "Retry-After": "0" },
    );

    await assert.rejects(
      () => toft.jobs.start("reconcile"),
      (err) => {
        assert.ok(err instanceof QueueFull);
        assert.equal(err.inFlight, 3);
        assert.equal(err.limit, 3);
        assert.equal(err.retryable, true);
        return true;
      },
    );
  });
});

describe("listing runs", () => {
  it("mine scopes to the viewer and needs one", async () => {
    route("GET /api/v1/app/job-runs", 200, {
      runs: [{ id: "run_1", status: "succeeded", started_by: "viewer" }],
    });

    const runs = await me().jobs.mine({ job: "reconcile", limit: 20 });

    assert.deepEqual(runs.map((r) => r.id), ["run_1"]);
    assert.equal(runs[0].succeeded, true);
    assert.match(last().path, /scope=viewer/);
    assert.match(last().path, /job=reconcile/);
    assert.match(last().path, /limit=20/);
    assert.equal(last().headers["x-toft-act-for"], ACT_FOR);

    // The app acting as itself has no "mine" to ask about, and answering with
    // everything would be a silent widening rather than an answer.
    const before = requests.length;
    await assert.rejects(() => toft.jobs.mine(), (err) => {
      // A refusal no retry can change says so, with a code to branch on.
      assert.ok(err instanceof ToftError);
      assert.equal(err.code, "no_viewer");
      assert.equal(err.retryable, false);
      return true;
    });
    assert.equal(requests.length, before);
  });

  it("mine refuses the old (job, { limit }) shape rather than dropping the job", async () => {
    await assert.rejects(() => me().jobs.mine("reconcile", { limit: 20 }), (err) => {
      assert.equal(err.code, "bad_request");
      return true;
    });
    assert.equal(requests.length, 0);
  });

  it("list asks for the app scope", async () => {
    route("GET /api/v1/app/job-runs", 200, { runs: [] });
    assert.deepEqual(await toft.jobs.list(), []);
    assert.match(last().path, /scope=app/);
    assert.doesNotMatch(last().path, /status=/);
  });

  it("list filters on job and status", async () => {
    route("GET /api/v1/app/job-runs", 200, {
      runs: [{ id: "run_9", job: "nightly", status: "failed", failure_reason: "exit 3: connection refused" }],
    });
    const runs = await toft.jobs.list({ job: "nightly", status: "failed", limit: 5 });
    for (const part of [/job=nightly/, /status=failed/, /limit=5/]) assert.match(last().path, part);
    assert.equal(runs[0].failureReason, "exit 3: connection refused");
  });
});

describe("a run's state", () => {
  it("status words match the platform's", () => {
    const cases = {
      succeeded: ["done", "succeeded"],
      failed: ["done"],
      timed_out: ["done", "outOfResources"],
      out_of_memory: ["done", "outOfResources"],
      cancelled: ["done"],
      skipped: ["done"],
      running: [],
      queued: [],
    };
    for (const [status, expected] of Object.entries(cases)) {
      const run = new JobRun({ id: "r", status });
      assert.equal(run.done, expected.includes("done"), status);
      assert.equal(run.succeeded, expected.includes("succeeded"), status);
      assert.equal(run.outOfResources, expected.includes("outOfResources"), status);
    }
  });

  it("cancel and logs go to the run, carrying the viewer", async () => {
    route("POST /api/v1/app/job-runs/run_1/cancel", 204, {});
    route("GET /api/v1/app/job-runs/run_1", 200, { id: "run_1", status: "cancelled" });
    route("GET /api/v1/app/job-runs/run_1/logs", 200, {
      lines: [{ seq: 1, line: "reconciling" }],
      status: "running",
    });

    const run = new JobRun({ id: "run_1" }, { "X-Toft-Act-For": ACT_FOR });
    const lines = await run.logs();
    assert.equal(lines[0].line, "reconciling");
    assert.match(last().path, /after=0/);

    const after = await run.cancel();
    // The run read again after the cancel (DOC-6), not undefined.
    assert.equal(after.status, "cancelled");
    // The viewer rides along, because who may cancel a run is decided by who
    // started it.
    assert.equal(last().headers["x-toft-act-for"], ACT_FOR);
    assert.ok(requests.some((r) => r.method === "POST" && r.path.endsWith("/cancel") &&
      r.headers["x-toft-act-for"] === ACT_FOR));
  });
});

describe("a run's output", () => {
  it("is null when the run produced none (BB-019)", async () => {
    // Null, not an empty buffer: an output written empty is a different fact
    // from no output at all, and a failed run that wrote nothing must not read
    // as an empty report.
    route("GET /api/v1/app/job-runs/run_1/output", 404, {
      error: "this run produced no output object",
      code: "no_output",
    });
    assert.equal(await new JobRun({ id: "run_1" }).output(), null);
    assert.equal(await new JobRun({ id: "run_1" }).outputJson(), null);
  });

  it("is an empty array, not null, when the run wrote an empty output", async () => {
    route("GET /api/v1/app/job-runs/run_1/output", 302, {}, { Location: `${base}/empty-object` });
    route("GET /empty-object", 200, Buffer.alloc(0));
    const bytes = await new JobRun({ id: "run_1" }).output();
    assert.ok(bytes instanceof Uint8Array);
    assert.equal(bytes.length, 0);
  });

  it("throws on any other 404, such as a run that is not there", async () => {
    route("GET /api/v1/app/job-runs/run_1/output", 404, { error: "run not found", code: "not_found" });
    await assert.rejects(() => new JobRun({ id: "run_1" }).output(), (err) => err.code === "not_found");
  });

  it("outputJson on bytes that are not JSON is not_json, never a raw SyntaxError (BB-020)", async () => {
    route("GET /api/v1/app/job-runs/run_1/output", 302, {}, { Location: `${base}/text-object` });
    route("GET /text-object", 200, Buffer.from("plain text, not json"));
    await assert.rejects(
      () => new JobRun({ id: "run_1" }).outputJson(),
      (err) => err instanceof ToftError && err.code === "not_json" && err.retryable === false,
    );
  });

  it("throws when the artifact existed and expired", async () => {
    // Distinct from "produced none": a run still reading succeeded with nothing
    // behind it is the failure this distinction exists to prevent, and it must
    // not read as an empty report.
    route("GET /api/v1/app/job-runs/run_1/output", 410, {
      error: "this run's output has expired",
      code: "output_expired",
    });
    await assert.rejects(
      () => new JobRun({ id: "run_1" }).output(),
      (err) => err instanceof ToftError && err.code === "output_expired",
    );
  });

  it("follows the redirect itself, without replaying the app token", async () => {
    // fetch would carry the Authorization header to whatever host the object
    // store lives on. The second request must be bare — a presigned URL is
    // already the whole authorization.
    route("GET /api/v1/app/job-runs/run_1/output", 302, {}, { Location: `${base}/signed-object` });
    route("GET /signed-object", 200, { rows: 1204 });

    const bytes = await new JobRun({ id: "run_1" }).output();
    assert.deepEqual(JSON.parse(Buffer.from(bytes).toString()), { rows: 1204 });
    assert.deepEqual(await new JobRun({ id: "run_1" }).outputJson(), { rows: 1204 });

    const objectRequest = requests[requests.length - 1];
    assert.equal(objectRequest.path, "/signed-object");
    assert.equal(objectRequest.headers.authorization, undefined);
    assert.equal(objectRequest.headers["x-toft-app-contract"], undefined);
  });
});

describe("starting with params alone", () => {
  it("refuses a payload rather than dropping it", async () => {
    await assert.rejects(
      () => toft.jobs.start("reconcile", { payload: { rows: [1] } }),
      (err) => err.code === "bad_request" && /payload/.test(err.message),
    );
    assert.equal(requests.length, 0);
  });

  it("carries every contract field", () => {
    const run = new JobRun({
      id: "r", job: "j", status: "out_of_memory", started_by: "schedule", rerun_of: "r0",
      params: { a: 1 }, deadline: "d", progress: 80, progress_message: "writing",
      peak_memory_mb: 1024, failure_reason: "stopped at 1024 MB",
      status_url: "/apps/a/__toft/runs/r", logs_url: "/api/v1/app/job-runs/r/logs",
    });
    assert.deepEqual(
      [run.rerunOf, run.deadline, run.progressMessage, run.peakMemoryMb, run.logsUrl],
      ["r0", "d", "writing", 1024, "/api/v1/app/job-runs/r/logs"],
    );
    assert.equal(run.done, true);
    assert.equal(run.outOfResources, true);
    // The viewer's blob is a credential, and a logged run must not carry it.
    const viewerRun = new JobRun({ id: "r" }, { "X-Toft-Act-For": ACT_FOR });
    assert.doesNotMatch(JSON.stringify(viewerRun), new RegExp(ACT_FOR));
  });
});

describe("no retries", () => {
  it("a failed run is done, and refresh reads only itself", async () => {
    // A field an older platform might still send must not make a failed run
    // read as unfinished, nor send refresh() chasing it.
    route("GET /api/v1/app/job-runs/run_1", 200, { id: "run_1", status: "failed", retried_by: "run_2" });
    route("GET /api/v1/app/job-runs/run_2", 200, { id: "run_2", status: "succeeded" });

    const first = new JobRun({ id: "run_1", status: "failed", retried_by: "run_2" });
    assert.equal(first.done, true);
    const again = await first.refresh();
    assert.equal(again.id, "run_1");
    assert.deepEqual(requests.map((r) => r.path), ["/api/v1/app/job-runs/run_1"]);
  });

  it("wait resolves with the run in its final state", async () => {
    route("GET /api/v1/app/job-runs/run_1", 200, { id: "run_1", status: "succeeded", has_output: true });
    const final = await new JobRun({ id: "run_1" }).wait({ pollIntervalMs: 1 });
    assert.equal(final.succeeded, true);
    assert.equal(final.hasOutput, true);
  });

  it("wait waits out the rate limit rather than rejecting", async () => {
    // A busy app's other calls spend the same limiter; a poll it refuses is
    // not the run failing (jobs-regression #4).
    const limited = { status: 429, payload: { code: "rate_limited", retryable: true, error: "slow down" },
      headers: { "Retry-After": "0" } };
    routes["GET /api/v1/app/job-runs/run_1"] = [
      { status: 200, payload: { id: "run_1", status: "running" } },
      limited, limited, limited,
      { status: 200, payload: { id: "run_1", status: "succeeded" } },
    ];
    const final = await new JobRun({ id: "run_1" }).wait({ pollIntervalMs: 1 });
    assert.equal(final.succeeded, true);
  });

  it("names a wait that ran out, so a caller can branch on it", async () => {
    route("GET /api/v1/app/job-runs/run_1", 200, { id: "run_1", status: "running" });
    await assert.rejects(
      () => new JobRun({ id: "run_1" }).wait({ timeoutMs: 0, pollIntervalMs: 1 }),
      (err) => err instanceof ToftError && err.code === "wait_timeout" && err.retryable === true,
    );
  });
});

describe("respond(run)", () => {
  const RUN = { id: "run_1", job: "nightly", status: "queued", status_url: "/apps/books/__toft/runs/run_1" };

  it("answers a script 202, Location and the run", async () => {
    const r = toft.jobs.respond(new JobRun(RUN), { headers: { accept: "application/json" } });
    assert.ok(r instanceof Response);
    assert.equal(r.status, 202);
    assert.equal(r.headers.get("location"), RUN.status_url);
    assert.equal(r.headers.get("content-type"), "application/json");
    // The three empty fields are present as null, as every SDK documents (BB-030).
    assert.deepEqual(await r.json(), { ...RUN, rerun_of: null, scheduled_for: null, failure_reason: null });
  });

  it("refuses a job name in place of the run (BB-067)", () => {
    assert.throws(() => respond("nightly"), (err) => {
      assert.equal(err.code, "bad_request");
      assert.equal(err.retryable, false);
      assert.match(err.hint, /jobs\.start/);
      return true;
    });
  });

  it("answers with JSON when there is no request to read", () => {
    assert.equal(respond(new JobRun(RUN)).status, 202);
  });

  it("sends a browser 303 to the status page — Fetch Request", () => {
    const req = new Request("http://app.local/reports", {
      method: "POST",
      headers: { Accept: "text/html,application/xhtml+xml,*/*;q=0.8" },
    });
    const r = toft.jobs.respond(new JobRun(RUN), req);
    assert.equal(r.status, 303);
    assert.equal(r.headers.get("location"), RUN.status_url);
  });

  it("sends a browser 303 to the status page — Node IncomingMessage", async () => {
    // A real IncomingMessage, so the lowercased-headers object is the one
    // Express hands an app.
    const got = await new Promise((resolve) => {
      const s = createServer((req, res) => {
        resolve(toft.jobs.respond(new JobRun(RUN), req));
        res.end();
      });
      s.listen(0, "127.0.0.1", async () => {
        await fetch(`http://127.0.0.1:${s.address().port}/`, { headers: { Accept: "text/html" } });
        s.close();
      });
    });
    assert.equal(got.status, 303);
  });
});

describe("wait() inside a request", () => {
  const deadlineIn = (s) => new Date(Date.now() + s * 1000).toISOString();

  async function started(headers) {
    route("POST /api/v1/app/jobs/nightly/runs", 202, { id: "run_1", status: "queued" });
    route("GET /api/v1/app/job-runs/run_1", 200, { id: "run_1", status: "succeeded" });
    return toft.viewer(headers).jobs.start("nightly");
  }

  async function outlives(fn) {
    const before = requests.length;
    await assert.rejects(fn, (err) => {
      assert.equal(err.code, "would_outlive_request");
      assert.equal(err.retryable, false);
      assert.equal(err.hint, "return toft.jobs.respond(run, req)");
      return true;
    });
    // Refused at once: not one poll was spent finding out.
    assert.equal(requests.length, before);
  }

  it("refuses a viewer run with no timeout", async () => {
    const run = await started({ "x-toft-act-for": ACT_FOR, "x-toft-request-deadline": deadlineIn(300) });
    await outlives(() => run.wait());
  });

  it("refuses a timeout past the deadline", async () => {
    const run = await started({ "x-toft-act-for": ACT_FOR, "x-toft-request-deadline": deadlineIn(30) });
    await outlives(() => run.wait({ timeoutMs: 60_000 }));
  });

  it("allows a timeout that fits, and the refreshed run still knows the request", async () => {
    const run = await started({ "x-toft-act-for": ACT_FOR, "x-toft-request-deadline": deadlineIn(300) });
    assert.equal((await run.wait({ timeoutMs: 10_000, pollIntervalMs: 1 })).succeeded, true);
    // Still going on this read, so the wait would block — and is refused.
    route("GET /api/v1/app/job-runs/run_1", 200, { id: "run_1", status: "running" });
    const again = await run.refresh();
    await outlives(() => again.wait());
  });

  it("returns a finished run at once, with no timeout (BB-072)", async () => {
    const run = await started({ "x-toft-act-for": ACT_FOR, "x-toft-request-deadline": deadlineIn(300) });
    const done = await run.refresh();
    const before = requests.length;
    assert.equal((await done.wait()).succeeded, true);
    assert.equal(requests.length, before);
  });

  it("waits as before with no deadline", async () => {
    const run = await started({ "x-toft-act-for": ACT_FOR });
    assert.equal((await run.wait({ pollIntervalMs: 1 })).succeeded, true);
  });

  it("is guarded by explicit headers on an app run", async () => {
    const run = new JobRun({ id: "run_1" });
    await outlives(() => run.wait({ headers: new Headers({ "X-Toft-Request-Deadline": deadlineIn(300) }) }));
  });

  it("ignores a WebSocket handshake's deadline", async () => {
    const run = await started({
      "x-toft-act-for": ACT_FOR,
      upgrade: "websocket",
      "x-toft-request-deadline": deadlineIn(-3600),
    });
    assert.equal((await run.wait({ timeoutMs: 5000, pollIntervalMs: 1 })).succeeded, true);
  });
});

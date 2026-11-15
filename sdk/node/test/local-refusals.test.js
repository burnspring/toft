/**
 * A refusal the SDK makes before any request is coded and never retryable.
 * Without a body the status rule reads a statusless error as a network
 * failure — code "" and retryable true — which an agent's loop retries forever.
 */

import assert from "node:assert/strict";
import { describe, it } from "node:test";

import toft, { JobRun, ToftError } from "../index.js";

async function refused(code, fn) {
  await assert.rejects(
    async () => fn(),
    (err) => err instanceof ToftError && err.code === code && err.retryable === false,
  );
}

describe("refusals the SDK makes itself", () => {
  it("viewer() with no header is no_viewer", () => refused("no_viewer", () => toft.viewer({})));
  it("an email with no recipient is no_recipient", () =>
    refused("no_recipient", () => toft.notify.email({ to: [], subject: "s", text: "t" })));
  it("status() with no id is bad_request", () => refused("bad_request", () => toft.notify.status("")));
  it("data.set of an array is bad_request", () => refused("bad_request", () => toft.data.set([1])));
  it("sessionScratchDir with no id is bad_request", () =>
    refused("bad_request", () => toft.sessionScratchDir("")));
  it("no_viewer says how to act as the app", () =>
    assert.throws(() => toft.viewer({}), (err) => err.hint === "use toft.jobs.start(...) to start it as the app, or toft.jobs.list(...) for its runs"));
  it("no_viewer does not say only a signed-in session carries the header", () =>
    assert.throws(
      () => toft.viewer({}),
      (err) =>
        err.message.includes("anonymous visitor") &&
        err.message.includes("viewer_anonymous") &&
        !err.message.includes("only for a signed-in"),
    ));
  it("wait() past the request's deadline is would_outlive_request", () =>
    refused("would_outlive_request", () =>
      new JobRun({ id: "r" }).wait({ headers: { "x-toft-request-deadline": "2000-01-01T00:00:00Z" } })));
});

/**
 * What the SDK says when the environment it needs is not there.
 *
 * The same two sentences `sdk/python/tests/test_app_sdk.py` asserts in
 * `TestInsideAJob`, because one platform rule must not be explained two ways.
 */

import assert from "node:assert/strict";
import { beforeEach, describe, it } from "node:test";

import { ToftError, resetTransport } from "../index.js";
import toft from "../index.js";

beforeEach(() => {
  resetTransport(null);
  delete process.env.TOFT_APP_TOKEN;
  delete process.env.TOFT_RUN_ID;
  delete process.env.TOFT_JOB_NAME;
  process.env.TOFT_API_URL = "http://platform.invalid";
});

describe("inside a job", () => {
  // A job is on the platform and is deliberately given no app token. The
  // generic refusal told an author whose job had just done its real work that
  // they were not running on the platform, which sent them looking for a broken
  // deploy that was not there.
  it("names the run and the two ways round it", async () => {
    process.env.TOFT_RUN_ID = "80ced99a";
    process.env.TOFT_JOB_NAME = "archive";

    const err = await toft.data.get().then(
      () => null,
      (e) => e,
    );
    assert.ok(err instanceof ToftError, `expected a ToftError, got ${err}`);
    assert.match(err.message, /archive/);
    assert.match(err.message, /80ced99a/);
    assert.match(err.message, /deliberately/);
    assert.match(err.message, /writeOutputJson/);
    assert.doesNotMatch(err.message, /only works in an app running on the platform/);
  });

  it("leaves the off-platform sentence alone outside a run", async () => {
    const err = await toft.data.get().then(
      () => null,
      (e) => e,
    );
    assert.match(err.message, /only works in an app running on the platform/);
  });
});

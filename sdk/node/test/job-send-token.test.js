import { test } from "node:test";
import assert from "node:assert/strict";
import { AppTransport } from "../transport.js";

// Inside a job, which holds no app token, the transport sends with the run's
// own credential (ADR §108); the app's token wins wherever there is one.
test("a job sends with its run token", () => {
  const saved = { ...process.env };
  try {
    delete process.env.TOFT_APP_TOKEN;
    process.env.TOFT_RUN_ID = "run-1";
    process.env.TOFT_JOB_TOKEN = "jwt";
    assert.equal(new AppTransport().token, "jwt");
    process.env.TOFT_APP_TOKEN = "tofta_x";
    assert.equal(new AppTransport().token, "tofta_x");
  } finally {
    process.env = saved;
  }
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { Run } from "../index.js";

// A job's own calls read a refusal the way the app's do: the platform's code,
// retryable and hint survive, rather than a status and a sentence.
test("a run's refusal carries the envelope", async () => {
  const server = createServer((req, res) => {
    res.writeHead(409, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "skew", code: "contract_skew", retryable: false, hint: "upgrade the SDK" }));
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  try {
    const run = new Run({ baseUrl: `http://127.0.0.1:${server.address().port}`, token: "t", id: "r" });
    await assert.rejects(run.progress(50), (err) => {
      assert.equal(err.code, "contract_skew");
      assert.equal(err.hint, "upgrade the SDK");
      assert.equal(err.retryable, false);
      return true;
    });
  } finally {
    server.close();
  }
});

/**
 * Per-session scratch, and the anonymous viewer.
 *
 * `node --test` from `sdk/node`, with nothing installed.
 *
 * These are the same scenarios `sdk/python/tests/test_app_sdk.py` runs in
 * `TestSessionScratch`. The failure they guard is a data leak rather than a
 * crash: an app that answers a missing `X-Toft-Session` with a constant
 * puts every anonymous viewer of a public app in one directory, where one
 * viewer's upload overwrites another's. `newSessionId()` is the answer, so
 * what matters is that two mints never meet.
 */

import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";

import toft from "../index.js";

let root;

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "toft-scratch-"));
  process.env.TOFT_SCRATCH_DIR = root;
});

afterEach(() => {
  delete process.env.TOFT_SCRATCH_DIR;
  rmSync(root, { recursive: true, force: true });
});

describe("session scratch", () => {
  it("mints ids that are distinct and wide", () => {
    const ids = new Set();
    for (let i = 0; i < 500; i++) ids.add(toft.newSessionId());
    assert.equal(ids.size, 500);
    // Guessable ids are as bad as a shared one: the directory name is a hash
    // of this string, so guessing it names somebody else's directory.
    for (const id of ids) assert.ok(id.length >= 20);
  });

  it("keeps two anonymous viewers apart", () => {
    const first = toft.sessionScratchDir(toft.newSessionId());
    const second = toft.sessionScratchDir(toft.newSessionId());
    assert.notEqual(first, second);
    assert.ok(statSync(first).isDirectory());
    assert.ok(statSync(second).isDirectory());
  });

  it("returns one viewer to their own directory when they carry the id back", () => {
    const sid = toft.newSessionId();
    writeFileSync(join(toft.sessionScratchDir(sid), "upload.csv"), "a,b\n");
    assert.equal(readFileSync(join(toft.sessionScratchDir(sid), "upload.csv"), "utf8"), "a,b\n");
  });

  it("refuses an empty id rather than collapsing every session into one", () => {
    assert.throws(() => toft.sessionScratchDir(""), /non-empty session id/);
  });

  it("cannot be walked out of by a hostile id", () => {
    assert.equal(dirname(toft.sessionScratchDir("../../etc")), root);
  });
});

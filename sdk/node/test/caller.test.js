import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";

import {
  Caller,
  ENV_ROLE_KEY,
  ROLE_ANONYMOUS,
  ROLE_APP,
  ROLE_EDITOR,
  ROLE_HEADER,
  ROLE_OWNER,
  ROLE_SYSTEM,
  ROLE_UNKNOWN,
  ROLE_VIEWER,
  caller,
  verifyRole,
} from "../caller.js";

/**
 * The platform's mint, spelled here so this suite needs nothing from the Go
 * side. `sdk/rolevector_test.go` is what proves the two spellings agree against
 * a committed vector; this only needs *a* valid blob.
 */
function mint(key, role, expirySec) {
  const appId = Buffer.from("app-a").toString("base64url");
  const payload = `v1.${role}.${appId}.${expirySec}`;
  const mac = createHmac("sha256", key)
    .update(Buffer.concat([Buffer.from("toft-role"), Buffer.from([0])]))
    .update(payload)
    .digest("base64url");
  return `${payload}.${mac}`;
}

const KEY = "test-role-key";
const soon = () => Math.floor(Date.now() / 1000) + 60;

// The ladder is people only, and everything it cannot place is false.
//
// The temptation this refuses is ranking `system` and `app` somewhere: they are
// not more or less authorized than a person, they are a different kind of
// caller, and putting Toft's scheduler above an owner would be a guess the
// SDK has no business making on an app's behalf.
test("atLeast answers only about people", () => {
  const ladder = [ROLE_ANONYMOUS, ROLE_VIEWER, ROLE_EDITOR, ROLE_OWNER];
  for (const [i, role] of ladder.entries()) {
    for (const [j, want] of ladder.entries()) {
      assert.equal(
        new Caller(role).atLeast(want),
        i >= j,
        `${role}.atLeast(${want})`,
      );
    }
  }
  for (const role of [ROLE_SYSTEM, ROLE_APP, ROLE_UNKNOWN]) {
    for (const want of ladder) {
      assert.equal(new Caller(role).atLeast(want), false, `${role}.atLeast(${want})`);
    }
  }
  // Asking "at least system?" is the wrong question and must not be answered yes
  // however authorized the caller is.
  assert.equal(new Caller(ROLE_OWNER).atLeast(ROLE_SYSTEM), false);
});

// A missing header refuses, and that is the whole safety property: the one
// deployment where it is missing is the one nobody configured.
test("an unverifiable caller is unknown and unknown refuses", () => {
  delete process.env[ENV_ROLE_KEY];
  const off = caller({});
  assert.equal(off.role, ROLE_UNKNOWN);
  assert.equal(off.atLeast(ROLE_ANONYMOUS), false);
  assert.equal(off.isPerson, false);
  assert.equal(off.signedIn, false);
  assert.equal(off.isSystem, false);

  // A key, but a header that is a bare word rather than a signed assertion.
  process.env[ENV_ROLE_KEY] = KEY;
  try {
    assert.equal(caller({ "x-toft-role": "owner" }).role, ROLE_UNKNOWN);
    // And the real thing, read out of a lowercased Node header map.
    assert.equal(caller({ "x-toft-role": mint(KEY, ROLE_EDITOR, soon()) }).role, ROLE_EDITOR);
    // …and out of a Headers, and out of a canonically-cased plain object.
    assert.equal(caller(new Headers({ [ROLE_HEADER]: mint(KEY, ROLE_OWNER, soon()) })).role, ROLE_OWNER);
    assert.equal(caller({ [ROLE_HEADER]: mint(KEY, ROLE_VIEWER, soon()) }).role, ROLE_VIEWER);
  } finally {
    delete process.env[ENV_ROLE_KEY];
  }
});

// Bound to one key and one moment — the two properties the whole design rests
// on, from the app's side of them.
test("a wrong key and a stale blob are both unknown", () => {
  const blob = mint(KEY, ROLE_OWNER, soon());
  assert.equal(verifyRole(KEY, blob, Date.now()), ROLE_OWNER);
  assert.equal(verifyRole("another-app-key", blob, Date.now()), ROLE_UNKNOWN);
  assert.equal(verifyRole(KEY, mint(KEY, ROLE_OWNER, 1), Date.now()), ROLE_UNKNOWN);
  assert.equal(verifyRole("", blob, Date.now()), ROLE_UNKNOWN);
  assert.equal(verifyRole(KEY, "", Date.now()), ROLE_UNKNOWN);
});

// A blob signed with this app's own key still cannot introduce a word the SDK
// does not know. The app holds the key, so this is the one forgery it can
// actually mount — and while an app lying to its own handler crosses no
// privilege, app code branching on six values must not be handed a seventh.
test("a signed word the SDK does not know is unknown", () => {
  assert.equal(verifyRole(KEY, mint(KEY, "superuser", soon()), Date.now()), ROLE_UNKNOWN);
  // The same mint with a known word works, so the assertion above is about the
  // word rather than about a broken signature.
  assert.equal(verifyRole(KEY, mint(KEY, ROLE_EDITOR, soon()), Date.now()), ROLE_EDITOR);
});

// The one bit a public app gains: a signed-in reader is tellable from a
// stranger. It names nobody, and it is exactly what "refuse anonymous visitors"
// is.
test("signedIn separates a stranger from a reader", () => {
  assert.equal(new Caller(ROLE_ANONYMOUS).signedIn, false);
  assert.equal(new Caller(ROLE_ANONYMOUS).isPerson, true);
  assert.equal(new Caller(ROLE_VIEWER).signedIn, true);
  assert.equal(new Caller(ROLE_SYSTEM).signedIn, false);
  assert.equal(new Caller(ROLE_UNKNOWN).signedIn, false);
});

// The scheduled-task line from the doc comment, run: an endpoint serving both a
// person and its own schedule needs both halves, and neither admits the other.
test("a scheduled firing is not a person and a person is not a schedule", () => {
  const admits = (c) => c.role === ROLE_SYSTEM || c.atLeast(ROLE_EDITOR);
  for (const [role, want] of [
    [ROLE_SYSTEM, true],
    [ROLE_OWNER, true],
    [ROLE_EDITOR, true],
    [ROLE_VIEWER, false],
    [ROLE_ANONYMOUS, false],
    [ROLE_APP, false],
    [ROLE_UNKNOWN, false],
  ]) {
    assert.equal(admits(new Caller(role)), want, role);
  }
});

import { readFileSync, readdirSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { ACTION_PHRASE } from "./audit-actions";

/**
 * Every audit action the backend writes has a sentence on the Activity page.
 *
 * The phrase table existed and was good; what it did not have was a way of
 * noticing when it fell behind. Thirty-three actions had been added on the Go
 * side since anybody looked, so a column of written English was interrupted by
 * *Put*, *Presign*, *Change*, *Member add* and *Api key create* — the last
 * segment of an enum, title-cased, which is naming a thing by how it is stored.
 *
 * The fallback stays: an action from a newer server than this SPA must render
 * as something. This test is what stops it from being the normal case.
 *
 * It reads the Go source rather than a fixture, because a list of actions
 * maintained beside this test would be the same staleness one level along.
 */

const ROOT = fileURLToPath(new URL("../../..", import.meta.url));

function goSources(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) goSources(path, out);
    else if (name.endsWith(".go") && !name.endsWith("_test.go")) out.push(path);
  }
  return out;
}

/**
 * Every audit action the server writes, by the shapes it writes them in.
 *
 * Matching the literal alone does not work: a dotted lower-snake string in a Go
 * file is just as likely to be an OpenTelemetry metric name, a settings key or
 * `requirements.txt`. So this matches the *machinery* instead: a struct field, a
 * constant or local named for an action, and every literal passed to an
 * `audit…`, `stamp…` or `writeKey` helper (`stampDeployKey` takes its on and
 * off actions side by side). A new spelling will show up here as an action with no phrase,
 * which is a failing test that points at the right place.
 */
const ACTION = String.raw`[a-z][a-z_]*\.[a-z][a-z_.]*`;
const SHAPES = [
  new RegExp(String.raw`\bAction:\s*"(${ACTION})"`, "g"),          // Action: "app.stop"
  // Three older actions have no dot. Undotted, `Action: "read"` is an authz
  // verb, so these count only as an audit entry's field, beside its target.
  /\bAction:\s*"([a-z][a-z_]*)",\s*TargetType:/g,                  // Action: "deploy_source_refused", TargetType: …
  new RegExp(String.raw`Action\w*\s*=\s*"(${ACTION})"`, "g"),      // auditActionBuildCacheReclaim = "build_cache.reclaim"
  new RegExp(String.raw`\baction\s*:?=\s*"(${ACTION})"`, "g"),     // action := "storage_mapping.create"
  // The trusted-sources writes name an undotted action as a local audit
  // helper's first argument, with nested calls after it -- which HELPER_CALL's
  // no-parentheses body cannot see, so all four were in no chip.
  /\baudit\(\s*"([a-z][a-z_]*)"\s*,/g,                             // audit("source_policy_changed", …
];
const HELPER_CALL = /\b(?:audit|stamp|writeKey)\w*\(([^()]*)\)/g;     // audit(…, "group.create", …)
const DOTTED = /"([a-z][a-z_]*\.[a-z][a-z_.]*)"/g;

/**
 * The actions built by concatenation, which no shape can see — each with the
 * expression that builds it, so the list fails the day that expression goes
 * rather than passing over actions the server no longer writes.
 */
const COMPOSED: { file: string; builds: string; actions: string[] }[] = [
  {
    file: "internal/database/authproviders.go",
    builds: `"identity." + string(outcome)`,
    actions: ["identity.linked", "identity.provisioned"],
  },
  ...["user.api_key", "admin.user_api_key"].map((prefix) => ({
    file: "internal/api/user_apikey_handlers.go",
    builds: `keyPatchAction("${prefix}"`,
    actions: ["_disable", "_enable", "_rename"].map((s) => prefix + s),
  })),
];

function actionsWrittenByTheServer(): Set<string> {
  const found = new Set<string>();
  for (const file of goSources(join(ROOT, "internal"))) {
    const src = readFileSync(file, "utf8");
    for (const shape of SHAPES) {
      for (const m of src.matchAll(shape)) found.add(m[1]);
    }
    for (const call of src.matchAll(HELPER_CALL)) {
      for (const m of call[1].matchAll(DOTTED)) found.add(m[1]);
    }
  }
  for (const c of COMPOSED) {
    const src = readFileSync(join(ROOT, c.file), "utf8");
    if (!src.includes(c.builds)) throw new Error(`${c.file} no longer builds ${c.builds}`);
    c.actions.forEach((a) => found.add(a));
  }
  return found;
}

/** The Activity chips, read out of the Go map that defines them. */
function activityCategories(): Record<string, string[]> {
  const src = readFileSync(join(ROOT, "internal/api/event_handlers.go"), "utf8");
  const start = src.indexOf("var activityCategories = map[string][]string{");
  const body = src.slice(start, src.indexOf("\n}\n", start));
  const out: Record<string, string[]> = {};
  for (const m of body.matchAll(/"(\w+)":\s*\{([^}]*)\}/g)) {
    out[m[1]] = [...m[2].matchAll(/"([^"]+)"/g)].map((p) => p[1]);
  }
  return out;
}

const matches = (action: string, pattern: string) =>
  pattern.endsWith("*") ? action.startsWith(pattern.slice(0, -1)) : action === pattern;

describe("the Activity page's phrase table", () => {
  it("has a sentence for every action the server writes", () => {
    const written = actionsWrittenByTheServer();
    // A floor, so a regex that quietly stops matching fails loudly rather than
    // passing over an empty set.
    expect(written.size).toBeGreaterThan(40);

    const missing = [...written].filter((a) => !ACTION_PHRASE[a]).sort();
    expect(
      missing,
      `these actions would render as a title-cased enum: ${missing.join(", ")}`,
    ).toEqual([]);
  });

  it("has no phrase for an action the server has never heard of", () => {
    // The other direction, and deliberately the looser of the two.
    //
    // The forward check can be strict because a missing phrase is a visible
    // defect and the four shapes above are how audit actions are written. This
    // one cannot: the Activity page unions audit rows with *events*, and an
    // event carries its own summary rather than a phrase — so an action name
    // that only ever appears as an event category pattern (`app.create` and
    // `app.delete` are both, in `event_handlers.go`) is not written by any of
    // those shapes and is not stale either. Deleting a live phrase on a regex's
    // say-so is the worse failure of the two.
    //
    // So this asks the weakest question that still catches the real case: does
    // the server mention this string at all. A phrase left behind by an action
    // that was removed outright — `object_grant.delete`, which is spelled
    // `.remove` — has nothing anywhere, and that is what this finds.
    const mentioned = new Set<string>(COMPOSED.flatMap((c) => c.actions));
    for (const file of goSources(join(ROOT, "internal"))) {
      const src = readFileSync(file, "utf8");
      for (const action of Object.keys(ACTION_PHRASE)) {
        if (src.includes(`"${action}"`)) mentioned.add(action);
      }
    }
    const orphans = Object.keys(ACTION_PHRASE).filter((a) => !mentioned.has(a)).sort();
    expect(orphans, `no server code mentions: ${orphans.join(", ")}`).toEqual([]);
  });
});

describe("the Activity page's category chips", () => {
  // A chip is a family of action strings, and an action in no family is only
  // ever seen under All — and is missing from the compliance report's
  // audit_by_category evidence, which samples the same map. Eighty of 133 were.
  it("put every action the server writes in exactly one chip", () => {
    const cats = activityCategories();
    expect(Object.keys(cats).sort()).toEqual(["access", "deploys", "settings", "signins"]);
    const problems: string[] = [];
    for (const action of [...actionsWrittenByTheServer()].sort()) {
      const homes = Object.keys(cats).filter((c) => cats[c].some((p) => matches(action, p)));
      if (homes.length !== 1) problems.push(`${action} → [${homes.join(", ")}]`);
    }
    expect(problems, `actions in no chip or several: ${problems.join("; ")}`).toEqual([]);
  });

  it("have no pattern that matches nothing the server writes", () => {
    const written = [...actionsWrittenByTheServer()];
    const dead = Object.entries(activityCategories()).flatMap(([cat, patterns]) =>
      patterns.filter((p) => !written.some((a) => matches(a, p))).map((p) => `${cat}: ${p}`),
    );
    expect(dead).toEqual([]);
  });
});

/** The audit scope map, read out of the Go file that defines it. */
function auditScopes(): Record<string, string> {
  const src = readFileSync(join(ROOT, "internal/database/audit_scope.go"), "utf8");
  const start = src.indexOf("var AuditAppScope = map[string]AuditScope{");
  const body = src.slice(start, src.indexOf("\n}\n", start));
  const out: Record<string, string> = {};
  for (const m of body.matchAll(/"([^"]+)":\s*AuditScope(App|Install|Either)\b/g)) out[m[1]] = m[2];
  return out;
}

describe("the audit scope map", () => {
  // Which actions are part of an app's history decides what `?app=` on the
  // feed returns. An action in no scope is never checked at a test database's
  // cleanup, so a writer that forgot its app would ship and its rows would be
  // missing from that app's history for good.
  it("gives every action the server writes a scope", () => {
    const scopes = auditScopes();
    expect(Object.keys(scopes).length).toBeGreaterThan(30);
    const unscoped = [...actionsWrittenByTheServer()]
      .filter((a) => !Object.keys(scopes).some((p) => matches(a, p)))
      .sort();
    expect(unscoped, `actions in no scope: ${unscoped.join(", ")}`).toEqual([]);
  });

  it("has no pattern that matches nothing the server writes", () => {
    const written = [...actionsWrittenByTheServer()];
    const dead = Object.keys(auditScopes()).filter((p) => !written.some((a) => matches(a, p)));
    expect(dead).toEqual([]);
  });
});

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { FRAMEWORK_MARKS } from "./framework-marks";
import {
  LANDING_BUILT_OUTPUT,
  LANDING_LANGUAGES,
  landingFrameworkLabel,
} from "./landing-frameworks";
import { LANDING_INTEGRATION_GROUPS } from "./landing-integrations";

/* ------------------------------------------------------------------ *
 * The checked claims, handed to the product's site
 *
 * gettoft.com says which frameworks build, which languages have an SDK and
 * which systems an app can send to — the claims ADR §66 keeps checked. The
 * site is another repository and cannot run these tests, so the lists leave
 * here as one JSON file, `landing-claims.json`, which the site copies in
 * (`scripts/sync-claims.sh` in toft-site). This test is the drift guard:
 * a change to a list that does not regenerate the file fails here, so the
 * file can never say something the walks in `landing-frameworks.test.ts`
 * and `landing-integrations.test.ts` have not already checked.
 *
 * Regenerate with `UPDATE_LANDING_CLAIMS=1 npx vitest run landing-claims`.
 * ------------------------------------------------------------------ */

const FILE = fileURLToPath(new URL("./landing-claims.json", import.meta.url));

function mark(id: string) {
  const m = FRAMEWORK_MARKS[id];
  return m ? { title: m.title, color: m.color, path: m.path } : null;
}

function claims() {
  return {
    languages: LANDING_LANGUAGES.map((l) => ({
      language: l.language,
      mark: mark(l.markFor),
      named: l.named.map((id) => ({
        id,
        label: landingFrameworkLabel(id),
        mark: mark(id),
      })),
      generic: l.generic?.label ?? null,
      sdk: Boolean(l.sdk),
    })),
    builtOutput: [...LANDING_BUILT_OUTPUT],
    integrationGroups: LANDING_INTEGRATION_GROUPS.map((g) => ({
      label: g.label,
      names: [...g.names],
    })),
  };
}

describe("landing-claims.json", () => {
  it("is what the checked lists say", () => {
    const want = JSON.stringify(claims(), null, 2) + "\n";
    if (process.env.UPDATE_LANDING_CLAIMS) writeFileSync(FILE, want);
    expect(
      readFileSync(FILE, "utf8"),
      "regenerate with UPDATE_LANDING_CLAIMS=1 npx vitest run landing-claims",
    ).toBe(want);
  });
});

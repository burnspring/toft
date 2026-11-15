import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import {
  LANDING_INTEGRATION_GROUPS,
  LANDING_INTEGRATIONS,
} from "./landing-integrations";

/* ------------------------------------------------------------------ *
 * The landing page's second testable claim
 *
 * Naming ten outside systems on a marketing page is the same kind of
 * assertion the framework names are: each one says a connection of that
 * kind can be made and an app can send through it, and each one is true
 * only because a row exists in `appint.Known`. The invariant governing
 * that catalog already says the list is the specification — a kind with
 * no transport, no refusal copy or no settings-index mention is a test
 * failure — so this extends the same walk one surface further out.
 *
 * Both directions, because both failures are real: a name here the
 * catalog does not carry is a destination nobody can pick, and a kind
 * added to the catalog while this page still lists nine is a feature
 * shipped invisibly.
 *
 * Reading the Go source rather than importing anything, on
 * `landing-frameworks.test.ts`'s reasoning.
 * ------------------------------------------------------------------ */

const CATALOG = fileURLToPath(
  new URL("../../../internal/appint/catalog.go", import.meta.url),
);

/**
 * Every `Title:` inside the `Known` slice's literals.
 *
 * Anchored on two tabs, which is the indentation of a field inside one of
 * that slice's elements — a `Title` on any other struct in the file sits
 * at a different depth and is not swept up.
 */
function catalogTitles(): string[] {
  const source = readFileSync(CATALOG, "utf8");
  return [...source.matchAll(/^\t\tTitle:\s+"([^"]+)",?$/gm)].map((m) => m[1]);
}

describe("the landing page's integration names", () => {
  const catalog = catalogTitles();

  // A silently empty match list would make both directions pass against
  // nothing, which is the way this kind of test dies.
  it("finds the catalog's titles", () => {
    expect(catalog).toContain("Slack");
    expect(catalog.length).toBeGreaterThan(5);
  });

  it("names only kinds the catalog carries", () => {
    for (const title of LANDING_INTEGRATIONS) {
      expect(catalog, `the landing page claims "${title}"`).toContain(title);
    }
  });

  it("accounts for every kind in the catalog", () => {
    for (const title of catalog) {
      expect(
        LANDING_INTEGRATIONS,
        `the catalog carries "${title}" and the landing page says nothing about it`,
      ).toContain(title);
    }
  });

  it("names each kind exactly once", () => {
    expect(new Set(LANDING_INTEGRATIONS).size).toBe(LANDING_INTEGRATIONS.length);
  });

  // The page says "ten places" in words beside the list, and a sentence
  // that disagrees with the list under it is worse than no sentence.
  // The band's copy no longer states a count; this holds the list's length to
  // the catalog's, so a kind removed from both cannot pass unnoticed.
  it("is the catalog's eleven", () => {
    expect(LANDING_INTEGRATIONS.length).toBe(11);
  });

  // The page draws the groups, not the flat list, so a name the groups miss
  // is a name the page does not show.
  it("puts every kind in exactly one group", () => {
    const grouped = LANDING_INTEGRATION_GROUPS.flatMap((g) => g.names);
    expect([...grouped].sort()).toEqual([...LANDING_INTEGRATIONS].sort());
  });
});

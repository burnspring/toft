import { readdirSync, readFileSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { FRAMEWORK_MARKS } from "./framework-marks";
import {
  LANDING_BUILT_OUTPUT,
  LANDING_LANGUAGES,
  UNMARKED,
  landingFrameworkLabel,
  landingFrameworkNamed,
} from "./landing-frameworks";

/* ------------------------------------------------------------------ *
 * The landing page's testable claims
 *
 * Most of that page is prose, governed by a line in
 * the `toft-ui` skill and by whoever reads the diff. The framework
 * names are not: they assert that a repository of a particular shape
 * will deploy here, which is a claim about a code path, and the code
 * path is `DetectFramework`.
 *
 * So the names live in one array and this walks it against the
 * detector's own constants — in both directions, because both failures
 * are real. Adding "Django" to the page fails here, and it is the single
 * most damaging sentence available: it is the framework an evaluator is
 * most likely to try next, and a Django repo lands in the generic
 * `python` outcome with no `manage.py` handling of any kind. Deleting a
 * detector and leaving the page advertising it fails here too.
 *
 * The SDK column is the second such claim and is checked the same way,
 * against the directories under `sdk/`. A tick in that column says code
 * in that language has a client library shipped with the platform; an
 * SDK added without the column moving fails here, and so does a column
 * naming a language whose directory is gone.
 *
 * Reading the Go source and the tree rather than importing anything:
 * these are the facts in the product that live on the other side of a
 * language boundary, and a regex over a const block is a cheaper bridge
 * than a generated file that can be stale. `internal/tags/system_test.go`
 * makes the same crossing from the Go side for the tag values.
 * ------------------------------------------------------------------ */

const REPO = new URL("../../../", import.meta.url);
const DETECTOR = fileURLToPath(new URL("internal/builder/detector.go", REPO));
const SDK_DIR = fileURLToPath(new URL("sdk/", REPO));

/** Every `FrameworkX Framework = "x"` in the detector's const block. */
function detectorFrameworks(): string[] {
  const source = readFileSync(DETECTOR, "utf8");
  const ids = [
    ...source.matchAll(/^\tFramework\w+\s+Framework = "([a-z]+)"$/gm),
  ].map((m) => m[1]);
  return ids;
}

/**
 * Every language directory under `sdk/`.
 *
 * `testdata` is the one directory there that is not a client library — it
 * holds the role vector both SDKs are verified against — and it is named
 * here rather than filtered by a pattern, so that a second non-library
 * directory has to be thought about instead of silently matching.
 */
function sdkLanguages(): string[] {
  return readdirSync(SDK_DIR)
    .filter((entry) => statSync(`${SDK_DIR}${entry}`).isDirectory())
    .filter((entry) => entry !== "testdata")
    .sort();
}

describe("the landing page's framework names", () => {
  const detected = detectorFrameworks();

  // A silently empty match list would make every assertion below pass
  // against nothing, which is the way this kind of test dies.
  it("finds the detector's constants", () => {
    expect(detected).toContain("nextjs");
    expect(detected).toContain("unsupported");
    expect(detected.length).toBeGreaterThan(5);
  });

  const claimed = LANDING_LANGUAGES.flatMap((g) => [
    ...g.named,
    ...(g.generic ? [g.generic.outcome] : []),
  ]);

  it("names only frameworks the detector can return", () => {
    for (const id of claimed) {
      expect(detected, `the landing page claims "${id}"`).toContain(id);
    }
  });

  // `unsupported` is the refusal, not an outcome. A page listing it would
  // be advertising the error message.
  it("does not claim the refusal", () => {
    expect(claimed).not.toContain("unsupported");
  });

  it("accounts for every detection outcome", () => {
    for (const id of detected) {
      if (id === "unsupported") continue;
      expect(
        claimed,
        `the detector returns "${id}" and the landing page says nothing about it`,
      ).toContain(id);
    }
  });

  it("claims each outcome exactly once", () => {
    expect(new Set(claimed).size).toBe(claimed.length);
  });

  // Every name on that page is rendered beside its mark, so an outcome with
  // no mark is a hole in the row rather than a missing decoration. The
  // exception has to be declared in UNMARKED, so that a mark deleted by
  // accident cannot look like a vendor that never published one.
  it("has a vendored mark for everything it shows", () => {
    for (const id of claimed) {
      if (UNMARKED.includes(id)) {
        expect(
          FRAMEWORK_MARKS[id],
          `"${id}" is declared unmarked but has a mark`,
        ).toBeUndefined();
        continue;
      }
      expect(FRAMEWORK_MARKS[id], `no mark vendored for "${id}"`).toBeDefined();
      expect(FRAMEWORK_MARKS[id].path.length).toBeGreaterThan(50);
      expect(FRAMEWORK_MARKS[id].color).toMatch(/^#[0-9a-f]{6}$/);
    }
  });

  // The mark leading a row is the big one at the top of it, and it is the
  // one place a key that is not a detection outcome is allowed — R's row
  // leads with the language, because none of its four outcomes has a
  // published mark. What it may never be is a hole.
  it("has a mark for every row it leads with", () => {
    for (const group of LANDING_LANGUAGES) {
      expect(
        FRAMEWORK_MARKS[group.markFor],
        `the ${group.language} row leads with "${group.markFor}" and no mark is vendored for it`,
      ).toBeDefined();
    }
  });

  // A row's own mark is the big one at the top of it, so the exception may
  // only ever apply to a name inside a row.
  it("declares nothing unmarked that the page leads a row with", () => {
    for (const id of UNMARKED) {
      expect(
        claimed,
        `"${id}" is declared unmarked and is not on the page`,
      ).toContain(id);
      expect(LANDING_LANGUAGES.map((g) => g.markFor)).not.toContain(id);
    }
  });

  // The labels come from `frameworks.ts`, which is the product's one table
  // of display names — so "Next.js" is spelled in a single place and a
  // name the table has never heard of falls back to its own identifier,
  // which is how "nextjs" would end up on the page in lower case.
  //
  // Asked as "is it in the table", not as "is the label different from the
  // identifier": marimo spells its own name exactly the way the detector
  // does, so the second question answers no for a name that is perfectly
  // well known.
  it("has a display name for every framework it names", () => {
    for (const id of LANDING_LANGUAGES.flatMap((g) => g.named)) {
      expect(landingFrameworkNamed(id), `no display name for "${id}"`).toBe(
        true,
      );
      expect(landingFrameworkLabel(id).length).toBeGreaterThan(0);
    }
  });
});

describe("the landing page's SDK column", () => {
  const shipped = sdkLanguages();

  // Same guard as the detector's: an empty listing would make both
  // directions below pass against nothing.
  it("finds the shipped client libraries", () => {
    expect(shipped).toContain("go");
    expect(shipped.length).toBeGreaterThan(2);
  });

  const ticked = LANDING_LANGUAGES.flatMap((g) => (g.sdk ? [g.sdk] : [])).sort();

  it("ticks only languages with a library in the tree", () => {
    for (const dir of ticked) {
      expect(shipped, `the landing page claims an SDK for "${dir}"`).toContain(
        dir,
      );
    }
  });

  it("accounts for every library in the tree", () => {
    for (const dir of shipped) {
      expect(
        ticked,
        `sdk/${dir} ships and the landing page says nothing about it`,
      ).toContain(dir);
    }
  });

  it("ticks each language exactly once", () => {
    expect(new Set(ticked).size).toBe(ticked.length);
  });
});

/* ------------------------------------------------------------------ *
 * The generators named without a detector behind them
 *
 * The page's other list of names, and the one §66 does not forbid,
 * because it claims a different mechanism: a generator's *output* is a
 * folder of HTML, and the declared static path serves one. Nothing here
 * can prove that Astro builds — this repository never runs it — so what
 * is checked is the bound that keeps the claim from turning into the
 * other kind: **no name may be one the detector can return, and none may
 * be one the matrix already names.** A generator that gained a detector
 * would otherwise be claimed twice in two voices, and the weaker one is
 * what somebody would read.
 * ------------------------------------------------------------------ */

describe("the landing page's built-output names", () => {
  const detected = detectorFrameworks();

  it("names nothing the detector can return", () => {
    for (const name of LANDING_BUILT_OUTPUT) {
      expect(
        detected,
        `"${name}" is a detection outcome and belongs in the matrix`,
      ).not.toContain(name.toLowerCase().replace(/[^a-z]/g, ""));
    }
  });

  it("names nothing the matrix already names", () => {
    const claimed = LANDING_LANGUAGES.flatMap((g) => [
      g.language.toLowerCase(),
      ...g.named.map((id) => landingFrameworkLabel(id).toLowerCase()),
    ]);
    for (const name of LANDING_BUILT_OUTPUT) {
      expect(
        claimed,
        `"${name}" is already on the page as a recognized framework`,
      ).not.toContain(name.toLowerCase());
    }
  });

  it("is a list of display names, not identifiers", () => {
    expect(LANDING_BUILT_OUTPUT.length).toBeGreaterThan(0);
    for (const name of LANDING_BUILT_OUTPUT) {
      expect(name, `"${name}" is not capitalized like a product name`).toMatch(
        /^[A-Z]/,
      );
    }
  });
});

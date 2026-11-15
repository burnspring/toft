// What the landing page may say the platform runs, and the only place it is
// said. `landing-frameworks.test.ts` walks this against the detector's own
// constants in internal/builder/detector.go, in both directions: a name here
// the detector cannot return fails the build, and a detection outcome missing
// from here fails it too. The array is the specification.

/** The label for a named framework comes from the one table of those. */
import { frameworkMeta, hasFrameworkMeta } from "./frameworks";

/** The catch-all outcome of a row, where the row has one. */
export interface LandingGeneric {
  /** The detection outcome the sentence stands for. */
  outcome: string;
  /**
   * What everything else in this row is called. Generic on purpose: there is
   * no Django detector, so a Django repo lands here, and "any other Python app"
   * is the honest description of that. Deliberately free of file names — this
   * copy is read by somebody deciding whether to look further, not by somebody
   * debugging a build.
   */
  label: string;
}

export interface LandingLanguage {
  /** The row heading, which is a language or a kind — never a framework. */
  language: string;
  /**
   * Which mark leads the row. A key into `FRAMEWORK_MARKS`, and **not
   * necessarily a detection outcome**: R's four outcomes have no published mark
   * between them, so its row leads with the language's own.
   */
  markFor: string;
  /**
   * The detector identifiers this row names *by name*. The reason the Node row
   * is one name long is the point: Express, Fastify, NestJS, Hono and Koa all
   * detect as `node` and none of them are detected as themselves, so naming one
   * would be a claim about a code path that does not exist.
   */
  named: string[];
  /**
   * The catch-all, **absent where the row has none**. R is the row that has
   * none: an R project that is not a Shiny app, a plumber API or one of the two
   * documents is refused rather than built generically, and a sentence in this
   * column would say the opposite. Notebooks has one, because `notebook`
   * genuinely is "a .ipynb or a .qmd, whatever is in it".
   */
  generic?: LandingGeneric;
  /**
   * The directory under `sdk/` holding this language's client library, where
   * one exists. Checked against that directory in both directions, on the same
   * argument the framework names are: it asserts that code written in this
   * language can call the platform back, which is a claim about shipped files
   * rather than a sentence somebody wrote.
   */
  sdk?: string;
}

export const LANDING_LANGUAGES: LandingLanguage[] = [
  {
    language: "Python",
    markFor: "python",
    named: ["flask", "fastapi", "streamlit"],
    generic: { outcome: "python", label: "Any other Python app" },
    sdk: "python",
  },
  {
    language: "Node",
    markFor: "node",
    named: ["nextjs"],
    generic: { outcome: "node", label: "Any other Node app" },
    sdk: "node",
  },
  {
    language: "Go",
    markFor: "go",
    named: [],
    generic: { outcome: "go", label: "Any Go service" },
    sdk: "go",
  },
  {
    language: "R",
    markFor: "r",
    named: ["shiny", "plumber", "rmarkdown", "quarto"],
    sdk: "r",
  },
  {
    language: "Notebooks",
    markFor: "notebook",
    named: ["marimo"],
    generic: {
      outcome: "notebook",
      label: "Jupyter and Quarto documents, re-run when you deploy",
    },
  },
  {
    language: "Static",
    markFor: "static",
    named: [],
    generic: { outcome: "static", label: "A folder of HTML, served as it is" },
  },
];

/**
 * Detection outcomes this page names without a mark beside them.
 *
 * Every other name here is rendered beside a vendored trademark, and an outcome
 * with no mark is a hole in the row rather than a missing decoration — which is
 * why `landing-frameworks.test.ts` normally fails the build for one. This is the
 * stated exception, and it has to be stated rather than inferred from an absent
 * key, or a mark deleted by accident would look like this.
 *
 * marimo publishes no single-path 24x24 icon, and Simple Icons carries no entry
 * for it. The alternative is drawing one, and `framework-marks.ts` refuses that
 * in as many words: a hand-corrected trademark is a wrong trademark. So the name
 * is set in text and nothing is drawn.
 *
 * Shiny, plumber, rmarkdown and quarto are here for the same reason — Simple
 * Icons has no entry for any of the four. That is what leaves the R row with
 * nothing of its own to lead with, and why its `markFor` is a language rather
 * than an outcome.
 */
export const UNMARKED: readonly string[] = [
  "marimo",
  "shiny",
  "plumber",
  "rmarkdown",
  "quarto",
];

/** The display name for one of the named identifiers above. */
export function landingFrameworkLabel(id: string): string {
  return frameworkMeta(id)?.label ?? id;
}

/** Whether the one table of display names has an entry for this identifier. */
export const landingFrameworkNamed = hasFrameworkMeta;

/**
 * Generators the landing page names *without* the detector having heard of any
 * of them, and the one exception to §66's rule that it may not.
 *
 * The rule exists to stop the page claiming a code path that is not there:
 * "Django" is the sentence not to write, because a Django repo lands in the
 * generic `python` outcome and nothing in the product knows what `manage.py`
 * is. These names are the opposite case, and the difference is which mechanism
 * is being claimed. A generator's *output* is a folder of HTML, and the
 * declared static path in `DetectFramework` is answered before any detector
 * runs and overrides every disqualifier — which is what makes a repository with
 * a `package.json` servable at all. So the claim is not "Toft recognizes
 * Astro"; it is "a folder of HTML is served as it is, and this is where people
 * get one from". The product's own user guide has said exactly this since
 * static hosting landed, and this page was the last surface still silent about
 * it.
 *
 * The bound on what may go in here, and what `landing-frameworks.test.ts`
 * checks: **no name may be one the detector can return, or one the matrix
 * already names.** A generator that became a detection outcome would then be
 * claimed twice, in two different voices, and the weaker of the two would be
 * the one somebody read. The test cannot prove any of these builds — nothing in
 * this repository runs `astro build` — and that is the honest limit of it: what
 * it prevents is the page contradicting itself.
 *
 * Ordered by how likely a reader is to have one, not alphabetically.
 */
export const LANDING_BUILT_OUTPUT: readonly string[] = [
  "Vite",
  "Astro",
  "SvelteKit",
  "Hugo",
  "Eleventy",
  "Create React App",
];

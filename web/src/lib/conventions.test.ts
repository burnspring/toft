import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

import { describe, expect, it } from "vitest";

/**
 * The conventions that were being kept by hand, kept by the test run instead.
 *
 * Every rule below is one the `toft-ui` skill already states and asks a
 * person to grep for before finishing. A review of the whole of `web/src`
 * found each of them broken in five to thirty-five places — not because anyone
 * disagreed with the rule, but because every individual instance looked fine
 * and the grep is only run by whoever remembers it. So the greps run here.
 *
 * A failure is a list of file:line. Fix the site, or — if the rule genuinely
 * does not apply — add the file to that rule's exceptions **with the reason**,
 * which is the part that makes an exception reviewable.
 */

const SRC = new URL("..", import.meta.url).pathname;

function sources(exts: string[]): string[] {
  const out: string[] = [];
  (function walk(dir: string) {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (exts.some((e) => entry.endsWith(e))) out.push(full);
    }
  })(SRC);
  return out.sort();
}

/**
 * The file with its comments blanked out, line numbers intact.
 *
 * Every rule here is about what the product *says*, and a comment explaining
 * why a rule exists will quote the thing the rule forbids — which is how a
 * linter comes to fail on its own explanation. Replacing comment characters
 * with spaces rather than removing them keeps `:line` honest.
 */
function withoutComments(text: string): string {
  let out = "";
  let i = 0;
  while (i < text.length) {
    const two = text.slice(i, i + 2);
    if (two === "//") {
      while (i < text.length && text[i] !== "\n") {
        out += " ";
        i++;
      }
      continue;
    }
    if (two === "/*" && text[i - 1] !== ">") {
      while (i < text.length && text.slice(i, i + 2) !== "*/") {
        out += text[i] === "\n" ? "\n" : " ";
        i++;
      }
      out += "  ";
      i += 2;
      continue;
    }
    // A string literal is scanned as itself: a `//` inside one is not a
    // comment, and a rule about copy is mostly about string literals.
    if (text[i] === '"' || text[i] === "`" || text[i] === "'") {
      const quote = text[i];
      out += text[i];
      i++;
      while (i < text.length) {
        if (text[i] === "\\") {
          out += text.slice(i, i + 2);
          i += 2;
          continue;
        }
        out += text[i];
        if (text[i] === quote) {
          i++;
          break;
        }
        i++;
      }
      continue;
    }
    out += text[i];
    i++;
  }
  return out;
}

interface Violation {
  where: string;
  line: string;
}

function scan(
  pattern: RegExp,
  { exts = [".tsx"], skip = [] as string[] } = {},
): Violation[] {
  const found: Violation[] = [];
  for (const file of sources(exts)) {
    const rel = relative(SRC, file);
    if (rel.endsWith(".test.ts") || rel.endsWith(".test.tsx")) continue;
    // A prefix ending in "/" skips a directory; anything else is one file.
    if (skip.some((s) => (s.endsWith("/") ? rel.startsWith(s) : rel === s))) continue;
    withoutComments(readFileSync(file, "utf8"))
      .split("\n")
      .forEach((line, i) => {
        if (pattern.test(line)) found.push({ where: `${rel}:${i + 1}`, line: line.trim() });
      });
  }
  return found;
}

const report = (v: Violation[]) => v.map((x) => `${x.where}  ${x.line}`);

describe("colour", () => {
  /**
   * A state's colour comes from a tone, never from a palette step.
   *
   * The skill's own grep covered `bg-` only, so twenty-five sites survived it
   * in `text-` and `border-`. Six components had each grown a private copy of
   * the status table before `lib/tone.ts` existed, and they had drifted; these
   * are the same failure one property along.
   */
  it("has no raw Tailwind palette step in any colour position", () => {
    const violations = scan(
      /\b(?:bg|text|border|fill|stroke|ring|from|via|to|decoration|outline|shadow|accent|caret|divide)-(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}\b/,
      {
        exts: [".tsx", ".ts"],
        // The tag palette is not a state. A tag's colour is a label an
        // administrator picked from a bounded set, carries no meaning about
        // health or activity, and must not be drawn from the five tones — an
        // amber tag would read as a warning about the app wearing it.
        skip: ["lib/tags.ts"],
      },
    );
    expect(report(violations)).toEqual([]);
  });
});

describe("type", () => {
  /** `text-2xs` (11px) is the floor and the scale is the whole vocabulary. */
  it("has no arbitrary pixel type size", () => {
    expect(report(scan(/text-\[\d+px\]/))).toEqual([]);
  });

  /**
   * One radius for a raised surface. `Card` was `rounded-xl` with a ring while
   * `TableCard` was `rounded-lg` with a border, so a Gallery tile or a stat
   * tile beside a Section read as a second product's component.
   */
  it("rounds a surface at one radius", () => {
    expect(report(scan(/\brounded-(?:xl|2xl|3xl)\b/, { skip: ["components/ui/"] }))).toEqual([]);
  });

  /**
   * Sentence case is the product's case, and a label set in capitals is Title
   * Case's louder cousin: OWNER over a fact, OVERVIEW over a nav group and ON
   * THIS PAGE over a rail were eleven eyebrows on pages that otherwise speak
   * in sentences. A literal the reader types back — a device code, a license
   * key — is the one thing that may be shown in capitals.
   */
  it("sets no label in capitals", () => {
    const violations = scan(/\buppercase\b/, {
      skip: ["components/ui/", "pages/cli-sign-in.tsx"],
    });
    expect(report(violations)).toEqual([]);
  });
});

describe("disabled controls", () => {
  /**
   * A disabled element is not focusable and swallows mouse events, so a
   * `title` on one is unreachable by every route: keyboard, screen reader and
   * hover alike. It is also the state where the explanation is the whole
   * point — "why can I not press this" is the question the control just
   * raised. Put the reason in a `Notice` beside it, in the control's own
   * accessible name, or on a wrapper that still receives the pointer.
   */
  it("never explains a disabled control with a title", () => {
    const found: Violation[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const text = readFileSync(file, "utf8");
      for (const m of text.matchAll(/<[A-Za-z][^<>]*?>/gs)) {
        const tag = m[0];
        if (!/\bdisabled(=|\s|>|\})/.test(tag)) continue;
        if (!/\btitle=/.test(tag)) continue;
        found.push({
          where: `${rel}:${text.slice(0, m.index).split("\n").length}`,
          line: tag.split("\n")[0].trim(),
        });
      }
    }
    expect(report(found)).toEqual([]);
  });
});

describe("copy", () => {
  /**
   * One ellipsis. `…` is a character; `...` is three full stops that a
   * proportional font sets too wide and a screen reader announces as three
   * sentences ending. The product had 146 of the first and 16 of the second,
   * which is a fingerprint of who typed which rather than a decision.
   *
   * Spread and rest are not ellipses; the pattern only matches inside a string
   * literal or JSX text.
   */
  it("uses … rather than three dots", () => {
    const violations = scan(/(?:"[^"]*\.\.\.|`[^`]*\.\.\.|>[^<]*\.\.\.)/).filter(
      (v) => !/\.\.\.[A-Za-z_{[(]/.test(v.line),
    );
    expect(report(violations)).toEqual([]);
  });

  /**
   * One verb for a failure. "Failed to save", "Could not save" and "Couldn't
   * save" were 53, 35 and 18 sites of the same sentence — the product speaking
   * in three registers, none of which anybody chose. `Couldn’t` is the one the
   * rest of the copy is written in.
   */
  it("says Couldn’t rather than Failed to or Could not", () => {
    expect(report(scan(/["`](?:Failed to|Could not) /, { exts: [".tsx", ".ts"] }))).toEqual([]);
  });

  /**
   * One apostrophe, and it is the curly one — which is what the prose already
   * used in 103 places, against the straight one a keyboard produces.
   */
  it("uses ’ in copy", () => {
    // Anywhere a person can read it: inside a string literal, and in JSX text
    // between the tags. Comments are already blanked out by `withoutComments`.
    //
    // **Two shapes, and the second is the one that got through.** A letter on
    // both sides catches `don't`; a *plural possessive* has no letter after
    // the mark — "your apps' code", "your administrators' queue" — so three
    // visible strings kept a straight apostrophe while the rule was green. It
    // has to end a word, which is what keeps a JSX attribute's own quote and a
    // `'…'` string literal out of it.
    const violations = scan(/[A-Za-z]'[A-Za-z]|[A-Za-z]s'(?![A-Za-z'])/, {
      exts: [".tsx", ".ts"],
    });
    expect(report(violations)).toEqual([]);
  });

  /**
   * The same two marks inside an interpolation, where the letter-on-both-sides
   * pattern cannot see them: `${name}'s` has a brace before the mark, and
   * `"${name}"` puts straight quotes around a name in a toast.
   */
  it("uses ’ and “ ” around an interpolated name", () => {
    const violations = scan(/\}'s\b|`[^`]*"\$\{[^`]*`/, { exts: [".tsx", ".ts"] });
    expect(report(violations)).toEqual([]);
  });

  /**
   * A failure toast whose subject comes first — "That folder could not be
   * read" — is the same sentence as `Couldn’t read that folder`, said in the
   * register the rule above retired.
   */
  it("starts a failure toast with Couldn’t", () => {
    expect(report(scan(/toast\.error\([^)]*["`][^"`]*\bcould not\b/i, { exts: [".tsx", ".ts"] }))).toEqual([]);
  });

  /** "When off, …" — `Off:` is the same fragment as `Off, `. */
  it("writes a condition as When off", () => {
    expect(report(scan(/["`>]\s*O(?:ff|n)[:,] /, { exts: [".tsx", ".ts"] }))).toEqual([]);
  });

  /**
   * A read that could not be read says so in one grammar: "The queue could not
   * be read". "Couldn’t load the queue" is a toast's sentence, and thirteen
   * pages used it one heading away from fourteen that did not.
   */
  it("titles a QueryError with its subject", () => {
    expect(report(scan(/<QueryError[^>]*title=["{`][^"`]*Couldn’t/))).toEqual([]);
  });

  /**
   * One spelling, and it is the US one.
   *
   * The product had drifted into both: a tag's colour picker, a key the
   * license service had not recognised, an endpoint ignored in favour of a
   * saved one, and a dependency mode to run in for a fortnight. Each looked
   * fine on its own page; together they are two products. `license` is in
   * the pattern with the rest since the rename to Toft took the identifiers
   * with it, and the whole tree is held to it by
   * `internal/docsguard/spelling_test.go`.
   */
  it("spells copy the US way", () => {
    const violations = scan(
      /[Rr]ecognis|[Cc]olour|[Bb]ehaviour|[Ff]avour|[Oo]rganis[aei]|[Cc]atalogue|[Ff]ortnight|[Ll]icence|\b[Rr]ota\b/,
      { exts: [".tsx", ".ts"] },
    );
    expect(report(violations)).toEqual([]);
  });
});

describe("time", () => {
  /**
   * One place formats a date.
   *
   * Ten sites formatted their own, and they had drifted exactly as copies do:
   * one page spelled every date `en-GB` while the rest of the product spelled
   * them `en-US`, three charts over the same UTC buckets used three different
   * shapes, and one of them parsed at noon to work around a timezone slide the
   * others solved with `timeZone: "UTC"`. `lib/time.ts` is where a reader can
   * see all of that at once and where a change reaches every screen.
   *
   * `Number.toLocaleString()` is not this — a thousands separator on a count is
   * not a date, which is why the pattern is anchored on the date methods.
   */
  it("formats dates only in lib/time.ts", () => {
    const violations = scan(
      /\.toLocale(?:Date|Time)String\(|new Date\([^)]*\)\.toLocaleString\(/,
      { exts: [".tsx", ".ts"], skip: ["lib/time.ts"] },
    );
    expect(report(violations)).toEqual([]);
  });
});

describe("the base-ui variant", () => {
  /**
   * There is no `asChild` here. It is the single most common way to break this
   * build, because it is what every shadcn example on the internet uses.
   */
  it("uses the render prop rather than asChild", () => {
    expect(report(scan(/\basChild\b/))).toEqual([]);
  });

  /**
   * base-ui's Value renders the *stored* value, so a bare one puts
   * `system_admin` — or a raw app id — in the trigger.
   */
  it("gives every SelectValue a formatter", () => {
    expect(report(scan(/<SelectValue\s*\/>/))).toEqual([]);
  });
});

describe("the quality floor", () => {
  /**
   * The error boundary clears on navigation.
   *
   * `resetKey` was passed to the class as an ordinary prop that nothing read,
   * so a caught error latched for the life of the mount and one broken page
   * became a broken session — while the on-screen copy told the reader that
   * moving to another page clears it. The reset has now been asserted in prose
   * twice and broken twice; this is the assertion that runs.
   */
  it("clears a caught render error when the pathname changes", () => {
    const src = readFileSync(join(SRC, "components/error-boundary.tsx"), "utf8");
    expect(src).toMatch(/componentDidUpdate\(prev: Props\)/);
    expect(src).toMatch(/prev\.resetKey !== this\.props\.resetKey/);
  });

  /**
   * A new window never gets a `window.opener`.
   *
   * `<a target="_blank">` defaults to noopener; `window.open` does not, and an
   * app opened from the command palette without it could navigate the Toft
   * tab behind it to a look-alike sign-in page.
   */
  it("opens a window with noopener", () => {
    const opens = scan(/window\.open\(/, { exts: [".ts", ".tsx"] });
    expect(report(opens.filter((v) => !/noopener/.test(v.line)))).toEqual([]);
  });

  /**
   * The provider edit form does not write `enabled`.
   *
   * The checkbox left the form so a stale one could not take a provider off
   * the login page, but the field stayed in the payload and did exactly that.
   */
  it("sends enabled only when creating a provider", () => {
    const src = readFileSync(join(SRC, "pages/admin/authentication.tsx"), "utf8");
    const payload = src.slice(src.indexOf("const payload: AuthProviderInput = {"));
    const body = payload.slice(0, payload.indexOf("};"));
    expect(body).toContain("...(!editing && { enabled }),");
    expect(body).not.toMatch(/^\s*enabled,$/m);
  });

  /**
   * The app page remounts per app.
   *
   * `/apps/:id` kept one instance across a navigation to another app, so the
   * Settings tab's drafts — fields and the whole environment table — survived
   * it and Save wrote app A's values into app B.
   */
  it("keys the app detail page on the app id", () => {
    const src = readFileSync(join(SRC, "pages/app-detail.tsx"), "utf8");
    expect(src).toMatch(/export function AppDetailPage\(\) \{\s*const \{ id \} = useParams<[^>]*>\(\);\s*return <AppDetail key=\{id\} \/>;/);
  });
});

describe("light theme only", () => {
  /**
   * The `dark:` variant is redefined against a class nothing sets.
   *
   * Tailwind v4's default is `@media (prefers-color-scheme: dark)`, and the
   * vendored shadcn set still carries twenty-one `dark:` utilities. Without
   * this line every one of them fires for a viewer whose OS is in dark mode —
   * on a product with no dark theme, no toggle and no `.dark` palette. The
   * visible one was the Switch thumb: every OFF switch rendered near-black.
   *
   * Asserted here rather than by grepping for `dark:`, because the utilities
   * are in vendored files that should stay diffable against upstream.
   */
  it("neutralizes the dark variant in index.css", () => {
    const css = readFileSync(join(SRC, "index.css"), "utf8");
    expect(css).toMatch(/@custom-variant dark \(&:where\(\.dark, \.dark \*\)\);/);
  });

  /** And nothing outside the vendored set may add one. */
  it("uses no dark: utility outside components/ui/", () => {
    const violations = scan(/\bdark:/, {
      exts: [".tsx", ".ts"],
      skip: ["components/ui/"],
    });
    expect(report(violations)).toEqual([]);
  });
});

describe("the two reds", () => {
  /**
   * `--destructive` is the *control* red; a tone is the *state* red.
   *
   * Two red vocabularies in two different colours, with no rule saying which
   * was which — and one site computing a `Tone` and then discarding it for
   * `text-destructive`. `lib/tone.ts` states the rule; this is the half a grep
   * can hold. A colour class named `destructive` belongs to a control, so it
   * lives in the vendored set and in the form code listed below. Anything
   * describing a fact somebody reads — a badge, a banner, a heading over a
   * region, a failure reason, a count of things that broke — is a tone.
   *
   * Adding a file here is how a control gets declared to be one. The reason is
   * the part that makes it reviewable.
   */
  const DESTRUCTIVE_IS_A_CONTROL: Record<string, string> = {
    "components/automation-tab.tsx":
      "the stop icon on a running job's row, and stderr lines on the terminal surface",
    "components/field-help.tsx":
      "`FieldError` — the message under a field that will not save, which is what `lib/tone.ts` names as the control red. Six files came off this list when it existed, because every one of them was here to spell that message by hand",
    "components/pending-bar.tsx": "the count of fields that will not save",
    "components/source-policy-editor.tsx":
      "the list of invalid patterns, and the message inline in a pattern row — neither is a sentence under a field",
    "pages/admin/automation.tsx": "a destructive icon in a row menu",
    "pages/admin/settings.tsx": "the pending bar's dot for a field in error",
    "pages/app/settings-tab.tsx": "the same dot",
    "pages/dashboard.tsx": "the Logs link offered on a crashed row — an action, not the state, which the badge beside it already carries",
    "pages/gallery.tsx": "the required-field asterisk on a form",
    "pages/new-app.tsx": "hover on a remove-row button",
  };

  it("uses a destructive colour class only on a control", () => {
    const violations = scan(/\b(?:text|bg|border)-destructive\b/, {
      exts: [".tsx", ".ts"],
      skip: ["components/ui/", ...Object.keys(DESTRUCTIVE_IS_A_CONTROL)],
    });
    expect(report(violations)).toEqual([]);
  });
});

describe("contrast", () => {
  /**
   * A text token is used at full strength.
   *
   * `--muted-foreground` is oklch(0.50 0 0) — 6.0:1 on card, 5.66:1 on the page
   * background, comfortably passing. An opacity modifier on it drops below AA
   * at the 11px and 12px sizes where it is used most: the four admin rail group
   * labels and "On this page" were 3.1:1, and the deploy timeline's pending
   * phase names — the text naming the stages of a build in progress, on the one
   * surface the motion budget is spent on — were 2.6:1.
   *
   * The exceptions are the two places the text is not text: a decorative glyph
   * beside an already-labelled control carries nothing to read, so the 4.5:1
   * floor does not apply to it. The terminal gutter keeps a modifier because
   * its ground is `--terminal`, where the same fraction lands far higher — it
   * is at /60 and /70 rather than /40 and /50.
   */
  const NOT_TEXT: Record<string, string> = {
    "components/search-input.tsx":
      "the magnifier glyph inside the field — decoration beside a labelled control, and the clear button beside it is at full strength",
    "components/stat-card.tsx": "the icon beside a figure that is already labelled",
    "components/log-pane.tsx":
      "the time gutter, the line numbers, a folded step's count and the repeat count, on the `--terminal` ground rather than the page's — the same fraction of a near-white on near-black. It moved here from `logs-tab` when both logs came to share one pane (ADR §87)",
  };

  it("uses no opacity modifier on a text token", () => {
    const violations = scan(/text-(?:muted-|terminal-)?foreground\/\d+/, {
      exts: [".tsx"],
      skip: ["components/ui/", ...Object.keys(NOT_TEXT)],
    });
    expect(report(violations)).toEqual([]);
  });
});

describe("dialogs", () => {
  /**
   * A dialog taller than the viewport scrolls inside itself.
   *
   * `DialogContent` is `position: fixed` centred by a transform, with no
   * height bound unless `scrollable` is passed — so a tall one overflows above
   * and below the viewport and, being fixed, neither it nor the page can be
   * scrolled to reach the footer. The Save button is unreachable by mouse, by
   * keyboard and by touch. Seven dialogs were in that state, including the
   * twelve-field git-host form and a credentials dialog that grows without
   * bound through an "Add another" button.
   */
  it("bounds the height of every dialog with a body", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = readFileSync(file, "utf8");
      if (src.includes("<DialogBody") && !src.includes("DialogContent scrollable")) {
        offenders.push(rel);
      }
    }
    expect(offenders).toEqual([]);
  });

  /**
   * And it does it through `DialogBody`, not by hand.
   *
   * A `max-h` plus `overflow-y-auto` on the content itself scrolls the header
   * and the footer out with the body, which is the thing `DialogBody` exists
   * to stop — `dialog.tsx` names the anti-pattern in its own comment.
   */
  it("never hand-rolls a dialog's scroll", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      for (const [i, line] of readFileSync(file, "utf8").split("\n").entries()) {
        if (/<DialogContent/.test(line) && /max-h-\[/.test(line) && /overflow-(?:y-auto|hidden)/.test(line)) {
          offenders.push(`${rel}:${i + 1}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("a scrollable dialog", () => {
  /**
   * The reverse of "bounds the height": `scrollable` with no `DialogBody` is a
   * bounded box with nothing in it allowed to scroll, so the footer — the
   * Connect button on Admin → Integrations — is cut off below the fold.
   */
  it("gives every scrollable dialog a DialogBody", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      const opens = src.match(/<DialogContent[^>]*\bscrollable\b/g)?.length ?? 0;
      const bodies = src.match(/<DialogBody[\s>]/g)?.length ?? 0;
      if (opens > bodies) offenders.push(`${rel} (${opens} scrollable, ${bodies} bodies)`);
    }
    expect(offenders).toEqual([]);
  });
});

describe("destructive menu items", () => {
  /**
   * A destructive item in a `⋯` menu carries the variant, not a class.
   *
   * The variant colours the icon and the focus state too; a
   * `className="text-destructive"` colours the label alone, and no styling at
   * all leaves Delete rendered identically to Copy name directly above it. On
   * the store browser that was true of both Delete items while the two Delete
   * *buttons* on the same screen were correctly red — one screen rendering one
   * act two ways, with the ambiguous rendering in the crowded menu.
   */
  it("gives every deleting menu item the destructive variant", () => {
    const verbs = /\b(Delete|Revoke|Remove|Detach|Unmap)\b/;
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      // Each opening tag through to its closing tag, non-greedy.
      const re = /<DropdownMenuItem\b([^>]*)>([\s\S]*?)<\/DropdownMenuItem>/g;
      let m: RegExpExecArray | null;
      while ((m = re.exec(src)) !== null) {
        if (!verbs.test(m[2])) continue;
        if (/variant="destructive"/.test(m[1])) continue;
        const line = src.slice(0, m.index).split("\n").length;
        offenders.push(`${rel}:${line}  ${m[2].trim().split("\n")[0]}`);
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("one address for a deployed app", () => {
  /**
   * `@/lib/app-url` is the only module that says where a deployed app lives.
   *
   * Two exported functions were called `appUrl`. The one in `lib/app-url.ts`
   * returns `${appsBaseUrl}/apps/${slug}` — the address this product actually
   * serves, asked of the server because the apps origin is a different port.
   * The one in `lib/api.ts` returned `${protocol}//${slug}.${host}`, a
   * wildcard-subdomain scheme this install has never had DNS for. Eight call
   * sites imported the first and one imported the second: the "Open app"
   * anchor on the deploy console, the primary button on the screen a deploy
   * goes green on, pointing at a hostname that does not resolve. Nothing
   * reported it, because both names type-check and both return a string.
   *
   * So the rule is the shape of the cause: one exporter of the name.
   */
  it("exports appUrl from exactly one module", () => {
    const exporters: string[] = [];
    for (const file of sources([".ts", ".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.endsWith(".test.ts") || rel.endsWith(".test.tsx")) continue;
      if (/^export function appUrl\b/m.test(withoutComments(readFileSync(file, "utf8")))) {
        exporters.push(rel);
      }
    }
    expect(exporters).toEqual(["lib/app-url.ts"]);
  });

  /** And every caller imports it from there. */
  it("imports appUrl only from @/lib/app-url", () => {
    const offenders: string[] = [];
    for (const file of sources([".ts", ".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.endsWith(".test.ts") || rel.endsWith(".test.tsx")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/import\s*\{([^}]*)\}\s*from\s*"([^"]+)"/g)) {
        if (!/\bappUrl\b/.test(m[1])) continue;
        if (m[2] === "@/lib/app-url" || m[2] === "./app-url") continue;
        offenders.push(`${rel}:${src.slice(0, m.index).split("\n").length}  from "${m[2]}"`);
      }
    }
    expect(offenders).toEqual([]);
  });

  /**
   * And an address that leaves the page is the absolute one.
   *
   * `appUrl` is relative on a single-origin install — the platform redirects
   * `/apps/{slug}` to the gateway, so an `href` is correct either way. The
   * clipboard is not either way: the copy button on the app page, the apps
   * list and the tag browser all handed over `/apps/foo/`, which names nothing
   * once it is pasted into a ticket, a chat message or a spreadsheet cell, and
   * does not even say which install it came from. `appShareUrl` resolves it
   * against the page's own origin. Anything that copies, exports or mails an
   * app's address takes that one.
   */
  it("shares an app's address absolutely", () => {
    const offenders: string[] = [];
    for (const file of sources([".ts", ".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.endsWith(".test.ts") || rel.endsWith(".test.tsx")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      const leaves = rel === "lib/export-xlsx.ts" ? /\bappUrl\(/g : /copyToClipboard\(\s*appUrl\(/g;
      for (const m of src.matchAll(leaves)) {
        offenders.push(`${rel}:${src.slice(0, m.index).split("\n").length}  appUrl, not appShareUrl`);
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("table geometry", () => {
  /**
   * A table's breakpoints are the card's, never the window's — **and so are
   * the sentences that restate what a column dropped**.
   *
   * The container-query migration moved every column to `@`-variants and left
   * everything around them behind. A column hid on the card's width while the
   * line restating it under the row's first cell hid on the browser's, so the
   * two ladders crossed: between the crossings the fact is printed twice or is
   * in neither place. Seventeen sites, all of them looking fine on the one
   * width whoever wrote them had open.
   *
   * The rule is scoped to files that render a `TableCell`. A grep cannot tell
   * a table cell from a card, and a page-level `lg:hidden` on a nav or a
   * toolbar is correct — those *are* about the window. That accepts a
   * false negative on a table built somewhere else, which is the same trade
   * every rule here makes.
   */
  it("hides nothing on the viewport inside a file that renders a table", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = readFileSync(file, "utf8");
      if (!src.includes("<TableCell")) continue;
      withoutComments(src)
        .split("\n")
        .forEach((line, i) => {
          // A `@`-prefixed variant is the container's and is the right answer;
          // `xl:hidden` and `@xl:hidden` differ by one character, so the
          // pattern has to say the character before is not an `@`.
          for (const m of line.matchAll(/(^|[^@\w-])(sm|md|lg|xl|2xl):hidden\b/g)) {
            offenders.push(`${rel}:${i + 1}  ${m[2]}:hidden`);
          }
        });
    }
    expect(offenders).toEqual([]);
  });

  /**
   * And a container variant needs a container.
   *
   * `Table`'s scroll container carries `@container`, so every column in the
   * product measures the right box. Two hand-rolled `<table>`s on Admin →
   * Messages sat in a plain `overflow-x-auto` div: their `@md:table-cell`
   * column had nothing to measure and never appeared, at any width, on any
   * monitor. A container variant is silent when it does not match, which is
   * why this went unnoticed.
   */
  it("gives a hand-rolled table the container its variants ask for", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      if (!/@(?:xs|sm|md|lg|xl|2xl|3xl|4xl|5xl|6xl):/.test(src)) continue;
      // `<Table>` brings its own container; anything else has to declare one.
      if (/<Table[\s>]/.test(src)) continue;
      if (src.includes("@container")) continue;
      offenders.push(rel);
    }
    expect(offenders).toEqual([]);
  });
});

describe("three states", () => {
  /**
   * Error comes before empty, and the half a grep can hold is that the
   * component read `error` at all.
   *
   * `EmptyState` is the "nothing here, and here is what to do next" surface.
   * Rendered from a query whose failure the component never looked at, it says
   * that sentence to somebody for whom nothing can be done — "No deployments
   * yet. Use Redeploy in the header" to an owner with fifty builds whose
   * history could not be read, at the moment they are looking for one to roll
   * back to. Five surfaces were in that state.
   *
   * What this cannot see is the *order* of the branches, which is where most
   * of those five actually went wrong: they read `error` and checked it after
   * the empty test, or in a sibling that renders beside it rather than instead
   * of it. `QueryState` is the answer to that half — it owns the order — and
   * this grep is the floor under it.
   */
  it("reads error wherever an EmptyState is rendered from a query", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      const src = withoutComments(readFileSync(file, "utf8"));
      if (!src.includes("useQuery") || !src.includes("<EmptyState")) continue;
      if (/\berror\b/.test(src)) continue;
      offenders.push(rel);
    }
    expect(offenders).toEqual([]);
  });

  /**
   * And a rejection is never swallowed.
   *
   * The announcement editor read the live banner with `.catch(() => {})`, so a
   * failed read left an administrator looking at **Publish banner** over an
   * empty box — with a banner they could not see and were about to replace.
   * An ignored failure is the one failure mode with no symptom.
   *
   * Two exceptions, both because the rejection is genuinely not news:
   */
  it("never catches and ignores", () => {
    const allowed: Record<string, string> = {
      // Signing out locally is the point; a server that refuses the call has
      // not kept the session alive, and the next line clears it anyway.
      "App.tsx": "logout clears the session whatever the server answered",
      // These mutations already report through `onError`, which toasts. The
      // catch is only there so `void` does not leave an unhandled rejection.
      "pages/app/lifecycle.ts": "onError toasts; the catch only settles the promise",
      // A hint nobody asked for: the visit that follows wakes the app anyway,
      // and the gateway reports a failed wake to the person who arrives.
      "lib/wake-ahead.ts": "a hint; the visit after it wakes the app and reports any failure",
    };
    const offenders: string[] = [];
    for (const file of sources([".ts", ".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.endsWith(".test.ts") || rel.endsWith(".test.tsx")) continue;
      if (rel in allowed) continue;
      withoutComments(readFileSync(file, "utf8"))
        .split("\n")
        .forEach((line, i) => {
          if (/\.catch\(\s*\(\s*\)\s*=>\s*(\{\s*\}|undefined|null)\s*\)/.test(line)) {
            offenders.push(`${rel}:${i + 1}  ${line.trim()}`);
          }
        });
    }
    expect(offenders).toEqual([]);
  });
});

describe("one deploy date", () => {
  /**
   * "Last deployed" is `last_deployed_at`, never `updated_at`.
   *
   * `updated_at` is when the app *row* last changed, and 26 of the 34
   * `UPDATE apps SET` statements in `internal/database` touch it — the idle
   * reaper, the health watcher, the reconciler, app lock, and the git poller's
   * `SetLastSeenSHA` among them. On an install with idle sleep on, every app's
   * figure is rewritten twice per doze. Three surfaces carried that column
   * under this label: the apps list (where it is also the sort key, so sorting
   * by "Last deployed" during an incident returned the apps that had most
   * recently *broken*), the tag browser, and the Excel export somebody takes
   * to a meeting.
   *
   * The honest figure is the serving release's, computed by
   * `lastDeployedAtExpr` in `internal/database/estate.go` and carried on every
   * app row. The label is what was wrong; `updated_at` stays, and three other
   * readers of it are correct.
   */
  it("renders a deploy date from last_deployed_at wherever it says so", () => {
    const offenders: string[] = [];
    for (const file of sources([".ts", ".tsx"])) {
      const rel = relative(SRC, file);
      if (rel === "lib/api.ts") continue; // the type declaring both fields
      const src = readFileSync(file, "utf8");
      if (!/Last deployed/.test(src)) continue;
      if (src.includes("last_deployed_at")) continue;
      offenders.push(rel);
    }
    expect(offenders).toEqual([]);
  });
});

describe("the accessible half of a shared control", () => {
  /**
   * A row that does something when it is clicked can be reached without a mouse.
   *
   * A `<tr>` is not focusable and has no activation behaviour, so a bare
   * `<tr onClick>` is a control a keyboard cannot reach. Admin → Compliance
   * expanded every requirement and every piece of evidence that way — the one
   * page in this product built to be read by an assessor.
   *
   * Two ways to satisfy it, and the second is the reason this is not simply
   * "always use `rowActivation`". Where the row's click merely duplicates a
   * link inside it — the apps list, People, the storage inventory — the link
   * *is* the keyboard route, and it is the better one: it says where it goes,
   * a middle click opens it in a tab, and adding a tab stop on the row would
   * announce the same destination twice. Those rows say so in their own
   * comments. So: `rowActivation` (or `tabIndex` with `onKeyDown`), **or** an
   * interactive element inside the row.
   */
  it("gives every clickable row a keyboard route", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/<TableRow\b([^>]*)>([\s\S]*?)<\/TableRow>/g)) {
        const [, attrs, body] = m;
        if (!/\bonClick\b/.test(attrs)) continue;
        if (/\btabIndex\b/.test(attrs) || /rowActivation/.test(attrs)) continue;
        if (body.includes("<Link") || body.includes("<button")) continue;
        offenders.push(`${rel}:${src.slice(0, m.index).split("\n").length}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  /**
   * And a menu inside a clickable row keeps its clicks.
   *
   * A `⋯` menu's popup is portalled out of the table and into the body, so a
   * click on one of its items never touches the row in the DOM — but it still
   * bubbles to it through the *React* tree, which is where the row's `onClick`
   * is listening. Admin → Storage stopped propagation on the trigger and
   * nothing else, so opening the menu stayed put and then picking **Delete**
   * opened the confirm dialog *and* navigated to the resource's page, which
   * unmounted it. What an administrator saw was a Delete item that took them
   * somewhere else.
   *
   * Two ways to satisfy it, both already in use: stop propagation on the
   * `<TableCell>` holding the menu, which is above the whole thing in the
   * React tree — portal included — or stop it in every item's own handler,
   * which is what the apps list does because its items act rather than
   * navigate. Stopping it on the trigger alone satisfies neither: it covers
   * the click that opens the menu and no click inside it.
   */
  it("keeps a row menu's clicks out of the row's handler", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/<TableRow\b([^>]*)>([\s\S]*?)<\/TableRow>/g)) {
        const [, attrs, body] = m;
        if (!/\bonClick\b/.test(attrs) && !/rowActivation/.test(attrs)) continue;
        if (!body.includes("<DropdownMenuItem")) continue;
        // The cell that holds the menu, stopping the click for everything
        // under it. `[^>{]*` keeps this on the `<TableCell>`'s own attributes:
        // it cannot run past the tag into the trigger's `render={<Button …>}`,
        // which is the half that used to be mistaken for this one.
        if (/<TableCell[^>{]*onClick=\{\(e\) => e\.stopPropagation\(\)\}/.test(body)) continue;
        for (const item of body.matchAll(/<DropdownMenuItem\b([\s\S]*?)<\/DropdownMenuItem>/g)) {
          const chunk = item[1];
          if (!/\bonClick\b/.test(chunk)) continue;
          if (chunk.includes("stopPropagation")) continue;
          const line = src.slice(0, m.index! + item.index!).split("\n").length;
          offenders.push(`${rel}:${line}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });

  /**
   * And every field in a dialog has a name.
   *
   * A placeholder is not a label: it disappears on the first keystroke, and a
   * screen reader announces it as a hint or not at all. Three type-to-confirm
   * fields had one and nothing else — including `DeleteAppDialog`, the most
   * destructive act in the product and the one dialog that reaches
   * `api.deleteApp`.
   *
   * A `<Label htmlFor>` matching the field's `id` satisfies it, and so does an
   * `aria-label` — which is what a type-to-confirm field wants, since the
   * dialog's description has already said what to type and a second visible
   * label above the box would only repeat it.
   */
  it("names every input inside a dialog", () => {
    // A JSX tag cannot be matched with `[^>]*`: `onChange={(e) => …}` contains
    // a `>`. Walk to the `>` that closes the tag at brace depth zero.
    const tagAt = (s: string, i: number) => {
      let depth = 0;
      for (let j = i; j < s.length; j++) {
        const c = s[j];
        if (c === "{") depth++;
        else if (c === "}") depth--;
        else if (c === '"') while (++j < s.length && s[j] !== '"');
        else if (c === ">" && depth === 0) return s.slice(i, j + 1);
      }
      return s.slice(i);
    };
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const d of src.matchAll(/<DialogContent\b/g)) {
        const end = src.indexOf("</DialogContent>", d.index);
        const body = src.slice(d.index, end < 0 ? src.length : end);
        for (const t of body.matchAll(/<(?:Input|Textarea)\b/g)) {
          const tag = tagAt(body, t.index!);
          if (/aria-label(?:ledby)?[=\s]/.test(tag)) continue;
          const id = /\bid=("[^"]*"|\{[^}]*\})/.exec(tag);
          if (id && body.includes(`htmlFor=${id[1]}`)) continue;
          const line = src.slice(0, d.index! + t.index!).split("\n").length;
          offenders.push(`${rel}:${line}  ${tag.split("\n")[0].trim()}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("narrowing a list", () => {
  /**
   * A filter over a list is `SearchInput`, never a bare `<Input>`.
   *
   * `SearchInput` carries the two halves that keep getting left off: a × and
   * Escape that clear **without waiting** for the debounce, and the debounce
   * itself. Admin → Reach filtered a 52-row table with a raw input and told the
   * reader, in its own empty state, to "clear the filter" — with nothing to
   * clear it with. Compliance did the same one page over.
   *
   * The rule is scoped by the placeholder, because that is the only thing that
   * tells a filter from the other inputs on the page: a `<Input>` whose
   * placeholder says *filter* is narrowing something. It does not catch a
   * picker — "Search people and groups…" populates a results list rather than
   * narrowing one on screen, and `DirectoryPicker` is that control — so the
   * word `filter` is the whole test.
   */
  it("narrows a list with SearchInput", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      if (rel === "components/search-input.tsx") continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/<Input\b[\s\S]{0,400}?\/>/g)) {
        if (!/placeholder=["{`][^>]{0,80}?[Ff]ilter/.test(m[0])) continue;
        offenders.push(`${rel}:${src.slice(0, m.index).split("\n").length}`);
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("the measure", () => {
  /**
   * Every component that draws explanatory copy bounds it, and they all bound
   * it to the same thing.
   *
   * `max-w-prose` is 65ch — 603px at 14px Geist, about 72 characters — and
   * `setting-row.tsx` has set it on all three of a setting's prose slots since
   * it was written. Three components that draw more prose than that one set
   * nothing, so the worst line in the product was a `Notice` body at 197
   * characters, at 12px, on the page an administrator opens to decide who can
   * get in.
   *
   * A grep rather than a paragraph, because a removed class is invisible in
   * review, invisible to `tsc`, and shows up as a 197-character line on a page
   * nobody opened that sprint. The count is the check: it catches the slot
   * that loses its bound while its neighbour keeps one. A slot counts as
   * bounded when it either writes `max-w-prose` or renders a `FieldHelp` /
   * `FieldError`, which is where the sentence under a field gets it.
   *
   * The measure lives in these components and never in a page. A measure
   * written thirty times is a measure that will disagree with itself within a
   * month — which is how this product came to have sixty hand-written notices
   * before `Notice` existed, and about a hundred and twenty unbounded
   * paragraphs under form fields before `FieldHelp` did.
   */
  it("bounds every prose slot the shared components draw", () => {
    const required: Array<{ file: string; slots: number; what: string }> = [
      { file: "components/page-header.tsx", slots: 1, what: "the page description" },
      { file: "components/section.tsx", slots: 2, what: "SectionHeading's and Section's" },
      { file: "components/notice.tsx", slots: 2, what: "the title and the body" },
      { file: "components/setting-row.tsx", slots: 3, what: "a setting's three" },
      { file: "components/field-help.tsx", slots: 2, what: "FieldHelp's and FieldError's" },
    ];
    const missing: string[] = [];
    for (const { file, slots, what } of required) {
      const src = withoutComments(readFileSync(join(SRC, file), "utf8"));
      const found =
        (src.match(/max-w-prose/g) ?? []).length +
        (src.match(/<Field(?:Help|Error)\b/g) ?? []).length;
      if (found < slots) {
        missing.push(`${file}  ${found} of ${slots} bounded (${what})`);
      }
    }
    expect(missing).toEqual([]);
  });
});

describe("one card surface", () => {
  /**
   * A raised panel with a heading over it is a `Section`, and there is one of
   * those.
   *
   * There were three. `Section` titles itself `text-sm font-semibold` over a
   * 12px description; shadcn's `CardTitle` titled itself 16px/500 over a 14px
   * one; and `admin/settings.tsx` carried a private `SectionCard` — a `Card`
   * with a `CardTitle`, a `CardDescription` and a divided `CardContent`, which
   * is `Section` with a different heading size — drawing seven of the page's
   * sections. Where two of them nested, on Admin → Authentication, the child's
   * heading came out two pixels larger than the heading of the region
   * containing it.
   *
   * `Card` itself stays and so do the files that use it untitled: it is the
   * primitive `Section` is built on and it is right for a panel with no
   * heading — the Gallery's item card, `stat-card`, `login`, the access gates.
   * What ends is a second and third way to write a heading over one.
   *
   * The grep is on the import, because a page that titles a raised panel with
   * `Card` type-checks, lints, renders, and is simply a second grammar that
   * nothing else can see.
   */
  it("titles a raised panel only with Section", () => {
    expect(report(scan(/\bCardTitle\b/, { skip: ["components/ui/"] }))).toEqual([]);
  });

  /**
   * And the header grammar goes with it. `CardHeader`, `CardTitle` and
   * `CardDescription` are the three parts that title a raised panel; `Card`
   * and `CardContent` are the untitled panel itself and stay, which is what
   * the Gallery's item card, `stat-card`, `login` and both access gates use
   * them for.
   *
   * On the import, because a page that assembles the header by hand
   * type-checks, lints and renders — and because it is what a copy of an
   * existing page starts by pasting.
   */
  it("imports no card header part outside components/ui/", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/import\s*\{([^}]*)\}\s*from\s*"@\/components\/ui\/card"/g)) {
        const parts = m[1]
          .split(",")
          .map((x) => x.trim())
          .filter((x) => /^Card(Header|Title|Description)$/.test(x));
        if (parts.length === 0) continue;
        offenders.push(`${rel}:${src.slice(0, m.index).split("\n").length}  ${parts.join(", ")}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  /**
   * The case the two greps above cannot see: a `Card` titled by a heading
   * written by hand. Admin → System info had three — `Card`, `CardContent
   * p-5` and a 14px `<h3>` under no `<h2>` — which imported nothing the
   * import grep forbids and rendered a fourth grammar beside the Sections on
   * the same page.
   */
  it("titles no Card with a hand-written heading", () => {
    // The Gallery's item card names the *item*, one of a grid of them — the
    // untitled card the comment above keeps — not a region of the page.
    //
    // `Section` *is* the titled card, so it is the one file allowed to title
    // a TableCard. The storage browser's detail panel names the selected file,
    // the same item-not-region case as the Gallery. The landing page may draw
    // decoration the app has none of (the toft-ui skill, outside-spa.md § The landing page).
    const allowed = new Set([
      "pages/gallery.tsx",
      "components/section.tsx",
      "pages/store-browser.tsx",
      "pages/landing.tsx",
    ]);
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/") || allowed.has(rel)) continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/<(Card|TableCard)[\s>]/g)) {
        const end = src.indexOf(`</${m[1]}>`, m.index);
        if (end < 0) continue;
        if (/<h[23][\s>]/.test(src.slice(m.index, end))) {
          offenders.push(`${rel}:${src.slice(0, m.index).split("\n").length}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("no hand-built surface", () => {
  /**
   * `rounded-lg border … bg-muted/30` is the card surface restated with a
   * tint, which the `bg-card` half of the rule above cannot see. About thirty
   * of them drifted in different paddings and radii; `TableCard flat` and
   * `Notice` are the two things such a box is ever trying to be.
   */
  const NOT_A_SURFACE: Record<string, string> = {
    "pages/dashboard.tsx": "the Mine / All segmented control (ADR §90) — a control's track, not a panel",
    "pages/app/logs-tab.tsx": "the Run / Build switch, drawn as the same segmented control as the apps list's scope",
    "components/layout/layout.tsx": "the command palette's trigger — a button showing its shortcut",
    "components/store-delete-dialog.tsx": "a well of the literal names about to be deleted, in mono",
    "pages/store-browser.tsx": "the same well of names, in the replace confirmation",
    "pages/login.tsx": "outside the Layout, reviewed and kept (the toft-ui skill, outside-spa.md § What was reviewed)",
    "pages/cli-sign-in.tsx": "outside the Layout, reviewed and kept (the toft-ui skill, outside-spa.md § What was reviewed)",
  };

  it("restates no bordered muted box", () => {
    // `(?<![:\w-])` keeps a hover or focus tint out of it: a control that
    // tints on hover is a control, not a surface.
    const violations = scan(/rounded-(?:md|lg|xl)\b[^"`]*\bborder\b[^"`]*(?<![:\w-])bg-muted\/\d+|(?<![:\w-])bg-muted\/\d+[^"`]*\brounded-(?:md|lg|xl)\b[^"`]*\bborder\b/, {
      skip: ["components/ui/", ...Object.keys(NOT_A_SURFACE)],
    });
    expect(report(violations)).toEqual([]);
  });

  /**
   * A field's error id comes from `fieldErrorId`, or the control and the
   * message can name two different ids and the link between them is silently
   * gone.
   */
  it("derives a field's error id with fieldErrorId", () => {
    expect(report(scan(/aria-describedby=\{[^}]*`\$\{[^}]+\}-error`/, { skip: ["components/field-help.tsx"] }))).toEqual([]);
  });

  /**
   * An SPA route is a `<Link>`. A plain `<a href="/admin/…">` reloads the
   * whole application — and inside a dialog it throws the form away.
   */
  it("links to an SPA route with Link", () => {
    expect(report(scan(/<a\s[^>]*href=["{`]+\/(?!api\/|apps\/\$|auth\/)[a-z]/))).toEqual([]);
  });
});

describe("a comment is not copy", () => {
  /**
   * A `//` line between two tags is JSX text, not a comment, and renders: one
   * sat in every row of /me/storage. The project has no eslint-plugin-react,
   * whose `jsx-no-comment-textnodes` is this rule.
   */
  it("puts no // comment between JSX tags", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      const lines = readFileSync(file, "utf8").split("\n");
      lines.forEach((line, i) => {
        if (!/^\s*\/\/ /.test(line)) return;
        let j = i - 1;
        while (j >= 0 && (lines[j].trim() === "" || /^\s*\/\//.test(lines[j]))) j--;
        const prev = j >= 0 ? lines[j].trimEnd() : "";
        if (/<[A-Za-z][^<]*[^=\-/]>$/.test(prev) && !/^\s*(?:return|if|const|let|\}|\))/.test(prev)) {
          offenders.push(`${rel}:${i + 1}`);
        }
      });
    }
    expect(offenders).toEqual([]);
  });
});

describe("a filter is not a setting", () => {
  /**
   * The brand fill belongs to the control that writes.
   *
   * `FilterChips` drew its selected chip as `variant="default"` — the filled
   * indigo — at `size="sm" h-7 text-xs` with `aria-pressed`. So does
   * `ChoiceButtons`, whose documented contract is that it **saves the moment
   * one is chosen**. They compiled to the same button. On People the role
   * chips are one; on Admin → Settings the execution-mode chips are the other;
   * an administrator meets both on the same afternoon and cannot tell by
   * looking which one costs something.
   *
   * Both greps below compile, lint and render either way, which is the whole
   * reason they are here.
   */
  it("gives a filter chip no brand fill", () => {
    const src = withoutComments(
      readFileSync(join(SRC, "components/filter-bar.tsx"), "utf8"),
    );
    // Neither control in this file has a filled state, so the literal is the
    // whole test — `variant={on ? "default" : "outline"}` is how it was
    // written, and a grep for `variant="default"` would have walked past it.
    const offenders = src
      .split("\n")
      .map((line, i) => ({ line, at: i + 1 }))
      .filter((x) => /"default"/.test(x.line))
      .map((x) => `components/filter-bar.tsx:${x.at}  ${x.line.trim()}`);
    expect(offenders).toEqual([]);
  });

  /**
   * And every chip says how many are behind it — unless the *server* is doing
   * the filtering, where a count per option would be a request per option.
   * Those three are named here so a fifth has to be argued for rather than
   * added.
   */
  it("counts every filter chip the page can count", () => {
    const SERVER_FILTERED = [
      // Keyset paged: a count would count the rows that happen to be loaded.
      "pages/admin/audit.tsx",
      // Request parameters: status, kind and the window are sent to the
      // server, so a count per option is a request per option.
      "pages/admin/automation.tsx",
      "pages/admin/publisher-requests.tsx",
      // A time window is a request parameter too — 7, 30 or 90 days is a
      // different read, not a narrowing of rows already loaded.
      "pages/admin/usage.tsx",
      "pages/admin/integration-log.tsx",
      "pages/app/usage-tab.tsx",
      "pages/admin/usage-audience.tsx",
    ];
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel === "components/filter-bar.tsx") continue;
      const src = withoutComments(readFileSync(file, "utf8"));
      for (const m of src.matchAll(/<FilterChips\b[\s\S]{0,600}?\/>/g)) {
        const line = src.slice(0, m.index).split("\n").length;
        // The accessible name is what the group is called; without it the
        // whole row is announced as "Filter".
        if (!/\blabel=/.test(m[0])) offenders.push(`${rel}:${line}  no label`);
        if (SERVER_FILTERED.includes(rel)) continue;
        // The options are either built in the prop or named by it. A bare
        // identifier is followed to its declaration in the same file, which is
        // where `dependencies-tab` and `people` assemble theirs.
        const prop = /options=\{([\s\S]*?)\}\s*\n/.exec(m[0])?.[1] ?? "";
        const named = /^\s*([A-Za-z_$][\w$]*)\s*$/.exec(prop)?.[1];
        const built = named
          ? new RegExp(`(?:const|let)\\s+${named}\\b[\\s\\S]{0,900}`).exec(src)?.[0] ?? ""
          : prop;
        if (!/\bcount:/.test(built)) offenders.push(`${rel}:${line}  no count`);
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("the sentence under a field", () => {
  /**
   * A field's help text is a `FieldHelp`, and its refusal is a `FieldError`.
   *
   * A hundred and thirty-seven of these were hand-written paragraphs in
   * fourteen different class strings for one thing, and none of them carried
   * the measure — so forty across the admin surface painted lines of 95 to 195
   * characters, because a paragraph in a form takes the width of the form and a
   * form is as wide as the card.
   *
   * The rule is scoped to a **field block**: the run of JSX from a
   * `<Label htmlFor>` (or a `<FieldLabel>`) to the next one. That is the shape
   * a copied form starts from, and a bare `<p>` in it type-checks, lints and
   * renders — which is why nothing else can see it. Prose elsewhere on a page
   * is not this rule's business: a table cell, a row's subtitle and a section's
   * closing line are all legitimately raw paragraphs.
   */
  it("renders a field's help as FieldHelp", () => {
    const offenders: string[] = [];
    for (const file of sources([".tsx"])) {
      const rel = relative(SRC, file);
      if (rel.startsWith("components/ui/")) continue;
      const lines = withoutComments(readFileSync(file, "utf8")).split("\n");
      const indent = (l: string) => l.length - l.trimStart().length;
      const seen = new Set<number>();
      for (let i = 0; i < lines.length; i++) {
        if (!/<(?:Label htmlFor|FieldLabel)\b/.test(lines[i])) continue;
        const depth = indent(lines[i]);
        // The field block is the run that stays inside the element holding the
        // label. The first line that dedents past it has closed that element,
        // and what follows is a sibling — the picker popup's "Searching…", a
        // table's empty state, the "Last sent" record under a form. Those are
        // paragraphs on their own account and this rule is not about them.
        for (let j = i + 1; j < lines.length; j++) {
          if (lines[j].trim() !== "" && indent(lines[j]) < depth) break;
          // A paragraph carrying its own padding is a *box's* content, not a
          // line under a field: every directory picker puts its Searching…,
          // its refusal and its no-matches inside the bordered results list
          // that sits under the search input, and those three are the list's
          // states rather than the input's help.
          if (/\bp-2\.5\b/.test(lines[j])) continue;
          if (
            !seen.has(j) &&
            /<p\b[^>]*className=(?:"[^"]*|\{`[^`]*)(?:text-muted-foreground|text-destructive|tone-\w+-fg|toneText)/.test(
              lines[j],
            )
          ) {
            seen.add(j);
            offenders.push(`${rel}:${j + 1}`);
          }
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

describe("a link out says nothing about where it came from", () => {
  /**
   * Every new-tab link carries `rel="noopener noreferrer"`.
   *
   * The documentation is a public site now, and the Documentation page is a
   * page of links to it: without `noreferrer` each click hands the docs host
   * this install's hostname and the path it was on — exactly what the install
   * otherwise never tells the outside (it makes no request to the docs host at
   * all). The same holds for every other link that leaves the product. All
   * fifteen already complied when this was written; the rule is what keeps the
   * sixteenth honest.
   */
  it("gives every target=_blank link noreferrer", () => {
    const violations: string[] = [];
    for (const file of sources([".tsx"])) {
      if (file.includes(".test.")) continue;
      const text = withoutComments(readFileSync(file, "utf8"));
      for (const m of text.matchAll(/<(a|Link)\b[^>]*?target="_blank"[^>]*?>/gs)) {
        if (!m[0].includes("noreferrer")) {
          const line = text.slice(0, m.index).split("\n").length;
          violations.push(`${relative(SRC, file)}:${line}`);
        }
      }
    }
    expect(violations).toEqual([]);
  });
});

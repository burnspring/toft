/**
 * Tone — the five things this product ever says about a state.
 *
 * Every badge, dot, banner, chip and bar draws from here, and nothing outside
 * this file names a palette colour for a state. Six components used to hold
 * their own copy of that table (`status-badge`, `status-dot`, `key-state-badge`,
 * `send-status`, the storage access chip, and a page's worth of inline
 * banners), and they had already drifted apart: a sleeping app was one grey as
 * a dot and a different grey as a badge, and "expired" was `destructive` in one
 * place and amber in another. The value of one table is not tidiness — it is
 * that a reader can learn the colours once.
 *
 * The tokens themselves live in `index.css`, so a tone is a CSS variable rather
 * than a Tailwind palette step, and changing what "attention" looks like is one
 * edit rather than sixty.
 *
 * What each one means, which is the part worth getting right:
 *
 * - `ok`        — it is doing what it should. Running, sent, approved, passed.
 * - `attention` — look at this. **Not a failure**: a build in progress, a key an
 *                 admin disabled, a store an app may overwrite. Reaching for it
 *                 to mean "broken" is what makes a warning colour stop working.
 * - `danger`    — it broke, or it is about to do something unrecoverable.
 * - `neutral`   — a recorded fact with no valence. Stopped, sleeping, skipped,
 *                 not applicable. An app that slept did what it was told to.
 * - `info`      — the platform did this, rather than a person. Auto-deploys,
 *                 promotions, a capability the install provides.
 *
 * ## `danger` and `--destructive` are both red, and they are not the same red
 *
 * There were two red vocabularies here — thirty-two tone uses against
 * seventy-two `*-destructive` ones — in two different colours
 * (`oklch 0.577 0.245 27.325` against `0.505 0.213 27.518`), with no rule
 * saying which was which. One site computed a `Tone` and then threw it away for
 * `text-destructive`, which is what a missing rule looks like from the inside.
 *
 * The rule, which is the one their actual use already implied:
 *
 * - **`--destructive` is the control red.** It belongs to a *control* and to
 *   what a person is about to do with it: `variant="destructive"` on a Button
 *   or a menu item, an `aria-invalid` ring, the message under a field that
 *   will not save, the dot on a pending bar counting fields in error. It lives
 *   in `components/ui/` and in a small, listed set of form code.
 * - **A tone is the state red.** It belongs to a *fact*: a badge, a dot, a
 *   banner, a heading over a region, a recorded failure reason, a count of
 *   things that broke. Anything a person reads rather than presses.
 *
 * `conventions.test.ts` holds the exception list, so a new `text-destructive`
 * has to be argued for in review rather than typed.
 */
export type Tone = "ok" | "attention" | "danger" | "neutral" | "info";

/**
 * A tinted surface with a matching edge and readable text: the chip and the
 * banner. Pair with `variant="outline"` on a Badge, which supplies the shape.
 */
export const toneSurface: Record<Tone, string> = {
  ok: "border-tone-ok-border bg-tone-ok-surface text-tone-ok-fg",
  attention: "border-tone-attention-border bg-tone-attention-surface text-tone-attention-fg",
  danger: "border-tone-danger-border bg-tone-danger-surface text-tone-danger-fg",
  neutral: "border-tone-neutral-border bg-tone-neutral-surface text-tone-neutral-fg",
  info: "border-tone-info-border bg-tone-info-surface text-tone-info-fg",
};

/** The filled form — a status dot, a bar, a chart series. */
export const toneSolid: Record<Tone, string> = {
  ok: "bg-tone-ok-solid",
  attention: "bg-tone-attention-solid",
  danger: "bg-tone-danger-solid",
  neutral: "bg-tone-neutral-solid",
  info: "bg-tone-info-solid",
};

/** Text alone, on the page's own background. */
export const toneText: Record<Tone, string> = {
  ok: "text-tone-ok-fg",
  attention: "text-tone-attention-fg",
  danger: "text-tone-danger-fg",
  neutral: "text-tone-neutral-fg",
  info: "text-tone-info-fg",
};

/** The border alone, for a panel that carries its tint some other way. */
export const toneBorder: Record<Tone, string> = {
  ok: "border-tone-ok-border",
  attention: "border-tone-attention-border",
  danger: "border-tone-danger-border",
  neutral: "border-tone-neutral-border",
  info: "border-tone-info-border",
};

/**
 * The same values as CSS colours rather than classes, for the two places that
 * cannot use a class: an inline `fill`/`stroke` on an SVG the browser draws
 * from an attribute, and a canvas. `sparkline` and `key-usage-chart` hardcoded
 * hex before this existed.
 */
export const toneVar: Record<Tone, string> = {
  ok: "var(--tone-ok-solid)",
  attention: "var(--tone-attention-solid)",
  danger: "var(--tone-danger-solid)",
  neutral: "var(--tone-neutral-solid)",
  info: "var(--tone-info-solid)",
};

/**
 * What a CPU reading means, as a tone.
 *
 * Here rather than in the page that draws it, because the bands are a judgement
 * about a reading and a second copy of them would be a second judgement. **No
 * reading gets no tone**: a green cell over a number nobody measured is the
 * reassuring half of the bug this column had.
 */
export function cpuTone(percent: number | undefined): Tone | undefined {
  if (percent === undefined) return undefined;
  if (percent >= 80) return "danger";
  if (percent >= 50) return "attention";
  return undefined;
}

/**
 * What a "how full is it" reading means, as a tone.
 *
 * Separate bands from `cpuTone`, because these are separate kinds of number. A
 * CPU reading is pressure — it rises and falls within a minute, and half of one
 * core's worth of queue is already worth noticing. A disk or a memory reading is
 * a reservoir filling: 60% is unremarkable and stays unremarkable, and the
 * question is only how close the lid is.
 *
 * The three host meters used to draw every reading in the brand colour, so a
 * saturated box and an idle one differed by bar length alone — which is a state
 * carried by size and nothing else, on the page an operator opens *because*
 * something is wrong.
 */
export function capacityTone(percent: number | undefined): Tone | undefined {
  if (percent === undefined) return undefined;
  if (percent >= 90) return "danger";
  if (percent >= 75) return "attention";
  return undefined;
}

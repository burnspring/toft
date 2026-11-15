import { TableCard } from "@/components/table-card";
import { HeadingLevel } from "@/lib/heading-level";
import { toneText, type Tone } from "@/lib/tone";
import { cn } from "@/lib/utils";

/**
 * Detail-page primitives, promoted from the user account record. A Section is
 * a titled card whose header explains consequences in muted copy; Fact is a
 * label/value pair for a facts row; SectionHeading is the same header standing
 * on its own, above something that is not a card. Use these on any detail page
 * instead of restating the chrome.
 *
 * They are the *only* way this product titles a raised panel. `CardTitle` and
 * a private `SectionCard` in `admin/settings.tsx` were the other two, and the
 * greps in `conventions.test.ts` are what keeps there being one.
 */

/**
 * A heading over a region of a page — a table, a group of controls, a set of
 * cards — with the muted line that says what the region is for.
 *
 * There were forty-six of these written by hand in six different ways:
 * `text-sm font-medium`, `text-sm font-semibold`, `text-sm font-medium
 * text-muted-foreground`, some with a `mt-1` description and some with
 * `mt-0.5`. Nobody chose between them; they are what a pattern looks like
 * before it has a name.
 *
 * Semibold and full-contrast, deliberately. The muted-grey variant made a
 * heading quieter than the body text underneath it, which inverts the one job
 * a heading has — and it is the description, not the heading, that is
 * supporting copy.
 *
 * **Three steps, and a parent is never smaller than its child.** A page title
 * is 24px/600, a region heading 18px/600, a sub-region 14px/600. The middle
 * step used to be 14px — the same size as the body under it and the same size
 * as a field label — which is how a 16px `CardTitle` came to outrank the
 * heading of the region containing it without anybody noticing. 18px is the
 * smallest step that reads as a landmark when scrolling a 5,000px page and the
 * largest that does not compete with the page title.
 *
 * **The description carries the measure.** `max-w-prose` is 65ch, which is
 * what `setting-row.tsx` has bounded a setting's prose to since it was
 * written; unbounded, these descriptions ran to 131 characters at 12px on a
 * 1440px viewport. The measure belongs in the component and never in the page
 * — a measure written thirty times is a measure that will disagree with itself
 * within a month, which is how this product got sixty hand-written notices.
 */
export function SectionHeading({
  title,
  description,
  actions,
  tone,
  level = 2,
}: {
  title: string;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  /**
   * Where this heading actually sits. Two by default, because most of them are
   * a page's own sections; three where the region is nested inside one.
   *
   * Nineteen headings were hand-rolled at `h3` with these exact classes rather
   * than use this component, and on the Access tab that produced four `h3`
   * siblings around one `h2` — visually identical, and announced to a screen
   * reader as though the odd one out were the parent of the other four. The
   * level was the thing they were working around, so it is a prop.
   */
  level?: 2 | 3;
  /**
   * A heading whose region carries a state, which in practice is one heading:
   * *Danger zone*. The app's Settings tab has had a red one since it had a
   * danger zone; a second surface that grew one should not have to hand-write
   * the colour to match.
   */
  tone?: Tone;
}) {
  const H = level === 3 ? "h3" : "h2";
  return (
    // Wraps: wide `actions` drop under the title rather than crushing it to a
    // word per line, which is what a non-wrapping row did on a phone.
    <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
      <div className="min-w-0">
        <H
          className={cn(
            "font-semibold text-foreground",
            level === 3 ? "text-sm" : "text-lg",
            tone && toneText[tone],
          )}
        >
          {title}
        </H>
        {description && (
          <p className={cn("mt-1 max-w-prose text-muted-foreground", level === 3 ? "text-xs" : "text-sm")}>{description}</p>
        )}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

export function Section({
  id,
  title,
  description,
  actions,
  className,
  titleHidden = false,
  children,
}: {
  /** An anchor, when a link from ⌘K or another page names this section. */
  id?: string;
  title: string;
  /**
   * The title is already on screen somewhere else — New app's step strip names
   * the step the card holds — so the header bar goes and the heading stays for
   * a reader moving by headings. A card titled twice in a row was the defect.
   */
  titleHidden?: boolean;
  description?: React.ReactNode;
  /** The one control the section is for — an Add, a Map, a Connect. */
  actions?: React.ReactNode;
  /** On the card, for a body that rules its own rows. The header's own
   *  border is unconditional, so `divide-y` here divides only the children. */
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <TableCard id={id} className={cn(id && "scroll-mt-20", className)}>
      {titleHidden ? <h2 className="sr-only">{title}</h2> : (
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 border-b border-border/60 px-4 py-3">
        <div className="min-w-0">
          <h2 className="text-lg font-semibold text-foreground">{title}</h2>
          {description && (
            <p className="mt-0.5 max-w-prose text-sm text-muted-foreground">{description}</p>
          )}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      )}
      {/* Its children sit under that h2, so an EmptyState or a QueryError in
          here is an h3 without anybody having to remember a prop. */}
      <HeadingLevel.Provider value={3}>{children}</HeadingLevel.Provider>
    </TableCard>
  );
}

export function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    // `min-w-0` because a grid item's minimum width is its content: one long
    // branch or folder name widened its column past the card instead of
    // wrapping inside it.
    <div className="min-w-0">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 text-sm text-foreground break-words">{children}</dd>
    </div>
  );
}

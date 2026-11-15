import type { ReactNode } from "react";

import { useHeadingLevel } from "@/lib/heading-level";

interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  description?: string;
  action?: ReactNode;
  /**
   * The heading level, when position cannot answer it.
   *
   * It is normally derived: `Section` draws an `h2` over its children and says
   * so through `HeadingLevel`, so an empty state inside one is an `h3` and one
   * that *is* the page's content is an `h2`. This was a prop with a default of
   * `h3`, used at 1 of 60 sites, which is why seventeen pages went `h1` → `h3`
   * whenever they were empty or broken. See `lib/heading-level.ts`.
   *
   * `h1` is for `/404`, where this empty state is the whole page.
   */
  as?: "h1" | "h2" | "h3";
  /**
   * Announce it. Off by default, and the same word for the same behaviour as
   * `Notice`'s.
   *
   * An empty list is the page's content, not news, so the sixty plain empty
   * states stay silent. A *failed read* is news — it appears because of the
   * navigation the person has just made — so `QueryError` turns this on, which
   * covers all seventy-five of them at once. Before this, a screen-reader user
   * who opened a page and was refused heard the page title and then silence:
   * the skeletons went, nothing replaced them audibly, and the page read as
   * still loading, forever.
   */
  live?: boolean;
  /**
   * `inline` is the sub-list inside a `Section` or a tab's second table: no
   * icon tile, a 14px title and a short pad, so "No runs yet" under a heading
   * does not take the vertical room of a page with nothing on it. `page` is
   * the whole content of a page or a tab.
   *
   * Derived when omitted, the way the heading level is: under a `Section`'s
   * `h2` it is a sub-list and so `inline`, and anywhere else it is `page`.
   * Forty-odd sites took the default, and every one inside a titled card drew
   * a 200px block with an icon tile in one region of a page.
   */
  size?: "page" | "inline";
}

export function EmptyState({ icon, title, description, action, as, live = false, size: sizeProp }: EmptyStateProps) {
  const level = useHeadingLevel();
  const size = sizeProp ?? (level >= 3 ? "inline" : "page");
  // An inline empty state is a sub-list under a heading, so its title is a
  // sub-region's `h3` even where no `Section` says so — under a bare
  // `SectionHeading` it was an `h2` drawn at 14px, a level too high.
  const Heading = as ?? (size === "inline" ? "h3" : (`h${level}` as "h2" | "h3"));
  return (
    <div
      role={live ? "status" : undefined}
      aria-live={live ? "polite" : undefined}
      className={
        size === "inline"
          ? "flex flex-col items-center justify-center py-6 text-center"
          : "flex flex-col items-center justify-center py-16 text-center"
      }
    >
      {icon && size === "page" && (
        <div className="mb-4 rounded-lg bg-muted p-4 text-muted-foreground">{icon}</div>
      )}
      <Heading
        className={
          size === "inline"
            ? "text-sm font-semibold text-foreground"
            : "text-lg font-semibold text-foreground"
        }
      >
        {title}
      </Heading>
      {description && (
        <p className="mt-1.5 max-w-sm text-sm text-muted-foreground">{description}</p>
      )}
      {action && <div className={size === "inline" ? "mt-3" : "mt-5"}>{action}</div>}
    </div>
  );
}

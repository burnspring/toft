import { cn } from "@/lib/utils";

/**
 * The product's standard surface: white, soft border, and — usually — a shadow.
 * Every page used to restate these classes inline; add chrome here, not in
 * pages.
 *
 * `flat` is the second shape, and it is a real distinction rather than a knob.
 * A raised card sits on the page background and says "this is a thing". A flat
 * one is a container *inside* something already raised — the grant list on the
 * Access tab, the member picker in a dialog, the tag rows in an editor — where
 * a second shadow reads as a card floating on a card.
 *
 * Both spellings were already in use across twenty-six hand-written copies of
 * this class string, and the split fell roughly along that line. It was never
 * decided, though, which is why four top-level sections on one admin page
 * were missing a shadow their neighbours had. Naming it makes the difference a
 * choice somebody makes rather than a class somebody forgets.
 */
export function TableCard({
  id,
  className,
  flat = false,
  children,
}: {
  /** An anchor, when a link from ⌘K or another page names this card. */
  id?: string;
  className?: string;
  /** No shadow: this sits inside something already raised. */
  flat?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div
      id={id}
      className={cn(
        "rounded-lg border border-border/60 bg-card",
        !flat && "shadow-sm",
        className,
      )}
    >
      {children}
    </div>
  );
}

import { timeAgo, formatDateTime } from "@/lib/time";

/**
 * Relative time, with the absolute timestamp available on hover.
 *
 * The `title` stays what it is — a mouse-only convenience — at all sixty-odd
 * call sites. Wrapping every one of them in a tooltip would insert a focusable
 * element into every dense table and every dropdown item in the product, and
 * would make an animated surface the most-rendered thing in it, in a product
 * whose motion budget is spent on the deploy timeline alone.
 *
 * `exact` is the cheap answer for the handful of places where *when precisely*
 * is the question a person came to the page with — an audit trail, a list of
 * sign-ins. It adds the absolute time for a screen reader without adding a tab
 * stop or a hover for anybody.
 */
export function RelativeTime({
  iso,
  className,
  exact = false,
}: {
  iso: string;
  className?: string;
  exact?: boolean;
}) {
  if (!iso) return <span className={className}>—</span>;
  return (
    <time dateTime={iso} title={formatDateTime(iso)} className={className}>
      {timeAgo(iso)}
      {exact && <span className="sr-only"> — {formatDateTime(iso)}</span>}
    </time>
  );
}

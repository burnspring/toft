import { Link } from "react-router-dom";

import { searchSettings, settingHref, settingPageLabel } from "@/lib/settings-index";

/** How many are named before the rest become a count. */
const SHOWN = 6;

/**
 * Where else this word is a setting.
 *
 * A page-scoped filter can only ever answer for its own page, and the one it
 * gave was "No setting by that name" — which is not "not here", it is "this
 * does not exist", said in the one place an administrator was taught to look.
 * Typing `smtp`, `otlp`, `budget`, `waiver` or `issuer` into Settings got that
 * answer about settings this install very much has.
 *
 * So the index answers instead. Each match is a link to `route#anchor`, which
 * lands on the setting rather than on the page that holds it.
 *
 * It renders nothing when there is nothing to say — a filter that found
 * everything it was looking for should not be followed by a line about
 * elsewhere.
 */
export function OtherPagesMatch({
  query,
  route,
  className,
}: {
  query: string;
  /** The page doing the filtering. Its own matches are not "elsewhere". */
  route: string;
  className?: string;
}) {
  const matches = searchSettings(query, route);
  if (matches.length === 0) return null;

  const shown = matches.slice(0, SHOWN);
  const rest = matches.length - shown.length;

  return (
    <p className={`text-sm text-muted-foreground ${className ?? ""}`} aria-live="polite">
      {matches.length} setting{matches.length === 1 ? "" : "s"} on other pages match “{query}”:{" "}
      {shown.map((entry, i) => (
        <span key={entry.id}>
          {i > 0 && ", "}
          <Link
            to={settingHref(entry)}
            className="rounded-sm text-primary hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            {entry.label}
          </Link>{" "}
          <span className="text-xs">({settingPageLabel(entry.route)})</span>
        </span>
      ))}
      {rest > 0 && <>, and {rest} more</>}.
    </p>
  );
}

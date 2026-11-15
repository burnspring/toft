import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { ArrowLeft } from "lucide-react";

/**
 * The one-level breadcrumb every detail page carries: *parent* / **this**.
 *
 * Four pages had written the same markup three different ways, and the busiest
 * one had written it wrong. On the app page — the most-visited destination in
 * the product — the crumb was a `<button onClick={navigate}>`, so it could not
 * be middle-clicked into a background tab, could not be ⌘-clicked to keep the
 * page you were on, showed no destination in the status bar on hover, and had
 * no right-click menu to copy the address from. The argument against exactly
 * that is written on the apps list, about the same control.
 *
 * So it is a component, and the next detail page cannot fork a fifth copy.
 *
 * `current` is optional on purpose. A page renders the crumb above **all** of
 * its branches — the failed read and the skeleton included — because on a route
 * that is reachable only from one row of one table, the crumb is the only exit
 * the page ever names, and those are the two states in which a reader is most
 * stuck. In those branches there is no name to put after the slash yet.
 */
export function ParentCrumb({
  to,
  parent,
  current,
}: {
  to: string;
  parent: string;
  current?: ReactNode;
}) {
  return (
    <div className="flex items-center gap-2 text-sm text-muted-foreground">
      <Link to={to} className="flex items-center gap-1 transition-colors hover:text-foreground">
        <ArrowLeft className="h-3.5 w-3.5" />
        {parent}
      </Link>
      {current !== undefined && (
        <>
          <span>/</span>
          <span className="font-medium text-foreground">{current}</span>
        </>
      )}
    </div>
  );
}

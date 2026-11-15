import { useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

/**
 * Says where you have arrived, for people who cannot see that they have.
 *
 * A full page load announces itself: the browser reads the new document. A
 * single-page navigation does not — React swaps the DOM under a screen reader
 * that was told nothing, so following a link appears to do nothing at all, and
 * the reader is left wherever the old page had put it.
 *
 * The announcement is the page's own title, read one tick after the route
 * changes so that `PageHeader` has had its effect and set it. Not on first
 * mount: the document load already did that, and announcing twice is worse
 * than not at all.
 *
 * Deliberately not focus management. Moving focus to the heading on every
 * navigation is the other common answer, and on a console where people
 * navigate with ⌘K and keep typing it takes the caret out from under them.
 * An announcement tells you where you are without taking anything.
 *
 * # Where this must be mounted, and why it is not a detail
 *
 * Once, in `App.tsx`, above `Routes` — never inside a layout.
 *
 * The guard below is per *component instance*, so it is only quiet on the
 * first render if this component survives every navigation. It did not: the
 * app mounts `Layout` from two places — `RootRoute` for `/`, and the
 * `ProtectedRoute` wrapper for everything else — and React treats those as
 * different positions in the tree, so moving between them unmounts one layout
 * and mounts the other. An announcer living inside `Layout` was remounted
 * exactly on the navigations the top nav makes, `first.current` went back to
 * `true`, and the announcement was swallowed. Navigation *within* a section
 * worked the whole time, which is what made it look like the effect rather
 * than the mount point.
 *
 * `tests/e2e` SPA.13 asserts both halves — within a section and across two
 * top-level destinations — and it is what found this.
 */
export function RouteAnnouncer() {
  const { pathname } = useLocation();
  const [message, setMessage] = useState("");
  const first = useRef(true);

  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    // A tick later: the new page's title effect has not run yet on this commit.
    const t = setTimeout(() => setMessage(document.title), 100);
    return () => clearTimeout(t);
  }, [pathname]);

  return (
    <div aria-live="polite" aria-atomic="true" className="sr-only">
      {message}
    </div>
  );
}

import { useEffect, useRef } from "react";
import { useLocation, useNavigationType } from "react-router-dom";

/**
 * Where a route starts, and where a Back lands.
 *
 * The SPA never touched the scroll offset, so a click from the bottom of a long
 * apps list into an app opened that app's page already scrolled halfway down
 * it — past the header, the status and the banners the page exists to put in
 * front of somebody. The browser does this for a document navigation and does
 * not for a client-side one, and the absence reads as a rendering fault.
 *
 * Two rules, because they are two different acts:
 *
 * - **Going somewhere new starts at the top.** A push or a replace is a fresh
 *   destination and has no remembered position to be at.
 * - **Going back returns to where you were.** The offsets are remembered per
 *   history entry, so returning from an app to the list you left puts the row
 *   you clicked back under the cursor.
 *
 * A hash is left alone in both directions: `useHashScroll` owns that, and the
 * two moving the same page would race. A change to the query string alone is
 * left alone too — that is a filter being narrowed, and yanking the reader to
 * the top for it is the same fault in the other direction.
 */
export function useScrollRestoration() {
  const location = useLocation();
  const navigationType = useNavigationType();
  const positions = useRef(new Map<string, number>());
  const previousKey = useRef<string | null>(null);
  const previousPath = useRef<string | null>(null);

  useEffect(() => {
    // Record where the entry we are leaving was, before moving anything.
    if (previousKey.current !== null) {
      positions.current.set(previousKey.current, window.scrollY);
    }
    const key = location.key;
    const samePath = previousPath.current === location.pathname;
    previousKey.current = key;
    previousPath.current = location.pathname;

    if (location.hash) return;
    if (samePath) return;

    if (navigationType === "POP") {
      const remembered = positions.current.get(key);
      window.scrollTo({ top: remembered ?? 0, behavior: "instant" });
      return;
    }
    window.scrollTo({ top: 0, behavior: "instant" });
  }, [location.key, location.pathname, location.hash, navigationType]);
}

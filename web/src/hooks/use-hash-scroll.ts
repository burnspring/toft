import { useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

/**
 * Honour `#anchor` on arrival, once the content it names exists.
 *
 * The browser resolves a hash before React has rendered anything, and an admin
 * page's sections only exist once its query resolves — so a link from another
 * page lands somebody at the top of a long form and leaves them to find the row
 * they were sent for. This is what makes `route#anchor` a real destination, and
 * `route#anchor` is what the settings index promises: every setting on this
 * install is reachable by name from ⌘K, whichever page renders it.
 *
 * `ready` is the page's own signal that the anchor could exist — usually
 * `!!data`. Keyed on the hash rather than a once-flag, so a second link to a
 * different anchor within the same visit still moves, and deliberately not
 * re-fired on every render, which would fight the person scrolling.
 *
 * **`ready` is a permission to start looking, not a promise the anchor is
 * there.** A page whose sections come from several queries has no single
 * signal: the app detail page passes `!!app`, and its Access tab and its
 * storage block are still in flight at that moment — so the one check this
 * used to make found nothing, returned, and never ran again. It now watches
 * for the element to appear, for a bounded spell, and gives up rather than
 * moving somebody who has started reading.
 */
const ANCHOR_WAIT_MS = 4000;

export function useHashScroll(ready: boolean = true) {
  const { hash } = useLocation();
  const jumpedTo = useRef("");

  useEffect(() => {
    const id = hash.replace(/^#/, "");
    if (!ready || !id || jumpedTo.current === id) return;

    const deadline = Date.now() + ANCHOR_WAIT_MS;
    let frame = 0;
    const look = () => {
      const el = document.getElementById(id);
      if (!el) {
        if (Date.now() < deadline) frame = requestAnimationFrame(look);
        return;
      }
      jumpedTo.current = id;
      const smooth = !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      el.scrollIntoView({ behavior: smooth ? "smooth" : "auto", block: "start" });
    };
    frame = requestAnimationFrame(look);
    return () => cancelAnimationFrame(frame);
  }, [ready, hash]);
}

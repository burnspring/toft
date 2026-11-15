import { useEffect } from "react";

/**
 * Warn before a reload or a close would throw away work in flight.
 *
 * The browser's own dialog, which is the only one that can appear here: a
 * `beforeunload` handler may ask, and may not choose. The wording is the
 * browser's too — every one of them ignores whatever string is returned — so
 * this takes a reason for the *reader of the code* rather than for the person.
 *
 * It covers a reload, a close and a typed URL. It does not cover a click on a
 * link inside the SPA, which is not an unload at all; where that matters the
 * page has to say so itself.
 */
export function useUnloadGuard(active: boolean, reason: string) {
  useEffect(() => {
    if (!active) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Assigning `returnValue` is what still arms the prompt in Chrome and
      // Safari; `preventDefault` alone is the standard and is not yet enough.
      e.returnValue = reason;
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [active, reason]);
}

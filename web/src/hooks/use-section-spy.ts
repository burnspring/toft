import { useEffect, useState } from "react";

/**
 * Which of a page's sections you are currently standing in.
 *
 * The rail beside a long page is only navigation while it marks where you are,
 * and "where you are" is a scroll position rather than a route — so it is read
 * from the DOM rather than derived from state. 96px is the header's height plus
 * a little: a heading is "current" from the moment it reaches the bottom of the
 * sticky bar rather than the top of the window.
 *
 * Lived inside the app's Settings tab until the documentation grew a rail too.
 */
export function useSectionSpy(ids: string[], onActive: (id: string) => void) {
  // Joined rather than passed as an array: a new array each render would re-arm
  // the listener on every one of them.
  const key = ids.join(",");
  useEffect(() => {
    const list = key ? key.split(",") : [];
    if (list.length === 0) return;
    function onScroll() {
      let current = list[0];
      for (const id of list) {
        const el = document.getElementById(id);
        if (el && el.getBoundingClientRect().top <= 96) current = id;
      }
      onActive(current);
    }
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
    // `onActive` is a setState, stable for the life of the component.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
}

/**
 * Which of a rail's sections are actually on the page.
 *
 * The rail's heading is "On this page", and it was rendering a static table:
 * the app's Access tab listed **Platform lock** and **Waiting for you** to a
 * reader who has neither — an owner who is not an administrator on an unlocked
 * app, with nobody waiting — and clicking either did nothing at all, because
 * `SectionRail`'s own jump returns early when the element is absent. A link
 * that silently does nothing is worse than an absent one: it reads as the page
 * being broken rather than as the section not existing.
 *
 * Read from the DOM rather than from a second copy of each section's
 * condition. The conditions live at the call sites — `can_approve_access_requests`,
 * an administrator on an unlocked app, an install with no groups — and a rail
 * that re-derived them would be the exact second definition that drifts.
 *
 * A `MutationObserver` because sections appear as their queries land, well
 * after the first render, and the rail must not be a snapshot of the page
 * before its contents arrived.
 */
export function usePresentSections(ids: string[]): Set<string> {
  const key = ids.join(",");
  const [present, setPresent] = useState<Set<string>>(new Set());
  useEffect(() => {
    const list = key ? key.split(",") : [];
    if (list.length === 0) return;
    let pending: ReturnType<typeof setTimeout> | 0 = 0;
    function read() {
      pending = 0;
      const found = new Set(list.filter((id) => document.getElementById(id)));
      setPresent((prev) =>
        prev.size === found.size && [...found].every((id) => prev.has(id)) ? prev : found,
      );
    }
    read();
    const observer = new MutationObserver(() => {
      // Coalesced onto a timeout, **not** `requestAnimationFrame`: rAF does
      // not run in a backgrounded tab or in a headless browser that is not
      // compositing, and this is a correctness read rather than an animation.
      // Measured: with rAF the rail went on offering a section that had
      // unmounted, which is the exact defect this hook exists to end.
      if (pending === 0) pending = setTimeout(read, 0);
    });
    observer.observe(document.body, { childList: true, subtree: true });
    return () => {
      observer.disconnect();
      if (pending) clearTimeout(pending);
    };
  }, [key]);
  return present;
}

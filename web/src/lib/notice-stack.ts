import type { ReactNode } from "react";

/**
 * One notice in an app's stack, with the one thing that decides its place.
 *
 * `serving` is *is this app still doing its job?* — false for a crashed app,
 * for a build in flight, and for a deploy that never shipped; true for every
 * condition that is a fact about an app which is up. It is the only ordering
 * input, because the alternative is a priority number per condition and an
 * eighth condition arriving with an argument about where it goes.
 */
export interface StackedNotice {
  key: string;
  serving: boolean;
  node: ReactNode;
  /**
   * The *event* this notice is about, where more than one notice can be about
   * the same one.
   *
   * The ceiling counts events, not notices. A failed deploy raises two — the
   * console, which lingers for whoever was watching, and the banner naming the
   * consequence for everybody who was not — and with a ceiling of two that one
   * event took both slots, collapsing every notice that carries a way out: the
   * source this install no longer deploys from, the variables the workload was
   * not started with, the deploys the dependency policy is refusing. The two
   * do not repeat each other, so neither should go; what they should not do is
   * crowd out a *different* fact.
   *
   * Absent means the notice is its own event, which is all but two of them.
   */
  about?: string;
}

/** How many are on screen before the rest become a line. */
export const NOTICE_CEILING = 2;

/**
 * The order and the ceiling, as a function rather than as JSX.
 *
 * Here rather than in the component because it is the part with a rule in it,
 * and `npm test` runs without a DOM (ADR §50) — so the thing worth asserting
 * has to be assertable without one.
 *
 * The sort is **stable**: within "the app is down" and within "the app is up",
 * the order the caller declared is the deliberate one — the dependency notices
 * go last because neither means the app stopped working, and the malware
 * banner suppresses the blocked-deploys one so that neither gets scrolled past
 * — and none of that is renegotiated here.
 *
 * **The ceiling counts events.** Two notices sharing an `about` are one thing
 * that happened, and they cost one slot between them; see `StackedNotice`.
 */
export function orderNotices(
  notices: (StackedNotice | false | undefined | null)[],
  expanded = false,
): { shown: StackedNotice[]; hidden: number } {
  const live = notices.filter((n): n is StackedNotice => !!n);
  const ordered = [...live].sort((a, b) => Number(a.serving) - Number(b.serving));
  if (expanded) return { shown: ordered, hidden: 0 };

  const shown: StackedNotice[] = [];
  const events = new Set<string>();
  for (const n of ordered) {
    const event = n.about ?? n.key;
    if (!events.has(event) && events.size >= NOTICE_CEILING) continue;
    events.add(event);
    shown.push(n);
  }
  return { shown, hidden: ordered.length - shown.length };
}

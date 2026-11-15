import { createContext, useContext } from "react";

/**
 * What heading level the thing you are about to render sits under.
 *
 * `EmptyState` — and `QueryError`, which renders through it — had a prop for
 * this and it was used at 1 of 60 call sites. So seventeen pages jumped from
 * the `h1` in their header straight to `h3` the moment they were empty or
 * broken, and every one of the seventy-five error surfaces in the product was
 * an `h3` under an `h1`. A heading outline with a gap in it is what a screen
 * reader's landmark list is built from, and asking sixty call sites to
 * remember a prop is what produced the gap.
 *
 * So it is not a prop any more, it is a fact about position. `Section` — the
 * one component that draws an `h2` over its own children — provides `3`, and
 * everything else is at `2` because it is the page's own content. The prop is
 * still there for the two cases position cannot answer: `/404`, whose empty
 * state *is* the page's `h1`, and a heading drawn by hand.
 */
export const HeadingLevel = createContext<2 | 3>(2);

export function useHeadingLevel(): 2 | 3 {
  return useContext(HeadingLevel);
}

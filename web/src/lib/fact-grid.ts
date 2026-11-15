/**
 * How many columns a grid of fact tiles takes, from the number of tiles that
 * will actually render.
 *
 * The rule was written once, inside the app page's hub-tile grid, and not
 * applied to the process stat row two hundred lines above it — which declared
 * four columns whatever it had, so a shared-mode app, whose backend reports no
 * CPU, sat three tiles in a four-column grid with a third of a raised card
 * empty beside them. A lone tile is the same problem and is what the original
 * comment was about: a third of a row with two thirds of nothing beside it,
 * reading as a card that had failed to load its neighbours.
 *
 * A module of its own rather than a helper in the page, for the reason
 * `buttonVariants` is: a `.tsx` file that exports both a component and a plain
 * value cannot hot-reload as a unit. It is also the only shape a table of
 * cases can test.
 *
 * **The mobile base belongs to the caller.** A stat card carries a figure and
 * a hub tile carries a sentence, and two of each across a phone is right for
 * one and wrong for the other — so this returns the `sm`-and-up ladder only.
 */
export function statColumns(n: number): string {
  if (n <= 1) return "";
  if (n === 2) return "sm:grid-cols-2";
  if (n === 3) return "sm:grid-cols-2 lg:grid-cols-3";
  return "sm:grid-cols-2 lg:grid-cols-4";
}

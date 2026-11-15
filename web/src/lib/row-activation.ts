import type { KeyboardEvent } from "react";

/**
 * A table row that does something when it is clicked, made reachable without a
 * mouse.
 *
 * Four tables in this product open a detail, expand a panel or select a row by
 * clicking anywhere on it, and three of them had no keyboard route to that at
 * all — the row was a `<tr>` with an `onClick`, which is not focusable and has
 * no activation behaviour. Tab went straight past them to whatever link the row
 * happened to contain, and on a row with no link there was nothing.
 *
 * The row stays a row: no `role="button"`, because a `<tr>` announced as a
 * button takes its cells' column headers with it and the table stops being one.
 * What it gains is a tab stop, an accessible name saying what activating it
 * does, and Enter/Space — which is what a focusable row is expected to answer.
 *
 * Spread onto the `<TableRow>`. Anything inside it that is itself interactive
 * still needs its own `stopPropagation`, exactly as it did for the click.
 */
export function rowActivation(label: string, activate: () => void) {
  return {
    className: "cursor-pointer",
    tabIndex: 0,
    "aria-label": label,
    onClick: activate,
    onKeyDown: (e: KeyboardEvent) => {
      // Only when the row itself has focus: Enter inside a cell's own input or
      // menu belongs to that control, not to the row around it.
      if (e.target !== e.currentTarget) return;
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        activate();
      }
    },
  };
}

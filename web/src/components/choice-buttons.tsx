import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";

export interface Choice<T extends string> {
  value: T;
  label: string;
  /** One line under the row, for the option that is chosen. */
  hint: string;
  /** This option alone cannot be picked here. Disabled rather than hidden: an
   *  administrator reading a row is entitled to know the setting has a second
   *  value, and the reason belongs in a `Notice` beside it — never a `title`,
   *  which a disabled control cannot deliver to anybody. */
  disabled?: boolean;
}

/**
 * A small set of named values, as buttons rather than a dropdown.
 *
 * Used where there are exactly two or three options, each needs a sentence, and
 * a menu would hide the others behind a click — the gallery's three modes, the
 * dependency policy's off/warn/block.
 *
 * It saves the moment one is chosen, like a switch and for the same reason: it
 * is a single deliberate act, and the consequence the server answers with
 * belongs to the act that caused it, not to a bar six edits later.
 *
 * The hint belongs to the *chosen* option, not to all of them. Three sentences
 * stacked under three buttons is a paragraph nobody reads; one sentence saying
 * what the current setting means is the thing an administrator came for.
 *
 * **Which one is chosen is `aria-pressed`, not the fill.** A filled button
 * beside two outlined ones is a colour and a shape carrying the whole state,
 * which is exactly what this product's rules say a state may never be — and
 * here it is the state the control exists to report.
 */
export function ChoiceButtons<T extends string>({
  label,
  value,
  options,
  disabled,
  note,
  onChange,
}: {
  /** The group's accessible name — what is being chosen. */
  label: string;
  value: T;
  options: Choice<T>[];
  disabled?: boolean;
  /** Said instead of the chosen option's hint, where the sentence is about
   *  something other than the choice — the log level's is about the level
   *  actually in effect, which the install's own setting can decide. */
  note?: ReactNode;
  onChange: (v: T) => void;
}) {
  const chosen = options.find((o) => o.value === value);
  return (
    <div className="flex flex-col items-start gap-1.5 sm:items-end">
      <div role="group" aria-label={label} className="flex flex-wrap gap-1.5">
        {options.map((o) => (
          <Button
            key={o.value}
            size="sm"
            variant={o.value === value ? "default" : "outline"}
            className="h-7 text-xs"
            disabled={disabled || o.disabled}
            aria-pressed={o.value === value}
            onClick={() => o.value !== value && onChange(o.value)}
          >
            {o.label}
          </Button>
        ))}
      </div>
      {note ?? (chosen && <span className="text-xs text-muted-foreground">{chosen.hint}</span>)}
    </div>
  );
}

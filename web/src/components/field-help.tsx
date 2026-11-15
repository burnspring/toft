import type { ReactNode } from "react";

import { toneText, type Tone } from "@/lib/tone";
import { cn, fieldErrorId } from "@/lib/utils";

/**
 * The sentence under a field.
 *
 * Every form in this product has them — what the value means, what happens if
 * you leave it blank, which environment variable it overrides — and every one
 * of them was written by hand as `<p className="text-xs text-muted-foreground">`.
 * Which is fine until you measure it: forty of them across the admin surface
 * painted lines of 95 to 195 characters, because a `<p>` in a form takes the
 * width of the form and a form is as wide as the card.
 *
 * So this carries the measure, which is the same `max-w-prose` the five
 * components that draw explanatory copy already carry (ADR §100). It is the
 * only reason it exists: a class written two hundred and seventy-six times is
 * not a convention, it is two hundred and seventy-six chances to leave one off.
 *
 * **A tone is for a consequence, never for a refusal.** Absent is the muted
 * default and is what nearly every field wants. `attention` is for a sentence
 * that says what this setting is *doing* to somebody right now — "1 app would
 * stop being reachable" — which is a fact about the install and therefore a
 * tone. A field that will not save is a `FieldError`, in the control red, for
 * the reason `lib/tone.ts` gives: `--destructive` belongs to a control and to
 * what a person is about to do with it, and a tone belongs to a fact they read.
 *
 * `id` is optional and should be set wherever the control can point at it with
 * `aria-describedby`. It is not wired here because the attribute belongs to the
 * input, not to the paragraph.
 */
export function FieldHelp({
  id,
  tone,
  className,
  children,
}: {
  id?: string;
  /** A consequence rather than an explanation. Rare, and never for a refusal. */
  tone?: Tone;
  className?: string;
  children?: ReactNode;
}) {
  // Nothing to say renders nothing, so a caller can pass an optional string
  // straight in rather than guarding it — which is where a stray empty
  // paragraph with margin above it comes from.
  if (children === undefined || children === null || children === false) return null;
  return (
    <p
      id={id}
      className={cn(
        "max-w-prose text-xs",
        tone ? toneText[tone] : "text-muted-foreground",
        className,
      )}
    >
      {children}
    </p>
  );
}

/**
 * Why this field will not save, under the field it is about.
 *
 * The control red, because this is the one line under a field that is about
 * something the person is doing rather than something the install is —
 * `lib/tone.ts` names it explicitly, and `conventions.test.ts` had nine files
 * on its "destructive is a control" exception list whose stated reason was
 * some wording of *a field's own validation message*. One component is what
 * takes them off it.
 *
 * **It derives its own `id` from the field's**, so the control writes
 * `aria-describedby={error ? fieldErrorId(id) : undefined}` and the two cannot
 * drift apart. `fieldErrorId` is in `lib/utils.ts` rather than here for the
 * reason `buttonVariants` is not in `button.tsx`: a file exporting both a
 * component and a plain function cannot hot-reload as a unit. A refusal announced with no `aria-describedby` is a message a
 * screen reader reaches only by accident, and the reader who mistyped one of
 * three numbers is left working out which.
 */
export function FieldError({
  field,
  className,
  children,
}: {
  /** The id of the control this is about. */
  field: string;
  className?: string;
  children?: ReactNode;
}) {
  if (children === undefined || children === null || children === false) return null;
  return (
    <p id={fieldErrorId(field)} className={cn("max-w-prose text-xs text-destructive", className)}>
      {children}
    </p>
  );
}

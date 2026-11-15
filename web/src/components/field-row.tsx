import type { ReactNode } from "react";

import { Label } from "@/components/ui/label";
import { FieldHelp } from "@/components/field-help";

/**
 * The one shape of a setting: its name and one help line on the left, the
 * control in a fixed column on the right.
 *
 * The app's Settings tab stacked a label over a full-width field while Admin →
 * Settings put the field to the right, and Admin's right column was whatever
 * width each control happened to be — so `w-72` and `w-44` inputs started at
 * two different x positions down one card. A fixed column gives every text
 * field the same left edge and every switch the same right edge, on both
 * surfaces. Below `sm` the two stack, which is the only width at which a
 * column cannot be spared.
 */
export const FIELD_ROW =
  "grid gap-x-8 gap-y-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,20rem)] sm:items-start";
/** The control's cell. Short controls (a switch, a number) sit at its right
 *  edge; a text field takes the whole column. */
export const FIELD_CONTROL = "flex min-w-0 sm:justify-end sm:pt-0.5";

export function FieldRow({
  label,
  htmlFor,
  help,
  extra,
  children,
}: {
  label: ReactNode;
  htmlFor: string;
  help?: ReactNode;
  /** Anything else under the help line — a note, an error, a disclosure. */
  extra?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className={FIELD_ROW}>
      <div className="min-w-0 space-y-1">
        <Label htmlFor={htmlFor} className="text-sm font-medium">
          {label}
        </Label>
        {help && <FieldHelp>{help}</FieldHelp>}
        {extra}
      </div>
      <div className={FIELD_CONTROL}>{children}</div>
    </div>
  );
}

import { useContext, useState, type ReactNode } from "react";
import { CircleHelp } from "lucide-react";

import { Label } from "@/components/ui/label";
import { FieldHelp } from "@/components/field-help";
import { FilterTerm } from "@/lib/filter-term";
import { SETTING, type SettingEntryId } from "@/lib/settings-index";
import { type Tone } from "@/lib/tone";
import { cn } from "@/lib/utils";
import { FIELD_CONTROL, FIELD_ROW } from "@/components/field-row";

/**
 * One setting: what it is on the left, the control on the right, and the
 * reasoning folded away underneath. The label is a real `<label>` for the
 * control's id, so the whole name is a click target for the switch.
 */
export function SettingRow({
  id,
  visible = true,
  note,
  noteTone,
  children,
}: {
  id: SettingEntryId;
  visible?: boolean;
  /** The blast radius, in a sentence. A `ReactNode` rather than a string
   *  because a note that states a count often owes the reader a way to *open*
   *  that count — see the anonymous-access row, whose figure is also on
   *  Admin → Reach. */
  note?: ReactNode;
  /**
   * The note's colour, as a **tone** rather than a word of its own.
   *
   * It was `"muted" | "warning"`, which is a second vocabulary for the thing
   * `lib/tone.ts` already names — and "warning" is not one of the five, so the
   * one place it was used had to be read to find out whether it meant
   * `attention` or `danger`. Absent is the muted default; a tone is a state
   * saying something about itself.
   */
  noteTone?: Tone;
  children: React.ReactNode;
}) {
  const copy = SETTING[id];
  const q = useContext(FilterTerm);
  if (!visible) return null;
  // A row whose only match is inside the folded-away reasoning would look like
  // a mistake. Open it, so the words that brought it back are on screen.
  const openWhy =
    q !== "" &&
    !!copy.why &&
    copy.why.toLowerCase().includes(q) &&
    !`${copy.label} ${copy.summary}`.toLowerCase().includes(q);
  return <Row key={openWhy ? "open" : "shut"} id={id} copy={copy} openWhy={openWhy} note={note} noteTone={noteTone}>{children}</Row>;
}

/**
 * The row itself. The reasoning, where there is any, sits behind a small `?`
 * beside the label rather than a "Why this matters" link ending every help
 * line: on a page of forty rows that link was forty pieces of furniture, and
 * the reasoning it hides is read by somebody who has already read the summary.
 * Collapsed is the resting state on purpose; a filter that matched only the
 * hidden words opens it, so the words that brought the row back are on screen.
 */
function Row({
  id,
  copy,
  openWhy,
  note,
  noteTone,
  children,
}: {
  id: SettingEntryId;
  copy: (typeof SETTING)[SettingEntryId];
  openWhy: boolean;
  note?: ReactNode;
  noteTone?: Tone;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(openWhy);
  const whyId = `${id}-why`;
  return (
    <div className={cn(FIELD_ROW, "py-4 first:pt-0 last:pb-0")}>
      <div className="min-w-0 space-y-1">
        <div className="flex items-center gap-1">
          <Label htmlFor={id} className="text-sm font-medium">
            {copy.label}
          </Label>
          {copy.why && (
            <button
              type="button"
              onClick={() => setOpen(!open)}
              aria-expanded={open}
              aria-controls={open ? whyId : undefined}
              aria-label={`Why this matters: ${copy.label}`}
              className={cn(
                "inline-flex h-4 w-4 items-center justify-center rounded-sm text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none",
                open && "text-foreground",
              )}
            >
              <CircleHelp className="h-3.5 w-3.5" aria-hidden />
            </button>
          )}
        </div>
        <FieldHelp>{copy.summary}</FieldHelp>
        {copy.why && open && (
          <p
            id={whyId}
            className="max-w-prose border-l-2 border-border/60 pl-3 text-xs leading-relaxed text-muted-foreground"
          >
            {copy.why}
          </p>
        )}
        <FieldHelp tone={noteTone}>{note}</FieldHelp>
      </div>
      <div className={FIELD_CONTROL}>{children}</div>
    </div>
  );
}

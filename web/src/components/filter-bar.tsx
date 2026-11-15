import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

interface FilterOption {
  label: string;
  value: string;
  /**
   * How many rows are behind it.
   *
   * `FilterBar`'s callers fold theirs into the label because they have always
   * had one; `FilterChips` takes it here so the parentheses are written once.
   * Omitted only where the *server* does the filtering and a count per option
   * would be a request per option — Admin → Publisher requests, the activity
   * feed, the integration log and the job-run feed, all of which are keyset
   * paged or server narrowed.
   */
  count?: number;
}

interface FilterBarProps {
  options: FilterOption[];
  value: string;
  onChange: (value: string) => void;
  /** What is being narrowed — the group's accessible name. */
  label?: string;
}

/**
 * A list's primary axis: one strip of tabs, each carrying its count.
 *
 * **Which one is selected is `aria-pressed`, not the underline.** An
 * underline and a darker word is a colour and a shape carrying the whole
 * state, and this is the control the page's entire contents depend on — a
 * reader who cannot see which tab is active cannot tell a filtered list from
 * the whole one.
 *
 * It scrolls sideways inside itself rather than taking the page with it. A
 * bare `flex` with seven tabs is what made the apps list scroll horizontally
 * at 390px — the width people reach for this at during an incident, which does
 * not wait for a desk.
 *
 * **`overflow-y-hidden` is not redundant beside it.** CSS computes the other
 * axis to `auto` the moment one of them stops being `visible`, so `overflow-x`
 * alone bought a vertical scroll container too — and the tabs' `-mb-px`, which
 * is what lands their underline *on* the strip's own border, puts their border
 * box one pixel past the content box. One pixel of vertical overflow is enough:
 * Chrome drew a full scrollbar with its arrows at the right-hand end of the
 * strip, a control that scrolls nothing and looks like a stepper somebody
 * forgot to wire up. Reported as "what is that widget supposed to do?", which
 * is the correct question.
 */
export function FilterBar({ options, value, onChange, label = "Filter" }: FilterBarProps) {
  return (
    <div
      role="group"
      aria-label={label}
      className="flex items-center gap-1 overflow-x-auto overflow-y-hidden border-b border-border"
    >
      {options.map((opt) => (
        <button
          key={opt.value}
          type="button"
          aria-pressed={value === opt.value}
          onClick={() => onChange(opt.value)}
          className={`shrink-0 px-3 py-2 text-sm font-medium transition-colors border-b-2 -mb-px focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-ring ${
            value === opt.value
              ? "border-primary text-foreground"
              : "border-transparent text-muted-foreground hover:text-foreground"
          }`}
        >
          <TabLabel label={opt.label} />
        </button>
      ))}
    </div>
  );
}

/**
 * The count as a figure of its own rather than a parenthesis in the label.
 * A count with rows behind it sits in a muted pill; a nought is still printed
 * — the strip says how many, always — but plainly, so "Crashed 0" does not
 * carry the weight of "Stopped 1". Callers fold their count into the label as
 * `Name (n)`, so this reads it back out rather than asking every caller to
 * pass it twice; a label in any other shape renders as it is.
 */
function TabLabel({ label }: { label: string }) {
  const m = /^(.*\S)\s*\((\d+)\)$/.exec(label);
  if (!m) return <>{label}</>;
  const n = Number(m[2]);
  return (
    <>
      {m[1]}
      <span
        className={cn(
          "ml-1.5 inline-block min-w-5 rounded-full px-1.5 text-center text-xs tabular-nums",
          n > 0 ? "bg-muted text-foreground" : "text-muted-foreground",
        )}
      >
        {m[2]}
      </span>
    </>
  );
}

interface FilterChipsProps {
  options: FilterOption[];
  value: string;
  onChange: (value: string) => void;
  /** What is being narrowed — the group's accessible name. */
  label?: string;
}

/**
 * A list's secondary axis, as a row of chips.
 *
 * **The selected chip is not filled.** It was `variant="default"` — the brand
 * indigo — at exactly the size, height and `aria-pressed` that `ChoiceButtons`
 * draws a chosen option at, so the two controls compiled to the same button.
 * One of them narrows a table and costs nothing; the other **writes to the
 * install the moment it is thrown**. An administrator meets both on the same
 * afternoon and cannot tell by looking which one is which, and no amount of
 * `aria-pressed` distinguishes them for somebody looking at the screen.
 *
 * So selection here is the `accent` treatment the admin nav and table row
 * selection already use — one visual idea of *this one is chosen* rather than
 * two — and the brand fill is left to the control that writes.
 *
 * **And every chip says how many are behind it**, unless the server is doing
 * the filtering. A facet that cannot say has to be clicked to be read, which
 * on a directory during an incident is the difference between one click and
 * four.
 */
export function FilterChips({ options, value, onChange, label = "Filter" }: FilterChipsProps) {
  // Wrapping, because the set is open-ended: the integration log draws one chip
  // per known kind, and ten of them were 760px in a row that does not scroll —
  // the whole page went sideways by 248 at a 768 viewport.
  return (
    <div role="group" aria-label={label} className="flex flex-wrap items-center gap-1.5">
      {options.map((opt) => {
        const on = value === opt.value;
        return (
          <Button
            key={opt.value}
            variant="outline"
            size="sm"
            aria-pressed={on}
            onClick={() => onChange(opt.value)}
            className={cn(
              "h-7 text-xs font-normal",
              on && "border-primary/40 bg-accent font-medium text-accent-foreground",
            )}
          >
            {opt.label}
            {/* A figure, as the tab strip prints its own — not a parenthesis:
                the two counting controls on one toolbar (the Logs tab has
                both) read as one product only if they count the same way. */}
            {opt.count !== undefined && (
              <span className="tabular-nums text-muted-foreground">{opt.count}</span>
            )}
          </Button>
        );
      })}
    </div>
  );
}

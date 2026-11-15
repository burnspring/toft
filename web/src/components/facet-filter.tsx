import { useMemo, useState, type ReactNode } from "react";
import { Check, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList,
} from "@/components/ui/command";
import { cn } from "@/lib/utils";

/**
 * One value a list can be narrowed by — a tag pair, an execution mode, whatever
 * the next facet turns out to be.
 *
 * The point of the type is that a facet's *vocabulary* is the caller's and its
 * *shape* is not. Tags and Mode were on their way to being two popovers that
 * looked alike by coincidence and drifted the first time either was touched;
 * a third facet would have made it three.
 */
export interface FacetOption {
  /** Identifies the option within its own facet. Never compared across facets. */
  key: string;
  label: string;
  /** Heading to file this option under. Ungrouped options come first, unheaded. */
  group?: string;
  /** Muted qualifier on the selected chip — "Team:" before "Engineering". */
  prefix?: string;
  /** A dot or an icon, shown in the list and on the chip. */
  marker?: ReactNode;
  /** Extra words the popover's search should match beyond the label. */
  search?: string;
  /** Colour for the selected chip. Tags carry their own; most facets don't. */
  chipClassName?: string;
}

/**
 * A secondary filter, as a popover of checkable options.
 *
 * Multi-select and ORed within one facet — "Shared *or* isolated" — because a
 * facet whose values are mutually exclusive on a single row can only ever mean
 * "or", and the caller ANDs across facets. It renders **nothing** when it has
 * no options: a facet with one possible value narrows nothing, and a filter
 * that cannot change the list is a control that does not work.
 *
 * **Unless its options are a predicate rather than a partition**, which is
 * what `predicate` says. The two-option floor assumes every row carries one of
 * the values — every app has a mode, so filtering by the only mode present
 * selects the whole list. Availability is the other shape: `Locked` is held by
 * *some* rows, so one option narrows perfectly well, and the floor meant there
 * was no way to filter for locked apps at all unless the dependency policy had
 * also locked one — the estate where an administrator least needs a second
 * kind of lock is exactly the one where they most want the first.
 */
export function FacetFilter({
  label,
  icon,
  options,
  value,
  onChange,
  placeholder,
  predicate = false,
  empty = "No matches.",
}: {
  label: string;
  icon?: ReactNode;
  options: FacetOption[];
  value: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  /** True where the options are held by *some* rows rather than partitioning
   *  all of them, so a single option still narrows. See the note above. */
  predicate?: boolean;
  empty?: string;
}) {
  const [open, setOpen] = useState(false);

  // Grouped in first-seen order rather than alphabetically: the caller has already sorted
  // its own vocabulary, and re-sorting here would silently override that.
  const groups = useMemo(() => {
    const byGroup = new Map<string, FacetOption[]>();
    for (const opt of options) {
      const key = opt.group ?? "";
      const list = byGroup.get(key);
      if (list) list.push(opt);
      else byGroup.set(key, [opt]);
    }
    return [...byGroup.entries()];
  }, [options]);

  if (options.length < (predicate ? 1 : 2)) return null;

  function toggle(key: string) {
    onChange(value.includes(key) ? value.filter((k) => k !== key) : [...value, key]);
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button variant="outline" size="sm" className="h-8 gap-1.5 text-xs font-normal">
            {/* Every facet trigger carries its icon at one size and weight, so
                a row of them reads as one control family — callers passed
                three different sizes and opacities, and two passed none. */}
            {icon && <span aria-hidden className="flex opacity-60 [&_svg]:size-3.5">{icon}</span>}
            {label}
            {value.length > 0 && (
              <Badge variant="secondary" className="ml-0.5 h-4 px-1 text-2xs">{value.length}</Badge>
            )}
          </Button>
        }
      />
      <PopoverContent align="start" className="w-64 p-0">
        <Command>
          <CommandInput placeholder={placeholder ?? `Filter by ${label.toLowerCase()}…`} />
          <CommandList>
            <CommandEmpty>{empty}</CommandEmpty>
            {groups.map(([heading, opts]) => (
              <CommandGroup key={heading} heading={heading || undefined}>
                {opts.map((opt) => {
                  const on = value.includes(opt.key);
                  return (
                    <CommandItem
                      key={opt.key}
                      value={`${heading} ${opt.label} ${opt.search ?? ""}`}
                      onSelect={() => toggle(opt.key)}
                    >
                      {opt.marker}
                      <span className="flex-1">{opt.label}</span>
                      {on && <Check className="h-3.5 w-3.5" />}
                    </CommandItem>
                  );
                })}
              </CommandGroup>
            ))}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

/** One selected value, and the way back out of it. */
export interface FacetChip {
  option: FacetOption;
  onRemove: () => void;
}

/**
 * Every facet's selections in one row, with one way to clear them all.
 *
 * Deliberately not part of `FacetFilter`: when each popover kept its own chips
 * beside its own button, two active facets pushed the buttons apart and the
 * row reflowed as you filtered. What somebody wants to read is *what is being
 * filtered*, which is one list regardless of how many popovers produced it.
 */
export function FacetChips({ chips, onClear }: { chips: FacetChip[]; onClear: () => void }) {
  if (chips.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-2">
      {chips.map(({ option, onRemove }) => (
        <Badge
          key={`${option.prefix ?? ""}${option.key}`}
          variant="outline"
          className={cn("gap-1 border text-2xs font-normal", option.chipClassName)}
        >
          {option.marker}
          {option.prefix && <span className="opacity-60">{option.prefix}</span>}
          {option.label}
          <button
            type="button"
            onClick={onRemove}
            className="-mr-0.5 ml-0.5 rounded-full p-0.5 opacity-60 hover:opacity-100"
            aria-label={`Remove ${option.prefix ?? ""}${option.label} filter`}
          >
            <X className="h-2.5 w-2.5" />
          </button>
        </Badge>
      ))}
      {/* From one chip, matching the empty state's Clear: a link that
          appeared only with the second chip came and went as facets were
          added. */}
      <button
        type="button"
        onClick={onClear}
        className="text-xs text-muted-foreground hover:text-foreground"
      >
        Clear
      </button>
    </div>
  );
}

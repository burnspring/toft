import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import { TableHead } from "@/components/ui/table";
import type { SortState } from "@/lib/sort";

/**
 * A column header that sorts its table. Clicking the active column flips the
 * direction; clicking a new one starts at descending, which is what "sort by
 * requests" or "sort by last seen" is nearly always asking for.
 */
export function SortableHead<K extends string>({
  label, sortKey, sort, onSort, className, align = "left",
}: {
  label: string;
  sortKey: K;
  sort: SortState<K>;
  onSort: (key: K) => void;
  className?: string;
  align?: "left" | "right";
}) {
  const active = sort.key === sortKey;
  const Icon = !active ? ChevronsUpDown : sort.dir === "asc" ? ArrowUp : ArrowDown;
  return (
    <TableHead
      className={className}
      aria-sort={active ? (sort.dir === "asc" ? "ascending" : "descending") : "none"}
    >
      <button
        type="button"
        onClick={() => onSort(sortKey)}
        className={`flex items-center gap-1 transition-colors hover:text-foreground ${
          align === "right" ? "ml-auto flex-row-reverse" : ""
        } ${active ? "text-foreground" : ""}`}
      >
        {label}
        <Icon className={`h-3 w-3 ${active ? "" : "opacity-50"}`} />
      </button>
    </TableHead>
  );
}

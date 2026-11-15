import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import {
  Pagination, PaginationContent, PaginationEllipsis, PaginationItem,
  PaginationLink, PaginationNext, PaginationPrevious,
} from "@/components/ui/pagination";

/**
 * The footer under a long table: how much of it you are looking at, how many
 * rows you would rather see, and the way to the rest.
 *
 * A component rather than a pattern, because it was a pattern once and the
 * second page that needed it was going to be a copy. People had all of this
 * inline; Apps needed the same thing the day an estate stopped being twelve
 * apps somebody could hold in their head. Two copies of a page-size control
 * are two chances to disagree about whether changing it returns you to page
 * one — it does, and here that is one line rather than two.
 *
 * The caller owns `page` and `pageSize` and does its own slicing. This draws
 * the controls and nothing else, so a page whose rows come from the server
 * rather than an array can use it unchanged.
 */

const pageSizeOptions = [10, 25, 50, 100];
/** What every caller starts at. */
const defaultPageSize = 25;

/** first, last, current ±1, with ellipses where the window skips pages. */
function pageWindow(current: number, total: number): (number | "ellipsis")[] {
  const kept = [...new Set([1, current - 1, current, current + 1, total])]
    .filter((p) => p >= 1 && p <= total)
    .sort((a, b) => a - b);
  const out: (number | "ellipsis")[] = [];
  let prev = 0;
  for (const p of kept) {
    if (prev && p - prev > 1) out.push("ellipsis");
    out.push(p);
    prev = p;
  }
  return out;
}

export function TablePagination({
  total,
  page,
  pageSize,
  onPage,
  onPageSize,
}: {
  /** Rows after filtering — what "of N" counts, not what the server holds. */
  total: number;
  page: number;
  pageSize: number;
  onPage: (page: number) => void;
  onPageSize: (size: number) => void;
}) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  // A list that fits on one page at the default size has nothing to page:
  // "Showing 1 to 1 of 1" over a page-size picker is furniture. Past the
  // default the picker stays, so a reader who chose 50 can choose 25 again.
  if (total <= Math.min(pageSize, defaultPageSize)) return null;

  return (
    <div className="flex flex-wrap items-center justify-between gap-3 pt-2">
      <p className="text-sm text-muted-foreground">
        Showing {(page - 1) * pageSize + 1} to {Math.min(page * pageSize, total)} of {total}
      </p>
      <div className="flex items-center gap-3">
        <Select
          value={String(pageSize)}
          // Back to page one, always. Changing the page size while deep in a
          // list otherwise lands you on a page that no longer exists, and the
          // table renders empty under a footer saying there are two hundred
          // rows.
          onValueChange={(v) => { onPageSize(Number(v)); onPage(1); }}
        >
          <SelectTrigger size="sm" aria-label="Rows per page">
            <SelectValue>{(v) => `${v ?? pageSize} per page`}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {pageSizeOptions.map((n) => (
              <SelectItem key={n} value={String(n)}>{n} per page</SelectItem>
            ))}
          </SelectContent>
        </Select>
        {totalPages > 1 && (
          <Pagination className="mx-0 w-auto">
            <PaginationContent>
              <PaginationItem>
                <PaginationPrevious
                  href="#"
                  aria-disabled={page === 1}
                  className={page === 1 ? "pointer-events-none opacity-50" : undefined}
                  onClick={(e) => { e.preventDefault(); onPage(Math.max(1, page - 1)); }}
                />
              </PaginationItem>
              {pageWindow(page, totalPages).map((p, i) => (
                <PaginationItem key={p === "ellipsis" ? `e${i}` : p}>
                  {p === "ellipsis" ? (
                    <PaginationEllipsis />
                  ) : (
                    <PaginationLink
                      href="#"
                      isActive={p === page}
                      onClick={(e) => { e.preventDefault(); onPage(p); }}
                    >
                      {p}
                    </PaginationLink>
                  )}
                </PaginationItem>
              ))}
              <PaginationItem>
                <PaginationNext
                  href="#"
                  aria-disabled={page === totalPages}
                  className={page === totalPages ? "pointer-events-none opacity-50" : undefined}
                  onClick={(e) => { e.preventDefault(); onPage(Math.min(totalPages, page + 1)); }}
                />
              </PaginationItem>
            </PaginationContent>
          </Pagination>
        )}
      </div>
    </div>
  );
}

/** Shared table-sorting state. The header component lives in
 * `components/sortable-head.tsx`; the state and its reducer live here so a
 * component file exports only components (Fast Refresh). */
export interface SortState<K extends string> {
  key: K;
  dir: "asc" | "desc";
}

/** Same column flips direction, a new column starts descending — which is
 * what "sort by requests" or "sort by last seen" nearly always means. */
export function nextSort<K extends string>(current: SortState<K>, key: K): SortState<K> {
  return current.key === key
    ? { key, dir: current.dir === "asc" ? "desc" : "asc" }
    : { key, dir: "desc" };
}

/**
 * Two optional timestamps in `dir` order, with a missing one **last in either
 * direction**. The pin is applied after the direction, so a caller must not
 * multiply the result by the direction again — that flipped it, and a
 * descending sort put every undated row at the top.
 */
export function compareDatesMissingLast(
  a: string | null | undefined,
  b: string | null | undefined,
  dir: "asc" | "desc",
): number {
  const ta = a ? new Date(a).getTime() : null;
  const tb = b ? new Date(b).getTime() : null;
  if (ta === null && tb === null) return 0;
  if (ta === null) return 1;
  if (tb === null) return -1;
  return (ta - tb) * (dir === "asc" ? 1 : -1);
}

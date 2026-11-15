/**
 * Keeping a "load more" feed's pages chained to each other.
 *
 * Each page after the first holds the cursor the page before it returned
 * *when it was first read*. A refetch of an earlier page after new rows have
 * landed returns a newer tail, and the rows that fell off its end are then on
 * no page, with nothing on screen marking the gap. So when page `index`
 * reports a tail that is not the cursor the next page holds, the pages after
 * it are dropped and Load more re-reads them from the new cursor.
 *
 * Returns `pages` itself when nothing changes, so a caller can bail out of a
 * state update.
 */
export function chainedPages<C extends { before_id: string } | null>(
  pages: C[],
  index: number,
  tailId: string | undefined,
): C[] {
  if (index >= pages.length - 1) return pages;
  if (pages[index + 1]?.before_id === tailId) return pages;
  return pages.slice(0, index + 1);
}

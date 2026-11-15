import { formatBytes } from "./bytes";

/**
 * One name the platform removed from an uploaded archive, totalled over
 * everywhere in the tree it appeared.
 *
 * Declared here rather than in `api.ts` — which re-exports it — so this module
 * stays free of anything that touches the DOM. Its tests run under the node
 * project, which has no `lib.dom`, and a type-only import is enough to drag the
 * whole API surface in with it.
 */
export interface RemovedEntry {
  path: string;
  files: number;
  bytes: number;
}

/**
 * The short form of the removal report, for a toast.
 *
 * The server's answer is the whole list, largest first, and the build log
 * prints all of it. A toast has room for the fact and not the table: the
 * biggest thing that went, by name, so somebody who did not expect it goes and
 * reads the log. Silent when nothing was removed — an ordinary archive's toast
 * is what it always was.
 */
export function removedSummary(removed: RemovedEntry[] | undefined): string {
  if (!removed || removed.length === 0) return "";
  const [first, ...rest] = removed;
  const size = formatBytes(first.bytes);
  const more =
    rest.length === 1
      ? ` and ${rest[0].path}`
      : rest.length > 1
        ? ` and ${rest.length} more`
        : "";
  return `${size} of ${first.path}${more} was removed before installing.`;
}

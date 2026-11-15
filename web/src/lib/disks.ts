import type { DiskReport, DisksBlock } from "@/lib/api";
import type { Tone } from "@/lib/tone";

/** A disk’s bar colour. Ordinary use keeps the brand colour, which is what a
 *  bar that is merely counting up should stay; past a watermark it is a state. */
export function diskTone(d: DiskReport): Tone | undefined {
  if (d.level === "full") return "danger";
  if (d.level === "low") return "attention";
  return undefined;
}

/** “62% full”. A disk with anything on it never reads 0%. */
export function fullnessLabel(d: DiskReport): string {
  const pct = d.used_percent;
  if (pct > 0 && pct < 1) return "Under 1% full";
  return `${Math.round(pct)}% full`;
}


/**
 * The folders the table's own rows already measure: the "Release disk" check
 * states free space for exactly these four (`checkReleaseDisk`), and "Build
 * cache directory" names the cache. Matched on `what` because that is the one
 * thing the report says about a folder; the labels are `diskFolders`' in
 * `internal/api/disks.go`.
 */
const MEASURED_BY_A_ROW = new Set(["App releases", "Job runs", "Build cache", "Logs"]);

/**
 * What of the disks block the checks table does not already say: the disks
 * holding a storage drive, and every folder whose disk did not answer (the
 * release-disk row skips those silently). Null when that is nothing, so a page
 * states each disk once.
 */
export function disksBeyondTheRows(block: DisksBlock): DisksBlock | null {
  const disks = block.disks.filter((d) => d.folders.some((f) => !MEASURED_BY_A_ROW.has(f.what)));
  if (disks.length === 0 && !block.unanswered?.length) return null;
  return { ...block, disks };
}

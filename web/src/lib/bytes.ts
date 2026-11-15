/**
 * Byte sizes, the one formatter the SPA has.
 *
 * Binary (÷1024) with the labels `du -h`, the build log and `toft` all print —
 * KB, MB, GB — and one decimal from KB up. The server's checks and build log
 * (`builder.FormatBytes`) write the same shape, so a disk on Admin → Checks and
 * the same disk on Admin → Processes read as one figure.
 */
const UNITS = ["KB", "MB", "GB", "TB", "PB", "EB"] as const;

export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "--";
  if (bytes < 1024) return `${Math.round(bytes)} B`;
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(1)} ${UNITS[unit]}`;
}

/**
 * A size that may not have been measured. Null is not zero — an unmeasured
 * release is one whose directory and artifact are both gone, and rendering that
 * as "0 B" would read as a build that costs nothing to keep.
 */
export function formatOptionalBytes(bytes: number | null | undefined): string {
  return bytes == null ? "--" : formatBytes(bytes);
}

/**
 * Megabytes as a person reads them.
 *
 * Separate from `formatBytes` because these figures arrive already in MB —
 * from the backend's process listing and from the consumption roll-up — and routing them
 * back through a byte count to be divided again is how a rounding difference
 * appears between two screens showing the same app.
 */
export function formatMB(mb: number): string {
  if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`;
  return `${Math.round(mb)} MB`;
}

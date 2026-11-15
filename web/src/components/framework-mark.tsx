import { FRAMEWORK_MARKS } from "@/lib/framework-marks";

/**
 * A framework's own mark, at whatever size the caller asks for.
 *
 * The paths are vendored trademarks (`lib/framework-marks.ts`), so the colour
 * has to be the brand's own too — a real logo in an approximated colour is
 * still the wrong logo. That is why this reads `FRAMEWORK_MARKS` and not
 * `frameworks.ts`, whose colours are deliberately muted to sit inside a badge.
 *
 * Decorative: every caller has the name in text beside it, so a second
 * announcement of the same word is noise. An id with no mark draws nothing.
 */
export function FrameworkMark({ id, className }: { id: string; className: string }) {
  const mark = FRAMEWORK_MARKS[id];
  if (!mark) return null;
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} style={{ color: mark.color }}>
      <path d={mark.path} fill="currentColor" />
    </svg>
  );
}

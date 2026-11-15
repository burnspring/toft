/**
 * The decisions behind reading a long log, as pure functions — what a step is,
 * what an error line looks like, which steps start folded, and what a line's
 * address is. They live here rather than in `LogPane` because they are the part
 * worth asserting, and `npm test` runs without a DOM (ADR §50).
 */

/**
 * One step of a build, as the builder narrates it: a `==> ` line and every line
 * up to the next one. `start` is the header's index, `end` is exclusive.
 */
export interface LogSection {
  start: number;
  end: number;
}

/** Whether a line is the builder announcing a step. */
export function isStepHeader(line: string): boolean {
  return line.startsWith("==> ");
}

/**
 * The steps in a build log. Lines before the first header belong to no step
 * and are never folded — the upload's removed-files report sits there, and it
 * is the first thing anybody reading the log is owed.
 */
export function logSections(lines: string[]): LogSection[] {
  const out: LogSection[] = [];
  for (let i = 0; i < lines.length; i++) {
    if (!isStepHeader(lines[i])) continue;
    if (out.length > 0) out[out.length - 1].end = i;
    out.push({ start: i, end: lines.length });
  }
  return out;
}

/**
 * Which steps open folded. A failed build folds every step but the last — the
 * one it died in, which is the only one anybody opened the log to read. A build
 * that worked, or is still working, folds nothing: there is no step to point
 * at, and hiding output from somebody watching it arrive reads as a stall.
 */
export function defaultFolded(sections: LogSection[], failed: boolean): Set<number> {
  if (!failed || sections.length < 2) return new Set();
  return new Set(sections.slice(0, -1).map((s) => s.start));
}

/** The step a line sits in, as its header's index, or -1 for none. */
export function sectionOf(sections: LogSection[], index: number): number {
  for (const s of sections) {
    if (index >= s.start && index < s.end) return s.start;
  }
  return -1;
}

/**
 * Whether a line is a failure speaking. Deliberately narrow: the words that
 * tools print when something has stopped, at the start of the line, after the
 * builder's indent. "0 errors" and a package named `error-stack-parser` are
 * output, not failures.
 */
const ERROR_LINE =
  /^\s*(ERROR\b|error(\[[A-Z0-9]+\])?:|Error:|npm ERR!|npm error\b|Traceback \(most recent call last\)|FATAL\b|fatal:|E: |Execution halted)/;

export function isErrorLine(line: string): boolean {
  return ERROR_LINE.test(line);
}

/** The first line a failure printed, or -1. */
export function firstErrorLine(lines: string[]): number {
  return lines.findIndex(isErrorLine);
}

/** A line's address in the page's fragment: one-based, as people count. */
export function lineHash(index: number): string {
  return `#L${index + 1}`;
}

/** The line a fragment names, zero-based, or null when it names none. */
export function parseLineHash(hash: string): number | null {
  const m = /^#L(\d+)$/.exec(hash);
  if (!m) return null;
  const n = Number(m[1]);
  return n >= 1 ? n - 1 : null;
}

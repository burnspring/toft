import type { EstateSeries, EstateVersionState } from "@/lib/api";

/**
 * One definition of "which release line is this app on", shared by the census
 * and the apps list's facet.
 *
 * The *derivation* is the server's — a language is read off the serving
 * release or, failing that, off the framework through the detector's own
 * mapping, and a version becomes a series because patch never matters. Both
 * surfaces receive it already folded, so neither reimplements it here. What
 * this file owns is the pair of things a URL and a filter need: a stable key,
 * and the words a person reads.
 */
export interface RuntimeSeries {
  language: string;
  label: string;
  series: string;
  state: EstateVersionState;
}

/**
 * The key a series filters by, and its wire form in the URL.
 *
 * The two wordless states get keys of their own rather than collapsing into
 * one empty value, because "this app runs no interpreter" and "nobody recorded
 * which interpreter this app runs" are different facts and a filter that merged
 * them would answer neither question.
 */
export function runtimeSeriesKey(r: Pick<RuntimeSeries, "language" | "series" | "state">) {
  return `${r.language}:${r.state === "resolved" ? r.series : r.state}`;
}

/** What the version cell says. Never a patch, and never a blank. */
export function runtimeSeriesLabel(r: Pick<RuntimeSeries, "series" | "state">) {
  switch (r.state) {
    case "resolved":
      return r.series;
    case "unresolved":
      return "Unresolved";
    default:
      return "Not applicable";
  }
}

/**
 * Why a row has no version, in the words the census uses.
 *
 * Absent is a state with a name and it never reads as zero, so each state says
 * what it means rather than leaving a reader to assume the number is missing.
 */
export function runtimeStateExplanation(state: EstateVersionState): string {
  switch (state) {
    case "unresolved":
      return "No version was recorded — a Go build the go command settled, or a release older than the columns.";
    case "not_applicable":
      return "Runs no interpreter, so there is no version to record.";
    default:
      return "";
  }
}

/** The full name of a row, for a filter chip and an accessible link name. */
export function runtimeSeriesFullLabel(language: string, s: Pick<EstateSeries, "series" | "state">) {
  return s.state === "resolved" ? `${language} ${s.series}` : `${language} — ${runtimeSeriesLabel(s).toLowerCase()}`;
}

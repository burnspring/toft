/**
 * Where a chart's top gridline sits.
 *
 * Both bar charts in the product used to label their gridlines with the series
 * maximum and half of it, so a month whose busiest day saw 58 requests drew
 * lines at `58` and `29`. Those are readings of the data, not a scale: the axis
 * moved every time the data did, two charts side by side could not be compared,
 * and the number a person read off a gridline was one they had to divide to
 * use.
 *
 * So the ceiling is rounded to a number somebody would have chosen, and every
 * mark on the chart is measured against *that* rather than against the maximum.
 */

/** The normalised ceilings, in the order a person would reach for them. */
const STEPS = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 7, 8, 10];

/**
 * The smallest round number at or above `max`.
 *
 * Below ten the result is nudged to an even number, because these are counts —
 * requests, calls, viewers — and the midpoint gridline is labelled too. A
 * ceiling of 5 puts `2.5 requests` on the axis, which is a number of nothing.
 */
export function niceCeiling(max: number): number {
  const m = Math.max(1, max);
  const magnitude = Math.pow(10, Math.floor(Math.log10(m)));
  const normalised = m / magnitude;
  const step = STEPS.find((s) => s >= normalised - 1e-9) ?? 10;
  const top = step * magnitude;
  return top < 10 ? Math.max(2, Math.ceil(top / 2) * 2) : top;
}

/**
 * The ceiling and the values to draw a line at, top last.
 *
 * Two lines is the whole budget: this is a sparkline's bigger sibling sitting
 * inside a card, not an analysis surface, and a third line costs more than it
 * tells anybody.
 */
export function niceScale(max: number): { top: number; ticks: number[] } {
  const top = niceCeiling(max);
  return { top, ticks: [top / 2, top] };
}

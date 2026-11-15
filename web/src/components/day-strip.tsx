import type { UpcomingSchedules } from "@/lib/api";
import { formatHourMinute } from "@/lib/time";
import { durationLong, firingName, recurrencePhrase, stackLanes } from "@/lib/upcoming";

const LANE_PX = 12;
const BAND_PX = 8;
const AXIS_PX = 18;

/**
 * One day of an install's scheduled firings as a strip (ADR §156): each firing
 * a bar from its time for its usual length, stacked into lanes so an overlap
 * reads as height; a firing with no history a point with its bound faint
 * behind it; a schedule firing more often than hourly a thin band underneath.
 * Before `now` is dimmed — past firings are the run feed's, not a forecast's.
 *
 * Decorative to a screen reader: the agenda beside it is the same day in
 * words, which is where every fact on the strip can also be read.
 */
export function DayStrip({
  data, from, to, now,
}: {
  data: Pick<UpcomingSchedules, "firings" | "recurring">;
  from: number;
  to: number;
  now: number;
}) {
  const span = to - from;
  const pct = (t: number) => `${(Math.min(Math.max(t - from, 0), span) / span) * 100}%`;
  const width = (a: number, b: number) => `${(Math.max(Math.min(b, to) - Math.max(a, from), 0) / span) * 100}%`;
  // Ten minutes of the day, so two firings close together stay two bars.
  const { bars, lanes } = stackLanes(data.firings, span / 144);
  const laneRows = Math.max(lanes, 1);
  const height = AXIS_PX + laneRows * LANE_PX + data.recurring.length * BAND_PX + 4;

  // Hour boundaries by instant, so a 23- or 25-hour day draws as one.
  const hours: number[] = [];
  for (let t = from; t < to; t += 3_600_000) hours.push(t);

  return (
    <div aria-hidden className="relative select-none" style={{ height }}>
      {hours.map((t, i) => (
        <div key={t} className="absolute top-0 bottom-0" style={{ left: pct(t) }}>
          <div className={`h-full w-px ${i % 3 === 0 ? "bg-border" : "bg-border/40"}`} />
          {i % 3 === 0 && (
            <span className="absolute top-0 left-1 text-2xs text-muted-foreground tabular-nums">
              {formatHourMinute(t).slice(0, 2)}
            </span>
          )}
        </div>
      ))}

      {bars.map((b) => {
        const top = AXIS_PX + b.lane * LANE_PX;
        const title = `${formatHourMinute(b.start)} ${firingName(b.firing)} · ${b.firing.app_name} · ${durationLong(b.firing)}`;
        return (
          <div key={`${b.firing.schedule_id}:${b.start}`}>
            {b.upper !== undefined && (
              <div
                className="absolute h-2 rounded-sm bg-primary/15"
                style={{ top, left: pct(b.start), width: width(b.start, b.upper) }}
              />
            )}
            <div
              title={title}
              className="absolute h-2 min-w-[3px] rounded-sm bg-primary"
              style={{ top, left: pct(b.start), width: width(b.start, b.end) }}
            />
          </div>
        );
      })}

      {data.recurring.map((r, i) => {
        const a = Date.parse(r.at);
        const b = Date.parse(r.last ?? r.at) + (r.typical_sec ?? 60) * 1000;
        return (
          <div
            key={r.schedule_id}
            title={`${firingName(r)} · ${r.app_name} · ${recurrencePhrase(r)}`}
            className="absolute h-1 rounded-full bg-primary/40"
            style={{ top: AXIS_PX + laneRows * LANE_PX + i * BAND_PX + 2, left: pct(a), width: width(a, b) }}
          />
        );
      })}

      {now > from && (
        <div
          className="pointer-events-none absolute top-0 bottom-0 left-0 bg-background/60"
          style={{ width: width(from, now) }}
        />
      )}
      {now > from && now < to && (
        <div className="absolute top-0 bottom-0 w-px bg-foreground" style={{ left: pct(now) }} />
      )}
    </div>
  );
}

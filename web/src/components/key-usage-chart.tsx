import { useState } from "react";
import { useElementWidth } from "@/hooks/use-element-width";
import { niceScale } from "@/lib/chart-scale";
import { formatUTCDay, formatUTCMonth } from "@/lib/time";
import { toneVar } from "@/lib/tone";
import type { KeyUsageBucket, KeyUsagePoint } from "@/lib/api";

/**
 * Traffic for one API key, stacked by outcome.
 *
 * The split is the whole point. A plain request count cannot tell a healthy
 * nightly job from one that has been 404ing since March, and "it is still
 * running" is exactly the wrong reassurance to give in that case. Successes sit
 * on the baseline; failures stack above them in the warning and danger tones,
 * so a bar that is mostly not-green reads as wrong at a glance.
 */
export function KeyUsageChart({
  series,
  bucket,
}: {
  series: KeyUsagePoint[];
  bucket: KeyUsageBucket;
}) {
  const [hover, setHover] = useState<number | null>(null);

  const [wrapRef, W] = useElementWidth<HTMLDivElement>();
  const H = 160,
    PAD_L = 40,
    PAD_B = 18;
  const plotW = W - PAD_L,
    plotH = H - PAD_B;
  // A round ceiling rather than the busiest bucket, so the axis is a scale and
  // not a second reading of the data. See `lib/chart-scale.ts`.
  const { top: axisTop, ticks } = niceScale(Math.max(0, ...series.map((d) => d.requests)));
  const slot = plotW / Math.max(1, series.length);
  const barW = Math.max(2, Math.min(24, slot - 2));
  const tickEvery = Math.ceil(series.length / 6);
  /** One coordinate space for the gridlines and the bars alike. */
  const yOf = (value: number) => plotH - (value / axisTop) * (plotH - 8);

  // The same three tones the badges use, so a rejected call is the colour of a
  // warning everywhere it appears. `toneVar` exists because an SVG `fill` takes
  // a colour and not a class.
  const bands = [
    { key: "success" as const, fill: toneVar.ok, label: "succeeded" },
    { key: "client_error" as const, fill: toneVar.attention, label: "rejected" },
    { key: "server_error" as const, fill: toneVar.danger, label: "failed" },
  ];

  const hovered = hover !== null ? series[hover] : null;

  return (
    <div ref={wrapRef} className="relative">
      {hovered && (
        <div
          className="pointer-events-none absolute -top-2 z-10 -translate-x-1/2 -translate-y-full rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-md"
          style={{ left: `${((PAD_L + (hover! + 0.5) * slot) / W) * 100}%` }}
        >
          <div className="font-medium">{formatBucket(hovered.bucket, bucket)}</div>
          <div className="text-muted-foreground">
            {hovered.requests.toLocaleString()} call{hovered.requests === 1 ? "" : "s"}
          </div>
          {(hovered.client_error > 0 || hovered.server_error > 0) && (
            <div className="text-muted-foreground">
              {hovered.client_error > 0 && `${hovered.client_error.toLocaleString()} rejected`}
              {hovered.client_error > 0 && hovered.server_error > 0 && " · "}
              {hovered.server_error > 0 && `${hovered.server_error.toLocaleString()} failed`}
            </div>
          )}
        </div>
      )}
      <svg
        viewBox={`0 0 ${W} ${H}`}
        // Drawn at one unit per CSS pixel (`useElementWidth`), so a wide
        // column widens the plot without scaling its type or its height.
        className="block w-full"
        style={{ height: H }}
        // A group of focusable bars rather than one image: each bar's figures
        // were in a hover tooltip alone, which no keyboard or touch reaches.
        role="group"
        aria-label={`Calls per ${bucket}, split by outcome`}
        onMouseLeave={() => setHover(null)}
      >
        {ticks.map((t) => (
          <g key={t}>
            <line
              x1={PAD_L}
              x2={W}
              y1={yOf(t)}
              y2={yOf(t)}
              stroke="var(--border)"
              strokeWidth="1"
            />
            {/* y is clamped so the topmost label is not half-cut by the
                viewBox edge: it lands at y=3 otherwise, and a 10px glyph does
                not fit above that. */}
            <text
              x={PAD_L - 6}
              y={Math.max(11, yOf(t) + 3)}
              textAnchor="end"
              className="text-2xs"
              fill="var(--muted-foreground)"
            >
              {t.toLocaleString()}
            </text>
          </g>
        ))}
        <line x1={PAD_L} x2={W} y1={plotH} y2={plotH} stroke="var(--border)" strokeWidth="1" />

        {series.map((d, i) => {
          const x = PAD_L + i * slot + (slot - barW) / 2;
          // Stacked from the baseline up, so the eye reads total height as
          // volume and colour as health without doing arithmetic.
          let cursor = plotH;
          return (
            <g key={d.bucket}>
              {bands.map((band) => {
                const value = d[band.key];
                if (value <= 0) return null;
                const h = (value / axisTop) * (plotH - 8);
                const y = cursor - h;
                cursor = y;
                return (
                  <rect
                    key={band.key}
                    x={x}
                    y={y}
                    width={barW}
                    height={h}
                    fill={band.fill}
                    fillOpacity={hover === i ? 1 : 0.85}
                  />
                );
              })}
              <rect
                x={PAD_L + i * slot}
                y={0}
                width={slot}
                height={plotH}
                fill="transparent"
                role="img"
                tabIndex={0}
                aria-label={barLabel(d, bucket)}
                className="outline-none"
                onMouseEnter={() => setHover(i)}
                onFocus={() => setHover(i)}
                onBlur={() => setHover(null)}
              />
              {i % tickEvery === 0 && (
                <text
                  x={PAD_L + i * slot + slot / 2}
                  y={H - 4}
                  textAnchor="middle"
                  className="text-2xs"
                  fill="var(--muted-foreground)"
                >
                  {formatBucket(d.bucket, bucket)}
                </text>
              )}
            </g>
          );
        })}
      </svg>

      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1">
        {bands.map((band) => (
          <span key={band.key} className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <span
              className="h-2 w-2 rounded-[2px]"
              style={{ backgroundColor: band.fill }}
              aria-hidden
            />
            {band.label}
          </span>
        ))}
      </div>
    </div>
  );
}

/** Buckets are UTC calendar periods, so a month reads as a month rather than
 * as "the 1st". */
function formatBucket(day: string, bucket: KeyUsageBucket): string {
  if (bucket === "month") return formatUTCMonth(day);
  const label = formatUTCDay(day);
  return bucket === "week" ? `Week of ${label}` : label;
}

/** One bar's figures, in words, for whoever cannot hover it. */
function barLabel(d: KeyUsagePoint, bucket: KeyUsageBucket): string {
  const parts = [`${d.requests.toLocaleString()} call${d.requests === 1 ? "" : "s"}`];
  if (d.client_error > 0) parts.push(`${d.client_error.toLocaleString()} rejected`);
  if (d.server_error > 0) parts.push(`${d.server_error.toLocaleString()} failed`);
  return `${formatBucket(d.bucket, bucket)}: ${parts.join(", ")}`;
}

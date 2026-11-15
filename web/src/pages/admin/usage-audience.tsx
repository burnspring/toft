import { useState } from "react";
import { useElementWidth } from "@/hooks/use-element-width";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Activity, Globe, Users } from "lucide-react";

import { api, type AudienceBucket, type AudienceGrain, type SystemAudience } from "@/lib/api";
import { useUrlParam } from "@/hooks/use-list-state";
import { niceScale } from "@/lib/chart-scale";
import { statColumns } from "@/lib/fact-grid";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { formatUTCDay, formatUTCMonth } from "@/lib/time";
import { FilterChips } from "@/components/filter-bar";
import { QueryState } from "@/components/query-state";
import { Section } from "@/components/section";
import { StatCard } from "@/components/stat-card";
import { Skeleton } from "@/components/ui/skeleton";

/** The windows a CTO asks about, each with the grain that draws it legibly. */
const AUDIENCE_WINDOWS: { value: string; label: string; days: number; grain: AudienceGrain }[] = [
  { value: "12w", label: "12 weeks", days: 12 * 7, grain: "week" },
  { value: "6m", label: "6 months", days: 182, grain: "week" },
  { value: "12m", label: "12 months", days: 365, grain: "month" },
];

/** A UTC day `n` days before today, YYYY-MM-DD. */
function utcDaysAgo(n: number): string {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() - n);
  return d.toISOString().slice(0, 10);
}

/**
 * Admin → Usage § Audience — how many people the install reaches, as one line
 * a CTO can be shown: people signed in, anonymous browsers, page loads.
 *
 * Distinct across the window, so none of the three is a sum of the chart's
 * bars. Anonymous browsers is absent unless the administrator switched it on
 * (Admin → Authentication), and a bucket from before it was on draws a gap,
 * never a zero.
 */
export function AudienceSection() {
  const [windowKey, setWindowKey] = useUrlParam("audience", "12w");
  const chosen = AUDIENCE_WINDOWS.find((w) => w.value === windowKey) ?? AUDIENCE_WINDOWS[0];
  const since = utcDaysAgo(chosen.days - 1);
  const until = utcDaysAgo(0);

  const query = useQuery<SystemAudience>({
    queryKey: ["system-audience", since, until, chosen.grain],
    queryFn: () => api.getSystemAudience(since, until, chosen.grain),
    retry: false,
  });
  const data = query.data;
  const on = data?.audience_measurement === true;
  // Figures recorded while counting was on are read with it off too (BB-43):
  // off stops counting, not the reading of what was counted.
  const recorded = data?.buckets.some((b) => b.anonymous_browsers != null) ?? false;
  const showBrowsers = on || recorded;

  return (
    <Section
      id="audience"
      title="Audience"
      description="How many people and browsers opened apps on this install."
      actions={
        <FilterChips
          label="Audience window"
          value={chosen.value}
          onChange={setWindowKey}
          options={AUDIENCE_WINDOWS.map((w) => ({ value: w.value, label: w.label }))}
        />
      }
    >
      <div className="space-y-4 p-4">
        <QueryState
          query={query}
          loading={
            <>
              <div className={`grid grid-cols-1 gap-3 ${statColumns(3)}`}>
                {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-24" />)}
              </div>
              <Skeleton className="h-40 w-full" />
            </>
          }
          title="The audience could not be read"
          fallback={ADMIN_ONLY_FALLBACK}
        >
          {data && (
            <>
              <div className={`grid grid-cols-1 gap-3 ${statColumns(3)}`}>
                <StatCard
                  label="People signed in"
                  value={data.totals.people.toLocaleString()}
                  icon={<Users className="h-4 w-4" />}
                />
                {showBrowsers ? (
                  <StatCard
                    label="Anonymous browsers"
                    value={(data.totals.anonymous_browsers ?? 0).toLocaleString()}
                    description={
                      on ? (
                        "Estimated, ±1%"
                      ) : (
                        <>
                          Recorded while counting was on.{" "}
                          <Link to="/admin/authentication#anonymous" className="text-primary hover:underline">
                            Counting is off
                          </Link>
                        </>
                      )
                    }
                    icon={<Globe className="h-4 w-4" />}
                  />
                ) : (
                  <StatCard
                    label="Anonymous browsers"
                    value="Off"
                    description={
                      <Link to="/admin/authentication#anonymous" className="text-primary hover:underline">
                        Turn on counting
                      </Link>
                    }
                    icon={<Globe className="h-4 w-4" />}
                  />
                )}
                <StatCard
                  label="Page loads"
                  value={data.totals.page_loads.toLocaleString()}
                  icon={<Activity className="h-4 w-4" />}
                />
              </div>
              <AudienceChart buckets={data.buckets} grain={data.grain} showBrowsers={showBrowsers} />
            </>
          )}
        </QueryState>
      </div>
    </Section>
  );
}

function bucketLabel(start: string, grain: AudienceGrain): string {
  return grain === "month" ? formatUTCMonth(start) : formatUTCDay(start);
}

/**
 * People and anonymous browsers per bucket, side by side. A bucket with no
 * browser count (the switch was off) draws no browser bar at all — a gap, not
 * a zero — and says so in its tooltip.
 */
function AudienceChart({
  buckets,
  grain,
  showBrowsers,
}: {
  buckets: AudienceBucket[];
  grain: AudienceGrain;
  showBrowsers: boolean;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const [wrapRef, W] = useElementWidth<HTMLDivElement>();
  const H = 180, PAD_L = 36, PAD_B = 18, PAD_T = 10;
  const plotW = W - PAD_L, plotH = H - PAD_B;
  const max = Math.max(
    0,
    ...buckets.map((b) => Math.max(b.people, showBrowsers ? (b.anonymous_browsers ?? 0) : 0)),
  );
  const { top, ticks } = niceScale(max);
  const yOf = (v: number) => plotH - (v / top) * (plotH - PAD_T);
  const slot = plotW / Math.max(1, buckets.length);
  const series = showBrowsers ? 2 : 1;
  const barW = Math.max(2, Math.min(14, (slot - 4) / series));
  const tickEvery = Math.ceil(buckets.length / 6);
  const hovered = hover !== null ? buckets[hover] : null;

  return (
    <div ref={wrapRef} className="relative">
      {hovered && (
        <div
          className="pointer-events-none absolute -top-2 z-10 -translate-x-1/2 -translate-y-full rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-md"
          style={{ left: `${((PAD_L + (hover! + 0.5) * slot) / W) * 100}%` }}
        >
          <div className="font-medium">
            {grain === "week" ? "Week of " : ""}
            {bucketLabel(hovered.start, grain)}
          </div>
          <div className="text-muted-foreground">
            {hovered.people.toLocaleString()} signed in
            {showBrowsers &&
              (hovered.anonymous_browsers == null
                ? " · browsers not counted"
                : ` · ${hovered.anonymous_browsers.toLocaleString()} anonymous`)}
            {` · ${hovered.page_loads.toLocaleString()} page loads`}
          </div>
        </div>
      )}
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="block w-full"
        style={{ height: H }}
        role="img"
        aria-label={showBrowsers ? "People signed in and anonymous browsers" : "People signed in"}
        onMouseLeave={() => setHover(null)}
      >
        {ticks.map((t) => (
          <g key={t}>
            <line x1={PAD_L} x2={W} y1={yOf(t)} y2={yOf(t)} stroke="var(--border)" strokeWidth="1" />
            <text x={PAD_L - 6} y={yOf(t) + 3} textAnchor="end" className="text-2xs" fill="var(--muted-foreground)">
              {t.toLocaleString()}
            </text>
          </g>
        ))}
        {buckets.map((b, i) => {
          const x0 = PAD_L + i * slot + (slot - barW * series) / 2;
          return (
            <g key={b.start} onMouseEnter={() => setHover(i)}>
              <rect x={PAD_L + i * slot} y={0} width={slot} height={plotH} fill="transparent" />
              <rect x={x0} y={yOf(b.people)} width={barW} height={plotH - yOf(b.people)} fill="var(--chart-1)" rx="2" />
              {showBrowsers && b.anonymous_browsers != null && (
                <rect
                  x={x0 + barW}
                  y={yOf(b.anonymous_browsers)}
                  width={barW}
                  height={plotH - yOf(b.anonymous_browsers)}
                  fill="var(--chart-2)"
                  rx="2"
                />
              )}
              {i % tickEvery === 0 && (
                <text x={PAD_L + (i + 0.5) * slot} y={H - 4} textAnchor="middle" className="text-2xs" fill="var(--muted-foreground)">
                  {bucketLabel(b.start, grain)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
      <div className="mt-2 flex flex-wrap gap-4 text-xs text-muted-foreground">
        <span className="flex items-center gap-1.5">
          <span className="inline-block h-2.5 w-2.5 rounded-sm bg-chart-1" /> People signed in
        </span>
        {showBrowsers && (
          <span className="flex items-center gap-1.5">
            <span className="inline-block h-2.5 w-2.5 rounded-sm bg-chart-2" /> Anonymous browsers
          </span>
        )}
      </div>
    </div>
  );
}

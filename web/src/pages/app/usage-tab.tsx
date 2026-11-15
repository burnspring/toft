import { useState, useMemo } from "react";
import { useElementWidth } from "@/hooks/use-element-width";
import { Disclosure } from "@/components/disclosure";

import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";


import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { toast } from "sonner";

import { Notice } from "@/components/notice";
import { SectionHeading } from "@/components/section";

import { capacityTone } from "@/lib/tone";
import { StatCard } from "@/components/stat-card";

import { TableCard } from "@/components/table-card";

import { SortableHead } from "@/components/sortable-head";
import { UserAvatar } from "@/components/user-avatar";

import { formatCpuSeconds, formatMillicores, formatUTCDay } from "@/lib/time";
import { formatMB } from "@/lib/bytes";
import { niceScale } from "@/lib/chart-scale";
import { nextSort, type SortState } from "@/lib/sort";
import { Clock, Cpu, HardDrive, EyeOff, Activity, Users, KeyRound, Globe } from "lucide-react";


import { api } from "@/lib/api";

import type { AppConsumption, AppUsage, AppUsageDay, AppUsageViewer, AppUsageViewers } from "@/lib/api";
import { QueryError } from "@/components/query-error";
import { QueryState } from "@/components/query-state";
import { EmptyState } from "@/components/empty-state";
import { FilterChips } from "@/components/filter-bar";

// ── Usage Tab ─────────────────────────────────────────────────

const USAGE_RANGES = [7, 30, 90] as const;

/** Fill the window so days without traffic render as zero-height bars. */
function densifyUsage(series: AppUsageDay[], days: number): AppUsageDay[] {
  const byDay = new Map(series.map((d) => [d.day, d]));
  const out: AppUsageDay[] = [];
  const today = new Date();
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate() - i));
    const key = d.toISOString().slice(0, 10);
    out.push(byDay.get(key) ?? { day: key, requests: 0, viewers: 0, page_loads: 0 });
  }
  return out;
}

const formatUsageDay = formatUTCDay;

/**
 * Demand and cost, over one window.
 *
 * `GET /apps/{id}/consumption` had been built, authorized by the app's own read
 * rule, and unrendered — the per-app half of the split `work-remaining.md` §5
 * records, where the estate half was answered by ADR §50 D7. That answer
 * settled the units, so this reuses them exactly: CPU-seconds and peak
 * megabytes, through the same formatters Admin → Usage draws its columns with,
 * so the estate figure and the app figure cannot disagree.
 *
 * **On this tab and over this tab's window**, not on a page of its own. "Which
 * apps matter and what do they cost" was two pages that never joined, and the
 * reason the estate answer works is that the two halves are over *one* period;
 * an app'scoped version on a separate surface with its own range picker would
 * reintroduce exactly the split it was built to close.
 */
export function UsageTab({
  appId,
  canSeeViewers,
  batch = false,
}: {
  appId: string;
  canSeeViewers: boolean;
  /** A batch app (ADR §107) serves no pages, so it has no requests and no
   * viewers to count — only what its runs spent. */
  batch?: boolean;
}) {
  const [days, setDays] = useState<number>(30);

  const { data: usage, isLoading, error, refetch } = useQuery<AppUsage>({
    queryKey: ["usage", appId, days],
    queryFn: () => api.getAppUsage(appId, days),
    refetchInterval: 60000,
    enabled: !batch,
  });

  // The same window, deliberately: the range buttons drive both.
  const { data: consumption, isLoading: consumptionLoading, error: consumptionError, refetch: refetchConsumption } = useQuery<AppConsumption>({
    queryKey: ["app-consumption", appId, days],
    queryFn: () => api.getAppConsumption(appId, days),
    retry: false,
  });

  const series = useMemo(
    () => densifyUsage(usage?.series ?? [], days),
    [usage?.series, days],
  );
  const hasTraffic = (usage?.totals.requests ?? 0) > 0;
  // Counting on, or figures recorded while it was (BB-43) — off stops the
  // counting, not the reading of what was counted. Never for an app nobody counted.
  const counting = usage?.audience_measurement === true;
  const audience = counting || usage?.totals.anonymous_browsers !== undefined;
  const cost = consumption?.total;

  // The envelope this app's peak is judged against, and how close it came.
  // Absent when the install sets no limit — a headroom figure against no ceiling
  // is a percentage of nothing.
  const memoryCeiling =
    consumption && consumption.memory_limit_mb > 0 ? consumption.memory_limit_mb : undefined;
  const memoryHeadroom =
    memoryCeiling && cost && cost.peak_memory_mb > 0
      ? capacityTone((cost.peak_memory_mb / memoryCeiling) * 100)
      : undefined;

  // What the scheduler holds for this app, stated beside what it spent. No
  // tone and no percentage: CPU-seconds against a reservation is not a ratio,
  // and a headroom figure invented out of two different units would read as a
  // measurement. Absent on a shared workload, where nothing reserves anything.
  const cpuReservation =
    consumption?.cpu_request_millicores
      ? formatMillicores(consumption.cpu_request_millicores)
      : undefined;

  return (
    <div className="space-y-6 pt-4">
      <SectionHeading
        title={`${batch ? "Consumption" : "Traffic and consumption"} — last ${days} days`}
        actions={
          <>
            {/* `aria-pressed`, not the fill, and a named group: "7d" beside "30d"
                says nothing about what is being chosen. */}
            <FilterChips
              label="How far back"
              value={String(days)}
              onChange={(v) => setDays(Number(v))}
              options={USAGE_RANGES.map((r) => ({ value: String(r), label: `${r} days` }))}
            />
          </>
        }
      />

      {/* One wrapper over the tiles *and* the chart, because they are one
          read. The empty state used to sit outside the error branch, so a
          failed read rendered "No traffic recorded yet. Counts appear within a
          minute of the first visit." directly beneath the card explaining that
          the figures could not be read — an invitation to wait for something
          that is not coming. Five tiles reading "—" over an empty chart is
          also exactly what an app with no traffic looks like. */}
      <QueryState
        query={
          batch
            ? { isLoading: consumptionLoading, error: consumptionError, refetch: refetchConsumption }
            : { isLoading, error, refetch }
        }
        loading={
          <>
            {/* Shaped like what lands: a batch app has two tiles and no chart. */}
            <div className={batch ? "grid grid-cols-2 gap-3" : "grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5"}>
              {Array.from({ length: batch ? 2 : 5 }).map((_, i) => <Skeleton key={i} className="h-24" />)}
            </div>
            {!batch && <Skeleton className="h-48 w-full" />}
          </>
        }
        title="This app’s usage could not be read"
        fallback="You need access to this app to see its usage."
      >
      <>
      {/* One row: three figures for demand and two for cost, over the one
          window. Splitting them into two blocks with two range pickers is the
          shape that made "which apps matter and what do they cost" unanswerable
          on the estate side. */}
      <div className={batch ? "grid grid-cols-2 gap-3" : "grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5"}>
        {!batch && (
          <>
            {/* Page loads lead: one per page a person opened, however chatty
                the framework. Requests stay beneath, for capacity. */}
            <StatCard
              label="Page loads"
              value={usage ? usage.totals.page_loads.toLocaleString() : "—"}
              description={usage ? `${usage.totals.requests.toLocaleString()} requests` : undefined}
              icon={<Activity className="h-4 w-4" />}
            />
            <StatCard
              label="People signed in"
              value={usage ? usage.totals.viewers.toLocaleString() : "—"}
              icon={<Users className="h-4 w-4" />}
            />
            {audience ? (
              <StatCard
                label="Anonymous browsers"
                value={(usage?.totals.anonymous_browsers ?? 0).toLocaleString()}
                description={counting ? "Estimated, ±1%" : "Recorded while counting was on"}
                icon={<Globe className="h-4 w-4" />}
              />
            ) : (
            <StatCard
              label="Requests / day"
              // "0 a day" beside "2 requests" contradicts the tile next to it;
              // a rate that rounds away says it is under one.
              value={
                usage
                  ? usage.totals.requests > 0 && usage.totals.requests / days < 1
                    ? "< 1"
                    : Math.round(usage.totals.requests / days).toLocaleString()
                  : "—"
              }
              icon={<Clock className="h-4 w-4" />}
            />
            )}
          </>
        )}
        <StatCard
          label="CPU time"
          // `formatCpuSeconds` dashes anything under a second, which on a
          // tile reads as "not measured" when it was measured and was tiny.
          value={
            cost
              ? cost.cpu_seconds < 1 && cost.observed_seconds > 0
                ? "< 1s"
                : formatCpuSeconds(cost.cpu_seconds)
              : "—"
          }
          description={cpuReservation ? `${cpuReservation} reserved` : undefined}
          icon={<Cpu className="h-4 w-4" />}
        />
        {/* The peak and the ceiling it is judged against, on the same tile.
            The limit used to be a muted sentence four rows below, unattached to
            the figure it explains — so an app sitting at 95% of its envelope
            said so nowhere a person would read it as a state. */}
        <StatCard
          label="Peak memory"
          value={cost ? formatMB(cost.peak_memory_mb) : "—"}
          description={memoryCeiling ? `of ${formatMB(memoryCeiling)} allowed` : undefined}
          tone={memoryHeadroom}
          icon={<HardDrive className="h-4 w-4" />}
        />
      </div>

      {/* The sentence a reporting surface has to carry rather than smooth over.
          A gap makes every figure beside it a floor, and an owner sizing an app
          against a limit needs to know which way it is wrong. */}
      {cost?.incomplete && (
        // A tinted line of text is colour carrying meaning on its own, which is
        // exactly what `Notice` exists to stop. It also began in lower case, as
        // a fragment appended to a full stop that was drawn separately.
        <Notice tone="attention" size="sm" title="These figures are a lower bound">
          {/* Not the server's sentence, which ends by restating this
              notice's title — Admin → Usage shows it alone, as a tooltip. */}
          Part of this period was not observed.
        </Notice>
      )}

      </>

      {batch ? null : !hasTraffic ? (
        <EmptyState size="inline" title={`No visits in the last ${days} days`} />
      ) : (
        <>
          <TableCard className="p-4">
            <UsageChart series={series} />
          </TableCard>

          <Disclosure label="View as table">
            <TableCard>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>Day</TableHead>
                    <TableHead className="text-right">Page loads</TableHead>
                    <TableHead className="text-right">Requests</TableHead>
                    <TableHead className="text-right">People</TableHead>
                    {audience && <TableHead className="text-right">Anonymous browsers</TableHead>}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {series.filter((d) => d.requests > 0).reverse().map((d) => (
                    <TableRow key={d.day}>
                      <TableCell className="text-xs">{formatUsageDay(d.day)}</TableCell>
                      <TableCell className="text-right text-xs">{d.page_loads.toLocaleString()}</TableCell>
                      <TableCell className="text-right text-xs">{d.requests.toLocaleString()}</TableCell>
                      <TableCell className="text-right text-xs">{d.viewers.toLocaleString()}</TableCell>
                      {audience && (
                        <TableCell className="text-right text-xs">
                          {d.anonymous_browsers === undefined ? "Not counted" : d.anonymous_browsers.toLocaleString()}
                        </TableCell>
                      )}
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableCard>
          </Disclosure>
        </>
      )}
      </QueryState>

      {canSeeViewers && !batch && <UsageViewersTable appId={appId} days={days} />}
    </div>
  );
}

type ViewerSortKey = "display_name" | "requests" | "days" | "first_day" | "last_day";

function viewerIcon(kind: AppUsageViewer["kind"]) {
  if (kind === "anonymous") return <EyeOff className="h-3.5 w-3.5 text-muted-foreground" />;
  if (kind === "api_key") return <KeyRound className="h-3.5 w-3.5 text-muted-foreground" />;
  return <Users className="h-3.5 w-3.5 text-muted-foreground" />;
}

/**
 * Who produced the traffic above. Anonymous visitors and API keys get their
 * own rows rather than being hidden, so this adds up to the totals beside it;
 * only the app's grant managers see it, which is why the whole section is
 * absent — not empty — for everyone else.
 */
/** A UTC date `n` days before today, as the API's YYYY-MM-DD. */
function utcDaysAgo(n: number): string {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() - n);
  return d.toISOString().slice(0, 10);
}

/**
 * The viewers as a file, one line per person per day, over any range — the
 * question an audit asks ("who opened this app this year") rather than the
 * ninety days the table can chart. A year by default.
 */
function ViewersDownload({ appId, retainedFrom }: { appId: string; retainedFrom: string }) {
  const [since, setSince] = useState(() => utcDaysAgo(364));
  const [until, setUntil] = useState(() => utcDaysAgo(0));
  const [busy, setBusy] = useState(false);

  async function download() {
    setBusy(true);
    try {
      const blob = await api.downloadAppUsageViewers(appId, since, until);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `viewers-${since}-to-${until}.csv`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
      if (retainedFrom && since < retainedFrom) {
        toast.warning(`This install keeps visits from ${formatUTCDay(retainedFrom)}, so the file starts there`);
      }
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t download the viewers");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Input type="date" aria-label="From" value={since} onChange={(e) => setSince(e.target.value)} className="h-8 w-36 text-xs md:text-xs" />
      <Input type="date" aria-label="To" value={until} onChange={(e) => setUntil(e.target.value)} className="h-8 w-36 text-xs md:text-xs" />
      <Button variant="ghost" size="sm" onClick={() => void download()} disabled={busy || !since || !until}>
        {busy ? "Downloading…" : "Download CSV"}
      </Button>
    </div>
  );
}

function UsageViewersTable({ appId, days }: { appId: string; days: number }) {
  const [sort, setSort] = useState<SortState<ViewerSortKey>>({ key: "requests", dir: "desc" });

  const { data, isLoading, error, refetch } = useQuery<AppUsageViewers>({
    queryKey: ["usage-viewers", appId, days],
    queryFn: () => api.getAppUsageViewers(appId, days),
    retry: false,
  });

  const viewers = data?.viewers ?? [];
  const rows = [...viewers].sort((a, b) => {
    const dir = sort.dir === "asc" ? 1 : -1;
    if (sort.key === "requests" || sort.key === "days") {
      return (a[sort.key] - b[sort.key]) * dir;
    }
    return a[sort.key].localeCompare(b[sort.key]) * dir;
  });

  const people = viewers.filter((v) => v.kind === "user").length;
  const anonymous = viewers.find((v) => v.kind === "anonymous");

  return (
    <div className="space-y-3">
      <SectionHeading
        title={`Viewers — last ${days} days`}
        actions={<ViewersDownload appId={appId} retainedFrom={data?.retained_from ?? ""} />}
        description={
          <>
            {people} signed-in {people === 1 ? "person" : "people"}
            {anonymous
              ? `, plus ${anonymous.requests.toLocaleString()} anonymous request${anonymous.requests === 1 ? "" : "s"}`
              : ""}
            . Visible only to people who manage sharing.
          </>
        }
      />

      {isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-8 w-full" />)}
        </TableCard>
      ) : error ? (
        <QueryError
          title="The viewers could not be read"
          error={error}
          fallback="Only people who manage this app’s sharing can see its viewers."
          onRetry={() => void refetch()}
        />
      ) : rows.length === 0 ? (
        <EmptyState
          size="inline"
          title={`Nobody has opened this app in the last ${days} days`}
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableHead label="Viewer" sortKey="display_name" sort={sort} onSort={(k) => setSort((s) => nextSort(s, k))} />
                <SortableHead label="Requests" sortKey="requests" sort={sort} onSort={(k) => setSort((s) => nextSort(s, k))} className="w-[120px] text-right" align="right" />
                {/* Who and how much stay; the span yields, and reappears under
                    the name at the width that drops it — a sortable column can
                    be hidden, but the fact it sorts by cannot vanish with it. */}
                <SortableHead label="Days active" sortKey="days" sort={sort} onSort={(k) => setSort((s) => nextSort(s, k))} className="hidden w-[120px] text-right @md:table-cell" align="right" />
                <SortableHead label="First seen" sortKey="first_day" sort={sort} onSort={(k) => setSort((s) => nextSort(s, k))} className="hidden w-[130px] @3xl:table-cell" />
                <SortableHead label="Last seen" sortKey="last_day" sort={sort} onSort={(k) => setSort((s) => nextSort(s, k))} className="hidden w-[130px] @xl:table-cell" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((v) => (
                <TableRow key={`${v.kind}-${v.id ?? "none"}`}>
                  <TableCell>
                    <div className="flex items-center gap-2">
                      {v.kind === "user" && v.avatar_url ? (
                        <UserAvatar id={v.id ?? undefined} name={v.display_name} avatarUrl={v.avatar_url} size="xs" />
                      ) : (
                        viewerIcon(v.kind)
                      )}
                      {v.kind === "user" && v.id ? (
                        <Link
                          to={`/people/${v.id}`}
                          className="text-sm font-medium text-foreground hover:text-primary hover:underline"
                        >
                          {v.display_name}
                        </Link>
                      ) : (
                        <span className="text-sm text-muted-foreground">{v.display_name}</span>
                      )}
                      {v.kind === "api_key" && (
                        <Badge variant="outline" className="text-2xs">API key</Badge>
                      )}
                    </div>
                    {/* Each restatement on its own column's container tier:
                        Days active `@md`, First seen `@3xl`, Last seen `@xl`.
                        Read on the viewport the ladders crossed both ways —
                        Days active was printed twice from 640px to the card's
                        `@md`, and Last seen was gone between `@xl` and
                        768px. */}
                    <span className="mt-0.5 flex flex-wrap gap-x-2 text-2xs text-muted-foreground @3xl:hidden">
                      <span className="@md:hidden">
                        {v.days} day{v.days === 1 ? "" : "s"} active
                      </span>
                      <span className="@3xl:hidden">
                        {formatUsageDay(v.first_day)}
                        <span className="@xl:hidden"> – {formatUsageDay(v.last_day)}</span>
                      </span>
                    </span>
                  </TableCell>
                  <TableCell className="text-right text-sm tabular-nums">
                    {v.requests.toLocaleString()}
                  </TableCell>
                  <TableCell className="hidden text-right text-sm tabular-nums text-muted-foreground @md:table-cell">
                    {v.days}
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @3xl:table-cell">
                    {formatUsageDay(v.first_day)}
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">
                    {formatUsageDay(v.last_day)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}
    </div>
  );
}

/**
 * Single-series daily request bars: rounded data-ends, 2px gaps, recessive
 * gridlines, per-bar hover tooltip carrying requests + viewers.
 */
function UsageChart({ series }: { series: AppUsageDay[] }) {
  const [hover, setHover] = useState<number | null>(null);

  const [wrapRef, W] = useElementWidth<HTMLDivElement>();
  const H = 180, PAD_L = 36, PAD_B = 18;
  // Headroom at the top, and it belongs to the *plot* rather than to the bars.
  // The bars were scaled into `plotH - 8` while the gridlines were drawn across
  // `plotH`, so a day at the maximum stopped eight pixels short of the line
  // labelled with its own number — and that line, at y=0, had its label
  // clipped by the top of the viewBox. One region now, and both read it.
  const PAD_T = 10;
  const plotW = W - PAD_L, plotH = H - PAD_B;
  // The axis is a round number at or above the busiest day, not the busiest day
  // itself: gridlines labelled `58` and `29` are a reading of the data rather
  // than a scale, and two apps' charts could not be compared.
  const { top: axisTop, ticks } = niceScale(Math.max(0, ...series.map((d) => d.requests)));
  /** Where a value sits, in the one coordinate space this chart has. */
  const yOf = (value: number) => plotH - (value / axisTop) * (plotH - PAD_T);
  const slot = plotW / series.length;
  const barW = Math.max(2, Math.min(24, slot - 2));
  const tickEvery = Math.ceil(series.length / 6);

  // Rounded top corners only; the baseline edge stays square.
  function barPath(x: number, h: number): string {
    if (h <= 0) return "";
    const r = Math.min(3, barW / 2, h);
    const top = plotH - h;
    return `M ${x} ${plotH} V ${top + r} Q ${x} ${top} ${x + r} ${top} H ${x + barW - r} Q ${x + barW} ${top} ${x + barW} ${top + r} V ${plotH} Z`;
  }

  const hovered = hover !== null ? series[hover] : null;

  return (
    <div ref={wrapRef} className="relative">
      {hovered && (
        <div
          className="pointer-events-none absolute -top-2 z-10 -translate-x-1/2 -translate-y-full rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-md"
          style={{ left: `${((PAD_L + (hover! + 0.5) * slot) / W) * 100}%` }}
        >
          <div className="font-medium">{formatUsageDay(hovered.day)}</div>
          <div className="text-muted-foreground">
            {hovered.page_loads.toLocaleString()} page load{hovered.page_loads === 1 ? "" : "s"} ·{" "}
            {hovered.requests.toLocaleString()} requests
            {hovered.viewers > 0 && ` · ${hovered.viewers.toLocaleString()} viewer${hovered.viewers === 1 ? "" : "s"}`}
          </div>
        </div>
      )}
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="block w-full"
        style={{ height: H }}
        role="img"
        aria-label="Daily requests"
        onMouseLeave={() => setHover(null)}
      >
        {ticks.map((t) => (
          <g key={t}>
            <line
              x1={PAD_L} x2={W} y1={yOf(t)} y2={yOf(t)}
              stroke="var(--border)" strokeWidth="1"
            />
            <text
              x={PAD_L - 6} y={yOf(t) + 3}
              textAnchor="end" className="text-2xs" fill="var(--muted-foreground)"
            >
              {t.toLocaleString()}
            </text>
          </g>
        ))}
        <line x1={PAD_L} x2={W} y1={plotH} y2={plotH} stroke="var(--border)" strokeWidth="1" />

        {series.map((d, i) => {
          const x = PAD_L + i * slot + (slot - barW) / 2;
          const h = plotH - yOf(d.requests);
          return (
            <g key={d.day}>
              {d.requests > 0 && (
                <path d={barPath(x, h)} fill="var(--primary)" fillOpacity={hover === i ? 1 : 0.8} />
              )}
              {/* Hit target wider than the mark */}
              <rect
                x={PAD_L + i * slot} y={0} width={slot} height={plotH}
                fill="transparent"
                onMouseEnter={() => setHover(i)}
              />
              {i % tickEvery === 0 && (
                <text
                  x={PAD_L + i * slot + slot / 2} y={H - 4}
                  textAnchor="middle" className="text-2xs" fill="var(--muted-foreground)"
                >
                  {formatUsageDay(d.day)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
    </div>
  );
}

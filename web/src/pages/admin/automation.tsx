import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { CalendarClock, ChevronLeft, ChevronRight, CircleSlash, Terminal } from "lucide-react";

import {
  api, kernelHoldsMemory, type AdminJobRun, type AdminJobRunList, type EstateSchedule, type PlatformSettings,
  type ScheduledJob, type ScheduledOverview, type UpcomingFiring, type UpcomingSchedules,
} from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { FilterBar, FilterChips } from "@/components/filter-bar";
import { AppOwner } from "@/components/app-owner";
import { StatusBadge } from "@/components/status-badge";
import { EmptyState } from "@/components/empty-state";
import { TableCard } from "@/components/table-card";
import { SectionHeading } from "@/components/section";
import { RelativeTime } from "@/components/relative-time";
import {
  JobRunProvenance, JobRunResult, JobRunStatusBadge,
} from "@/components/job-run-status";
import { jobRunInFlight, ordinal } from "@/lib/job-runs";
import {
  formatJobDuration, formatJobMemory, jobWhatRuns, placementSentence, refusalText,
} from "@/lib/automation";
import { describeCron } from "@/lib/cron-describe";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { splitSchedule } from "@/lib/schedule";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { Input } from "@/components/ui/input";
import { DayStrip } from "@/components/day-strip";
import {
  displayDayBounds, displayToday, displayZoneName, formatDayHeading, formatExactTime,
  formatHourMinute, isDayKey, shiftDay,
} from "@/lib/time";
import {
  KIND_LABEL, durationLong, durationShort, firingHref, firingName, heldLine, quietStretches,
  recurrencePhrase,
} from "@/lib/upcoming";

/**
 * Admin → Automation: what this install fires, and what has run.
 *
 * Toft has one automation concept, the job, declared in each app's toft.toml;
 * a document's re-render and an app's scheduled restart are listed beside the
 * jobs, because ADR §33 promises every schedule on the install. This page is read-only about definitions — a
 * schedule is changed in its app's repository, and paused on its app's page —
 * and says where runs go. What it *can* do is stop a run, because that is an operator's
 * decision about this machine rather than an edit to somebody's app.
 */

// A run's whole purpose is to change while nobody is looking at it — the same
// interval as Queue and Processes.
const REFRESH_MS = 10_000;

/** How many runs one read of this feed returns. There is no cursor here, so
 *  the page says when it has stopped at the cap rather than presenting its
 *  first page as the whole history. */
const RUN_FEED_LIMIT = 200;

const stateFilters = [
  { label: "All", value: "all" },
  { label: "Enabled", value: "enabled" },
  { label: "Paused", value: "paused" },
];

const runStatuses = [
  { label: "All", value: "" },
  { label: "In flight", value: "in_flight" },
  { label: "Failed", value: "unsuccessful" },
  { label: "Succeeded", value: "succeeded" },
  { label: "Cancelled", value: "cancelled" },
  { label: "Skipped", value: "skipped" },
];

const windows = [
  { label: "24 hours", value: "1" },
  { label: "7 days", value: "7" },
  { label: "30 days", value: "30" },
  { label: "All time", value: "" },
];

export function AdminAutomationPage() {
  return (
    <div className="space-y-8">
      <PageHeader title="Automation" />
      <UpcomingSection />
      <SchedulesSection />
      <JobRunsSection />
    </div>
  );
}

/** Where runs go and how memory is held, from the install's own settings. */
function placementLine(settings: PlatformSettings | undefined): string {
  if (!settings) return "";
  const where = placementSentence(settings.job_placement);
  if (!where) return "Jobs can’t run on this install.";
  if (settings.job_placement !== "this machine") return where;
  const memory = !settings.shared_containment
    ? ""
    : kernelHoldsMemory(settings.shared_containment)
      ? " Memory limits are enforced by the kernel."
      : " Memory limits are sampled by a watchdog.";
  return where + memory;
}

/** One line of the schedule list, whichever kind of schedule it is. */
interface ScheduleRow {
  key: string;
  name: string;
  /** What it does, under the name. */
  what: string;
  /** A job's timeout and memory; absent for the other kinds. */
  limits?: string;
  /** Where on the app the schedule is shown and paused. */
  to: string;
  app_id: string;
  app_name: string;
  owner_login?: string;
  owner_id?: string;
  owner_group_id?: string;
  schedule: string;
  enabled: boolean;
  next_run_at?: string;
  last_run_at?: string;
  /** The app's lock holds every firing without pausing the schedule. */
  app_locked?: boolean;
  last: ReactNode;
}

function jobRow(j: ScheduledJob): ScheduleRow {
  return {
    ...j,
    schedule: j.schedule ?? "",
    key: `job:${j.id}`,
    what: jobWhatRuns(j),
    limits: formatJobDuration(j.timeout_sec) + (j.override_memory_mb ? ` · ${formatJobMemory(j.override_memory_mb)}` : ""),
    to: `/apps/${j.app_id}/automation`,
    last: j.last_status ? <JobRunStatusBadge status={j.last_status} /> : null,
  };
}

function renderRow(r: EstateSchedule): ScheduleRow {
  return {
    ...r,
    key: `render:${r.id}`,
    name: "Re-render",
    what: "Rebuilds the document as a new release",
    to: `/apps/${r.app_id}/settings`,
    // The deployment the firing started, so its status is a deploy's.
    last: r.last_status ? <StatusBadge status={r.last_status} /> : null,
  };
}

/** A restart firing's outcome, which is its own word and not a run's. */
const RESTART_OUTCOME: Record<string, { label: string; tone: "ok" | "neutral" | "danger" }> = {
  restarted: { label: "Restarted", tone: "ok" },
  skipped: { label: "Skipped", tone: "neutral" },
  failed: { label: "Failed", tone: "danger" },
};

function restartRow(r: EstateSchedule): ScheduleRow {
  const o = r.last_status ? RESTART_OUTCOME[r.last_status] : undefined;
  return {
    ...r,
    key: `restart:${r.id}`,
    name: "Restart",
    what: "Restarts the app if it is running",
    to: `/apps/${r.app_id}/settings#runtime`,
    last: o ? <StatusBadge status={r.last_status ?? ""} label={o.label} tone={o.tone} /> : null,
  };
}

/** Re-reads the clock once a minute, so today's "now" marker moves. */
function useMinuteClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 60_000);
    return () => window.clearInterval(t);
  }, []);
  return now;
}

/**
 * What fires on one day, in the reader's zone (ADR §156) — the firings the
 * schedules below produce, which is what a maintenance window is planned
 * around. The server expands every expression; this draws what it returned.
 * The day lives in the URL, so "what runs on the night of the upgrade" can be
 * sent as a link.
 */
function UpcomingSection() {
  const now = useMinuteClock();
  const zone = displayZoneName();
  const today = displayToday(new Date(now), zone);
  const [dayParam, setDayParam] = useUrlParam("day");
  const day = isDayKey(dayParam) ? dayParam : today;
  const setDay = (d: string) => setDayParam(d === today ? "" : d);
  const { from, to } = displayDayBounds(day, zone);
  const passed = to.getTime() <= now;

  const { data, isLoading, error, refetch } = useQuery<UpcomingSchedules>({
    queryKey: ["system-upcoming", day, zone],
    queryFn: () => api.getUpcomingSchedules(from, to),
    enabled: !passed,
    refetchInterval: 60_000,
    retry: false,
  });

  const when = day === today ? "today" : day === shiftDay(today, 1) ? "tomorrow" : "on this day";
  const held = data ? heldLine(data.held, when) : "";
  const quiet = data ? quietStretches(data, Math.max(from.getTime(), now), to.getTime()) : [];
  const empty = data && data.firings.length === 0 && data.recurring.length === 0;

  return (
    <section className="space-y-4">
      <SectionHeading
        title="Upcoming"
        description={`Times in ${zone}. As scheduled now — a deploy can change it.`}
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              size="icon-sm"
              aria-label="Previous day"
              disabled={day <= today}
              onClick={() => setDay(shiftDay(day, -1))}
            >
              <ChevronLeft />
            </Button>
            <span className="min-w-[9.5rem] text-center text-sm font-medium tabular-nums">
              {formatDayHeading(day)}
            </span>
            <Button variant="outline" size="icon-sm" aria-label="Next day" onClick={() => setDay(shiftDay(day, 1))}>
              <ChevronRight />
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setDay(today)}>Today</Button>
            <Button variant="ghost" size="sm" onClick={() => setDay(shiftDay(today, 1))}>Tomorrow</Button>
            <Input
              type="date"
              aria-label="Day"
              min={today}
              value={day}
              onChange={(e) => isDayKey(e.target.value) && setDay(e.target.value)}
              className="h-8 w-36 text-xs md:text-xs"
            />
          </div>
        }
      />

      {passed ? (
        <EmptyState
          size="inline"
          icon={<CalendarClock className="h-6 w-6" />}
          title="This day has passed"
          description="What ran is in Job runs below."
        />
      ) : error ? (
        <QueryError
          title="Upcoming firings could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : isLoading || !data ? (
        <TableCard className="space-y-2 p-4">
          <Skeleton className="h-16" />
          <Skeleton className="h-10" />
        </TableCard>
      ) : (
        <>
          <TableCard className="space-y-3 p-4">
            {held && <p className="text-xs text-muted-foreground">{held}</p>}
            <DayStrip data={data} from={from.getTime()} to={to.getTime()} now={now} />
            <div className="space-y-1 text-xs">
              <p>
                <span
                  className="font-medium"
                  title="Only scheduled work is counted. A run an app, a person or the CLI starts, a deploy, and a run that outlasts its usual length are not."
                >
                  {empty ? "Nothing scheduled" : quiet.length ? "Nothing else scheduled" : "Something is scheduled in every half hour"}
                </span>
                {quiet.length > 0 && !empty && (
                  <span className="text-muted-foreground tabular-nums">
                    {"  "}
                    {quiet
                      .map((g) => `${formatHourMinute(g.start)}–${g.end === to.getTime() ? "24:00" : formatHourMinute(g.end)}`)
                      .join(" · ")}
                  </span>
                )}
              </p>
              <p className="text-muted-foreground">
                Anything due while this install is down runs once when it is back.
              </p>
            </div>
          </TableCard>

          {!empty && <UpcomingAgenda data={data} now={now} />}
        </>
      )}
    </section>
  );
}

/** The day's firings in time order, frequent schedules first as "All day". */
function UpcomingAgenda({ data, now }: { data: UpcomingSchedules; now: number }) {
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            {/* Time, name and duration survive at every width — the three
                facts a window is chosen from. */}
            <TableHead className="w-[80px]">Time</TableHead>
            <TableHead className="w-full">Schedule</TableHead>
            <TableHead className="hidden @xl:table-cell">Kind</TableHead>
            <TableHead className="min-w-[8rem]">Duration</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.recurring.map((r) => (
            <AgendaRow key={`r:${r.schedule_id}`} f={r} time="All day" duration={recurrencePhrase(r)} past={false} />
          ))}
          {data.firings.map((f) => (
            <AgendaRow
              key={`${f.schedule_id}:${f.at}`}
              f={f}
              time={formatHourMinute(f.at)}
              duration={durationShort(f)}
              past={Date.parse(f.at) < now}
            />
          ))}
        </TableBody>
      </Table>
    </TableCard>
  );
}

function AgendaRow({ f, time, duration, past }: {
  f: UpcomingFiring;
  time: string;
  duration: string;
  past: boolean;
}) {
  const words = describeCron(f.schedule) ?? f.schedule;
  return (
    <TableRow className={past ? "text-muted-foreground" : undefined}>
      <TableCell
        className="text-xs tabular-nums"
        title={`${formatExactTime(f.at, "UTC")} · ${words} (${f.zone || "UTC"})`}
      >
        {time}
      </TableCell>
      <TableCell className="max-w-0">
        <div className="truncate font-medium">{firingName(f)}</div>
        <Link
          to={firingHref(f)}
          className="block truncate text-xs text-muted-foreground hover:text-primary hover:underline"
        >
          {f.app_name}
        </Link>
      </TableCell>
      <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">{KIND_LABEL[f.kind]}</TableCell>
      {/* Wraps rather than truncates: "every 5 minutes, 288 times" is the
          payload, and the name beside it needs the width more. */}
      <TableCell className="min-w-[8rem] text-xs whitespace-normal text-muted-foreground" title={f.count ? undefined : durationLong(f)}>
        {duration}
      </TableCell>
    </TableRow>
  );
}

function SchedulesSection() {
  // In the URL, so a narrowed list can be sent to somebody. Distinct names
  // from the job-run feed's below, which shares the address.
  const [search, setSearch] = useUrlParam("q");
  const [state, setState] = useUrlParam("state", "all");

  const { data, isLoading, error, refetch } = useQuery<ScheduledOverview>({
    queryKey: ["system-scheduled"],
    queryFn: api.getScheduledOverview,
    retry: false,
  });
  // Admin-only, like this page; a failed read only drops the placement line.
  const { data: settings } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
    retry: false,
  });

  if (error) {
    return (
      <section className="space-y-3">
        <SectionHeading title="Schedules" />
        <QueryError
          title="Schedules could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  const rows: ScheduleRow[] = [
    ...(data?.jobs ?? []).map(jobRow),
    ...(data?.renders ?? []).map(renderRow),
    ...(data?.restarts ?? []).map(restartRow),
  ].sort(
    (a, b) => a.app_name.localeCompare(b.app_name) || a.name.localeCompare(b.name),
  );

  const enabled = rows.filter((r) => r.enabled).length;
  const q = search.toLowerCase();
  const narrowed = rows.filter(
    (r) =>
      !q ||
      r.name.toLowerCase().includes(q) ||
      r.app_name.toLowerCase().includes(q) ||
      (r.owner_login ?? "").toLowerCase().includes(q),
  );
  const stateCounts: Record<string, number> = {
    all: narrowed.length,
    enabled: narrowed.filter((r) => r.enabled).length,
    paused: narrowed.filter((r) => !r.enabled).length,
  };
  const shown = narrowed.filter((r) => {
    if (state === "enabled" && !r.enabled) return false;
    if (state === "paused" && r.enabled) return false;
    return true;
  });
  const placement = placementLine(settings);

  return (
    <section className="space-y-4">
      <SectionHeading
        title="Schedules"
        description={
          isLoading || !data || rows.length === 0
            ? placement || undefined
            : `${enabled} running on schedule, ${rows.length - enabled} paused. ${placement}`
        }
      />

      {isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : rows.length === 0 ? (
        <EmptyState
          size="inline"
          icon={<CalendarClock className="h-6 w-6" />}
          title="Nothing is scheduled"
          description="A job, a re-render or a restart gets a schedule in its app’s toft.toml."
        />
      ) : (
        <div className="space-y-4">
          <div className="flex flex-wrap items-center gap-4">
            <SearchInput
              placeholder="Search by schedule, app, or owner…"
              value={search}
              onChange={setSearch}
              className="w-full sm:max-w-sm"
            />
            <FilterBar
              options={stateFilters.map((f) => ({
                ...f,
                label: `${f.label} (${stateCounts[f.value] ?? 0})`,
              }))}
              value={state}
              onChange={setState}
            />
          </div>

          {shown.length === 0 ? (
            <EmptyState
              size="inline"
              title="No schedules match"
              action={
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    setSearch("");
                    setState("all");
                  }}
                >
                  Clear
                </Button>
              }
            />
          ) : (
          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {/* What survives at every width is the job and whether it is
                      running on its schedule; the rest yield in the order that
                      leaves that pair readable. */}
                  <TableHead className="w-full">Schedule</TableHead>
                  <TableHead className="hidden @md:table-cell">App</TableHead>
                  <TableHead className="hidden @4xl:table-cell">Owner</TableHead>
                  <TableHead className="hidden @xl:table-cell">When</TableHead>
                  <TableHead className="w-[110px]">State</TableHead>
                  <TableHead className="hidden w-[130px] @3xl:table-cell">Next run</TableHead>
                  <TableHead className="hidden w-[170px] @2xl:table-cell">Last run</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((r) => {
                  const { zone, expr } = splitSchedule(r.schedule);
                  return (
                  <TableRow key={r.key}>
                    <TableCell className="max-w-0 font-medium">
                      <div className="truncate">{r.name}</div>
                      <div
                        className={`truncate text-xs font-normal text-muted-foreground ${r.limits === undefined ? "" : "font-mono"}`}
                      >
                        {r.what}
                      </div>
                      {r.limits !== undefined && (
                        <div className="truncate text-2xs font-normal text-muted-foreground">{r.limits}</div>
                      )}
                    </TableCell>
                    {/* Bounded, not `max-w-0`: with Job taking `w-full`, a
                        zero-floored App column was squeezed to "Ba…". */}
                    <TableCell className="hidden max-w-[12rem] @md:table-cell">
                      <Link
                        to={r.to}
                        className="block truncate text-sm text-foreground hover:text-primary hover:underline"
                      >
                        {r.app_name}
                      </Link>
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @4xl:table-cell">
                      <AppOwner
                        ownerId={r.owner_id}
                        ownerGroupId={r.owner_group_id}
                        ownerLogin={r.owner_login}
                        withCount={false}
                      />
                    </TableCell>
                    <TableCell className="hidden text-xs @xl:table-cell">
                      <div>{describeCron(expr) ?? <span className="font-mono">{expr}</span>}</div>
                      <div className="text-muted-foreground">{zone || "UTC"}</div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={r.enabled ? "active" : "paused"} />
                    </TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground @3xl:table-cell">
                      {/* A paused schedule has a stored next_run_at that will
                          never arrive; showing it would read as a promise. */}
                      {/* A lock holds a schedule without pausing it: the scheduler
                          skips every firing, so a next run would be a promise too. */}
                      {r.enabled && r.app_locked ? (
                        "Held by lock"
                      ) : r.enabled && r.next_run_at ? (
                        <RelativeTime iso={r.next_run_at} />
                      ) : (
                        "—"
                      )}
                    </TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground @2xl:table-cell">
                      {r.last_run_at ? (
                        <span className="flex items-center gap-2">
                          <RelativeTime iso={r.last_run_at} />
                          {r.last}
                        </span>
                      ) : (
                        "Never"
                      )}
                    </TableCell>
                  </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </TableCard>
          )}
        </div>
      )}

      <p className="text-xs text-muted-foreground">
        A schedule runs in UTC unless toft.toml names a time zone. Pausing lasts until the app’s next deploy.
      </p>
    </section>
  );
}

/**
 * Every background job run on the install.
 *
 * Deliberately not shaped like Admin → Queue. Builds share one in-memory
 * channel, so that page can number them; runs are limited **per app**, so a
 * queued run's position is its place in its own app's line and is labelled
 * that way. There is no install-wide queue to be third in.
 *
 * The status chips group, but no row ever does: "Failed" covers the three ways a
 * run ends badly because that is the question somebody arrives with, while each
 * row still says which one it was — a bigger memory limit, less work, and a code
 * change are different fixes.
 */
function JobRunsSection() {
  const queryClient = useQueryClient();
  const [status, setStatus] = useUrlParam("status");
  const [days, setDays] = useUrlParam("window", "7");
  const [cancelling, setCancelling] = useState<AdminJobRun | null>(null);

  const { data, isLoading, error, refetch } = useQuery<AdminJobRunList>({
    queryKey: ["system-job-runs", status, days],
    queryFn: () =>
      api.getAllJobRuns({
        status: status || undefined,
        days: days ? Number(days) : undefined,
        limit: RUN_FEED_LIMIT,
      }),
    refetchInterval: REFRESH_MS,
    retry: false,
  });

  if (error) {
    return (
      <section className="space-y-3">
        <SectionHeading title="Job runs" />
        <QueryError
          title="Job runs could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  const runs = data?.runs ?? [];
  const anyInFlight = runs.some((r) => jobRunInFlight(r.status));
  // A feed that stops at its cap and says nothing reads as the whole history.
  const capped = runs.length >= RUN_FEED_LIMIT;

  return (
    <section className="space-y-4">
      <SectionHeading
        title="Job runs"
      />

      {/* An install-level refusal, in the words the platform gives it. Without
          this an install that never turned jobs on shows an empty table that
          reads as "nothing has run lately". */}
      {data && !data.jobs_available && (
        <Notice title="Background jobs aren’t available on this install">
          {data.unavailable_bec}
        </Notice>
      )}

      {/* Uncounted, and it is the exception the rule already names: both of
          these are request parameters, so a count per option would be a
          request per option. */}
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2">
        {/* Status narrows runs that exist; with none in the window and none
            chosen, six chips would narrow nothing. The window stays, because
            widening it is the way to find some. */}
        {(isLoading || runs.length > 0 || status) && (
          <FilterChips label="Filter runs by status" options={runStatuses} value={status} onChange={setStatus} />
        )}
        <FilterChips label="How far back" options={windows} value={days} onChange={setDays} />
      </div>

      {isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : runs.length === 0 ? (
        <EmptyState
          size="inline"
          icon={<Terminal className="h-6 w-6" />}
          // An empty table means three different things, and only the last one
          // is "this install runs no jobs" — saying that when a filter is on
          // would be wrong in the direction somebody acts on.
          title={
            status
              ? "No runs match"
              : days
                ? `Nothing has run in the last ${windows.find((w) => w.value === days)?.label ?? `${days} days`}`
                : "No job runs yet"
          }
          description={
            status || days
              ? undefined
              : "Runs from schedules, Run now, the CLI and apps themselves all appear here."
          }
          action={
            status || days ? (
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  setStatus("");
                  setDays("");
                }}
              >
                {status ? "Clear" : "Show all time"}
              </Button>
            ) : undefined
          }
        />
      ) : (
        <TableCard>
          {capped && (
            <p className="border-b border-border/60 px-4 py-2 text-xs text-muted-foreground">
              Showing the {RUN_FEED_LIMIT} most recent. Narrow the filters to see older runs.
            </p>
          )}
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                {/* Queued, app, job and status stay: when, whose, which, and
                    how it went. Who started it and the result line yield. */}
                <TableHead className="hidden w-[130px] @md:table-cell">Queued</TableHead>
                <TableHead>App</TableHead>
                <TableHead>Job</TableHead>
                <TableHead className="hidden @3xl:table-cell">Started by</TableHead>
                <TableHead className="w-[170px]">Status</TableHead>
                <TableHead className="hidden @4xl:table-cell">Result</TableHead>
                {/* Only while a run is in flight: an always-empty column
                    cost a phone 11px of overflow for nothing. */}
                {anyInFlight && <TableHead className="w-16 text-right">Cancel</TableHead>}
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.map((run) => (
                <TableRow key={run.id}>
                  <TableCell className="hidden whitespace-nowrap text-sm text-muted-foreground @md:table-cell">
                    <RelativeTime iso={run.queued_at} />
                  </TableCell>
                  <TableCell>
                    <Link
                      to={`/apps/${run.app_id}`}
                      className="font-medium text-foreground hover:text-primary hover:underline"
                    >
                      {run.app_name}
                    </Link>
                  </TableCell>
                  <TableCell className="text-sm">
                    {run.job}
                    {/* The Provenance column's own tier, on the card's width:
                        read on the viewport it printed the fact twice between
                        1024px and the card's `@3xl`. */}
                    <span className="mt-0.5 block text-2xs text-muted-foreground @3xl:hidden">
                      <JobRunProvenance run={run} />
                    </span>
                  </TableCell>
                  <TableCell className="hidden @3xl:table-cell">
                    <JobRunProvenance run={run} />
                  </TableCell>
                  <TableCell>
                    <div className="flex items-center gap-2">
                      <JobRunStatusBadge status={run.status} />
                      {/* Where a pending run stands *in its own app's line*, and
                          how far a running one has got. Both answer "how long is
                          this going to be". */}
                      {run.status === "queued" && run.queue_position ? (
                        <span className="whitespace-nowrap text-xs text-muted-foreground">
                          {ordinal(run.queue_position)} for this app
                        </span>
                      ) : null}
                      {run.status === "running" && run.progress > 0 ? (
                        <span className="text-xs text-muted-foreground">{run.progress}%</span>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell className="hidden max-w-xs truncate text-xs text-muted-foreground @4xl:table-cell">
                    <JobRunResult run={run} />
                  </TableCell>
                  {anyInFlight && (
                  <TableCell className="text-right">
                    {jobRunInFlight(run.status) && (
                      <Button
                        variant="ghost"
                        size="sm"
                        title="Cancel this run"
                        aria-label={`Cancel this run of ${run.job} on ${run.app_name}`}
                        onClick={() => setCancelling(run)}
                      >
                        <CircleSlash className="h-4 w-4 text-destructive" />
                      </Button>
                    )}
                  </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      <p className="text-xs text-muted-foreground">
        {data?.concurrency_limit
          ? `Each app runs up to ${data.concurrency_limit} job${data.concurrency_limit === 1 ? "" : "s"} at once${
              data.max_queued ? `, with up to ${data.max_queued} waiting` : ""
            }. `
          : null}
        Logs are kept for 14 days, run history for 90.
      </p>

      <CancelRunDialog
        run={cancelling}
        onOpenChange={(open) => !open && setCancelling(null)}
        onCancelled={() => {
          setCancelling(null);
          queryClient.invalidateQueries({ queryKey: ["system-job-runs"] });
        }}
      />
    </section>
  );
}

/**
 * Stopping somebody else's run.
 *
 * The owner's Automation tab cancels with no dialog, because it is their app
 * and their run in front of them. Here it is neither: an operator is stopping
 * work another team started, and half of these runs were started by an app
 * rather than a person, so nobody is watching for it to stop. The dialog says
 * whose app and which job, and that the run cannot be resumed — only started
 * again, from the app.
 */
function CancelRunDialog({
  run,
  onOpenChange,
  onCancelled,
}: {
  run: AdminJobRun | null;
  onOpenChange: (open: boolean) => void;
  onCancelled: () => void;
}) {
  const [busy, setBusy] = useState(false);

  async function submit() {
    if (!run) return;
    setBusy(true);
    try {
      await api.cancelJobRun(run.app_id, run.id);
      toast.success(`“${run.job}” cancelled on “${run.app_name}”`);
      onCancelled();
    } catch (err) {
      toast.error(refusalText(err, `Couldn’t cancel “${run.job}”`));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={run !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Cancel this run?</DialogTitle>
          <DialogDescription>
            {run
              ? `${run.job} on ${run.app_name} stops now and can’t be resumed. Whoever started it isn’t told.`
              : ""}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            Leave it running
          </Button>
          <Button variant="destructive" onClick={submit} disabled={busy}>
            {busy ? "Cancelling…" : "Cancel the run"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

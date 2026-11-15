import { BarChart3, Users, Moon, LockKeyhole } from "lucide-react";
import { useState } from "react";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { toneSurface } from "@/lib/tone";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  api,
  type ConsumptionSummary,
  type UsageOverview,
  type QuietApp,
  type QuietAppList,
} from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { FilterChips } from "@/components/filter-bar";
import { Sparkline } from "@/components/sparkline";
import { SortableHead } from "@/components/sortable-head";
import { EmptyState } from "@/components/empty-state";
import { TableCard } from "@/components/table-card";
import { QueryError } from "@/components/query-error";
import { SectionHeading } from "@/components/section";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { LockAppDialog } from "@/components/lock-app-dialog";
import { toast } from "sonner";
import { useQueryClient } from "@tanstack/react-query";
import { nextSort, type SortState } from "@/lib/sort";
import { formatCpuSeconds, formatUTCDay, formatUTCMonthLong, utcDayKeys } from "@/lib/time";
import { formatMB } from "@/lib/bytes";
import { AppOwner } from "@/components/app-owner";
import { AudienceSection } from "@/pages/admin/usage-audience";

const windowFilters = [
  { label: "7 days", value: "7" },
  { label: "30 days", value: "30" },
  { label: "90 days", value: "90" },
];

/** A day with no row is a day with no traffic, and a gap in a sparkline reads
 * as missing data rather than as zero. Fill the window before drawing it. */
function fillDays(series: { day: string; requests: number }[], days: number): number[] {
  const byDay = new Map(series.map((d) => [d.day, d.requests]));
  return utcDayKeys(days).map((day) => byDay.get(day) ?? 0);
}

/** The buckets are UTC days, and `formatUTCDay` is the one place that says so
 *  — this had its own parse-at-noon workaround for the same problem. */
const formatDay = formatUTCDay;

type SortKey = "app" | "requests" | "viewers" | "cpu" | "peak";

const quietFilters = [
  { label: "30 days", value: "30" },
  { label: "90 days", value: "90" },
  { label: "6 months", value: "180" },
  { label: "1 year", value: "365" },
];

/**
 * A raw day count stops being readable somewhere around three digits: "quiet
 * for 1,204 days" is a number to convert rather than a fact to act on. The
 * exact figure is still available — it is what the column sorts on, and the
 * precise date sits in the column beside it.
 */
function formatQuietFor(days: number): string {
  if (days < 90) return `${days} day${days === 1 ? "" : "s"}`;
  if (days < 730) {
    const months = Math.round(days / 30);
    return `${months} month${months === 1 ? "" : "s"}`;
  }
  return `${(days / 365).toFixed(1)} years`;
}

/**
 * Why the clock starts where it does. Without this the number is untrustworthy
 * in the one direction that matters: an app deployed last night with no traffic
 * and an app abandoned in January both show "no visits", and only one of them
 * is a candidate for anything.
 */
function quietBasisPhrase(a: QuietApp): string {
  switch (a.quiet_basis) {
    case "visit":
      return "Not opened since";
    case "deploy":
      return "Deployed, never opened since";
    case "retention":
      // The visits that would answer this have aged out of the record, so the
      // clock starts at the oldest day the install still keeps.
      return "Not opened in the retained record since";
    default:
      return "Never deployed or opened";
  }
}

/**
 * What to say to somebody who opens a retired app.
 *
 * Seeded from the row's own evidence rather than left blank, because the person
 * who arrives at a locked app is usually the one person who still wanted it,
 * and "quiet since March 2025" is what tells them whether that is a mistake.
 * Deliberately names no channel or team: this install may have neither, and a
 * default that invents one is a default that gets sent unedited.
 */
function defaultLockMessage(a: QuietApp): string {
  const since = formatUTCMonthLong(a.quiet_since.slice(0, 10));
  return `Retired — nobody has opened this since ${since}. Ask an administrator if you still need it.`;
}

/**
 * Admin → Usage § Quiet apps — the estate's other end.
 *
 * The table above answers "which apps matter" and cannot answer this one, for a
 * structural reason rather than a presentational one: its last-visit is capped
 * at the window it charts, so at 30 days every app quiet for longer reports the
 * same empty answer and they all sort together at the bottom. This section
 * calls a different endpoint whose last-visit is unwindowed — `app_usage_daily`
 * keeps everything unless the access record's retention says otherwise, so it
 * can see back as far as the install keeps.
 *
 * Quiet is measured from the most recent evidence anybody wanted the app: a
 * visit, a successful deploy, or failing both the day it was created. That is
 * what keeps an app deployed last night off a list of things to retire.
 *
 * It is a list to read, not a verdict. Low traffic is not low value — a
 * quarter-end report opened four times a year is quiet by every measure here
 * and doing exactly its job — so the section names candidates and leaves the
 * decision with the person reading it.
 */
function QuietAppsSection({ search, onClear }: { search: string; onClear: () => void }) {
  // In the URL with the page's search and window: a narrowed report is
  // something you send to somebody.
  const [range, setRange] = useUrlParam("quiet", "90");
  const days = Number(range);
  const queryClient = useQueryClient();
  // The row a Lock decision is open on. Null closes the dialog, and holding the
  // row rather than an id is what lets it seed the message from the evidence.
  const [locking, setLocking] = useState<QuietApp | null>(null);
  const [unlocking, setUnlocking] = useState("");

  // Unlocking is a single reversible click and takes no confirmation, which is
  // the same rule the Sharing tab follows. The toast says what did *not*
  // happen, because the obvious assumption is wrong: it restores permission,
  // not the process.
  async function unlock(a: QuietApp) {
    setUnlocking(a.app_id);
    try {
      await api.unlockApp(a.app_id);
      queryClient.invalidateQueries({ queryKey: ["quiet-apps"] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      toast.success(`“${a.name}” unlocked — it’s stopped until somebody starts it`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t unlock “${a.name}”`);
    } finally {
      setUnlocking("");
    }
  }

  const { data, isLoading, error, refetch } = useQuery<QuietAppList>({
    queryKey: ["quiet-apps", days],
    queryFn: () => api.getQuietApps(days),
    retry: false,
  });

  const q = search.toLowerCase();
  const shown = (data?.apps ?? []).filter(
    (a) =>
      !q ||
      a.name.toLowerCase().includes(q) ||
      a.slug.toLowerCase().includes(q) ||
      (a.owner_login ?? "").toLowerCase().includes(q) ||
      (a.owner_group ?? "").toLowerCase().includes(q),
  );
  // Over the rows on screen, not over the whole answer. This section shares
  // the page's search box and nothing else, so typing a name used to leave the
  // heading reporting unfiltered totals above a table showing one row — the
  // page stating a count that contradicted what was under it.
  const lockedCount = shown.filter((a) => a.locked).length;

  return (
    <div className="space-y-4">
      <SectionHeading
        title="Quiet apps"
        description={
          !data
            ? "Apps nobody has opened or deployed for a long time."
            : q
              ? `${shown.length} of ${data.apps.length} quiet app${data.apps.length === 1 ? "" : "s"} match “${search}”${lockedCount > 0 ? `, ${lockedCount} of them already locked` : ""}.`
              : `${data.apps.length} of ${data.total} app${data.total === 1 ? "" : "s"} have had no visitor or deploy for at least ${days} days${lockedCount > 0 ? `, ${lockedCount} already locked` : ""}.`
        }
        actions={
          <FilterChips
            label="Quiet for at least"
            options={quietFilters}
            value={range}
            onChange={setRange}
          />
        }
      />

      {/* Error before empty: a failed read rendered as "nothing is quiet" is
          the wrong sentence at the worst moment. */}
      {error ? (
        <QueryError
          title="Quiet apps could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : shown.length === 0 ? (
        q ? (
          <EmptyState
            size="inline"
            title="No quiet apps match"
            action={
              <Button variant="outline" size="sm" onClick={onClear}>
                Clear
              </Button>
            }
          />
        ) : (
          <EmptyState
            size="inline"
            icon={<Moon className="h-6 w-6" />}
            title="Nothing has gone quiet"
            description={`Every app on this install has been opened, deployed or created within the last ${days} days.`}
          />
        )
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">App</TableHead>
                <TableHead className="hidden @3xl:table-cell">Owner</TableHead>
                <TableHead className="text-right">Quiet for</TableHead>
                {/* The reason and the date travel together — the phrase is
                    meaningless without the date and the date is ambiguous
                    without the phrase. */}
                <TableHead className="hidden @xl:table-cell">Since</TableHead>
                <TableHead className="hidden text-right @4xl:table-cell">Requests, all time</TableHead>
                {/* The decision this list exists to inform, taken on the row
                    that informed it — rather than by opening each app in turn
                    and finding the same control on its Sharing tab. */}
                <TableHead className="text-right">Action</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((a) => (
                <TableRow key={a.app_id}>
                  <TableCell className="max-w-0">
                    <span className="flex min-w-0 items-center gap-2">
                      <Link
                        to={`/apps/${a.app_id}/usage`}
                        className="truncate font-medium text-foreground hover:text-primary hover:underline"
                      >
                        {a.name}
                      </Link>
                      {/* Already dealt with. A chip rather than a filter that
                          hides it: an administrator who locked this last month
                          needs to see the decision standing, not to lose the
                          row and wonder whether the app went away. */}
                      {a.locked && (
                        <Badge variant="outline" className={`shrink-0 gap-1 text-2xs ${toneSurface.attention}`}>
                          <LockKeyhole className="h-3 w-3" aria-hidden /> Locked
                        </Badge>
                      )}
                    </span>
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                    {a.owner_group ? (
                      <span className="inline-flex min-w-0 items-center gap-1.5">
                        <Users className="h-3.5 w-3.5 shrink-0" aria-hidden />
                        <span className="truncate">{a.owner_group}</span>
                      </span>
                    ) : (
                      // AppOwner, so this column names an owner the way
                      // every other page does.
                      <AppOwner ownerId={a.owner_id} ownerLogin={a.owner_login} withCount={false} />
                    )}
                  </TableCell>
                  <TableCell className="text-right font-medium">
                    {formatQuietFor(a.quiet_days)}
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                    {quietBasisPhrase(a)}{" "}
                    <span className="text-foreground">{formatDay(a.quiet_since.slice(0, 10))}</span>
                  </TableCell>
                  <TableCell className="hidden text-right text-muted-foreground @4xl:table-cell">
                    {a.requests.toLocaleString()}
                  </TableCell>
                  <TableCell className="text-right">
                    {a.locked ? (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={unlocking === a.app_id}
                        onClick={() => unlock(a)}
                      >
                        {unlocking === a.app_id ? "Unlocking…" : "Unlock"}
                      </Button>
                    ) : (
                      <Button variant="outline" size="sm" onClick={() => setLocking(a)}>
                        <LockKeyhole className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                        Lock
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      {locking && (
        <LockAppDialog
          key={locking.app_id}
          appId={locking.app_id}
          appName={locking.name}
          open
          onOpenChange={(o) => { if (!o) setLocking(null); }}
          defaultMessage={defaultLockMessage(locking)}
        />
      )}

      <p className="text-xs text-muted-foreground">
        API key traffic counts as a visit.
      </p>
    </div>
  );
}

/**
 * Admin → Usage: which apps matter, and what they cost.
 *
 * One page and one table, because it is one question. It was two — Usage had
 * requests, viewers and last visit; Metrics had CPU time and memory — and an
 * administrator deciding what to retire needs them joined: an app nobody has
 * opened in ninety days that is holding a gigabyte resident is the row that
 * decision is made from, and neither page could draw it (ADR §50 D7). The
 * window applies to both halves, so demand and cost are always over the same
 * period.
 *
 * Counts and no identities. Who visited a particular app is on that app's own
 * Usage tab, behind manage_grants — whoever decides who may reach an app is who
 * may see who did, and an estate-wide list of names would route around that
 * rule.
 *
 * Apps with no traffic are listed too. "Nobody has opened this in thirty days"
 * is the answer this page most often exists to give, and an app missing from
 * the table reads as an app that does not exist.
 *
 * Host CPU, memory and disk are **not** here. They are the machine's state
 * right now rather than the estate's over a window, and they moved to Processes
 * with the rest of "what is happening on this box".
 */
/** One sentence about this page, said the same way on every branch.
 *  Each of these files hand-wrote a second, shorter one for its error and
 *  loading paths, so the page described itself one way to whoever loaded it
 *  and another to whoever was refused. */
const PAGE_DESCRIPTION =
  "Which apps are used, and what they cost.";

export function AdminUsagePage() {
  // Search and window in the URL, replaced rather than pushed.
  const [search, setSearch] = useUrlParam("q");
  const [range, setRange] = useUrlParam("window", "30");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "requests", dir: "desc" });
  const days = Number(range);

  const { data, isLoading, error, refetch } = useQuery<UsageOverview>({
    queryKey: ["system-usage", days],
    queryFn: () => api.getUsageOverview(days),
    retry: false,
  });
  // Absent on an install that records no consumption — the cost columns then
  // read "—" and the demand half of the page is unaffected.
  const { data: consumption } = useQuery<ConsumptionSummary>({
    queryKey: ["consumption-summary", days],
    queryFn: () => api.getConsumptionSummary(days),
    retry: false,
  });

  if (error) {
    return (
      <div>
        <PageHeader title="Usage" description={PAGE_DESCRIPTION} />
        <QueryError
          title="Usage could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const apps = data?.apps ?? [];
  const cost = new Map((consumption?.apps ?? []).map((c) => [c.app_id, c]));
  const q = search.toLowerCase();
  const dir = sort.dir === "asc" ? 1 : -1;
  const shown = apps
    .filter(
      (a) =>
        !q ||
        a.name.toLowerCase().includes(q) ||
        a.slug.toLowerCase().includes(q) ||
        (a.owner_login ?? "").toLowerCase().includes(q) ||
        (a.owner_group ?? "").toLowerCase().includes(q),
    )
    .sort((a, b) => {
      switch (sort.key) {
        case "app":
          return dir * a.name.localeCompare(b.name);
        case "viewers":
          return dir * (a.viewers - b.viewers);
        case "cpu":
          return dir * ((cost.get(a.app_id)?.cpu_seconds ?? 0) - (cost.get(b.app_id)?.cpu_seconds ?? 0));
        case "peak":
          return (
            dir * ((cost.get(a.app_id)?.peak_memory_mb ?? 0) - (cost.get(b.app_id)?.peak_memory_mb ?? 0))
          );
        default:
          return dir * (a.requests - b.requests);
      }
    });
  const totalRequests = apps.reduce((sum, a) => sum + a.requests, 0);
  const untouched = apps.filter((a) => a.requests === 0).length;
  const hasGaps = shown.some((a) => cost.get(a.app_id)?.incomplete);
  const onSort = (k: SortKey) => setSort(nextSort(sort, k));

  return (
    <div>
      <PageHeader
        title="Usage"
        description={
          isLoading || !data
            ? PAGE_DESCRIPTION
            : (
                // How many *people* use the install is System info's question,
                // with its own four windows — pointed to here, in the header,
                // rather than from a lone line over the table.
                <>
                  {`${totalRequests.toLocaleString()} request${totalRequests === 1 ? "" : "s"} across ${apps.length} app${apps.length === 1 ? "" : "s"} in the last ${days} days${untouched > 0 ? `, ${untouched} of them untouched` : ""}.`}{" "}
                  Active people are on{" "}
                  <Link to="/admin/info#people" className="underline underline-offset-2 hover:text-foreground">
                    System info
                  </Link>
                  .
                </>
              )
        }
      />

      <div className="space-y-4">
        <AudienceSection />
        <div className="flex flex-wrap items-center gap-4">
          <SearchInput
            placeholder="Search by app or owner…"
            value={search}
            onChange={setSearch}
            className="w-full sm:max-w-sm"
          />
          <FilterChips
            label="How far back"
            options={windowFilters}
            value={range}
            onChange={setRange}
          />
        </div>

        {isLoading ? (
          <TableCard className="space-y-2 p-4">
            {Array.from({ length: 5 }).map((_, i) => (
              <Skeleton key={i} className="h-10" />
            ))}
          </TableCard>
        ) : apps.length === 0 ? (
          <EmptyState
            icon={<BarChart3 className="h-6 w-6" />}
            title="No apps yet"
            description="Deploy an app and its figures appear here shortly after."
          />
        ) : shown.length === 0 ? (
          <EmptyState
            title="No apps match"
            action={
              <Button variant="outline" size="sm" onClick={() => setSearch("")}>
                Clear
              </Button>
            }
          />
        ) : (
          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {/* Eight columns, and five of them used to be unconditional
                      — so at any width narrower than a desktop the table ran
                      off the card no matter which three it had dropped. Only
                      the app and its request count are the page's subject;
                      everything else is a reading beside it and yields, in
                      the order somebody scanning a busy install would give
                      them up. */}
                  <SortableHead label="App" sortKey="app" sort={sort} onSort={onSort} className="w-full" />
                  <TableHead className="hidden @3xl:table-cell">Owner</TableHead>
                  <SortableHead
                    label="Requests"
                    sortKey="requests"
                    sort={sort}
                    onSort={onSort}
                    align="right"
                  />
                  <TableHead className="hidden w-[110px] @xl:table-cell">Last {days} days</TableHead>
                  <SortableHead
                    label="Viewers"
                    sortKey="viewers"
                    sort={sort}
                    onSort={onSort}
                    align="right"
                    className="hidden @md:table-cell"
                  />
                  <TableHead className="hidden w-[110px] @4xl:table-cell">Last visit</TableHead>
                  <SortableHead
                    label="CPU time"
                    sortKey="cpu"
                    sort={sort}
                    onSort={onSort}
                    align="right"
                    className="hidden @2xl:table-cell"
                  />
                  <SortableHead
                    label="Peak memory"
                    sortKey="peak"
                    sort={sort}
                    onSort={onSort}
                    align="right"
                    className="hidden @2xl:table-cell"
                  />
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((a) => {
                  const c = cost.get(a.app_id);
                  return (
                    <TableRow key={a.app_id}>
                      {/* Bounded and truncating. `TableCell` is
                          `whitespace-nowrap`, so one app with a long name set
                          this column's floor for every other row — 436px of
                          it on this install, which is most of what the table
                          was overflowing by. */}
                      <TableCell className="max-w-0">
                        <span className="flex items-baseline gap-1">
                          <Link
                            to={`/apps/${a.app_id}/usage`}
                            className="truncate font-medium text-foreground hover:text-primary hover:underline"
                          >
                            {a.name}
                          </Link>
                          {/* The marker points at the footnote under the
                              table; the row's own reason is text, not a
                              `title` no keyboard or touch reaches. */}
                          {c?.incomplete && (
                            <span className="shrink-0 text-muted-foreground">
                              <span aria-hidden>*</span>
                              <span className="sr-only">
                                {c.incomplete_message || "Part of this period wasn’t recorded."}
                              </span>
                            </span>
                          )}
                        </span>
                      </TableCell>
                      <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                        <AppOwner
                          ownerId={a.owner_id}
                          ownerGroupId={a.owner_group_id}
                          ownerLogin={a.owner_login}
                          withCount={false}
                        />
                      </TableCell>
                      <TableCell className="text-right font-medium">
                        {a.requests.toLocaleString()}
                      </TableCell>
                      <TableCell className="hidden @xl:table-cell">
                        {a.requests > 0 ? (
                          <Sparkline
                            data={fillDays(a.series, days)}
                            label={`${a.name}: ${a.requests.toLocaleString()} requests over the last ${days} days`}
                          />
                        ) : (
                          <span className="text-xs text-muted-foreground">No traffic</span>
                        )}
                      </TableCell>
                      <TableCell className="hidden text-right text-muted-foreground @md:table-cell">
                        {/* Distinct signed-in people. Anonymous visitors and API
                            keys are requests, not viewers, so the two columns
                            deliberately do not reconcile. */}
                        {a.viewers.toLocaleString()}
                      </TableCell>
                      <TableCell className="hidden text-sm text-muted-foreground @4xl:table-cell">
                        {a.last_day ? formatDay(a.last_day) : "—"}
                      </TableCell>
                      <TableCell className="hidden text-right text-muted-foreground @2xl:table-cell">
                        {c ? formatCpuSeconds(c.cpu_seconds) : "—"}
                      </TableCell>
                      <TableCell className="hidden text-right text-muted-foreground @2xl:table-cell">
                        {c ? formatMB(c.peak_memory_mb) : "—"}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </TableCard>
        )}

        <p className="text-xs text-muted-foreground">
          Viewers are distinct signed-in people. Anonymous and API key traffic counts as requests only.
          {hasGaps && " * Part of this period wasn’t recorded, so those figures are a minimum."}
        </p>

        {/* Below the window, and outside it. Everything above is bounded by the
            range control at the top of the page; this section deliberately is
            not, because the app it exists to surface went quiet before any
            window a sparkline can draw. It carries its own threshold and shares
            only the search box. */}
        <div className="pt-4">
          <QuietAppsSection search={search} onClear={() => setSearch("")} />
        </div>
      </div>
    </div>
  );
}

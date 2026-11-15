import { useState } from "react";
import { capacityTone, cpuTone, toneText, type Tone } from "@/lib/tone";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Cpu, HardDrive, MemoryStick, Square, Users } from "lucide-react";
import {
  api,
  type Presence,
  type ProcessList,
  type SystemHealth,
  type SystemProcess,
} from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { UserAvatar } from "@/components/user-avatar";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Section } from "@/components/section";
import { SortableHead } from "@/components/sortable-head";
import { nextSort, type SortState } from "@/lib/sort";
import { formatUptime } from "@/lib/time";
import { formatBytes, formatMB } from "@/lib/bytes";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { StatCard } from "@/components/stat-card";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { AdminQueuePage } from "@/pages/admin/queue";
import { StopAppDialog } from "@/components/stop-app-dialog";

const REFRESH_MS = 10_000;

const PAGE_DESCRIPTION = "What this machine is using, and every workload running on it.";

/**
 * One of the host's three resources, as a `StatCard` with a meter.
 *
 * These were the top of Admin → Metrics until that page merged into Usage
 * (ADR §50 D7). They are the machine's state *right now*, which is this page's
 * question — the estate's consumption over a window is the other one, and it
 * went with the per-app table.
 *
 * No reading gets no tone: a coloured bar over a number nobody measured is the
 * reassuring half of the bug this card had. And the detail is never empty: an
 * empty line under two filled ones reads as a card that failed to load.
 */
function ResourceCard({
  label,
  percent,
  detail,
  tone,
  icon,
}: {
  label: string;
  percent?: number;
  detail: string;
  tone: (percent: number | undefined) => Tone | undefined;
  icon: React.ReactNode;
}) {
  return (
    <StatCard
      label={label}
      icon={icon}
      value={percent !== undefined ? `${percent.toFixed(1)}%` : "—"}
      tone={tone(percent)}
      meter={percent ?? 0}
      description={percent !== undefined ? detail : "Not available on this install"}
    />
  );
}

/** CPU, memory and disk on the node that answered. */
function HostResources() {
  const { data: health, error, refetch } = useQuery<SystemHealth>({
    queryKey: ["system-health"],
    queryFn: api.getSystemHealth,
    refetchInterval: REFRESH_MS,
    retry: false,
  });
  const system = health?.system;

  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
      {/* A failed read is not a fact about the install.  With `error` never
          read, all three cards printed "—" under "Not available on this
          install" — a claim about what this machine can measure, where the
          truth was that a request failed.  And because it polls every ten
          seconds, an operator watching a box under load saw the meters assert
          it, correct themselves, and assert it again.

          `!system` is the other half: a dropped poll must not blank readings a
          previous one already produced. */}
      {error && !system && (
        <div className="sm:col-span-3">
          <QueryError
            title="The host’s resources could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      )}
      {!(error && !system) && (
        <>
      {/* CPU is not utilisation and the label alone implied it was. The reading
          is the one-minute load average divided by the core count and capped at
          100 — so a box queueing four times its cores reads exactly like one
          queueing one and a half, and a bare "100.0%" hides which. The detail
          line says what the number is; the cap is the reason it has to. */}
      <ResourceCard
        label="CPU"
        icon={<Cpu className="h-4 w-4" />}
        percent={system?.cpu_percent}
        tone={cpuTone}
        detail={
          system && system.cpu_percent >= 100
            ? "Load average per core, at or above capacity"
            : "Load average per core, over the last minute"
        }
      />
      <ResourceCard
        label="Memory"
        icon={<MemoryStick className="h-4 w-4" />}
        percent={system?.memory_percent}
        tone={capacityTone}
        detail={
          system ? `${formatBytes(system.memory_used)} / ${formatBytes(system.memory_total)}` : ""
        }
      />
      <ResourceCard
        label="Disk"
        icon={<HardDrive className="h-4 w-4" />}
        percent={system?.disk_percent}
        tone={capacityTone}
        detail={
          system ? `${formatBytes(system.disk_used)} / ${formatBytes(system.disk_total)}` : ""
        }
      />
        </>
      )}
    </div>
  );
}

/** The bands live in `lib/tone.ts` — they are a judgement about a reading, and
 *  a second copy of them here would be a second judgement. */
function cpuColor(percent: number | undefined): string {
  if (percent === undefined) return "text-muted-foreground";
  const tone = cpuTone(percent);
  return tone ? toneText[tone] : "text-foreground";
}

/**
 * Whether the backend is running this workload, in the four words both backends
 * answer in (`online | launching | stopped | errored`) plus "missing" for a
 * name no backend holds at all.
 *
 * A row that is not up is on this page for exactly one reason: the app is
 * recorded running and there is no process behind it. Which of the four ways
 * the backend words that is a detail of the backend, and the row reads the same
 * either way — no workload, no readings, no uptime, and nothing to stop.
 */
function isUp(p: SystemProcess): boolean {
  return p.process_status === "online" || p.process_status === "launching";
}

/**
 * Who is using this install right now.
 *
 * Presence is held in memory by the process that answered, so it is scoped to
 * one node and resets when Toft restarts — the same caveat as the host CPU
 * reading above it. The muted copy says so rather than letting an operator read
 * a partial view as the whole install.
 */
function OnlineNow() {
  const { data, isLoading, error, refetch } = useQuery<Presence>({
    queryKey: ["presence"],
    queryFn: () => api.getPresence(),
    refetchInterval: REFRESH_MS,
    retry: false,
  });

  const minutes = Math.round((data?.window_seconds ?? 300) / 60);

  return (
    <Section
      title="Online now"
      description={
        <>
          Used Toft or opened an app in the last {minutes} minutes, as seen by{" "}
          <span className="whitespace-nowrap font-mono">{data?.node || "this node"}</span>.
        </>
      }
      actions={
        <span className="text-sm text-muted-foreground">
          {isLoading || !data ? "—" : `${data.count} ${data.count === 1 ? "person" : "people"}`}
        </span>
      }
    >
      <div className="p-4">
        {error ? (
          <QueryError
            title="Who is online could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        ) : isLoading || !data ? (
          <div className="space-y-2">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-9" />
            ))}
          </div>
        ) : data.users.length === 0 ? (
          <EmptyState
            size="inline"
            title="Nobody online"
            description={`Nobody has used Toft or an app in the last ${minutes} minutes.`}
          />
        ) : (
          <ul className="divide-y divide-border/60">
            {data.users.map((u) => (
              <li key={u.id} className="flex items-center gap-3 py-2">
                <UserAvatar id={u.id} login={u.login} avatarUrl={u.avatar_url} />
                <Link
                  to={`/people/${u.id}`}
                  className="text-sm font-medium text-foreground hover:text-primary hover:underline"
                >
                  {u.name || u.login}
                </Link>
                <span className="flex-1 truncate text-xs text-muted-foreground">
                  {u.app_id ? (
                    <>
                      {u.in_app ? "In " : "Was in "}
                      <Link
                        to={`/apps/${u.app_id}`}
                        className="text-foreground hover:text-primary hover:underline"
                      >
                        {u.app_name}
                      </Link>
                    </>
                  ) : (
                    "Browsing Toft"
                  )}
                </span>
                <RelativeTime iso={u.last_seen} className="text-xs text-muted-foreground" />
              </li>
            ))}
          </ul>
        )}

      </div>
    </Section>
  );
}

type SortKey = "app" | "cpu" | "memory" | "uptime" | "restarts";

/**
 * Order the rows by one column.
 *
 * A reading nobody took sorts last in *both* directions, which is the only
 * part of this that is a decision rather than a comparator. Ascending by CPU
 * means "the quietest app that anybody measured", not "every isolated app
 * first": nil is the absence of an answer and treating it as the smallest
 * number turns it into the claim that the app is idle. That is the same
 * argument the server makes for its own busiest-first default, which is what
 * this starts at.
 */
function compare(a: SystemProcess, b: SystemProcess, sort: SortState<SortKey>): number {
  const dir = sort.dir === "asc" ? 1 : -1;
  if (sort.key === "app") return dir * a.app_name.localeCompare(b.app_name);

  if (sort.key === "cpu" || sort.key === "memory") {
    const x = sort.key === "cpu" ? a.cpu : a.memory;
    const y = sort.key === "cpu" ? b.cpu : b.memory;
    if ((x === undefined) !== (y === undefined)) return x === undefined ? 1 : -1;
    if (x !== undefined && y !== undefined && x !== y) return dir * (x - y);
    return a.app_name.localeCompare(b.app_name);
  }

  const x = sort.key === "uptime" ? a.uptime : a.restarts;
  const y = sort.key === "uptime" ? b.uptime : b.restarts;
  return x === y ? a.app_name.localeCompare(b.app_name) : dir * (x - y);
}

export function AdminProcessesPage() {
  const queryClient = useQueryClient();
  const [toStop, setToStop] = useState<SystemProcess | null>(null);
  // The server's own order, restated as this table's starting state so that
  // the first click on CPU flips it rather than appearing to do nothing.
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "cpu", dir: "desc" });

  const { data, isLoading, error, refetch } = useQuery<ProcessList>({
    queryKey: ["system-processes"],
    queryFn: api.getProcesses,
    refetchInterval: REFRESH_MS,
    retry: false,
  });

  const processes = data?.processes ?? [];
  const rows = [...processes].sort((a, b) => compare(a, b, sort));
  const onSort = (k: SortKey) => setSort(nextSort(sort, k));
  /** Whether any backend on this install takes a reading at all. */
  const measured = processes.some((p) => p.cpu !== undefined || p.memory !== undefined);
  /** Whether the gap is the isolated backend specifically, which is the case
   *  worth naming: the columns are useful for the shared workloads beside it. */
  const unmeasuredIsolated = processes.some(
    (p) => p.mode === "isolated" && p.cpu === undefined && isUp(p),
  );
  /** Mode and host are one repeated word each on a single-mode install, and a
   *  column that says the same thing on every row spends width to say nothing —
   *  200px of it, for a hostname the page title has already printed. Shown only
   *  where the rows disagree, which is the same "absent, not a column of
   *  dashes" rule the readings above already follow.
   *
   *  One flag governs both, because host is derived from mode rather than
   *  independent of it: a shared workload runs on the node that answered, and
   *  an isolated one runs somewhere Toft does not name. The fact the two
   *  columns carried is not lost with them — it is said once under the table,
   *  because "every workload here is isolated" is a fact about the install and
   *  not about each of its workloads in turn. */
  const mixedModes = processes.some((p) => p.mode !== processes[0]?.mode);
  const soleMode = mixedModes ? undefined : processes[0]?.mode;

  /** Apps the list left out because they have no workload. Both are ordinary
   *  states rather than problems, so they are a sentence and not rows. */
  const idle = (data?.sleeping ?? 0) + (data?.stopped ?? 0);

  if (error) {
    return (
      <div>
        <PageHeader title="Processes" description={PAGE_DESCRIPTION} />
        <QueryError
          title="Processes could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Processes"
        description={
          isLoading || !data
            ? PAGE_DESCRIPTION
            : `What ${data.node} is using, and every workload running on it.`
        }
      />

      <div className="space-y-6">
        <HostResources />

        <OnlineNow />

        {data?.degraded && (
          <Notice size="md">{data.degraded}</Notice>
        )}

        <Section title="Running workloads">

          {isLoading ? (
            <div className="space-y-2 p-4">
              {Array.from({ length: 4 }).map((_, i) => (
                <Skeleton key={i} className="h-10" />
              ))}
            </div>
          ) : processes.length === 0 ? (
            <EmptyState
              size="inline"
              title="Nothing is running on this node"
              description={
                idle > 0
                  ? `${idle} ${idle === 1 ? "app is" : "apps are"} sleeping or stopped.`
                  : "Deploy or start an app and it appears here."
              }
            />
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    {/* Eight columns and no plan, on a page people open during
                        an incident and an incident does not wait for a desk.
                        The app and its two readings stay — what is running and
                        what it is costing is the question. Owner, mode, host,
                        uptime and restarts yield in that order, and each of
                        them reappears under the app's name at the width that
                        drops it.

                        Every breakpoint here is a step later than the space it
                        describes: these are the viewport's, and the table sits
                        inside a 256px sidebar, so each column used to arrive
                        while the room for it was still next door. Mode and
                        host go one better and are absent rather than
                        responsive — a column carrying the same word on every
                        row buys nothing at any width, and on a shared-only
                        install the host column was 200px repeating a hostname
                        the page title has already printed. */}
                    <SortableHead label="App" sortKey="app" sort={sort} onSort={onSort} />
                    <TableHead className="hidden @3xl:table-cell">Owner</TableHead>
                    {mixedModes && <TableHead className="hidden @4xl:table-cell">Mode</TableHead>}
                    {mixedModes && <TableHead className="hidden @4xl:table-cell">Host</TableHead>}
                    {/* Absent, not a column of dashes. A backend that takes no
                        reading gets no column, and the note under the table says
                        so once instead of every row saying it again. */}
                    {measured && (
                      <SortableHead label="CPU" sortKey="cpu" sort={sort} onSort={onSort} align="right" />
                    )}
                    {measured && (
                      <SortableHead label="Memory" sortKey="memory" sort={sort} onSort={onSort} align="right" />
                    )}
                    <SortableHead
                      label="Uptime"
                      sortKey="uptime"
                      sort={sort}
                      onSort={onSort}
                      align="right"
                      className="hidden @3xl:table-cell"
                    />
                    {/* Sortable because a crash loop is a thing you go looking
                        for and there was no way to find one but to read the
                        column. */}
                    <SortableHead
                      label="Restarts"
                      sortKey="restarts"
                      sort={sort}
                      onSort={onSort}
                      align="right"
                      className="hidden @xl:table-cell"
                    />
                    <TableHead className="w-[70px]" />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((p) => (
                    <TableRow key={p.release}>
                      {/* The release id is up to sixty-odd characters of mono,
                          and every cell in this table is `whitespace-nowrap`, so
                          it was setting this column's minimum width at nearly
                          500px and pushing the two rightmost columns off a
                          1600px screen. It is a reference rather than something
                          anybody reads across, so it wraps within a bounded
                          cell instead of widening one — and it wraps rather than
                          truncating because the hash on the end is the half that
                          distinguishes two deploys of one app. */}
                      <TableCell className="max-w-[20rem]">
                        <div className="flex items-center gap-2">
                          <StatusDot status={p.process_status === "online" ? "running" : p.process_status} />
                          <Link
                            to={`/apps/${p.app_id}`}
                            className="truncate font-medium text-foreground hover:text-primary hover:underline"
                          >
                            {p.app_name}
                          </Link>
                          {!isUp(p) && (
                            <span className={`shrink-0 text-xs ${toneText.attention}`}>no workload</span>
                          )}
                        </div>
                        <span className="block break-all whitespace-normal font-mono text-xs text-muted-foreground">
                          {p.release}
                        </span>
                        {/* What the hidden columns held, at the widths that hide
                            them — a table may not be the only route to a fact
                            and then drop the column carrying it. A column left
                            out for saying the same thing on every row is not
                            hidden, so it is not restated here either.

                            Each restatement carries **its own column's** tier,
                            and a container one: the columns moved to the card's
                            width and these did not, so mode and host were
                            printed twice for every width above the card's
                            `@4xl`, and owner and uptime for a band of the
                            viewport either side of 1024px. */}
                        <span className="mt-0.5 flex flex-wrap gap-x-2 text-2xs text-muted-foreground @4xl:hidden">
                          {(p.owner_group || p.owner_name || p.owner_login) && (
                            <span className="@3xl:hidden">
                              {p.owner_group || p.owner_name || p.owner_login}
                            </span>
                          )}
                          {mixedModes && <span className="capitalize @4xl:hidden">{p.mode}</span>}
                          {mixedModes && <span className="@4xl:hidden">{p.host || "cluster"}</span>}
                          {p.uptime > 0 && (
                            <span className="@3xl:hidden">up {formatUptime(p.uptime)}</span>
                          )}
                        </span>
                      </TableCell>
                      <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                        {/* A group gets its own icon rather than sitting in a
                            column of logins looking like somebody's account. */}
                        {p.owner_group ? (
                          <Link
                            to="/people?tab=groups"
                            className="inline-flex min-w-0 items-center gap-1.5 hover:text-primary hover:underline"
                          >
                            <Users className="h-3.5 w-3.5 shrink-0" aria-hidden />
                            <span className="truncate">{p.owner_group}</span>
                          </Link>
                        ) : p.owner_id ? (
                          <Link
                            to={`/people/${p.owner_id}`}
                            className="hover:text-primary hover:underline"
                          >
                            {p.owner_name || p.owner_login}
                          </Link>
                        ) : (
                          "—"
                        )}
                      </TableCell>
                      {mixedModes && (
                        <TableCell className="hidden text-sm text-muted-foreground capitalize @4xl:table-cell">
                          {p.mode}
                        </TableCell>
                      )}
                      {mixedModes && (
                        <TableCell className="hidden font-mono text-xs text-muted-foreground @4xl:table-cell">
                          {/* An isolated workload runs on a node Toft does not name. */}
                          {p.host || "cluster"}
                        </TableCell>
                      )}
                      {/* Absent covers both a workload the backend has never
                          heard of and one whose backend takes no reading. */}
                      {measured && (
                        <TableCell className={`text-right text-sm font-medium tabular-nums ${cpuColor(p.cpu)}`}>
                          {p.cpu === undefined ? "—" : `${p.cpu.toFixed(1)}%`}
                        </TableCell>
                      )}
                      {measured && (
                        <TableCell className="text-right text-sm tabular-nums text-muted-foreground">
                          {p.memory === undefined ? "—" : formatMB(p.memory)}
                        </TableCell>
                      )}
                      {/* Nothing that is not up has been up for a while, and
                          the server sends zero rather than the start time its
                          backend still remembers. */}
                      <TableCell className="hidden text-right text-sm text-muted-foreground @3xl:table-cell">
                        {p.uptime > 0 ? formatUptime(p.uptime) : "—"}
                      </TableCell>
                      <TableCell className="hidden text-right text-sm text-muted-foreground @xl:table-cell">
                        {isUp(p) ? p.restarts : "—"}
                      </TableCell>
                      <TableCell>
                        {/* Absent rather than disabled when there is nothing
                            to stop: a disabled control is not focusable and
                            swallows the hover, so the `title` that used to
                            explain it was unreachable by every route — and the
                            row already says "no workload" beside the name. */}
                        {isUp(p) && (
                          <Button
                            variant="ghost"
                            size="icon-xs"
                            aria-label={`Stop ${p.app_name}`}
                            onClick={() => setToStop(p)}
                          >
                            <Square className="h-3.5 w-3.5" />
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {/* One sentence rather than a column of dashes. Which backend
                  takes no reading is a fact about the install, not about each
                  of its workloads in turn. */}
              {(soleMode || !measured || unmeasuredIsolated || idle > 0 || rows.length > 0) && (
                <div className="space-y-1 border-t border-border/60 px-4 py-3 text-xs text-muted-foreground">
                  {/* **What the Restarts column counts, said once.** Every
                      deploy, platform restart and supervisor recreate starts a
                      fresh workload with a fresh counter — a pod is replaced,
                      and the agent's workload is described again — so a low number here is not
                      evidence that an app has been stable. It is the column
                      somebody sorts by when hunting a crash loop, which is
                      exactly when the scope has to be on the page rather than
                      in somebody's head. */}
                  {rows.length > 0 && (
                    <p>
                      Restarts count from the last deploy or platform restart.
                    </p>
                  )}
                  {/* What the mode and host columns would have said on every
                      row, said once. */}
                  {soleMode && (
                    <p>
                      {soleMode === "shared"
                        ? "All workloads run shared, on the node above."
                        : "All workloads run isolated."}
                    </p>
                  )}
                  {(!measured || unmeasuredIsolated) && (
                    <p>
                      {measured
                        ? "Isolated workloads report no CPU or memory here."
                        : "CPU and memory aren’t available on this install."}
                    </p>
                  )}
                  {/* The account of what is *not* here. Without it, removing
                      the workload-less rows reads as apps having left the
                      estate — which is what they look like on a page whose
                      subject is the machine and whose door to the estate is
                      somewhere else.

                      `scope=all` on both links, because these counts are the
                      whole estate and the apps list defaults to `mine`: an
                      administrator who clicked "12 sleeping" landed on their
                      own two, with a heading that had promised twelve. */}
                  {idle > 0 && (
                    <p>
                      {(data?.sleeping ?? 0) > 0 && (
                        <Link
                          to="/?scope=all&status=sleeping"
                          className="underline underline-offset-2 hover:text-foreground"
                        >
                          {data?.sleeping} sleeping
                        </Link>
                      )}
                      {(data?.sleeping ?? 0) > 0 && (data?.stopped ?? 0) > 0 && " and "}
                      {(data?.stopped ?? 0) > 0 && (
                        <Link
                          to="/?scope=all&status=stopped"
                          className="underline underline-offset-2 hover:text-foreground"
                        >
                          {data?.stopped} stopped
                        </Link>
                      )}
                      {idle === 1 ? " app is not listed here." : " apps are not listed here."}
                    </p>
                  )}
                </div>
              )}
            </>
          )}
        </Section>
      </div>

      {/* The build queue, which had a page of its own for one short table.
          "What is running on this machine now" is one question, and builds
          waiting and in progress are half of its answer (ADR §50). */}
      <section id="queue" className="mt-6 scroll-mt-20">
        <AdminQueuePage />
      </section>

      <StopAppDialog
        appId={toStop?.app_id ?? ""}
        appName={toStop?.app_name ?? ""}
        open={toStop !== null}
        onOpenChange={() => setToStop(null)}
        onStopped={() => {
          // The app list and its detail page both show a status this changed.
          queryClient.invalidateQueries({ queryKey: ["system-processes"] });
          queryClient.invalidateQueries({ queryKey: ["system-health"] });
          queryClient.invalidateQueries({ queryKey: ["apps"] });
        }}
      />
    </div>
  );
}

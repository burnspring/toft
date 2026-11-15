import { Fragment, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  CalendarClock, CircleSlash, Download, ExternalLink, FilePlus2, MoreHorizontal, Pause, Play,
  RotateCcw, Terminal,
} from "lucide-react";

import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { StatusBadge } from "@/components/status-badge";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { Disclosure } from "@/components/disclosure";
import { SectionHeading, Fact } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { RelativeTime } from "@/components/relative-time";
import { Notice } from "@/components/notice";
import { Code } from "@/components/code";
import { JobRunProvenance, JobRunResult, JobRunStatusBadge } from "@/components/job-run-status";
import { LogPane, type LogPaneLine } from "@/components/log-pane";
import { RunNowDialog } from "@/components/run-now-dialog";
import { NewJobDialog } from "@/components/new-job-dialog";
import { useRunLog, type RunLogLine, RUN_LOG_CAP } from "@/hooks/use-run-log";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { FieldError, FieldHelp } from "@/components/field-help";
import { enforcementPhrase } from "@/lib/enforcement";
import { fieldErrorId } from "@/lib/utils";
import { rowActivation } from "@/lib/row-activation";
import { jobRunInFlight, ordinal } from "@/lib/job-runs";
import {
  formatJobCPU, formatJobDuration, formatJobMemory, jobWhatRuns, limitSourceLabel, paramsSummary,
  placementSentence, refusalText,
} from "@/lib/automation";
import { describeCron } from "@/lib/cron-describe";
import { api, jobRunOutputUrl } from "@/lib/api";
import type { JobDefinitionList, JobListEntry, JobRun } from "@/lib/api";
import { formatDateTime, formatDuration, formatTimestamp } from "@/lib/time";
import { formatExpectWithin, splitSchedule } from "@/lib/schedule";
import { appUrl } from "@/lib/app-url";
import { toneText } from "@/lib/tone";

/**
 * Automation: the jobs this app declares in toft.toml, and their runs.
 *
 * The UI operates jobs; toft.toml defines them. Nothing here creates, edits or
 * deletes one — the one change a person makes is pausing a schedule until the
 * next deploy, and the acts are starting a run, running a finished one again
 * and cancelling an open one. *New job* writes the toft.toml block to paste,
 * and saves nothing.
 *
 * The invariant underneath the whole tab: **params are never interpolated into
 * a command**. They reach the run as `TOFT_PARAMS` and `TOFT_PARAM_<NAME>`.
 */
export function AutomationTab({
  appId, appSlug, canEdit, filesOnly = false,
}: {
  appId: string;
  /** For naming a downloaded log, the CLI line and the run's status page. */
  appSlug: string;
  canEdit: boolean;
  /** A static site runs no code at all, so it has no jobs and nothing is offered. */
  filesOnly?: boolean;
}) {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string | null>(null);
  const [running, setRunning] = useState<JobListEntry | null>(null);
  const [creating, setCreating] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);

  const { data, isLoading, error, refetch } = useQuery<JobDefinitionList>({
    queryKey: ["jobs", appId],
    queryFn: () => api.listJobDefinitions(appId),
    refetchInterval: 30000,
  });
  const { data: runData } = useQuery<{ runs: JobRun[] }>({
    queryKey: ["job-runs", appId],
    queryFn: () => api.listJobRuns(appId, { limit: 100 }),
    refetchInterval: 10000,
  });

  const jobs = [...(data?.jobs ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  const runs = runData?.runs ?? [];
  const available = data?.jobs_available ?? false;

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["jobs", appId] });
    void queryClient.invalidateQueries({ queryKey: ["job-runs", appId] });
    // The app's own page counts scheduled and paused jobs on its Automation tile.
    void queryClient.invalidateQueries({ queryKey: ["app", appId] });
  };

  async function handlePause(job: JobListEntry, enabled: boolean) {
    setBusy(job.id);
    try {
      await api.pauseJob(appId, job.id, enabled);
      toast.success(
        enabled
          ? `“${job.name}” runs on its schedule again`
          : `“${job.name}” paused until the next deploy`,
      );
      invalidate();
    } catch (err) {
      toast.error(refusalText(err, `Couldn’t ${enabled ? "resume" : "pause"} “${job.name}”`));
    } finally {
      setBusy(null);
    }
  }

  if (isLoading) {
    return (
      <div className="space-y-3 pt-4">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="pt-4">
        <QueryError
          title="This app’s jobs could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see its jobs."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const selectedJob = jobs.find((j) => j.id === selected) ?? null;
  const placement = placementSentence(data?.placement);
  const newJob = !filesOnly && (
    <Button size="sm" variant="outline" onClick={() => setCreating(true)}>
      <FilePlus2 className="mr-1.5 h-4 w-4" />
      New job…
    </Button>
  );

  return (
    <div className="space-y-6 pt-4">
      <div id="jobs" className="scroll-mt-20">
        <SectionHeading
          title="Jobs"
          description={
            <>
              Declared in <Code>toft.toml</Code>.
              {/* Where runs go and how many at once describe runs, so they
                  wait for a job to have some; over the empty state they were
                  two facts about nothing. */}
              {jobs.length > 0 && ` ${placement}`}
              {jobs.length > 0 && data && available
                ? ` Up to ${data.concurrency_limit} at once, ${data.max_queued} waiting.`
                : ""}
            </>
          }
          // Once: on an empty tab the empty state carries it, and the same
          // button twice on one screen is two ways in to one act.
          actions={jobs.length > 0 ? newJob : undefined}
        />
      </div>

      {/* An install-level refusal, in the words the platform gives it. */}
      {!available && !filesOnly && (
        <Notice title="Jobs can’t run on this install">
          {data?.unavailable_bec || "An administrator can turn background jobs on."}
        </Notice>
      )}

      {jobs.length === 0 ? (
        <EmptyState
          icon={<CalendarClock className="h-6 w-6" />}
          title="This app declares no jobs"
          description={
            filesOnly
              ? "A static site is served as files and runs no code, so it has no jobs."
              : "Declare one in toft.toml and deploy. New job writes the block for you."
          }
          action={newJob || undefined}
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">Job</TableHead>
                <TableHead className="hidden @3xl:table-cell">Schedule</TableHead>
                <TableHead className="hidden @xl:table-cell">Next run</TableHead>
                <TableHead className="hidden @md:table-cell">Last run</TableHead>
                <TableHead className="hidden @4xl:table-cell">Limits</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {jobs.map((job) => {
                const last = runs.find((r) => r.job === job.name && !jobRunInFlight(r.status));
                const queued = runs.filter((r) => r.job === job.name && r.status === "queued").length;
                const live = runs.filter((r) => r.job === job.name && r.status === "running").length;
                const clamped = job.limits.timeout.source === "clamped";
                return (
                  <TableRow
                    key={job.id}
                    {...rowActivation(`Show runs of ${job.name}`, () =>
                      setSelected(selected === job.id ? null : job.id),
                    )}
                    data-state={selected === job.id ? "selected" : undefined}
                  >
                    <TableCell className="max-w-0 whitespace-normal">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="font-medium">{job.name}</span>
                        {job.paused && <StatusBadge status="paused" label="Paused until next deploy" />}
                        {clamped && <StatusBadge status="clamped" tone="attention" label="Clamped" />}
                        {live > 0 && <StatusBadge status="running" tone="ok" label="Running" />}
                        {queued > 0 && (
                          <StatusBadge status="queued" tone="attention" label={`${queued} queued`} />
                        )}
                      </div>
                      <div className="truncate font-mono text-xs text-muted-foreground">
                        {jobWhatRuns(job)}
                      </div>
                      {/* Each dropped column restated at its own tier, on the card's width. */}
                      <div className="mt-0.5 flex flex-wrap gap-x-3 text-2xs text-muted-foreground @4xl:hidden">
                        <span className="@3xl:hidden">{scheduleSentence(job.schedule)}</span>
                        <span className="@xl:hidden"><NextRun job={job} /></span>
                        <span className="@md:hidden">
                          {last ? `Last: ${last.status.replace(/_/g, " ")}` : "Never run"}
                        </span>
                        <span>{limitsLine(job)}</span>
                      </div>
                    </TableCell>
                    <TableCell className="hidden text-xs @3xl:table-cell">
                      <ScheduleCell schedule={job.schedule} />
                    </TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">
                      <NextRun job={job} />
                    </TableCell>
                    <TableCell className="hidden max-w-[16rem] text-xs @md:table-cell">
                      {last ? (
                        <div className="space-y-0.5">
                          <div className="flex items-center gap-2">
                            <JobRunStatusBadge status={last.status} />
                            <span className="text-muted-foreground">
                              <RelativeTime iso={last.finished_at ?? last.queued_at} />
                            </span>
                          </div>
                          {last.failure_reason && (
                            <div className={`truncate ${toneText.danger}`}>{last.failure_reason}</div>
                          )}
                        </div>
                      ) : (
                        <span className="text-muted-foreground">Never</span>
                      )}
                    </TableCell>
                    <TableCell className="hidden text-xs @4xl:table-cell">
                      <div>{limitsLine(job)}</div>
                      <div className="text-muted-foreground">
                        {clamped ? "Lowered to the install’s ceiling" : sourceLine(job)}
                      </div>
                    </TableCell>
                    <TableCell className="text-right" onClick={(e) => e.stopPropagation()}>
                      <div className="flex justify-end gap-1">
                        {canEdit && (
                          <Button
                            variant="outline" size="sm"
                            disabled={!available}
                            aria-label={`Run now: ${job.name}…`}
                            onClick={() => setRunning(job)}
                          >
                            <Play className="mr-1 h-3.5 w-3.5" />
                            Run now…
                          </Button>
                        )}
                        <DropdownMenu>
                          <DropdownMenuTrigger
                            render={
                              <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${job.name}`}>
                                <MoreHorizontal className="h-4 w-4" />
                              </Button>
                            }
                          />
                          <DropdownMenuContent align="end">
                            <DropdownMenuItem onClick={() => setSelected(job.id)}>
                              <Terminal className="mr-2 h-3.5 w-3.5" />
                              Show runs
                            </DropdownMenuItem>
                            {canEdit && job.schedule && (
                              <DropdownMenuItem
                                disabled={busy === job.id}
                                onClick={() => void handlePause(job, job.paused)}
                              >
                                {job.paused ? (
                                  <Play className="mr-2 h-3.5 w-3.5" />
                                ) : (
                                  <Pause className="mr-2 h-3.5 w-3.5" />
                                )}
                                {job.paused ? "Resume schedule" : "Pause until next deploy"}
                              </DropdownMenuItem>
                            )}
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableCard>
      )}

      {selectedJob ? (
        <div id="runs" className="scroll-mt-20 space-y-3">
          <SectionHeading level={3} title={`${selectedJob.name}`} />
          <JobFacts job={selectedJob} appId={appId} />
          <JobRuns appId={appId} appSlug={appSlug} job={selectedJob.name} canEdit={canEdit} />
        </div>
      ) : (
        runs.length > 0 && (
          // Collapsed rather than gone: the only place a run of a job that has
          // since left toft.toml can still be read.
          <Disclosure label="All runs on this app">
            <JobRuns appId={appId} appSlug={appSlug} job={null} canEdit={canEdit} />
          </Disclosure>
        )
      )}

      {running && (
        <RunNowDialog
          key={running.id} appId={appId} appSlug={appSlug} job={running} open
          onOpenChange={(open) => { if (!open) setRunning(null); }}
          onStarted={() => { setSelected(running.id); invalidate(); }}
        />
      )}
      {creating && (
        <NewJobDialog
          appSlug={appSlug} open
          onOpenChange={(open) => { if (!open) setCreating(false); }}
        />
      )}
    </div>
  );
}

/** The schedule as a sentence, in the job's own zone. */
function scheduleSentence(schedule: string | undefined): string {
  const { zone, expr } = splitSchedule(schedule);
  if (!expr) return "On demand";
  const words = describeCron(expr) ?? expr;
  return `${words} · ${zone || "UTC"}`;
}

function ScheduleCell({ schedule }: { schedule: string | undefined }) {
  const { zone, expr } = splitSchedule(schedule);
  if (!expr) return <span className="text-muted-foreground">On demand</span>;
  const words = describeCron(expr);
  return (
    <div className="space-y-0.5">
      <div>{words ?? <span className="font-mono">{expr}</span>}</div>
      <div className="text-muted-foreground">
        {words && <span className="font-mono">{expr}</span>}
        {words ? " · " : ""}
        {zone || "UTC"}
      </div>
    </div>
  );
}

function NextRun({ job }: { job: JobListEntry }) {
  if (!job.schedule) return <>Not scheduled</>;
  // A paused schedule has a stored next_run_at that will never arrive.
  if (job.paused) return <>Paused</>;
  if (!job.next_run_at) return <>—</>;
  return <RelativeTime iso={job.next_run_at} />;
}

function limitsLine(job: JobListEntry): string {
  return `${formatJobDuration(job.limits.timeout.value)} · ${formatJobMemory(job.limits.memory.value)} · ${formatJobCPU(job.limits.cpu.value)}`;
}

/** Where the three limits came from, said once when they agree. */
function sourceLine(job: JobListEntry): string {
  const t = limitSourceLabel(job.limits.timeout, formatJobDuration);
  const m = limitSourceLabel(job.limits.memory, formatJobMemory);
  const c = limitSourceLabel(job.limits.cpu, formatJobCPU);
  return t === m && m === c ? t.charAt(0).toUpperCase() + t.slice(1) : `Time ${t}; memory ${m}; CPU ${c}`;
}

const OVERLAP_WORDS: Record<string, string> = {
  "": "Waits for the last run",
  queue: "Waits for the last run",
  skip: "Skips while the last run is going",
};
const NOTIFY_WORDS: Record<string, string> = {
  "": "Nobody",
  none: "Nobody",
  failures: "Owners, when a run fails",
  always: "Owners, after every run",
};

/** Seconds held at the CPU limit, as a run's fact. */
function formatThrottled(sec: number): string {
  if (sec < 1) return "Never";
  return formatJobDuration(Math.round(sec));
}

/**
 * A job's own memory and CPU, set by an administrator (ADR §128): toft.toml
 * declares neither, and an override survives every redeploy. Absent for
 * anybody else — the figures above say what applies. Each number saves alone,
 * its Save shown once edited; blank hands the job back to the install's
 * default.
 */
function JobLimitsEditor({ job, appId }: { job: JobListEntry; appId: string }) {
  const queryClient = useQueryClient();
  const { data: user } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const [memory, setMemory] = useState(job.override_memory_mb ? String(job.override_memory_mb) : "");
  const [cpu, setCPU] = useState(job.override_cpu_millicores ? String(job.override_cpu_millicores / 1000) : "");
  const [saving, setSaving] = useState(false);
  if (user?.system_role !== "system_admin") return null;

  const mb = memory.trim() === "" ? 0 : Math.round(Number(memory));
  const m = cpu.trim() === "" ? 0 : Math.round(Number(cpu) * 1000);
  const memBroken = memory.trim() !== "" && (!Number.isFinite(Number(memory)) || mb < 64 || mb > 1048576);
  const cpuBroken = cpu.trim() !== "" && (!Number.isFinite(Number(cpu)) || m < 50 || m > 64000);
  const dirty = mb !== (job.override_memory_mb ?? 0) || m !== (job.override_cpu_millicores ?? 0);

  async function save() {
    setSaving(true);
    try {
      await api.setJobLimits(appId, job.id, {
        memory_limit_mb: mb === 0 ? null : mb,
        cpu_limit_millicores: m === 0 ? null : m,
      });
      toast.success(`Saved. “${job.name}” runs with these limits from its next run`);
      void queryClient.invalidateQueries({ queryKey: ["jobs", appId] });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : `Couldn’t save the limits of “${job.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="mt-4 border-t border-border/60 pt-4">
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <Label htmlFor={`job-mem-${job.id}`}>Memory for this job (MB)</Label>
          <Input
            id={`job-mem-${job.id}`}
            inputMode="numeric"
            className="w-32"
            placeholder="Default"
            value={memory}
            disabled={saving}
            aria-invalid={memBroken}
            aria-describedby={memBroken ? fieldErrorId(`job-mem-${job.id}`) : undefined}
            onChange={(e) => setMemory(e.target.value)}
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor={`job-cpu-${job.id}`}>CPU for this job (cores)</Label>
          <Input
            id={`job-cpu-${job.id}`}
            inputMode="decimal"
            className="w-32"
            placeholder="Default"
            value={cpu}
            disabled={saving}
            aria-invalid={cpuBroken}
            aria-describedby={cpuBroken ? fieldErrorId(`job-cpu-${job.id}`) : undefined}
            onChange={(e) => setCPU(e.target.value)}
          />
        </div>
        {dirty && (
          <Button size="sm" variant="outline" disabled={saving || memBroken || cpuBroken} onClick={save}>
            {saving ? "Saving…" : "Save"}
          </Button>
        )}
      </div>
      {memBroken && (
        <FieldError field={`job-mem-${job.id}`}>Memory must be between 64 MB and 1,048,576 MB.</FieldError>
      )}
      {cpuBroken && <FieldError field={`job-cpu-${job.id}`}>CPU must be between 0.05 and 64 cores.</FieldError>}
      <FieldHelp>Blank uses the install’s job default. Only an administrator sees this, and a deploy keeps it.</FieldHelp>
    </div>
  );
}

/** The selected job in full: what it runs, when, with what, and who hears. */
function JobFacts({ job, appId }: { job: JobListEntry; appId: string }) {
  const params = Object.entries(job.params_spec ?? {});
  return (
    <TableCard flat className="p-4">
      <dl className="grid gap-4 sm:grid-cols-2">
        <Fact label="Runs">
          <span className="font-mono text-xs break-all">{jobWhatRuns(job)}</span>
        </Fact>
        <Fact label="Schedule">
          {scheduleSentence(job.schedule)}
          {job.schedule && (
            <span className="block text-xs text-muted-foreground">
              {OVERLAP_WORDS[job.overlap] ?? job.overlap}
              {job.paused ? ". Paused until the next deploy" : ""}
            </span>
          )}
        </Fact>
        <Fact label="Timeout">
          {formatJobDuration(job.limits.timeout.value)}
          <span className="block text-xs text-muted-foreground">
            {limitSourceLabel(job.limits.timeout, formatJobDuration)}
          </span>
        </Fact>
        <Fact label="Memory">
          {formatJobMemory(job.limits.memory.value)}
          <span className="block text-xs text-muted-foreground">
            {limitSourceLabel(job.limits.memory, formatJobMemory)}
            {enforcementPhrase(job.limits.memory.enforcement) && `. ${enforcementPhrase(job.limits.memory.enforcement)}`}
          </span>
        </Fact>
        <Fact label="CPU">
          {formatJobCPU(job.limits.cpu.value)}
          <span className="block text-xs text-muted-foreground">
            {limitSourceLabel(job.limits.cpu, formatJobCPU)}
            {job.limits.cpu.value > 0 && enforcementPhrase(job.limits.cpu.enforcement) &&
              `. ${enforcementPhrase(job.limits.cpu.enforcement)}`}
          </span>
        </Fact>
        <Fact label="Email">{NOTIFY_WORDS[job.notify] ?? job.notify}</Fact>
        {job.expect_within_sec ? (
          <Fact label="Overdue after">{formatExpectWithin(job.expect_within_sec)} without a success</Fact>
        ) : null}
        <Fact label="Params">
          {params.length === 0 ? (
            "None"
          ) : (
            <span className="flex flex-wrap gap-x-3 gap-y-1 font-mono text-xs">
              {params.map(([name, p]) => (
                <span key={name}>
                  {name}
                  <span className="text-muted-foreground">
                    : {p.type}
                    {p.required ? ", required" : p.default !== undefined ? ` = ${String(p.default)}` : ""}
                  </span>
                </span>
              ))}
            </span>
          )}
        </Fact>
      </dl>
      <JobLimitsEditor key={job.id} job={job} appId={appId} />
    </TableCard>
  );
}

// --- Runs ---

/**
 * Runs — one job's when a row is selected, the app's when none is.
 *
 * A run's history outlives the job that made it, so the unfiltered view is the
 * only place a run of a job since removed from toft.toml can be read.
 */
function JobRuns({
  appId, appSlug, job, canEdit,
}: {
  appId: string;
  appSlug: string;
  /** The job to show, or null for every run on the app. */
  job: string | null;
  canEdit: boolean;
}) {
  const queryClient = useQueryClient();
  const [openRun, setOpenRun] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const { data, isLoading, error, refetch } = useQuery<{ runs: JobRun[] }>({
    queryKey: ["job-runs", appId, job ?? "*"],
    queryFn: () => api.listJobRuns(appId, { job: job ?? undefined, limit: 50 }),
    refetchInterval: 10000,
  });
  const runs = data?.runs ?? [];
  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["job-runs", appId] });

  async function handleCancel(run: JobRun) {
    setBusy(run.id);
    try {
      await api.cancelJobRun(appId, run.id);
      toast.success(`Stopped this run of “${run.job}”`);
      void invalidate();
    } catch (err) {
      toast.error(refusalText(err, `Couldn’t stop this run of “${run.job}”`));
    } finally {
      setBusy(null);
    }
  }

  async function handleRerun(run: JobRun) {
    setBusy(run.id);
    try {
      await api.rerunJobRun(appId, run.id);
      toast.success(`“${run.job}” queued again with the same params`);
      void invalidate();
    } catch (err) {
      toast.error(refusalText(err, `Couldn’t run “${run.job}” again`));
    } finally {
      setBusy(null);
    }
  }

  if (error) {
    return (
      <QueryError
        title="This app’s runs could not be read"
        error={error}
        fallback="Only the app’s owner and editors can see its runs."
        onRetry={() => void refetch()}
      />
    );
  }
  if (isLoading) return <RunsSkeleton />;
  if (runs.length === 0) return <EmptyState size="inline" title="No runs yet" />;

  const cols = job === null ? 6 : 5;

  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            {job === null && <TableHead>Job</TableHead>}
            <TableHead>Status</TableHead>
            <TableHead className="hidden @3xl:table-cell">Started by</TableHead>
            <TableHead className="hidden @md:table-cell">Queued</TableHead>
            <TableHead className="hidden w-full @xl:table-cell">Result</TableHead>
            <TableHead className="text-right">Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {runs.map((run) => {
            const open = jobRunInFlight(run.status);
            return (
              <Fragment key={run.id}>
                <TableRow>
                  {job === null && <TableCell className="font-medium">{run.job}</TableCell>}
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-2">
                      <JobRunStatusBadge status={run.status} />
                      <RunAside run={run} />
                    </div>
                  </TableCell>
                  <TableCell className="hidden @3xl:table-cell">
                    <JobRunProvenance run={run} />
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @md:table-cell">
                    <RelativeTime iso={run.queued_at} />
                  </TableCell>
                  <TableCell className="hidden max-w-0 truncate text-xs text-muted-foreground @xl:table-cell">
                    <JobRunResult run={run} />
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost" size="icon-sm"
                        aria-expanded={openRun === run.id}
                        aria-controls={`job-run-${run.id}`}
                        aria-label={`${openRun === run.id ? "Hide" : "Show"} the log of this run of ${run.job}`}
                        onClick={() => setOpenRun(openRun === run.id ? null : run.id)}
                      >
                        <Terminal className="h-4 w-4" />
                      </Button>
                      {(canEdit || run.has_output) && (
                        <DropdownMenu>
                          <DropdownMenuTrigger
                            render={
                              <Button
                                variant="ghost" size="icon-sm"
                                aria-label={`Actions for this run of ${run.job}`}
                              >
                                <MoreHorizontal className="h-4 w-4" />
                              </Button>
                            }
                          />
                          <DropdownMenuContent align="end">
                            {run.has_output && (
                              <DropdownMenuItem
                                render={<a href={jobRunOutputUrl(appId, run.id)} download aria-label="Download output" />}
                              >
                                <Download className="mr-2 h-3.5 w-3.5" />
                                Download output
                              </DropdownMenuItem>
                            )}
                            {canEdit && !open && (
                              <DropdownMenuItem disabled={busy === run.id} onClick={() => void handleRerun(run)}>
                                <RotateCcw className="mr-2 h-3.5 w-3.5" />
                                Run again
                              </DropdownMenuItem>
                            )}
                            {canEdit && open && (
                              <>
                                {run.has_output && <DropdownMenuSeparator />}
                                <DropdownMenuItem
                                  variant="destructive"
                                  disabled={busy === run.id}
                                  onClick={() => void handleCancel(run)}
                                >
                                  <CircleSlash className="mr-2 h-3.5 w-3.5" />
                                  Cancel run
                                </DropdownMenuItem>
                              </>
                            )}
                          </DropdownMenuContent>
                        </DropdownMenu>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
                {openRun === run.id && (
                  <TableRow className="hover:bg-transparent">
                    <TableCell colSpan={cols} className="p-0 whitespace-normal" id={`job-run-${run.id}`}>
                      <RunDetail appId={appId} appSlug={appSlug} run={run} />
                    </TableCell>
                  </TableRow>
                )}
              </Fragment>
            );
          })}
        </TableBody>
      </Table>
    </TableCard>
  );
}

/** Beside the status: where a queued run stands, how far a running one has got,
 *  which instant it is for, and which run it repeats. */
function RunAside({ run }: { run: JobRun }) {
  const bits: ReactNode[] = [];
  if (run.status === "queued" && run.queue_position) {
    bits.push(`${ordinal(run.queue_position)} in line`);
  }
  if (run.status === "running" && run.progress > 0) bits.push(`${run.progress}%`);
  if (run.scheduled_for) bits.push(`for ${formatTimestamp(run.scheduled_for)}`);
  if (run.rerun_of) bits.push(`again, of ${run.rerun_of.slice(0, 8)}`);
  if (bits.length === 0) return null;
  return <span className="text-xs text-muted-foreground">{bits.join(" · ")}</span>;
}

/** One run, opened: its facts, then what it printed. */
function RunDetail({ appId, appSlug, run }: { appId: string; appSlug: string; run: JobRun }) {
  const params = paramsSummary(run.params);
  return (
    <div className="space-y-3 p-4">
      <dl className="grid gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
        <RunFact label="Run">
          <span className="font-mono">{run.id.slice(0, 8)}</span>
        </RunFact>
        <RunFact label="Params">
          <span className="font-mono break-all">{params || "None"}</span>
        </RunFact>
        {run.started_at && (
          <RunFact label="Took">
            {run.finished_at ? formatDuration(run.started_at, run.finished_at) : "Still going"}
          </RunFact>
        )}
        {run.deadline && jobRunInFlight(run.status) && (
          <RunFact label="Stopped at">{formatDateTime(run.deadline)}</RunFact>
        )}
        {run.peak_memory_mb !== undefined && (
          <RunFact label="Peak memory">{formatJobMemory(run.peak_memory_mb)}</RunFact>
        )}
        {run.cpu_throttled_seconds !== undefined && (
          <RunFact label="Held at CPU limit">{formatThrottled(run.cpu_throttled_seconds)}</RunFact>
        )}
        {run.exit_code !== undefined && <RunFact label="Exit code">{run.exit_code}</RunFact>}
        {run.progress_message && <RunFact label="Progress">{run.progress_message}</RunFact>}
        {run.failure_reason && (
          <RunFact label="Why">
            <span className={toneText.danger}>{run.failure_reason}</span>
          </RunFact>
        )}
        <RunFact label="Status page">
          <a
            href={appUrl(appSlug, `/__toft/runs/${run.id}`)}
            target="_blank" rel="noreferrer"
            className="inline-flex items-center gap-1 text-primary hover:underline"
          >
            Open <ExternalLink className="h-3 w-3" />
          </a>
        </RunFact>
      </dl>
      <RunLogs appId={appId} appSlug={appSlug} run={run} />
    </div>
  );
}

function RunFact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="text-foreground">{children}</dd>
    </div>
  );
}

/**
 * A run's lines, as the terminal pane wants them. `stderr` becomes a tint
 * rather than a class, so a line the job coloured with ANSI keeps its colour.
 */
function runLogLines(lines: RunLogLine[]): LogPaneLine[] {
  return lines.map((l) => ({
    line: l.line,
    at: l.at,
    tint: l.stream === "stderr" ? "stderr" : undefined,
  }));
}

function RunLogs({ appId, appSlug, run }: { appId: string; appSlug: string; run: JobRun }) {
  const inFlight = jobRunInFlight(run.status);
  const log = useRunLog(run.id, (after) => api.getJobRunLogs(appId, run.id, after), inFlight);

  if (log.loading) return <Skeleton className="h-24 w-full" />;
  // Before the pane, which would otherwise report a failed read as a run that
  // printed nothing — and send somebody debugging their own job.
  if (log.error) {
    return (
      <QueryError
        title="This run’s log could not be read"
        error={log.error}
        fallback="You need edit access to the app."
        onRetry={log.reload}
      />
    );
  }
  return (
    <LogPane
      lines={runLogLines(log.lines)}
      label={`Log of this run of ${run.job}`}
      subject="log"
      paneClassName="max-h-80"
      stream={inFlight ? "live" : "none"}
      emptyMessage={inFlight ? "No output yet." : "This run printed nothing."}
      downloadName={(narrowed) =>
        `${appSlug}-${run.job}-${run.id.slice(0, 8)}${narrowed ? "-filtered" : ""}.log`
      }
      footnote={
        log.dropped > 0 ? (
          <span className={toneText.attention}>
            Showing the last {RUN_LOG_CAP.toLocaleString()} lines. {log.dropped.toLocaleString()} earlier{" "}
            {log.dropped === 1 ? "line is" : "lines are"} not on screen.
          </span>
        ) : (
          "Logs are kept for less time than the run record."
        )
      }
    />
  );
}

function RunsSkeleton() {
  return (
    <TableCard className="space-y-2 p-4">
      {[0, 1, 2].map((i) => (
        <Skeleton key={i} className="h-8 w-full" />
      ))}
    </TableCard>
  );
}

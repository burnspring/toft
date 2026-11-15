import { useState } from "react";

import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/components/status-badge";

import { severityTone } from "@/lib/status";

import { TableCard } from "@/components/table-card";
import { SectionHeading } from "@/components/section";
import { RelativeTime } from "@/components/relative-time";

import { AlertTriangle, CheckCircle2 } from "lucide-react";

import { api } from "@/lib/api";

import type { AppEvent } from "@/lib/api";
import { QueryError } from "@/components/query-error";
import { rowActivation } from "@/lib/row-activation";

const EVENT_KIND_LABEL: Record<string, string> = {
  deploy_failed: "Deploy failed",
  git_auth_failed: "Repository access denied",
  git_fetch_failed: "Repository unreachable",
  app_crashed: "Crashed",
  app_flapping: "Restart loop",
  app_restarted: "Restarted",
  app_memory_limit: "Memory limit",
  app_out_of_memory: "Out of memory",
  app_cpu_throttled: "Held at CPU limit",
  app_not_restarting: "Not restarting",
  // Rows written before scheduled tasks were removed still carry this kind.
  cron_run_failed: "Scheduled task failed",
  job_run_failed: "Job run failed",
  schedule_disabled: "Schedule disabled",
  schedule_removed: "Schedule removed",
  job_schedule_skipped: "Scheduled run skipped",
  job_run_overdue: "Job overdue",
  limit_clamped: "Job limit lowered",
  releases_pruned: "Old versions removed",
};

/**
 * What has gone wrong with this app, and when.
 *
 * Every failure is here whether or not anyone was emailed about it — the
 * notification switch and the cooldown both act on mail alone. A resolved row
 * is kept rather than removed: "it broke on Tuesday and fixed itself" is the
 * answer to a question somebody will ask on Friday.
 */
export function EventsTab({ appId }: { appId: string }) {
  const { data: events, isLoading, error, refetch } = useQuery({
    queryKey: ["app-events", appId],
    queryFn: () => api.listAppEvents(appId, { limit: 100 }),
    retry: false,
  });

  if (isLoading) {
    return (
      <TableCard className="space-y-3 p-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="flex items-center gap-3">
            <Skeleton className="h-4 w-32" />
            <Skeleton className="h-4 w-40" />
            <Skeleton className="ml-auto h-4 w-56" />
          </div>
        ))}
      </TableCard>
    );
  }

  if (error) {
    return (
      <div className="pt-4">
        <QueryError
          title="This app’s failures could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see its failures."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  // A section above the log stream now, not a tab of its own — so the empty
  // case is one reassuring line rather than a sixteen-rem empty state pushing
  // the logs off the screen. The reassurance is worth keeping: "nothing has
  // gone wrong" and "we are not recording" look identical otherwise.
  if (!events || events.length === 0) {
    return (
      // **Scoped, because of what sits directly beside it.** This line is
      // about the platform's own record — deploys, crashes, scheduled runs —
      // and the pane it shares a tab with is the workload's stdout. On an app
      // whose code is logging errors all day, "Nothing has gone wrong" beside
      // a wall of them read as the page contradicting itself. Naming whose
      // record it is costs four words.
      <p className="flex max-w-prose items-start gap-2 pt-4 text-sm text-muted-foreground">
        <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-tone-ok-fg" />
        <span>
          Toft has recorded no failed deploys, crashes or missed scheduled runs for
          this app.
        </span>
      </p>
    );
  }

  return (
    <div className="space-y-3 pt-4">
      <SectionHeading
        title="Recent failures"
        description="Every failure is listed; repeats are emailed at most once every few hours."
      />
      <TableCard>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>When</TableHead>
              <TableHead>What</TableHead>
              <TableHead className="w-full">Summary</TableHead>
              <TableHead className="hidden @xl:table-cell">State</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((event) => (
              <EventRow key={event.id} appId={appId} event={event} />
            ))}
          </TableBody>
        </Table>
      </TableCard>
    </div>
  );
}

function EventRow({ appId, event }: { appId: string; event: AppEvent }) {
  const [open, setOpen] = useState(false);
  const expandable = event.detail.trim().length > 0;

  return (
    <>
      <TableRow
        {...(expandable
          ? rowActivation(`${open ? "Hide" : "Show"} the detail of: ${event.summary}`, () =>
              setOpen((o) => !o),
            )
          : {})}
      >
        <TableCell className="text-sm text-muted-foreground">
          <RelativeTime iso={event.occurred_at} />
        </TableCell>
        <TableCell>
          <StatusBadge
            status=""
            tone={severityTone(event.severity)}
            label={EVENT_KIND_LABEL[event.kind] ?? event.kind.replace(/_/g, " ")}
          />
        </TableCell>
        <TableCell className="text-sm whitespace-normal">
          <span className="text-muted-foreground">{event.summary}</span>
          {/* That build's log, not the table of every build. The row carries
              the deployment id, and the log is the answer to the question the
              row raises — the same address the failure banner above the tabs
              already links to. "View deployments" landed a reader on a list
              and left them to find the row they had just clicked. */}
          {event.deployment_id && (
            <Link
              to={`/apps/${appId}/logs?source=build&build=${event.deployment_id}`}
              className="ml-2 text-xs text-primary hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              View build log
            </Link>
          )}
          {/* State's restatement, on its column's own tier. */}
          <div className="mt-0.5 text-2xs @xl:hidden">
            <EventState resolved={!!event.resolved_at} />
          </div>
        </TableCell>
        <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">
          <EventState resolved={!!event.resolved_at} />
        </TableCell>
      </TableRow>
      {open && expandable && (
        <TableRow className="hover:bg-transparent">
          <TableCell colSpan={4} className="bg-muted/40 p-0">
            <pre className="max-h-72 overflow-auto px-6 py-3 font-mono text-xs whitespace-pre-wrap text-muted-foreground">
              {event.detail}
            </pre>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

function EventState({ resolved }: { resolved: boolean }) {
  return resolved ? (
    <span className="inline-flex items-center gap-1 text-tone-ok-fg">
      <CheckCircle2 className="h-3.5 w-3.5" />
      Resolved
    </span>
  ) : (
    <span className="inline-flex items-center gap-1 text-muted-foreground">
      <AlertTriangle className="h-3.5 w-3.5" />
      Open
    </span>
  );
}

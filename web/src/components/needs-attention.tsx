import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, ChevronRight } from "lucide-react";
import { api, statusOf } from "@/lib/api";
import { TableCard } from "@/components/table-card";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { Skeleton } from "@/components/ui/skeleton";
import { toneSolid, toneSurface, toneText, type Tone } from "@/lib/tone";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { cn } from "@/lib/utils";
import type { SystemCheckReport, SystemHealth, BuildQueue, TelemetrySettings } from "@/lib/api";

/**
 * What is wrong on this install right now, and where to fix it.
 *
 * Admin → Info was the best-designed page here and answered the wrong
 * question: it described the install's *shape* — version, uptime, toolchains,
 * how many apps of each status — which is what somebody wants on their first
 * day and almost never afterwards. What an administrator actually arrives with
 * is "is anything broken", and answering it meant visiting System Checks, then
 * Queue, then Telemetry, then Publisher Requests, then scanning the apps list
 * for crashes. Twenty-seven destinations and no front page.
 *
 * Three rules, and they are what stop this becoming a dashboard nobody reads:
 *
 * - **Nothing wrong means nothing here.** One quiet line, not five cards
 *   reading zero. A console that always shows the same rows teaches people to
 *   stop looking at it, and a row that is only ever "0 crashed" is noise that
 *   will one day say "1" to someone who has learned to skip it.
 * - **Every row goes somewhere.** A count with no destination is a puzzle. Each
 *   one links to the page that fixes that particular thing, not to a general
 *   area.
 * - **It reads; it never acts.** Approving, restarting and reconfiguring all
 *   stay on the pages that own them, with their own confirmations and audit.
 *   A summary that mutates is a summary somebody will click by accident.
 *
 * Every query here is one the admin section already makes, at the interval it
 * already uses, and each `retry: false` — a capability that is off answers 404
 * and that is an absent row, not an error.
 */

interface Item {
  key: string;
  tone: Tone;
  count?: number;
  label: string;
  href: string;
}

/**
 * `heading` is drawn over a list, a refusal or a skeleton, and not over the
 * all-clear: an 18px "Needs attention" above "Nothing needs attention" was
 * the loudest thing on the page saying the quietest thing.
 */
export function NeedsAttention({ heading }: { heading?: React.ReactNode } = {}) {
  // The three that are not optional read their own failure. An all-clear is a
  // *positive assertion* about the install, and it was rendered from an empty
  // `items` array -- which is true in three situations this component could not
  // tell apart: nothing is wrong, the reads have not answered, and the reads
  // failed. So every cold load of /admin showed a green tick for a round trip,
  // and an install whose /system/health was returning 500 showed one for ever.
  // On the surface most likely to be read during an incident, in the worse
  // direction: a reassurance rather than an invitation.
  const {
    data: health, isLoading: healthLoading, error: healthErr, refetch: refetchHealth,
  } = useQuery<SystemHealth>({
    queryKey: ["system-health"],
    queryFn: api.getSystemHealth,
    refetchInterval: 10_000,
    retry: false,
  });
  const {
    data: checks, isLoading: checksLoading, error: checksErr, refetch: refetchChecks,
  } = useQuery<SystemCheckReport>({
    queryKey: ["system-checks"],
    queryFn: api.getSystemChecks,
    retry: false,
  });
  const {
    data: queue, isLoading: queueLoading, error: queueErr, refetch: refetchQueue,
  } = useQuery<BuildQueue>({
    queryKey: ["system-queue"],
    queryFn: api.getQueue,
    refetchInterval: 10_000,
    retry: false,
  });
  const { data: promotions, error: promotionsErr, refetch: refetchPromotions } = useQuery({
    queryKey: ["promotion-requests", "pending"],
    queryFn: () => api.adminListPromotionRequests("pending"),
    retry: false,
  });
  const { data: telemetry, error: telemetryErr, refetch: refetchTelemetry } = useQuery<TelemetrySettings>({
    queryKey: ["telemetry-settings"],
    queryFn: api.getTelemetrySettings,
    refetchInterval: 30_000,
    retry: false,
  });

  // The two optional reads answer 404 when their capability is off, which is
  // an absent row. Anything else is a failure, and a failure that shortened
  // the list silently could produce the all-clear.
  const optionalErr = (e: unknown) => (e && statusOf(e) !== 404 ? e : null);
  const unread =
    healthErr ?? checksErr ?? queueErr ?? optionalErr(promotionsErr) ?? optionalErr(telemetryErr);
  const loading = healthLoading || checksLoading || queueLoading;
  const retryAll = () => {
    void refetchHealth();
    void refetchChecks();
    void refetchQueue();
    void refetchPromotions();
    void refetchTelemetry();
  };

  const items: Item[] = [];

  if (checks?.failed) {
    items.push({
      key: "checks-failed",
      tone: "danger",
      count: checks.failed,
      label: `system check${checks.failed === 1 ? "" : "s"} failing`,
      href: "/admin/checks",
    });
  }
  if (checks?.warned) {
    items.push({
      key: "checks-warned",
      tone: "attention",
      count: checks.warned,
      label: `system check${checks.warned === 1 ? "" : "s"} warning`,
      href: "/admin/checks",
    });
  }
  // Two rows rather than one, and each carries the filter it counts. A single
  // "crashed" row counted failed apps too and linked to a bare `/`, so one
  // failed app read as a crash and the reader landed on an unfiltered list
  // scoped to their own apps — where the Crashed tab, if they found it, held
  // nothing. The count here is the whole estate, so the link says `scope=all`.
  if (health?.summary.crashed) {
    items.push({
      key: "crashed",
      tone: "danger",
      count: health.summary.crashed,
      label: `app${health.summary.crashed === 1 ? "" : "s"} crashed`,
      href: "/?scope=all&status=crashed",
    });
  }
  if (health?.summary.failed) {
    items.push({
      key: "failed",
      tone: "danger",
      count: health.summary.failed,
      label: `app${health.summary.failed === 1 ? "" : "s"} failed to build`,
      href: "/?scope=all&status=failed",
    });
  }

  // A queue with work in it is not a fault — builds are meant to queue. It is
  // worth a line only when there is more waiting than can start, which is the
  // difference between "busy" and "backed up".
  if (queue && queue.count > queue.concurrency) {
    items.push({
      key: "queue",
      tone: "attention",
      count: queue.count,
      label: `builds queued, ${queue.concurrency} at a time`,
      href: "/admin/processes#queue",
    });
  }
  if (promotions?.length) {
    items.push({
      key: "promotions",
      tone: "info",
      count: promotions.length,
      label: `publisher request${promotions.length === 1 ? "" : "s"} waiting`,
      href: "/admin/publisher-requests",
    });
  }

  const h = telemetry?.health;
  if (h && (h.export_failures > 0 || h.metric_failures > 0 || h.spans_dropped > 0)) {
    items.push({
      key: "telemetry",
      tone: "attention",
      label: "Telemetry isn’t reaching the collector",
      href: "/admin/telemetry",
    });
  }

  // Three predecessors to the all-clear, so that by the time it renders all
  // three reads have answered and it is true.
  const titled = (body: React.ReactNode) => (
    <div className="space-y-2">
      {heading}
      {body}
    </div>
  );

  if (unread && items.length === 0) {
    return titled(
      <QueryError
        title="What needs attention could not be read"
        error={unread}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={retryAll}
      />,
    );
  }
  if (loading && items.length === 0) {
    // One bar the shape of the one line it replaces. Three rows would imply
    // three faults before anything has been read.
    return titled(<Skeleton className="h-5 w-[34rem] max-w-full" />);
  }
  if (items.length === 0) {
    return (
      <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
        <CheckCircle2 className="h-4 w-4 text-tone-ok-fg" />
        <span>
          Nothing needs attention.
          {/* When the checks half of that was taken. The health and queue reads
              poll every ten seconds; the checks report can take twenty to
              produce, so it is fetched once and re-run on a return to the tab
              — which makes it the oldest thing in this sentence and the one
              the sentence has to date. An all-clear with no age is a claim
              about now made from a reading of some other time. */}
          {checks?.ran_at && (
            <>
              {" "}Checked <RelativeTime iso={checks.ran_at} />.
            </>
          )}
        </span>
      </p>
    );
  }

  return titled(
    <div className="space-y-3">
      {/* A partial failure is never silently absorbed into a shorter list. */}
      {unread && (
        <Notice
          tone="attention"
          size="sm"
          title="This list may be incomplete"
          action={
            <button type="button" onClick={retryAll} className="font-medium hover:underline">
              Retry
            </button>
          }
        >
          Some of these checks couldn’t be read.
        </Notice>
      )}
    <TableCard className="divide-y divide-border/60 overflow-hidden">
      {items.map((item) => (
        <Link
          key={item.key}
          to={item.href}
          className="flex items-center gap-3 px-4 py-2.5 text-sm transition-colors hover:bg-muted/50"
        >
          {/* A row with no count gets a tone dot in the bubble's place: a "!"
              in a count bubble read as a count. */}
          {item.count !== undefined ? (
            <span
              className={cn(
                "flex h-6 min-w-6 items-center justify-center rounded-full border px-1.5 font-mono text-xs font-semibold",
                toneSurface[item.tone],
              )}
            >
              {item.count}
            </span>
          ) : (
            <span className="flex h-6 min-w-6 items-center justify-center" aria-hidden>
              <span className={cn("h-2 w-2 rounded-full", toneSolid[item.tone])} />
            </span>
          )}
          <span className={cn("flex-1", item.count === undefined && toneText[item.tone])}>
            {item.label}
          </span>
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
        </Link>
      ))}
    </TableCard>
    </div>,
  );
}

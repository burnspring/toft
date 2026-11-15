import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/lib/api";
import type { BuildQueue, SystemCheckReport, SystemHealth, TelemetrySettings } from "@/lib/api";
import { adminGroups } from "@/lib/admin-nav";
import { PageHeader } from "@/components/page-header";
import { SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { NeedsAttention } from "@/components/needs-attention";
import { toneSurface, type Tone } from "@/lib/tone";
import { cn } from "@/lib/utils";

/**
 * Admin home.
 *
 * What needs attention, then one tile per destination that has a live fact to
 * show. The directory is the sidebar beside it; repeating all twenty-six pages
 * here as tiles was a second copy of it (ADR §50).
 *
 * Four rules, and they are what keep a hub from becoming a second dashboard:
 *
 * - **Needs attention stays at the top.** It is already built, already links
 *   each row to the page that fixes it, already mutates nothing, and is already
 *   quiet when there is nothing wrong.
 * - **No tile mutates.** Approving, restarting and refreshing stay on the pages
 *   that own them, with their confirmations and their audit rows.
 * - **No request of its own.** Every fact below comes from a query the admin
 *   section already makes, on the key it already uses, so react-query serves it
 *   from the cache the moment anything else has asked. A destination with no
 *   cheap fact gets none: a tile is not worth a round trip, and a count of zero
 *   dressed as a fact is worse than silence.
 * - **One list.** The tiles are drawn from `adminGroups`, the same array the
 *   sidebar and the command palette render, so a tile cannot name a page the
 *   nav does not have.
 */
export function AdminHomePage() {
  // All five are NeedsAttention's own queries, verbatim — same keys, same
  // intervals, same `retry: false` so a capability that is off answers 404 and
  // becomes an absent fact rather than an error.
  const { data: health } = useQuery<SystemHealth>({
    queryKey: ["system-health"],
    queryFn: api.getSystemHealth,
    refetchInterval: 10_000,
    retry: false,
  });
  const { data: checks } = useQuery<SystemCheckReport>({
    queryKey: ["system-checks"],
    queryFn: api.getSystemChecks,
    retry: false,
  });
  const { data: queue } = useQuery<BuildQueue>({
    queryKey: ["system-queue"],
    queryFn: api.getQueue,
    refetchInterval: 10_000,
    retry: false,
  });
  const { data: promotions } = useQuery({
    queryKey: ["promotion-requests", "pending"],
    queryFn: () => api.adminListPromotionRequests("pending"),
    retry: false,
  });
  const { data: telemetry } = useQuery<TelemetrySettings>({
    queryKey: ["telemetry-settings"],
    queryFn: api.getTelemetrySettings,
    retry: false,
  });

  /**
   * The one line of live state a tile carries, where there is one to carry —
   * and what that state *is*.
   *
   * Every one of these used to render in the same small grey mono, so `1
   * warning` was typographically identical to `v0.2.0`: the badge that means
   * *go and look* read exactly like the badge that means *this is the build
   * number*. A version is a fact with no valence and stays neutral; a failure,
   * a warning and a queue with work in it are states, and say so.
   */
  function fact(href: string): { text: string; tone?: Tone } | undefined {
    switch (href) {
      case "/admin/info":
        return health?.server ? { text: `v${health.server.version}` } : undefined;
      case "/admin/checks":
        // Failing, then warning, then the count that passed. Reporting
        // "21 passing" over an install with a warning is the kind of half-truth
        // that teaches people to stop reading the tile.
        if (!checks) return undefined;
        if (checks.failed) return { text: `${checks.failed} failing`, tone: "danger" };
        if (checks.warned)
          return {
            text: `${checks.warned} warning${checks.warned === 1 ? "" : "s"}`,
            tone: "attention",
          };
        return { text: `${checks.passed} passing`, tone: "ok" };
      case "/admin/processes":
        // The build queue lives on Processes now, and builds in flight are
        // the livelier fact: the platform acting, not a person and not a fault.
        if (queue?.count) return { text: `${queue.count} building`, tone: "info" };
        // Never "0 running": every app asleep and every app broken read the
        // same in two characters, and one of them is not a fact worth a tile.
        return health?.summary.running
          ? { text: `${health.summary.running} running`, tone: "ok" }
          : undefined;
      case "/admin/publisher-requests": {
        const pending = promotions?.length ?? 0;
        return pending > 0 ? { text: `${pending} waiting`, tone: "attention" } : undefined;
      }
      case "/admin/telemetry":
        if (!telemetry) return undefined;
        // Off is a recorded fact about a capability nobody switched on, not a
        // problem — which is exactly what `neutral` is for.
        return telemetry.enabled
          ? { text: "Sending", tone: "info" }
          : { text: "Off", tone: "neutral" };
      default:
        return undefined;
    }
  }

  const tiles = adminGroups
    .flatMap((group) => group.items)
    .filter((item) => item.sidebar !== false)
    .map((item) => ({ item, state: fact(item.href) }))
    .filter((t) => t.state !== undefined);

  return (
    <div>
      <PageHeader title="Admin" />

      <div className="space-y-8">
        <NeedsAttention heading={<SectionHeading title="Needs attention" />} />

        {tiles.length > 0 && (
          <div className="space-y-2">
            <SectionHeading title="At a glance" />
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {tiles.map(({ item, state }) => (
                <TableCard key={item.href} className="transition-colors hover:bg-muted/40">
                  <Link to={item.href} className="flex h-full items-baseline justify-between gap-3 p-4">
                    <span className="text-sm font-medium text-foreground">{item.label}</span>
                    <span
                      className={cn(
                        "shrink-0 rounded border px-1.5 py-0.5 text-xs",
                        // A state is a word, in the badge face StatusBadge uses;
                        // a version is a literal, and keeps the mono voice.
                        state!.tone
                          ? cn("font-medium", toneSurface[state!.tone])
                          : "border-transparent font-mono text-muted-foreground",
                      )}
                    >
                      {state!.text}
                    </span>
                  </Link>
                </TableCard>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { ArrowUpRight, ChevronDown, ClipboardCopy, RefreshCw } from "lucide-react";
import { api, type SystemCheckReport } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { TableCard } from "@/components/table-card";
import { RelativeTime } from "@/components/relative-time";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { QueryError } from "@/components/query-error";
import { BuildCacheCard } from "@/components/build-cache-card";
import { DisksCard } from "@/components/disks-card";
import { disksBeyondTheRows } from "@/lib/disks";
import { copyToClipboard } from "@/lib/clipboard";
import { checkDestination, reportAsText, sortChecks } from "@/lib/system-checks";

/** What went wrong, first — or nothing, when the badge beside the title
 *  already says *Passed*. */
function headline(report: SystemCheckReport): string {
  if (report.failed > 0) {
    return `${report.failed} check${report.failed === 1 ? "" : "s"} failed. `;
  }
  if (report.warned > 0) {
    return `${report.warned} check${report.warned === 1 ? " needs" : "s need"} attention. `;
  }
  return "";
}

/** The rest of the tally, without restating whatever the headline counted. */
function tally(report: SystemCheckReport): string {
  const parts = [`${report.passed} passed`];
  if (report.failed > 0 && report.warned > 0) {
    parts.push(`${report.warned} warning${report.warned === 1 ? "" : "s"}`);
  }
  if (report.skipped > 0) parts.push(`${report.skipped} not applicable`);
  return parts.join(", ");
}

/**
 * Admin → System Checks: the install's self-diagnosis.
 *
 * There is no history to page through, deliberately. A stored report would be
 * a weaker second copy of facts the platform already keeps — Activity for what
 * people did, an app's Logs tab for what broke, Processes for what the host is
 * doing — and the question this page answers is always about right now: an
 * operator opens it because something is wrong, or before they change
 * something. So the report is computed when asked for, and the button asks
 * again rather than filing another row.
 */
/** One sentence about this page, said the same way on every branch.
 *  Each of these files hand-wrote a second, shorter one for its error and
 *  loading paths, so the page described itself one way to whoever loaded it
 *  and another to whoever was refused. */
const PAGE_DESCRIPTION =
  "What this install can see about itself, checked when you ask.";

export function AdminChecksPage() {
  const [showSkipped, setShowSkipped] = useState(false);
  const { data, isLoading, isFetching, error, refetch } = useQuery<SystemCheckReport>({
    queryKey: ["system-checks"],
    queryFn: api.getSystemChecks,
    retry: false,
    // A reclaim is the one thing on this page that finishes while somebody is
    // watching it, and it has no stream — started, running and freed is all
    // there is to know, so the report is re-asked until it stops saying
    // "running". Off the rest of the time: this page's answer is about now, and
    // a poll that never stops is a page that never settles.
    refetchInterval: (q) => (q.state.data?.build_cache?.reclaiming ? 5000 : false),
  });

  if (error) {
    return (
      <div>
        <PageHeader title="System checks" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The system checks could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const sorted = sortChecks(data?.checks ?? []);
  // A row that does not apply is the shape of the install, not a reading, and
  // eleven of them pushed the build cache below the fold. Folded under
  // the table, counted, one click away.
  const skipped = sorted.filter((c) => c.status === "skip").length;
  const checks = showSkipped ? sorted : sorted.filter((c) => c.status !== "skip");
  const beyond = data?.disks ? disksBeyondTheRows(data.disks) : null;

  return (
    <div>
      <PageHeader
        title="System checks"
        badge={data && <StatusBadge status={data.status} />}
        description={
          data ? (
            <>
              {headline(data)}
              {tally(data)}. Run on {data.hostname},{" "}
              <RelativeTime iso={data.ran_at} />.
            </>
          ) : (
            PAGE_DESCRIPTION
          )
        }
        actions={
          <div className="flex items-center gap-2">
            {/* What an operator does next: paste the state of this install into
                a ticket or a chat. It is also why the no-history decision
                survives a page of forty rows — the report leaves in their
                hands rather than into a table with a retention policy. */}
            <Button
              size="sm"
              variant="outline"
              disabled={!data}
              onClick={() => data && copyToClipboard(reportAsText(data), "report")}
            >
              <ClipboardCopy className="mr-1.5 h-4 w-4" />
              Copy report
            </Button>
            <Button size="sm" onClick={() => refetch()} disabled={isFetching}>
              {/* No spin: motion is spent on the deploy, and the label
                  already says "Running…". */}
              <RefreshCw className="mr-1.5 h-4 w-4" />
              {isFetching ? "Running…" : "Run checks"}
            </Button>
          </div>
        }
      />

      {isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 8 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-[220px]">Check</TableHead>
                <TableHead className="hidden w-[130px] @md:table-cell">Area</TableHead>
                <TableHead className="w-[140px]">Result</TableHead>
                {/* Detail is the payload — it is the sentence that says what a
                    warning is about — so it is never the column that goes. Below
                    lg it moves under the check's name rather than off the edge. */}
                <TableHead className="hidden @3xl:table-cell">Detail</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {checks.map((c) => {
                // Only a finding has one: the backend never stamps a
                // destination onto a pass or a skip, so a row that does not
                // link is a row that is telling you the shape of the install
                // rather than asking for something.
                const href = checkDestination(c);
                return (
                <TableRow key={c.id}>
                  <TableCell className="align-top font-medium whitespace-normal">
                    {href ? (
                      // The arrow, not the colour, is what says this one goes
                      // somewhere: only findings link here, so most rows are
                      // plain text and a hover-only underline would leave the
                      // affordance undiscoverable on a page of forty rows.
                      <Link
                        to={href}
                        className="inline-flex items-center gap-1 text-foreground hover:text-primary hover:underline"
                      >
                        {c.name}
                        <ArrowUpRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
                      </Link>
                    ) : (
                      c.name
                    )}
                    {/* Container variants, matching the columns above. These
                        were window breakpoints, so between a container at 768
                        and a window at 1024 the detail rendered twice. */}
                    <span className="mt-0.5 block text-xs font-normal text-muted-foreground @md:hidden">
                      {c.category}
                    </span>
                    <span className="mt-1 block text-xs font-normal text-muted-foreground @3xl:hidden">
                      {c.detail}
                    </span>
                  </TableCell>
                  <TableCell className="hidden align-top text-sm text-muted-foreground @md:table-cell">
                    {c.category}
                  </TableCell>
                  <TableCell className="align-top">
                    <StatusBadge status={c.status} />
                  </TableCell>
                  {/* Wrapping, not nowrap: a detail long enough to matter is a
                      detail long enough to push itself past the card's edge. */}
                  <TableCell className="hidden align-top text-sm whitespace-normal text-muted-foreground @3xl:table-cell">
                    {c.detail}
                  </TableCell>
                </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableCard>
      )}

      {skipped > 0 && (
        <button
          type="button"
          onClick={() => setShowSkipped(!showSkipped)}
          aria-expanded={showSkipped}
          className="mt-3 flex items-center gap-1 rounded-sm text-xs font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
        >
          {/* Disclosure's shape, drawn here because what it opens is rows of
              the table above rather than a block below it. */}
          <ChevronDown
            className={`h-3.5 w-3.5 transition-transform ${showSkipped ? "" : "-rotate-90"}`}
            aria-hidden
          />
          {skipped} not applicable — capabilities this install doesn’t offer
        </button>
      )}

      {/* Only the disks no row measures — a storage drive's, or one that did
          not answer. Disk space and Release disk already state the rest, and
          a card restating them said every figure twice. */}
      {beyond && <DisksCard block={beyond} />}

      {/* Last, and after the paragraph that closes off the checks. Every row
          above is a read; this is the one control on the page that destroys
          something, and it belongs where nobody passes it on the way to the
          answer they opened the page for. */}
      {data?.build_cache && (
        <BuildCacheCard
          cache={data.build_cache}
          cacheCheck={data.checks.find((c) => c.id === "cache_dir")}
        />
      )}
    </div>
  );
}

import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Inbox } from "lucide-react";
import { api, type BuildQueue } from "@/lib/api";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Section, SectionHeading } from "@/components/section";
import { AppOwner } from "@/components/app-owner";
import { StatusBadge } from "@/components/status-badge";
import { EmptyState } from "@/components/empty-state";
import { RelativeTime } from "@/components/relative-time";
import { QueryError } from "@/components/query-error";

// A build finishes without anyone asking, so the page has to notice on its
// own — the same interval as Processes.
const REFRESH_MS = 10_000;

/**
 * Admin → Queue: every build that has not finished.
 *
 * The queue itself is an in-memory channel inside the builder, so this is the
 * queue as the database can see it: `pending` and `building` deployments,
 * oldest first, which is the order the worker will take them in. Position is
 * derived from that order rather than stored — a number written to a row would
 * be wrong the moment anything ahead of it finished.
 */
export function AdminQueuePage() {
  // A section of another admin page rather than a page of its own (ADR §50):
  // the host wraps it in the anchor its old address redirects to.
  const { data, isLoading, error, refetch } = useQuery<BuildQueue>({
    queryKey: ["system-queue"],
    queryFn: api.getQueue,
    refetchInterval: REFRESH_MS,
    retry: false,
  });

  if (error) {
    return (
      <div className="space-y-3">
        <SectionHeading title="Build queue" />
        <QueryError
          title="The queue could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const items = data?.items ?? [];

  return (
    <div>
      {/* A Section, like Running workloads above it: two halves of one
          question drawn in one grammar. The count is the description, and
          only when there is one — the empty state says the rest. */}
      <Section
        title="Build queue"
        description={
          // The install's concurrency and the restart caveat used to float
          // under the card as a loose line; they describe the queue, so they
          // are its description.
          data
            ? `${data.count > 0 ? `${data.count} build${data.count === 1 ? "" : "s"} waiting and in progress, in order. ` : ""}Up to ${data.concurrency} at once; restarting Toft fails any queued build.`
            : undefined
        }
      >
        {isLoading ? (
          <div className="space-y-2 p-4">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-10" />
            ))}
          </div>
        ) : items.length === 0 ? (
          <EmptyState
            size="inline"
            icon={<Inbox className="h-6 w-6" />}
            title="Nothing is building"
            description="Finished builds are on each app’s Deployments tab."
          />
        ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {/* Place in the queue, which app, and which of the two waits
                      it is in — that is the page. Who owns it, what triggered it
                      and how long it has been there are context, and yield. */}
                  <TableHead className="w-[60px]">#</TableHead>
                  <TableHead className="w-full">App</TableHead>
                  <TableHead className="hidden @2xl:table-cell">Owner</TableHead>
                  <TableHead className="hidden w-[110px] @3xl:table-cell">Trigger</TableHead>
                  <TableHead className="w-[110px]">Status</TableHead>
                  <TableHead className="hidden w-[130px] @md:table-cell">Queued</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell className="text-sm font-medium">{item.position}</TableCell>
                    <TableCell className="max-w-0">
                      <span className="flex items-baseline gap-2">
                        <Link
                          to={`/apps/${item.app_id}`}
                          className="truncate font-medium text-foreground hover:text-primary hover:underline"
                        >
                          {item.app_name}
                        </Link>
                        {item.commit_sha && (
                          <span className="shrink-0 font-mono text-xs text-muted-foreground">
                            {item.commit_sha.slice(0, 7)}
                          </span>
                        )}
                      </span>
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @2xl:table-cell">
                      {/* One component that knows how to render either. A group
                          owns an app just as a person does, and "—" said nobody
                          did — the one thing the ownership model forbids. */}
                      <AppOwner
                        ownerId={item.owner_id}
                        ownerGroupId={item.owner_group_id}
                        ownerLogin={item.owner_login}
                        withCount={false}
                      />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                      {/* Same vocabulary as the app's Deployments tab: a manual
                          deploy is the unremarkable case and gets no chip. */}
                      {item.triggered_by === "manual" ? (
                        "Manual"
                      ) : (
                        <StatusBadge status={item.triggered_by} />
                      )}
                    </TableCell>
                    <TableCell>
                      {/* `pending` is waiting for the worker; `building` is the
                          one it is on. Two different waits, and an operator
                          wondering why a deploy is slow needs to tell them
                          apart. */}
                      <StatusBadge
                        status={item.status}
                        label={item.status === "pending" ? "Waiting" : "Building"}
                      />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @md:table-cell">
                      {item.enqueued_at ? <RelativeTime iso={item.enqueued_at} /> : "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
        )}
      </Section>

    </div>
  );
}

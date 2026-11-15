import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { MoreHorizontal, Plus } from "lucide-react";
import { useState } from "react";

import { api } from "@/lib/api";
import type { AnnouncementEmail } from "@/lib/api";
import { useAuth } from "@/hooks/use-auth";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { DeleteAnnouncementDialog } from "@/components/delete-announcement-dialog";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { buttonVariants } from "@/components/ui/button-variants";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { toneSurface, type Tone } from "@/lib/tone";
import { AUDIENCE_LABEL } from "@/lib/announcements";

/**
 * Admin → Messages → Announcements: what this install has told the people on
 * it, and the button that tells them something else.
 *
 * **The table is a record, not a queue.** Every row is a thing that happened,
 * and the only column anybody scans for is the one that says how many people it
 * did not reach — so failures lead the counts and a row with none says nothing
 * at all rather than "0 failed".
 *
 * With no relay configured the button is **absent** rather than disabled: a
 * Send that promises mail an install cannot send is "copy may not promise mail"
 * broken by a control rather than by a sentence. Composing still works, and the
 * notice says where to fix it.
 *
 * **A record being a record does not make it permanent.** Every row that is not
 * mid-flight carries a delete, because an install's announcement history is the
 * administrator's to keep or be rid of — and the row in flight has no menu at
 * all rather than a disabled item, since the runner is still writing into it
 * and it becomes deletable a minute later on its own.
 */

const STATUS: Record<AnnouncementEmail["status"], { label: string; tone: Tone }> = {
  // Not `attention`. A draft nobody has sent is a thing doing exactly what it
  // was asked to, which is what neutral means.
  draft: { label: "Draft", tone: "neutral" },
  sending: { label: "Sending", tone: "info" },
  sent: { label: "Sent", tone: "ok" },
};

export function AnnouncementEmailList() {
  const { user } = useAuth();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [toDelete, setToDelete] = useState<AnnouncementEmail | null>(null);

  const isAdmin = user?.system_role === "system_admin";
  const query = useQuery({
    queryKey: ["announcement-emails"],
    queryFn: api.listAnnouncementEmails,
    enabled: isAdmin,
  });

  if (!isAdmin) return null;

  const list = query.data?.announcements ?? [];

  return (
    <Section
      title="Announcements"
      description="Email to the people on this install, from you."
      actions={
        query.data?.can_send ? (
          // A link to an empty composer, not a POST: the draft is created by
          // its first save, so backing out leaves nothing behind.
          <Link to="/admin/messages/announcement/new" className={buttonVariants({ size: "sm" })}>
            <Plus className="mr-1.5 h-4 w-4" aria-hidden />
            New announcement
          </Link>
        ) : undefined
      }
    >
      <div className="space-y-4 p-4">
        {query.data && !query.data.can_send && (
          <Notice
            tone="neutral"
            title="Email isn’t set up"
            action={
              <Button size="sm" variant="secondary" render={<Link to="/admin/email" />}>
                Set up email
              </Button>
            }
          >
            An announcement is email, so there is nothing to send it with yet.
            Everything else on this page still works.
          </Notice>
        )}

        {query.isError ? (
          <QueryError
            error={query.error}
            title="The announcements could not be read"
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => query.refetch()}
          />
        ) : query.isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </div>
        ) : list.length === 0 ? (
          <EmptyState
            title="Nothing has been announced"
            description={
              query.data?.can_send
                ? "Write one to email everyone on this install, or only publishers."
                : "Announcements need email to be set up on Admin → Email."
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Subject</TableHead>
                <TableHead>Audience</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Reached</TableHead>
                <TableHead className="hidden @md:table-cell">Sent</TableHead>
                <TableHead className="w-12">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((a) => (
                <TableRow key={a.id} className="align-top">
                  <TableCell>
                    <Link
                      to={`/admin/messages/announcement/${a.id}`}
                      className="font-medium text-foreground hover:underline"
                    >
                      {a.subject}
                    </Link>
                    {a.cloned_from && (
                      <span className="ml-2 text-2xs text-muted-foreground">copy</span>
                    )}
                    {a.resend_of && (
                      <span className="ml-2 text-2xs text-muted-foreground">resend</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {a.resend_of ? "Who the original didn’t reach" : AUDIENCE_LABEL[a.audience]}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className={toneSurface[STATUS[a.status].tone]}>
                      {STATUS[a.status].label}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <AnnouncementCounts announcement={a} />
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @md:table-cell">
                    {a.sent_at ? (
                      <>
                        <RelativeTime iso={a.sent_at} />
                        {a.sent_by_login && <div className="text-2xs">by {a.sent_by_login}</div>}
                      </>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell>
                    {a.status !== "sending" && (
                      <DropdownMenu>
                        <DropdownMenuTrigger
                          render={
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              aria-label={`Actions for ${a.subject}`}
                            >
                              <MoreHorizontal className="h-4 w-4" />
                            </Button>
                          }
                        />
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem
                            onClick={() => navigate(`/admin/messages/announcement/${a.id}`)}
                          >
                            {a.status === "draft" ? "Edit" : "Open record"}
                          </DropdownMenuItem>
                          <DropdownMenuItem variant="destructive" onClick={() => setToDelete(a)}>
                            Delete…
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}

        {toDelete && (
          <DeleteAnnouncementDialog
            key={toDelete.id}
            announcement={toDelete}
            open
            onOpenChange={(o) => { if (!o) setToDelete(null); }}
            onDeleted={() => {
              setToDelete(null);
              queryClient.invalidateQueries({ queryKey: ["announcement-emails"] });
            }}
          />
        )}
      </div>
    </Section>
  );
}

/**
 * The counts, with the bad news first and the zeroes absent.
 *
 * A row saying "0 failed, 0 skipped" trains people to stop reading the column
 * that is the only reason this table exists.
 */
export function AnnouncementCounts({ announcement: a }: { announcement: AnnouncementEmail }) {
  if (a.status === "draft") {
    return <span className="text-xs text-muted-foreground">Not sent</span>;
  }
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
      {a.failed > 0 && (
        <span className={`rounded px-1.5 py-0.5 ${toneSurface.danger}`}>{a.failed} failed</span>
      )}
      {a.abandoned > 0 && (
        <span className={`rounded px-1.5 py-0.5 ${toneSurface.danger}`}>
          {a.abandoned} abandoned
        </span>
      )}
      {a.skipped > 0 && (
        <span className={`rounded px-1.5 py-0.5 ${toneSurface.neutral}`}>{a.skipped} skipped</span>
      )}
      <span className="text-muted-foreground">
        {a.delivered} of {a.resolved} delivered
      </span>
    </span>
  );
}

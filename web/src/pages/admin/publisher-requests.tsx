import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Inbox } from "lucide-react";
import { api, type PromotionRequest, type PromotionRequestStatus } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { FilterChips } from "@/components/filter-bar";
import { RelativeTime } from "@/components/relative-time";
import { StatusBadge } from "@/components/status-badge";
import { TableCard } from "@/components/table-card";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";

type Filter = PromotionRequestStatus | "all";

// "All" first, as on every admin list; Pending stays the default.
const filters: { label: string; value: Filter }[] = [
  { label: "All", value: "all" },
  { label: "Pending", value: "pending" },
  { label: "Approved", value: "approved" },
  { label: "Declined", value: "denied" },
  { label: "Withdrawn", value: "withdrawn" },
];

const PAGE_DESCRIPTION = "Approving a request lets that viewer create and deploy apps.";

/**
 * The queue of people asking for publisher access.
 *
 * Pending is the working view; the decided statuses are history, and history is
 * what makes "has anyone already turned this person down, and why" answerable
 * without reading the audit log.
 */
export function AdminPublisherRequestsPage() {
  const queryClient = useQueryClient();
  const [params, setParams] = useSearchParams();
  // In the URL, so "declined requests" is a link and Back keeps it.
  const [statusParam, setStatusParam] = useUrlParam("status", "pending");
  const status = statusParam as Filter;
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [denying, setDenying] = useState<PromotionRequest | null>(null);

  // The link in the notification email carries the request it is about. Landing
  // on a row that is no longer pending is normal — someone else decided it — so
  // the highlight follows the row into whichever filter holds it.
  const highlighted = params.get("request");

  const { data: requests = [], isLoading, error, refetch } = useQuery({
    queryKey: ["promotion-requests", status],
    queryFn: () => api.adminListPromotionRequests(status),
    retry: false,
  });

  // A deep link that lands on the pending queue but names a decided request
  // would otherwise show an empty table; widen the filter once, for that case.
  const widened = useRef(false);
  useEffect(() => {
    if (!highlighted || widened.current || isLoading || status !== "pending") return;
    if (!requests.some((r) => r.id === highlighted)) {
      widened.current = true;
      setStatusParam("all");
    }
  }, [highlighted, requests, isLoading, status, setStatusParam]);

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["promotion-requests"] });
    // An approval changes a role, which changes the user directory.
    queryClient.invalidateQueries({ queryKey: ["users"] });
  }

  async function approve(req: PromotionRequest) {
    setPendingId(req.id);
    try {
      await api.adminApprovePromotionRequest(req.id);
      refresh();
      toast.success(`“${req.requester_login}” is now a publisher`, {
        description: "They need to sign in again.",
      });
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : `Couldn’t approve “${req.requester_login}”’s request`,
      );
    } finally {
      setPendingId(null);
    }
  }

  if (error) {
    return (
      <>
        <PageHeader title="Publisher requests" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The requests could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Publisher requests"
        description={PAGE_DESCRIPTION}
      />

      <div className="space-y-4">
        <div className="flex flex-wrap items-center gap-4">
          {/* Chips rather than a counted tab strip, and deliberately: the
              server filters this list, so a count per tab would be four
              requests for a queue whose working view is Pending. The chips
              say what they select and nothing they cannot know. */}
          <FilterChips
            label="Filter requests by status"
            options={filters}
            value={status}
            onChange={(v) =>
              // One write for both: the highlight belongs to the email that
              // brought the admin here, not to whatever they browse afterwards,
              // and two writes in one handler would let the second undo the
              // first.
              setParams(
                (prev) => {
                  const out = new URLSearchParams(prev);
                  if (v === "pending") out.delete("status");
                  else out.set("status", v);
                  out.delete("request");
                  return out;
                },
                { replace: true },
              )
            }
          />
        </div>

        {isLoading ? (
          <TableCard className="space-y-3 p-4">
            {Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className="flex items-center gap-3">
                <Skeleton className="h-4 w-40" />
                <Skeleton className="h-4 w-56" />
                <Skeleton className="ml-auto h-4 w-24" />
              </div>
            ))}
          </TableCard>
        ) : requests.length === 0 ? (
          <EmptyState
            icon={<Inbox className="h-6 w-6" />}
            title={
              status === "pending"
                ? "Nothing waiting"
                : status === "all"
                  ? "No requests yet"
                  : `No ${(filters.find((f) => f.value === status)?.label ?? status).toLowerCase()} requests`
            }
            description={
              status === "pending" || status === "all"
                ? "Requests arrive when a viewer tries to create an app."
                : "Choose All to see every request."
            }
            action={
              status !== "pending" && status !== "all" ? (
                <Button variant="outline" size="sm" onClick={() => setStatusParam("all")}>
                  Show all
                </Button>
              ) : undefined
            }
          />
        ) : (
          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {/* Who asked, where it stands, and the pair of buttons that
                      answers it. Everything else is the record of a decision
                      already taken and yields first. */}
                  <TableHead className="w-full">Requester</TableHead>
                  <TableHead className="hidden @2xl:table-cell">Email</TableHead>
                  <TableHead className="w-[110px]">Status</TableHead>
                  <TableHead className="hidden w-[130px] @md:table-cell">Asked</TableHead>
                  <TableHead className="hidden w-[170px] @3xl:table-cell">Decided</TableHead>
                  <TableHead className="w-[180px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {requests.map((req) => (
                  <TableRow
                    key={req.id}
                    className={req.id === highlighted ? "bg-primary/5" : undefined}
                  >
                    <TableCell className="max-w-0">
                      {/* The account, openable. This queue asks an
                          administrator to widen somebody's rights across the
                          whole install, and the record that would inform the
                          decision — what they own, what they have been given,
                          when they last signed in — was a click away and
                          named nowhere on the row. */}
                      <Link
                        to={`/people/${req.requester_id}`}
                        className="block truncate font-medium text-primary hover:underline"
                      >
                        {req.requester_name || req.requester_login}
                      </Link>
                      {req.requester_name && (
                        <div className="truncate text-xs text-muted-foreground">
                          {req.requester_login}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="hidden max-w-0 truncate text-sm text-muted-foreground @2xl:table-cell">
                      {req.requester_email || "—"}
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={req.status} />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @md:table-cell">
                      <RelativeTime iso={req.created_at} />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                      {req.decided_at ? (
                        <>
                          <RelativeTime iso={req.decided_at} />
                          {req.approver_login && (
                            <span className="block text-xs">by {req.approver_login}</span>
                          )}
                          {req.reason && (
                            <span className="mt-0.5 block whitespace-pre-wrap text-xs italic">
                              {req.reason}
                            </span>
                          )}
                        </>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                    <TableCell className="text-right">
                      {req.status === "pending" && (
                        <div className="flex justify-end gap-2">
                          <Button
                            size="sm"
                            disabled={pendingId === req.id}
                            onClick={() => approve(req)}
                          >
                            {pendingId === req.id ? "Approving…" : "Approve"}
                          </Button>
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={pendingId === req.id}
                            onClick={() => setDenying(req)}
                          >
                            Decline
                          </Button>
                        </div>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableCard>
        )}
      </div>

      <DenyRequestDialog
        key={denying?.id ?? "none"}
        request={denying}
        onOpenChange={(open) => !open && setDenying(null)}
        onDenied={() => {
          setDenying(null);
          refresh();
        }}
      />
    </>
  );
}

/**
 * Denial with a reason. The reason is optional to type and always sent — it
 * reaches the requester by email, and a refusal they cannot act on is the one
 * they route around by asking in chat instead.
 */
function DenyRequestDialog({
  request,
  onOpenChange,
  onDenied,
}: {
  request: PromotionRequest | null;
  onOpenChange: (open: boolean) => void;
  onDenied: () => void;
}) {
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);

  async function submit() {
    if (!request) return;
    setSaving(true);
    try {
      await api.adminDenyPromotionRequest(request.id, reason.trim());
      toast.success(`Declined ${request.requester_login}’s request`);
      onDenied();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t decline “${request.requester_login}”’s request`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={request !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Decline publisher access</DialogTitle>
          <DialogDescription>
            {request?.requester_login} keeps their current access and can ask again. They’ll see
            your reason.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2">
          <Label htmlFor="deny-reason">Reason (optional)</Label>
          <Textarea
            id="deny-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            maxLength={600}
            rows={3}
            placeholder="e.g. Complete the deployment training first, then ask again."
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={saving}>
            Cancel
          </Button>
          {/* Not red: declining is recoverable — they can ask again. */}
          <Button onClick={submit} disabled={saving}>
            {saving ? "Declining…" : "Decline request"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

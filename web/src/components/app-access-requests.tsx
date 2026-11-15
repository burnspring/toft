import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type AccessRequest } from "@/lib/api";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { grantRoleLabel } from "@/lib/utils";

/**
 * The decider's half of the access-request workflow: who has asked for this
 * app, in their own words, and the two buttons that answer them.
 *
 * It renders only for somebody who may actually decide — the read behind it is
 * `ActionApproveAccessRequest` — and only when there is something waiting, so
 * an app nobody has asked for gains nothing on the page. Decided requests are
 * deliberately not listed here: the history belongs to the audit log, and a
 * queue that never empties is a queue nobody reads.
 *
 * Approve and deny are direct-with-a-toast rather than dialogs. Both are one
 * named row, both are audited, and both are recoverable — the grant can be
 * removed and the person can ask again.
 */
export function AppAccessRequests({ appId }: { appId: string }) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  /** Which act `busy` is, so each button names its own pending state. */
  const [approving, setApproving] = useState(false);
  const [denying, setDenying] = useState<string | null>(null);
  const [reason, setReason] = useState("");

  const { data = [], isLoading, error, refetch } = useQuery({
    queryKey: ["access-requests", appId],
    queryFn: () => api.getAccessRequests(appId),
  });
  const pending = data.filter((r) => r.status === "pending");

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["access-requests", appId] });
    // `["app-grants", appId]` — the key the Access tab's own list actually
    // uses. `["grants", …]` matches nothing, so approving a request added a
    // grant the table two inches below went on not showing until a reload,
    // and the approver had no way to tell it from a refusal.
    queryClient.invalidateQueries({ queryKey: ["app-grants", appId] });
    queryClient.invalidateQueries({ queryKey: ["app-permissions", appId] });
  }

  async function approve(req: AccessRequest) {
    const who = req.requester_name || req.requester_login || "They";
    setBusy(req.id);
    setApproving(true);
    try {
      await api.approveAccessRequest(appId, req.id);
      refresh();
      toast.success(
        req.requested_role === "viewer" || !req.requested_role
          ? `${who} can now open this app`
          : `${who} now has ${grantRoleLabel(req.requested_role)} access`,
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t approve ${who}’s request`);
    } finally {
      setBusy(null);
      setApproving(false);
    }
  }

  async function deny(req: AccessRequest) {
    setBusy(req.id);
    try {
      await api.denyAccessRequest(appId, req.id, reason.trim());
      refresh();
      setDenying(null);
      setReason("");
      toast.success("Request declined");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t decline the request");
    } finally {
      setBusy(null);
    }
  }

  // Nothing waiting is not a state worth a card. The section disappears rather
  // than standing there saying so — the roster below it is what this tab is
  // for.
  if (!isLoading && !error && pending.length === 0) return null;

  return (
    <section id="access-requests" className="scroll-mt-20 space-y-3">
      <SectionHeading
        title="Waiting for you"
        description="People who can see this app and have asked to open it."
      />

      {isLoading ? (
        <Skeleton className="h-20 w-full" />
      ) : error ? (
        <QueryError
          title="This app’s access requests could not be read"
          error={error}
          fallback="Reading who has asked for an app needs the right to decide those requests."
          onRetry={() => void refetch()}
        />
      ) : (
        <TableCard className="divide-y divide-border/60">
          {pending.map((req) => (
            <div key={req.id} className="space-y-2 px-3 py-2.5">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span className="text-sm font-medium text-foreground">
                  {req.requester_name || req.requester_login || req.requester_id}
                </span>
                <span className="text-xs text-muted-foreground">
                  asked for {grantRoleLabel(req.requested_role).toLowerCase()} ·{" "}
                  <RelativeTime iso={req.created_at} />
                </span>
                <div className="ml-auto flex items-center gap-1.5">
                  <Button
                    size="sm"
                    disabled={busy === req.id}
                    onClick={() => approve(req)}
                  >
                    {busy === req.id && approving ? "Approving…" : "Approve"}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy === req.id}
                    onClick={() =>
                      setDenying(denying === req.id ? null : req.id)
                    }
                  >
                    {denying === req.id ? "Cancel" : "Decline…"}
                  </Button>
                </div>
              </div>

              {req.reason && (
                <p className="whitespace-pre-wrap text-xs text-muted-foreground">
                  {req.reason}
                </p>
              )}

              {denying === req.id && (
                <div className="flex items-center gap-1.5">
                  <Input
                    autoFocus
                    value={reason}
                    maxLength={500}
                    onChange={(e) => setReason(e.target.value)}
                    placeholder="Why (optional) — this reaches them"
                    aria-label={`Why you are declining ${req.requester_login || req.requester_id}`}
                  />
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy === req.id}
                    onClick={() => deny(req)}
                  >
                    {busy === req.id && !approving ? "Declining…" : "Decline request"}
                  </Button>
                </div>
              )}
            </div>
          ))}
        </TableCard>
      )}
    </section>
  );
}

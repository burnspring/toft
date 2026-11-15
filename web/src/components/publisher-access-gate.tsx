import { useState } from "react";
import { Notice } from "@/components/notice";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ShieldAlert, Clock, Check } from "lucide-react";
import { api, type PromotionRequest } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { Skeleton } from "@/components/ui/skeleton";
import { RelativeTime } from "@/components/relative-time";

/**
 * The refusal a viewer meets when they try to create an app, with the way out
 * attached.
 *
 * Shown at the point of need rather than as a permanently visible "ask for more
 * access" link: the moment someone wants to publish is the only moment the
 * request means anything, and it is the moment they can say what they wanted it
 * for. The structured 403 the API returns is the same refusal one layer down —
 * this panel exists so nobody has to read it.
 */
export function PublisherAccessGate() {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);

  const { data: caps } = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.getCapabilities,
    retry: false,
  });
  const {
    data: requests, isLoading: requestsLoading, error: requestsError,
  } = useQuery({
    queryKey: ["my-promotion-requests"],
    queryFn: api.getMyPromotionRequests,
    retry: false,
  });

  const open = requests?.find((r) => r.status === "pending");
  // Only the newest decision is worth showing; older ones are history nobody
  // asked for on this page.
  const latestDecided = requests?.find((r) => r.status === "denied" || r.status === "approved");

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["my-promotion-requests"] });
    queryClient.invalidateQueries({ queryKey: ["capabilities"] });
  }

  async function requestAccess() {
    setPending(true);
    try {
      await api.createPromotionRequest();
      refresh();
      toast.success("Request sent to your administrators");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t send the request");
    } finally {
      setPending(false);
    }
  }

  async function withdraw(req: PromotionRequest) {
    setPending(true);
    try {
      await api.withdrawPromotionRequest(req.id);
      refresh();
      toast.success("Request withdrawn");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t withdraw the request");
    } finally {
      setPending(false);
    }
  }

  // Not answered yet is neither "nothing outstanding" nor "something is".
  // `requests` is `undefined` for the whole round trip, so `open` was falsy
  // and this panel offered **Request publisher access** to somebody whose
  // request is already in the queue — a second request, and a second row for
  // an administrator to decide.
  if (requestsLoading) {
    return (
      <Card>
        <CardContent className="p-6">
          <Skeleton className="mx-auto h-24 w-full max-w-md" />
        </CardContent>
      </Card>
    );
  }

  // And a read that failed is not "nothing outstanding" either. The button is
  // absent rather than offered-and-refused, with the server's own words above
  // it and a way to ask again.
  if (requestsError) {
    return (
      <Card>
        <CardContent className="p-6">
          <QueryError
            title="Your earlier requests could not be read"
            error={requestsError}
            fallback="Reload the page to try again."
            onRetry={() => refresh()}
          />
        </CardContent>
      </Card>
    );
  }

  if (open) {
    return (
      <Card>
        <CardContent className="p-6">
          <EmptyState
            icon={<Clock className="h-6 w-6" />}
            title="Your request is with your administrators"
            // Deliberately promises no email: whether this install sends any is
            // an administrator's setting, and a request that is answered in the
            // UI either way should not claim a channel that may be switched off.
            description="An administrator will approve or decline it. You’ll see the decision here, and by email if this install sends it."
            action={
              <div className="flex flex-col items-center gap-3">
                <p className="text-xs text-muted-foreground">
                  Sent <RelativeTime iso={open.created_at} />
                </p>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={pending}
                  onClick={() => withdraw(open)}
                >
                  {pending ? "Withdrawing…" : "Withdraw request"}
                </Button>
              </div>
            }
          />
        </CardContent>
      </Card>
    );
  }

  // Approved, but this session was minted before the promotion — signing in
  // again is what picks it up, and saying so keeps it from reading as a bug.
  if (latestDecided?.status === "approved") {
    return (
      <Card>
        <CardContent className="p-6">
          <EmptyState
            icon={<Check className="h-6 w-6" />}
            title="Your publisher access was approved"
            description="Sign out and back in to start using it — this sign-in still carries your old role."
          />
        </CardContent>
      </Card>
    );
  }

  const canAsk = caps?.can_request_publisher_role ?? false;

  return (
    <Card>
      <CardContent className="p-6">
        <EmptyState
          icon={<ShieldAlert className="h-6 w-6" />}
          title="You need publisher access to create apps"
          description={
            canAsk
              ? "Your account can open the apps shared with you. Request publisher access and an administrator can approve it."
              : "Your account cannot create apps. Ask an administrator if you need to publish."
          }
          action={
            canAsk ? (
              <Button size="sm" disabled={pending} onClick={requestAccess}>
                {pending ? "Sending…" : "Request publisher access"}
              </Button>
            ) : undefined
          }
        />
        {latestDecided?.status === "denied" && (
          <Notice
            tone="neutral"
            className="mx-auto mt-2 max-w-md"
            title={
              <>
                An earlier request was declined
                {latestDecided.approver_login ? ` by ${latestDecided.approver_login}` : ""}{" "}
                <RelativeTime iso={latestDecided.decided_at ?? latestDecided.created_at} />
              </>
            }
          >
            {latestDecided.reason && (
              <p className="whitespace-pre-wrap">{latestDecided.reason}</p>
            )}
          </Notice>
        )}
      </CardContent>
    </Card>
  );
}

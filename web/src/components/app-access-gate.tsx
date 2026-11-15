import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Lock, Clock } from "lucide-react";

import { api, type AccessRequest } from "@/lib/api";
import { AppIcon } from "@/components/app-icon";
import { AppOwner } from "@/components/app-owner";
import { FrameworkBadge } from "@/components/framework-badge";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { useDocumentTitle } from "@/hooks/use-document-title";

/**
 * What somebody holding the `list` rung sees when they open an app: the row
 * they were shown, and the way to ask for the rest.
 *
 * The twin of PublisherAccessGate one axis over, and it inherits that
 * component's rule verbatim — *shown at the point of need rather than as a
 * permanently visible "ask for more access" link: the moment someone wants to
 * open an app is the only moment the request means anything, and it is the
 * moment they can say what they wanted it for.*
 *
 * **It is built from the listing's payload, never from `GET /apps/{id}`** —
 * which would 403, and rightly. What a `list` grant discloses is the row in
 * somebody's app list and not one field more, so this page shows the name, the
 * owner, the slug and the framework, and no status, no URL, no events and no
 * deploy history. (The PRD wrote "description" into that list; apps do not have
 * one, so there is nothing to withhold.)
 */
export function AppAccessGate({ appId }: { appId: string }) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");

  // The listing, which is the only thing this caller may read about the app.
  // Shared with the apps page through the query cache, so arriving here from
  // the list costs no request at all.
  const { data: apps = [] } = useQuery({
    queryKey: ["apps"],
    queryFn: () => api.listApps(),
  });
  const row = apps.find((a) => a.id === appId);

  const { data: perms } = useQuery({
    queryKey: ["app-permissions", appId],
    queryFn: () => api.getAppPermissions(appId),
  });
  const { data: mine = [] } = useQuery({
    queryKey: ["my-access-requests"],
    queryFn: api.getMyAccessRequests,
    retry: false,
  });

  // The open request comes from the permissions payload rather than from the
  // list, so the two arrive together and the gate does not flash between "you
  // may ask" and "you already did".
  const open = perms?.pending_access_request ?? null;
  // Only the newest decision is worth showing; older ones are history nobody
  // asked for on this page.
  const latestDecided = mine.find(
    (r) => r.object_id === appId && (r.status === "denied" || r.status === "approved"),
  );

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["app-permissions", appId] });
    queryClient.invalidateQueries({ queryKey: ["my-access-requests"] });
    queryClient.invalidateQueries({ queryKey: ["app", appId] });
  }

  async function ask() {
    setBusy(true);
    try {
      await api.createAccessRequest(appId, "viewer", note.trim() || undefined);
      refresh();
      setNote("");
      toast.success("Request sent to the app’s owner");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t send the request");
    } finally {
      setBusy(false);
    }
  }

  async function withdraw(req: AccessRequest) {
    setBusy(true);
    try {
      await api.withdrawAccessRequest(appId, req.id);
      refresh();
      toast.success("Request withdrawn");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t withdraw the request");
    } finally {
      setBusy(false);
    }
  }

  const name = row?.name ?? "This app";

  // A page that draws its own `<h1>` instead of `PageHeader` still owes the
  // browser tab, the history entry and the route announcer a name. Without it
  // the announcement on arriving here was "Toft" — the document title
  // whatever route you came from — so the one screen whose whole job is
  // telling somebody where they have landed said nothing about it.
  useDocumentTitle(row?.name);

  return (
    <Card className="mx-auto max-w-xl">
      <CardContent className="space-y-5 p-6">
        <div className="flex items-start gap-3">
          <AppIcon name={name} seed={row?.slug ?? appId} size="lg" />
          <div className="min-w-0 space-y-1">
            <h1 className="truncate text-2xl font-semibold tracking-tight">{name}</h1>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
              {row?.slug && <span className="font-mono">{row.slug}</span>}
              {row && (
                <AppOwner
                  ownerId={row.owner_id}
                  ownerGroupId={row.owner_group_id}
                  ownerLogin={row.owner?.login}
                  className="text-xs"
                />
              )}
              {row?.framework && <FrameworkBadge framework={row.framework} />}
            </div>
          </div>
        </div>

        {open ? (
          <Notice
            tone="info"
            icon={<Clock className="h-4 w-4" aria-hidden />}
            title={
              <>
                Your request is with the owner — sent{" "}
                <RelativeTime iso={open.created_at} />
              </>
            }
            action={
              <Button variant="outline" size="sm" disabled={busy} onClick={() => withdraw(open)}>
                {busy ? "Withdrawing…" : "Withdraw"}
              </Button>
            }
          >
            {open.reason && <p className="whitespace-pre-wrap">{open.reason}</p>}
          </Notice>
        ) : (
          <Notice
            tone="neutral"
            icon={<Lock className="h-4 w-4" aria-hidden />}
            title="You don’t have access to this app yet"
          >
            {/* Nothing until the permissions land: pointing at a form that
                may not render is a dead end. */}
            {perms?.can_request_access ? (
              <p>Ask for access below. The owner’s answer shows up here.</p>
            ) : perms && row ? (
              <p>
                Ask the owner,{" "}
                <AppOwner
                  ownerId={row.owner_id}
                  ownerGroupId={row.owner_group_id}
                  ownerLogin={row.owner?.login}
                />
                , to share it with you.
              </p>
            ) : null}
          </Notice>
        )}

        {!open && perms?.can_request_access && (
          <div className="space-y-2">
            <Label htmlFor="access-note">Why you need it (optional)</Label>
            <Textarea
              id="access-note"
              rows={3}
              maxLength={500}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder="What you’re working on, and what you need from this app."
            />
            <div className="flex justify-end">
              <Button size="sm" disabled={busy} onClick={ask}>
                {busy ? "Sending…" : "Request access"}
              </Button>
            </div>
          </div>
        )}

        {!open && latestDecided?.status === "denied" && (
          <Notice
            tone="neutral"
            title={
              <>
                An earlier request was declined
                {latestDecided.approver_login ? ` by ${latestDecided.approver_login}` : ""}{" "}
                <RelativeTime iso={latestDecided.decided_at ?? latestDecided.created_at} />
              </>
            }
          >
            {latestDecided.reason && <p className="whitespace-pre-wrap">{latestDecided.reason}</p>}
          </Notice>
        )}
      </CardContent>
    </Card>
  );
}

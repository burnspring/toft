import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { AccountFacts } from "@/components/account-facts";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { Section } from "@/components/section";
import { EmptyState } from "@/components/empty-state";
import { MyTimeZone } from "@/components/my-time-zone";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import type { MyAccount, PromotionRequest, UserAccessReport } from "@/lib/api";

/**
 * Who you are, what you may do, and what the platform is going to do about it.
 */
export function MeOverviewTab({
  account,
  access,
}: {
  account: MyAccount;
  access: UserAccessReport | undefined;
}) {
  const user = account.user;
  // The display zone rides on /auth/me, which the session already read.
  const { user: me } = useAuth();

  // Personally owned, and owned through a group: two numbers because they mean
  // different things. Derived from the access report rather than counted
  // separately, so this page and the administrator's agree by construction.
  const ownedApps = (access?.explicit ?? []).filter((e) =>
    e.sources.some((s) => s.kind === "owner")
  ).length;
  const ownedViaGroup = (access?.explicit ?? []).filter((e) =>
    e.sources.some((s) => s.kind === "owner_via_group")
  ).length;

  return (
    <div className="space-y-6">
      <AccountFacts
        user={user}
        groupCount={account.groups.length}
        ownedApps={ownedApps}
        ownedViaGroup={ownedViaGroup}
        footer={<WhatYouCanDo role={user.system_role} />}
      />

      {/* Present only when the server says so: absent while the sweep is off,
          and absent for an administrator, whom it never locks. The page does
          not decide either of those — see MeDormancy. */}
      {account.dormancy && (
        <Notice
          tone="attention"
          size="md"
          title={`Accounts unused for ${account.dormancy.threshold_days} days are locked`}
        >
          At your current level of activity, yours would be locked{" "}
          <RelativeTime iso={account.dormancy.locks_on} />. Signing in or using an API key resets
          this.
        </Notice>
      )}

      {me?.time_zone && <MyTimeZone tz={me.time_zone} />}

      <Section
        title="Groups"
        description="Set by an administrator. When your groups change, you’re signed out everywhere."
      >
        {account.groups.length === 0 ? (
          <EmptyState size="inline" title="You aren’t a member of any group" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Group</TableHead>
                <TableHead className="w-[180px]">Added</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {account.groups.map((g) => (
                <TableRow key={g.id}>
                  <TableCell className="font-medium text-foreground">{g.name}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {g.added_at ? <RelativeTime iso={g.added_at} /> : "Unknown"}
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

const ROLE_SENTENCE: Record<string, string> = {
  system_admin:
    "You administer this install: every app, account and setting.",
  publisher: "You can create apps and deploy them, and share the ones you own.",
  viewer:
    "You can open the apps shared with you.",
};

/**
 * The role in a sentence somebody can act on, and the way to ask for more.
 *
 * `/me/capabilities` has been returning `can_request_publisher_role` since the
 * promotion workflow shipped, and the only surface that read it was a gate on
 * the New App button — so a viewer who never pressed that button was never told
 * there was a role above theirs or a way to ask for it.
 *
 * This is the second door to the same room, not a replacement:
 * `PublisherAccessGate` keeps its place at the point of need. The two read the
 * *same* two queries by key, so they cannot end up disagreeing about whether a
 * request is outstanding — which is the only way two surfaces onto one workflow
 * go wrong.
 */
function WhatYouCanDo({ role }: { role?: string }) {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);

  const { data: caps } = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.getCapabilities,
    retry: false,
  });
  const { data: requests } = useQuery({
    queryKey: ["my-promotion-requests"],
    queryFn: api.getMyPromotionRequests,
    retry: false,
  });

  const open = requests?.find((r) => r.status === "pending");
  const latestDecided = requests?.find(
    (r) => r.status === "denied" || r.status === "approved"
  );

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["my-promotion-requests"] });
    queryClient.invalidateQueries({ queryKey: ["capabilities"] });
  }

  async function request() {
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

  const canAsk = caps?.can_request_publisher_role ?? false;

  /**
   * An approval this sign-in has not caught up with.
   *
   * The role on the session is the test, not the approval: the line below asks
   * somebody to sign out and back in, and it went on asking after they had —
   * for ever, to anybody whose request was ever approved, including the people
   * already holding the role it was telling them to go and collect.
   */
  const approvedButNotYetInEffect =
    latestDecided?.status === "approved" && role !== "publisher" && role !== "system_admin";

  // One rendering of the role, whether or not there is anything to do about
  // it: the same fact used to move into a titled card, and change weight, the
  // moment a button appeared beside it. The controls go under the line.
  return (
    <div className="space-y-3">
      <p className="max-w-prose text-sm text-muted-foreground">
        {(role && ROLE_SENTENCE[role]) ?? ROLE_SENTENCE.viewer}
      </p>

        {open ? (
          <div className="flex flex-wrap items-center gap-3">
            <p className="text-xs text-muted-foreground">
              You asked for publisher access <RelativeTime iso={open.created_at} />.
            </p>
            <Button variant="outline" size="sm" disabled={pending} onClick={() => withdraw(open)}>
              {pending ? "Withdrawing…" : "Withdraw request"}
            </Button>
          </div>
        ) : approvedButNotYetInEffect ? (
          <p className="text-xs text-muted-foreground">
            Your publisher access was approved. Sign out and back in to use it.
          </p>
        ) : canAsk ? (
          <div className="flex flex-wrap items-center gap-3">
            <Button size="sm" disabled={pending} onClick={request}>
              {pending ? "Sending…" : "Request publisher access"}
            </Button>
            {latestDecided?.status === "denied" && (
              <p className="text-xs text-muted-foreground">
                An earlier request was declined
                {latestDecided.approver_login ? ` by ${latestDecided.approver_login}` : ""}{" "}
                <RelativeTime iso={latestDecided.decided_at ?? latestDecided.created_at} />.
                {/* The administrator's own words, where they left any. A
                    decision reported without its reason sends somebody back to
                    ask the question that was already answered. */}
                {latestDecided.reason && <> They said: “{latestDecided.reason}”</>}
              </p>
            )}
          </div>
        ) : null}
    </div>
  );
}

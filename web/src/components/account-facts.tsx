import { RelativeTime } from "@/components/relative-time";
import { Fact } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { systemRoleLabel } from "@/lib/utils";
import type { User } from "@/lib/api";

/**
 * The six facts that make an account legible, as one strip. Not the login:
 * both pages print it in the header, as the title or under it.
 *
 * Extracted out of `user-detail.tsx` when `/me` grew a record of its own, so
 * that the administrator's view of somebody and that person's view of
 * themselves cannot start formatting the same date two ways.
 *
 * The pair worth protecting is **Last sign-in** and **Last used**. Signed in a
 * year ago, used yesterday, is a live automation — and either fact alone tells
 * the opposite story. "Last used" is also the figure the dormancy sweep reads,
 * which is why the two are always shown together and never collapsed into one.
 */
export function AccountFacts({
  user,
  groupCount,
  ownedApps,
  ownedViaGroup,
  footer,
}: {
  user: User;
  groupCount: number;
  /** Owned personally. This is the count that blocks deleting an account. */
  ownedApps: number;
  /** Owned through a group — the apps that stay behind when the person goes. */
  ownedViaGroup: number;
  /** A line about these facts — `/me`'s role sentence — inside the card it
   *  describes rather than floating between this card and the next. */
  footer?: React.ReactNode;
}) {
  return (
    <TableCard className="p-4">
      {/* Six, always — so the widest tier is three and the strip reads as
          two full rows. */}
      <dl className="grid grid-cols-2 gap-4 sm:grid-cols-3">
        <Fact label="Role">{user.system_role ? systemRoleLabel(user.system_role) : "—"}</Fact>
        <Fact label="Joined"><RelativeTime iso={user.created_at} /></Fact>
        <Fact label="Last sign-in">
          {user.last_login_at ? (
            <RelativeTime iso={user.last_login_at} />
          ) : (
            <span className="text-muted-foreground">Never</span>
          )}
        </Fact>
        <Fact label="Last used">
          {user.last_active_at ? (
            <>
              <RelativeTime iso={user.last_active_at} />
              {user.last_key_use_at && !user.last_login_at && (
                <span className="block text-2xs text-muted-foreground">via API key</span>
              )}
            </>
          ) : (
            // "Never", like the fact beside it. An em dash means "no value" —
            // and this one has a value: it is that nothing has ever happened,
            // which is exactly the finding somebody reads this strip for.
            <span className="text-muted-foreground">Never</span>
          )}
        </Fact>
        {/* Two numbers, because they mean different things and only the first
            one blocks deleting the account. */}
        <Fact label="Apps owned">
          {ownedApps}
          {ownedViaGroup > 0 && (
            <span className="ml-1.5 text-xs font-normal text-muted-foreground">
              + {ownedViaGroup} via {ownedViaGroup === 1 ? "a group" : "groups"}
            </span>
          )}
        </Fact>
        <Fact label="Groups">{groupCount}</Fact>
      </dl>
      {footer && <div className="mt-4 border-t border-border/60 pt-4">{footer}</div>}
    </TableCard>
  );
}

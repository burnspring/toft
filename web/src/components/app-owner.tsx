import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Users } from "lucide-react";

import { api } from "@/lib/api";
import { useAuth } from "@/hooks/use-auth";

/**
 * Who owns an app, for the one-line places that say so.
 *
 * An app is owned by a **principal** — a user or a group, never both and never
 * neither. This is the one component that knows how to render either, so that
 * the app page, the estate tables and anything added later cannot drift into
 * describing the same fact three ways.
 *
 * The rule it exists to keep: **a group is not a person with an unusual name.**
 * It gets its own icon and its member count, because "Finance Platform Team" in
 * an Owner column that otherwise holds logins reads as somebody's account until
 * you notice, and because how many people that is happens to be the most
 * load-bearing fact about it — six people can administer this app.
 *
 * `owner_group_id` empty means a person owns it, and then this renders exactly
 * what it always did. On an install with no groups nothing here ever fires.
 *
 * **The name is a link only for whoever can follow it.** Both destinations —
 * `/people/:id` and the group roster — are behind `AdminRoute`, which navigates
 * to the apps list with no toast and no `?redirect=`. So a publisher who
 * clicked the owner of an app was silently relocated, losing the page they were
 * on, on the four densest surfaces in the product and on one of the two tabs a
 * viewer has at all. ADR §53: a link that bounces is worse than no link. The
 * decision is made here rather than at the six render sites, for the reason
 * everything else about an owner is.
 */
export function AppOwner({
  ownerId,
  ownerGroupId,
  ownerLogin,
  className,
  /** Show the member count beside a group's name. Off in a dense table column
   *  where the row already has too much, on wherever there is room to say it. */
  withCount = true,
}: {
  ownerId?: string;
  ownerGroupId?: string;
  ownerLogin?: string;
  className?: string;
  withCount?: boolean;
}) {
  const { user } = useAuth();
  const isAdmin = user?.system_role === "system_admin";
  // One list read, shared by every consumer through the query cache, rather
  // than a lookup per row. It is also what makes this safe in a table: N owner
  // cells cost one request between them.
  const { data: groups = [] } = useQuery({
    queryKey: ["groups"],
    queryFn: api.listGroups,
    enabled: !!ownerGroupId,
    staleTime: 5 * 60_000,
  });

  if (ownerGroupId) {
    const group = groups.find((g) => g.id === ownerGroupId);
    const count = group?.member_count;
    const name = group?.name ?? "a group";
    return (
      <span className={`inline-flex min-w-0 items-center gap-1.5 ${className ?? ""}`}>
        <Users className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
        {isAdmin ? (
          <Link
            // The group itself, not the whole list: GroupsTab opens the
            // members dialog for `?group=`.
            to={`/people?tab=groups&group=${ownerGroupId}`}
            className="min-w-0 truncate rounded-sm hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            {name}
          </Link>
        ) : (
          <span className="min-w-0 truncate">{name}</span>
        )}
        {withCount && count !== undefined && (
          <span className="shrink-0 text-xs text-muted-foreground">
            ({count} {count === 1 ? "member" : "members"})
          </span>
        )}
      </span>
    );
  }

  if (!ownerId && !ownerLogin) {
    return <span className={`text-muted-foreground ${className ?? ""}`}>—</span>;
  }

  // **Never the id.** A caller that has the owner's id but not their login is a
  // caller whose payload did not carry the person — the access gate was one for
  // as long as it existed — and falling back to `ownerId` rendered a raw uuid
  // in an Owner column, on the one screen whose entire job is to tell a
  // stranger who to ask. "an owner" is the peer of the group branch's "a
  // group" above: less than we wanted to say, but a true sentence.
  const who = ownerLogin ?? "an owner";

  if (!isAdmin) {
    return <span className={`min-w-0 truncate ${className ?? ""}`}>{who}</span>;
  }

  return (
    <Link
      to={`/people/${ownerId}`}
      className={`min-w-0 truncate rounded-sm hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 ${className ?? ""}`}
    >
      {who}
    </Link>
  );
}

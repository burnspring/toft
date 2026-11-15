import { Link } from "react-router-dom";
import { Crown, UserCheck, Users } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { StatusBadge } from "@/components/status-badge";
import { toneSurface } from "@/lib/tone";
import { grantRoleLabel } from "@/lib/utils";
import type { AccessSource, UserAccessReport } from "@/lib/api";

/**
 * The access report, rendered.
 *
 * Extracted out of `user-detail.tsx` when `/me` grew a report of its own. Two
 * pages now render one computation, and the point of extracting rather than
 * copying is that they cannot drift: an administrator reviewing somebody and
 * that person reading their own standing should not be able to disagree about
 * what a source chip means or how a role is spelled.
 *
 * The server keeps the other half of that promise — both reads are backed by
 * one builder (`buildAccessReport`), so this component is a reader of a
 * computation and never a second copy of one. Nothing here re-derives access
 * from grants.
 */

export function SourceChip({ source }: { source: AccessSource }) {
  if (source.kind === "owner") {
    return (
      // "Owner", not "Direct": a direct grant's chip says Direct too, so the
      // word that told owning from being shared was the same word. Neutral,
      // because ownership is a fact; `info` is the platform acting, and the
      // crown carries the distinction.
      <Badge variant="outline" className={`text-xs ${toneSurface.neutral}`}>
        <Crown className="mr-1 h-3 w-3" /> Owner
      </Badge>
    );
  }
  // Owner *through* a group is its own kind, not a group grant that happens to
  // say owner. The distinction is the reportable one: a group grant is somebody
  // sharing an app with a team; this is the team being the owner, which an
  // access review that lumped them together would silently under-report.
  if (source.kind === "owner_via_group") {
    return (
      <Badge variant="outline" className={`text-xs ${toneSurface.neutral}`}>
        <Crown className="mr-1 h-3 w-3" /> Owner via {source.group_name || "a group"}
      </Badge>
    );
  }
  if (source.kind === "group") {
    return (
      <Badge variant="outline" className={`text-xs ${toneSurface.neutral}`}>
        <Users className="mr-1 h-3 w-3" /> {source.group_name || "group"} ·{" "}
        {grantRoleLabel(source.role)}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className={`text-xs ${toneSurface.neutral}`}>
      <UserCheck className="mr-1 h-3 w-3" /> Direct · {grantRoleLabel(source.role)}
    </Badge>
  );
}

/**
 * `voice` decides who the copy is about, and it is the only difference between
 * the two callers. An administrator reads about "them"; a person reads about
 * themselves. Getting this wrong is how a self-service page ends up telling
 * somebody what *they* can do in the third person.
 */
export type AccessVoice = "self" | "other";

export function AccessTable({
  report,
  voice,
  error,
  onRetry,
}: {
  report: UserAccessReport | undefined;
  voice: AccessVoice;
  /** The read's failure, where the caller has one. Without it an absent report
   *  is indistinguishable from a slow one, and this component sat on a skeleton
   *  for ever — the loading state as a permanent answer, which is the shape
   *  people describe as "the page never loads". */
  error?: unknown;
  onRetry?: () => void;
}) {
  if (error) {
    return (
      <QueryError
        title={voice === "self" ? "Your access could not be read" : "This account’s access could not be read"}
        error={error}
        fallback={
          voice === "self"
            ? "Reload the page to try again."
            : "Only administrators can read another account’s access."
        }
        onRetry={onRetry}
      />
    );
  }
  if (!report) {
    return (
      <div className="space-y-2 p-4">
        {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-8 w-full" />)}
      </div>
    );
  }

  const { authenticated_apps: authed, public_apps: pub } = report.visibility_access;

  return (
    <>
      {report.explicit.length === 0 ? (
        <EmptyState
          size="inline"
          title={voice === "self" ? "No apps owned by or shared with you" : "No apps owned by or shared with this account"}
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="w-full">App</TableHead>
              <TableHead className="hidden w-[110px] @md:table-cell">State</TableHead>
              <TableHead className="w-[130px]">Role</TableHead>
              <TableHead className="hidden @xl:table-cell">Source</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {report.explicit.map((e) => (
              <TableRow key={e.app.id}>
                {/* Name over slug, both truncating. Side by side on one
                    `whitespace-nowrap` line this column was 826px wide and the
                    table did not fit its card until 1,100. */}
                <TableCell className="max-w-0">
                  <Link
                    to={`/apps/${e.app.id}`}
                    className="block truncate font-medium text-foreground hover:text-primary hover:underline"
                  >
                    {e.app.name}
                  </Link>
                  <span className="block truncate font-mono text-xs text-muted-foreground">
                    {e.app.slug}
                  </span>
                  {/* Source, at the width its column goes. "Who gave me this"
                      is the question this report is read for, and below `@xl`
                      — which on `/me` beside its rail is most of a laptop —
                      the answer was rendered nowhere. */}
                  <span className="mt-0.5 flex flex-wrap gap-1 @xl:hidden">
                    {e.sources.map((s, i) => <SourceChip key={i} source={s} />)}
                  </span>
                </TableCell>
                <TableCell className="hidden @md:table-cell">
                  <StatusBadge status={e.app.status} />
                </TableCell>
                <TableCell className="text-sm">
                  {grantRoleLabel(e.effective_role)}
                </TableCell>
                <TableCell className="hidden @xl:table-cell">
                  <div className="flex flex-wrap gap-1">
                    {e.sources.map((s, i) => <SourceChip key={i} source={s} />)}
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {/* The sentence that makes the number above honest: the table is explicit
          access, and everybody also stands on a floor of apps shared with all
          signed-in users. Without this, a report showing two rows reads as
          "you can reach two apps", which is false on most installs. */}
      {/* A zero clause is dropped, not stated: "reach 0 apps visible to
          anyone signed in" is a sentence about nothing. */}
      {(authed > 0 || pub > 0) && (
        <p className="border-t border-border/60 px-4 py-3 text-xs text-muted-foreground">
          Like every signed-in user, {voice === "self" ? "you" : "they"} can also reach{" "}
          {[
            authed > 0 && `${authed} app${authed !== 1 ? "s" : ""} visible to anyone signed in`,
            pub > 0 && `${pub} public app${pub !== 1 ? "s" : ""}`,
          ]
            .filter(Boolean)
            .join(" and ")}
          .
        </p>
      )}
    </>
  );
}

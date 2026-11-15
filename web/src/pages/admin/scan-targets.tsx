import { Copy, LockKeyhole, Radar } from "lucide-react";
import { EffectiveAudience } from "@/components/effective-audience";
import { RelativeTime } from "@/components/relative-time";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { useQuery } from "@tanstack/react-query";
import { api, type EstateTarget, type EstateTargets } from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { PageHeader } from "@/components/page-header";
import { TableCard } from "@/components/table-card";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { StatusBadge } from "@/components/status-badge";
import { AppOwner } from "@/components/app-owner";
import { Notice } from "@/components/notice";
import { copyToClipboard } from "@/lib/clipboard";
import { toneSurface } from "@/lib/tone";
import { isBatchFramework } from "@/lib/frameworks";

/**
 * Admin → Estate → Scan targets: every app as an address.
 *
 * Not the Languages census beside it, and not the apps list: this page has one
 * reader, and it is whoever is pointing a scanner at this install. What that
 * person needs is a URL per app, whether it answers a signed-out caller, and
 * whether it is worth visiting at all — and what they need most is the whole
 * list out of here in one action, which is why the header's button is the
 * feature and the table is the evidence for it.
 *
 * **The coverage line leads**, as it does on the census, and for a sharper
 * version of the reason: a list of addresses reads as "these are the things
 * that exist", so the sentence saying how many apps have no address sits above
 * the table rather than being discoverable by noticing a blank column.
 *
 * The platform does not scan. It says what to point at, how not to be counted
 * as usage (`X-Toft-Scan`, on an app key) and how to be contacted
 * (`/.well-known/security.txt`, on Settings). Findings, schedules and verdicts
 * belong to whatever does the scanning.
 */
export function AdminScanTargetsPage() {
  const { data, isLoading, error, refetch } = useQuery<EstateTargets>({
    queryKey: ["estate-targets"],
    queryFn: () => api.getEstateTargets(),
  });

  const addressable = (data?.targets ?? []).filter((t) => t.url);

  return (
    <div>
      <PageHeader
        title="Scan targets"
        description={data && `Served from ${data.apps_origin}.`}
        actions={
          addressable.length > 0 ? (
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                copyToClipboard(addressable.map((t) => t.url).join("\n"), "address list")
              }
            >
              <Copy className="h-4 w-4" />
              Copy {addressable.length} address{addressable.length === 1 ? "" : "es"}
            </Button>
          ) : undefined
        }
      />

      {/* Error before empty: “nothing is deployed” is the wrong thing to tell
          somebody whose read simply failed. */}
      {error ? (
        <QueryError
          title="The scan targets could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : data && data.targets.length === 0 ? (
        <EmptyState
          icon={<Radar className="h-6 w-6" />}
          title="No apps yet"
          description="Each app appears here, with an address once it’s been deployed."
        />
      ) : data ? (
        <div className="space-y-4">
          {data.apps_addressable === 0 && (
            <Notice tone="neutral" size="sm" title="No app is serving pages">
              Nothing has an address, so there’s nothing to scan.
            </Notice>
          )}

          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {/* The address is what this page is for, so it is the
                      column given the room: everything beside it is bounded
                      and it takes what is left. */}
                  <TableHead className="w-[16rem]">App</TableHead>
                  {/* `w-full` is what makes this the column that *takes* the
                      leftover rather than the one that gives it up. Without it
                      the auto layout shares the slack out evenly and the
                      address — the only thing on this page anybody came for —
                      ends up narrower than the "Reachable by" beside it. */}
                  <TableHead className="w-full">Address</TableHead>
                  <TableHead className="hidden @xl:table-cell">Reachable by</TableHead>
                  <TableHead className="hidden @md:table-cell">Status</TableHead>
                  <TableHead className="hidden @3xl:table-cell">Owner</TableHead>
                  <TableHead className="hidden @4xl:table-cell">Last deployed</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.targets.map((t) => (
                  <TargetRow key={t.app_id} target={t} />
                ))}
              </TableBody>
            </Table>
            <p className="border-t border-border/60 px-4 py-3 text-xs text-muted-foreground">
              A scan run with an app key can send{" "}
              <code className="font-mono">X-Toft-Scan: 1</code> to stay out of that app’s usage
              figures.
            </p>
          </TableCard>
        </div>
      ) : null}
    </div>
  );
}

function TargetRow({ target }: { target: EstateTarget }) {
  return (
    <TableRow>
      {/* Name over slug, each truncating inside a bounded cell. Side by side
          on one `whitespace-nowrap` line they were 826px of this table's
          2,014 — in a card 998 wide — and the slug is the half a scanner
          operator matches against a hostname, so neither may be dropped. */}
      <TableCell className="max-w-[9rem] @xl:max-w-[16rem]">
        <div className="truncate font-medium text-foreground">{target.name}</div>
        <div className="truncate font-mono text-2xs text-muted-foreground">{target.slug}</div>
      </TableCell>

      {/* The column this page exists for. Never composed in the browser: the
          address is the install’s own answer, and it differs between a
          single-origin install and one that serves apps from their own host. */}
      {/* `min-w-0` does nothing to a table cell in auto layout — the column
          simply grew to the longest URL, all 708px of it. The bound plus
          `truncate` is what actually caps it, and the copy button beside it is
          why truncating is safe: nobody reads an address out of this column,
          they take it. */}
      <TableCell className="max-w-0">
        {target.url ? (
          <span className="flex min-w-0 items-center gap-1.5">
            <UrlText url={target.url} slug={target.slug} />
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Copy the address of ${target.name}`}
              onClick={() => copyToClipboard(target.url ?? "", "URL")}
            >
              <Copy className="h-3.5 w-3.5" />
            </Button>
          </span>
        ) : (
          <span className="whitespace-normal text-xs text-muted-foreground">
            {isBatchFramework(target.framework)
              ? "Batch app — serves no pages"
              : "Never deployed — nothing to scan"}
          </span>
        )}
      </TableCell>

      <TableCell className="hidden whitespace-nowrap @xl:table-cell">
        <EffectiveAudience visibility={target.visibility} withheld={target.visibility_withheld} />
      </TableCell>

      <TableCell className="hidden @md:table-cell">
        <span className="flex flex-wrap items-center gap-1.5">
          <StatusBadge status={target.status} />
          {target.locked && (
            <Badge variant="outline" className={`gap-1 text-2xs ${toneSurface.attention}`}>
              <LockKeyhole className="h-3 w-3" aria-hidden /> Locked
            </Badge>
          )}
        </span>
      </TableCell>

      <TableCell className="hidden @3xl:table-cell">
        <AppOwner
          ownerId={target.owner_id}
          ownerGroupId={target.owner_group_id}
          ownerLogin={target.owner_login ?? target.owner}
          withCount={false}
          className="text-sm text-muted-foreground"
        />
      </TableCell>

      <TableCell className="hidden whitespace-nowrap text-sm text-muted-foreground @4xl:table-cell">
        {target.last_deployed_at ? <RelativeTime iso={target.last_deployed_at} /> : "—"}
      </TableCell>
    </TableRow>
  );
}


/**
 * An address that truncates its origin, never its slug. The origin is the
 * same on every row and is stated once above the table; the slug is the only
 * part that differs, and plain `truncate` cut exactly that on a phone.
 */
function UrlText({ url, slug }: { url: string; slug: string }) {
  const at = url.lastIndexOf(slug);
  if (at <= 0) return <span className="truncate font-mono text-xs">{url}</span>;
  return (
    <span className="flex min-w-0 font-mono text-xs">
      <span className="min-w-[3ch] truncate">{url.slice(0, at)}</span>
      <span className="shrink-0">{url.slice(at)}</span>
    </span>
  );
}

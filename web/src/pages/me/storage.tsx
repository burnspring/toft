import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Disclosure } from "@/components/disclosure";
import { StorageUsage } from "@/components/storage-usage";
import { QueryError } from "@/components/query-error";
import { Section } from "@/components/section";
import { EmptyState } from "@/components/empty-state";
import { api } from "@/lib/api";
import { ACCESS_RUNGS, accessBadgeClass, accessLabel, grantSummary } from "@/lib/storage";
import { cn } from "@/lib/utils";

/**
 * The storage you reach — the other half of the Access tab beside it, and for
 * the same reason: what this platform has given you, read by the account it was
 * given to.
 *
 * It was a top-level nav item, which put a working destination in the header
 * for the minority of people who hold a grant and nothing there for everybody
 * else. The files themselves are still their own page — `/storage/:slug` is
 * full-width and is where uploading and deleting happen — and this is the
 * index of them.
 *
 * **A table, and the ladder said once.** Every row used to carry
 * `grantSummary(access)` under it, which is a function of the chip already in
 * the row: six read-only stores printed "Download and list. Nothing can be
 * uploaded." six times, and the fact that mattered — *which* store, at *which*
 * folder — was the small mono line above the repetition. The rungs actually
 * held are spelled out once, under the table.
 *
 * **No object counts.** Knowing one costs a full scan of the store, and a
 * number that is slow or wrong is worse than no number at all.
 */
export function MeStorageTab() {
  const navigate = useNavigate();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
  });

  const stores = data?.stores ?? [];
  // One row per *scope*: a store somebody was granted two folders of is two
  // entry points, at two levels, and one row would have to pick a level to
  // show and a folder to open.
  const href = (s: { slug: string; folder?: string }) =>
    `/storage/${s.slug}${s.folder ? `/${s.folder}/` : ""}`;

  // The rungs this person actually holds, in ladder order — the legend is what
  // is on the screen, never the whole ladder. Somebody with one read-only
  // grant should not have to read what Delete would have meant.
  const held = ACCESS_RUNGS.filter((rung) => stores.some((s) => s.access === rung));

  return (
    <Section
      // Not "Storage" alone: that is the tab above it, and a card restating its
      // own tab's name is a heading that says nothing. "You can reach" is the
      // content; "folders" was wrong, since most rows are a whole resource.
      title="Storage you can reach"
      description={
        <>
          Granted by an administrator. Open them here or with{" "}
          <code className="font-mono">toft store</code>.
        </>
      }
    >
      {error ? (
        <div className="p-4">
          {/* Broken before empty, always: a page that says "nobody has given
              you access" because the read failed is telling somebody something
              untrue about their own permissions. */}
          <QueryError
            title="Your storage could not be read"
            error={error}
            fallback="Reload the page. If it keeps failing, tell an administrator."
            onRetry={() => void refetch()}
          />
        </div>
      ) : isLoading ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-full" />
        </div>
      ) : stores.length === 0 ? (
        // Reachable only in the moment between the tab's own gate and this
        // query settling — the tab is absent for somebody who holds nothing.
        <EmptyState
          size="inline"
          title="No storage yet"
          description="Ask an administrator for access."
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">Name</TableHead>
                <TableHead className="w-[150px]">You can</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {stores.map((store) => (
                <TableRow
                  key={`${store.slug}/${store.folder ?? ""}`}
                  className="cursor-pointer"
                  onClick={() => navigate(href(store))}
                >
                  {/* The cell stops the row's click, because the disclosure
                      inside it is a button and opening it must not also leave
                      the page; the link is still the way to the store. */}
                  <TableCell
                    className="max-w-0 whitespace-normal"
                    onClick={(e) => e.stopPropagation()}
                  >
                    {/* A real link, not a row handler alone: the row's click is
                        a convenience and this is the route a keyboard takes.
                        `Link`, not an `<a>` that preventDefaults and then
                        navigates: that cancels ⌘-click and middle-click on an
                        address that is a real route. Matches admin/storage. */}
                    {/* The name *is* what the CLI and the SDKs take, so it is
                        the link, in the mono voice, with the folder written
                        the way `toft store ls` prints it — one column where a
                        label and a "Path" used to be two names for one thing. */}
                    <Link
                      to={href(store)}
                      onClick={(e) => e.stopPropagation()}
                      className="font-mono font-medium text-primary hover:underline"
                    >
                      {store.folder ? `${store.slug}/${store.folder}/` : store.slug}
                    </Link>
                    {store.folder && (
                      <div className="mt-0.5 text-2xs text-muted-foreground">
                        This folder and everything in it
                      </div>
                    )}
                    {store.description && (
                      <div className="mt-0.5 text-xs text-muted-foreground">
                        {store.description}
                      </div>
                    )}
                    {/* Under the row it is about, closed: a table of six stores
                        with six open panels is a page of code. */}
                    <Disclosure label="Commands and code" size="xs" className="mt-1">
                      <StorageUsage
                        name={store.slug}
                        kind={store.kind}
                        folder={store.folder}
                        access={store.access}
                        className="mt-2"
                      />
                    </Disclosure>
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant="outline"
                      className={cn("text-2xs", accessBadgeClass(store.access))}
                    >
                      {accessLabel(store.access)}
                    </Badge>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          <dl className="space-y-1 border-t border-border/60 px-4 py-3">
            {held.map((rung) => (
              <div key={rung} className="flex flex-wrap gap-x-2 text-xs">
                <dt className="font-medium text-foreground">{accessLabel(rung)}</dt>
                <dd className="text-muted-foreground">{grantSummary(rung)}</dd>
              </div>
            ))}
          </dl>
        </>
      )}
    </Section>
  );
}

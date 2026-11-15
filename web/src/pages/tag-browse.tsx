import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, TagIcon } from "lucide-react";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { buttonVariants } from "@/components/ui/button-variants";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { StatusBadge } from "@/components/status-badge";
import { AppIcon } from "@/components/app-icon";
import { RelativeTime } from "@/components/relative-time";
import { TableCard } from "@/components/table-card";
import { TagChips } from "@/components/tag-chip";
import { tagDotClass } from "@/lib/tags";
import { api } from "@/lib/api";
import type { AppWithMetrics, Tag, TagValue } from "@/lib/api";
import { cn } from "@/lib/utils";
import { QueryError } from "@/components/query-error";
import { AppOwner } from "@/components/app-owner";
import { AppOpenAction } from "@/components/app-open-action";

/** Table-shaped loading rows, so the skeleton matches what replaces it. */
function TableSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <TableCard className="space-y-3 p-4">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex items-center gap-4">
          <Skeleton className="h-9 w-9 rounded-lg" />
          <div className="flex-1 space-y-1.5">
            <Skeleton className="h-4 w-48" />
            <Skeleton className="h-3 w-32" />
          </div>
          <Skeleton className="h-5 w-16 rounded-full" />
          <Skeleton className="h-4 w-24" />
        </div>
      ))}
    </TableCard>
  );
}

/**
 * Browse by tag. Serves two routes:
 *   /tags/:tagSlug              — the values in use, with counts
 *   /tags/:tagSlug/:valueSlug   — the apps carrying that value
 *
 * Both read through the same visibility-scoped endpoints as the apps list, so
 * neither can surface an app the viewer couldn't otherwise see. A tag with no
 * readable apps is an empty state, not a 404 — the vocabulary is public.
 */
export function TagBrowsePage() {
  const { tagSlug, valueSlug } = useParams();

  const { data: tags = [], isLoading: loadingTags, error, refetch } = useQuery<Tag[]>({
    queryKey: ["tags"],
    queryFn: api.listTags,
  });
  const tag = tags.find((t) => t.slug === tagSlug);

  if (loadingTags) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-48" />
        <TableSkeleton />
      </div>
    );
  }

  // Ahead of the not-found branch below, which reads the *absence* of a tag in
  // a list that never arrived as the tag having been deleted — and tells
  // somebody their link is stale when it is fine.
  if (error) {
    return (
      <div>
        <PageHeader title="Tags" />
        <QueryError
          title="Tags could not be read"
          error={error}
          fallback="Reload the page. If it keeps failing, tell an administrator."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  if (!tag) {
    return (
      <div>
        <PageHeader title="Tags" />
        <EmptyState
          icon={<TagIcon className="h-6 w-6" />}
          title={`There’s no tag called “${tagSlug}”`}
          description="It may have been renamed or deleted."
        />
      </div>
    );
  }

  return valueSlug ? <AppsForValue tag={tag} valueSlug={valueSlug} /> : <ValuesForTag tag={tag} />;
}

function ValuesForTag({ tag }: { tag: Tag }) {
  const { data: values = [], isLoading, error, refetch } = useQuery<TagValue[]>({
    queryKey: ["tag-values", tag.slug, ""],
    queryFn: () => api.listTagValues(tag.slug),
  });

  return (
    <div>
      <PageHeader
        title={tag.name}
        description={
          tag.description ||
          (tag.kind === "system"
            ? "Assigned automatically by Toft."
            : "Values in use across apps you can see.")
        }
      />

      {isLoading ? (
        <TableSkeleton />
      ) : error ? (
        <QueryError
          title={`The values for ${tag.name} could not be read`}
          error={error}
          fallback="Reload the page. If it keeps failing, tell an administrator."
          onRetry={() => void refetch()}
        />
      ) : values.length === 0 ? (
        <EmptyState
          icon={<TagIcon className="h-6 w-6" />}
          title="No values yet"
          description={
            tag.kind === "system"
              ? "No app has been deployed with a detected framework yet."
              : `No app you can see has a value for ${tag.name}. App owners set it from an app’s Settings tab.`
          }
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Value</TableHead>
                <TableHead className="w-[100px]">Apps</TableHead>
                <TableHead className="w-[40px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {values.map((v) => (
                <TableRow key={v.slug}>
                  <TableCell>
                    <Link
                      to={`/tags/${tag.slug}/${v.slug}`}
                      className="inline-flex items-center gap-2 font-medium hover:underline"
                    >
                      <span className={cn("h-2.5 w-2.5 rounded-full", tagDotClass(tag.color))} />
                      {v.value}
                    </Link>
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{v.app_count}</TableCell>
                  <TableCell>
                    <ChevronRight className="h-4 w-4 text-muted-foreground" />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}
    </div>
  );
}

function AppsForValue({ tag, valueSlug }: { tag: Tag; valueSlug: string }) {
  const navigate = useNavigate();

  const { data: apps = [], isLoading, error, refetch } = useQuery<AppWithMetrics[]>({
    queryKey: ["apps", tag.slug, valueSlug],
    queryFn: () => api.listApps({ tag: tag.slug, value: valueSlug }),
  });

  // Prefer the value as actually stored over the slug from the URL.
  const label =
    apps.flatMap((a) => a.tags ?? []).find((t) => t.tag_slug === tag.slug && t.value_slug === valueSlug)
      ?.value ?? valueSlug;

  return (
    <div>
      <PageHeader
        title={`${tag.name}: ${label}`}
        description={
          isLoading
            ? undefined
            : `${apps.length} app${apps.length !== 1 ? "s" : ""} you can see`
        }
        actions={
          <Link
            to={`/tags/${tag.slug}`}
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            All {tag.name} values
          </Link>
        }
      />

      {isLoading ? (
        <TableSkeleton />
      ) : error ? (
        <QueryError
          title="These apps could not be read"
          error={error}
          fallback="Reload the page. If it keeps failing, tell an administrator."
          onRetry={() => void refetch()}
        />
      ) : apps.length === 0 ? (
        <EmptyState
          icon={<TagIcon className="h-6 w-6" />}
          title="No apps here"
          description={`No app you can see has ${tag.name} set to “${label}”.`}
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                {/* The apps list's plan, which this table is a narrowed view
                    of: Name takes the leftover and truncates, Status and the
                    row's Open stay, the owner and the date yield, tags last
                    because they are what the reader filtered by to get here.
                    No URL column — Open is the button, as on the apps list. */}
                <TableHead className="w-full">Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="hidden @xl:table-cell">Owner</TableHead>
                <TableHead className="hidden @3xl:table-cell">Tags</TableHead>
                <TableHead className="hidden w-[130px] @md:table-cell">Last deployed</TableHead>
                <TableHead className="w-[124px]">
                  <span className="sr-only">Open</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {apps.map((app) => (
                <TableRow
                  key={app.id}
                  className="cursor-pointer"
                  onClick={() => navigate(`/apps/${app.id}`)}
                >
                  <TableCell className="max-w-0">
                    <div className="flex min-w-0 items-center gap-3">
                      <AppIcon name={app.name} seed={app.slug} size="md" />
                      <div className="min-w-0">
                        {/* A real link keeps the row keyboard-reachable; the
                            row onClick is the mouse convenience. */}
                        <Link
                          to={`/apps/${app.id}`}
                          onClick={(e) => e.stopPropagation()}
                          className="block truncate font-medium text-foreground hover:text-primary hover:underline"
                        >
                          {app.name}
                        </Link>
                        <p className="truncate text-xs text-muted-foreground">{app.slug}</p>
                        {/* The owner, at the width its column goes — the
                            column's own tier, on the card's width. The slug
                            above already carries the address, so nothing here
                            is the only route to itself. */}
                        <span className="mt-0.5 block text-2xs text-muted-foreground @xl:hidden">
                          <AppOwner
                            ownerId={app.owner_id}
                            ownerGroupId={app.owner_group_id}
                            ownerLogin={app.owner?.login}
                            withCount={false}
                          />
                        </span>
                      </div>
                    </div>
                  </TableCell>
                  <TableCell>
                    {/* A row held at the `list` rung carries no status: the
                        rung shows that an app exists, not whether it is up. */}
                    {app.can_open ? (
                      <StatusBadge status={app.status} />
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  {/* One component renders a user or a group; the cell this
                      replaces drew an empty avatar and a blank name for every
                      group-owned app. */}
                  <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                    <AppOwner
                      ownerId={app.owner_id}
                      ownerGroupId={app.owner_group_id}
                      ownerLogin={app.owner?.login}
                      withCount={false}
                    />
                  </TableCell>
                  <TableCell className="hidden @3xl:table-cell">
                    <TagChips tags={app.tags} max={4} link={false} />
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @md:table-cell">
                    {/* The serving release's date, not `updated_at`: see the
                        apps list, which this table mirrors. */}
                    {app.last_deployed_at ? (
                      <RelativeTime iso={app.last_deployed_at} />
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end">
                      <AppOpenAction app={app} />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}
    </div>
  );
}

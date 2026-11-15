import { FilterChips } from "@/components/filter-bar";
import { StatCard } from "@/components/stat-card";
import { SearchInput } from "@/components/search-input";
import { useUrlParam } from "@/hooks/use-list-state";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Building2, Database, Globe, KeyRound, LockKeyhole, Radar } from "lucide-react";
import { EffectiveAudience } from "@/components/effective-audience";
import { Button } from "@/components/ui/button";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

import { api, type EstateReach, type EstateReachRow } from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { PageHeader } from "@/components/page-header";
import { TableCard } from "@/components/table-card";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { AppOwner } from "@/components/app-owner";
import { accessBadgeClass, accessLabel } from "@/lib/storage";
import { toneSurface } from "@/lib/tone";
import { cn } from "@/lib/utils";
import type { StorageAccess } from "@/lib/api";

/**
 * Admin → Governance → Reach: what each app holds, and which apps hold each
 * thing.
 *
 * A third estate reader beside the census and the scan targets. A row there is
 * a language and a count, or an app to visit; a row here is an app and what it
 * was given — and the two axes beside it are the same rows transposed, never a
 * second query, so "which apps hold the production database credential" cannot
 * disagree with the list it was derived from.
 *
 * **No verdict.** No score, no severity, no "this app is risky". A score is a
 * verdict with the argument deleted, it would be tuned by whoever complained
 * loudest, and the first app it rated low on the morning of an incident would
 * end this page's credibility permanently. The columns sort; the reader decides.
 */
export function AdminReachPage() {
  const { data, isLoading, error, refetch } = useQuery<EstateReach>({
    queryKey: ["estate-reach"],
    queryFn: () => api.getEstateReach(),
  });
  // In the URL, and cleared with a × or Escape. A raw `<Input>` in component
  // state made a narrowed estate report impossible to send to anybody — the
  // one table an administrator narrows *in order to* show somebody — and the
  // empty state below told them to clear a control that had nothing to clear
  // with. `SearchInput` is the house control and carries both.
  const [q, setQ] = useUrlParam("q");
  const [audience, setAudience] = useUrlParam("audience");
  // The tab too: "which apps hold credential X" is By credential, and a link
  // to it has to open there.
  const [view, setView] = useUrlParam("view", "apps");

  // The chips count the search-narrowed rows on every axis but their own, so
  // each figure is what clicking it would show.
  const searchedRows = data ? searched(data.rows, q) : [];
  const byAudience = countByAudience(searchedRows);
  const audienceChips = [
    { value: "all", label: "All", count: searchedRows.length },
    ...AUDIENCES.map((a) => ({ value: a.value, label: a.label, count: byAudience[a.value] })),
  ];

  return (
    <div>
      <PageHeader title="Reach" />

      {/* Error before empty: “nothing is deployed” is the wrong thing to tell
          somebody whose read simply failed. */}
      {error ? (
        <QueryError
          title="What apps can reach could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : data && data.rows.length === 0 ? (
        <EmptyState
          icon={<Radar className="h-6 w-6" />}
          title="No apps yet"
          description="Each app appears here with everything it’s been given."
        />
      ) : data ? (
        <div className="space-y-4">
          {/* Background, not a state: a muted line rather than a Notice. */}
          <p className="max-w-prose text-xs text-muted-foreground">{data.cannot_see}</p>

          <AudienceSummary rows={data.rows} />

          <Tabs value={view} onValueChange={(v) => setView(String(v))}>
            <TabsList variant="line">
              <TabsTrigger value="apps">By app</TabsTrigger>
              <TabsTrigger value="credentials">By credential</TabsTrigger>
              {data.show_storage && <TabsTrigger value="storage">By storage</TabsTrigger>}
            </TabsList>

            <TabsContent value="apps">
              <div className="space-y-3 pt-4">
                <SearchInput
                  value={q}
                  onChange={setQ}
                  placeholder="Search by app, owner, credential or storage…"
                  className="w-full sm:max-w-sm"
                />
                <FilterChips
                  label="Reachable by"
                  options={audienceChips}
                  value={audience || "all"}
                  onChange={(v) => setAudience(v === "all" ? "" : v)}
                />
                <ByApp
                  rows={filterRows(data.rows, q, audience)}
                  data={data}
                  onClear={() => {
                    setQ("");
                    setAudience("");
                  }}
                />
              </div>
            </TabsContent>

            <TabsContent value="credentials">
              <div className="pt-4">
                <ByCredential rows={data.rows} />
              </div>
            </TabsContent>

            {data.show_storage && (
              <TabsContent value="storage">
                <div className="pt-4">
                  <ByStore rows={data.rows} />
                </div>
              </TabsContent>
            )}
          </Tabs>
        </div>
      ) : null}
    </div>
  );
}

/**
 * The page's one narrowing beyond the search box: who can open the app —
 * Public, Anyone signed in or Restricted — counted in the tiles and the chips.
 *
 * Public has an address of its own because Admin → Authentication states that
 * count under the anonymous-access switch and links here to show it.
 */
export const PUBLIC_REACH_HREF = "/admin/reach?audience=public";

/** Who can open an app, as one of three words — read from the **effective**
 *  visibility the server sends, so an app set to Public on an install that has
 *  switched anonymous access off counts as signed-in, as its row says. */
function audienceOf(r: EstateReachRow): Audience {
  if (r.visibility === "public") return "public";
  if (r.visibility === "authenticated") return "authenticated";
  return "restricted";
}

type Audience = "public" | "authenticated" | "restricted";

const AUDIENCES: { value: Audience; label: string; icon: ReactNode }[] = [
  { value: "public", label: "Public", icon: <Globe className="h-4 w-4" /> },
  { value: "authenticated", label: "Anyone signed in", icon: <Building2 className="h-4 w-4" /> },
  { value: "restricted", label: "Restricted", icon: <LockKeyhole className="h-4 w-4" /> },
];

function countByAudience(rows: EstateReachRow[]): Record<Audience, number> {
  const n: Record<Audience, number> = { public: 0, authenticated: 0, restricted: 0 };
  for (const r of rows) n[audienceOf(r)]++;
  return n;
}

/** The estate's split, each figure a link to the apps behind it. Whole-estate
 *  totals: the search box narrows the table, not this. */
function AudienceSummary({ rows }: { rows: EstateReachRow[] }) {
  const n = countByAudience(rows);
  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
      {AUDIENCES.map((a) => (
        <StatCard
          key={a.value}
          label={a.label}
          value={n[a.value]}
          icon={a.icon}
          to={`/admin/reach?audience=${a.value}`}
        />
      ))}
    </div>
  );
}

function searched(rows: EstateReachRow[], q: string): EstateReachRow[] {
  const needle = q.trim().toLowerCase();
  if (!needle) return rows;
  return rows.filter((r) =>
    [
      r.name,
      r.slug,
      r.owner ?? "",
      ...r.credentials.map((c) => c.name),
      ...r.mounts.map((m) => `${m.resource_slug} ${m.path}`),
      ...r.integrations.map((i) => i.kind),
    ]
      .join(" ")
      .toLowerCase()
      .includes(needle),
  );
}

function filterRows(rows: EstateReachRow[], q: string, audience: string): EstateReachRow[] {
  rows = searched(rows, q);
  if (audience) rows = rows.filter((r) => audienceOf(r) === audience);
  return rows;
}

function ByApp({
  rows, data, onClear,
}: {
  rows: EstateReachRow[];
  data: EstateReach;
  onClear: () => void;
}) {
  if (rows.length === 0) {
    return (
      <EmptyState
        title="No apps match"
        action={
          <Button variant="outline" size="sm" onClick={onClear}>
            Clear
          </Button>
        }
      />
    );
  }
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            {/* App takes the leftover and truncates in it; Holds wraps at
                its own width. With Holds taking the slack, a long app name
                was cut at 16rem beside a half-empty column. */}
            <TableHead className="w-full">App</TableHead>
            <TableHead className="hidden @xl:table-cell">Reachable by</TableHead>
            <TableHead>Holds</TableHead>
            <TableHead className="hidden @3xl:table-cell">Owner</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.app_id}>
              <TableCell className="max-w-0">
                {/* `truncate` needs a block box — on an inline <a> it does
                    nothing, and a long app name walks straight over the column
                    beside it. The wrapper is what bounds it. */}
                <div className="truncate">
                  <Link
                    to={`/apps/${r.app_id}/reach`}
                    className="font-medium text-foreground hover:underline"
                  >
                    {r.name}
                  </Link>
                </div>
                <div className="truncate font-mono text-2xs text-muted-foreground">{r.slug}</div>
              </TableCell>
              <TableCell className="hidden whitespace-nowrap @xl:table-cell">
                <EffectiveAudience visibility={r.visibility} withheld={r.visibility_withheld} />
              </TableCell>
              <TableCell className="min-w-[12rem] whitespace-normal">
                <Holds row={r} data={data} />
              </TableCell>
              <TableCell className="hidden @3xl:table-cell">
                <AppOwner
                  ownerId={r.owner_id}
                  ownerGroupId={r.owner_group_id}
                  ownerLogin={r.owner}
                  withCount={false}
                  className="text-sm text-muted-foreground"
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableCard>
  );
}

/**
 * What one app holds, as chips.
 *
 * Counts rather than names, because the names are one click away on the app's
 * own tab and forty chips in a table cell is not a list anybody reads. A chip
 * is absent at zero: a row of "0 credentials, 0 mounts, 0 keys" carries no
 * information and makes the app that holds three of each harder to spot.
 */
function Holds({ row, data }: { row: EstateReachRow; data: EstateReach }) {
  const writable = row.mounts.some((m) => m.access !== "read");
  const chips: { key: string; label: string; tone?: keyof typeof toneSurface }[] = [];

  if (row.credentials.length > 0) {
    chips.push({
      key: "cred",
      label: `${row.credentials.length} credential${row.credentials.length === 1 ? "" : "s"}`,
    });
  }
  if (data.show_storage && row.mounts.length > 0) {
    chips.push({
      key: "mount",
      label: `${row.mounts.length} storage attached${writable ? ", writable" : ""}`,
      tone: writable ? "attention" : undefined,
    });
  }
  if (data.show_integrations && row.integrations.length > 0) {
    chips.push({
      key: "int",
      label: row.integrations.map((i) => i.kind).join(", "),
    });
  }
  if (row.env_keys > 0) {
    chips.push({ key: "env", label: `${row.env_keys} env var${row.env_keys === 1 ? "" : "s"}` });
  }
  if (row.viewer_identity) {
    chips.push({ key: "viewer", label: "Knows its viewer", tone: "attention" });
  }
  if (row.report_recipients > 0) {
    chips.push({
      key: "mail",
      label: `Mails ${row.report_recipients} ${row.report_recipients === 1 ? "person" : "people"}`,
    });
  }
  if (row.has_data) {
    chips.push({ key: "data", label: "Stored document" });
  }

  if (chips.length === 0) {
    return <span className="text-xs text-muted-foreground">Nothing</span>;
  }
  return (
    <span className="flex flex-wrap items-center gap-1.5">
      {chips.map((c) => (
        <Badge
          key={c.key}
          variant="outline"
          className={cn("text-xs font-medium", c.tone && toneSurface[c.tone])}
        >
          {c.label}
        </Badge>
      ))}
    </span>
  );
}

/**
 * The transpose: which apps hold each credential.
 *
 * The question an incident asks first, and until now the count was on screen
 * while the list was a SELECT. Built from the same rows the table above
 * renders — never a second query, so the two cannot disagree.
 */
function ByCredential({ rows }: { rows: EstateReachRow[] }) {
  const byCredential = new Map<string, { name: string; apps: EstateReachRow[] }>();
  for (const row of rows) {
    for (const c of row.credentials) {
      const entry = byCredential.get(c.id) ?? { name: c.name, apps: [] };
      // The id is kept because the name has a page behind it — see below.
      entry.apps.push(row);
      byCredential.set(c.id, entry);
    }
  }
  const entries = [...byCredential.entries()].sort((a, b) => a[1].name.localeCompare(b[1].name));

  if (entries.length === 0) {
    return (
      <EmptyState
        icon={<KeyRound className="h-6 w-6" />}
        title="No app holds a credential"
        description="Administrators attach credentials to apps. App owners never see the values."
      />
    );
  }
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="w-[18rem]">Credential</TableHead>
            <TableHead className="w-full">Held by</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {entries.map(([id, entry]) => (
            <TableRow key={id}>
              <TableCell className="max-w-[18rem]">
                {/* The credential's own page. This transpose exists to answer
                    "which apps hold this", and it named the thing and linked
                    nowhere — so the answer ended one question short of where
                    somebody could act on it. */}
                <Link
                  to={`/admin/credentials/${id}`}
                  className="block truncate font-medium text-primary hover:underline"
                >
                  {entry.name}
                </Link>
                <div className="text-2xs text-muted-foreground">
                  {entry.apps.length} app{entry.apps.length === 1 ? "" : "s"}
                </div>
              </TableCell>
              <TableCell className="max-w-0">
                <AppLinks apps={entry.apps} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableCard>
  );
}

/** The same transpose for durable storage: which apps mount each resource, and
 *  at what level. */
function ByStore({ rows }: { rows: EstateReachRow[] }) {
  type Mount = { row: EstateReachRow; path: string; access: string; kind: string };
  const byResource = new Map<string, { name: string; slug: string; mounts: Mount[] }>();
  for (const row of rows) {
    for (const m of row.mounts) {
      const entry = byResource.get(m.resource_id) ?? {
        name: m.resource_name || m.resource_slug,
        // The slug, because the resource's own page is keyed by it and this
        // column is otherwise a name with nowhere to go.
        slug: m.resource_slug,
        mounts: [],
      };
      entry.mounts.push({ row, path: m.path, access: m.access, kind: m.resource_kind });
      byResource.set(m.resource_id, entry);
    }
  }
  const entries = [...byResource.entries()].sort((a, b) => a[1].name.localeCompare(b[1].name));

  if (entries.length === 0) {
    return (
      <EmptyState
        icon={<Database className="h-6 w-6" />}
        title="No app has durable storage attached"
        description="Administrators create storage and attach it to apps."
      />
    );
  }
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="w-[18rem]">Resource</TableHead>
            <TableHead className="w-full">Attached to</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {entries.map(([id, entry]) => (
            <TableRow key={id}>
              <TableCell className="max-w-[18rem]">
                <Link
                  to={`/admin/storage/${entry.slug}`}
                  className="block truncate font-medium text-primary hover:underline"
                >
                  {entry.name}
                </Link>
                <div className="text-2xs text-muted-foreground">
                  {entry.mounts.length} attachment{entry.mounts.length === 1 ? "" : "s"}
                </div>
              </TableCell>
              <TableCell className="max-w-0">
                <div className="space-y-1.5">
                  {entry.mounts.map((m) => (
                    <div
                      key={`${m.row.app_id}:${m.path}`}
                      className="flex flex-wrap items-center gap-2"
                    >
                      <Link
                        to={`/apps/${m.row.app_id}/reach`}
                        className="text-sm text-foreground hover:underline"
                      >
                        {m.row.name}
                      </Link>
                      <span className="truncate font-mono text-2xs text-muted-foreground">
                        {m.path}
                      </span>
                      <Badge
                        variant="outline"
                        className={cn(
                          "text-xs font-medium",
                          accessBadgeClass(m.access as StorageAccess),
                        )}
                      >
                        {accessLabel(m.access as StorageAccess)}
                      </Badge>
                    </div>
                  ))}
                </div>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableCard>
  );
}

function AppLinks({ apps }: { apps: EstateReachRow[] }) {
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {apps.map((a) => (
        <Link
          key={a.app_id}
          to={`/apps/${a.app_id}/reach`}
          className="text-sm text-foreground hover:underline"
        >
          {a.name}
          {a.visibility === "public" && (
            <span className="ml-1 text-2xs text-muted-foreground">— public</span>
          )}
        </Link>
      ))}
    </span>
  );
}


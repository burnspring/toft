import { Fragment, useEffect, useState, type ReactNode } from "react";
import { activityTarget } from "@/lib/activity-target";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, AppWindow, ChevronDown, ChevronRight, ScrollText, UserRound } from "lucide-react";
import {
  api,
  type ActivityCategory,
  type ActivityEntry,
} from "@/lib/api";
import { summaryOf } from "@/lib/audit-actions";
import { chainedPages } from "@/lib/keyset-pages";
import { describeDevice } from "@/lib/user-agent";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { useUrlParam, useUrlParams } from "@/hooks/use-list-state";
import { FilterBar, FilterChips } from "@/components/filter-bar";
import { FacetChips, FacetFilter, type FacetChip, type FacetOption } from "@/components/facet-filter";
import { Input } from "@/components/ui/input";
import { FieldError } from "@/components/field-help";
import { zonedWallToInstant } from "@/lib/schedule";
import { fieldErrorId } from "@/lib/utils";
import { Notice } from "@/components/notice";
import { displayIsUTC, displayWallClock, displayZoneName, formatDateTime, formatExactTime } from "@/lib/time";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { TableCard } from "@/components/table-card";
import { RelativeTime } from "@/components/relative-time";
import { severityTone } from "@/lib/status";
import { toneSurface } from "@/lib/tone";

/**
 * The activity feed: everything a person did that the platform audits, and
 * everything that broke without anyone doing it, in one chronological list.
 *
 * They are two tables — audit_log is actor-driven and append-only, app_events
 * is machine observation that repeats — and they are merged only here. Which
 * is why the chips are server-side: "access" is a family of action strings,
 * and a list kept in this file would drift silently the first time the backend
 * writes a new one.
 */

const categories: { label: string; value: ActivityCategory }[] = [
  { label: "All", value: "all" },
  { label: "Deploys", value: "deploys" },
  { label: "Access", value: "access" },
  // Its own chip, not part of Access: sign-ins are the highest-volume family
  // in the table, and an administrator reading "who was given what" should not
  // have to page past a morning of logins to find it.
  { label: "Sign-ins", value: "signins" },
  { label: "Settings", value: "settings" },
  { label: "System", value: "system" },
];

const PAGE_SIZE = 50;

// How far back, as a request parameter: the feed is keyset paged from now, so
// without one, last month's rows were a long walk of Load more away.
// Shortest first and All last, as on Automation and the integration log —
// then Custom, which is the one that does not end at now: an incident is
// "Tuesday between two and four", and every preset made that a walk too.
const windows: { label: string; value: string }[] = [
  { label: "1 hour", value: "1h" },
  { label: "24 hours", value: "24h" },
  { label: "7 days", value: "7d" },
  { label: "30 days", value: "30d" },
  { label: "All time", value: "all" },
  { label: "Custom", value: "custom" },
];

const WINDOW_MS: Record<string, number> = {
  "1h": 3_600_000,
  "24h": 86_400_000,
  "7d": 7 * 86_400_000,
  "30d": 30 * 86_400_000,
};

// A predicate, not a partition: most rows are neither. An audit row is a
// warning when it records a refusal or a failure — the server's rule, by the
// action's own name — so the two together are "what went wrong".
const severities: FacetOption[] = [
  { key: "warning", label: "Warning" },
  { key: "error", label: "Error" },
];

/** A page of results plus the cursor that follows it. */
type Cursor = { before: string; before_id: string } | null;

/** Everything the feed is narrowed by, as the request carries it. */
interface Narrowing {
  search: string;
  category: ActivityCategory;
  app: string;
  /** An actor's id. */
  who: string;
  action: string;
  severity: string[];
  since: string | undefined;
  until: string | undefined;
}

/** One page's query. Shared so the page can read the first response — which
 *  names the actor — out of the cache the rows fill, without a second request. */
function feedQuery(n: Narrowing, cursor: Cursor) {
  return {
    queryKey: [
      "activity", n.category, n.search, n.app, n.who, n.action, n.severity.join(","),
      n.since, n.until, cursor?.before_id ?? "first",
    ],
    queryFn: () =>
      api.listActivity({
        q: n.search || undefined,
        category: n.category,
        app: n.app || undefined,
        actor: n.who || undefined,
        action: n.action || undefined,
        severity: n.severity.join(",") || undefined,
        since: n.since,
        until: n.until,
        limit: PAGE_SIZE,
        before: cursor?.before,
        before_id: cursor?.before_id,
      }),
    retry: false as const,
  };
}

/** A row's way to one thing's whole history: that filter alone, since the
 *  search or window that found the row would hide most of what is asked for. */
const historyHref = (name: "app" | "who" | "action", value: string) =>
  `/admin/audit?${new URLSearchParams({ [name]: value })}`;

export function AdminAuditPage() {
  // In the URL. Activity is read during an incident and the useful act is
  // sending somebody the narrowed feed — "every access change in the last
  // hour" — which component state made impossible: no link, no reload, and
  // Back stepped out of the page rather than out of the last filter.
  const [search, setSearch] = useUrlParam("q");
  const [category, setCategory] = useUrlParam("category", "all");
  const [range, setRange] = useUrlParam("window", "all");
  // A preset's lower edge is fixed when the window is chosen, not per
  // request: every page of one reading shares it, and a reload takes a fresh
  // one. Held beside the window it was taken for, so a link that changes the
  // window cannot leave the last one's edge behind.
  const [preset, setPreset] = useState(() => ({ range, since: sinceFor(range) }));
  if (preset.range !== range) setPreset({ range, since: sinceFor(range) });
  // A custom range is two wall-clock times in the display zone, which is what
  // every time on this page is printed in.
  const [from, setFrom] = useUrlParam("from");
  const [to, setTo] = useUrlParam("to");
  const custom = range === "custom";
  const zone = displayZoneName();
  const fromAt = custom ? zonedWallToInstant(from, zone) : null;
  const toAt = custom ? zonedWallToInstant(to, zone) : null;
  const backwards = Boolean(fromAt && toAt && fromAt >= toAt);
  // One app's history, by id: the server narrows both halves of the feed to
  // it, including the rows whose target is not the app — a denied request,
  // a presign the app made. An id rather than a slug, because a deleted
  // app's history is what somebody arrives looking for and its slug can
  // belong to another app by then.
  const [app, setApp] = useUrlParam("app");
  const { data: apps = [] } = useQuery({ queryKey: ["apps"], queryFn: () => api.listApps() });
  const appOptions: FacetOption[] = apps
    .map((a) => ({ key: a.id, label: a.name, search: a.slug }))
    .sort((a, b) => a.label.localeCompare(b.label));
  // What one person did, by id. Search could only approximate it: it matches
  // a name anywhere, so it also returned the rows somebody was the *target*
  // of, and every login theirs is part of. The label is the login, which is
  // what the Who column prints.
  const [who, setWho] = useUrlParam("who");
  const { data: users = [] } = useQuery({ queryKey: ["users"], queryFn: () => api.listUsers() });
  const whoOptions: FacetOption[] = users
    .map((u) => ({ key: u.id, label: u.login, search: `${u.name ?? ""} ${u.email}` }))
    .sort((a, b) => a.label.localeCompare(b.label));
  const [severity, setSeverity] = useUrlParams("severity");
  // Arrived at, from a row's Action badge: there are some two hundred
  // actions and nobody picks one from a list, but everybody knows the one
  // they are looking at.
  const [action, setAction] = useUrlParam("action");

  const narrowing: Narrowing = {
    search,
    category: category as ActivityCategory,
    app,
    who,
    action,
    severity,
    since: custom ? fromAt?.toISOString() : preset.since,
    until: custom ? toAt?.toISOString() : undefined,
  };

  // Pages accumulate rather than replace: this is a "load more" list, and the
  // cursor for the next request is whatever the last response handed back.
  // Held beside the narrowing they were read under, so any change to it — a
  // chip, or a row's link to this same page — starts from the first page
  // rather than carrying one feed's cursors into another.
  const reading = feedQuery(narrowing, null).queryKey.join("\u0000");
  const [paged, setPaged] = useState<{ reading: string; pages: Cursor[] }>({ reading, pages: [null] });
  const pages = paged.reading === reading ? paged.pages : [null];

  // A chosen value with no option still gets its chip: it arrives from a
  // link, and a deleted app — or an actor that is an app or a deploy key
  // rather than a person — is exactly the case. The actor's name is the
  // server's, read from the first page the rows below are already fetching.
  const appChip = app
    ? appOptions.find((o) => o.key === app) ?? { key: app, label: "A deleted app" }
    : undefined;
  const whoOption = whoOptions.find((o) => o.key === who);
  const first = useQuery({ ...feedQuery(narrowing, null), enabled: Boolean(who) && !whoOption });
  const whoLabel = whoOption?.label ?? first.data?.actor_label ?? (first.isError ? who : "…");
  const chips: FacetChip[] = [
    ...(appChip ? [{ option: { ...appChip, prefix: "App: " }, onRemove: () => setApp("") }] : []),
    ...(who ? [{ option: { key: who, label: whoLabel, prefix: "Who: " }, onRemove: () => setWho("") }] : []),
    ...severity.map((sev) => ({
      option: { ...(severities.find((o) => o.key === sev) ?? { key: sev, label: sev }), prefix: "Severity: " },
      onRemove: () => setSeverity(severity.filter((k) => k !== sev)),
    })),
    ...(action
      ? [{ option: { key: action, label: action, prefix: "Action: ", chipClassName: "font-mono" }, onRemove: () => setAction("") }]
      : []),
  ];
  const clearFacets = () => {
    setApp("");
    setWho("");
    setSeverity([]);
    setAction("");
  };

  return (
    <div>
      <PageHeader
        title="Activity"
        description="What people did on this install, and what failed on its own."
      />

      <div className="space-y-4">
        {/* The integration log's shape: the facets beside the search, and
            the category — what somebody opens this page for, "every access
            change" — as the primary axis on a row of its own. As two chip
            rows in one line they were eleven identical chips, and where the
            line broke depended on the window's width. */}
        <div className="flex flex-wrap items-center gap-2">
          <SearchInput
            placeholder="Search by app, person, action, file, IP or request ID…"
            value={search}
            onChange={setSearch}
            className="w-full sm:max-w-sm"
          />
          <FacetFilter
            label="App"
            icon={<AppWindow />}
            options={appOptions}
            value={app ? [app] : []}
            // One app at a time: the feed takes one id.
            onChange={(next) => setApp(next.filter((k) => k !== app).at(-1) ?? "")}
            placeholder="Find an app…"
            predicate
          />
          <FacetFilter
            label="Who"
            icon={<UserRound />}
            options={whoOptions}
            value={who ? [who] : []}
            // One actor at a time, as with the app.
            onChange={(next) => setWho(next.filter((k) => k !== who).at(-1) ?? "")}
            placeholder="Find a person…"
            predicate
          />
          <FacetFilter
            label="Severity"
            icon={<AlertTriangle />}
            options={severities}
            value={severity}
            onChange={setSeverity}
            predicate
          />
          <FilterChips
            label="How far back"
            options={windows}
            value={range}
            onChange={(v) => {
              setRange(v);
              // Custom opens on the last day rather than on two empty
              // fields, which narrow nothing and look like they have.
              const seed = v === "custom" && !from && !to;
              setFrom(v !== "custom" ? "" : seed ? displayWallClock(new Date(Date.now() - WINDOW_MS["24h"])) : from);
              setTo(v !== "custom" ? "" : seed ? displayWallClock() : to);
            }}
          />
        </div>
        {custom && (
          <div className="space-y-1.5">
            <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
              <Input
                type="datetime-local"
                aria-label="From"
                value={from}
                onChange={(e) => setFrom(e.target.value)}
                className="h-8 w-48 text-xs md:text-xs"
              />
              <span>to</span>
              <Input
                id="activity-to"
                type="datetime-local"
                aria-label="To"
                value={to}
                aria-invalid={backwards}
                aria-describedby={backwards ? fieldErrorId("activity-to") : undefined}
                onChange={(e) => setTo(e.target.value)}
                className="h-8 w-48 text-xs md:text-xs"
              />
              {/* Named, because the fields cannot say it: a time typed here
                  is read in the zone every row on the page is printed in. */}
              <span>{zone} time. Leave either empty for no limit on that side.</span>
            </div>
            {backwards && <FieldError field="activity-to">The range ends before it starts.</FieldError>}
          </div>
        )}
        <FacetChips chips={chips} onClear={clearFacets} />
        {/* Uncounted: the feed pages by keyset, so a count would be a
            count of what happens to be loaded rather than of what the
            category holds. The exception the rule already names. */}
        <FilterBar
          label="Filter the feed by category"
          options={categories}
          value={category}
          onChange={setCategory}
        />

        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-[40px]" />
                <TableHead className="w-[150px]">When</TableHead>
                {/* Column priority: when, what, and the sentence. Who and
                    which object yield first — and reappear inside the Summary
                    cell at the width that dropped them, because not every row
                    expands and a table may not hide the only route to a
                    fact. */}
                <TableHead className="hidden w-[160px] @2xl:table-cell">Who</TableHead>
                {/* The summary restates the action in words, which is what
                    makes this one droppable at the width where 380px of fixed
                    columns had left the summary nothing. */}
                <TableHead className="hidden w-[190px] @xl:table-cell">Action</TableHead>
                <TableHead className="hidden w-[170px] @3xl:table-cell">Target</TableHead>
                <TableHead className="w-full">Summary</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {pages.map((cursor, i) => (
                <ActivityPageRows
                  key={`${reading}:${cursor ? cursor.before_id : "first"}`}
                  cursor={cursor}
                  index={i}
                  onTail={(index, tailId) =>
                    setPaged((p) => {
                      if (p.reading !== reading) return p;
                      const chained = chainedPages(p.pages, index, tailId);
                      return chained === p.pages ? p : { reading, pages: chained };
                    })
                  }
                  narrowing={narrowing}
                  whoLabel={whoLabel}
                  isLast={i === pages.length - 1}
                  isOnly={pages.length === 1}
                  onMore={(next) => setPaged({ reading, pages: [...pages, next] })}
                  onClear={() => {
                    setSearch("");
                    setCategory("all");
                    setRange("all");
                    setFrom("");
                    setTo("");
                    clearFacets();
                  }}
                />
              ))}
            </TableBody>
          </Table>
        </TableCard>
      </div>
    </div>
  );
}

/** The window's lower edge, or none for All, Custom and an unknown value. */
function sinceFor(range: string): string | undefined {
  const ms = WINDOW_MS[range];
  return ms ? new Date(Date.now() - ms).toISOString() : undefined;
}

/** Which nothing this is. An empty audit result is often the finding, so it
 *  names the narrowest thing that was asked for rather than "Nothing here". */
function emptyTitle(n: Narrowing, whoLabel: string): string {
  if (n.search) return "No activity matches that search";
  if (n.who) return `Nothing recorded by ${whoLabel}`;
  if (n.action) return "Nothing recorded with this action";
  if (n.app) return "Nothing recorded for this app";
  if (n.severity.length > 0) {
    const names = n.severity.map((sev) => `${sev}s`).join(" or ");
    return `No ${names} recorded`;
  }
  if (n.since || n.until) return "Nothing recorded in this window";
  if (n.category !== "all") return "Nothing recorded in this category";
  return "Nothing recorded yet";
}

/**
 * One page of the feed.
 *
 * A component per page rather than one query with accumulated state: each page
 * keeps its own cursor, so paging cannot double-count or skip a row when new
 * activity lands between requests — which on a feed that grows while it is
 * being read is not hypothetical. That holds for Load more, not for a refetch:
 * a page re-read after new rows landed has a newer tail than the cursor the
 * next page holds, so each page reports its tail and the pages after one that
 * moved are dropped (`chainedPages`) rather than leaving a silent gap.
 */
function ActivityPageRows({
  cursor, index, onTail, narrowing, whoLabel, isLast, isOnly, onMore, onClear,
}: {
  cursor: Cursor;
  index: number;
  onTail: (index: number, tailId: string | undefined) => void;
  narrowing: Narrowing;
  whoLabel: string;
  isLast: boolean;
  isOnly: boolean;
  onMore: (next: Cursor) => void;
  onClear: () => void;
}) {
  const { data, isLoading, error, refetch } = useQuery(feedQuery(narrowing, cursor));
  const { app } = narrowing;
  const loaded = data !== undefined;
  const tailId = data?.next_before_id || undefined;
  useEffect(() => {
    if (loaded) onTail(index, tailId);
  }, [loaded, tailId, index, onTail]);

  if (isLoading) {
    return (
      <TableRow className="hover:bg-transparent">
        <TableCell colSpan={6} className="p-4">
          <div className="space-y-3">
            {Array.from({ length: isOnly ? 6 : 2 }).map((_, i) => (
              <div key={i} className="flex items-center gap-3">
                <Skeleton className="h-4 w-28" />
                <Skeleton className="h-4 w-32" />
                <Skeleton className="h-4 w-40" />
                <Skeleton className="ml-auto h-4 w-56" />
              </div>
            ))}
          </div>
        </TableCell>
      </TableRow>
    );
  }

  if (error) {
    // 404 is what a non-administrator gets, deliberately: what an install
    // records about itself is not something everyone is owed.
    return isOnly ? (
      <TableRow className="hover:bg-transparent">
        {/* Wraps: the server refuses in a sentence, and a cell is nowrap —
            a refused filter ran 800px past the card at a phone's width. */}
        <TableCell colSpan={6} className="whitespace-normal">
          <QueryError
            title="The activity feed could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </TableCell>
      </TableRow>
    ) : null;
  }

  const entries = data?.entries ?? [];

  if (entries.length === 0) {
    const title = emptyTitle(narrowing, whoLabel);
    return isOnly ? (
      <TableRow className="hover:bg-transparent">
        <TableCell colSpan={6} className="whitespace-normal">
          <EmptyState
            icon={narrowing.search ? undefined : <ScrollText className="h-6 w-6" />}
            title={title}
            action={
              title !== "Nothing recorded yet" ? (
                <Button variant="outline" size="sm" onClick={onClear}>
                  Clear
                </Button>
              ) : undefined
            }
          />
        </TableCell>
      </TableRow>
    ) : null;
  }

  const next: Cursor =
    data?.next_before && data.next_before_id
      ? { before: data.next_before, before_id: data.next_before_id }
      : null;

  return (
    <>
      {/* Where attribution starts, so a narrowed history from before it is
          not read as complete. Once, above the first page. */}
      {cursor === null && app && data?.app_scope_started && (
        <TableRow className="hover:bg-transparent">
          <TableCell colSpan={6} className="whitespace-normal p-3">
            <Notice tone="info" title={`This history is complete from ${formatDateTime(data.app_scope_started)}`}>
              Earlier rows appear here only when the app was what they changed. Search finds the rest.
            </Notice>
          </TableCell>
        </TableRow>
      )}
      {runsOf(entries).map((run) =>
        run.length === 1 ? (
          <ActivityRow key={`${run[0].source}-${run[0].id}`} entry={run[0]} narrowing={narrowing} />
        ) : (
          <ActivityRun key={`run-${run[0].source}-${run[0].id}`} run={run} narrowing={narrowing} />
        ),
      )}
      {isLast && next && (
        <TableRow className="hover:bg-transparent">
          <TableCell colSpan={6} className="py-3 text-center">
            <Button variant="outline" size="sm" onClick={() => onMore(next)}>
              Load more
            </Button>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

/**
 * The raw action, and the way to every row that carries it. A link rather
 * than a filter control: nobody picks one of two hundred actions from a list,
 * but the one in front of them is one click. Plain once the feed is already
 * narrowed to it.
 */
function ActionBadge({ entry, current }: { entry: ActivityEntry; current: string }) {
  const tone = toneSurface[severityTone(entry.severity)];
  if (current === entry.action) {
    return <Badge variant="outline" className={`text-2xs font-medium ${tone}`}>{entry.action}</Badge>;
  }
  return (
    <Link
      to={historyHref("action", entry.action)}
      aria-label={`Show every ${entry.action}`}
      onClick={(e) => e.stopPropagation()}
      className="rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      <Badge variant="outline" className={`text-2xs font-medium hover:underline ${tone}`}>{entry.action}</Badge>
    </Link>
  );
}

/**
 * The request behind an audited row: where it came from and what made it.
 *
 * Recorded on every audit row since the table existed and shown nowhere until
 * now, which left an install able to say who changed something but not from
 * where — the first question a security review asks. An event row has none of
 * this and renders nothing, because no request produced an observation.
 */
function RequestDetails({ entry }: { entry: ActivityEntry }) {
  const fields: { label: string; value: ReactNode }[] = [];

  // The exact moment, to the second and with its zone, first: the relative
  // "3h ago" in the row is for scanning, and this is what somebody copies
  // into an incident note. UTC beside it unless that is the zone already —
  // it is what the server's own logs and every export are written in.
  fields.push({
    label: "Time",
    value: (
      <>
        <span>{formatExactTime(entry.occurred_at)}</span>
        {!displayIsUTC() && (
          <span className="block font-mono text-2xs text-muted-foreground">
            {formatExactTime(entry.occurred_at, "UTC")}
          </span>
        )}
      </>
    ),
  });

  if (entry.ip) {
    fields.push({ label: "IP address", value: <span className="font-mono">{entry.ip}</span> });
  }
  if (entry.api_key_name) {
    fields.push({ label: "API key", value: entry.api_key_name });
  } else if (entry.api_key_id) {
    // The name was only ever a join; the id is on the row and outlives it.
    fields.push({
      label: "API key",
      value: (
        <>
          <span className="font-mono">{entry.api_key_id}</span>
          <span className="text-muted-foreground"> (deleted)</span>
        </>
      ),
    });
  }
  // The device as a person would name it, with the raw string under it for
  // whoever has to match it character by character.
  const device = describeDevice(entry.user_agent);
  if (device) {
    fields.push({
      label: "Client",
      value: (
        <>
          <span>{device.label}</span>
          {device.raw !== device.label && (
            <span className="block font-mono text-2xs text-muted-foreground">{device.raw}</span>
          )}
        </>
      ),
    });
  }
  if (entry.request_id) {
    fields.push({
      label: "Request ID",
      value: <span className="font-mono">{entry.request_id}</span>,
    });
  }

  if (fields.length === 0) return null;

  return (
    // `whitespace-normal` is load-bearing, not tidiness. This panel lives in a
    // TableCell, which carries `whitespace-nowrap`, and that inherits all the
    // way down — while it holds, `break-all` below can do nothing, because
    // word-break decides *where* a line may break and white-space decides
    // whether it may break at all. Without this a browser's user agent renders
    // as one 900px line inside a 214px column and paints straight over the
    // request id in the next one.
    <dl className="grid gap-x-8 gap-y-3 px-6 py-3 whitespace-normal sm:grid-cols-2 lg:grid-cols-4">
      {fields.map((f) => (
        <div key={f.label} className="min-w-0">
          <dt className="text-xs text-muted-foreground">
            {f.label}
          </dt>
          {/* Wraps rather than truncates: a user agent is long and is exactly
              the kind of value somebody reads character by character, and a
              truncated one with no way to see the rest is worse than a tall
              row in a region you had to open on purpose. */}
          <dd className="mt-0.5 text-xs break-all">{f.value}</dd>
        </div>
      ))}
    </dl>
  );
}

function ActivityRow({ entry, narrowing }: { entry: ActivityEntry; narrowing: Narrowing }) {
  const [open, setOpen] = useState(false);
  const target = activityTarget(entry);
  const hasDetail = entry.detail.trim().length > 0;
  // A row offers the whole history of each thing it names — its app (the way
  // in for a row whose target is not the app), whoever did it, and its
  // action — except the ones the feed is already narrowed to. Here as well as
  // on the Action badge, whose column a narrow table drops.
  const histories: { href: string; label: ReactNode }[] = [];
  if (entry.app_id && !narrowing.app) {
    histories.push({ href: historyHref("app", entry.app_id), label: "This app’s history" });
  }
  if (entry.actor_id && entry.actor_id !== narrowing.who) {
    histories.push({ href: historyHref("who", entry.actor_id), label: `Everything by ${entry.actor_label}` });
  }
  if (entry.action !== narrowing.action) {
    histories.push({
      href: historyHref("action", entry.action),
      label: <>Every <span className="font-mono">{entry.action}</span></>,
    });
  }

  return (
    <Fragment>
      <TableRow
        // Every row opens: the exact time is in its details, and that is the
        // one fact every event has.
        className="cursor-pointer"
        onClick={() => setOpen((o) => !o)}
      >
        <TableCell className="text-muted-foreground">
          {/* A real button, not a decorative chevron: without it the exact
              time, the address and the credential behind a row are
              unreachable by keyboard, which is the whole reason it expands. */}
          <button
            type="button"
            aria-expanded={open}
            aria-label={`${open ? "Hide" : "Show"} details for ${entry.action}`}
            onClick={(e) => {
              e.stopPropagation();
              setOpen((o) => !o);
            }}
            className="rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {open ? (
              <ChevronDown className="h-4 w-4" />
            ) : (
              <ChevronRight className="h-4 w-4" />
            )}
          </button>
        </TableCell>
        <TableCell className="text-sm text-muted-foreground">
          <RelativeTime iso={entry.occurred_at} exact />
        </TableCell>
        <TableCell className="hidden text-sm text-foreground @2xl:table-cell">{entry.actor_label}</TableCell>
        <TableCell className="hidden @xl:table-cell">
          <ActionBadge entry={entry} current={narrowing.action} />
        </TableCell>
        <TableCell className="hidden max-w-[170px] truncate text-sm @3xl:table-cell">
          {/* `activityTarget` decides: a live app links by id, a deleted one
              is named and never linked. */}
          {target.href ? (
            <Link
              to={target.href}
              className="text-primary hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {target.text}
            </Link>
          ) : (
            <span className="text-muted-foreground">
              {target.text || "—"}
              {target.gone && target.text !== "Deleted app" && " (deleted)"}
            </span>
          )}
        </TableCell>
        {/* Wraps: it is the column the page is for, and truncated it was
            clipped mid-word on a phone. */}
        <TableCell className="min-w-[12rem] whitespace-normal text-sm text-muted-foreground">
          <span className="block">{summaryOf(entry)}</span>
          {/* Who and Target are columns at `@2xl` and `@3xl`. Below those they
              are *here*, because a table may only hide a column it is not the
              only route to — and a row with no detail and no request does not
              expand, so on a phone those two facts were simply gone from an
              audit trail. Each restatement carries **its own column's**
              container tier: read on the viewport, the two ladders crossed,
              and on this page — which sits inside the admin rail — Who was
              printed twice for most of a desktop's width. */}
          <span className="mt-0.5 flex flex-wrap gap-x-2 text-2xs @3xl:hidden">
            <span className="@2xl:hidden">by {entry.actor_label}</span>
            {(entry.target_label || entry.target_id) && (
              <span className="@3xl:hidden">on {target.text}</span>
            )}
          </span>
        </TableCell>
      </TableRow>
      {open && (
        <TableRow className="hover:bg-transparent">
          <TableCell colSpan={6} className="bg-muted/40 p-0">
            <RequestDetails entry={entry} />
            {histories.length > 0 && (
              <div className="flex flex-wrap gap-x-5 gap-y-1 px-6 py-2 text-xs">
                {histories.map((h) => (
                  <Link key={h.href} to={h.href} className="text-primary hover:underline">
                    {h.label}
                  </Link>
                ))}
              </div>
            )}
            {hasDetail && (
              <pre
                tabIndex={0}
                role="region"
                aria-label="Event detail"
                className={`max-h-64 overflow-auto focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none px-6 py-3 font-mono text-xs whitespace-pre-wrap text-muted-foreground border-t border-border/60`}
              >
                {entry.detail}
              </pre>
            )}
          </TableCell>
        </TableRow>
      )}
    </Fragment>
  );
}

/**
 * Consecutive rows that say the same thing — one person, one action, one
 * target — as runs. A morning of sign-ins was most of the default view, each
 * row restating the one above, and the deploy between them was the row nobody
 * saw. Only *consecutive* rows join, so the order of events is never changed,
 * and a run opens to every row it holds: an audit trail folds, it never drops.
 */
function runsOf(entries: ActivityEntry[]): ActivityEntry[][] {
  const runs: ActivityEntry[][] = [];
  for (const e of entries) {
    const last = runs[runs.length - 1];
    const head = last?.[0];
    if (
      head &&
      head.actor_label === e.actor_label &&
      head.action === e.action &&
      head.target_type === e.target_type &&
      head.target_id === e.target_id
    ) {
      last.push(e);
    } else {
      runs.push([e]);
    }
  }
  return runs;
}

function ActivityRun({ run, narrowing }: { run: ActivityEntry[]; narrowing: Narrowing }) {
  const [open, setOpen] = useState(false);
  const head = run[0];
  const earliest = run[run.length - 1];
  const target = activityTarget(head);
  return (
    <Fragment>
      <TableRow className="cursor-pointer" onClick={() => setOpen((o) => !o)}>
        <TableCell className="text-muted-foreground">
          <button
            type="button"
            aria-expanded={open}
            aria-label={`${open ? "Hide" : "Show"} ${run.length} ${head.action} rows`}
            onClick={(e) => {
              e.stopPropagation();
              setOpen((o) => !o);
            }}
            className="rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
          </button>
        </TableCell>
        <TableCell className="text-sm text-muted-foreground">
          <RelativeTime iso={head.occurred_at} exact />
        </TableCell>
        <TableCell className="hidden text-sm text-foreground @2xl:table-cell">{head.actor_label}</TableCell>
        <TableCell className="hidden @xl:table-cell">
          <ActionBadge entry={head} current={narrowing.action} />
        </TableCell>
        <TableCell className="hidden max-w-[170px] truncate text-sm @3xl:table-cell">
          {target.href ? (
            <Link to={target.href} className="text-primary hover:underline" onClick={(e) => e.stopPropagation()}>
              {target.text}
            </Link>
          ) : (
            <span className="text-muted-foreground">{target.text || "—"}</span>
          )}
        </TableCell>
        <TableCell className="min-w-[12rem] whitespace-normal text-sm text-muted-foreground">
          <span className="block">
            {summaryOf(head)}{" "}
            <span className="tabular-nums text-foreground">× {run.length}</span>
          </span>
          <span className="mt-0.5 block text-2xs">
            Since <RelativeTime iso={earliest.occurred_at} exact />
            <span className="@2xl:hidden"> · by {head.actor_label}</span>
          </span>
        </TableCell>
      </TableRow>
      {open &&
        run.map((e) => (
          <ActivityRow key={`${e.source}-${e.id}`} entry={e} narrowing={narrowing} />
        ))}
    </Fragment>
  );
}

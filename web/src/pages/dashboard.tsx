import { useState, useMemo } from "react";
import { copyToClipboard } from "@/lib/clipboard";
import { Link, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  Plus, MoreHorizontal, RotateCcw, Play, Square, Trash2,
  Copy, ScrollText, PackageSearch, Tag as TagIcon, Boxes, Languages as LanguagesIcon,
  LockKeyhole, Download, UserRound,
} from "lucide-react";
import { appShareUrl, type AppAddress } from "@/lib/app-url";
import { APP_KIND_LABEL, appKind, isBatchFramework } from "@/lib/frameworks";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { SortableHead } from "@/components/sortable-head";
import { TableCard } from "@/components/table-card";
import { compareDatesMissingLast, nextSort, type SortState } from "@/lib/sort";
import { FilterBar } from "@/components/filter-bar";
import { Badge } from "@/components/ui/badge";
import { StatusBadge } from "@/components/status-badge";
import { StopAppDialog } from "@/components/stop-app-dialog";
import { stopNeedsConfirm } from "@/lib/stop-tier";
import { useAuth } from "@/hooks/use-auth";
import { EmptyState } from "@/components/empty-state";
import { AppIcon } from "@/components/app-icon";
import { FrameworkBadge } from "@/components/framework-badge";
import { RelativeTime } from "@/components/relative-time";
import { DeleteAppDialog } from "@/components/delete-app-dialog";
import { TagChips } from "@/components/tag-chip";
import { FacetChips, FacetFilter, type FacetChip, type FacetOption } from "@/components/facet-filter";
import { modeFacetOptions, ownerFacetKey, ownerFacetOptions, tagFacetKey, tagFacetOptions, versionFacetOptions } from "@/lib/app-facets";
import { runtimeSeriesKey } from "@/lib/runtime-series";
import { ModeBadge, ModePending } from "@/components/mode-badge";
import { anyIsolated, pendingMode, runningMode } from "@/lib/execution-mode";
import type { AppWithMetrics } from "@/lib/api";
import { toneSurface, toneText } from "@/lib/tone";
import { cn } from "@/lib/utils";
import { QueryError } from "@/components/query-error";
import { useUrlParam, useUrlParams } from "@/hooks/use-list-state";
import { AppOpenAction } from "@/components/app-open-action";
import { AppOwner } from "@/components/app-owner";
import { TablePagination } from "@/components/table-pagination";
import { appsToXlsx } from "@/lib/export-xlsx";

/**
 * The primary axis, and it is the platform's whole vocabulary of app status.
 *
 * `failed` was missing, which made an app in that state reachable only through
 * All — the one status where somebody is looking for the app rather than
 * browsing. The list is the server's `AppStatus*` constants in the order a
 * reader wants them, and it stays that: a status the platform can set and this
 * strip cannot select is a filter bar that quietly excludes rows.
 */
/** The page's two halves. `mine` is the default and is never spelled in the
 *  URL — see useUrlParam. */
const SCOPES = [
  { value: "mine", label: "Mine" },
  { value: "all", label: "All apps" },
] as const;

const statusFilters = [
  { label: "All", value: "all" },
  { label: "Running", value: "running" },
  { label: "Building", value: "building" },
  { label: "Sleeping", value: "sleeping" },
  { label: "Ready", value: "ready" },
  { label: "Stopped", value: "stopped" },
  { label: "Crashed", value: "crashed" },
  { label: "Failed", value: "failed" },
];

/** Tabs shown even at zero — the incident questions. The rest appear when
 *  something is in them. */
const ALWAYS_SHOWN = new Set(["all", "running", "crashed", "failed"]);

const IN_PROGRESS = new Set(["building", "deploying", "pending"]);

type SortKey = "name" | "status" | "deployed";
// "none" keeps the API's natural order until a header is clicked.
type SortColumn = SortKey | "none";

export function DashboardPage() {
  /**
   * All of it in the URL, not just the one facet that started there.
   *
   * The runtime facet lived in the URL because Admin → Languages links a series
   * here, and everything else lived in component state — so this page could be
   * narrowed to something and then not be sent to anybody. No link went to
   * "Crashed", Back stepped out of the page rather than out of the last filter,
   * and a reload put a person back at the top of the whole list. That is the
   * difference between pasting a link into a channel during an incident and
   * describing which buttons to press.
   */
  const [search, setSearch] = useUrlParam("q");
  const [statusFilter, setStatusFilter] = useUrlParam("status", "all");
  // Each entry in tagFilters is "tagSlug:valueSlug"; modeFilters holds
  // execution modes; versionFilters holds `runtimeSeriesKey`s.
  const [tagFilters, setTagFilters] = useUrlParams("tag");
  const [modeFilters, setModeFilters] = useUrlParams("mode");
  const [versionFilters, setVersionFilters] = useUrlParams("runtime");
  // Locking creates a row state, and the confirmation policy's fourth clause
  // is that a new row state gets its matching list filter in the same change.
  // An overlay rather than a status tab: a locked app is *also* stopped, and
  // the strip is a pure partition of the server's AppStatus constants — adding
  // `locked` to that vocabulary is what the invariants forbid.
  const [lockedFilters, setLockedFilters] = useUrlParams("locked");
  // `user:<id>` and `group:<id>` (ownerFacetKey) — a person and a group are
  // two kinds of owner, so the facet keeps them apart under a heading each.
  const [ownerFilters, setOwnerFilters] = useUrlParams("owner");
  // The page's two halves (ADR §90). Without them the install-wide rung turns a
  // personal list of four apps into an estate list of four hundred and reads as
  // a regression — and `mine` being the default is the whole reason it is not
  // one: somebody who has always had four apps still opens the page to four.
  //
  // A **scope**, not a facet, which is why it is a single value and why Clear
  // below leaves it alone: every other control on this page reveals rows when
  // you clear it, and a Clear that dropped you from `all` back to `mine` would
  // be the one that hides them. It replaced the `?access=open|listed` facet
  // rather than joining it — two controls for one partition, one of them a chip
  // and one of them a scope, disagree on any URL carrying both.
  //
  // An unrecognized value reads as `mine`, because only `all` is tested for.
  const [scope, setScope] = useUrlParam("scope", "mine");
  // The address the Owner facet replaced, kept working because links to it are
  // out there (a group's "3 apps" used to land on it). The group pages link to
  // `?owner=group:<id>` now; this still narrows, and still gets its chip.
  const [ownerGroup, setOwnerGroup] = useUrlParam("owner_group");
  // Arrived at from Admin → System info's app kinds, never built: there is no
  // control for it, only the chip that says the list is narrowed. `jobs` reads
  // `has_jobs`, which only an administrator's listing carries.
  const [kind, setKind] = useUrlParam("kind");
  const [sort, setSort] = useState<SortState<SortColumn>>({ key: "none", dir: "desc" });
  const [appToDelete, setAppToDelete] = useState<AppWithMetrics | null>(null);
  const [toStop, setToStop] = useState<AppWithMetrics | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(25);
  const [exporting, setExporting] = useState(false);
  const { user } = useAuth();
  // Availability is an administrator's question — which apps are locked, and
  // which of those the dependency policy locked — so it is absent for
  // everybody else, and a `?locked=` arriving in their URL narrows nothing.
  const isAdmin = user?.system_role === "system_admin";
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  // The same read AppOwner does, hitting the same cache key — one request for
  // the whole table, and the export gets a group's name without a second way
  // of working it out.
  const { data: groups = [] } = useQuery({
    queryKey: ["groups"],
    queryFn: api.listGroups,
    staleTime: 5 * 60_000,
  });

  const { data: apps = [], isLoading, error, refetch } = useQuery<AppWithMetrics[]>({
    queryKey: ["apps"],
    queryFn: () => api.listApps(),
    // Live statuses: poll fast while any deploy is in flight, slowly otherwise.
    refetchInterval: (query) =>
      (query.state.data ?? []).some((a) => IN_PROGRESS.has(a.status) || a.deploying)
        ? 3000
        : 15000,
  });

  // What this account may do, for the empty state alone. Same key as the
  // publisher gate and the wizard, so it costs one cached request.
  const { data: caps } = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.getCapabilities,
    retry: false,
  });

  // Everything except the status axis.
  //
  // Split out so the tab bar's counts can be taken from it: "Crashed (2)" has
  // to mean two crashed apps among the ones searched and faceted down to, or
  // the count sits above a table that visibly disagrees with it. Status is the
  // one thing left out, because the tabs are what select it.
  const unscoped = useMemo(() => apps.filter((app) => {
    const q = search.toLowerCase();
    const matchesSearch = app.name.toLowerCase().includes(q) ||
      app.slug.toLowerCase().includes(q) ||
      (app.tags ?? []).some((t) => t.value.toLowerCase().includes(q));
    // Multiple tag filters are ANDed: "Engineering *and* Production".
    const matchesTags = tagFilters.every((key) =>
      (app.tags ?? []).some((t) => tagFacetKey(t) === key));
    // ORed, unlike tags: an app runs under exactly one mode, so ANDing two of
    // them would always select nothing. The facets still AND with each other —
    // "isolated *and* tagged Production".
    // A list-rung row has no mode to match, rather than reading as shared.
    const matchesMode = modeFilters.length === 0 ||
      (app.can_open && modeFilters.includes(runningMode(app)));
    // ORed for the same reason as mode: an app is on exactly one release line.
    const matchesVersion = versionFilters.length === 0 ||
      (!!app.runtime && versionFilters.includes(runtimeSeriesKey(app.runtime)));
    // Narrows before the counts are computed, so the strip keeps agreeing with
    // the table beneath it.
    // ORed within the facet, like mode and version: `locked` is every lock and
    // `locked:dependency` is the subset the dependency policy placed, so
    // picking both is picking the wider one.
    const matchesLocked =
      !isAdmin || lockedFilters.length === 0 ||
      lockedFilters.some((k) =>
        k === "locked:dependency"
          ? !!app.locked_at && app.locked_source === "dependency"
          : !!app.locked_at,
      );
    // "Mine" is a relationship somebody wrote down — you own it, a group you
    // are in owns it, or it was granted to you. Not the apps you can *open*,
    // which was the first answer and is wrong in the direction that matters: a
    // public app belonging to a team you have never met is openable by
    // everybody, and it does not belong beside the four apps you actually work
    // on. It is also not the apps you *own* — one shared with you as editor is
    // yours in every sense this page means.
    // ORed, like mode: an app has exactly one owner, person or group.
    const matchesOwner = ownerFilters.length === 0 || ownerFilters.includes(ownerFacetKey(app));
    const matchesKind = !isAdmin || !kind ||
      (kind === "jobs" ? !!app.has_jobs : appKind(app.framework) === kind);
    return matchesSearch && matchesTags && matchesMode && matchesVersion &&
      matchesLocked && matchesOwner && matchesKind;
  }), [apps, search, tagFilters, modeFilters, versionFilters, lockedFilters, ownerFilters, isAdmin, kind]);

  // "Mine" is a relationship somebody wrote down — you own it, a group you
  // are in owns it, or it was granted to you. Not the apps you can *open*,
  // which was the first answer and is wrong in the direction that matters: a
  // public app belonging to a team you have never met is openable by
  // everybody, and it does not belong beside the four apps you actually work
  // on. It is also not the apps you *own* — one shared with you as editor is
  // yours in every sense this page means.
  //
  // Applied *after* the rest, and kept apart from it, so the empty state can
  // ask the one question that matters when a search comes back with nothing:
  // would it have matched something on the other side of this scope? It
  // otherwise said "None of the 49 apps on this install match your search"
  // about a colleague's app that matches perfectly, and the only control it
  // offered cleared the filters that were not the problem.
  const scoped = useMemo(() => {
    const byScope = scope === "all" ? unscoped : unscoped.filter((app) => app.mine);
    if (!ownerGroup) return byScope;
    return byScope.filter((app) => app.owner_group_id === ownerGroup);
  }, [unscoped, scope, ownerGroup]);
  /** The group named in the URL, from the same read `AppOwner` and the export
   *  already share — the app row carries the id and not the name. */
  const ownerGroupName = ownerGroup
    ? groups.find((g) => g.id === ownerGroup)?.name
    : undefined;

  const filtered = useMemo(() => {
    const list = statusFilter === "all"
      ? scoped
      : scoped.filter((app) => app.status === statusFilter);
    if (sort.key === "none") return list;
    // Each comparator applies the direction itself, because "never deployed"
    // is pinned last whichever way the column runs.
    const dir = sort.dir === "asc" ? 1 : -1;
    const cmp: Record<SortKey, (a: AppWithMetrics, b: AppWithMetrics) => number> = {
      name: (a, b) => a.name.localeCompare(b.name) * dir,
      // `?? ""`: a row held at the `list` rung carries no status, and sorts
      // with the other unknowns rather than throwing.
      status: (a, b) => (a.status ?? "").localeCompare(b.status ?? "") * dir,
      // The real deploy date, and **never deployed sorts last** in either
      // direction rather than as the epoch: an app that has never shipped is
      // not the oldest deploy, and putting it at the top of a descending sort
      // during an incident is exactly the wrong answer.
      deployed: (a, b) => compareDatesMissingLast(a.last_deployed_at, b.last_deployed_at, sort.dir),
    };
    const key = sort.key;
    return [...list].sort((a, b) => cmp[key](a, b));
  }, [scoped, statusFilter, sort]);

  // The page's slice, and the guard that makes it survive filtering. Narrowing
  // a list while on page four leaves a page that no longer exists — an empty
  // table under a footer that says there are thirty rows — so the page index
  // is clamped to what the filter actually produced rather than reset by an
  // effect watching every filter on the page.
  const totalPages = Math.max(1, Math.ceil(filtered.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const paged = filtered.slice((currentPage - 1) * pageSize, currentPage * pageSize);

  /** What the Owner column renders, as one string. The sheet and the column
   *  answer from the same place so they cannot disagree about a group. */
  function ownerLabel(app: AppWithMetrics): string {
    if (app.owner_group_id) {
      return groups.find((g) => g.id === app.owner_group_id)?.name ?? "";
    }
    return app.owner?.login ?? "";
  }

  // Exports what the table currently shows across all pages, not one page —
  // the same rule People's export follows, and the one somebody filtering to
  // "every crashed app" is relying on.
  async function exportApps() {
    setExporting(true);
    try {
      await appsToXlsx(filtered, ownerLabel);
      toast.success("Apps exported");
    } catch {
      toast.error("Couldn’t build the Excel file — try again");
    } finally {
      setExporting(false);
    }
  }

  const counts = useMemo(() => {
    const c: Record<string, number> = { all: scoped.length };
    // A list-rung row has no status to count; it is in All and in no tab.
    scoped.forEach((a) => { if (a.status) c[a.status] = (c[a.status] || 0) + 1; });
    return c;
  }, [scoped]);

  // Derived from the apps already loaded — see lib/app-facets. Each facet
  // renders nothing below two options, so an install that runs everything one
  // way gets no mode filter: a filter offering one value narrows nothing.
  //
  // The column is gated separately and asymmetrically — see anyIsolated. Shared
  // is the default every install has, so a column saying it about every row is
  // noise; isolated is the notable state, and an install where everything is
  // isolated needs the column most.
  const tagOptions = useMemo(() => tagFacetOptions(apps), [apps]);
  const modeOptions = useMemo(() => modeFacetOptions(apps), [apps]);
  const versionOptions = useMemo(() => versionFacetOptions(apps), [apps]);
  const ownerOptions = useMemo(() => ownerFacetOptions(apps, groups), [apps, groups]);
  const showMode = useMemo(() => anyIsolated(apps), [apps]);
  // Absent while nothing is locked, like every other facet on this row: a
  // filter for a state the estate is not in is a control to be read and
  // ignored.
  // Two options rather than one, because a lock the platform placed is a
  // different row state from a lock somebody decided: nobody was present, the
  // owner was told rather than asked, and the way back is a waiver rather than
  // an unlock. The second only appears where the estate actually has one.
  const lockedOptions: FacetOption[] = !isAdmin ? [] : [
    ...(apps.some((a) => a.locked_at) ? [{ key: "locked", label: "Locked" }] : []),
    ...(apps.some((a) => a.locked_at && a.locked_source === "dependency")
      ? [{ key: "locked:dependency", label: "Locked by policy" }]
      : []),
  ];
  // The scope renders only where the two halves differ — the rule every facet
  // beside it follows. On an install where the rung is off, or where you can
  // open everything you can see, there is nothing to choose between and the
  // toolbar keeps the shape it has always had.
  const showScope = useMemo(() => apps.some((a) => !a.mine), [apps]);

  // The empty state's third reader, and the one the default creates. On an
  // install listing the whole estate, somebody who has been given nothing lands
  // on `mine`, sees an empty table, and would otherwise be told "Deploy your
  // first app to get started" — a wrong diagnosis and an action that refuses
  // half of them, which is the exact failure the two readers below were split
  // apart to fix. Here the list is empty because they are looking at their half
  // of it, and the other half is one button away.
  const elsewhereInAll = showScope && scope !== "all";
  // How many apps are on the other side of the scope, and how many of them the
  // current search and facets would have found. Both are counted rather than
  // asserted: the sentence used to say the install had `apps.length` apps "you
  // can see the name of but not open", which is wrong twice — that is the
  // whole install rather than the not-mine half, and *not mine* is not *not
  // openable*, so it told people they could not open a public app they can.
  const notMine = useMemo(() => apps.filter((a) => !a.mine).length, [apps]);
  const matchesElsewhere = elsewhereInAll ? unscoped.length - scoped.length : 0;

  function chipsFor(
    options: FacetOption[],
    value: string[],
    setValue: (next: string[]) => void,
    prefix: string,
  ): FacetChip[] {
    // **A key with no option still gets a chip.** These come from the URL, and
    // a link naming a runtime this install no longer has — or a tag value
    // somebody renamed — narrowed the list to nothing and then rendered no
    // chip for the thing doing the narrowing. An empty table with a Clear that
    // is not there is a page that looks broken.
    return value.map((key: string) => {
      const option = options.find((o) => o.key === key) ?? {
        key,
        label: key.includes(":") ? key.split(":").slice(1).join(":") : key,
        prefix,
      };
      return { option, onRemove: () => setValue(value.filter((k) => k !== key)) };
    });
  }

  const chips = [
    ...chipsFor(modeOptions, modeFilters, setModeFilters, "Mode:"),
    ...chipsFor(versionOptions, versionFilters, setVersionFilters, "Runtime:"),
    ...chipsFor(tagOptions, tagFilters, setTagFilters, "Tag:"),
    ...chipsFor(ownerOptions, ownerFilters, setOwnerFilters, "Owner:"),
    ...(isAdmin ? chipsFor(lockedOptions, lockedFilters, setLockedFilters, "") : []),
    // The arrived-at narrowing gets a chip like every other one: a list
    // narrowed by something the page has no control for, with nothing on
    // screen saying so, is a list that looks broken.
    ...(isAdmin && kind
      ? [{
          option: {
            key: kind,
            label: APP_KIND_LABEL[kind as keyof typeof APP_KIND_LABEL] ?? kind,
            prefix: "Kind:",
          },
          onRemove: () => setKind(""),
        }]
      : []),
    ...(ownerGroup
      ? [{
          option: { key: ownerGroup, label: ownerGroupName ?? "this group", prefix: "Owner:" },
          onRemove: () => setOwnerGroup(""),
        }]
      : []),
  ];
  const narrowed = Boolean(search) || statusFilter !== "all" ||
    tagFilters.length > 0 || modeFilters.length > 0 || versionFilters.length > 0 ||
    (isAdmin && lockedFilters.length > 0) || ownerFilters.length > 0 || Boolean(ownerGroup) ||
    (isAdmin && Boolean(kind));

  /** Every facet on the row above the table, in one act — and the facet row's
   *  own Clear calls exactly this. It used to hold its own list of three
   *  setters beside a chip row built from four, so Clear left the Availability
   *  chip standing and, with one chip left, took its own link away with it. */
  function clearFacets() {
    setModeFilters([]);
    setVersionFilters([]);
    setTagFilters([]);
    setLockedFilters([]);
    setOwnerFilters([]);
    setOwnerGroup("");
    setKind("");
  }

  /** Everything the page is narrowed by, which is the facets plus the two
   *  controls that are not facets: the search box and the status tab. */
  function clearNarrowing() {
    setSearch("");
    setStatusFilter("all");
    clearFacets();
    // Not the scope: see where it is declared. Clearing reveals rows
    // everywhere else on this page, and dropping `all` back to `mine` would
    // be the one clear that hides them.
  }

  function toggleSort(key: SortColumn) {
    setSort((prev) => nextSort(prev, key));
  }

  // The server's own `detail` is carried where it sent one. A 202 start says
  // "this can take a minute in isolated mode, while the pod is scheduled and
  // its image pulled" — the one sentence that explains why nothing has
  // visibly happened — and this threw it away for the word "Restarting".
  async function runAction(
    started: string,
    failed: string,
    fn: () => Promise<{ detail?: string } | unknown>,
  ) {
    try {
      const res = await fn();
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      const detail =
        res && typeof res === "object" && "detail" in res
          ? (res as { detail?: string }).detail
          : undefined;
      toast.success(started, detail ? { description: detail } : undefined);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : failed);
    }
  }

  function copyAppUrl(app: AppAddress) {
    void copyToClipboard(appShareUrl(app), "URL");
  }

  return (
    <div>
      <PageHeader
        title="Apps"
        // No description: the title says what the page is, and the status
        // strip says what each app is doing. A subtitle restating both was
        // the first line anybody read and the one that told them nothing.
        actions={
          <Link to="/apps/new" className={buttonVariants({ size: "sm" })}>
            <Plus className="mr-1.5 h-4 w-4" />
            New app
          </Link>
        }
      />

      <div className="space-y-4">
        {/* Two idioms, and the split between them is the whole design. Status
            is the one primary axis — it carries counts and "Crashed (2)" is a
            single click during an incident — so it keeps the tab bar below.
            Every *secondary* facet is a popover in this row instead, sharing
            one shape, one count badge and one Clear, so adding the next one is
            a call rather than a component. */}
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            {/* The page's scope, ahead of the search box because it decides
                what the search is searching. A segmented control rather than a
                second strip of tabs: the status axis below already is one, and
                two tab bars stacked would claim they are peers when one of them
                decides what the other counts. */}
            {showScope && (
              <div
                role="group"
                aria-label="Which apps"
                className="inline-flex shrink-0 items-center gap-0.5 rounded-md border border-border/60 bg-muted/50 p-0.5"
              >
                {SCOPES.map((opt) => (
                  <button
                    key={opt.value}
                    type="button"
                    aria-pressed={scope === opt.value}
                    onClick={() => setScope(opt.value)}
                    className={`rounded px-2.5 py-1 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring ${
                      scope === opt.value
                        ? "bg-card text-foreground shadow-sm"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {opt.label}
                  </button>
                ))}
              </div>
            )}
            <SearchInput
              placeholder="Search apps…"
              value={search}
              onChange={setSearch}
              className="w-full sm:max-w-sm"
            />
            <div className="flex flex-wrap items-center gap-2">
              <FacetFilter
                label="Mode"
                icon={<Boxes />}
                options={modeOptions}
                value={modeFilters}
                onChange={setModeFilters}
                placeholder="Filter by mode…"
                empty="No matching modes."
              />
              {/* Administrators only, and it needs no check here: the listing
                  carries no runtime for anybody else, so this renders nothing.
                  See versionFacetOptions. */}
              <FacetFilter
                label="Runtime"
                icon={<LanguagesIcon />}
                options={versionOptions}
                value={versionFilters}
                onChange={setVersionFilters}
                placeholder="Filter by version…"
                empty="No matching versions."
              />
              {/* Administrators only: lockedOptions is empty for anybody else.
                  One option, because there is one thing to say: locked apps
                  are a subset of the list rather than another axis of it —
                  which is exactly what `predicate` declares. Without it the
                  two-option floor hid this facet on every install where the
                  dependency policy had not *also* locked something, so there
                  was no way to filter for a locked app at all. */}
              <FacetFilter
                label="Availability"
                predicate
                icon={<LockKeyhole />}
                options={lockedOptions}
                value={lockedFilters}
                onChange={setLockedFilters}
                placeholder="Filter by availability…"
                empty="Nothing to filter."
              />
              <FacetFilter
                label="Owner"
                icon={<UserRound />}
                options={ownerOptions}
                value={ownerFilters}
                onChange={setOwnerFilters}
                placeholder="Filter by owner…"
                empty="No matching owners."
              />
              <FacetFilter
                label="Tags"
                icon={<TagIcon />}
                options={tagOptions}
                value={tagFilters}
                onChange={setTagFilters}
                placeholder="Filter by tag…"
                empty="No matching tags."
              />
            </div>
            {/* Only when the controls have taken something away. `apps` is
                everything listable and `scoped` what survived the scope, the
                search and the facets — a number no other line on the page
                carries, since the pagination footer counts what the status
                strip has narrowed to and not what it started from. */}
            <div className="ml-auto flex shrink-0 items-center gap-3">
              {!isLoading && scoped.length !== apps.length && (
                <p className="text-sm text-muted-foreground">
                  {scoped.length} of {apps.length} app{apps.length !== 1 ? "s" : ""}
                </p>
              )}
              {/* In the table's own toolbar, where People keeps its export,
                  rather than beside New app: two buttons in the page header
                  read as two equal ways in, and on a phone this one wrapped
                  above the search. It exports what the controls left, which is
                  why it sits with them. Disabled while there is nothing to
                  write — a spreadsheet with a header row and no rows is worse
                  than no button. */}
              <Button
                variant="ghost"
                size="sm"
                disabled={exporting || isLoading || filtered.length === 0}
                onClick={exportApps}
              >
                <Download className="mr-1.5 h-3.5 w-3.5" />
                {exporting ? "Exporting…" : "Export to Excel"}
              </Button>
            </div>
          </div>
          <FacetChips
            chips={chips}
            onClear={clearFacets}
          />
        </div>

        <FilterBar
          options={statusFilters
            .filter((f) => ALWAYS_SHOWN.has(f.value) || f.value === statusFilter || (counts[f.value] ?? 0) > 0)
            .map((f) => ({
            ...f,
            // Always a count, including zero: "Crashed (0)" is the answer
            // somebody opening this page during an incident came for. The
            // transient states only appear when something is in them.
            label: `${f.label} (${counts[f.value] ?? 0})`,
          }))}
          value={statusFilter}
          onChange={setStatusFilter}
        />

        {/* Before the empty state, never after it: "No apps found — deploy
            your first app to get started" is a lie when the list simply did
            not arrive, and it is a lie told at the worst possible moment. */}
        {error ? (
          <QueryError
            title="Your apps could not be read"
            error={error}
            fallback="Reload the page. If it keeps failing, tell an administrator."
            onRetry={() => void refetch()}
          />
        ) : isLoading ? (
          <TableCard className="p-4 space-y-3">
            {Array.from({ length: 4 }).map((_, i) => (
              <div key={i} className="flex items-center gap-4">
                <Skeleton className="h-9 w-9 rounded-lg" />
                <div className="flex-1 space-y-1.5">
                  <Skeleton className="h-4 w-48" />
                  <Skeleton className="h-3 w-32" />
                </div>
                <Skeleton className="h-5 w-16 rounded-full" />
                <Skeleton className="h-4 w-40" />
                <Skeleton className="h-4 w-24" />
              </div>
            ))}
          </TableCard>
        ) : filtered.length === 0 ? (
          <EmptyState
            // Two readers, two reasons the list is empty. A viewer's list is
            // empty because nobody has shared an app, not because they have not
            // deployed one — and "Deploy your first app" handed them a
            // diagnosis that was wrong and an action that refuses them. Guarded
            // on `caps &&` so a publisher never reads the viewer sentence for
            // the one round trip before the answer arrives.
            title={
              narrowed && matchesElsewhere > 0
                ? "None of your apps match"
                : narrowed
                  ? "No apps match your filters"
                  : elsewhereInAll
                    ? "You don’t have any apps yet"
                    : caps && !caps.can_create_objects
                      ? "No apps shared with you yet"
                      : "No apps yet"
            }
            description={
              // A search on `mine` that would have matched elsewhere says so
              // and counts it, rather than asserting that nothing on the
              // install matches — which is what it said about a colleague's
              // app whose name was typed in full.
              narrowed && matchesElsewhere > 0
                ? `${matchesElsewhere} other app${matchesElsewhere === 1 ? "" : "s"} on this install match${matchesElsewhere === 1 ? "es" : ""}.`
                : narrowed
                  ? `None of the ${apps.length} app${apps.length === 1 ? "" : "s"} on this install match your search and filters.`
                  : elsewhereInAll
                    ? `This install has ${notMine} other app${notMine === 1 ? "" : "s"}.`
                    : caps && !caps.can_create_objects
                      ? "Apps appear here when somebody shares one with you."
                      : "Deploy from a Git repository or upload an archive."
            }
            // An empty state that names an action and does not offer it is
            // asking somebody to go and find the controls they have just been
            // looking at. "Try adjusting your filters" is now the button.
            action={
              // The way out, and for a search that would match elsewhere it is
              // the scope rather than the filters: `clearNarrowing` never
              // touches the scope (see where it is declared), so the only
              // control offered here used to clear the thing that was not the
              // problem and leave the thing that was.
              narrowed && matchesElsewhere > 0 ? (
                <Button variant="secondary" size="sm" onClick={() => setScope("all")}>
                  Search all apps
                </Button>
              ) : narrowed ? (
                <Button variant="secondary" size="sm" onClick={clearNarrowing}>
                  Clear search and filters
                </Button>
              ) : elsewhereInAll ? (
                <Button variant="secondary" size="sm" onClick={() => setScope("all")}>
                  Show all apps
                </Button>
              ) : (
                <Link to="/apps/new" className={buttonVariants({ size: "sm" })}>
                  <Plus className="mr-1.5 h-4 w-4" />
                  New app
                </Link>
              )
            }
          />
        ) : (
          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <SortableHead label="Name" sortKey="name" sort={sort} onSort={toggleSort} className="w-full" />
                  <SortableHead label="Status" sortKey="status" sort={sort} onSort={toggleSort} />
                  {/* Column priority, narrowest screen first. Name, Status and
                      the row's actions are never dropped — they are the reason
                      somebody opened this page. Framework and URL go first:
                      the slug is already under the name and Open is a button,
                      so neither column is the only way to that information.

                      Mode survives to `lg`, one step longer than Framework: an
                      isolated workload and a shared process fail differently
                      and cost differently, and the person reading this list on
                      a laptop mid-incident is the one who needs to know which
                      they are looking at. */}
                  {showMode && <TableHead className="hidden w-[120px] @3xl:table-cell">Mode</TableHead>}
                  <TableHead className="hidden w-[110px] @4xl:table-cell">Framework</TableHead>
                  {/* Owner is `md` rather than `lg`, one step earlier than
                      the rule above would put it. An estate people import into
                      in bulk is one where "whose is this" is the second
                      question after "is it up" — thirty apps arrived under one
                      account and the column that says so was the one the
                      viewport had dropped. */}
                  <TableHead className="hidden @xl:table-cell">Owner</TableHead>
                  {/* The serving release's date, not `updated_at`. That column
                      moves on every status transition — a crash, a doze, a
                      lock, a poller writing `last_seen_sha` — so sorting by it
                      during an incident returned what had most recently
                      *broken*. See `lastDeployedAtExpr` in
                      `internal/database/estate.go`. */}
                  <SortableHead label="Last deployed" sortKey="deployed" sort={sort} onSort={toggleSort} className="hidden w-[130px] @md:table-cell" />
                  <TableHead className="w-[124px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {paged.map((app) => (
                  <TableRow
                    key={app.id}
                    className="cursor-pointer"
                    onClick={() => navigate(`/apps/${app.id}`)}
                  >
                    {/* Capped below `sm`: the name is the one cell with no
                        width, so a long one pushed Open and the row menu past
                        the right edge of a phone. It truncates instead — the
                        row is a link to the page that spells it out. */}
                    {/* Name takes the leftover width and truncates in it
                        (`max-w-0` under a `w-full` head): it is the column
                        the page is for, and fixed at 280px it left the slack
                        to Owner. */}
                    <TableCell className="max-w-0">
                      <div className="flex min-w-0 items-center gap-3">
                        <AppIcon name={app.name} seed={app.slug} size="md" />
                        <div className="min-w-0">
                          {/* A real link keeps the row keyboard-reachable; the
                              row onClick is the mouse convenience. */}
                          <div className="flex min-w-0 items-center gap-1.5">
                            <Link
                              to={`/apps/${app.id}`}
                              onClick={(e) => e.stopPropagation()}
                              className="block truncate font-medium text-foreground hover:text-primary hover:underline"
                            >
                              {app.name}
                            </Link>
                            {/* One glyph, shown exactly when the app's
                                Dependencies badge is non-zero: red for malware
                                or critical, amber for anything else. The label
                                carries the meaning; the colour is never the
                                only carrier. */}
                            {app.dependency_alert && (() => {
                              const alert = app.dependency_alert;
                              const what = dependencyAlertWords(alert);
                              return (
                                <Link
                                  to={`/apps/${app.id}/dependencies`}
                                  onClick={(e) => e.stopPropagation()}
                                  aria-label={`${app.name}: ${what}`}
                                  title={what}
                                  className={cn(
                                    "inline-flex",
                                    toneText[
                                      alert.malware || alert.severity === "critical"
                                        ? "danger"
                                        : "attention"
                                    ],
                                  )}
                                >
                                  <PackageSearch className="size-3.5" />
                                </Link>
                              );
                            })()}
                          </div>
                          <p className="truncate text-xs text-muted-foreground">{app.slug}</p>
                          <TagChips
                            tags={app.tags?.filter((t) => t.kind === "custom")}
                            max={2}
                            link={false}
                            className="mt-1"
                          />
                        </div>
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        {/* A row held at the `list` rung carries no status:
                            the rung shows that an app exists, not whether it
                            is up (ADR §90). */}
                        {app.can_open ? (
                          <StatusBadge status={app.status} />
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                        {/* The row state itself, which was invisible here: a
                            locked app read as an ordinary Stopped row on the
                            page an owner opens when their app has gone dark.
                            `attention` because lib/status.ts already maps the
                            word to that tone. */}
                        {app.locked_at && (
                          <Badge
                            variant="outline"
                            className={`gap-1 ${toneSurface.attention}`}
                          >
                            <LockKeyhole className="h-3 w-3" aria-hidden />
                            Locked
                          </Badge>
                        )}
                        {/* Only for somebody who may open the tab: the log
                            endpoints are edit-gated, so on a viewer's row this
                            was a link to a page they cannot have. */}
                        {app.status === "crashed" && app.can_edit && (
                          <Link
                            to={`/apps/${app.id}/logs`}
                            onClick={(e) => e.stopPropagation()}
                            className="inline-flex items-center gap-1 text-xs text-destructive hover:underline"
                          >
                            <ScrollText className="h-3 w-3" />
                            Logs
                          </Link>
                        )}
                      </div>
                      {/* A build in flight, which the status word cannot say.
                          `status` is what the app *is* — an app whose last
                          build failed is still failed while the next one runs,
                          because what a viewer would reach is still not there
                          — so a six-minute rebuild read as a plain "Failed"
                          row under a strip saying "Building (0)". The strip is
                          a pure partition of AppStatus and may not gain a
                          word; this is the second fact beside the first, on
                          `ModePending`'s terms and in its shape.

                          Suppressed while the badge already says it: a first
                          build puts the app itself in `building`, and two
                          controls saying the same thing is how one of them
                          stops being read. */}
                      {app.deploying && app.status !== "building" && (
                        <span className="mt-1 block text-2xs text-tone-attention-fg">
                          {app.deploying === "pending"
                            ? "queued to rebuild"
                            : "rebuilding now"}
                        </span>
                      )}
                    </TableCell>
                    {/* The mode the release that is *serving* was started
                        under, never the setting — an app promoted this morning
                        and not yet redeployed is still a shared process, and
                        the pending line below says so rather than the badge
                        quietly telling somebody the wrong thing. */}
                    {showMode && (
                      <TableCell className="hidden @3xl:table-cell">
                        {/* Nor a mode: without one it would read as shared. */}
                        {app.can_open && (
                          <>
                            <ModeBadge mode={runningMode(app)} />
                            <ModePending mode={pendingMode(app)} />
                          </>
                        )}
                      </TableCell>
                    )}
                    <TableCell className="hidden @4xl:table-cell">
                      <FrameworkBadge framework={app.framework} link={false} />
                    </TableCell>
                    {/* `AppOwner`, not a login and an avatar: an app is owned
                        by a user *or a group*, and a group has no `owner` on
                        the row — so the hand-rolled cell this replaces drew an
                        empty avatar beside a blank name for every group-owned
                        app. One component renders either, which is the whole
                        reason it exists. */}
                    <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                      <AppOwner
                        ownerId={app.owner_id}
                        ownerGroupId={app.owner_group_id}
                        ownerLogin={app.owner?.login}
                        withCount={false}
                      />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @md:table-cell">
                      {/* Empty, never a date, for an app that has never
                          shipped — `updated_at` gave one, so a locked app that
                          had never deployed read "Last deployed: 3d ago". */}
                      {app.last_deployed_at ? (
                        <RelativeTime iso={app.last_deployed_at} />
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center justify-end gap-1">
                        {/* Opening the app is the most common thing anyone
                            wants from this list, and a row's primary action
                            should not be the hardest one to find. A list-rung
                            row offers the ask instead; a batch app nothing.
                            See AppOpenAction. */}
                        <AppOpenAction app={app} />
                        <DropdownMenu>
                          <DropdownMenuTrigger
                          render={
                            <Button
                              variant="ghost"
                              size="icon-xs"
                              aria-label={`Actions for ${app.name}`}
                              onClick={(e) => e.stopPropagation()}
                            >
                              <MoreHorizontal className="h-4 w-4" />
                            </Button>
                          }
                        />
                        {/* Only what this caller can actually do, and only
                            what makes sense for the state the row is in. The
                            menu offered all four on every readable app and the
                            server refused each one for a viewer — four controls
                            that exist to fail, on every row of somebody's main
                            page. `can_edit` and `can_delete` come from the
                            server's own `Authorize`, so this is not a second
                            definition of who may act. */}
                        <DropdownMenuContent align="end">
                          {!isBatchFramework(app.framework) && (
                            <DropdownMenuItem onClick={(e) => { e.stopPropagation(); copyAppUrl(app); }}>
                              <Copy className="mr-2 h-3.5 w-3.5" /> Copy link
                            </DropdownMenuItem>
                          )}
                          {app.can_edit && (
                            <>
                              {/* Disabled rather than hidden: a vanished
                                  action reads as "you may not", and the Locked
                                  badge in the same row is the reason carrier
                                  the skill asks for instead of a `title`. */}
                              <DropdownMenuItem disabled={!!app.locked_at} onClick={(e) => { e.stopPropagation(); runAction(`Redeploying “${app.name}”`, `Couldn’t redeploy “${app.name}”`, () => api.deploy(app.id)); }}>
                                <RotateCcw className="mr-2 h-3.5 w-3.5" /> Redeploy
                              </DropdownMenuItem>
                              {!isBatchFramework(app.framework) && (
                              <DropdownMenuItem disabled={!!app.locked_at} onClick={(e) => { e.stopPropagation(); runAction(`Restarting “${app.name}”`, `Couldn’t restart “${app.name}”`, () => api.restart(app.id)); }}>
                                <Play className="mr-2 h-3.5 w-3.5" />
                                {app.status === "stopped" || app.status === "sleeping" ? "Start" : "Restart"}
                              </DropdownMenuItem>
                              )}
                              {/* Nothing to stop on an app that is already
                                  down: the verb above already says Start. */}
                              {app.status !== "stopped" && !isBatchFramework(app.framework) && (
                                <DropdownMenuItem
                              disabled={!!app.locked_at}
                              onClick={(e) => {
                                e.stopPropagation();
                                // Whose users notice is a property of the app,
                                // not of the page. An administrator could take
                                // any app on the install down from here with
                                // one click, and got a dialog for the identical
                                // act on Processes.
                                if (stopNeedsConfirm(app.owner_id, user?.id)) setToStop(app);
                                else runAction(`Stopping “${app.name}”`, `Couldn’t stop “${app.name}”`, () => api.stop(app.id));
                              }}
                            >
                                  <Square className="mr-2 h-3.5 w-3.5" /> Stop
                                </DropdownMenuItem>
                              )}
                            </>
                          )}
                          {app.can_edit && app.can_delete && <DropdownMenuSeparator />}
                          {app.can_delete && (
                            <DropdownMenuItem variant="destructive" onClick={(e) => { e.stopPropagation(); setAppToDelete(app); }}>
                              <Trash2 className="mr-2 h-3.5 w-3.5" /> Delete…
                            </DropdownMenuItem>
                          )}
                          {/* Only when the menu would otherwise be empty — a
                              batch app has no link to copy. Beside Copy link
                              it contradicted the item above it. */}
                          {isBatchFramework(app.framework) && !app.can_edit && !app.can_delete && (
                            <DropdownMenuItem disabled>
                              No actions available to you
                            </DropdownMenuItem>
                          )}
                        </DropdownMenuContent>
                        </DropdownMenu>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableCard>
        )}
        {!isLoading && filtered.length > 0 && (
          <TablePagination
            total={filtered.length}
            page={currentPage}
            pageSize={pageSize}
            onPage={setPage}
            onPageSize={setPageSize}
          />
        )}
      </div>

      <StopAppDialog
        appId={toStop?.id ?? ""}
        appName={toStop?.name ?? ""}
        open={toStop !== null}
        onOpenChange={() => setToStop(null)}
        onStopped={() => queryClient.invalidateQueries({ queryKey: ["apps"] })}
      />

      {appToDelete && (
        <DeleteAppDialog
          app={appToDelete}
          open={!!appToDelete}
          onOpenChange={(o) => { if (!o) setAppToDelete(null); }}
        />
      )}
    </div>
  );
}

/** The glyph's one sentence, most urgent first — so an app that is both
 *  refused and critical says the thing its owner has to act on, once. */
function dependencyAlertWords(
  alert: NonNullable<AppWithMetrics["dependency_alert"]>,
): string {
  if (alert.malware) return "A package in the release that is serving is reported as malicious";
  if (alert.blocking) return "The next deploy will be refused by the dependency policy";
  return `The release that is serving has a ${alert.severity ?? "known"} advisory`;
}

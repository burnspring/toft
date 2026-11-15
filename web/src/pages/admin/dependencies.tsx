import { useState } from "react";
import { isHttpUrl } from "@/lib/http-url";
import { RelativeTime } from "@/components/relative-time";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { History, MoreHorizontal, PackageSearch, RefreshCw } from "lucide-react";

import {
  api,
  type AdminDependencyApp,
  type AdminDependencyState,
  type AdvisoryGroup,
  type DependencyFinding,
  type DependencyLockOn,
  type DependencyWaiver,
  type PackageOccurrence,
  type PackageSearchResult,
  type PlatformSettings,
} from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { EmptyState } from "@/components/empty-state";
import { Disclosure } from "@/components/disclosure";
import { LockAppDialog } from "@/components/lock-app-dialog";
import { WaiverDialog } from "@/components/dependencies-tab";
import { FilterBar, FilterChips } from "@/components/filter-bar";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { SearchInput } from "@/components/search-input";
import { Section, SectionHeading } from "@/components/section";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { ChoiceButtons } from "@/components/choice-buttons";
import { Code } from "@/components/code";
import { SortableHead } from "@/components/sortable-head";
import { nextSort, type SortState } from "@/lib/sort";
import { statusMeaning } from "@/lib/status";
import { SettingRow } from "@/components/setting-row";
import { PendingBar } from "@/components/pending-bar";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { TableCard } from "@/components/table-card";
import { UnreadableCredentialNotice } from "@/components/unreadable-credential";
import { FieldError, FieldHelp } from "@/components/field-help";
import { credentialPlaceholder } from "@/lib/credentials";
import { formatDateTime } from "@/lib/time";
import { toneSurface, toneText, type Tone } from "@/lib/tone";
import { cn, fieldErrorId } from "@/lib/utils";

/**
 * Admin → Dependencies.
 *
 * One model, said in the same words everywhere on it: each app **records** its
 * packages, they are **checked** against advisories, and a finding at or above
 * the **threshold** is one the policy acts on — it **refuses** the next deploy
 * under Refuse, and can **lock** a serving app. A **waiver** excuses one finding
 * for a while.
 *
 * The page runs in the order an administrator arrives: what *needs a decision*
 * (the one list that is empty when they are done), then the search box —
 * *we just read about GHSA-xxxx, which of our forty internal tools has it* —
 * then the estate as reference, then the settings.
 *
 * **By advisory is the default axis, and by app is the toggle.** By app answers
 * "how bad is this app"; by advisory answers "how bad is this news", which is
 * the question an administrator actually arrives with — and the by-app table is
 * already forty rows on day one while the thing being triaged is usually one
 * advisory.
 */

const SEVERITY_TONE: Record<string, Tone> = {
  malware: "danger",
  critical: "danger",
  high: "attention",
  moderate: "attention",
  low: "neutral",
  unknown: "neutral",
};

/**
 * The four ways to narrow the by-app table.
 *
 * **Defined once because three of them are also numbers in the row of stats
 * above the table**, and a card reading 3 that opens a list of 5 is worse than
 * no card at all. The predicates are the single definition; the stats and the
 * chips both read them.
 *
 * `unscanned` is the one that has to exist: "not yet scanned" is a row state,
 * and a row state without a matching list filter is a fact an administrator
 * can only find by reading every row. On forty apps that is an annoyance; the
 * estate this feature is for is not forty apps.
 */
const APP_FILTERS: {
  value: string;
  label: string;
  /** The heading when this filter matches nothing. Written per filter rather
   *  than built from the label, because "No apps are with findings" is what
   *  generating it produces. */
  empty: string;
  match: (a: AdminDependencyApp) => boolean;
}[] = [
  { value: "all", label: "All", empty: "No apps", match: () => true },
  {
    // The headline count's own definition: a stopped app is not exposure.
    value: "exposed",
    label: "At or above the threshold",
    empty: "No serving app is at or above the threshold",
    match: (a) => a.scanned && a.serving && a.counts.counted > 0,
  },
  {
    // Everything the corpus matched, threshold or not — which is nearly every
    // Node app, and is why it is not the headline.
    value: "findings",
    label: "Any finding",
    empty: "No app has a finding of any kind",
    match: (a) => a.scanned && a.counts.total > 0,
  },
  {
    // **Narrowed by `packages`, because "not scanned" stopped being one fact.**
    // An app with nothing recorded records its own on the next deploy; an app
    // whose whole list is R has already recorded it and a redeploy changes
    // nothing. Folding them together puts "each records its own on the next
    // deploy" over a set of apps that already did.
    value: "unscanned",
    label: "Not recorded yet",
    empty: "Every app has recorded its packages",
    match: (a) => !a.scanned && a.packages === 0,
  },
  {
    // The fifth, by the same argument as the fourth: a row that reads
    // "recorded, not checked" is a state, and a state with no filter is
    // something an administrator can only find by reading every row.
    value: "recorded",
    label: "Can’t be checked",
    empty: "No app has packages in an ecosystem nothing checks",
    match: (a) => !a.scanned && a.packages > 0,
  },
  {
    // The sixth, and the one that is a *decision* rather than a reading: these
    // apps were taken off the air by the policy itself, with nobody present.
    // A new row state gets its matching list filter in the same change.
    value: "locked",
    label: "Locked by policy",
    empty: "This policy has not locked anything",
    match: (a) => !!a.locked_at && a.locked_source === "dependency",
  },
];

/** One filter's predicate, falling back to "all" for an unknown value. */
function appFilterMatch(value: string): (a: AdminDependencyApp) => boolean {
  return (APP_FILTERS.find((f) => f.value === value) ?? APP_FILTERS[0]).match;
}

function severityKey(f: DependencyFinding): string {
  return f.kind === "malware" ? "malware" : f.severity || "unknown";
}

function severityLabel(key: string): string {
  if (key === "malware") return "Malicious";
  if (key === "unknown") return "Unscored";
  return key.charAt(0).toUpperCase() + key.slice(1);
}

/** Within a week. Kept beside expiresIn so the two time judgements this page
 *  makes live together — and out of the render body, which is where reading
 *  the clock is a lint error and, more to the point, a value that will not
 *  update on its own. Both move once a day, which is the resolution a waiver
 *  needs. */
function expiresSoon(iso: string): boolean {
  return new Date(iso).getTime() - Date.now() < 7 * 86_400_000;
}

function expiresIn(iso: string): string {
  const ms = new Date(iso).getTime() - Date.now();
  if (ms <= 0) return "expired";
  const days = Math.ceil(ms / 86_400_000);
  if (days === 1) return "expires tomorrow";
  if (days < 45) return `expires in ${days} days`;
  return `expires ${formatDateTime(iso)}`;
}

export function AdminDependenciesPage() {
  const queryClient = useQueryClient();
  // In the URL: "which of our apps has GHSA-…?" is a link somebody sends.
  const [axis, setAxis] = useUrlParam("view", "advisory");
  const [appFilter, setAppFilter] = useUrlParam("apps", "all");
  const [query, setQuery] = useUrlParam("q");
  const [, setParams] = useSearchParams();

  // A stat card is a way into the list it counts. Both halves move together:
  // switching the axis without setting the filter would land somebody on the
  // full table, which is the thing they clicked a number to avoid. One write,
  // because two setters each read the URL they were rendered with.
  const showApps = (filter: string) =>
    setParams((prev) => {
      const out = new URLSearchParams(prev);
      out.set("view", "app");
      if (filter === "all") out.delete("apps");
      else out.set("apps", filter);
      return out;
    }, { replace: true });

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["admin-dependencies"],
    queryFn: () => api.getAdminDependencies(),
  });
  // `/admin/dependencies#policy` from ⌘K, once the page has something to
  // scroll to.
  useHashScroll(!!data);

  const refresh = useMutation({
    mutationFn: async () => {
      // One button, two steps: "refresh the data" and "re-answer the question"
      // are one intent, and one label — *Check now* — for both. When the corpus was carried in by hand there is nothing
      // to fetch, so only the second half runs.
      if (data?.can_sync) await api.syncAdvisoryFeed();
      else await api.recheckAllDependencies();
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["admin-dependencies"] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      toast.success(
        data?.can_sync
          ? "Advisory data refreshed and every app checked"
          : "Every app checked",
      );
    },
    onError: (e: Error) => toast.error(e.message || "Couldn’t check"),
  });

  if (error) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Dependencies"
          description="Packages your apps install, checked against published advisories."
        />
        <QueryError
          title="The dependency estate could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }
  if (isLoading || !data) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Dependencies"
          description="Packages your apps install, checked against published advisories."
        />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }

  if (!data.available) {
    // Off. Every install is entitled to the policy (ADR §32, *Toft is free*),
    // so unavailable means the mode is off and nothing else. **Settings turns
    // it on, this page configures how it behaves**, so the mode is the one
    // control shown.
    return (
      <div className="space-y-6">
        <PageHeader
          title="Dependencies"
          description="Packages your apps install, checked against published advisories."
        />
        <Notice tone="neutral" size="md" title="Dependency policy is off">
          Nothing is downloaded or checked. Start with{" "}
          <strong className="font-medium">Warn</strong> below: it records
          findings and fails no build.
        </Notice>
        {/* The mode renders here too. It used to live only in the configured
            branch, so choosing Off removed the only control that could set it
            back. */}
        <DependencyModeSection />
      </div>
    );
  }

  const decisions = needsDecision(data);
  // Through APP_FILTERS, so each number is the length of exactly the list its
  // link opens.
  const notRecorded = data.apps.filter(appFilterMatch("unscanned")).length;
  const cannotCheck = data.apps.filter(appFilterMatch("recorded")).length;
  const appsWhere = (p: (d: Decision) => boolean) =>
    new Set(decisions.filter(p).map((d) => d.app_id)).size;
  // Malware refuses under Warn too (deps.Policy.Refuses), so the two differ
  // only while the mode is Warn.
  const refusedNow = appsWhere(
    (d) => d.would_block && (data.enforcing || d.kind === "malware"),
  );
  const refusedUnderRefuse = appsWhere((d) => d.would_block);
  const feedProblem =
    data.feeds.length === 0 || data.feed_stale || data.feeds.some((f) => !!f.last_error);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Dependencies"
        description="Packages your apps install, checked against published advisories."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => refresh.mutate()}
            disabled={refresh.isPending}
          >
            <RefreshCw
              className={cn("size-3.5", refresh.isPending && "animate-spin")}
            />
            {refresh.isPending ? "Checking…" : "Check now"}
          </Button>
        }
      />

      {/* The advisory data's own section is near the foot; only a problem
          with it is said up here, because every answer below depends on it. */}
      {feedProblem && (
        <Notice
          tone="attention"
          title={
            data.feeds.length === 0
              ? "No advisory data has loaded yet"
              : "The advisory data needs attention"
          }
          action={
            <a
              href="#advisory-data"
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              Show
            </a>
          }
        >
          Until it is current, the lists below may be behind or empty.
        </Notice>
      )}

      {/* **What needs a decision leads.** Everything else on the page is
          reference; this is the one list that is empty when the administrator
          is done. */}
      <NeedsDecision
        decisions={decisions}
        enforcing={data.enforcing}
        notRecorded={notRecorded}
        cannotCheck={cannotCheck}
        onShowApps={showApps}
      />

      {/* It reads stored inventory and reaches no registry and no feed, so it
          works with the watch off, with the data stale, and under Warn. */}
      <AdvisorySearch query={query} setQuery={setQuery} />

      {query.trim() === "" ? (
        <>
          <div className="space-y-3">
            <SectionHeading title="Across the estate" />
            {/* **No counts, and that is the difference between an axis and a
                filter.** These three are three ways of looking, and "By
                advisory (37)" beside "By app (12)" would count different
                nouns. */}
            <FilterBar
              options={[
                { label: "By advisory", value: "advisory" },
                { label: "By app", value: "app" },
                { label: "Package catalog", value: "catalog" },
              ]}
              value={axis}
              onChange={setAxis}
            />
            {axis === "advisory" ? (
              <ByAdvisory groups={data.advisories} />
            ) : axis === "app" ? (
              <ByApp
                apps={data.apps}
                enforcing={data.enforcing}
                filter={appFilter}
                setFilter={setAppFilter}
              />
            ) : (
              // Picking a package types it into the box above, which is where
              // the two-section answer for that name already lives.
              <PackageCatalog onPick={setQuery} />
            )}
          </div>

          {data.waivers.length > 0 && (
            <AllWaivers waivers={data.waivers} enforcing={data.enforcing} />
          )}
        </>
      ) : (
        <p className="text-xs text-muted-foreground">
          Clear the search to see the estate and waivers.
        </p>
      )}

      <FeedStatus data={data} />

      {/* Configuration last. It is outside the search guard: this page's
          indexed settings live here, and a ⌘K jump to one of them cannot
          depend on what is typed in a box above it. */}
      <PolicyControls
        credentialSet={data.credential_set}
        credentialUnreadable={data.credential_unreadable}
        refusedNow={refusedNow}
        refusedUnderRefuse={refusedUnderRefuse}
        enforcing={data.enforcing}
      />
    </div>
  );
}

/** One (app, advisory) pair waiting on an administrator. */
type Decision = AdvisoryGroup["apps"][number] & {
  advisory_id: string;
  kind: AdvisoryGroup["kind"];
  severity: AdvisoryGroup["severity"];
  summary?: string;
  url?: string;
  locked_by_policy: boolean;
  owner_name?: string;
};

/**
 * The pairs that need a decision: at or above the threshold, unwaived, on an
 * app that is serving or that the policy itself locked. A stopped app is not
 * exposure, and an app an administrator locked has had its decision.
 *
 * Built from the advisory axis the page already holds, in its order — malware
 * first, then reach, then severity — so the two lists cannot disagree.
 */
function needsDecision(data: AdminDependencyState): Decision[] {
  const apps = new Map(data.apps.map((a) => [a.app_id, a]));
  const out: Decision[] = [];
  for (const g of data.advisories) {
    for (const a of g.apps) {
      if (!a.counted) continue;
      const app = apps.get(a.app_id);
      const lockedByPolicy = !!app?.locked_at && app.locked_source === "dependency";
      if (app?.locked_at && !lockedByPolicy) continue;
      if (!a.serving && !lockedByPolicy) continue;
      out.push({
        ...a,
        advisory_id: g.advisory_id,
        kind: g.kind,
        severity: g.severity,
        summary: g.summary,
        url: g.url,
        locked_by_policy: lockedByPolicy,
        owner_name: app?.owner_name,
      });
    }
  }
  return out;
}

/**
 * *Needs a decision* — the page's lead.
 *
 * Each row ends one of three ways: the owner upgrades (the fix is on the row,
 * and the app's name opens the tab that says what to change), an
 * administrator waives it, or an administrator locks the app. Waiving or
 * upgrading takes it off the list; locking takes the app off the air.
 */
function NeedsDecision({
  decisions,
  enforcing,
  notRecorded,
  cannotCheck,
  onShowApps,
}: {
  decisions: Decision[];
  enforcing: boolean;
  notRecorded: number;
  cannotCheck: number;
  onShowApps: (filter: string) => void;
}) {
  const [waiving, setWaiving] = useState<Decision | null>(null);
  const [locking, setLocking] = useState<Decision | null>(null);
  const queryClient = useQueryClient();
  const { data: settings } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: () => api.getPlatformSettings(),
  });
  const appCount = new Set(decisions.map((d) => d.app_id)).size;

  return (
    <Section
      id="decide"
      title="Needs a decision"
      description={
        decisions.length === 0
          ? undefined
          : `${decisions.length} finding${decisions.length === 1 ? "" : "s"} at or above the threshold, on ${appCount} app${appCount === 1 ? "" : "s"}. Upgrade, waive or lock each.`
      }
    >
      {decisions.length === 0 ? (
        <EmptyState
          icon={<PackageSearch className="size-6" />}
          title="Nothing needs a decision"
          description="No serving app has an unwaived finding at or above the threshold."
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>App</TableHead>
              <TableHead className="w-full">Finding</TableHead>
              <TableHead>Severity</TableHead>
              <TableHead className="hidden @3xl:table-cell">Fix</TableHead>
              <TableHead className="w-[1%]">
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {decisions.map((d) => {
              const key = d.kind === "malware" ? "malware" : d.severity || "unknown";
              const refuses = d.would_block && (enforcing || d.kind === "malware");
              const fix = d.fixed_in ? `Upgrade to ${d.fixed_in}` : "No fix published";
              return (
                <TableRow key={`${d.app_id}-${d.advisory_id}-${d.package}-${d.version}`} className="align-top">
                  <TableCell>
                    <Link
                      to={`/apps/${d.app_id}/dependencies`}
                      className="font-medium text-foreground hover:text-primary hover:underline"
                    >
                      {d.name || d.slug}
                    </Link>
                    <p className="text-xs text-muted-foreground">
                      {d.owner_name || d.slug}
                    </p>
                    {d.locked_by_policy && (
                      <Badge variant="outline" className={cn("mt-1", toneSurface.danger)}>
                        Locked by policy
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="max-w-0">
                    {isHttpUrl(d.url) ? (
                      <a
                        href={d.url}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="font-mono text-sm text-foreground hover:text-primary hover:underline"
                      >
                        {d.advisory_id}
                      </a>
                    ) : (
                      <span className="font-mono text-sm text-foreground">{d.advisory_id}</span>
                    )}
                    <p className="truncate font-mono text-xs text-muted-foreground">
                      {d.package} {d.version}
                    </p>
                    {/* Below the breakpoint the Fix column folds under the finding. */}
                    <p className="text-xs text-muted-foreground @3xl:hidden">{fix}</p>
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className={toneSurface[SEVERITY_TONE[key] ?? "neutral"]}>
                      {severityLabel(key)}
                    </Badge>
                    {d.would_block && (
                      <p className={cn("mt-1 text-xs", refuses ? toneText.danger : "text-muted-foreground")}>
                        {refuses ? "Refuses deploy" : "Would refuse deploy"}
                      </p>
                    )}
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @3xl:table-cell">
                    {d.fixed_in ? (
                      <>
                        Upgrade to <span className="font-mono">{d.fixed_in}</span>
                      </>
                    ) : (
                      "No fix published"
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Actions for ${d.advisory_id} on ${d.name || d.slug}`}
                          >
                            <MoreHorizontal className="h-4 w-4" />
                          </Button>
                        }
                      />
                      <DropdownMenuContent align="end">
                        {/* Absent rather than disabled when waivers are off
                            (a longest waiver of 0 days). */}
                        {settings?.dependency_waiver_max_days !== 0 && (
                          <DropdownMenuItem onClick={() => setWaiving(d)}>
                            Waive…
                          </DropdownMenuItem>
                        )}
                        {!d.locked_by_policy && (
                          <DropdownMenuItem
                            variant="destructive"
                            onClick={() => setLocking(d)}
                          >
                            Lock app…
                          </DropdownMenuItem>
                        )}
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}

      {(notRecorded > 0 || cannotCheck > 0) && (
        <p className="border-t border-border/60 px-4 py-3 text-xs text-muted-foreground">
          Not on this list:{" "}
          {notRecorded > 0 && (
            <button
              type="button"
              onClick={() => onShowApps("unscanned")}
              className="underline underline-offset-2 hover:text-foreground"
            >
              {notRecorded} app{notRecorded === 1 ? "" : "s"} not recorded yet
            </button>
          )}
          {notRecorded > 0 && cannotCheck > 0 && " · "}
          {cannotCheck > 0 && (
            <button
              type="button"
              onClick={() => onShowApps("recorded")}
              className="underline underline-offset-2 hover:text-foreground"
            >
              {cannotCheck} app{cannotCheck === 1 ? "" : "s"} that can’t be checked
            </button>
          )}
          .{notRecorded > 0 && " Each records its packages on its next deploy."}
          {cannotCheck > 0 && " Few advisories are published for R, so a redeploy won’t help there."}
        </p>
      )}

      {waiving && (
        <WaiverDialog
          key={`${waiving.app_id}-${waiving.advisory_id}`}
          appId={waiving.app_id}
          appName={waiving.name || waiving.slug}
          finding={waiving}
          maxDays={settings?.dependency_waiver_max_days ?? 0}
          onClose={() => setWaiving(null)}
          onGranted={() => {
            setWaiving(null);
            queryClient.invalidateQueries({ queryKey: ["admin-dependencies"] });
          }}
        />
      )}
      {locking && (
        <LockAppDialog
          key={locking.app_id}
          appId={locking.app_id}
          appName={locking.name || locking.slug}
          open
          onOpenChange={(o) => !o && setLocking(null)}
          defaultMessage={`Locked while ${locking.advisory_id} in ${locking.package} is dealt with.`}
        />
      )}
    </Section>
  );
}

/**
 * One writer for the platform settings this page owns. Both branches of the
 * page save the mode through it, so their invalidations cannot drift apart.
 */
function useSaveDependencySettings(
  onSaved?: (sent: Parameters<typeof api.updatePlatformSettings>[0]) => void,
) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (patch: Parameters<typeof api.updatePlatformSettings>[0]) =>
      api.updatePlatformSettings(patch),
    onSuccess: (res, sent) => {
      queryClient.setQueryData(["platform-settings"], res.settings);
      queryClient.invalidateQueries({ queryKey: ["admin-dependencies"] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      onSaved?.(sent);
      if (res.warning) toast.warning(res.warning, { duration: 12000 });
      else toast.success("Saved");
    },
    onError: (e: Error) => toast.error(e.message || "Couldn’t save"),
  });
}

/**
 * The mode itself, and not a link to it. It was the only part of this policy
 * that lived on Settings, and this page linked back at it twice — one setting
 * with two owners, which is what ADR §50 D2 moves. Saved the moment it is
 * chosen, like the switches beside it.
 *
 * One definition, rendered from both branches of the page: Off is a mode this
 * page can be set to, so it has to be a mode this page can be set back from.
 */
function DependencyModeRow({
  settings,
  save,
}: {
  settings: PlatformSettings;
  save: ReturnType<typeof useSaveDependencySettings>;
}) {
  return (
    <SettingRow id="dependency_policy">
      <ChoiceButtons
        label="Dependency policy"
        value={settings.dependency_policy}
        disabled={save.isPending}
        onChange={(v) => save.mutate({ dependency_policy: v })}
        options={[
          {
            value: "off",
            label: "Off",
            hint: "Nothing downloaded, nothing resolved, nothing checked.",
          },
          {
            value: "warn",
            label: "Warn",
            hint: "Records and shows everything. Refuses only malicious packages.",
          },
          {
            value: "block",
            label: "Refuse",
            hint: "Refuses a deploy that brings in a finding at or above the threshold.",
          },
        ]}
      />
    </SettingRow>
  );
}

/**
 * The off branch's Policy section — the mode row and nothing else, because
 * there is nothing else to configure while it is off. It carries `id="policy"`
 * like its counterpart in {@link PolicyControls}; only one of the two renders
 * at a time, so the settings index's anchor resolves either way.
 */
function DependencyModeSection() {
  const { data: settings, isLoading } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: () => api.getPlatformSettings(),
  });
  const save = useSaveDependencySettings();

  return (
    <Section id="policy" title="Policy">
      <div className="p-4">
        {isLoading || !settings ? (
          <Skeleton className="h-16 w-full" />
        ) : (
          <DependencyModeRow settings={settings} save={save} />
        )}
      </div>
    </Section>
  );
}

/**
 * The controls.
 *
 * On this page rather than on Settings for the two reasons Email and
 * Telemetry each have a page of their own: it holds a credential, and it has
 * an action that talks to something outside this install and can fail in ways
 * only that thing's own error text explains.
 *
 * **A switch saves the moment it is thrown; the numbers batch.** The server's
 * warning belongs to the toggle that caused it — "going straight from off to
 * blocking" is a sentence about the click, not about a form.
 */
function PolicyControls({
  credentialSet,
  credentialUnreadable,
  refusedNow,
  refusedUnderRefuse,
  enforcing,
}: {
  /** Serving apps whose next deploy the policy in force refuses — the
   *  *Needs a decision* list's own count, so the two cannot disagree. It is
   *  what Preview used to answer, and needs no button: every control here
   *  saves on the throw, so the policy on screen is the policy in force. */
  refusedNow: number;
  /** The same under Refuse — what switching from Warn would add. */
  refusedUnderRefuse: number;
  enforcing: boolean;
  credentialSet: boolean;
  /** The stored mirror credential cannot be decrypted — the key changed. The
   * overview owns this fact, so it arrives as a prop rather than a second
   * fetch of the same endpoint. */
  credentialUnreadable: boolean;
}) {
  const queryClient = useQueryClient();
  const {
    data: settings,
    isLoading,
    error,
    refetch,
  } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: () => api.getPlatformSettings(),
  });
  const [draft, setDraft] = useState<Record<string, number | string>>({});
  const [credential, setCredential] = useState("");
  // Advanced opens itself for a ⌘K jump to one of the settings inside it.
  const { hash } = useLocation();

  const save = useSaveDependencySettings((sent) => {
    // Only the drafts that went out. The switches and selects save through
    // this same writer on the throw, and clearing the whole map there threw
    // away a typed feed address or interval — silently, under a "Saved" toast.
    setDraft((d) => {
      const next = { ...d };
      for (const key of Object.keys(sent)) delete next[key];
      return next;
    });
  });

  const saveCredential = useMutation({
    mutationFn: () => api.setAdvisoryFeedCredential(credential),
    onSuccess: () => {
      setCredential("");
      queryClient.invalidateQueries({ queryKey: ["admin-dependencies"] });
      toast.success("Saved");
    },
    onError: (e: Error) =>
      toast.error(e.message || "Couldn’t save the credential"),
  });

  useUnloadGuard(
    Object.keys(draft).length > 0,
    "The dependency policy has unsaved changes",
  );

  // Shares the `["platform-settings"]` key with Admin → Settings and the
  // personal-keys switch, so one outage renders all three at once -- which is
  // why each says which subject it could not read rather than three copies of
  // one sentence.
  if (error) {
    return (
      <QueryError
        title="The dependency policy could not be read"
        error={error}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={() => void refetch()}
      />
    );
  }
  if (isLoading || !settings) return <Skeleton className="h-64 w-full" />;

  // The typed text, never a number. See NumberRow for why.
  const raw = (k: NumberSetting) =>
    draft[k] !== undefined ? String(draft[k]) : String(settings[k]);
  const fieldError = (k: NumberSetting) =>
    numberError(raw(k), NUMBER_BOUNDS[k][0], NUMBER_BOUNDS[k][1]);
  const broken = NUMBER_SETTINGS.filter((k) => fieldError(k) !== null).length;
  const on = settings.dependency_policy !== "off";
  // The id is written at each call site as a literal, because the settings
  // index's guard reads the page's source for `id="…"`.
  const numberRow = (k: NumberSetting) => ({
    value: raw(k),
    error: fieldError(k),
    min: NUMBER_BOUNDS[k][0],
    max: NUMBER_BOUNDS[k][1],
    onChange: (v: string) => setDraft((d) => ({ ...d, [k]: v })),
  });

  return (
    <Section id="policy" title="Policy">
      {/* One row shape for every control on the card (`SettingRow`): name and
          help from the settings index on the left, the control in the fixed
          column on the right — the switches, the selects and the numbers
          alike, the way Admin → Authentication's editors draw them. */}
      <div className="divide-y divide-border/60 p-4">
        <DependencyModeRow settings={settings} save={save} />

        {on && (
          <>
            <SettingRow id="dependency_block_malware">
              <Switch
                id="dependency_block_malware"
                checked={settings.dependency_block_malware}
                onCheckedChange={(v) =>
                  save.mutate({ dependency_block_malware: v })
                }
              />
            </SettingRow>

            <SettingRow id="dependency_block_severity">
              <Select
                value={settings.dependency_block_severity}
                onValueChange={(v) =>
                  save.mutate({ dependency_block_severity: v as string })
                }
              >
                <SelectTrigger id="dependency_block_severity" className="w-full">
                  <SelectValue>
                    {(v) => THRESHOLD_LABEL[v as string] ?? "Choose…"}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {Object.keys(THRESHOLD_LABEL).map((k) => (
                    <SelectItem key={k} value={k}>
                      {THRESHOLD_LABEL[k]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </SettingRow>

            {settings.dependency_watch_enabled && (
              // The one control on this page that takes a running app off the
              // air. It only renders with the watch on, because locking
              // happens on a scheduled pass and the server refuses the
              // combination.
              <SettingRow
                id="dependency_lock_on"
                noteTone="attention"
                note={
                  settings.dependency_lock_on === "off"
                    ? undefined
                    : settings.dependency_lock_on === "malware"
                      ? "A locked app is stopped and its owner is told. An unlock lasts until the next check; use a waiver instead."
                      : "Above malware, one bad advisory can stop a healthy app. An unlock lasts until the next check; use a waiver instead."
                }
              >
                <Select
                  value={settings.dependency_lock_on}
                  onValueChange={(v) =>
                    save.mutate({
                      dependency_lock_on: v as DependencyLockOn,
                    })
                  }
                >
                  <SelectTrigger id="dependency_lock_on" className="w-full">
                    <SelectValue>
                      {(v) => LOCK_LABEL[v as string] ?? "Choose…"}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {Object.keys(LOCK_LABEL).map((k) => (
                      <SelectItem key={k} value={k}>
                        {LOCK_LABEL[k]}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </SettingRow>
            )}
          </>
        )}
      </div>

      {on && (
        <>
          {/* What the policy in force does today, from the same rows as
              *Needs a decision*. It replaces a Preview button: every control
              above saves on the throw, so there was never a draft to preview. */}
          <div className="space-y-1 border-t border-border/60 px-4 py-3 text-xs text-muted-foreground">
            <p>
              {refusedNow === 0
                ? "Today this refuses no serving app’s next deploy"
                : `Today this refuses the next deploy of ${refusedNow} serving app${refusedNow === 1 ? "" : "s"}`}
              {!enforcing && refusedUnderRefuse !== refusedNow
                ? `; under Refuse it would be ${refusedUnderRefuse}. `
                : ". "}
              {refusedUnderRefuse > 0 && (
                <a href="#decide" className="underline underline-offset-2 hover:text-foreground">
                  Show them
                </a>
              )}
            </p>
            {/* What this cannot refuse, said rather than discovered (ADR §39). */}
            <p>
              Not checked: packages an isolated R app installs while it runs.
              No advisory against an R package is not a clean result.
            </p>
          </div>

          <div id="advanced" className="scroll-mt-20 border-t border-border/60 px-4 py-3">
            <Disclosure
              label="Advanced"
              defaultOpen={hash === "#advanced" || hash === "#feed"}
            >
              <div className="divide-y divide-border/60">
                <SettingRow id="dependency_block_only_fixable">
                  <Switch
                    id="dependency_block_only_fixable"
                    checked={settings.dependency_block_only_fixable}
                    onCheckedChange={(v) =>
                      save.mutate({ dependency_block_only_fixable: v })
                    }
                  />
                </SettingRow>

                <SettingRow id="dependency_scan_on_deploy">
                  <Switch
                    id="dependency_scan_on_deploy"
                    checked={settings.dependency_scan_on_deploy}
                    onCheckedChange={(v) =>
                      save.mutate({ dependency_scan_on_deploy: v })
                    }
                  />
                </SettingRow>

                <SettingRow id="dependency_watch_enabled">
                  <Switch
                    id="dependency_watch_enabled"
                    checked={settings.dependency_watch_enabled}
                    onCheckedChange={(v) =>
                      save.mutate({ dependency_watch_enabled: v })
                    }
                  />
                </SettingRow>

                {settings.dependency_watch_enabled && (
                  <>
                    <NumberRow id="dependency_scan_interval_hours" {...numberRow("dependency_scan_interval_hours")} unit="hours" />
                    <NumberRow id="dependency_scan_history_kept" {...numberRow("dependency_scan_history_kept")} />
                  </>
                )}

                <NumberRow id="dependency_waiver_max_days" {...numberRow("dependency_waiver_max_days")} unit="days" />
              </div>

              <div id="feed" className="mt-2 scroll-mt-20 space-y-2 border-t border-border/60 pt-4">
                <SectionHeading
                  level={3}
                  title="Advisory data source"
                  description="Your package list never leaves this install."
                />
                <div className="divide-y divide-border/60">
                  <SettingRow id="dependency_feed_endpoint">
                    <Input
                      id="dependency_feed_endpoint"
                      value={
                        draft.dependency_feed_endpoint !== undefined
                          ? String(draft.dependency_feed_endpoint)
                          : settings.dependency_feed_endpoint
                      }
                      onChange={(e) =>
                        setDraft((d) => ({
                          ...d,
                          dependency_feed_endpoint: e.target.value,
                        }))
                      }
                      className="w-full font-mono text-xs"
                    />
                  </SettingRow>

                  <SettingRow id="dependency_feed_credential">
                    <div className="flex w-full gap-2">
                      <Input
                        id="dependency_feed_credential"
                        type="password"
                        value={credential}
                        onChange={(e) => setCredential(e.target.value)}
                        placeholder={credentialPlaceholder(
                          credentialSet,
                          credentialUnreadable,
                          "",
                        )}
                        className="min-w-0 flex-1 font-mono text-xs"
                      />
                      <Button
                        variant="outline"
                        onClick={() => saveCredential.mutate()}
                        disabled={saveCredential.isPending || credential === ""}
                      >
                        {saveCredential.isPending ? "Saving…" : "Save"}
                      </Button>
                    </div>
                  </SettingRow>

                  <NumberRow id="dependency_feed_interval_hours" {...numberRow("dependency_feed_interval_hours")} unit="hours" />
                  <NumberRow id="dependency_feed_max_age_hours" {...numberRow("dependency_feed_max_age_hours")} unit="hours" />
                </div>
                {credentialUnreadable && (
                  <div className="pt-2">
                    <UnreadableCredentialNotice what="mirror credential" />
                  </div>
                )}
              </div>
            </Disclosure>
          </div>
        </>
      )}

      <div className="px-4 pb-4 empty:hidden">
        <PendingBar
          count={Object.keys(draft).length}
          broken={broken}
          saving={save.isPending}
          onDiscard={() => setDraft({})}
          onSave={() => {
            // The numbers are held as text and coerced here, at the one
            // moment they are known to be well formed.
            const patch = { ...draft };
            for (const k of NUMBER_SETTINGS) {
              if (patch[k] !== undefined) patch[k] = Number(raw(k).trim());
            }
            save.mutate(
              patch as Parameters<typeof api.updatePlatformSettings>[0],
            );
          }}
        />
      </div>
    </Section>
  );
}

/** The batched numbers on this page. They belong in
 *  `lib/settings-fields.ts` beside the Settings page's own NUMBER_FIELDS
 *  eventually; they are here because their bounds are read by nothing else. */
const NUMBER_SETTINGS = [
  "dependency_feed_interval_hours",
  "dependency_feed_max_age_hours",
  "dependency_waiver_max_days",
  "dependency_scan_interval_hours",
  "dependency_scan_history_kept",
] as const;

type NumberSetting = (typeof NUMBER_SETTINGS)[number];

/** Client-side bounds mirroring the server's exactly. */
const NUMBER_BOUNDS: Record<NumberSetting, [number, number]> = {
  dependency_feed_interval_hours: [1, 168],
  dependency_feed_max_age_hours: [6, 8760],
  dependency_waiver_max_days: [0, 365],
  dependency_scan_interval_hours: [1, 168],
  dependency_scan_history_kept: [1, 365],
};

function numberError(value: string, lo: number, hi: number): string | null {
  const t = value.trim();
  const n = Number(t);
  if (t === "" || !Number.isInteger(n) || n < lo || n > hi) {
    return `Must be a whole number from ${lo} to ${hi}.`;
  }
  return null;
}

/** What this install locks a *serving* app for. Deliberately shorter than
 *  THRESHOLD_LABEL: there is no moderate and no low, because a moderate
 *  transitive advisory with no fix is on nearly every Node app and a value that
 *  would take an estate off the air over one should not be pickable. */
const LOCK_LABEL: Record<string, string> = {
  off: "Nothing — never lock a serving app",
  malware: "A package reported as malicious",
  critical: "Malicious packages, or a critical advisory",
  high: "Malicious packages, or a high advisory and above",
};

const THRESHOLD_LABEL: Record<string, string> = {
  critical: "Critical",
  high: "High and above",
  moderate: "Moderate and above",
  low: "Low and above — everything scored",
  off: "Never refuse on severity alone",
};

/**
 * A batched number, held as the raw string the person typed.
 *
 * Not a `number`: `Number("")` is 0, so a field held as a number writes a 0
 * into the draft the moment it is emptied to be retyped. On
 * `dependency_waiver_max_days` 0 is a legal value that means *waivers off
 * entirely*, so that save landed, every live waiver stopped applying, and the
 * administrator was told "Saved". Empty is an error; 0 is a value; the two
 * have to stay tellable apart, and a string is what keeps them apart.
 */
function NumberRow({
  id,
  unit,
  value,
  onChange,
  min,
  max,
  error,
}: {
  id: NumberSetting;
  unit?: string;
  value: string;
  onChange: (raw: string) => void;
  min: number;
  max: number;
  error: string | null;
}) {
  // No Save of its own: these batch into the card's pending bar.
  return (
    <SettingRow id={id}>
      <div className="space-y-1 sm:text-right">
        <div className="flex items-center gap-2 sm:justify-end">
          <Input
            id={id}
            type="number"
            min={min}
            max={max}
            value={value}
            aria-invalid={!!error}
            aria-describedby={error ? fieldErrorId(id) : undefined}
            onChange={(e) => onChange(e.target.value)}
            className="w-24"
          />
          {/* A fixed slot, so a field with no unit, or a shorter one, keeps
              the same right edge as its neighbours. */}
          <span className="w-9 text-left text-xs text-muted-foreground">
            {unit}
          </span>
        </div>
        {error && (
          <FieldError field={id} className="max-w-xs sm:ml-auto">
            {error}
          </FieldError>
        )}
      </div>
    </SettingRow>
  );
}

/**
 * The corpus, per ecosystem — because they are fetched and swapped
 * independently: npm succeeding while PyPI 404s must read as one current corpus
 * and one stale one, not as an average.
 */
function FeedStatus({ data }: { data: AdminDependencyState }) {
  const imported = data.feeds.some((f) => f.origin === "import");
  return (
    <Section id="advisory-data" title="Advisory data">
      {data.feeds.length === 0 ? (
        <div className="px-4 py-6">
          <Notice tone="attention" title="No advisory data has ever loaded">
            {data.mode === "block"
              ? "Set to Refuse, but nothing is refused until data loads. "
              : ""}
            {data.can_sync
              ? "Check now fetches it."
              : (
                <>
                  Load a snapshot with{" "}
                  <Code>
                    toft advisory-import --ecosystem &lt;name&gt; --as-of &lt;date&gt; &lt;file.zip&gt;
                  </Code>
                  .
                </>
              )}
          </Notice>
        </div>
      ) : (
        <div className="divide-y divide-border/60">
          {data.feeds.map((f) => (
            <div
              key={f.ecosystem}
              className="flex flex-wrap items-baseline justify-between gap-2 px-4 py-3"
            >
              <div>
                <p className="text-sm font-medium text-foreground">
                  {f.ecosystem}
                </p>
                <p className="mt-0.5 text-xs text-muted-foreground">
                  {f.advisory_count} advisor{f.advisory_count === 1 ? "y" : "ies"}
                  {f.snapshot_at && (
                    <>
                      {" · published "}
                      <time
                        dateTime={f.snapshot_at}
                        title={formatDateTime(f.snapshot_at)}
                      >
                        <RelativeTime iso={f.snapshot_at} />
                      </time>
                    </>
                  )}
                  {f.origin === "import" && " · imported"}
                  {!f.covered && " · can’t be checked"}
                </p>
              </div>
              {f.last_error ? (
                <span
                  className={cn("max-w-md text-right text-xs", toneText.danger)}
                >
                  Last refresh failed: {f.last_error}
                </span>
              ) : (
                <span className={cn("text-xs", toneText.ok)}>Current</span>
              )}
              {!f.covered && (
                <p className="w-full text-xs text-muted-foreground">
                  Few advisories are published for this ecosystem, so it’s left
                  out of the staleness check.
                </p>
              )}
            </div>
          ))}
        </div>
      )}

      {data.feed_stale && (
        <div className="border-t border-border/60 px-4 py-3">
          <Notice
            tone="attention"
            title="Older than this install’s staleness threshold"
          >
            Stale data never blocks a deploy, but findings may be behind.{" "}
            {imported
              ? "Nothing refreshes an imported snapshot on its own; import a newer one."
              : (
                <>
                  Check now, or look at the source under{" "}
                  <a href="#feed" className="underline underline-offset-2 hover:text-foreground">
                    Advanced
                  </a>
                  .
                </>
              )}
          </Notice>
        </div>
      )}
    </Section>
  );
}

/**
 * By advisory: one row per advisory, the apps under it.
 *
 * The default axis. Sorted malware first, then by how many serving apps it
 * touches, then by severity — the same order every finding table in the
 * product uses, so a reader who learned it on an app's tab does not relearn
 * it here.
 */
function ByAdvisory({ groups }: { groups: AdvisoryGroup[] }) {
  if (groups.length === 0) {
    return (
      <TableCard>
        <EmptyState
          icon={<PackageSearch className="size-6" />}
          title="No known advisories across the estate"
          description="Apps that haven’t recorded their packages yet aren’t counted here."
        />
      </TableCard>
    );
  }
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="w-full">Advisory</TableHead>
            <TableHead>Severity</TableHead>
            <TableHead className="hidden @2xl:table-cell">Apps</TableHead>
            <TableHead className="hidden @4xl:table-cell">Fixed in</TableHead>
            <TableHead className="text-right">Serving</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {groups.map((g) => {
            const key = g.kind === "malware" ? "malware" : g.severity || "unknown";
            const fixed = Array.from(new Set(g.apps.map((a) => a.fixed_in).filter(Boolean)));
            return (
              <TableRow key={g.advisory_id} className="align-top">
                <TableCell className="max-w-0">
                  {isHttpUrl(g.url) ? (
                    <a
                      href={g.url}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="font-mono text-sm text-foreground hover:text-primary hover:underline"
                    >
                      {g.advisory_id}
                    </a>
                  ) : (
                    <span className="font-mono text-sm text-foreground">{g.advisory_id}</span>
                  )}
                  {g.summary && (
                    <p className="truncate text-xs text-muted-foreground" title={g.summary}>
                      {g.summary}
                    </p>
                  )}
                  {/* Below the breakpoint the Apps column folds under the advisory. */}
                  <div className="@2xl:hidden">
                    <AdvisoryApps apps={g.apps} />
                  </div>
                </TableCell>
                <TableCell>
                  <Badge variant="outline" className={toneSurface[SEVERITY_TONE[key] ?? "neutral"]}>
                    {severityLabel(key)}
                  </Badge>
                </TableCell>
                <TableCell className="hidden whitespace-normal @2xl:table-cell">
                  <AdvisoryApps apps={g.apps} />
                </TableCell>
                <TableCell className="hidden font-mono text-xs text-muted-foreground @4xl:table-cell">
                  {fixed.length > 0 ? fixed.join(", ") : "—"}
                </TableCell>
                <TableCell className="text-right text-xs text-muted-foreground">
                  {/* A plain count: the severity chip beside it carries the state. */}
                  <span>{g.serving}</span>
                  {g.stopped > 0 && ` · ${g.stopped} stopped`}
                  {g.waived > 0 && ` · ${g.waived} waived`}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </TableCard>
  );
}

/** The apps one advisory touches: slug, package and version, and its state. */
function AdvisoryApps({ apps }: { apps: AdvisoryGroup["apps"] }) {
  return (
    <ul className="space-y-0.5">
      {apps.map((a) => (
        <li key={`${a.app_id}-${a.package}-${a.version}`} className="text-xs">
          <Link to={`/apps/${a.app_id}/dependencies`} className="hover:text-primary hover:underline">
            {a.slug}
          </Link>
          <span className="ml-1.5 font-mono text-muted-foreground">
            {a.package} {a.version}
          </span>
          {a.waived && <span className={cn("ml-1.5", toneText.neutral)}>waived</span>}
          {!a.serving && <span className="ml-1.5 text-muted-foreground">stopped</span>}
        </li>
      ))}
    </ul>
  );
}

/**
 * By app: how bad is each app — and, through the chips, which apps are in a
 * given state.
 *
 * **Every app is a row here, including one that has never deployed at all**,
 * because the query behind it is a LEFT JOIN: an app the platform knows nothing
 * about is exactly the row an administrator is looking for, and filtering it
 * out of the estate view would make "not scanned" unfindable in the one place
 * it is named.
 */
function ByApp({
  apps,
  enforcing,
  filter,
  setFilter,
}: {
  apps: AdminDependencyApp[];
  enforcing: boolean;
  filter: string;
  setFilter: (v: string) => void;
}) {
  const [history, setHistory] = useState<AdminDependencyApp | null>(null);

  if (apps.length === 0) {
    return (
      <TableCard>
        <EmptyState
          title="No apps yet"
          description="Deploy one and its packages are recorded here."
        />
      </TableCard>
    );
  }

  const shown = apps.filter(appFilterMatch(filter));
  const active = APP_FILTERS.find((f) => f.value === filter) ?? APP_FILTERS[0];

  return (
    <div className="space-y-2">
      <FilterChips
        label="Narrow by state"
        options={APP_FILTERS.map((f) => ({
          label: f.label,
          value: f.value,
          count: apps.filter(f.match).length,
        }))}
        value={filter}
        onChange={setFilter}
      />
      {shown.length === 0 ? (
        <TableCard>
          <EmptyState
            title={active.empty}
            description="Choose All to see every app."
          />
        </TableCard>
      ) : (
        <AppTable apps={shown} enforcing={enforcing} onHistory={setHistory} />
      )}
      {history && (
        <ScanHistoryDialog
          key={history.app_id}
          app={history}
          onClose={() => setHistory(null)}
        />
      )}
    </div>
  );
}

/**
 * One app's readings, newest first — the governance question.
 *
 * **Administrator-only, and deliberately not on the owner's tab.** The owner
 * gets the latest reading, which answers *what do I fix* completely. A stack of
 * readings answers something else: how long this sat, whether anybody acted,
 * how many passes went by — a statement about response, which is the class the
 * audit log's two-reader rule already governs.
 */
function ScanHistoryDialog({
  app,
  onClose,
}: {
  app: AdminDependencyApp;
  onClose: () => void;
}) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["dependency-scans", app.app_id],
    queryFn: () => api.getAppDependencyScans(app.app_id),
  });

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent scrollable className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{app.name} — check history</DialogTitle>
          <DialogDescription>
            What each check found at the time. Checks can’t be rebuilt
            later.
          </DialogDescription>
        </DialogHeader>

        {/* The scroll is the body's, not a `max-h-[60vh]` on the table region.
            The dialog itself had no bound, so the header and the sentence
            under the table went off the viewport with it — and
            `DialogContent` is `position: fixed`, so nothing scrolled them
            back. */}
        <DialogBody className="space-y-4">
          {error ? (
            <QueryError
              title="This app’s history could not be read"
              error={error}
              fallback={ADMIN_ONLY_FALLBACK}
            />
          ) : isLoading || !data ? (
            <Skeleton className="h-48 w-full" />
          ) : data.scans.length === 0 ? (
            <EmptyState
              title="Nothing recorded yet"
              description="A reading is written on each deploy and background check."
            />
          ) : (
            <div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>When</TableHead>
                    <TableHead>Why</TableHead>
                    <TableHead>Saw</TableHead>
                    <TableHead className="hidden @md:table-cell">
                      Advisories
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.scans.map((scan) => (
                    <TableRow key={scan.id}>
                      <TableCell className="align-top text-xs">
                        <time
                          dateTime={scan.occurred_at}
                          title={formatDateTime(scan.occurred_at)}
                        >
                          <RelativeTime iso={scan.occurred_at} />
                        </time>
                      </TableCell>
                      <TableCell className="align-top text-xs text-muted-foreground">
                        {TRIGGER_LABEL[scan.trigger] ?? scan.trigger}
                      </TableCell>
                      <TableCell className="align-top">
                        {!scan.scanned ? (
                          // Coverage, not a count. False is *unknown* and must
                          // never render as "no findings".
                          <span className="text-xs text-muted-foreground">
                            {scan.packages > 0
                              ? `${scan.packages} packages, none checked against anything`
                              : "Nothing recorded"}
                          </span>
                        ) : (
                          <span className="flex flex-wrap items-center gap-1.5">
                            {scan.malware > 0 && (
                              <Badge
                                variant="outline"
                                className={toneSurface.danger}
                              >
                                {scan.malware} malicious
                              </Badge>
                            )}
                            {scan.counted > 0 ? (
                              <Badge
                                variant="outline"
                                className={toneSurface.attention}
                              >
                                {scan.counted} at or above the threshold
                              </Badge>
                            ) : (
                              <span className={cn("text-xs", toneText.ok)}>
                                None at or above the threshold
                              </span>
                            )}
                            {scan.waived > 0 && (
                              <Badge
                                variant="outline"
                                className={toneSurface.neutral}
                              >
                                {scan.waived} waived
                              </Badge>
                            )}
                          </span>
                        )}
                      </TableCell>
                      <TableCell className="hidden max-w-0 truncate align-top font-mono text-xs text-muted-foreground @md:table-cell">
                        {scan.advisories.length > 0
                          ? scan.advisories.join(", ")
                          : "—"}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}

          {data && data.scans.length > 0 && (
            <p className="text-xs text-muted-foreground">
              The last {data.kept} readings are kept per app; this one has{" "}
              {data.total}. Set the count under Policy.
            </p>
          )}
        </DialogBody>
      </DialogContent>
    </Dialog>
  );
}

/** Why a reading was taken. Only a scheduled pass can lock: an automatic
 *  consequence needs an automatic trigger, and a button is a person. */
const TRIGGER_LABEL: Record<string, string> = {
  deploy: "On deploy",
  scheduled: "Background check",
  manual: "On request",
};

/** The table itself, so ByApp above stays about which rows to show. */
function AppTable({
  apps,
  enforcing,
  onHistory,
}: {
  apps: AdminDependencyApp[];
  enforcing: boolean;
  onHistory: (app: AdminDependencyApp) => void;
}) {
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>App</TableHead>
            <TableHead>Findings</TableHead>
            <TableHead className="hidden @md:table-cell">Packages</TableHead>
            <TableHead className="hidden @3xl:table-cell">Owner</TableHead>
            {/* The observation, not the release's own verdict date. "When was
                this app last checked" had no answer before the sweep started
                writing one down, and that is the question this column is. */}
            <TableHead className="hidden @3xl:table-cell">
              Last checked
            </TableHead>
            <TableHead className="w-[1%]">
              <span className="sr-only">History</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {apps.map((a) => (
            <TableRow key={a.app_id}>
              <TableCell>
                <Link
                  to={`/apps/${a.app_id}/dependencies`}
                  className="font-medium text-foreground hover:text-primary hover:underline"
                >
                  {a.name}
                </Link>
                <p className="text-xs text-muted-foreground">
                  {a.slug}
                  {!a.serving && " · stopped"}
                </p>
                {a.locked_at && a.locked_source === "dependency" && (
                  <Badge
                    variant="outline"
                    className={cn("mt-1", toneSurface.danger)}
                  >
                    Locked by policy
                  </Badge>
                )}
              </TableCell>
              <TableCell>
                {!a.scanned ? (
                  // Unscanned never renders as zero: it is a different fact.
                  // And it is two facts — nothing recorded, or recorded and
                  // covered by no corpus — which a redeploy answers only one
                  // of, so the row says which.
                  <span className="text-xs text-muted-foreground">
                    {a.packages > 0
                      ? "Can’t be checked"
                      : "Not recorded yet"}
                  </span>
                ) : a.counts.counted === 0 ? (
                  <span className={cn("text-xs", toneText.ok)}>None</span>
                ) : (
                  <span className="flex flex-wrap items-center gap-1.5">
                    {a.counts.malware > 0 && (
                      <Badge variant="outline" className={toneSurface.danger}>
                        {a.counts.malware} malicious
                      </Badge>
                    )}
                    {a.counts.blocking > 0 && (
                      // "would refuse" while the mode is Warn, because nothing
                      // is being refused — the same distinction `would_block`
                      // carries on every finding.
                      <Badge variant="outline" className={toneSurface.danger}>
                        {a.counts.blocking}{" "}
                        {enforcing ? "refuse deploy" : "would refuse deploy"}
                      </Badge>
                    )}
                    <Badge variant="outline" className={toneSurface.attention}>
                      {a.counts.counted} at or above threshold
                    </Badge>
                    {a.counts.waived > 0 && (
                      <Badge variant="outline" className={toneSurface.neutral}>
                        {a.counts.waived} waived
                      </Badge>
                    )}
                  </span>
                )}
              </TableCell>
              <TableCell className="hidden text-xs text-muted-foreground @md:table-cell">
                {a.scanned ? a.packages : "—"}
              </TableCell>
              <TableCell className="hidden text-xs text-muted-foreground @3xl:table-cell">
                {a.owner_name || "—"}
              </TableCell>
              <TableCell className="hidden text-xs text-muted-foreground @3xl:table-cell">
                {a.last_scan_at ? (
                  <time
                    dateTime={a.last_scan_at}
                    title={formatDateTime(a.last_scan_at)}
                  >
                    <RelativeTime iso={a.last_scan_at} />
                  </time>
                ) : (
                  // Never a date it did not take. Before the sweep started
                  // recording, this read as the release's own verdict date,
                  // which is a different fact.
                  "Never"
                )}
              </TableCell>
              <TableCell className="text-right">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => onHistory(a)}
                  aria-label={`Check history for ${a.name}`}
                >
                  <History className="size-3.5" />
                  <span className="hidden @xl:inline">History</span>
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableCard>
  );
}

/**
 * "Which of our apps has this."
 *
 * By package name as well as by advisory id, because the news arrives as
 * "there's something going round in event-stream" hours before anybody has an
 * identifier.
 */
/**
 * The version predicate, in the operators a person thinks in.
 *
 * They render to `advisory.Range`'s own fields, which is what the server
 * compares with — `before` is the exclusive upper bound because that is what
 * `fixed` means everywhere else in this feature.
 */
const VERSION_OPS = {
  any: "Any version",
  is: "Is exactly",
  before: "Before",
  from: "From",
  between: "Between",
} as const;

type VersionOp = keyof typeof VERSION_OPS;

/**
 * One search box, two questions.
 *
 * *Affected* is the advisory search, unchanged. *Also installed* is the
 * inventory read, and it is what makes the empty state true by construction:
 * before it, "no app has recorded a package matching lodash" was printed while
 * eleven apps carried lodash and none of them was vulnerable.
 */
function AdvisorySearch({
  query,
  setQuery,
}: {
  query: string;
  setQuery: (v: string) => void;
}) {
  const trimmed = query.trim();
  const [op, setOp] = useState<VersionOp>("any");
  const [lower, setLower] = useState("");
  const [upper, setUpper] = useState("");

  // Rendered inline rather than memoised: it is four strings, and the query key
  // below has to see the same values anyway.
  const version =
    op === "is" && lower
      ? { version: lower }
      : op === "before" && lower
        ? { fixed: lower }
        : op === "from" && lower
          ? { introduced: lower }
          : op === "between" && lower && upper
            ? { introduced: lower, fixed: upper }
            : undefined;

  const { data, isFetching, error } = useQuery({
    queryKey: ["dependency-search", trimmed],
    queryFn: () => api.searchDependencies(trimmed),
    enabled: trimmed.length > 0,
  });
  const inventory = useQuery({
    queryKey: ["package-search", trimmed, op, lower, upper],
    queryFn: () => api.searchPackages(trimmed, version),
    enabled: trimmed.length > 0,
  });

  const affected = data?.apps ?? [];
  const also = inventory.data?.occurrences ?? [];
  const odd = inventory.data?.incomparable ?? [];
  const loading =
    (isFetching && !data) || (inventory.isFetching && !inventory.data);

  return (
    <Section title="Find an advisory or a package">
      <div className="space-y-4 px-4 py-4">
        <SearchInput
          value={query}
          onChange={setQuery}
          placeholder="GHSA-p6mc-m468-83gg, MAL-2024-…, or event-stream"
        />

        <VersionPredicate
          op={op}
          setOp={setOp}
          lower={lower}
          setLower={setLower}
          upper={upper}
          setUpper={setUpper}
        />

        {trimmed !== "" && error && (
          <QueryError
            title="The search could not be run"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
          />
        )}
        {trimmed !== "" && !error && inventory.error && (
          <QueryError
            title="The inventory search could not be run"
            error={inventory.error}
            fallback={ADMIN_ONLY_FALLBACK}
          />
        )}

        {trimmed !== "" && !error && (
          <>
            {loading ? (
              <Skeleton className="h-24 w-full" />
            ) : affected.length === 0 &&
              also.length === 0 &&
              odd.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No app is affected by “{trimmed}” or has it installed.
              </p>
            ) : (
              <div className="space-y-5">
                {inventory.data && <CoverageLine data={inventory.data} />}

                {affected.length > 0 && data && (
                  <div className="space-y-3">
                    <SectionHeading
                      level={3}
                      title="Affected"
                      description="An advisory covers the version this app is serving."
                    />
                    <p className="text-sm text-foreground">
                      {data.serving} serving app{data.serving === 1 ? "" : "s"}{" "}
                      affected
                      {data.stopped > 0 && (
                        <span className="text-muted-foreground">
                          {" "}
                          · {data.stopped} stopped, not counted
                        </span>
                      )}
                      .
                    </p>
                    <div className="divide-y divide-border/60 rounded-md border border-border/60">
                      {data.apps.map((a) => (
                        <div key={a.app_id} className="px-3 py-2.5">
                          <div className="flex flex-wrap items-baseline justify-between gap-2">
                            <Link
                              to={`/apps/${a.app_id}/dependencies`}
                              className="text-sm font-medium text-foreground hover:text-primary hover:underline"
                            >
                              {a.name}
                            </Link>
                            <span className="text-xs text-muted-foreground">
                              {statusMeaning(a.status).label}
                            </span>
                          </div>
                          <ul className="mt-1 space-y-0.5">
                            {a.findings.map((f) => (
                              <li
                                key={`${f.advisory_id}-${f.package}-${f.version}`}
                                className="flex flex-wrap items-center gap-2 text-xs"
                              >
                                <span className="font-mono">
                                  {f.package} {f.version}
                                </span>
                                <Badge
                                  variant="outline"
                                  className={
                                    toneSurface[
                                      SEVERITY_TONE[severityKey(f)] ?? "neutral"
                                    ]
                                  }
                                >
                                  {severityLabel(severityKey(f))}
                                </Badge>
                                <span className="font-mono text-muted-foreground">
                                  {f.advisory_id}
                                </span>
                                {f.waived && (
                                  <span className={toneText.neutral}>
                                    waived
                                    {f.waiver_expires_at &&
                                      ` · ${expiresIn(f.waiver_expires_at)}`}
                                  </span>
                                )}
                                {f.fixed_in && (
                                  <span className="text-muted-foreground">
                                    fixed in {f.fixed_in}
                                  </span>
                                )}
                              </li>
                            ))}
                          </ul>
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                {also.length > 0 && (
                  <OccurrenceList
                    title="Also installed"
                    description="Installed, with no live advisory for the serving version."
                    rows={also}
                  />
                )}

                {odd.length > 0 && (
                  <OccurrenceList
                    title={`${odd.length} occurrence${odd.length === 1 ? "" : "s"} could not be compared to that version`}
                    description="Git URLs, file paths and local tags have no version order, so they’re listed here."
                    rows={odd}
                  />
                )}
              </div>
            )}
          </>
        )}
      </div>
    </Section>
  );
}

/**
 * The coverage line, which leads every governance answer.
 *
 * Without it "2 apps use requests" on an install where 61 of 84 apps have no
 * inventory is read as an estate-wide all-clear, and read that way in a meeting.
 * A search result feels exhaustive in a way a list does not, which is why this
 * sits above the rows rather than under them.
 */
function CoverageLine({ data }: { data: PackageSearchResult }) {
  const missing = data.apps_total - data.apps_scanned;
  return (
    <Notice tone={missing > 0 ? "attention" : "neutral"} icon={null}>
      Covers the {data.apps_scanned} of {data.apps_total} app
      {data.apps_total === 1 ? "" : "s"} with recorded packages.
      {missing > 0 && (
        <>
          {" "}
          The other {missing} are unknown until their next deploy.
        </>
      )}
    </Notice>
  );
}

/**
 * Occurrence rows, grouped by app the way the Affected section above is.
 *
 * A row is still one (app, package, version) — that is what the route returns
 * and what the counts mean — but one app carrying twenty-four variants of the
 * same library is one answer to "who has this", not twenty-four, and repeating
 * its name down the page buries the one column that varies. The app is the
 * heading; the occurrences are the list under it.
 */
function OccurrenceList({
  title,
  description,
  rows,
}: {
  title: string;
  description: string;
  rows: PackageOccurrence[];
}) {
  // Grouped inline: the route already returns them ordered by app.
  const byApp: { app: PackageOccurrence; items: PackageOccurrence[] }[] = [];
  for (const o of rows) {
    const last = byApp[byApp.length - 1];
    if (last && last.app.app_id === o.app_id) last.items.push(o);
    else byApp.push({ app: o, items: [o] });
  }

  return (
    <div className="space-y-3">
      {/* Level three, a sibling of *Affected*: both sit inside the search
          section's own heading rather than beside it. */}
      <SectionHeading level={3} title={title} description={description} />
      <div className="divide-y divide-border/60 rounded-md border border-border/60">
        {byApp.map(({ app, items }) => (
          <div key={app.app_id} className="px-3 py-2.5">
            <div className="flex flex-wrap items-baseline justify-between gap-2">
              <Link
                to={`/apps/${app.app_id}/dependencies`}
                className="text-sm font-medium text-foreground hover:text-primary hover:underline"
              >
                {app.name}
              </Link>
              <span className="text-xs text-muted-foreground">
                {items.length === 1
                  ? statusMeaning(app.status).label
                  : `${items.length} occurrences · ${statusMeaning(app.status).label}`}
              </span>
            </div>
            <ul className="mt-1 space-y-1">
              {items.map((o) => (
                <li
                  key={`${o.ecosystem}-${o.package}-${o.version}`}
                  className="min-w-0"
                >
                  <div className="flex flex-wrap items-center gap-2 text-xs">
                    <span className="font-mono">
                      {o.package} {o.version}
                    </span>
                    <span className="text-muted-foreground">{o.ecosystem}</span>
                    <Badge variant="outline" className={toneSurface.neutral}>
                      {o.direct ? "direct" : "transitive"}
                    </Badge>
                    {o.dev && (
                      <Badge variant="outline" className={toneSurface.neutral}>
                        dev
                      </Badge>
                    )}
                  </div>
                  {o.path && !o.direct && (
                    <p
                      className="truncate font-mono text-2xs text-muted-foreground"
                      title={o.path}
                    >
                      {o.path}
                    </p>
                  )}
                </li>
              ))}
            </ul>
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * The version predicate, collapsed by default so the common case stays one box
 * and one word: *do we have this at all*.
 */
function VersionPredicate({
  op,
  setOp,
  lower,
  setLower,
  upper,
  setUpper,
}: {
  op: VersionOp;
  setOp: (v: VersionOp) => void;
  lower: string;
  setLower: (v: string) => void;
  upper: string;
  setUpper: (v: string) => void;
}) {
  const [open, setOpen] = useState(false);

  if (!open) {
    return (
      <Button
        variant="ghost"
        size="sm"
        className="-ml-2"
        onClick={() => setOpen(true)}
      >
        Narrow by version
      </Button>
    );
  }

  return (
    <div className="flex flex-wrap items-end gap-2">
      <div className="space-y-1">
        <Label htmlFor="version-op">Version</Label>
        <Select value={op} onValueChange={(v) => setOp(v as VersionOp)}>
          <SelectTrigger id="version-op" className="w-40">
            <SelectValue>
              {(v) => (v ? VERSION_OPS[v as VersionOp] : "Any version")}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {Object.entries(VERSION_OPS).map(([value, label]) => (
              <SelectItem key={value} value={value}>
                {label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {op !== "any" && (
        <div className="space-y-1">
          <Label htmlFor="version-lower">
            {op === "before" ? "Version" : "From"}
          </Label>
          <Input
            id="version-lower"
            value={lower}
            onChange={(e) => setLower(e.target.value)}
            placeholder="4.17.20"
            className="w-32 font-mono"
          />
        </div>
      )}
      {op === "between" && (
        <div className="space-y-1">
          <Label htmlFor="version-upper">Before</Label>
          <Input
            id="version-upper"
            value={upper}
            onChange={(e) => setUpper(e.target.value)}
            placeholder="5.0.0"
            className="w-32 font-mono"
          />
        </div>
      )}

      <Button
        variant="ghost"
        size="sm"
        onClick={() => {
          setOp("any");
          setLower("");
          setUpper("");
          setOpen(false);
        }}
      >
        Clear
      </Button>
      {op === "before" || op === "between" ? (
        <FieldHelp className="w-full">
          “Before” is exclusive: that version is <em>not</em> matched.
        </FieldHelp>
      ) : null}
    </div>
  );
}

/**
 * The catalog: the same query with no name typed.
 *
 * A search box cannot answer *how fragmented are we*, *what is the long tail*,
 * *which library is in everything* — you have to know the name to type it. Both
 * counts are shown and labelled, because the exposure number (apps) and the
 * fragmentation number (occurrences) are different questions.
 */
function PackageCatalog({ onPick }: { onPick: (pkg: string) => void }) {
  // A sort, so it is a column header and not a FilterBar — the strip is the
  // primary-axis filter, and an uncounted one there read as a filter.
  const [sort, setSort] = useState<SortState<"apps" | "spread">>({ key: "apps", dir: "desc" });
  const { data, isLoading, error } = useQuery({
    queryKey: ["package-catalog"],
    queryFn: () => api.getPackageCatalog(),
  });

  if (error) {
    return (
      <QueryError
        title="The package catalog could not be read"
        error={error}
        fallback={ADMIN_ONLY_FALLBACK}
      />
    );
  }
  if (isLoading || !data) {
    return <Skeleton className="h-64 w-full" />;
  }
  if (data.packages.length === 0) {
    return (
      <EmptyState
        icon={<PackageSearch className="h-6 w-6" />}
        title="No packages recorded yet"
        description="Each app records its packages on its next deploy."
      />
    );
  }

  const sign = sort.dir === "desc" ? 1 : -1;
  const rows = [...data.packages].sort((a, b) =>
    sort.key === "apps"
      ? sign * (b.apps - a.apps) || a.package.localeCompare(b.package)
      : sign * (b.versions.length - a.versions.length) ||
        a.package.localeCompare(b.package),
  );

  return (
    <div className="space-y-3">
      <CoverageLine data={data} />
      <TableCard>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Package</TableHead>
              <TableHead className="hidden @md:table-cell">Ecosystem</TableHead>
              <SortableHead
                label="Apps"
                sortKey="apps"
                sort={sort}
                onSort={(k) => setSort((prev) => nextSort(prev, k))}
                align="right"
                className="text-right"
              />
              <TableHead className="text-right">Occurrences</TableHead>
              <SortableHead
                label="Versions in use"
                sortKey="spread"
                sort={sort}
                onSort={(k) => setSort((prev) => nextSort(prev, k))}
                className="hidden @3xl:table-cell"
              />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((p) => (
              <TableRow key={`${p.ecosystem}-${p.package}`}>
                <TableCell>
                  <button
                    type="button"
                    className="font-mono text-sm text-foreground hover:text-primary hover:underline"
                    onClick={() => onPick(p.package)}
                  >
                    {p.package}
                  </button>
                </TableCell>
                <TableCell className="hidden text-muted-foreground @md:table-cell">
                  {p.ecosystem}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {p.apps}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {p.occurrences}
                </TableCell>
                <TableCell className="hidden @3xl:table-cell">
                  <span className="font-mono text-xs">
                    {p.versions.length}
                    {p.newest && p.oldest && p.newest !== p.oldest && (
                      <span className="text-muted-foreground">
                        {" "}
                        · {p.oldest} → {p.newest}
                      </span>
                    )}
                  </span>
                  {p.incomparable > 0 && (
                    <span className="ml-2 text-2xs text-muted-foreground">
                      {p.incomparable} unorderable
                    </span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableCard>
    </div>
  );
}

/**
 * Every live waiver on the install, sorted by expiry — the aggregate a security
 * review asks for, which must not require walking forty app pages to assemble.
 */
function AllWaivers({
  waivers,
  enforcing,
}: {
  waivers: DependencyWaiver[];
  enforcing: boolean;
}) {
  return (
    <Section
      title="Live waivers"
      description={
        enforcing
          ? "Accepted risks, soonest to expire first. When one lapses, the next deploy is refused again."
          : "Accepted risks, soonest to expire first. When one lapses, its finding counts again."
      }
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>App</TableHead>
            <TableHead>Advisory</TableHead>
            <TableHead className="hidden @3xl:table-cell">Reason</TableHead>
            <TableHead>Expires</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {waivers.map((w) => (
            <TableRow key={w.id}>
              <TableCell>
                <Link
                  to={`/apps/${w.app_id}/dependencies`}
                  className="text-sm hover:text-primary hover:underline"
                >
                  {w.app_slug || w.app_id}
                </Link>
              </TableCell>
              <TableCell className="font-mono text-xs">
                {w.advisory_id}
                {w.package && (
                  <span className="text-muted-foreground"> · {w.package}</span>
                )}
              </TableCell>
              <TableCell className="hidden max-w-md text-xs @3xl:table-cell">
                {w.reason}
              </TableCell>
              <TableCell className="text-xs">
                <span
                  className={
                    expiresSoon(w.expires_at)
                      ? toneText.attention
                      : "text-muted-foreground"
                  }
                >
                  {expiresIn(w.expires_at)}
                </span>
                <p className="mt-0.5 text-muted-foreground">
                  {w.created_by_name || "an administrator"}
                </p>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Section>
  );
}

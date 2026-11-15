import { ParentCrumb } from "@/components/parent-crumb";
import { useState, useEffect, useMemo } from "react";
import { Disclosure } from "@/components/disclosure";

import {
  useParams,
  useNavigate,
  useLocation,
  Link,
  Navigate,
} from "react-router-dom";
import {
  useQuery,
  useQueryClient,
  useMutation,
  useIsMutating,
  useMutationState,
} from "@tanstack/react-query";
import { toast } from "sonner";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { Input } from "@/components/ui/input";

import { Skeleton } from "@/components/ui/skeleton";

import { StatusBadge } from "@/components/status-badge";
import { StatusDot } from "@/components/status-dot";
import { IconButton } from "@/components/icon-button";
import { Notice } from "@/components/notice";
import { DependenciesTab } from "@/components/dependencies-tab";
import { DeployConsole } from "@/components/deploy-console";
import { deployTimeline, expectedDeploy } from "@/lib/deploy-steps";
import { wakeAheadProps } from "@/lib/wake-ahead";

import { cpuTone, toneSurface, toneText, type Tone } from "@/lib/tone";
import { statColumns } from "@/lib/fact-grid";
import { StatCard } from "@/components/stat-card";

import { NotebookPanel } from "@/components/notebook-panel";
import { TableCard } from "@/components/table-card";
import { Fact, Section } from "@/components/section";
import { RelativeTime } from "@/components/relative-time";

import { AppIcon } from "@/components/app-icon";
import { FrameworkBadge } from "@/components/framework-badge";
import { TagChips } from "@/components/tag-chip";

import { UploadVersionDialog } from "@/components/upload-version-dialog";
import { DeleteAppDialog } from "@/components/delete-app-dialog";
import { StopAppDialog } from "@/components/stop-app-dialog";
import { stopNeedsConfirm } from "@/lib/stop-tier";
import { AppIntegrationsTab } from "@/components/app-integrations";
import { AutomationTab } from "@/components/automation-tab";
import { SharingTab } from "@/components/sharing-tab";

import { AppOwner } from "@/components/app-owner";
import { formatUptime } from "@/lib/time";
import { formatMB } from "@/lib/bytes";

import {
  Copy,
  AlertTriangle,
  ChevronDown,
  FileText,
  Building2,
  CheckCircle2,
  Clock,
  Cpu,
  ExternalLink,
  GitCommit,
  Globe,
  HardDrive,
  Loader2,
  LockKeyhole,
  MoreHorizontal,
  Play,
  RefreshCw,
  RotateCcw,
  SquareCode,
  Square,
  Trash2,
  Unlink,
  Upload,
} from "lucide-react";
import { copyToClipboard } from "@/lib/clipboard";
import { appDisplayPath, appShareUrl, appUrl } from "@/lib/app-url";
import {
  firstDeployFailed, lastDeployFailedOverServing, servingLatest,
} from "@/pages/app/deploy-state";
import { rememberApp } from "@/lib/recents";
import { cn } from "@/lib/utils";
import { useAuth } from "@/hooks/use-auth";
import { isBatchFramework } from "@/lib/frameworks";
import { api, isGitSource, refusalCodeOf, servingUploadedCode } from "@/lib/api";

import type {
  App,
  AppDependencyPolicy,
  AppDetail,
  AppMetrics,
  AppPolicy,
  AppSummary,
  AppPermissions,
  Deployment,
} from "@/lib/api";
import { QueryError } from "@/components/query-error";
import { AppAccessGate } from "@/components/app-access-gate";

import { NoticeStack } from "@/components/notice-stack";

import { useDocumentTitle } from "@/hooks/use-document-title";
import { useHashScroll } from "@/hooks/use-hash-scroll";

import { APP_MOVED_ANCHORS } from "@/lib/settings-index";
import {
  APP_TABS,
  LEGACY_APP_TABS,
  appTabsFor,
  type AppTab,
} from "@/lib/app-tabs";

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { DEPLOY_IN_PROGRESS, RESTART_IN_FLIGHT, deploymentsPollMs, headerPrimary } from "@/pages/app/state";
import { useAppLifecycle } from "@/pages/app/lifecycle";
import { ReachTab } from "@/pages/app/reach-tab";
import { AuditTab } from "@/pages/app/audit-tab";
import { StorageBlock } from "@/pages/app/storage-block";
import { UsageTab } from "@/pages/app/usage-tab";
import { LogsTab } from "@/pages/app/logs-tab";
import { DeploymentsTab } from "@/pages/app/deployments-tab";
import { SettingsTab } from "@/pages/app/settings-tab";
import { EventsTab } from "@/pages/app/events-tab";

// Keyed on the app, so every draft a tab holds — Settings' fields and its
// environment table, the memory limit, the repoint form — is thrown away with
// the app it was typed against. Without the key a navigation from one app to
// another (the command palette, a link in a tab) kept the same instance, and
// Save wrote app A's typed values into app B.
export function AppDetailPage() {
  const { id } = useParams<{ id: string }>();
  return <AppDetail key={id} />;
}

function AppDetail() {
  const { id, tab } = useParams<{ id: string; tab?: string }>();
  const { hash } = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const [redeploying, setRedeploying] = useState(false);
  const [uploadVersionOpen, setUploadVersionOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [stopOpen, setStopOpen] = useState(false);
  const [unlinkingGarth, setUnlinkingGarth] = useState(false);
  // Two places start this app — the Settings button and the un-applied-changes
  // notice on Overview — and the pill in the header has to report either. A
  // keyed mutation is what lets it, without lifting a boolean through three
  // components that do not otherwise care.
  const restartInFlight = useIsMutating({ mutationKey: ["restart", id] });

  /**
   * A start that is still happening after its request came back.
   *
   * The server answers 202 when an isolated start outlives its hold — the pod
   * is scheduled and the image is pulled long after the HTTP call returns — so
   * the mutation settles while the app is still on its way up. `useIsMutating`
   * drops to zero at that moment, and the pill reverted to the app's *stored*
   * status, which is still `stopped` or `crashed`, for as long as the next poll
   * took. So the last restart's own answer is read: a 202 keeps the pill on
   * *Restarting* and the poll fast until the status the server sends actually
   * moves, or the window runs out.
   */
  const restartAnswers = useMutationState<{
    status?: string;
    submittedAt: number;
  }>({
    filters: { mutationKey: ["restart", id] },
    select: (m) => ({
      status: (m.state.data as { status?: string } | undefined)?.status,
      submittedAt: m.state.submittedAt,
    }),
  });
  const lastRestart = restartAnswers[restartAnswers.length - 1];
  const [startWatch, setStartWatch] = useState<{
    since: number;
    from: string;
    /** Set by a timer once the window has passed — reading the clock during
     * render would be a different answer on every re-render. */
    expired?: boolean;
  } | null>(null);
  const startWatchOpen = startWatch !== null && !startWatch.expired;
  useEffect(() => {
    if (!startWatch || startWatch.expired) return;
    const since = startWatch.since;
    const timer = setTimeout(
      () => setStartWatch((w) => (w && w.since === since ? { ...w, expired: true } : w)),
      Math.max(0, since + RESTART_WATCH_MS - Date.now()),
    );
    return () => clearTimeout(timer);
  }, [startWatch]);

  const currentTab: AppTab =
    tab && APP_TABS.includes(tab as AppTab)
      ? (tab as AppTab)
      : (tab && LEGACY_APP_TABS[tab]) || "overview";

  const {
    data: detail,
    isLoading,
    error,
    refetch: refetchApp,
  } = useQuery<AppDetail>({
    queryKey: ["app", id],
    queryFn: () => api.getApp(id!),
    enabled: !!id,
    // Poll fast while a deploy is in flight so status flips live.
    // Poll fast while the platform is working — a deploy, or a start that
    // outlived its request and answered 202. Without the second case the pill
    // would sit on *Restarting* until something else refetched.
    refetchInterval: (query) => {
      const status = query.state.data?.app?.status;
      if (status === "building") return 3000;
      // Still on the status the start was asked from, and inside the window:
      // the platform is working and the pill is saying so.
      if (startWatchOpen && status === startWatch?.from) return 3000;
      return 15000;
    },
  });

  const app = detail?.app;

  // Opening the watch is a render's worth of work and belongs where the answer
  // arrives, not in an effect that would re-run on every poll.
  if (
    lastRestart?.status === "restarting" &&
    app &&
    startWatch?.since !== lastRestart.submittedAt
  ) {
    setStartWatch({ since: lastRestart.submittedAt, from: app.status });
  }
  // Closed as soon as the server says something else, so a start that lands in
  // three seconds does not hold the pill for the whole window.
  const startPending = startWatchOpen && app?.status === startWatch?.from;

  // 0 for a viewer and 0 when there is no workload to restart — the server
  // decides both, so nothing here has to re-derive who may act on it.
  const unappliedEnv = detail?.unapplied_env_changes ?? 0;
  useDocumentTitle(app?.name);
  // `#anchor` on arrival, once the tab that holds it has something to scroll
  // to. It is what makes `/apps/{id}/settings#source` from ⌘K land on the
  // branch field rather than at the top of the tab that holds it.
  useHashScroll(!!app);
  // Feeds the command palette's Recent group. localStorage only — see lib/recents.
  useEffect(() => {
    if (id) rememberApp(id);
  }, [id]);
  const metrics = detail?.metrics;

  // Who is asking, which decides the strip, the Deployments read below, and
  // every fact on this page derived from it. Above them because they wait on
  // it; the SharingTab it also gates re-checks per control.
  const { data: perms } = useQuery({
    queryKey: ["app-permissions", id],
    queryFn: () => api.getAppPermissions(id!),
    enabled: !!id,
  });
  const canShare =
    !!perms && (perms.can_manage_grants || perms.can_change_visibility);
  const canEdit = !!perms?.can_edit;
  // Start, restart and stop, through the one hook the app list's row menu and
  // this header both go through. See `pages/app/lifecycle.ts` for why they are
  // not in Settings' danger zone any more.
  const lifecycle = useAppLifecycle(id!, app);
  const isBatch = isBatchFramework(app?.framework);

  /**
   * Every build this app has had — **and a request only an editor may make.**
   *
   * `GET /apps/{id}/deployments` authorizes with `logsAction`, which is
   * `edit_content` rather than read, so for a viewer this fired on every page
   * load and answered 403. The list then defaulted to empty, and the Health
   * block on Overview rendered that emptiness as fact: *All 0 builds*, *Nothing
   * deployed yet*, *Last deploy: Never*, about an app that is serving in front
   * of them. A refusal read as an answer is worse than either.
   *
   * So it waits for `perms` and is never asked for at all by somebody who
   * cannot have it. The cost is that an owner's Health block lands one round
   * trip later; the deploy banner does not, because `building` is on the app
   * row and arrives with the detail.
   */
  const {
    data: deployments = [],
    isLoading: deploymentsLoading,
    error: deploymentsError,
    refetch: refetchDeployments,
  } = useQuery<Deployment[]>({
    queryKey: ["deployments", id],
    queryFn: () => api.listDeployments(id!),
    enabled: !!id && canEdit,
    refetchInterval: (query) => deploymentsPollMs(query.state.data, app?.status),
  });

  const activeDeployment = deployments.find((d) =>
    DEPLOY_IN_PROGRESS.has(d.status),
  );

  /**
   * The build this page has been watching, which outlives it finishing.
   *
   * **A console is a watcher.** It stays through the handoff — the success line
   * with its two links, or the failure with its pane and its step — because the
   * last act of watching a deploy is being told how it went. It does not appear
   * for a build that was already over when the page loaded: that result belongs
   * to the Deployments tab and to the app's status, and a console there would be
   * a fourth place the same outcome is reported (ADR §87). Tabbing away does not
   * lose it; a reload is somebody arriving after the fact.
   */
  const [watchedDeploy, setWatchedDeploy] = useState<string | null>(null);
  // Adjusting state during render is React's own answer to "a value changed and
  // some state derives from it", and it is the same pattern `search-input` uses;
  // an effect for this costs an extra render and would set state from inside one.
  if (activeDeployment && activeDeployment.id !== watchedDeploy) {
    setWatchedDeploy(activeDeployment.id);
  }
  const consoleDeployment =
    deployments.find((d) => d.id === watchedDeploy) ?? activeDeployment;

  // The row that owns the code actually executing. After a promotion these
  // differ, which is exactly why the Deployments tab distinguishes them.
  const serving =
    deployments.find((d) => d.serving) ?? deployments.find((d) => d.current);
  const latest = deployments[0];

  // A failed deploy leaves the previous release running, so the app stays
  // "running" and the apps list shows nothing wrong. Somebody who pushed a
  // commit an hour ago and sees a green app has no reason to suspect their
  // code never shipped. Computed here rather than in Overview because the
  // banner about it is above the tabs now (ADR §53 D6).
  //
  // Two sentences, not one, and `pages/app/deploy-state.ts` decides which. The
  // condition used to be `!serving || serving.id !== latest.id`, whose first
  // disjunct is the *never-served* case — so a first deploy that failed took
  // the branch reading "the app is still serving the previous release …
  // Nothing is down", three inches above a Health card saying "Nothing
  // deployed yet".
  const keptServing = lastDeployFailedOverServing(latest, serving);
  const firstFailed = firstDeployFailed(latest, serving);
  const lastDeployFailed = keptServing || firstFailed;
  // Where it died, when the row recorded a step. "It got as far as checking
  // dependencies" and "it got as far as starting" send somebody to two
  // different places; "it failed" sends them to the log to work it out.
  const failedStep = lastDeployFailed
    ? deployTimeline(latest)?.find((r) => r.state === "failed")
    : undefined;
  const uploadedOverGit = app ? servingUploadedCode(app, serving) : false;
  const primary = headerPrimary({
    status: app?.status ?? "",
    canEdit,
    locked: !!app?.locked_at,
    batch: isBatch,
    firstDeployFailed: firstFailed,
  });

  // The install no longer deploys from where this app's code lives. Read from
  // the app's own policy block rather than from /system/settings, which 404s to
  // the owner who needs the answer.
  const sourceBlocked = detail?.policy ? !detail.policy.source_allowed : false;

  // What the dependency policy has decided about this app. Absent while the
  // feature is off, and absent for a viewer — so every read of it is optional
  // and nothing below has to branch on the mode.
  const deps = detail?.policy?.dependencies;

  /**
   * Which tabs this caller actually has — the rule, its reasoning and its test
   * are in `@/lib/app-tabs`, and this page is one of its two consumers.
   *
   * The strip was conditional and the bodies were conditional on something
   * else, and the URL validator checked the *name* in the URL rather than
   * whether this caller had that tab — so `/apps/{id}/dependencies`, opened by
   * a viewer or by an owner on an install with the dependency policy off,
   * rendered a header, a strip with nothing selected, and an empty page (ADR
   * §53 D5).
   *
   * Both inputs are already in hand before the strip renders, so this costs the
   * page nothing — and **the strip reads it too**. It used to re-derive the
   * same rule from `canEdit`/`canShare`/`deps` inline, one `&&` per trigger,
   * which is a second copy in the one place the guard in `app-tabs.test.ts`
   * cannot see.
   */
  const availableTabs = useMemo(() => appTabsFor(perms, !!deps), [perms, deps]);

  /**
   * Whether Settings has been opened on this visit.
   *
   * The panel is kept mounted once it has, because leaving the tab used to
   * unmount the form and take every unsaved draft with it — six fields, the
   * whole environment table and a git credential, gone for looking at the log
   * and coming back, with nothing said. It is not kept mounted from the start:
   * the tab is the largest surface on this page, and mounting it for everybody
   * who opens an app to read its Overview is a cost paid by the people who
   * never go there.
   */
  const [settingsVisited, setSettingsVisited] = useState(currentTab === "settings");
  if (currentTab === "settings" && !settingsVisited) setSettingsVisited(true);

  function onTabChange(value: unknown) {
    const v = value as string;
    navigate(v === "overview" ? `/apps/${id}` : `/apps/${id}/${v}`);
  }

  // Reversible — the next deploy from a Garth session links it again — and
  // audited, so it acts at once and toasts, with no dialog.
  async function handleUnlinkGarth() {
    setUnlinkingGarth(true);
    try {
      await api.unlinkGarth(id!);
      toast.success("Unlinked from Garth");
      queryClient.invalidateQueries({ queryKey: ["app", id] });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t unlink from Garth");
    } finally {
      setUnlinkingGarth(false);
    }
  }

  async function handleRedeploy() {
    if (!id) return;
    setRedeploying(true);
    try {
      const deploy = await api.deploy(id);
      // Redeploying an extension follows the gallery, so which version this
      // build is taking is the first thing to say: it is a different act from
      // rebuilding the code already running, and the person pressed one button
      // for both.
      const taking =
        deploy.gallery_version &&
        deploy.gallery_version !== detail?.policy?.gallery?.installed_version
          ? deploy.gallery_version
          : "";
      toast.success(
        taking
          ? `Updating to ${taking}`
          : "Redeploying",
      );
      queryClient.invalidateQueries({ queryKey: ["app", id] });
      queryClient.invalidateQueries({ queryKey: ["deployments", id] });
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : "Couldn’t redeploy",
      );
    } finally {
      setRedeploying(false);
    }
  }

  // An anchor whose section has moved lands where the section went. Fourteen
  // became six and API keys changed tab entirely, and every link to one of them
  // — in a bookmark, in another page's copy — still means what it meant. Below
  // the hooks, because a redirect is a render and the hooks above it are not
  // optional; and scoped to the Settings tab, which is the only tab whose
  // sections moved.
  const movedAnchor =
    currentTab === "settings"
      ? APP_MOVED_ANCHORS[hash.replace(/^#/, "")]
      : undefined;
  if (movedAnchor) {
    return (
      <Navigate
        to={`/apps/${id}/${movedAnchor.tab}#${movedAnchor.anchor}`}
        replace
      />
    );
  }

  // A tab this caller does not have sends them to the app, not to an apology:
  // whether this app has a dependency verdict is not a secret, and somebody
  // sent a link to a tab they cannot open should land on the app.
  //
  // **Both inputs, not one.** Availability is permissions *and* capabilities,
  // and the two queries land independently — so deciding on `perms` alone
  // bounced every link to the Dependencies tab back to Overview for as long as
  // `detail` was still in flight, which is every cold load.
  if (
    perms &&
    detail &&
    currentTab !== "overview" &&
    !availableTabs.has(currentTab)
  ) {
    return <Navigate to={`/apps/${id}`} replace />;
  }

  if (isLoading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-4 w-40" />
        <div className="flex items-center gap-3">
          <Skeleton className="h-11 w-11 rounded-lg" />
          <div className="space-y-2">
            <Skeleton className="h-6 w-56" />
            <Skeleton className="h-4 w-36" />
          </div>
        </div>
        <Skeleton className="h-9 w-96" />
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      </div>
    );
  }

  if (!app && refusalCodeOf(error) === "deny_list_only") {
    // The `list` rung: this person was told the app exists by an owner who
    // wrote them a grant saying so, and the API says so in the refusal rather
    // than pretending there is nothing here. The gate is the request surface —
    // built from the listing's payload, because that is the whole of what the
    // rung discloses.
    return <AppAccessGate appId={id!} />;
  }

  if (!app) {
    // "App not found" was the answer to every failure here, including a 500 and
    // a dropped connection — and 404 is the *deliberate* answer this API gives
    // for an app somebody may not read (ADR §1), so the two are not even
    // rare-versus-common. Telling an owner their app is gone when the platform
    // merely stumbled is the kind of wrong answer people act on.
    return (
      <div>
        <QueryError
          title={
            error ? "This app could not be read" : "There is no app at this address"
          }
          error={error}
          fallback="It may have been deleted, or you may not have access to it."
          onRetry={() => void refetchApp()}
        />
        <div className="flex justify-center">
          <Button variant="outline" size="sm" onClick={() => navigate("/")}>
            Back to Apps
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <ParentCrumb to="/" parent="Apps" current={app.name} />

      <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-3">
          <AppIcon name={app.name} seed={app.slug} size="lg" />
          {/* `min-w-0` on every ancestor and `flex-wrap` on the badge row. A
              flex item's implicit minimum is its content, so a long app name
              beside four `shrink-0` badges had a floor wider than the page and
              pushed the whole document sideways — the one place in the product
              measured with horizontal overflow. The name truncates; the badges
              wrap. */}
          <div className="min-w-0">
            <div className="flex min-w-0 flex-wrap items-center gap-2.5">
              <h1 className="min-w-0 truncate text-2xl font-semibold tracking-tight">
                {app.name}
              </h1>
              {/* D10: *Restarting* is a rendering of an in-flight start, not a
                  stored status — `sleeping`, `stopped`, `crashed` and `running`
                  keep meaning what they mean. It borrows `building`'s tone
                  because it is the same kind of fact: the platform is working,
                  and nothing is wrong. The colour still has a word beside it. */}
              {restartInFlight > 0 || startPending ? (
                <StatusBadge status="building" label="Restarting" />
              ) : (
                <StatusBadge status={app.status} />
              )}
              <FrameworkBadge framework={app.framework} />
              <TagChips
                tags={detail?.tags?.filter((t) => t.kind === "custom")}
                max={4}
              />
            </div>
            {/* The address, and a way to take it. Copying an app's URL is the
                commonest thing anybody does with this page after opening the
                app — it is how the app gets into a message, a ticket or a
                dashboard — and it was on the apps list's row menu and nowhere
                on the app's own page, including the tab about who can reach
                it. */}
            {isBatch ? (
              <p className="text-sm text-muted-foreground">Batch app — runs jobs, serves no pages</p>
            ) : (
            <div className="flex items-center gap-1">
              <a
                href={appUrl(app)}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
              >
                {appDisplayPath(app)}
                <ExternalLink className="h-3 w-3" />
              </a>
              <IconButton
                size="icon-xs"
                className="text-muted-foreground"
                label={`Copy the address of ${app.name}`}
                onClick={() => void copyToClipboard(appShareUrl(app), "URL")}
              >
                <Copy className="h-3 w-3" />
              </IconButton>
            </div>
            )}
          </div>
        </div>
        {/* Wrapping: four controls and 411px of them, in a row that pushed the
            page sideways on anything narrower than a small laptop. */}
        {/* One variant per level: the primary is what this app's state asks
            for next (`headerPrimary`, `pages/app/state.ts`), the rest are
            outlined secondaries, and ⋯ is icon-only. Start-aligned on a phone,
            where the row sits under the title rather than beside it. */}
        <div className="flex flex-wrap items-center gap-2 sm:justify-end">
          {primary === "start" && (
            <Button size="sm" onClick={lifecycle.restart} disabled={lifecycle.restarting}>
              <Play className="h-3.5 w-3.5 mr-1.5" />
              {lifecycle.restarting ? "Starting…" : "Start app"}
            </Button>
          )}
          {primary === "restart" && (
            <Button size="sm" onClick={lifecycle.restart} disabled={lifecycle.restarting}>
              <RefreshCw className="h-3.5 w-3.5 mr-1.5" />
              {lifecycle.restarting ? "Restarting…" : "Restart app"}
            </Button>
          )}
          {primary === "build_log" && (
            <Link
              to={`/apps/${id}/logs?source=build${latest ? `&build=${latest.id}` : ""}`}
              className={buttonVariants({ size: "sm" })}
            >
              <FileText className="h-3.5 w-3.5 mr-1.5" />
              View build log
            </Link>
          )}
          {/* A real anchor, like the identical control on the apps list —
              which is where the argument for it is written. As a button it
              could not be middle-clicked into a background tab, ⌘-clicked, or
              right-clicked for its address, and `window.open` is what a popup
              blocker blocks. Outlined when the state put something else first,
              so the address is never more than one click away. */}
          {!isBatch && (
            <a
              href={appUrl(app)}
              target="_blank"
              rel="noopener noreferrer"
              {...wakeAheadProps(app)}
              className={buttonVariants({ size: "sm", variant: primary === "open" ? "default" : "outline" })}
            >
              <ExternalLink className="h-3.5 w-3.5 mr-1.5" />
              Open
            </a>
          )}
          {/* Both write, and both are `edit_content` on the server. A viewer
              was offered them and refused by the API — the same shape the apps
              list stopped shipping when its ⋯ menu learned `can_edit`. An
              uploaded app has two ways to deploy and they are one menu, so the
              header holds one Deploy rather than two peers; a Git app has one
              way and keeps the plain button. No `title` explaining a refusal:
              a disabled control swallows the pointer, and the reason is in the
              notice below. */}
          {canEdit && !isGitSource(app) && (
            <>
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={redeploying || app.status === "building"}
                    >
                      {redeploying ? (
                        <Loader2 className="h-3.5 w-3.5 mr-1.5 animate-spin" />
                      ) : (
                        <Upload className="h-3.5 w-3.5 mr-1.5" />
                      )}
                      {redeploying ? "Redeploying…" : "Deploy"}
                      <ChevronDown className="h-3.5 w-3.5 ml-1" />
                    </Button>
                  }
                />
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setUploadVersionOpen(true)}>
                    <Upload className="mr-2 h-3.5 w-3.5" /> Upload new version…
                  </DropdownMenuItem>
                  <DropdownMenuItem disabled={sourceBlocked} onClick={handleRedeploy}>
                    <RotateCcw className="mr-2 h-3.5 w-3.5" /> Redeploy current version
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
              <UploadVersionDialog
                appId={app.id}
                open={uploadVersionOpen}
                onOpenChange={setUploadVersionOpen}
              />
            </>
          )}
          {canEdit && isGitSource(app) && (
            <Button
              variant="outline"
              size="sm"
              onClick={handleRedeploy}
              disabled={
                redeploying || app.status === "building" || sourceBlocked
              }
            >
              {redeploying ? (
                <Loader2 className="h-3.5 w-3.5 mr-1.5 animate-spin" />
              ) : (
                <RotateCcw className="h-3.5 w-3.5 mr-1.5" />
              )}
              {redeploying ? "Redeploying…" : "Redeploy"}
            </Button>
          )}
          {/* Built by the server, and only sent to an owner or editor on an
              install with a Garth address, for an app a Garth session
              deployed — so it is absent, not disabled, everywhere else
              (ADR §158). It only navigates: Garth confirms
              before it does anything. */}
          {detail?.open_in_garth && (
            <a
              href={detail.open_in_garth}
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ size: "sm", variant: "outline" })}
            >
              <SquareCode className="h-3.5 w-3.5 mr-1.5" />
              Open in Garth
            </a>
          )}
          {/* Starting, restarting and stopping — the three controls the app
              *list* has offered on every row's ⋯ menu the whole time, while the
              app's own page filed them under a red "Danger zone" at the bottom
              of Settings. They are reversible and take no confirmation (the
              control beside them is the undo), so they are a menu here rather
              than three more buttons in a header that already has three. */}
          {canEdit && (
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <IconButton variant="outline" label={`More actions for ${app.name}`}>
                    <MoreHorizontal className="h-4 w-4" />
                  </IconButton>
                }
              />
              <DropdownMenuContent align="end">
                {/* A batch app has no service: Start, Restart and Stop are
                    absent, not disabled (ADR §107). */}
                {!isBatch && (
                <DropdownMenuItem
                  disabled={lifecycle.restarting || lifecycle.locked}
                  onClick={lifecycle.restart}
                >
                  {lifecycle.wasDown ? (
                    <Play className="mr-2 h-3.5 w-3.5" />
                  ) : (
                    <RefreshCw className="mr-2 h-3.5 w-3.5" />
                  )}
                  {lifecycle.restarting
                    ? lifecycle.wasDown
                      ? "Starting…"
                      : "Restarting…"
                    : lifecycle.wasDown
                      ? "Start app"
                      : "Restart app"}
                </DropdownMenuItem>
                )}
                {/* Nothing to stop on an app that is already down: the verb
                    above already says Start. */}
                {!isBatch && app.status !== "stopped" && (
                  <DropdownMenuItem
                    disabled={lifecycle.stopping || lifecycle.locked}
                    // Same tier as the apps list and Processes: a confirm
                    // unless the app is this person's alone.
                    onClick={() =>
                      stopNeedsConfirm(app.owner_id, user?.id) ? setStopOpen(true) : lifecycle.stop()
                    }
                  >
                    <Square className="mr-2 h-3.5 w-3.5" />
                    {lifecycle.stopping
                      ? "Stopping…"
                      : stopNeedsConfirm(app.owner_id, user?.id)
                        ? "Stop app…"
                        : "Stop app"}
                  </DropdownMenuItem>
                )}
                {/* Present exactly when the button is: an app a Garth
                    session deployed, shown to its editors (ADR §158). */}
                {detail?.open_in_garth && (
                  <DropdownMenuItem
                    disabled={unlinkingGarth}
                    onClick={handleUnlinkGarth}
                  >
                    <Unlink className="mr-2 h-3.5 w-3.5" />
                    {unlinkingGarth ? "Unlinking…" : "Unlink from Garth"}
                  </DropdownMenuItem>
                )}
                {perms?.can_delete && (
                  <>
                    <DropdownMenuSeparator />
                    {/* The same dialog the apps list opens, not a navigation
                        to the Settings tab's anchor. The same menu item in the
                        same position did two different things, and the slow
                        door was the one on the page where somebody has just
                        looked at the app and decided. The Danger zone row
                        stays where it is — both doors open this. */}
                    <DropdownMenuItem
                      variant="destructive"
                      onClick={() => setDeleting(true)}
                    >
                      <Trash2 className="mr-2 h-3.5 w-3.5" /> Delete app…
                    </DropdownMenuItem>
                  </>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          )}
          <StopAppDialog
            appId={app.id}
            appName={app.name}
            open={stopOpen}
            onOpenChange={setStopOpen}
            onStopped={() => {
              queryClient.invalidateQueries({ queryKey: ["app", app.id] });
              queryClient.invalidateQueries({ queryKey: ["apps"] });
            }}
          />
          {deleting && (
            <DeleteAppDialog
              app={app}
              open={deleting}
              onOpenChange={(o) => {
                if (!o) setDeleting(false);
              }}
              // The list stays put after a delete; this page cannot — the app
              // it is about is gone.
              onDeleted={() => navigate("/")}
            />
          )}
        </div>
      </div>

      {/*
        Every banner about this app's state, in one place and capped at two
        (ADR §53 D6).

        Two of these used to live inside Overview — running uploaded code over
        a git app, and the deploy that failed while an older release keeps
        serving — which meant *where* a message about this app appeared
        depended on which message it was, and on a 757-pixel Overview those two
        pushed its entire content below the fold.

        The relative order below is the deliberate one and is unchanged: the
        dependency notices go last because neither of them means the app
        stopped working, and the malware banner suppresses the blocked-deploys
        one so that neither gets scrolled past. What `NoticeStack` adds is a
        ceiling, and it sorts only on whether the app is still serving.
      */}
      <NoticeStack
        notices={[
          // The lock leads, above even a build in flight, and it is the one
          // notice that has to appear on every tab.
          //
          // Everything else in this stack is something the app's own people can
          // act on; this is the one thing they cannot, and without it the
          // symptoms — stopped, deploys refused, jobs refused — arrive with no
          // cause attached. The Access tab carries the control and the full
          // record; this says the word "locked" wherever the reader happens to
          // be standing.
          !!app.locked_at && {
            key: "locked",
            serving: false,
            node: (
              <Notice
                tone="danger"
                size="md"
                title={
                  app.locked_source === "dependency"
                    ? "This install’s dependency policy locked this app"
                    : "An administrator has locked this app"
                }
                // The sentence is for everybody standing here; the *record* is
                // on the Access tab, which a viewer does not have. A link into
                // a tab that bounces is worse than no link.
                action={
                  canShare ? (
                    <Link
                      to={`/apps/${id}/access#lock`}
                      className={buttonVariants({
                        variant: "outline",
                        size: "sm",
                      })}
                    >
                      View lock details
                    </Link>
                  ) : undefined
                }
              >
                {app.locked_source === "dependency"
                  ? "It’s stopped and refuses visitors and jobs. A deploy that no longer carries the flagged package unlocks it. Nothing has been deleted."
                  : "It’s stopped and refuses visitors, deploys and jobs until unlocked. Nothing has been deleted."}
              </Notice>
            ),
          },
          // An administrator has restricted where apps may come from, and this
          // one no longer qualifies. Stated here rather than left to the
          // Redeploy button's 403, because the app is *fine* — it is still
          // serving, and rollback still works. What has stopped is new code.
          // What has stopped is deploying new code, which is an editor's
          // concern and names an editor's controls.
          canEdit &&
            sourceBlocked && {
              key: "source-blocked",
              serving: true,
              node: (
                <Notice size="md" title={detail?.policy?.source_reason}>
                  The app keeps serving, and rollback still works. New deploys
                  are refused.
                </Notice>
              ),
            },
          // First whenever it is on: it is the only one of the seven that is
          // changing under the reader, and a build in flight is the most
          // time-sensitive thing this page ever says.
          //
          // **A viewer gets the sentence and nothing else.** The progress
          // stream, the log socket and the deployments list are all gated by
          // `logsAction`, which is stricter than read, so a console would be a
          // frame around three refusals. The console is absent for them rather
          // than empty (ADR §87).
          consoleDeployment || app.status === "building"
            ? {
                key: "deploying",
                serving: false,
                // One event with the failed-deploy banner below: the console
                // is the step and the output, the banner is the consequence,
                // and between them they were taking the whole ceiling.
                about: "deploy",
                node: canEdit ? (
                  <DeployConsole
                    appId={id!}
                    appSlug={app.slug}
                    deployment={consoleDeployment}
                    expected={expectedDeploy(deployments, consoleDeployment?.id)}
                    onRedeploy={handleRedeploy}
                    redeploying={redeploying}
                  />
                ) : (
                  <Notice tone="info" size="md" title="A deploy is in progress">
                    The app keeps serving the previous deployment until the new
                    one is healthy.
                  </Notice>
                ),
              }
            : false,
          app.status === "crashed" && {
            key: "crashed",
            serving: false,
            node: (
              <Notice
                tone="danger"
                size="md"
                // The fact is everybody's — it is on the status pill beside the
                // name — but the logs are the editors'. A viewer gets the
                // sentence without a link into a tab they do not have, and
                // without being told to go and read something they cannot.
                action={
                  canEdit ? (
                    <Link
                      to={`/apps/${id}/logs`}
                      className={buttonVariants({
                        variant: "outline",
                        size: "sm",
                      })}
                    >
                      View logs
                    </Link>
                  ) : undefined
                }
              >
                This app has crashed.
                {canEdit && " The process logs usually show why."}
              </Notice>
            ),
          },
          // The asymmetry the builder documents and nothing surfaced until now:
          // a failed deploy leaves the previous release running, so the app
          // stays "running", the apps list shows nothing wrong, and the only
          // record is a status on a row on another tab.
          // Unconditional, and it outlives the console: the console is only
          // there for somebody who was watching, and this is the banner that
          // meets everybody who was not. A failed deploy leaves the app
          // "running" with last week's code, and this is the only thing on the
          // install that says so. The two do not repeat each other — the
          // console names the step and holds the output, this names the
          // consequence.
          lastDeployFailed && {
            key: "deploy-failed",
            serving: false,
            about: "deploy",
            node: (
              <Notice
                tone="danger"
                size="md"
                title={
                  keptServing
                    ? "The last deploy failed. The previous deployment is still serving."
                    : "The first deploy failed. Nothing is serving yet."
                }
                action={
                  <Link
                    to={`/apps/${id}/logs?source=build&build=${latest?.id}`}
                    className={buttonVariants({
                      variant: "outline",
                      size: "sm",
                    })}
                  >
                    View build log
                  </Link>
                }
              >
                {keptServing
                  ? "Your new code is not live."
                  : "Nothing is served until a build succeeds."}
                {failedStep && (
                  <>
                    {" "}
                    It got as far as{" "}
                    <strong className="font-medium">{failedStep.label}</strong>.
                  </>
                )}
              </Notice>
            ),
          },
          // Saved variables the running workload was not started with. Railway's
          // staged changes: the fact lives on the app row, so it survives a
          // page reload and is there for whoever opens the app next — which is
          // the whole reason it is not the toast it replaced, one that named an
          // action ("redeploy to apply them") and was gone in four seconds.
          //
          // In this stack rather than inside Overview, because D6 settled that:
          // a notice about the app lives above the tabs, and the two that used
          // to sit inside Overview are exactly what pushed its content below
          // the fold. `serving: true` because nothing is down — the app is
          // running, just not on the newest values.
          unappliedEnv > 0 && {
            key: "unapplied-env",
            serving: true,
            node: (
              <UnappliedEnvNotice appId={id!} app={app} count={unappliedEnv} />
            ),
          },
          // What is running did not come from the repository this page names —
          // somebody pushed a working directory with `toft deploy`. Not an error
          // and not a warning: a state with an expiry date, and the page's job
          // is to make sure nobody meets it by surprise.
          uploadedOverGit && {
            key: "uploaded-over-git",
            serving: true,
            node: (
              <Notice
                tone="info"
                size="md"
                title="This deployment was pushed from a machine"
              >
                The next deploy from{" "}
                <span className="font-mono">
                  {app.branch || "the default branch"}
                </span>{" "}
                replaces it with a build from the repository.
              </Notice>
            ),
          },
          // The dependency pair, last, because neither means the app stopped
          // working. Exactly two conditions earn a banner and the budget is the
          // reason the first one still works: a moderate transitive CVE with no
          // fix is on nearly every Node app, and a banner for it would be
          // permanent, identical on forty apps, and scrolled past within a week
          // — after which the malware banner is scrolled past too.
          deps?.malware_serving && {
            key: "malware",
            serving: true,
            node: (
              <Notice
                tone="danger"
                size="md"
                title="A package in the serving deployment is reported as malicious"
                action={
                  <Link
                    to={`/apps/${id}/dependencies`}
                    className={buttonVariants({
                      variant: "outline",
                      size: "sm",
                    })}
                  >
                    View dependencies
                  </Link>
                }
              >
                The app is still serving. The Dependencies tab names the
                package.
              </Notice>
            ),
          },
          !deps?.malware_serving &&
            deps?.deploys_blocked && {
              key: "deploys-blocked",
              serving: true,
              node: (
                <Notice
                  tone="attention"
                  size="md"
                  title="This app’s next deploy will be refused"
                  action={
                    <Link
                      to={`/apps/${id}/dependencies`}
                      className={buttonVariants({
                        variant: "outline",
                        size: "sm",
                      })}
                    >
                      View dependencies
                    </Link>
                  }
                >
                  The app keeps serving, and rollback still works.
                </Notice>
              ),
            },
        ]}
      />

      {/*
        Eight tabs, down from ten, and the two that went were merged rather than
        deleted — a tab count is not worth losing a surface over.

        **Preview** is now a disclosure at the head of Overview. It was the
        weakest tab in the set: an iframe of your own app inside a page full of
        Toft's chrome, when "Open" in the header does the same thing
        better. It stays because previewing a path without leaving is
        occasionally the point; it stops being a top-level destination because
        it almost never is. It leads the tab rather than closing it because it
        is the one thing on Overview you *do* something with — everything else
        there is a fact to read.

        **Events** now leads the Logs tab. Both answer one question — what has
        this app been doing, and why did it break — and splitting them meant
        somebody debugging a crash checked two tabs to assemble one story: the
        platform's record of the failure in one, the process output around it in
        the other.

        Access and Integrations stay their own tabs. The obvious next merge is
        into Settings, and it is the wrong one: Settings is already the largest
        surface here, and burying who-can-see-this inside it is how permission
        management stops being reviewed.
      */}
      <Tabs value={currentTab} onValueChange={onTabChange}>
        <TabsList variant="line">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          {/* Logs and Deployments are editors' tabs, not read-only views: both
              endpoints authorize with `logsAction`, which is deliberately
              stricter than read because build logs are the build environment's
              stdout and runtime logs are whatever the app printed. */}
          {availableTabs.has("logs") && <TabsTrigger value="logs">Logs</TabsTrigger>}
          {availableTabs.has("deployments") && (
            <TabsTrigger value="deployments">Deployments</TabsTrigger>
          )}
          <TabsTrigger value="usage">Usage</TabsTrigger>
          {availableTabs.has("automation") && (
            <TabsTrigger value="automation">Automation</TabsTrigger>
          )}
          {availableTabs.has("dependencies") && deps && (
            <TabsTrigger value="dependencies">
              Dependencies
              {/* Absent at zero — a tab that always reads (0) is noise. What it
                  counts is what this install said it cares about, never the raw
                  total: a badge reading 31 on every app conveys nothing but
                  that Node exists. */}
              {deps.counts.counted > 0 && (
                <span
                  className={cn(
                    "ml-1.5 rounded-full px-1.5 py-0.5 text-2xs font-medium",
                    toneSurface[
                      deps.counts.malware > 0 || deps.deploys_blocked
                        ? "danger"
                        : "attention"
                    ],
                  )}
                >
                  {deps.counts.counted}
                </span>
              )}
            </TabsTrigger>
          )}
          {availableTabs.has("integrations") && (
            <TabsTrigger value="integrations">Integrations</TabsTrigger>
          )}
          {/* Everything the platform has given this app, assembled. Beside
              Dependencies and on the same gate: both are reads a viewer does
              not get, because a manifest and an inventory are each a map of
              what to attack. */}
          {availableTabs.has("reach") && <TabsTrigger value="reach">Reach</TabsTrigger>}
          {availableTabs.has("audit") && <TabsTrigger value="audit">Audit</TabsTrigger>}
          {availableTabs.has("access") && <TabsTrigger value="access">Access</TabsTrigger>}
          {availableTabs.has("settings") && (
            <TabsTrigger value="settings">Settings</TabsTrigger>
          )}
        </TabsList>

        <TabsContent value="overview">
          <OverviewTab
            appId={id!}
            app={app}
            detail={detail}
            deployments={deployments}
            deploymentsLoading={deploymentsLoading}
            deploymentsError={deploymentsError}
            refetchDeployments={() => void refetchDeployments()}
            metrics={metrics}
            perms={perms}
            batch={isBatch}
          />
        </TabsContent>
        <TabsContent value="usage">
          <UsageTab appId={id!} canSeeViewers={!!perms?.can_manage_grants} batch={isBatch} />
        </TabsContent>
        <TabsContent value="automation">
          {canEdit && (
            <AutomationTab appId={id!} appSlug={app.slug} canEdit
              filesOnly={app.framework === "static"} />
          )}
        </TabsContent>
        <TabsContent value="logs">
          {/* Both halves are edit-gated, and so is the tab that holds them:
              Events excerpts build logs and git stderr, and the log endpoints
              refuse a viewer outright. */}
          {/* The log leads. This tab is named for it and ADR §87 is about the
              switch between its two halves — and Events reads up to a hundred
              rows, so on an app with a history the run/build switch and the
              pane started a hundred rows down the page. Events keeps its place
              *in the reading order* (what broke, then what it printed) by
              being the thing you scroll to rather than the thing you scroll
              past. */}
          {canEdit && (
            <>
              <LogsTab
                appId={id!}
                appSlug={app.slug}
                deployments={deployments}
                deploymentsLoading={deploymentsLoading}
              />
              <EventsTab appId={id!} />
            </>
          )}
        </TabsContent>
        <TabsContent value="deployments">
          {canEdit && (
            <DeploymentsTab
              appId={id!}
              deployments={deployments}
              loading={deploymentsLoading}
              error={deploymentsError}
              onRetry={() => void refetchDeployments()}
              canEdit
            />
          )}
        </TabsContent>
        <TabsContent value="dependencies">
          {canEdit && <DependenciesTab appId={id!} appName={app.name} />}
        </TabsContent>
        <TabsContent value="reach">{canEdit && <ReachTab appId={id!} />}</TabsContent>
        <TabsContent value="audit">{canEdit && <AuditTab appId={id!} />}</TabsContent>
        <TabsContent value="integrations">
          {canEdit && (
            <AppIntegrationsTab
              appId={id!}
              isAdmin={user?.system_role === "system_admin"}
              isStatic={app.framework === "static"}
            />
          )}
        </TabsContent>
        <TabsContent value="access">
          <SharingTab
            appId={id!}
            app={app}
          />
        </TabsContent>
        <TabsContent value="settings" keepMounted={settingsVisited}>
          {canEdit && (
            <SettingsTab
              appId={id!}
              app={app}
              serverEnvVars={detail?.env_vars ?? []}
              hasGitToken={detail?.has_git_token ?? false}
              gitConnection={detail?.git_connection ?? null}
              appTags={detail?.tags ?? []}
              policy={detail?.policy}
              runtime={detail?.runtime}
              ownerLogin={detail?.owner?.login}
            />
          )}
        </TabsContent>
      </Tabs>
    </div>
  );
}

// ── Preview Tab ──────────────────────────────────────────────

function PreviewTab({ app }: { app: App }) {
  const [path, setPath] = useState("/");
  const iframeSrc = appUrl(app, path);

  function reload() {
    const iframe = document.getElementById("app-preview") as HTMLIFrameElement;
    if (iframe) iframe.src = iframeSrc;
  }

  return (
    <div className="space-y-3 pt-4">
      <div className="flex items-center gap-2">
        <div className="flex min-w-0 flex-1 items-center gap-1.5 rounded-md border border-input px-3 py-1.5 text-sm font-mono text-muted-foreground">
          <span className="shrink-0 whitespace-nowrap">
            {appDisplayPath(app)}
          </span>
          <Input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            className="h-6 border-0 bg-transparent p-0 text-sm font-mono shadow-none focus-visible:ring-0"
            placeholder="/"
            aria-label="Path to preview"
            onKeyDown={(e) => {
              if (e.key === "Enter") reload();
            }}
          />
        </div>
        <Button variant="outline" size="sm" onClick={reload}>
          Go
        </Button>
        <a
          href={iframeSrc}
          target="_blank"
          rel="noopener noreferrer"
          aria-label="Open in new tab"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          <ExternalLink className="h-3.5 w-3.5" />
        </a>
      </div>
      <div className="overflow-hidden rounded-lg border border-border/60">
        <iframe
          id="app-preview"
          src={iframeSrc}
          className="w-full bg-white"
          style={{ height: "70vh" }}
          title="App preview"
        />
      </div>
    </div>
  );
}

// ── Overview Tab ──────────────────────────────────────────────

/**
 * Overview: the hub this page is already the address of.
 *
 * Every link, every row click and every breadcrumb lands here, and it behaved
 * like a leftover. It measured **757 pixels in a 1,200-pixel window — the
 * shortest of nine tabs** — and a third of it was a second rendering of the tab
 * next door: "Recent deployments" was the first five rows of the Deployments
 * tab with the same badges and the same Current/Serving logic, and "Serving
 * now" was that tab's header facts. Meanwhile the facts somebody actually
 * arrives wanting — *who owns this, who can see it, is it current, is anybody
 * using it* — were on four other tabs or on none. `GET /apps/{id}` returned
 * `owner` on every load and the page rendered it **nowhere**; the apps list has
 * an Owner column, so the row you clicked told you who owned the app and
 * opening it took the fact away (ADR §53 D4).
 *
 * Three rules, inherited verbatim from ADR §50's admin hub and not
 * renegotiated here:
 *
 *   - **No tile mutates.** Redeploy, Stop, Rollback and Revoke stay on the
 *     pages that own them, with their confirmations and their audit rows.
 *   - **No request of its own.** Every fact comes from a query this page
 *     already makes. `summary` is four figures that travel with the app for
 *     exactly this reason.
 *   - **A fact true only by omission is not a fact.** *0 restarts* on an app
 *     that has never started, *no findings* on an install with the dependency
 *     policy off, and *nobody has access* on a public app are the three this
 *     refuses to print.
 */

/**
 * How long the page keeps calling a 202 start "in flight" before it stops
 * claiming to know.
 *
 * A ceiling rather than a guess at the duration: a pod that never schedules is
 * a real outcome, and a pill that read *Restarting* for ever would be the same
 * lie in the other direction. Two minutes covers an image pull; past it the
 * stored status is the honest thing to show.
 */
const RESTART_WATCH_MS = 120_000;

/**
 * Saved variables that the running workload was not started with.
 *
 * Railway's staged changes, in this product's idiom. The fact lives on the app
 * row and comes back with the detail response, so it survives a page reload and
 * is there for whoever opens the app next — which is the whole reason it is not
 * a toast. A toast is gone in four seconds, which is before the person has
 * decided, and the one this replaced named an action ("redeploy to apply them")
 * that they then had to go and find.
 *
 * The button is in the notice because the notice is where the decision is made.
 * It is the same restart as the one on Settings, including its 202: an isolated
 * start is a pod schedule and an image pull, and nothing here may imply it is
 * instant.
 */
/**
 * Saved variables that the running workload was not started with.
 *
 * Railway's staged changes, in this product's idiom. The fact lives on the app
 * row and comes back with the detail response, so it survives a page reload and
 * is there for whoever opens the app next — which is the whole reason it is not
 * a toast. A toast is gone in four seconds, which is before the person has
 * decided, and the one this replaced named an action ("redeploy to apply them")
 * they then had to go and find.
 *
 * The button is in the notice because the notice is where the decision is made.
 * It is the same restart as the one on Settings, including its 202: an isolated
 * start is a pod schedule and an image pull, and nothing here may imply it is
 * instant.
 */
function UnappliedEnvNotice({
  appId,
  app,
  count,
}: {
  appId: string;
  app: App;
  count: number;
}) {
  const queryClient = useQueryClient();

  // Keyed, so the header's pill can see it. Both restart buttons on this page
  // share the key and therefore the one *Restarting* rendering.
  const restart = useMutation({
    mutationKey: ["restart", appId],
    mutationFn: () => api.restart(appId),
    onSuccess: (res) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      if (res.status === "restarting") {
        toast.info(res.detail ?? RESTART_IN_FLIGHT, { duration: 8000 });
        return;
      }
      toast.success("Restarted with the saved variables");
    },
    onError: (err: Error) => toast.error(err.message || "Couldn’t restart app"),
  });
  const restarting = restart.isPending;

  const changes =
    count === 1 ? "1 variable change has" : `${count} variable changes have`;

  return (
    <Notice
      size="md"
      title="Saved, but not running yet"
      action={
        app.locked_at ? undefined : (
          <Button
            size="sm"
            onClick={() => restart.mutate()}
            disabled={restarting}
          >
            {restarting ? "Restarting…" : "Restart to apply"}
          </Button>
        )
      }
    >
      {changes} not been applied.{" "}
      {app.locked_at
        ? "They apply once an administrator unlocks the app."
        : "A restart applies them without rebuilding."}
    </Notice>
  );
}

function OverviewTab({
  appId,
  app,
  detail,
  deployments,
  deploymentsLoading,
  deploymentsError,
  refetchDeployments,
  metrics,
  perms,
  batch,
}: {
  appId: string;
  app: App;
  /** A batch app (ADR §107) serves no pages: no preview, no visitors. */
  batch: boolean;
  detail?: AppDetail;
  deployments: Deployment[];
  /** The Health block's four facts are all read from this list, so it has to
   *  know whether the list has arrived. Without them a serving app read "All 0
   *  builds" over "Nothing deployed yet" for a round trip, and for ever if the
   *  read failed. */
  deploymentsLoading: boolean;
  deploymentsError: unknown;
  refetchDeployments: () => void;
  metrics?: AppMetrics;
  perms?: AppPermissions;
}) {
  const serving =
    deployments.find((d) => d.serving) ?? deployments.find((d) => d.current);
  const latest = deployments[0];
  /** Whether this app's backend takes a live reading at all. An isolated
   *  workload carries a metrics object with both of these unanswered, which is
   *  not the same fact as having no metrics object. */
  const processMeasured =
    metrics?.cpu !== undefined || metrics?.memory !== undefined;
  /** Uptime and Restarts always, then whichever readings the backend takes.
   *  The grid is sized from this rather than from `processMeasured`, which
   *  is true when either reading is present and so promised four tiles for
   *  three. */
  const processTiles =
    2 +
    (metrics?.cpu !== undefined ? 1 : 0) +
    (metrics?.memory !== undefined ? 1 : 0);
  /**
   * Whether there is a workload to report on at all.
   *
   * The card printed Uptime and Restarts from whatever `metrics` last held —
   * a stopped app read "up 4 days", a crashed one read the uptime of the
   * process that died — and, where the backend takes no CPU or memory
   * reading, it explained the absence with "This app runs on a backend that
   * reports no CPU or memory reading". For an app that is not running, both
   * are false: the figures are stale and the explanation names the wrong
   * cause. Nothing is running, which is the fact.
   */
  const processRunning = app.status === "running";
  const summary = detail?.summary;
  const deps = detail?.policy?.dependencies;
  const policy = detail?.policy;

  // Is what is serving what was last pushed? The one derived fact nothing
  // stated: an app can be perfectly healthy and three commits behind itself.
  // `!serving` used to be folded in here too, so an app that has never served
  // got a green tick and "Serving the latest build" — see
  // `pages/app/deploy-state.ts`, where the three cases have names and a test.
  const current = servingLatest(latest, serving);
  const neverServed = !serving;

  return (
    <div className="space-y-6 pt-4">
      {/* One summary — what this app is, then (for an editor) whether it is
          working and current. These were two cards, "This app" and "Health",
          each a title bar over one row of facts, which on a wide screen was
          more chrome than content; one panel of label-and-value facts is what
          Linear's and Vercel's project pages use for the same job.

          **The deployment half is editors only, because every fact in it
          is.** All four read the deployment list, which the server refuses to
          a viewer — for them it was four assertions made out of a 403. What a
          viewer needs — what the app is, who owns it, whether it is up — is
          the first half and the pill beside its name. No Address: the header
          already carries it, and a batch app has none to give. */}
      <Section
        title="Summary"
      >
        <dl className="grid gap-x-8 gap-y-4 px-4 py-4 sm:grid-cols-2 lg:grid-cols-4">
          <Fact label="Owner">
            <AppOwner
              ownerId={app.owner_id}
              ownerGroupId={app.owner_group_id}
              ownerLogin={detail?.owner?.login}
            />
          </Fact>
          <Fact label="Who can open it">
            <VisibilityFact app={app} summary={summary} />
          </Fact>
          <Fact label="Created">
            <RelativeTime iso={app.created_at} />
          </Fact>
          {perms?.can_edit && !deploymentsError && (deploymentsLoading ? (
            [0, 1, 2, 3].map((i) => (
              <div key={i} className="space-y-1.5">
                <Skeleton className="h-3 w-16" />
                <Skeleton className="h-4 w-28" />
              </div>
            ))
          ) : (
            <>
              <Fact label="Serving">
                {/* An uploaded release has no commit and a pseudo-branch called
                  `upload`, which rendered as "— on upload": a dash, then a
                  preposition, under a heading about what is running. Say what it
                  is instead. */}
                {!serving ? (
                  <span className="text-muted-foreground">
                    Nothing deployed yet
                  </span>
                ) : serving.commit_sha ? (
                  /* Wraps, and breaks inside a name: a Gallery extension serves
                     `migration-console/v1.3.2` from `extensions/migration-console`,
                     and on one unbreakable line that ran out of a quarter-width
                     column. */
                  <span className="flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-0.5">
                    <GitCommit className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    <span className="font-mono">
                      {serving.commit_sha.slice(0, 7)}
                    </span>
                    {serving.branch && (
                      <span className="min-w-0 break-all text-xs text-muted-foreground">
                        on <span className="font-mono">{serving.branch}</span>
                      </span>
                    )}
                    {app.repo_subdir && (
                      <span className="min-w-0 break-all text-xs text-muted-foreground">
                        in <span className="font-mono">{app.repo_subdir}</span>
                      </span>
                    )}
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1.5">
                    <Upload className="h-3.5 w-3.5 text-muted-foreground" />
                    An uploaded archive
                  </span>
                )}
              </Fact>
              <Fact label="Live since">
                {serving?.finished_at ? (
                  <RelativeTime iso={serving.finished_at} />
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </Fact>
              <Fact label="Last deploy">
                {latest ? (
                  <span className="inline-flex items-center gap-2">
                    {/* Drawn as the Deployments tab draws it: the ordinary
                        outcome is a dot and a word, a pill only for one
                        worth a look. */}
                    {latest.status === "success" ? (
                      <StatusDot status={latest.status} showLabel />
                    ) : (
                      <StatusBadge status={latest.status} />
                    )}
                    {/* The fact is the way in, not a button in the header:
                        a *View deployments* button repeated the tab two
                        lines above it. */}
                    <Link
                      to={`/apps/${appId}/deployments`}
                      className="text-muted-foreground hover:text-foreground hover:underline"
                    >
                      <RelativeTime iso={latest.started_at} />
                    </Link>
                  </span>
                ) : (
                  <span className="text-muted-foreground">Never</span>
                )}
              </Fact>
              {/* The derived fact nothing stated. Absent when there is nothing
                deployed at all — "up to date" about no code is a fact true only
                by omission. */}
              <Fact label="Up to date">
                {!latest || neverServed ? (
                  <span className="text-muted-foreground">—</span>
                ) : current ? (
                  <span className="inline-flex items-center gap-1.5 text-sm">
                    <CheckCircle2 className="h-3.5 w-3.5 text-tone-ok-fg" />
                    Serving the latest deployment
                  </span>
                ) : (
                  <Link
                    to={`/apps/${appId}/deployments`}
                    className="inline-flex items-center gap-1.5 text-sm text-tone-attention-fg hover:underline"
                  >
                    <AlertTriangle className="h-3.5 w-3.5" />A newer deployment exists
                  </Link>
                )}
              </Fact>
            </>
          ))}
        </dl>
        {perms?.can_edit && !!deploymentsError && (
          <div className="border-t border-border/60 px-4 py-2">
            <QueryError
              title="This app’s deployments could not be read"
              error={deploymentsError}
              fallback="Only the app’s owner and editors can see its deployments."
              onRetry={() => void refetchDeployments()}
            />
          </div>
        )}
        {/* One fact per tab that has something to say, under a hairline in
            the same card. A destination with no cheap fact gets none, which
            is why this row is shorter on a viewer's page — and absent when
            nothing has anything to say. It was a titled "Activity" card of
            its own, which on most apps held two lines under a title bar. */}
        <TileGrid
          batch={batch}
          appId={appId}
          summary={summary}
          deps={deps}
          policy={policy}
          perms={perms}
        />
      </Section>

      {/* Second, under the summary, because for a web app it is the most
          telling thing on the page. Closed by default and mounted only when
          opened — the iframe loads the app, which for a slept app is a request
          that wakes it and for any app is a visit its Usage tab counts, so
          somebody looking at their own overview should not be starting
          workloads or inflating their figures. `Disclosure` guarantees it: a
          closed `<details>` keeps its children in the DOM and a hidden iframe
          still fetches. Absent on a batch app, which has no page to show. */}
      {!batch && (
        <TableCard>
          <div className="px-4 py-3">
            <Disclosure label="Preview">
              <PreviewTab app={app} />
            </Disclosure>
          </div>
        </TableCard>
      )}

      {/* What this app is, when the answer is a framework with something to
          say. A notebook is executed at deploy time with the app's own
          environment, and what it prints is published — which is the point of
          the framework and also the sharpest edge in it, so it is stated where
          the person who wrote the notebook will read it.

          Absent for marimo, which is the pair rendering as a difference
          somebody can see: a marimo app executes for each viewer, so there is
          nothing rendered and nothing to schedule. */}
      {app.framework === "notebook" && <NotebookPanel serving={serving} />}

      {/* Absent on the apps that have none, which is most of them — see the
          block's own note. Below the tiles because it is the one card here
          that fetches, and above Process because a mount outlives the process
          it is currently attached to. */}
      <StorageBlock appId={appId} app={app} canEdit={!!perms?.can_edit} />

      {/* Below the release, and labelled for what they are. These describe the
          process this app runs in; they are the only figures on the page that
          cannot be read on their own, and they stay until an app's own
          consumption over a window has a page (ADR §53 D7). */}
      {/* A card, like "Summary" and "Storage" above it. This block
          was a bare heading over four detached tiles, which made four
          consecutive regions of one page read as four different kinds of thing.
          The tiles keep their own shape inside it — they are figures, not
          label-and-value facts — but the region they sit in now matches. */}
      {/* Only while there is a workload. Not running, there are no figures,
          and which kind of not-running it is is the header pill's to say —
          this card restating it was the status said twice. */}
      {/* And only for a reader the record carries figures for: a viewer
          gets none (BB-106), and a panel of dashes would say "unknown"
          where the truth is "not yours to see". */}
      {processRunning && metrics && (
        <Section title="Process">
          <div className="space-y-3 px-4 py-4">
            {/* Absent, never a placeholder. Two of these four used to read
              `--` on every isolated app — half a row of tiles saying
              nothing, in a punctuation mark that is neither the product's
              dash nor a number. A backend that takes no reading gets no
              tile, and the line below says so once. */}
            <div className={cn("grid grid-cols-2 gap-3", statColumns(processTiles))}>
              <StatCard
                label="Uptime"
                value={metrics ? formatUptime(metrics.uptime) : "—"}
                icon={<Clock className="h-4 w-4" />}
              />
              {/* **Scoped to the workload that is running now, and the tile
                  has to say so.** Every deploy, platform restart and
                  supervisor recreate starts a fresh workload with a fresh
                  counter — a pod is replaced, and the agent's workload is described again —
                  so this is restarts *of this workload*, never of the app's
                  life. Read as the second it silently answered "has this app
                  been restarting?" with a number that had been reset an hour
                  ago. What is cumulative is the app's Logs tab and its
                  events; nothing counts across workloads, on purpose. */}
              <StatCard
                label="Restarts"
                value={metrics ? String(metrics.restarts) : "—"}
                description="Of the workload running now"
                icon={<RefreshCw className="h-4 w-4" />}
              />
              {metrics?.cpu !== undefined && (
                <StatCard
                  label="CPU"
                  value={`${metrics.cpu.toFixed(1)}%`}
                  tone={cpuTone(metrics.cpu)}
                  icon={<Cpu className="h-4 w-4" />}
                />
              )}
              {metrics?.memory !== undefined && (
                <StatCard
                  label="Memory"
                  value={formatMB(metrics.memory)}
                  icon={<HardDrive className="h-4 w-4" />}
                />
              )}
            </div>
            {!processMeasured && (
              <p className="text-xs text-muted-foreground">
                CPU and memory aren’t reported for this app.
              </p>
            )}
          </div>
        </Section>
      )}

    </div>
  );
}

/**
 * Who can open this app, in words, with its audience.
 *
 * The audience is only printed for a Restricted app, and that is the third
 * rule rather than an omission: *nobody has access* on a public app is a fact
 * true only by omission — the grant list stops limiting anything the moment
 * visibility widens, so counting it would be describing a control that is not
 * in force.
 */
function VisibilityFact({ app, summary }: { app: App; summary?: AppSummary }) {
  if (app.visibility === "public") {
    return (
      <span className="inline-flex items-center gap-1.5">
        <Globe className="h-3.5 w-3.5 text-muted-foreground" />
        Public
        <span className="text-xs text-muted-foreground">
          anyone with the link, without signing in
        </span>
      </span>
    );
  }
  if (app.visibility === "authenticated") {
    return (
      <span className="inline-flex items-center gap-1.5">
        <Building2 className="h-3.5 w-3.5 text-muted-foreground" />
        Anyone signed in
      </span>
    );
  }
  const people = summary?.grant_users ?? 0;
  const groups = summary?.grant_groups ?? 0;
  const audience = [
    people > 0 ? `${people} ${people === 1 ? "person" : "people"}` : null,
    groups > 0 ? `${groups} ${groups === 1 ? "group" : "groups"}` : null,
  ].filter(Boolean);
  return (
    <span className="inline-flex items-center gap-1.5">
      <LockKeyhole className="h-3.5 w-3.5 text-muted-foreground" />
      Restricted
      {audience.length > 0 && (
        <span className="text-xs text-muted-foreground">
          — {audience.join(", ")}
        </span>
      )}
    </span>
  );
}

/**
 * One tile per destination, each printing one fact and linking to the tab that
 * owns it.
 *
 * Nothing here mutates and nothing here fetches. A destination whose fact would
 * be true only by omission is absent rather than reassuring: no Dependencies
 * tile on an install with the policy off, no Automation tile saying "0
 * schedules" on an app that has never had one, no failure line on an app whose
 * events have never been recorded.
 */
/**
 * The unwindowed usage answer, and why each basis reads differently.
 *
 * The number alone is not the fact: an app quiet for 200 days because nobody
 * has opened it is a different situation from one deployed 200 days ago and
 * never announced, and from one that has never worked at all. The administrator
 * quiet list makes the same distinction, from the same `quiet_basis`, and the
 * two must not describe the same app in different words.
 */
function quietFact(summary: AppSummary): string {
  const days = summary.quiet_days;
  const span =
    days === 0
      ? "today"
      : days === 1
        ? "1 day"
        : `${days.toLocaleString()} days`;
  const ago = days === 0 ? "today" : `${span} ago`;

  switch (summary.quiet_basis) {
    case "visit":
      return days === 0 ? "Last opened today" : `No visitor in ${span}`;
    case "deploy":
      return `Never opened since it was deployed ${ago}`;
    case "retention":
      return `Not opened in the ${span} of visits this install keeps`;
    default:
      return `Never opened, and no successful deploy — created ${ago}`;
  }
}

function TileGrid({
  batch,
  appId,
  summary,
  deps,
  policy,
  perms,
}: {
  batch: boolean;
  appId: string;
  summary?: AppSummary;
  deps?: AppDependencyPolicy;
  policy?: AppPolicy;
  perms?: AppPermissions;
}) {
  const tiles: {
    to: string;
    label: string;
    fact: React.ReactNode;
    tone?: Tone;
  }[] = [];

  if (perms?.can_edit && deps && deps.counts.counted > 0) {
    tiles.push({
      to: `/apps/${appId}/dependencies`,
      label: "Dependencies",
      fact: `${deps.counts.counted} finding${deps.counts.counted === 1 ? "" : "s"}${
        deps.counts.malware > 0 ? `, ${deps.counts.malware} malicious` : ""
      }`,
      tone:
        deps.counts.malware > 0 || deps.deploys_blocked
          ? "danger"
          : "attention",
    });
  }

  // Automation is an edit-gated tab and its list endpoint refuses a viewer, so
  // the tile that links to it is gated with it. Same rule as the two below it.
  if (perms?.can_edit && summary && summary.schedules > 0) {
    tiles.push({
      to: `/apps/${appId}/automation`,
      label: "Automation",
      fact: `${summary.schedules} scheduled job${summary.schedules === 1 ? "" : "s"}${
        summary.schedules_enabled === 0 ? ", all paused" : ""
      }`,
      tone: summary.schedules_enabled === 0 ? "neutral" : undefined,
    });
  }

  if (perms?.can_edit && (policy?.integrations?.length ?? 0) > 0) {
    const attached = policy!.integrations;
    const on = attached.filter((i) => i.enabled);
    tiles.push({
      to: `/apps/${appId}/integrations`,
      label: "Integrations",
      // The one attached kind names where it sends; several are counted,
      // because a tile that lists four destinations is a tile nobody reads.
      fact:
        on.length === 0
          ? `${attached.length} attached, all switched off by an administrator`
          : on.length === 1
            ? `${on[0].title}${on[0].destination ? ` → ${on[0].destination}` : ""}`
            : `${on.length} connected`,
      tone: on.length === 0 ? "neutral" : undefined,
    });
  }

  // Zero requests over thirty days is a real answer — "nobody has opened this"
  // is exactly the fact an owner deciding whether to retire an app wants — but
  // only once there is a window to have been quiet in.
  //
  // Past the window the thirty-day count stops distinguishing anything: it says
  // "no traffic in 30 days" for an app quiet since 2022 and for one deployed
  // last week. So beyond it the tile switches to the unwindowed answer and says
  // which fact the clock runs from, which is the same verdict, from the same
  // query, that the administrator's quiet list shows. Until now the owner —
  // the one person who could actually retire the app — could only learn it from
  // an administrator's email.
  //
  // A batch app has no visitors to count, so it has no Usage fact here.
  if (summary && !batch) {
    tiles.push({
      to: `/apps/${appId}/usage`,
      label: "Usage",
      fact:
        summary.requests_30d > 0
          ? `${summary.requests_30d.toLocaleString()} request${summary.requests_30d === 1 ? "" : "s"}, ${summary.viewers_30d} viewer${summary.viewers_30d === 1 ? "" : "s"} — 30 days`
          : quietFact(summary),
      tone: summary.requests_30d > 0 ? undefined : "neutral",
    });
  }

  if (perms?.can_edit && summary?.last_failure_at) {
    tiles.push({
      to: `/apps/${appId}/logs`,
      label: "Logs",
      fact: (
        <>
          Last failure <RelativeTime iso={summary.last_failure_at} />
        </>
      ),
      tone: "neutral",
    });
  }

  if (tiles.length === 0) return null;

  // The foot of the Summary card: the same label-and-value facts, each a
  // link to the tab that owns it.
  return (
    <dl className="grid gap-x-8 gap-y-4 border-t border-border/60 px-4 py-4 sm:grid-cols-2 lg:grid-cols-4">
        {tiles.map((t) => (
          <Fact key={t.to} label={t.label}>
            <Link
              to={t.to}
              className={cn(
                "hover:underline",
                t.tone ? toneText[t.tone] : "text-foreground",
              )}
            >
              {t.fact}
            </Link>
          </Fact>
        ))}
    </dl>
  );
}

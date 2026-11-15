import { useState } from "react";
import { isHttpUrl } from "@/lib/http-url";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  ChevronDown,
  ChevronRight,
  PackageSearch,
  RefreshCw,
} from "lucide-react";

import {
  api,
  type DependencyFinding,
  type DependencyRelease,
  type DependencyState,
  type DependencyWaiver,
} from "@/lib/api";
import { toneSurface, toneText, type Tone } from "@/lib/tone";
import { cn, fieldErrorId } from "@/lib/utils";
import { formatDateTime } from "@/lib/time";
import { readWaiverDays } from "@/lib/waiver-days";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { Code } from "@/components/code";
import { EmptyState } from "@/components/empty-state";
import { FilterChips } from "@/components/filter-bar";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { SearchInput } from "@/components/search-input";
import { Section, SectionHeading } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { TableCard } from "@/components/table-card";
import { FieldError, FieldHelp } from "@/components/field-help";

/**
 * The app's Dependencies tab.
 *
 * **Named Dependencies, not Security.** This reads names and versions of
 * third-party packages and compares them to published advisories; a tab called
 * Security on a page with no SAST, no secret scanning and no runtime analysis
 * is the oversold copy the whole feature refuses.
 *
 * It leads with findings and not inventory. The package list is the least
 * interesting half on every day when nothing is wrong, and putting it first
 * buries the four rows that matter under four hundred that do not.
 *
 * Three things it keeps straight, each a different question somebody arrives
 * with: what is *serving* is not what was *refused*; a rollback is a
 * remediation and the release rows say which one is clean; and an app with two
 * ecosystems has two answers.
 */

/** The severity ordering every table here reads in. */
const SEVERITY_TONE: Record<string, Tone> = {
  malware: "danger",
  critical: "danger",
  high: "attention",
  moderate: "attention",
  low: "neutral",
  unknown: "neutral",
};

function severityKey(f: DependencyFinding): string {
  if (f.kind === "malware") return "malware";
  return f.severity || "unknown";
}

const SEVERITY_RANK: Record<string, number> = { low: 1, moderate: 2, high: 3, critical: 4 };

/** `Policy.LocksOn` in internal/deps/policy.go, restated: malware locks at any
 *  setting but off, a severity at or above the threshold otherwise, and a
 *  waived finding never. */
function locksOn(f: DependencyFinding, lockOn: DependencyState["lock_on"]): boolean {
  if (lockOn === "off" || f.waived) return false;
  if (f.kind === "malware") return true;
  if (lockOn === "malware") return false;
  return (SEVERITY_RANK[f.severity] ?? 0) >= SEVERITY_RANK[lockOn];
}

function severityLabel(key: string): string {
  if (key === "malware") return "Malicious";
  if (key === "unknown") return "Unscored";
  return key.charAt(0).toUpperCase() + key.slice(1);
}

/** How long a waiver has left, in the words somebody would say it in.
 *  A bare date makes a lapsing waiver a surprise, and lapsing is the one
 *  transition nobody performs. */
function expiresIn(iso: string): string {
  const ms = new Date(iso).getTime() - Date.now();
  if (ms <= 0) return "expired";
  const days = Math.ceil(ms / 86_400_000);
  if (days === 1) return "expires tomorrow";
  if (days < 45) return `expires in ${days} days`;
  return `expires ${formatDateTime(iso)}`;
}

export function DependenciesTab({
  appId,
  appName,
}: {
  appId: string;
  /** The app's name, which is how every sentence on the app page names it. */
  appName: string;
}) {
  const queryClient = useQueryClient();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["app-dependencies", appId],
    queryFn: () => api.getAppDependencies(appId),
  });

  const recheck = useMutation({
    mutationFn: () => api.recheckAppDependencies(appId),
    onSuccess: (next) => {
      queryClient.setQueryData(["app-dependencies", appId], next);
      // The app request carries the counts and the notices, so a re-check that
      // moved them has to move the page header too.
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success("Re-checked");
    },
    onError: (e: Error) => toast.error(e.message || "Couldn’t re-check"),
  });

  if (error) {
    return (
      <div className="pt-4">
        <QueryError
          title="This app’s dependencies could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see its dependencies."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }
  if (isLoading || !data) {
    return (
      <div className="space-y-4 pt-4">
        <Skeleton className="h-20 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (!data.available) {
    return (
      <div className="pt-4">
        <Notice
          tone="neutral"
          size="md"
          title="Dependency policy is off on this install"
        >
          {data.reason ||
            "Nothing checks this app’s packages. An administrator can turn it on in Admin → Dependencies."}
        </Notice>
      </div>
    );
  }

  // The third state, and it says so and stops. "0 findings" would claim a scan
  // happened; "not scanned" would send somebody to turn a setting on for an app
  // that would still have nothing to scan. There is no summary strip, no
  // baseline and no Check again, because none of them would ever move.
  if (data.scan_reason === "no_dependencies") {
    return (
      <div className="pt-4">
        <Notice
          tone="neutral"
          size="md"
          title="This app declares no dependencies"
        >
          {appName} installs no packages, so there’s nothing to check. A
          deploy that adds one is checked like any other.
        </Notice>
      </div>
    );
  }

  // The fourth state, and it is "no_dependencies" inverted: this app has
  // dependencies and its release named none — built before the installed
  // library became R's record, or never served. Saying "0 findings" would be a
  // status that is a lie, and "not scanned" would send somebody to a setting
  // that changes nothing — so the copy says what to commit instead.
  if (data.scan_reason === "not_enumerable") {
    return (
      <div className="pt-4">
        <Notice
          tone="attention"
          size="md"
          title="This app’s packages cannot be listed"
        >
          <p>
            {appName} is an R app whose release recorded no packages, so nothing
            on it can be checked. A redeploy lists what its build installs.
          </p>
          <p className="mt-2">
            To pin its own packages, run <Code>renv::init()</Code> and{" "}
            <Code>renv::snapshot()</Code> locally, then commit{" "}
            <Code>renv.lock</Code>.
          </p>
        </Notice>
      </div>
    );
  }

  return (
    <div className="space-y-6 pt-4">
      <SummaryStrip
        state={data}
        onRecheck={() => recheck.mutate()}
        rechecking={recheck.isPending}
      />

      {data.refused && (
        <RefusedBlock
          refused={data.refused}
          servingRelease={data.serving?.deployment_id}
        />
      )}

      {/* **A finding is shown wherever there is one; `scanned` only says whether
          silence is a verdict.** An R release matched against the CRAN corpus can
          carry a real advisory while still not being scanned, so branching on
          `scanned` alone would hide the one thing this tab exists to show. Only a
          release with nothing found *and* nothing trustworthy to find falls
          through to NotScanned. */}
      {data.serving && (data.scanned || data.serving.findings.length > 0) ? (
        <Findings
          release={data.serving}
          state={data}
          appId={appId}
          onChanged={() => {
            queryClient.invalidateQueries({
              queryKey: ["app-dependencies", appId],
            });
            queryClient.invalidateQueries({ queryKey: ["app", appId] });
          }}
        />
      ) : (
        <NotScanned state={data} appName={appName} />
      )}

      {data.waivers.length > 0 && (
        <Waivers
          waivers={data.waivers}
          canRevoke={data.can_waive}
          appId={appId}
          onChanged={() => {
            queryClient.invalidateQueries({
              queryKey: ["app-dependencies", appId],
            });
            queryClient.invalidateQueries({ queryKey: ["app", appId] });
          }}
        />
      )}

      {/* Recorded and displayed, which for `not_covered` is the whole of what
          this tab has to offer: the findings block above says nothing was
          checked, and this is the list it is talking about. */}
      {(data.scanned || data.scan_reason === "not_covered" || data.scan_reason === "no_corpus") &&
        data.serving && (
        <Inventory release={data.serving} />
      )}
    </div>
  );
}

/**
 * Mode, the release, when it was checked, and the corpus's own age.
 *
 * The age leads rather than being a footnote: **every number below it is scoped
 * to the data it was checked against**, and a figure without that scope is a
 * claim the platform cannot support.
 */
function SummaryStrip({
  state,
  onRecheck,
  rechecking,
}: {
  state: DependencyState;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  // Two policies, each named: what happens to a deploy, and what happens to
  // an app already serving. Unlabelled and adjacent, "Warning only" and a
  // sentence about locking read as opposites.
  const modeLabel =
    state.mode === "block"
      ? state.enforcing
        ? "Deploys: refused at or above the threshold"
        : "Deploys: refused at or above the threshold (nothing to check against yet)"
      : "Deploys: warning only";
  const lockingFindings = (state.serving?.findings ?? []).filter((f) =>
    locksOn(f, state.lock_on),
  ).length;
  const lockLabel =
    state.lock_on === "off"
      ? null
      : state.lock_on === "malware"
        ? "Serving apps: locked at a malicious package"
        : `Serving apps: locked at ${state.lock_on} or worse`;

  return (
    <div className="space-y-3">
      {/* No heading: the tab is already called Dependencies, and repeating it
          here spends the first line of the page saying nothing. What the strip
          owes the reader is the *scope* of every number below it — the mode,
          the data's age and the release — and the button that re-runs them. */}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <p className="text-xs text-muted-foreground">
          <>
            {modeLabel}
            {lockLabel && ` · ${lockLabel}`}
            {state.feed_as_of ? (
              <>
                {" · advisory data from "}
                <RelativeTime iso={state.feed_as_of} />
              </>
            ) : (
              " · no advisory data has been loaded yet"
            )}
            {state.serving?.deployment_id && (
              <>
                {" · deployment "}
                <span className="font-mono">
                  {state.serving.deployment_id.slice(0, 8)}
                </span>
              </>
            )}
            {/* **Never checked and checked-and-clean are different answers**,
                so the absence says so rather than resolving to a date nobody
                took. */}
            {state.last_scan ? (
              <>
                {" · last checked "}
                <RelativeTime iso={state.last_scan.occurred_at} />
              </>
            ) : (
              " · not checked yet"
            )}
          </>
        </p>
        <Button
          variant="outline"
          size="sm"
          className="shrink-0"
          onClick={onRecheck}
          disabled={rechecking}
        >
          <RefreshCw className={cn("size-3.5", rechecking && "animate-spin")} />
          {rechecking ? "Checking…" : "Check again"}
        </Button>
      </div>

      {/* Said plainly rather than left for somebody to infer from an empty
          table: with no corpus nothing is checked, and `block` blocks nothing. */}
      {state.available && !state.has_data && (
        <Notice
          tone="attention"
          title={
            state.mode === "block"
              ? "Nothing is being refused yet"
              : "Nothing has been checked yet"
          }
        >
          No advisory data has loaded yet, so an empty list here is not a clean
          result. An administrator can fetch it with Check now on Admin → Dependencies.
        </Notice>
      )}

      {state.feed_stale && (
        <Notice
          tone="attention"
          title="The advisory data is older than this install allows"
        >
          Stale data never refuses a deploy, but these findings may be behind.
        </Notice>
      )}

      {/* The app is being held down by the policy itself, and the way out is
          not the unlock button an administrator is looking at: the next pass
          locks it again while the finding still matches. Said here because this
          is the tab the finding is on. */}
      {state.locked_by_policy && (
        <Notice
          tone="danger"
          title="This app was locked by the dependency policy"
        >
          It’s stopped and closed to visitors because its deployment installs
          a package this install won’t run. Deploy without it, or ask an
          administrator for a waiver. Unlocking alone doesn’t last: the next
          check locks it again.
        </Notice>
      )}

      {/* What *will* happen, on the tab where the finding is, so a lock is
          never the first anybody hears of the threshold — and only when this
          app holds a finding the lock acts on. The policy itself is the strip's
          line above; drawn in `attention` on every app it was background in
          the tone that means *go and look*. */}
      {!state.locked_by_policy && state.watch_enabled && lockingFindings > 0 && (
        <Notice tone="attention">
          The next check will lock this app: {lockingFindings}{" "}
          {lockingFindings === 1 ? "finding is" : "findings are"} at or above the
          lock threshold. Upgrade the {lockingFindings === 1 ? "package" : "packages"}
          {state.waivers_available ? " or ask an administrator for a waiver" : ""}.
        </Notice>
      )}

      {!state.watch_enabled && (
        <Notice tone="neutral" icon={null}>
          Background re-checks are off, so these figures won’t change on their
          own. <strong className="font-medium">Check again</strong> re-runs them
          now.
        </Notice>
      )}
    </div>
  );
}

/**
 * The refused deploy, dated and kept apart from what is serving.
 *
 * Without this the developer reads today's refusal off yesterday's inventory
 * and fixes the wrong package.
 */
function RefusedBlock({
  refused,
  servingRelease,
}: {
  refused: NonNullable<DependencyState["refused"]>;
  servingRelease?: string;
}) {
  return (
    <Notice tone="danger" size="md" title="Your last deploy was refused">
      <div className="space-y-2">
        <p>
          {refused.at ? (
            <>
              <RelativeTime iso={refused.at} />, on{" "}
            </>
          ) : (
            "On "
          )}
          <span className="font-mono">
            {(refused.commit_sha || refused.deployment_id).slice(0, 8)}
          </span>
          .{" "}
          {servingRelease
            ? "The deployment below is still serving and was not stopped."
            : "Nothing is serving for this app."}
        </p>
        <pre className="overflow-x-auto whitespace-pre-wrap rounded-md bg-card/60 p-3 font-mono text-2xs leading-relaxed">
          {refused.reason}
        </pre>
      </div>
    </Notice>
  );
}

/**
 * Findings, the lead table.
 *
 * Sorted malware first, then blocking, then by severity — the server's own
 * order, so a reader who learned it on the estate view does not relearn it
 * here. Grouped by ecosystem when there is more than one, because the fix, the
 * lockfile and often the person are different for each.
 */
function Findings({
  release,
  state,
  appId,
  onChanged,
}: {
  release: DependencyRelease;
  state: DependencyState;
  appId: string;
  onChanged: () => void;
}) {
  const [filter, setFilter] = useState("all");
  const [waiving, setWaiving] = useState<DependencyFinding | null>(null);

  const counts = release.counts;
  const chips = [
    { label: "All", value: "all", count: counts.total },
    ...["malware", "critical", "high", "moderate", "low", "unknown"]
      .filter((k) => (counts.by_severity?.[k] ?? 0) > 0)
      .map((k) => ({
        label: severityLabel(k),
        value: k,
        count: counts.by_severity[k],
      })),
  ];

  const shown =
    filter === "all"
      ? release.findings
      : release.findings.filter((f) => severityKey(f) === filter);

  const ecosystems = Array.from(
    new Set(release.findings.map((f) => f.ecosystem)),
  );

  if (release.findings.length === 0) {
    return (
      <Section title="Findings">
        <EmptyState
          icon={<PackageSearch className="size-6" />}
          title="No known advisories"
          description={
            `Among the ${release.inventory.length} packages in this deployment, checked against ` +
            `the advisory data this install holds${
              state.feed_as_of
                ? ` as of ${formatDateTime(state.feed_as_of)}`
                : ""
            }.`
          }
        />
      </Section>
    );
  }

  return (
    <div className="space-y-3">
      <SectionHeading
        title="Findings"
        description={
          <>
            {counts.counted} at or above this install&rsquo;s threshold
            {counts.waived > 0 && `, ${counts.waived} waived`}
            {counts.unknown > 0 && `, ${counts.unknown} unscored`}.
          </>
        }
      />

      {/* Findings on a release that is not scanned. Everything below is real
          and was matched the same way every other finding is — what the release
          cannot support is the opposite inference, and this says so before the
          reader draws it from a short table. */}
      {!state.scanned && (
        <Notice tone="attention" size="sm" title="This list is not the whole picture">
          {release.inventory.length} package
          {release.inventory.length === 1 ? " is" : "s are"} R, which advisory
          data barely covers. A short list here isn’t proof of few problems.
        </Notice>
      )}
      {chips.length > 2 && (
        <FilterChips
          label="Narrow by severity"
          options={chips}
          value={filter}
          onChange={setFilter}
        />
      )}

      {ecosystems.map((ecosystem) => {
        const rows = shown.filter((f) => f.ecosystem === ecosystem);
        if (rows.length === 0) return null;
        return (
          <TableCard key={ecosystem}>
            {ecosystems.length > 1 && (
              <div className="border-b border-border/60 px-4 py-2 text-xs font-medium text-muted-foreground">
                {ecosystem}
              </div>
            )}
            <div className="divide-y divide-border/60">
              {rows.map((f) => (
                <FindingRow
                  key={`${f.advisory_id}-${f.package}-${f.version}`}
                  finding={f}
                  enforcing={state.enforcing}
                  canWaive={state.can_waive && state.waivers_available}
                  onWaive={() => setWaiving(f)}
                />
              ))}
            </div>
          </TableCard>
        );
      })}

      {waiving && (
        <WaiverDialog
          key={waiving.advisory_id}
          appId={appId}
          finding={waiving}
          maxDays={state.waiver_max_days}
          onClose={() => setWaiving(null)}
          onGranted={() => {
            setWaiving(null);
            onChanged();
          }}
        />
      )}
    </div>
  );
}

/** One finding, expandable to the chain and the advisory's own summary. */
function FindingRow({
  finding,
  enforcing,
  canWaive,
  onWaive,
}: {
  finding: DependencyFinding;
  enforcing: boolean;
  canWaive: boolean;
  onWaive: () => void;
}) {
  const [open, setOpen] = useState(false);
  const key = severityKey(finding);
  const tone = SEVERITY_TONE[key] ?? "neutral";

  // Three states, and the middle one is why `would_block` is not rendered as
  // "blocked": in warn mode only malware is refused (deps.Policy.Refuses).
  const refuses = enforcing || key === "malware";
  const state = finding.waived
    ? {
        label: `Waived · ${expiresIn(finding.waiver_expires_at ?? "")}`,
        tone: "neutral" as Tone,
      }
    : finding.would_block
      ? refuses
        ? { label: "Refuses deploy", tone: "danger" as Tone }
        : // Short on purpose. The strip above already says "Warning only", so
          // the qualifier is in context rather than in a label that wraps to
          // three lines on a phone; the expanded row spells it out in full.
          { label: "Would refuse deploy", tone: "attention" as Tone }
      : { label: "Recorded", tone: "neutral" as Tone };

  return (
    <div className="px-4 py-3">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <button
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          className="flex min-w-0 flex-1 items-start gap-2 text-left focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          {open ? (
            <ChevronDown className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          ) : (
            <ChevronRight className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          )}
          <span className="min-w-0">
            <span className="block break-words font-mono text-sm text-foreground">
              {finding.package} {finding.version}
            </span>
            <span className="mt-0.5 block text-xs text-muted-foreground">
              {finding.advisory_id}
              {finding.fixed_in
                ? ` · fixed in ${finding.fixed_in}`
                : " · no fixed version published"}
              {finding.dev && " · build-time dependency"}
            </span>
          </span>
        </button>

        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline" className={toneSurface[tone]}>
            {severityLabel(key)}
          </Badge>
          <span className={cn("text-xs", toneText[state.tone])}>
            {state.label}
          </span>
          {canWaive && !finding.waived && (
            <Button
              variant="outline"
              size="sm"
              className="h-7 text-xs"
              onClick={onWaive}
            >
              Waive…
            </Button>
          )}
        </div>
      </div>

      {open && (
        <div className="mt-3 space-y-2 pl-6 text-xs text-muted-foreground">
          {finding.summary && (
            <p className="text-foreground">{finding.summary}</p>
          )}
          {finding.path && !finding.direct && (
            <p>
              Reached via <span className="font-mono">{finding.path}</span>.
              Update that dependency to remove this.
            </p>
          )}
          {finding.direct && (
            <p>This app&rsquo;s own manifest names this package.</p>
          )}
          {finding.would_block && !refuses && !finding.waived && (
            <p>
              This install only warns, so no deploy is refused. Under
              Refuse, this would refuse the next deploy.
            </p>
          )}
          {finding.waived && (
            <p>
              Waived by {finding.waived_by || "an administrator"}
              {finding.waiver_expires_at &&
                ` · ${expiresIn(finding.waiver_expires_at)}`}
              {finding.waived_reason && ` — “${finding.waived_reason}”`}. When
              it lapses this counts again.
            </p>
          )}
          {isHttpUrl(finding.url) && (
            <p>
              <a
                href={finding.url}
                target="_blank"
                rel="noreferrer noopener"
                className="text-primary hover:underline"
              >
                Read the advisory
              </a>
            </p>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * The two unscanned states, which are different facts and must not render as
 * "no findings".
 *
 * One is fixed by a redeploy and one is not, so they get different copy and
 * different actions — an owner pressing Redeploy twice against a setting is
 * the product wasting their afternoon.
 */
function NotScanned({
  state,
  appName,
}: {
  state: DependencyState;
  appName: string;
}) {
  const bySetting = state.scan_reason === "not_scanned_by_policy";
  // The build could not list what it installs (a Python with no wheel for a
  // package, say), so a redeploy of the same tree records nothing new either.
  const unresolved = state.scan_reason === "unresolved";
  // The third branch, and the only one where the packages *are* below: the
  // release was read completely and matched against a corpus too thin for its
  // silence to mean anything, so the heading must not say "have not been
  // recorded" over a list of them — and must not say "clear" either.
  if (state.scan_reason === "not_covered") {
    return (
      <Section title="Findings">
        <EmptyState
          icon={<PackageSearch className="size-6" />}
          title="Nothing found, but coverage is thin"
          description={
            `Every package ${appName} installs is listed below. They’re R ` +
            "packages, which advisory data barely covers, so no findings means little."
          }
        />
      </Section>
    );
  }
  // Recorded, in an ecosystem whose advisory data has never loaded on this
  // install: checked against nothing, and an administrator's to fix.
  if (state.scan_reason === "no_corpus") {
    return (
      <Section title="Findings">
        <EmptyState
          icon={<PackageSearch className="size-6" />}
          title="Not checked: no advisory data for these packages"
          description={
            `Every package ${appName} installs is listed below. This install has no advisory ` +
            "data for their ecosystem yet, so none were checked. An administrator can sync or import it."
          }
        />
      </Section>
    );
  }
  return (
    <Section title="Findings">
      <EmptyState
        icon={<PackageSearch className="size-6" />}
        title="This deployment’s packages haven’t been recorded"
        description={
          bySetting
            ? "This install doesn’t check dependencies at deploy. An administrator can " +
              "change that on Admin → Dependencies; redeploying won’t help."
            : unresolved
              ? `The packages ${appName} installs couldn’t be listed as the build installs ` +
                "them, so none were checked. The build log says why; redeploying the same tree won’t help."
              : `Nothing has recorded what ${appName} installs yet. Its next deploy will.`
        }
      />
    </Section>
  );
}

/**
 * Live waivers, visible to the owner although only an administrator grants
 * one: "a waiver silences the refusal, never the record" is not worth much if
 * the record is admin-only.
 */
function Waivers({
  waivers,
  canRevoke,
  appId,
  onChanged,
}: {
  waivers: DependencyWaiver[];
  canRevoke: boolean;
  appId: string;
  onChanged: () => void;
}) {
  // A plain confirm: revoking makes the next deploy refused again, and where
  // the install locks on a finding, can lock the serving app on the next pass.
  const [toRevoke, setToRevoke] = useState<DependencyWaiver | null>(null);
  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeDependencyWaiver(appId, id),
    onSuccess: () => {
      toast.success(`Waiver on ${toRevoke?.advisory_id ?? "this finding"} revoked`);
      setToRevoke(null);
      onChanged();
    },
    onError: (e: Error) =>
      toast.error(e.message || `Couldn’t revoke the waiver on ${toRevoke?.advisory_id ?? "this finding"}`),
  });

  return (
    <Section
      title="Waivers"
      description="Accepted risks on this app. When one expires, the next deploy is refused again."
    >
      <div className="divide-y divide-border/60">
        {waivers.map((w) => (
          <div
            key={w.id}
            className="flex flex-wrap items-start justify-between gap-3 px-4 py-3"
          >
            <div className="min-w-0">
              <p className="font-mono text-sm text-foreground">
                {w.advisory_id}
                {w.package && (
                  <span className="text-muted-foreground"> · {w.package}</span>
                )}
              </p>
              <p className="mt-0.5 text-xs text-muted-foreground">
                {w.created_by_name || "an administrator"} ·{" "}
                {expiresIn(w.expires_at)}
              </p>
              <p className="mt-1 text-xs text-foreground">{w.reason}</p>
            </div>
            {canRevoke && (
              <Button
                variant="outline"
                size="sm"
                className="h-7 shrink-0 text-xs"
                onClick={() => setToRevoke(w)}
              >
                Revoke…
              </Button>
            )}
          </div>
        ))}
      </div>
      <Dialog open={toRevoke !== null} onOpenChange={(o) => !o && !revoke.isPending && setToRevoke(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke the waiver on {toRevoke?.advisory_id}?</DialogTitle>
            <DialogDescription>
              The next deploy will be refused while this finding matches. The
              finding itself stays on the record.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={revoke.isPending}
              onClick={() => setToRevoke(null)}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={revoke.isPending}
              onClick={() => toRevoke && revoke.mutate(toRevoke.id)}
            >
              {revoke.isPending ? "Revoking…" : "Revoke waiver"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Section>
  );
}

/**
 * The inventory, collapsed. It is the least interesting half on every day when
 * nothing is wrong, and leading with it would bury the rows that matter.
 */
function Inventory({ release }: { release: DependencyRelease }) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");

  const rows = query
    ? release.inventory.filter((d) =>
        `${d.package} ${d.version}`.toLowerCase().includes(query.toLowerCase()),
      )
    : release.inventory;

  return (
    <TableCard>
      <button
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="flex w-full items-center justify-between gap-3 px-4 py-3 text-left focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-ring"
      >
        <span>
          <span className="block text-sm font-semibold text-foreground">
            Packages
          </span>
          <span className="mt-0.5 block text-xs text-muted-foreground">
            {release.inventory.length} packages, {release.direct} named by this
            app
            {release.ecosystems && release.ecosystems.length > 0 && (
              <> · {release.ecosystems.join(", ")}</>
            )}
          </span>
        </span>
        {open ? (
          <ChevronDown className="size-4 shrink-0 text-muted-foreground" />
        ) : (
          <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
        )}
      </button>

      {open && (
        <div className="border-t border-border/60">
          <div className="px-4 py-3">
            <SearchInput
              value={query}
              onChange={setQuery}
              placeholder="Filter packages…"
            />
          </div>
          <div className="@container max-h-96 overflow-y-auto">
            <table className="w-full text-sm">
              <thead className="sticky top-0 bg-card">
                <tr className="border-b border-border/60 text-left text-xs text-muted-foreground">
                  <th className="px-4 py-2 font-medium">Package</th>
                  <th className="px-4 py-2 font-medium">Version</th>
                  <th className="hidden px-4 py-2 font-medium @md:table-cell">
                    Ecosystem
                  </th>
                  <th className="hidden px-4 py-2 font-medium @3xl:table-cell">
                    Reached via
                  </th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border/60">
                {rows.map((d) => (
                  <tr key={`${d.ecosystem}-${d.package}-${d.version}`}>
                    <td className="px-4 py-2 font-mono text-xs">
                      {d.package}
                      {d.direct && (
                        <Badge variant="outline" className="ml-2 text-2xs">
                          direct
                        </Badge>
                      )}
                      {d.dev && (
                        <Badge variant="outline" className="ml-2 text-2xs">
                          dev
                        </Badge>
                      )}
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">{d.version}</td>
                    <td className="hidden px-4 py-2 text-xs text-muted-foreground @md:table-cell">
                      {d.ecosystem}
                    </td>
                    <td className="hidden max-w-xs truncate px-4 py-2 font-mono text-2xs text-muted-foreground @3xl:table-cell">
                      {d.path}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {rows.length === 0 && (
              <p className="px-4 py-6 text-center text-sm text-muted-foreground">
                No package matches “{query}”.
              </p>
            )}
          </div>
        </div>
      )}
    </TableCard>
  );
}

/**
 * Granting a waiver. A reason is required and an expiry is capped — there is
 * no permanent waiver, because a permanent one is how this control dies
 * quietly with a list nobody has read since the quarter it was filed.
 */
export function WaiverDialog({
  appId,
  appName,
  finding,
  maxDays,
  onClose,
  onGranted,
}: {
  appId: string;
  /** Named where the dialog is opened away from the app — Admin →
   *  Dependencies' *Needs a decision* list. */
  appName?: string;
  finding: Pick<DependencyFinding, "advisory_id" | "package" | "version" | "summary">;
  maxDays: number;
  onClose: () => void;
  onGranted: () => void;
}) {
  const [reason, setReason] = useState("");
  // A string draft, not a number: `Number("")` is 0, the server reads a
  // non-positive `days` as "use the install's cap", and clearing this box
  // granted the longest waiver the install allows under a toast that said "for
  // 0 days". `readWaiverDays` is the bound and the test lives with it.
  const [daysDraft, setDaysDraft] = useState("30");
  const reading = readWaiverDays(daysDraft, maxDays);

  const grant = useMutation({
    mutationFn: (days: number) =>
      api.grantDependencyWaiver(appId, {
        advisory_id: finding.advisory_id,
        package: finding.package,
        reason,
        days,
      }),
    onSuccess: (_data, days) => {
      toast.success(
        `${finding.advisory_id} waived on ${appName ? `“${appName}”` : "this app"} for ${days} ${days === 1 ? "day" : "days"}`,
      );
      onGranted();
    },
    onError: (e: Error) =>
      toast.error(e.message || `Couldn’t waive ${finding.advisory_id}`),
  });

  return (
    <Dialog open onOpenChange={(v) => !v && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            Waive {finding.advisory_id} on {appName ?? "this app"}?
          </DialogTitle>
          <DialogDescription>
            The finding stays visible, marked as waived. When the waiver
            lapses, it counts again.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <TableCard flat className="p-3">
            <p className="font-mono text-sm">
              {finding.package} {finding.version}
            </p>
            {finding.summary && (
              <p className="mt-1 text-xs text-muted-foreground">
                {finding.summary}
              </p>
            )}
          </TableCard>

          <div className="space-y-2">
            <Label htmlFor="waiver-reason">Why this risk is accepted</Label>
            <Textarea
              id="waiver-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="No fix for our webpack major yet; reviewed with the platform team."
              rows={3}
            />
            <FieldHelp>
              Required. Future reviewers will read this.
            </FieldHelp>
          </div>

          <div className="space-y-2">
            <Label htmlFor="waiver-days">Waive for (days)</Label>
            <Input
              id="waiver-days"
              type="number"
              min={1}
              max={maxDays > 0 ? maxDays : undefined}
              value={daysDraft}
              onChange={(e) => setDaysDraft(e.target.value)}
              aria-invalid={!!reading.error}
              aria-describedby={reading.error ? fieldErrorId("waiver-days") : undefined}
              className="w-32"
            />
            {/* The refusal replaces the bound rather than sitting under it —
                the sentence a reader needs while the field is wrong is the one
                saying what is wrong with it. Two components rather than one
                paragraph that changes colour, because the colours mean
                different things: `--destructive` is what the person is doing,
                a muted line is what the install is. */}
            <FieldError field="waiver-days">{reading.error}</FieldError>
            {!reading.error && (
              <FieldHelp>
                {maxDays > 0
                  ? `Up to ${maxDays} days on this install. There is no permanent waiver.`
                  : "There is no permanent waiver."}
              </FieldHelp>
            )}
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            onClick={() => reading.days !== undefined && grant.mutate(reading.days)}
            disabled={!reason.trim() || reading.days === undefined || grant.isPending}
          >
            {grant.isPending ? "Waiving…" : "Waive"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

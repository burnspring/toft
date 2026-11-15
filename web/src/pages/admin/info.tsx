import { Clock, Box, Users } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import {
  api,
  type ActiveUserCounts,
  type PlatformSettings,
  type SharedContainment,
  type SystemHealth,
} from "@/lib/api";
import { StatCard } from "@/components/stat-card";
import { QueryError } from "@/components/query-error";
import { Badge } from "@/components/ui/badge";
import { Section, SectionHeading } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { AdminEstatePage } from "@/pages/admin/estate";
import { PageHeader } from "@/components/page-header";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { toneSurface, type Tone } from "@/lib/tone";
import { formatUptime } from "@/lib/time";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/**
 * How many distinct people opened an app in each window. "Active" is app
 * traffic, not sign-ins: someone who signed in and only browsed Toft
 * itself is not counted, and traffic is bucketed by UTC day, so "Today" is a
 * day rather than a rolling 24 hours.
 */
function ActiveUsersRow() {
  const { data, isLoading, error, refetch } = useQuery<ActiveUserCounts>({
    queryKey: ["active-users"],
    queryFn: api.getActiveUsers,
    refetchInterval: 60_000,
    retry: false,
  });

  const windows: { label: string; value: keyof ActiveUserCounts; description: string }[] = [
    { label: "Active today", value: "today", description: "So far today (UTC)" },
    { label: "Active this week", value: "week", description: "Last 7 days" },
    { label: "Active this month", value: "month", description: "Last 30 days" },
    { label: "Active this year", value: "year", description: "Last 365 days" },
  ];

  return (
    <div className="space-y-2">
      <SectionHeading title="People using apps" />
      {/* An em dash where a count belongs is "still loading" to a reader, and
          it was also what a failed read looked like — four tiles saying nothing
          for ever, with no way to tell which. */}
      {error ? (
        <QueryError
          title="The headcount could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          {windows.map((w) =>
            isLoading || !data ? (
              <Skeleton key={w.value} className="h-24" />
            ) : (
              <StatCard
                key={w.value}
                label={w.label}
                value={data[w.value].toLocaleString()}
                icon={<Users className="h-4 w-4" />}
                description={w.description}
              />
            ),
          )}
        </div>
      )}
      <p className="text-xs text-muted-foreground">
        Signed-in people who opened an app. Anonymous visitors and API keys aren’t counted.
      </p>
    </div>
  );
}

const TOPOLOGY_LABEL: Record<PlatformSettings["topology"], string> = {
  vm: "Single machine",
  ecs: "Amazon ECS",
  kubernetes: "Kubernetes",
};

/**
 * Shared mode's filesystem posture, and the one fact in this card that grades
 * itself — only `per-app` is a supported production posture, and the other two
 * are stated as what they cost rather than as neutral configuration.
 *
 * `danger` and not `attention` for the platform user: it is not "look at this",
 * it is every deployed app holding the platform's database credentials and
 * every other app's secrets. `attention` is right for the shared account,
 * which does separate apps from the platform and leaves only app↔app open.
 */
const APP_ACCOUNTS: Record<
  PlatformSettings["app_accounts"],
  { label: string; tone: Tone; detail: string }
> = {
  "per-app": {
    label: "One account per app",
    tone: "ok",
    detail: "Apps can’t read the platform’s files or each other’s.",
  },
  shared: {
    label: "One shared account",
    tone: "attention",
    detail: "Apps are separated from the platform, but not from each other.",
  },
  platform: {
    label: "The platform user",
    tone: "danger",
    detail:
      "Every app can read the platform’s database credentials and every other app’s secrets. Fix: set APP_USER_PER_APP=1 and run Toft as root.",
  },
};

/**
 * How this install was deployed — read-only, and set in deployment
 * configuration rather than here.
 *
 * These four rows opened Admin → Settings until ADR §50 D2: four facts nobody
 * can change, at the top of a page of editable values, which is the clearest
 * instance of the wall that page had become. They belong to a split the
 * `toft-ui` skill records — Info is identity and facts — and they
 * are what decides which switches Settings is allowed to offer at all.
 */
type ServerInfo = NonNullable<SystemHealth["server"]>;

/**
 * What versions of each language this install has, and what its isolated runner
 * images are declared to carry.
 *
 * A *set* rather than a row, because an operator building their final image on
 * top of the published base may install several versions of one language and an
 * app pins a range against them. Both columns, because a pin is judged against
 * the execution mode the app runs in and an administrator has to be able to see
 * the two agree — nothing else makes a divergence between the platform image
 * and a runner image visible.
 *
 * Go is the odd one and shows it: no runner column, because `go build` leaves
 * the toolchain on the build host and ships a binary, and a GOTOOLCHAIN line
 * instead, because an administrator debugging a Go build that cannot reach the
 * module proxy needs the installed toolchain and the posture in one place.
 */
function LanguageVersionsCard({ server }: { server: ServerInfo }) {
  const languages = server.toolchains ?? [];

  return (
    <Section id="versions" title="Language versions" description="Apps without a version pin get the default.">
      <div className="p-4">

        <div className="space-y-4">
          {languages.map((lang) => (
            <div key={lang.language} className="border-t border-border/60 pt-4 first:border-t-0 first:pt-0">
              <div className="mb-2 text-sm font-medium text-foreground">{lang.label}</div>

              {lang.installed.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  Not installed. Apps in this language can’t be built here.
                </p>
              ) : (
                <ul className="flex flex-wrap items-baseline gap-x-5 gap-y-1.5">
                  {lang.installed.map((v) => (
                    <li key={v.version} className="flex flex-wrap items-baseline gap-x-2 gap-y-1 text-sm">
                      <span className="font-mono">{v.version}</span>
                      {v.default && (
                        <Badge variant="secondary" className="text-2xs">
                          Default
                        </Badge>
                      )}
                    </li>
                  ))}
                </ul>
              )}

              {lang.runner && (
                <p className="mt-2 text-xs text-muted-foreground">
                  Isolated runner image carries{" "}
                  <span className="font-mono">{lang.runner}</span>.
                </p>
              )}
              {lang.language === "go" && (
                <p className="mt-2 text-xs text-muted-foreground">
                  {lang.posture ? (
                    <>
                      <span className="font-mono">GOTOOLCHAIN={lang.posture}</span>
                      {lang.posture === "local"
                        ? ": a newer toolchain in go.mod is refused."
                        : ": a newer toolchain in go.mod is downloaded."}
                    </>
                  ) : (
                    <>
                      A newer toolchain in go.mod is downloaded. Air-gapped installs should set{" "}
                      <span className="font-mono">GOTOOLCHAIN=local</span>.
                    </>
                  )}
                </p>
              )}
            </div>
          ))}
        </div>

      </div>
    </Section>
  );
}

const CONTAINMENT_LABEL: Record<string, string> = {
  process: "Processes",
  cgroup: "cgroups",
  namespace: "Namespaces",
};

/** What the agent's tier means for shared apps, and why it is not the next. */
function containmentSentence(c: SharedContainment): string {
  const why = c.reason ? ` ${c.tier === "process" ? "cgroups" : "Namespaces"} are unavailable: ${c.reason}.` : "";
  switch (c.tier) {
    case "namespace":
      return `Each shared app has its own network, mount and PID namespace${c.bridge ? ` on ${c.bridge}` : ""}, its own cgroup and at most 4,096 processes.`;
    case "cgroup":
      return `Shared apps are held by cgroups: the kernel enforces memory, CPU is shared by weight, at most 4,096 processes each. They still share loopback.${why}`;
    default:
      return `Shared apps are supervised as processes; memory limits are sampled and they share loopback.${why}`;
  }
}

function InstallCard() {
  const { data: settings, error, refetch } = useQuery<PlatformSettings>({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
    retry: false,
  });
  // The whole card vanished on a failed read, which on a page of facts about
  // the install reads as "this install has no such facts".
  if (error) {
    return (
      <Section id="install" title="Install">
        <div className="p-4">
          <QueryError
            title="The install’s configuration could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      </Section>
    );
  }
  if (!settings) return null;
  const posture = APP_ACCOUNTS[settings.app_accounts];

  return (
    <Section id="install" title="Install" description={<>
          Set when this install was deployed. These decide what{" "}
          <Link to="/admin/settings" className="underline underline-offset-2 hover:text-foreground">
            Settings
          </Link>{" "}
          can turn on.
        </>}>
      <div className="p-4">
        <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
          <div className="flex justify-between sm:flex-col sm:gap-0.5">
            <dt className="text-muted-foreground">Topology</dt>
            <dd>
              <Badge variant="secondary">{TOPOLOGY_LABEL[settings.topology]}</Badge>
            </dd>
          </div>
          <div className="flex justify-between sm:flex-col sm:gap-0.5">
            <dt className="text-muted-foreground">Isolated backend</dt>
            <dd>
              {settings.isolated_possible ? (
                <Badge variant="secondary">Configured</Badge>
              ) : (
                <Badge variant="outline">Not configured</Badge>
              )}
            </dd>
          </div>
          <div className="flex justify-between sm:flex-col sm:gap-0.5">
            <dt className="text-muted-foreground">Runner images</dt>
            <dd>
              {settings.runner_images?.length ? (
                <span className="flex flex-wrap gap-1">
                  {settings.runner_images.map((f) => (
                    <Badge key={f} variant="secondary">
                      {f}
                    </Badge>
                  ))}
                </span>
              ) : (
                <span className="text-muted-foreground">None</span>
              )}
            </dd>
          </div>
          {/* Factual, never a health warning. A local key is a supported
              configuration chosen deliberately — it is what every air-gapped
              site and the one-box install run — so this states what is
              protecting the data and does not grade it. */}
          <div className="flex justify-between sm:flex-col sm:gap-0.5">
            <dt className="text-muted-foreground">Secrets at rest</dt>
            <dd className="flex flex-wrap items-center gap-1.5">
              <Badge variant="secondary">
                {settings.encryption_backend === "managed"
                  ? "AES-256-GCM, key in a KMS"
                  : "AES-256-GCM, local key"}
              </Badge>
              <span className="font-mono text-2xs text-muted-foreground">
                {settings.encryption_key_id}
              </span>
            </dd>
          </div>
          {settings.encryption_kek && (
            <div className="flex justify-between sm:flex-col sm:gap-0.5">
              <dt className="text-muted-foreground">Key-encrypting key</dt>
              <dd className="break-all font-mono text-xs">{settings.encryption_kek}</dd>
            </div>
          )}
          {/* Spans both columns: the platform-user case carries a sentence
              nobody should have to read in a half-width cell, and the fix is
              in it.

              Absent when the value is one this build does not know. The server
              always sends one and the SPA ships inside the same binary, so this
              is unreachable in an install — but the alternative to saying
              nothing is guessing, and every guess here is a claim about
              whether the apps on this box can read each other's secrets. */}
          {/* Spans both columns for the same reason the row below it does: the
              sentence that matters is the one the platform *cannot* say. The
              isolated half of the block is a NetworkPolicy nothing verifies, so
              "On" means the rule was written and not that anything enforces it.
              Stated, never graded — an operator who chose their CNI knowingly
              should not be nagged, and one who did not is the only reader who
              can fix it. docs/invariants.md § Process and network boundaries. */}
          <div className="flex flex-col gap-0.5 sm:col-span-2">
            <dt className="text-muted-foreground">Cloud metadata endpoints</dt>
            <dd className="flex flex-wrap items-center gap-2">
              {/* `attention`, as System checks' `cloud_metadata` row warns:
                  on a cloud machine this hands out its identity, on any other
                  it reaches nothing, and the install cannot tell which. The two
                  pages had disagreed — red here, passing there. */}
              <Badge
                variant="outline"
                className={toneSurface[settings.metadata_blocked ? "ok" : "attention"]}
              >
                {settings.metadata_blocked ? "Blocked from apps" : "Reachable by apps"}
              </Badge>
              <span className="max-w-prose text-xs text-muted-foreground">
                {settings.metadata_blocked
                  ? settings.isolated_possible
                    ? "Isolated apps rely on the cluster’s network policy engine, which Toft can’t verify."
                    : "Enforced by a firewall rule."
                  : "Apps can take the cloud identity of the machine they run on. Fix: set BLOCK_AWS_METADATA."}
              </span>
            </dd>
          </div>
          {posture && (
            <div className="flex flex-col gap-0.5 sm:col-span-2">
              <dt className="text-muted-foreground">Apps run as (shared mode)</dt>
              <dd className="flex flex-wrap items-center gap-2">
                <Badge variant="outline" className={toneSurface[posture.tone]}>
                  {posture.label}
                </Badge>
                <span className="max-w-prose text-xs text-muted-foreground">{posture.detail}</span>
              </dd>
            </div>
          )}
          {/* How the shared-mode agent holds this machine's workloads — its
              tier, detected once at its start, and why the next tier up was
              refused, in the agent's words. Stated rather than graded beyond
              the tone: the reason is the one an operator can act on. */}
          {settings.shared_containment && (
            <div className="flex flex-col gap-0.5 sm:col-span-2">
              <dt className="text-muted-foreground">Shared containment</dt>
              <dd className="flex flex-wrap items-center gap-2">
                <Badge
                  variant="outline"
                  className={toneSurface[settings.shared_containment.tier === "namespace" ? "ok" : "attention"]}
                >
                  {CONTAINMENT_LABEL[settings.shared_containment.tier] ?? settings.shared_containment.tier}
                </Badge>
                <span className="max-w-prose text-xs text-muted-foreground">
                  {containmentSentence(settings.shared_containment)}
                </span>
              </dd>
            </div>
          )}
          {/* Where an app's output goes beyond its own Logs tab, per mode this
              install offers. Isolated output is the cluster's log agent's,
              which Toft cannot detect and so never claims. */}
          <div className="flex flex-col gap-0.5 sm:col-span-2">
            <dt className="text-muted-foreground">App output</dt>
            <dd className="flex max-w-prose flex-col gap-0.5 text-xs text-muted-foreground">
              <span>
                Shared apps: copied to the platform’s log,{" "}
                {settings.app_log_copy_uncapped ? "uncapped" : "capped at 100 lines a second per app"}.
              </span>
              {settings.isolated_execution_enabled && (
                <span>Isolated apps: your cluster’s log agent. Without one, output is lost at every redeploy.</span>
              )}
              {settings.app_accounts !== "per-app" && (
                <span>App logs on this install are not protected from the app that wrote them.</span>
              )}
            </dd>
          </div>
        </dl>

        <EnvelopeBlock settings={settings} />
      </div>
    </Section>
  );
}

/**
 * The operating envelope: every dial's effective value beside the figure this
 * process was started with, and the two logging facts that are not dials.
 *
 * The other half of reducing the knob count. A variable an administrator can
 * *see the effect of* is one they stop having to hunt for, and until this
 * existed the only way to answer "what is this install actually running with"
 * was to read a unit file on a box they may not have. It is the same argument
 * that put `app_accounts` above: the person who can fix a thing has to be told
 * what it is.
 *
 * Read-only here, deliberately. Five of these are settable on Settings and this
 * page never duplicates a control — what it adds is the environment's figure
 * beside the effective one, which is the pair neither page can show alone.
 */
function EnvelopeBlock({ settings }: { settings: PlatformSettings }) {
  const rows: { label: string; effective: string; env: string }[] = [
    {
      label: "Memory ceiling per app",
      effective: mb(settings.app_memory_limit_mb || settings.env_app_memory_limit_mb),
      env: mb(settings.env_app_memory_limit_mb),
    },
    {
      label: "CPU reserved per app",
      effective: cpu(
        settings.app_cpu_request_millicores || settings.env_app_cpu_request_millicores,
      ),
      env: cpu(settings.env_app_cpu_request_millicores),
    },
    {
      label: "CPU ceiling per app",
      effective: cpu(settings.app_cpu_limit_millicores || settings.env_app_cpu_limit_millicores),
      env: cpu(settings.env_app_cpu_limit_millicores),
    },
    {
      label: "Builds at once",
      effective: `${settings.max_concurrent_builds || settings.env_max_concurrent_builds} of ${settings.max_concurrent_builds_ceiling} workers`,
      env: String(settings.env_max_concurrent_builds),
    },
    {
      label: "Auto-deploy check",
      effective: settings.auto_deploy_poller_running
        ? `every ${settings.auto_deploy_poll_seconds || settings.env_auto_deploy_poll_seconds}s`
        : "not running",
      env: settings.env_auto_deploy_poll_seconds
        ? `every ${settings.env_auto_deploy_poll_seconds}s`
        : "off",
    },
    {
      label: "R package repository",
      effective: settings.r_package_repository || settings.env_r_package_repository || "—",
      env: settings.env_r_package_repository || "—",
    },
    {
      label: "Log level",
      effective: settings.effective_log_level,
      env: settings.env_log_level,
    },
  ];

  return (
    <div className="mt-5 border-t border-border/60 pt-4">
      <div className="mb-3">
        <SectionHeading
          level={3}
          title="Limits and logging"
          description={
            <>
              Override these in{" "}
              <Link to="/admin/settings#execution" className="underline underline-offset-2 hover:text-foreground">
                Settings
              </Link>
              ; a cleared setting falls back to the install’s figure.
            </>
          }
        />
      </div>
      {/* Label and value side by side in two columns. Justified to the card's
          edges, a value sat a thousand pixels from the label it answered. */}
      <dl className="space-y-1.5 text-sm">
        {rows.map((r) => (
          <div key={r.label} className="grid gap-x-6 gap-y-0.5 sm:grid-cols-[14rem_minmax(0,1fr)] sm:items-baseline">
            <dt className="text-muted-foreground">{r.label}</dt>
            {/* Wraps, and a URL breaks anywhere: the package repository's
                value and its install default side by side ran 63px off the
                card at 768. */}
            <dd className="flex min-w-0 flex-wrap items-baseline gap-x-2">
              <span className="font-mono text-xs break-all">{r.effective}</span>
              {r.effective !== r.env && (
                <span className="text-2xs break-all text-muted-foreground">
                  install: <span className="font-mono">{r.env}</span>
                </span>
              )}
            </dd>
          </div>
        ))}
        {/* Not a dial: what code a build may run is the environment's to say. */}
        <div className="grid gap-x-6 gap-y-0.5 sm:grid-cols-[14rem_minmax(0,1fr)] sm:items-baseline">
          <dt className="text-muted-foreground">npm lifecycle scripts</dt>
          <dd className="flex items-baseline gap-2">
            <span className="font-mono text-xs">
              {settings.build_allow_scripts ? "allowed" : "blocked"}
            </span>
            <span className="text-2xs text-muted-foreground">
              Set at deployment (BUILD_ALLOW_SCRIPTS)
            </span>
          </dd>
        </div>
      </dl>
    </div>
  );
}

/** A stored ceiling said the way somebody would say it out loud. */
function mb(n: number): string {
  if (n <= 0) return "no limit";
  return n >= 1024 && n % 1024 === 0 ? `${n / 1024} GB` : `${n} MB`;
}

/** Millicores, with the core figure beside it where that reads better. */
function cpu(n: number): string {
  if (n <= 0) return "none";
  return n % 1000 === 0 ? `${n / 1000} core${n === 1000 ? "" : "s"}` : `${n}m`;
}

export function AdminInfoPage() {
  const { data: health, isLoading, error, refetch } = useQuery<SystemHealth>({
    queryKey: ["system-health"],
    queryFn: api.getSystemHealth,
    refetchInterval: 10_000,
    retry: false,
  });
  // `#install` still arrives here from links that used to say
  // `/admin/settings#install`.
  useHashScroll(!!health);

  // `&& !health` matters more here than anywhere: this read polls every ten
  // seconds, so without it one dropped poll replaced a rendered page.
  //
  // And it is a QueryError rather than a hand-rolled empty state. That state
  // told an administrator watching their own install fall over that "this page
  // is limited to system administrators" — the sentence QueryError's own
  // docstring calls the worst available one — and it was defended by a comment
  // saying the API reports no status, which has not been true since `ApiError`
  // gained one. QueryError leads with the server's own words and offers a Retry
  // only when there is something to retry.
  if (error && !health) {
    return (
      <div>
        <PageHeader title="System info" />
        <QueryError
          title="System info could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  if (isLoading || !health) {
    return (
      <div>
        <PageHeader title="System info" />
        <div className="grid grid-cols-2 gap-3">
          {Array.from({ length: 2 }).map((_, i) => (
            <Skeleton key={i} className="h-24" />
          ))}
        </div>
      </div>
    );
  }

  const { server } = health;

  return (
    <div>
      <PageHeader title="System info" />

      <div className="space-y-6">
        <div className="grid grid-cols-2 gap-3">
          <StatCard
            label="Version"
            value={server ? `v${server.version}` : "—"}
            icon={<Box className="h-4 w-4" />}
            description={server?.revision || undefined}
          />
          <StatCard
            label="Uptime"
            value={server ? formatUptime(server.uptime_seconds) : "—"}
            icon={<Clock className="h-4 w-4" />}
          />
        </div>

        {/* Addressable, so Usage can point at it. Estate demand over a window
            is Usage's question, and this is the install-wide answer to it. */}
        <div id="people" className="scroll-mt-20">
          <ActiveUsersRow />
        </div>

        <InstallCard />

        {server && <LanguageVersionsCard server={server} />}
        {/* Which apps resolve to each version — the other half of the card
            above, which had a page of its own in another sidebar group. */}
        <section id="languages" className="scroll-mt-20">
          <AdminEstatePage />
        </section>

        <Section title="Server">
          <div className="p-4">
            <dl className="grid grid-cols-1 gap-3 sm:grid-cols-2 text-sm">
              <div className="flex justify-between sm:flex-col sm:gap-0.5">
                <dt className="text-muted-foreground">Hostname</dt>
                <dd className="font-mono">{server?.hostname ?? "—"}</dd>
              </div>
              <div className="flex justify-between sm:flex-col sm:gap-0.5">
                <dt className="text-muted-foreground">Operating system</dt>
                <dd className="font-mono">{server?.os ?? "—"}</dd>
              </div>
            </dl>
          </div>
        </Section>

        <p className="text-xs text-muted-foreground">Toft is made by Burnspring.</p>
      </div>
    </div>
  );
}

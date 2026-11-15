import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  Building2,
  Database,
  FileJson,
  Globe,
  KeyRound,
  LockKeyhole,
  Mail,
  Send,
  Terminal,
  User,
  Users,
} from "lucide-react";

import { api, type AppReach, type ReachIntegration } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { QueryError } from "@/components/query-error";
import { Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { accessBadgeClass, accessLabel } from "@/lib/storage";
import { cn, grantRoleLabel } from "@/lib/utils";
import { formatBytes } from "@/lib/bytes";
import { StatusBadge } from "@/components/status-badge";
import type { StorageAccess } from "@/lib/api";

/**
 * An app's Reach tab: everything this platform has given it, in one place.
 *
 * Its own tab rather than a section on Overview or a block in Settings. §53
 * already found Overview overloaded and moved two things *out* of it; and
 * Settings is a page of writes, where a read either grows a Save button it must
 * never have or reads as a control somebody forgot to finish.
 *
 * **Nothing here is editable, and nothing here is a value.** Every *section*
 * links to the tab that owns what it lists, which is the granularity the
 * destination actually has — six grants on one app all lead to the Access tab,
 * and a link per row would be the same address six times. The claim used to be
 * made per row and honoured nowhere: seven sections of read-only facts with no
 * way out of any of them, on a tab a reader reaches precisely because
 * something on it needs changing.
 *
 * The sections are in the order the closed list is in — who may open it, who
 * may frame it, what it is told, what it holds, what it can write to, what it
 * can send, who can act as it — because that order is roughly "outward", and a
 * reader scanning for the alarming row is scanning for the last third.
 */
/**
 * The way out of a section, to the tab that owns what it lists.
 *
 * A read-only page whose whole subject is *what this app has been given* is
 * reached because something on it needs changing, and every one of its seven
 * sections ended in a fact with nowhere to go.
 */
function OwnedBy({ appId, tab, label }: { appId: string; tab: string; label: string }) {
  return (
    <Link
      to={`/apps/${appId}/${tab}`}
      className="text-xs font-medium text-primary whitespace-nowrap hover:underline"
    >
      {label}
    </Link>
  );
}

export function ReachTab({ appId }: { appId: string }) {
  const { data, isLoading, error, refetch } = useQuery<AppReach>({
    queryKey: ["app-reach", appId],
    queryFn: () => api.getAppReach(appId),
  });
  // Each kind's catalog title, from the read the Integrations tab already
  // makes (same gate, shared cache) — so a row says "Slack", never "slack".
  const { data: appIntegrations } = useQuery({
    queryKey: ["app-integrations", appId],
    queryFn: () => api.getAppIntegrations(appId),
    enabled: (data?.integrations.length ?? 0) > 0,
  });
  const integrationTitles: Record<string, string> = Object.fromEntries(
    (appIntegrations?.integrations ?? []).map((i) => [i.kind, i.title]),
  );

  if (error) {
    return (
      <div className="pt-4">
        <QueryError
          title="What this app can reach could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see what it can reach."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }
  if (isLoading || !data) {
    return (
      <div className="space-y-4 pt-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-28" />
        ))}
      </div>
    );
  }

  return (
    <div className="space-y-4 pt-4">
      {/* The line that is not allowed to be missing, and it leads rather than
          trails: a page called Reach is read as exhaustive, and the one gap
          the platform genuinely has should be known before the list is read
          rather than after. It comes from the API so this surface and the
          estate page cannot drift apart. */}
      {/* Background, not a state, so a muted line rather than an info
          Notice — but still first. And where the app holds nothing, that is
          the summary, so it leads too instead of closing the page after four
          sections that each said so. */}
      <div className="max-w-prose space-y-1 text-xs text-muted-foreground">
        {holdsNothing(data) && (
          <p className="text-sm text-foreground">
            This app holds no credentials, mounts no storage, sends nothing and has no keys.
          </p>
        )}
        <p>{data.cannot_see}</p>
      </div>

      <Section
        title="Who can open it"
        actions={<OwnedBy appId={appId} tab="access" label="Access" />}
      >
        <div className="space-y-3 px-4 py-3">
          <Audience reach={data} />
          {data.owner && (
            <Row
              icon={data.owner.kind === "group" ? <Users /> : <User />}
              name={data.owner.name || data.owner.id}
              detail={data.owner.kind === "group" ? "Owner — a group" : "Owner"}
            />
          )}
          {data.grants.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              Nobody else has been granted access by name.
            </p>
          ) : (
            data.grants.map((g) => (
              <Row
                key={`${g.principal_type}:${g.principal_id}`}
                icon={g.principal_type === "group" ? <Users /> : <User />}
                name={g.name || g.principal_id}
                detail={`${grantRoleLabel(g.role)}${g.principal_type === "group" ? " — a group" : ""}`}
              />
            ))
          )}
        </div>
      </Section>

      <Section
        title="Viewer identity and embedding"
        actions={<OwnedBy appId={appId} tab="access" label="Access" />}
      >
        <div className="space-y-3 px-4 py-3">
          <Row
            icon={<Users />}
            name="Viewer identity"
            detail={
              data.viewer_identity
                ? "The app can ask who is looking at it."
                : "The app cannot ask who is looking at it."
            }
            tone={data.viewer_identity ? "attention" : undefined}
          />
          <Row
            icon={<Globe />}
            name="Embedding"
            detail={
              data.frame_ancestors
                ? `Framed by ${data.frame_ancestors}`
                : "Only this install may frame it."
            }
            mono={!!data.frame_ancestors}
          />
        </div>
      </Section>

      <Section
        title="Credentials and variables"
        actions={<OwnedBy appId={appId} tab="settings" label="Settings" />}
        description="Credentials and environment variables, by name only."
      >
        <div className="space-y-3 px-4 py-3">
          {data.credentials.length === 0 && data.env_keys.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No credentials or environment variables.
            </p>
          ) : (
            <>
              {data.credentials.map((c) => (
                <Row
                  key={c.id}
                  icon={<KeyRound />}
                  name={c.name}
                  detail={
                    c.keys.length > 0
                      ? `Credential — ${c.keys.join(", ")}`
                      : "Credential"
                  }
                  monoDetail
                />
              ))}
              {data.env_keys.map((e) => (
                <Row
                  key={e.key}
                  icon={<Terminal />}
                  name={e.key}
                  mono
                  detail={
                    e.sensitive
                      ? "Environment variable — the value is write-only"
                      : "Environment variable"
                  }
                />
              ))}
            </>
          )}
        </div>
      </Section>

      {/* Absent, not off. An install that has provisioned no storage is not an
          install where this app was refused some. */}
      {data.show_storage && (
        <Section
          title="Storage"
          actions={<OwnedBy appId={appId} tab="overview" label="Overview" />}
                  >
          <div className="space-y-3 px-4 py-3">
            {data.mounts.length === 0 ? (
              <p className="text-xs text-muted-foreground">No storage attached.</p>
            ) : (
              data.mounts.map((m) => {
                const access = m.access as StorageAccess;
                return (
                  <Row
                    key={`${m.resource_id}:${m.path}`}
                    icon={<Database />}
                    name={m.path}
                    mono
                    detail={
                      m.resource_missing
                        ? `${m.resource_slug || m.resource_id} — this resource no longer exists`
                        : `${m.resource_name || m.resource_slug}${m.folder ? ` / ${m.folder}` : ""}`
                    }
                    badge={
                      <Badge
                        variant="outline"
                        className={cn("text-xs font-medium", accessBadgeClass(access))}
                      >
                        {accessLabel(access)}
                      </Badge>
                    }
                  />
                );
              })
            )}
          </div>
        </Section>
      )}

      {data.show_integrations && (
        <Section
          title="Integrations"
          actions={<OwnedBy appId={appId} tab="integrations" label="Integrations" />}
          description="Channels an administrator attached, including ones switched off."
        >
          <div className="space-y-3 px-4 py-3">
            {data.integrations.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No integrations attached.
              </p>
            ) : (
              data.integrations.map((i) => (
                <Row
                  key={i.kind}
                  icon={<Send />}
                  name={integrationTitles[i.kind] ?? i.kind}
                  detail={i.destination || "The connection’s default destination"}
                  badge={<IntegrationState integration={i} />}
                />
              ))
            )}
          </div>
        </Section>
      )}

      <Section
        title="Data and reports"
        actions={<OwnedBy appId={appId} tab="settings" label="Settings" />}
      >
        <div className="space-y-3 px-4 py-3">
          <Row
            icon={<FileJson />}
            name="Stored document"
            detail={
              data.data
                ? `${formatBytes(data.data.bytes)}, version ${data.data.version}`
                : "The app has never written one."
            }
          />
          {data.report_recipients.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              Nobody is emailed on a schedule.
            </p>
          ) : (
            data.report_recipients.map((p) => (
              <Row
                key={`${p.schedule_id}:${p.user_id}`}
                icon={<Mail />}
                name={p.name || p.email || p.user_id}
                detail={
                  p.enabled
                    ? "Emailed on each re-render"
                    : "Schedule suspended"
                }
                tone={p.enabled ? "attention" : undefined}
              />
            ))
          )}
        </div>
      </Section>

    </div>
  );
}

function holdsNothing(r: AppReach): boolean {
  return (
    r.credentials.length === 0 &&
    r.env_keys.length === 0 &&
    r.mounts.length === 0 &&
    r.integrations.length === 0 &&
    r.report_recipients.length === 0 &&
    !r.data
  );
}

/**
 * Who can open the app — the **effective** answer, never the stored column.
 *
 * The same three shapes the scan-target list draws, and for the same reason: a
 * page that said `public` about an app the install has withheld would send
 * somebody to test access the platform refuses.
 */
function Audience({ reach }: { reach: AppReach }) {
  if (reach.visibility === "public") {
    return (
      <Row
        icon={<Globe />}
        name="Public"
        detail="Anyone with the link, without signing in."
        tone="attention"
      />
    );
  }
  if (reach.visibility === "authenticated") {
    return (
      <Row
        icon={<Building2 />}
        name="Anyone signed in"
        detail={
          reach.visibility_withheld
            ? "Public, but this install allows signed-in viewers only."
            : "Everyone with an account on this install."
        }
      />
    );
  }
  return (
    <Row
      icon={<LockKeyhole />}
      name="Restricted"
      detail="Only the owner and the people below."
    />
  );
}

/**
 * The three booleans, as one word a person acts on.
 *
 * Attached-and-not-effective is the state worth drawing: it is not "off", it is
 * an attachment waiting behind a switch somebody else owns, and reporting it as
 * absent is how it gets forgotten until the day it starts sending again.
 */
function IntegrationState({ integration }: { integration: ReachIntegration }) {
  if (integration.connected && integration.enabled) {
    return <StatusBadge status="" tone="ok" label="Can send" />;
  }
  return (
    <StatusBadge
      status=""
      tone="neutral"
      label={integration.connected ? "Attached, switched off" : "Attached, not offered to apps"}
    />
  );
}

/** One line of the list: an icon, the name of the thing, what it is. */
function Row({
  icon,
  name,
  detail,
  badge,
  mono,
  monoDetail,
  tone,
}: {
  icon: React.ReactNode;
  name: string;
  detail?: string;
  badge?: React.ReactNode;
  mono?: boolean;
  monoDetail?: boolean;
  tone?: "attention";
}) {
  return (
    <div className="flex items-start gap-2.5">
      <span
        className={cn(
          "mt-0.5 shrink-0 [&>svg]:h-4 [&>svg]:w-4",
          tone === "attention" ? "text-foreground" : "text-muted-foreground",
        )}
        aria-hidden="true"
      >
        {icon}
      </span>
      <div className="min-w-0 flex-1">
        <div className={cn("truncate text-sm text-foreground", mono && "font-mono text-xs")}>
          {name}
        </div>
        {detail && (
          <div className={cn("text-xs text-muted-foreground", monoDetail && "font-mono")}>
            {detail}
          </div>
        )}
      </div>
      {badge && <span className="shrink-0">{badge}</span>}
    </div>
  );
}

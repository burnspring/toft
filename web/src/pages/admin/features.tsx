import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Package } from "lucide-react";
import { toast } from "sonner";

import { api, type Feature } from "@/lib/api";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import { Separator } from "@/components/ui/separator";
import { PageHeader } from "@/components/page-header";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { toneText } from "@/lib/tone";
import { TableCard } from "@/components/table-card";

/**
 * Admin → Features: what this build offers, and what an administrator has
 * switched on.
 *
 * Toft is free (ADR §32, *Toft is free*), so every feature is included and no
 * row is locked: the server answers each one licensed, under the perpetual
 * license every install holds. What is left is the administrator's switch,
 * which is a separate question — somebody must be able to switch a feature off
 * during an incident or while a data-egress policy is reviewed.
 *
 * **Some rows have no switch at all.** Identity providers, groups, storage and
 * the other configuration surfaces are in this catalog, and a toggle for them
 * would be switching off the way colleagues sign in. Where `admin_switch` is
 * false the row says *Always on* and offers nothing to throw.
 *
 * A switched-off feature keeps its row rather than disappearing. Absent and off
 * are different states: an administrator whose question box stopped answering
 * opens this page to find out why, and a row that vanished would tell them
 * nothing.
 *
 * Per the confirmation policy, the switch is a reversible admin action: it acts
 * immediately and toasts. There is no dialog, because switching a feature off
 * takes nothing down — the app keeps serving and says why it cannot answer.
 */
export function AdminFeaturesPage() {
  const queryClient = useQueryClient();

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["features"],
    queryFn: api.listFeatures,
    retry: false,
  });
  // `/admin/features#features` from ⌘K, once there is a table to land on.
  useHashScroll(!!data);

  const toggle = useMutation({
    mutationFn: ({ name, enabled }: { name: string; enabled: boolean }) =>
      api.setFeatureEnabled(name, enabled),
    onSuccess: (f) => {
      toast.success(f.enabled ? `${f.title} is on` : `${f.title} is off`, {
        description: f.enabled
          ? "Apps granted it can use it now."
          : "Pages that use it now say it’s off.",
      });
      // The install action and the app's own page both read this state, and an
      // administrator who has just switched the FAQ off should not find its app
      // still claiming to be available.
      queryClient.invalidateQueries({ queryKey: ["features"] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
    },
    onError: (e, { name, enabled }) => {
      const title = data?.find((f) => f.name === name)?.title ?? name;
      toast.error(`Couldn’t switch ${title} ${enabled ? "on" : "off"}`, {
        description: e instanceof Error ? e.message : undefined,
      });
    },
  });

  const header = (
    <PageHeader
      title="Features"
      description="Optional capabilities: which this build offers, and which are on."
    />
  );

  if (isLoading) {
    return (
      <div>
        {header}
        <Skeleton className="h-48 w-full rounded-lg" />
      </div>
    );
  }

  if (error) {
    return (
      <div>
        {header}
        <QueryError
          title="The feature list could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const features = data ?? [];
  if (features.length === 0) {
    return (
      <div>
        {header}
        <EmptyState
          icon={<Package className="h-6 w-6" />}
          title="This build offers no optional features"
          description="Everything it can do is on by default."
        />
      </div>
    );
  }

  return (
    <div>
      {header}
      <TableCard id="features" className="scroll-mt-20">
        {features.map((f, i) => (
          // Each row is an anchor, because a page that says "turn it on in
          // Features" has to land somebody on the switch rather than at the top
          // of a list of nine.
          <div key={f.name} id={`feature-${f.name}`} className="scroll-mt-20">
            {i > 0 && <Separator />}
            <FeatureRow
              feature={f}
              pending={toggle.isPending && toggle.variables?.name === f.name}
              onToggle={(enabled) => toggle.mutate({ name: f.name, enabled })}
            />
          </div>
        ))}
      </TableCard>
    </div>
  );
}

function FeatureRow({
  feature,
  pending,
  onToggle,
}: {
  feature: Feature;
  pending: boolean;
  onToggle: (enabled: boolean) => void;
}) {
  return (
    <div className="flex flex-col gap-4 p-5 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0 space-y-1.5">
        <h3 className="text-sm font-semibold text-foreground">{feature.title}</h3>
        <p className="max-w-prose text-sm text-muted-foreground">{feature.summary}</p>

        {feature.enabled_at && (
          <p className="text-xs text-muted-foreground">
            {feature.enabled ? "On since" : "Last switched on"}{" "}
            <RelativeTime iso={feature.enabled_at} />
          </p>
        )}

        {/* Off, but it has been on before. The row stays for exactly this
            sentence: an administrator whose question box vanished needs to see
            that somebody threw a switch rather than find nothing on the page. */}
        {!feature.enabled && feature.ever_enabled && (
          <p className={`text-xs ${toneText.attention}`}>
            Switched off. Pages that use it say so.
          </p>
        )}
      </div>

      <div className="flex shrink-0 items-center gap-3">
        {/* No switch where there is nothing to switch. Rendering a disabled one
            would invite somebody to work out why it is disabled, and the answer
            would be "it never does anything" — which is not a state a control
            should be able to have. */}
        {feature.admin_switch ? (
          <Switch
            checked={feature.enabled}
            disabled={pending}
            onCheckedChange={onToggle}
            aria-label={feature.title}
          />
        ) : (
          <span className="text-xs text-muted-foreground">Always on</span>
        )}
      </div>
    </div>
  );
}

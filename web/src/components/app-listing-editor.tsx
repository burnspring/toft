import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type PlatformSettings } from "@/lib/api";
import { QueryError } from "@/components/query-error";
import { Section, SectionHeading } from "@/components/section";
import { SettingRow } from "@/components/setting-row";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";

/**
 * Whether everyone signed in can see that every app exists.
 *
 * The `list` rung (ADR §90) granted install-wide instead of row by row — which
 * is what makes the rung worth having at all. Granted per person it is the same
 * work as granting Viewer and earns nothing; granted here it is the difference
 * between an internal platform whose apps you can ask for and one whose apps
 * you have to already know about.
 *
 * Beside anonymous access because they are the same question about two
 * audiences, and this is the smaller half of it: nothing becomes *readable*.
 * Every app on the list still refuses to open for anybody who was not given it.
 *
 * **Default on**, which is the decision this page has to be honest about. An
 * install upgrading to a build carrying it discloses every app's name, slug and
 * owner to everybody who can sign in, on the first request and without being
 * asked — the argument being that a discovery feature nobody discovers is not a
 * feature, and the whole access-request flow has nothing to attach to while
 * apps are invisible. So this switch is mostly read by somebody turning it
 * *off*, and the copy is written for them.
 *
 * It saves the moment it is thrown (ADR §50): the server's warning counts the
 * names it is about to disclose, and that count belongs to the switch that
 * caused it.
 */
export function AppListingEditor({ bare = false }: { bare?: boolean } = {}) {
  const queryClient = useQueryClient();
  const {
    data: settings,
    isLoading,
    error: readError,
    refetch,
  } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
  });

  const save = useMutation({
    mutationFn: api.updatePlatformSettings,
    onSuccess: (
      result: { settings: PlatformSettings; warning: string },
      vars: Partial<PlatformSettings>,
    ) => {
      queryClient.setQueryData(["platform-settings"], (old: PlatformSettings | undefined) =>
        old ? { ...old, ...result.settings } : old,
      );
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      // Everybody's app list changes on this save, so the listing goes with it.
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      if (result.warning) toast.warning(result.warning, { duration: 12_000 });
      // A switch says what is now true, and of whom.
      else
        toast.success(
          vars.apps_listable_to_authenticated
            ? "Everyone signed in can see every app’s name"
            : "People see only apps shared with them",
        );
    },
    onError: (error: Error) => toast.error(error.message),
  });

  const heading = (
    <SectionHeading
      title="What everyone signed in can see"
    />
  );

  if (readError) {
    return (
      <section id="listing" className="scroll-mt-20 space-y-3">
        {heading}
        <QueryError
          title="The app listing setting could not be read"
          error={readError}
          fallback="The install’s settings are limited to system administrators."
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  if (isLoading || !settings) {
    return (
      <section id="listing" className="scroll-mt-20 space-y-3">
        {heading}
        <Skeleton className="h-32 w-full rounded-lg" />
      </section>
    );
  }

  const body = (
      <div className="divide-y divide-border/60 p-4">
        <SettingRow
          id="apps_listable_to_authenticated"
          note={
            settings.apps_listable_to_authenticated
              ? "Nobody gains access: opening an app still needs a grant. An owner can withdraw an app from the list on its Access tab."
              : "When off, people see only apps shared with them, so nobody can ask for one they haven’t heard of."
          }
        >
          <Switch
            id="apps_listable_to_authenticated"
            checked={settings.apps_listable_to_authenticated}
            disabled={save.isPending}
            onCheckedChange={(v) => save.mutate({ apps_listable_to_authenticated: v })}
          />
        </SettingRow>
      </div>
  );
  // `bare` inside the page's one "Who can see apps" card, which holds this
  // and its sibling; the anchor stays on the row block either way.
  return bare ? (
    <div id="listing" className="scroll-mt-20">{body}</div>
  ) : (
    // A titled card (`Section`), like every other single-card region on the
    // page — the heading above a bare card was the second grammar.
    <Section id="listing" title="What everyone signed in can see">
      {body}
    </Section>
  );
}

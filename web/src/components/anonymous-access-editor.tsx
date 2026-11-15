import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type PlatformSettings } from "@/lib/api";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryError } from "@/components/query-error";
import { Switch } from "@/components/ui/switch";
import { PUBLIC_REACH_HREF } from "@/pages/admin/reach";
import { Notice } from "@/components/notice";
import { Section, SectionHeading } from "@/components/section";
import { SettingRow } from "@/components/setting-row";

/**
 * Whether this install serves anybody who has not signed in.
 *
 * On Authentication rather than on Settings for the reason the session policy
 * and the dormancy pair are (ADR §50 D2): this is the page that shows what it
 * governs. Everything else here decides who may be here and for how long; this
 * one decides whether being here is required at all, so it leads the page's
 * policy half — a reader who has just configured an identity provider and wants
 * to know whether it can be walked past should not have to find that on a page
 * of retention figures.
 *
 * The switch saves the moment it is thrown (ADR §50): the server's warning names
 * how many apps it takes off the internet, and a warning that belonged to a
 * batch of edits could not say which one caused it.
 *
 * No confirmation dialog. It is reversible in the strongest sense the product
 * has — no app is edited, so turning it back on republishes exactly the same
 * apps — and the count is on screen *before* the switch is thrown rather than
 * in a dialog after it, which is the same argument the confirmation policy
 * makes for direct-and-toast.
 */
export function AnonymousAccessEditor({ bare = false }: { bare?: boolean } = {}) {
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
      // Every app's own page reads the same fact off its row — the Access tab
      // greys out Public and says why — so the app queries go too. An admin who
      // threw this switch and then opened an app would otherwise be looking at
      // a cached page still offering the thing they just closed.
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["app"] });
      queryClient.invalidateQueries({ queryKey: ["capabilities"] });
      queryClient.invalidateQueries({ queryKey: ["system-audience"] });
      queryClient.invalidateQueries({ queryKey: ["app-usage"] });
      if (result.warning) toast.warning(result.warning, { duration: 12_000 });
      // A switch says what is now true, and of whom.
      else if (vars.audience_measurement_enabled !== undefined)
        toast.success(
          vars.audience_measurement_enabled
            ? "Anonymous browsers are counted from now on"
            : "Anonymous browsers aren’t counted, and their cookies are removed as they return",
        );
      else
        toast.success(
          vars.public_apps_enabled
            ? "Apps their owners published are open to anyone again"
            : "Nothing on this install opens without signing in",
        );
    },
    onError: (error: Error) => toast.error(error.message),
  });

  const heading = (
    <SectionHeading title="Anonymous access" />
  );

  // Before the skeleton: a read that failed would otherwise leave this on its
  // loading shape for ever, which is the loading state as a permanent answer.
  if (readError) {
    return (
      <section id="anonymous" className="scroll-mt-20 space-y-3">
        {heading}
        <QueryError
          title="The anonymous access setting could not be read"
          error={readError}
          fallback="The install’s settings are limited to system administrators."
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  if (isLoading || !settings) {
    return (
      <section id="anonymous" className="scroll-mt-20 space-y-3">
        {heading}
        <Skeleton className="h-32 w-full rounded-lg" />
      </section>
    );
  }

  const published = settings.public_app_count;
  const apps = published === 1 ? "app" : "apps";

  const body = (
      <div className="divide-y divide-border/60 p-4">
        <SettingRow
          id="public_apps_enabled"
          // The blast radius, on screen before the switch is thrown rather than
          // in a toast after it. It is the owners' choices and not what is in
          // effect, so it stays truthful once the switch is already off — where
          // "in effect" would read zero and imply this control affects nothing.
          // The count is openable. It is stated here and again in Admin →
          // Reach's own description, and from neither could an administrator
          // get to *which* apps — on the one figure where the answer to
          // "which" is the reason the figure is on screen at all.
          note={
            published === 0 ? (
              "No app here is published to anonymous visitors at the moment."
            ) : (
              <>
                <Link to={PUBLIC_REACH_HREF} className="text-primary hover:underline">
                  {published} {apps}
                </Link>{" "}
                {settings.public_apps_enabled
                  ? "would stop being reachable without signing in."
                  : "would be published again."}
              </>
            )
          }
          noteTone={published > 0 && settings.public_apps_enabled ? "attention" : undefined}
        >
          <Switch
            id="public_apps_enabled"
            checked={settings.public_apps_enabled}
            disabled={save.isPending}
            onCheckedChange={(v) => save.mutate({ public_apps_enabled: v })}
          />
        </SettingRow>

        {/* The audience switch sits beside the door it counts through: with
            anonymous access off, nobody anonymous is served and the figure
            falls to zero honestly. */}
        <SettingRow
          id="audience_measurement_enabled"
          note={
            settings.audience_measurement_enabled
              ? "Each browser that opens an app without signing in gets a random ID, counted when it comes back."
              : undefined
          }
        >
          <Switch
            id="audience_measurement_enabled"
            checked={settings.audience_measurement_enabled}
            disabled={save.isPending}
            onCheckedChange={(v) => save.mutate({ audience_measurement_enabled: v })}
          />
        </SettingRow>

        {/* What the switch is *not*, stated where somebody is deciding rather
            than afterwards. Both halves are the ones people get wrong: that it
            edits the apps (it does not), and that it makes them private (it
            does not — a colleague who could open it still can). */}
        {!settings.public_apps_enabled && published > 0 && (
          <div className="pt-4">
            <Notice tone="info" size="sm" title="What the owners see">
              <p>
                Those {apps} still show Public on their own page, with a note that the install
                isn’t publishing them. Signed-in access doesn’t change.
              </p>
            </Notice>
          </div>
        )}
      </div>
  );
  // `bare` inside the page's one "Who can see apps" card, which holds this
  // and its sibling; the anchor stays on the row block either way.
  return bare ? (
    <div id="anonymous" className="scroll-mt-20">{body}</div>
  ) : (
    // A titled card (`Section`), like every other single-card region on the
    // page — the heading above a bare card was the second grammar.
    <Section id="anonymous" title="Anonymous access">
      {body}
    </Section>
  );
}

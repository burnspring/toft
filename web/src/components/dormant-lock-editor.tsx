import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type PlatformSettings } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { UnitLabel } from "@/components/unit-label";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryError } from "@/components/query-error";
import { Switch } from "@/components/ui/switch";
import { Section, SectionHeading } from "@/components/section";
import { SettingRow } from "@/components/setting-row";
import { FieldError } from "@/components/field-help";
import { NUMBER_FIELDS, fieldError } from "@/lib/settings-fields";

/**
 * What happens to an account nobody is using.
 *
 * On Authentication rather than on Settings, because this is the page that
 * shows what it governs: the providers that create accounts, the account
 * lifecycle they imply, and the built-in administrator the sweep is required to
 * leave alone. The setting's own copy already sent the reader to People for the
 * column it acts on; it was on a page of unrelated switches only because
 * Settings was where everything went that had no page of its own.
 *
 * The switch saves the moment it is thrown. The threshold has its own Save
 * because it is one field and one decision — and it is hidden, not disabled,
 * while the sweep is off: a figure on screen for a sweep that is not running
 * reads as a policy in force.
 */
export function DormantLockEditor() {
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

  /** Null means "not edited"; the stored value shows through until then. */
  const [draft, setDraft] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: api.updatePlatformSettings,
    onSuccess: (result: { settings: PlatformSettings; warning: string }) => {
      queryClient.setQueryData(["platform-settings"], (old: PlatformSettings | undefined) =>
        old ? { ...old, ...result.settings } : old,
      );
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      // A warning is not a failure — the server says what turning this on will
      // do to accounts that are already dormant.
      if (result.warning) toast.warning(result.warning, { duration: 10_000 });
      else toast.success("Saved");
      setDraft(null);
    },
    onError: (error: Error) => toast.error(error.message),
  });

  const heading = (
    <SectionHeading
      title="Unused accounts"
      description="Your identity provider doesn’t tell Toft when someone leaves."
    />
  );

  // Before the skeleton: a read that failed used to leave this on its loading
  // shape for ever, which is the loading state as a permanent answer.
  if (readError) {
    return (
      <section id="dormant" className="scroll-mt-20 space-y-3">
        {heading}
        <QueryError
          title="The dormancy setting could not be read"
          error={readError}
          fallback="The install’s settings are limited to system administrators."
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  if (isLoading || !settings) {
    return (
      <section id="dormant" className="scroll-mt-20 space-y-3">
        {heading}
        <Skeleton className="h-32 w-full rounded-lg" />
      </section>
    );
  }

  const value = draft ?? String(settings.dormant_lock_days);
  const error = fieldError("dormant_lock_days", value, () => value);
  const dirty = draft !== null && Number(value.trim()) !== settings.dormant_lock_days;
  const spec = NUMBER_FIELDS.dormant_lock_days;

  return (
    // A titled card (`Section`), like every other single-card region on the
    // page — the heading above a bare card was the second grammar.
    <Section id="dormant" title="Unused accounts" description="Your identity provider doesn’t tell Toft when someone leaves.">
      <div className="divide-y divide-border/60 p-4">
        <SettingRow id="dormant_lock_enabled">
          <Switch
            id="dormant_lock_enabled"
            checked={settings.dormant_lock_enabled}
            disabled={save.isPending}
            onCheckedChange={(v) => save.mutate({ dormant_lock_enabled: v })}
          />
        </SettingRow>

        {settings.dormant_lock_enabled && (
          <SettingRow id="dormant_lock_days">
            <div className="space-y-1 sm:text-right">
              <div className="flex items-center gap-2">
                <Input
                  id="dormant_lock_days"
                  inputMode="numeric"
                  className="w-24"
                  value={value}
                  disabled={save.isPending}
                  aria-invalid={!!error}
                  aria-describedby={error ? "dormant_lock_days-error" : undefined}
                  onChange={(e) => setDraft(e.target.value)}
                />
                <UnitLabel unit={spec.unit} />
                {(dirty || save.isPending) && (
                  <Button
                    size="sm"
                    variant="outline"
                    className="h-7 text-xs"
                    disabled={!!error || save.isPending}
                    onClick={() => save.mutate({ dormant_lock_days: Number(value.trim()) })}
                  >
                    {save.isPending ? "Saving…" : "Save"}
                  </Button>
                )}
              </div>
              {error && (
                <FieldError field="dormant_lock_days" className="max-w-xs sm:ml-auto">
                  {error}
                </FieldError>
              )}
            </div>
          </SettingRow>
        )}

        <p className="pt-4 text-xs text-muted-foreground">
          <Link to="/people" className="underline underline-offset-2 hover:text-foreground">
            See when each account was last used
          </Link>{" "}
          to see who this would lock.
        </p>
      </div>
    </Section>
  );
}

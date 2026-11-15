import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type PlatformSettings } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { UnitLabel } from "@/components/unit-label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { TableCard } from "@/components/table-card";
import { Notice } from "@/components/notice";
import { SettingRow } from "@/components/setting-row";
import { FieldError } from "@/components/field-help";
import { NUMBER_FIELDS, fieldError } from "@/lib/settings-fields";
import { fieldErrorId } from "@/lib/utils";

/**
 * Whether people may hold personal API keys at all.
 *
 * Above the list of every key on the install, because that is what it governs:
 * turning it off does not only stop new keys being created, it stops every key
 * that already exists, immediately — and the list of what just stopped is the
 * thing an administrator should be looking at while deciding. It was a section
 * on Settings whose footnote linked here (ADR §50).
 *
 * The page keeps working while this is off. That is deliberate: switching it
 * off is often the first thing somebody reaches for during an incident, and the
 * keys still need finding afterwards.
 *
 * Beneath it, while it is on, the cap on how long a new key may last — one
 * field and one decision, so it saves on its own and shows its Save only once
 * edited, like the dormancy threshold. Hidden while keys are off: a lifetime
 * for keys nobody can create reads as a policy in force.
 */
export function PersonalKeysSwitch() {
  const queryClient = useQueryClient();
  const { data: settings, isLoading, error } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
  });

  /** Null means "not edited"; the stored value shows through until then. */
  const [draft, setDraft] = useState<string | null>(null);

  const toggle = useMutation({
    mutationFn: api.updatePlatformSettings,
    onSuccess: (result: { settings: PlatformSettings; warning: string }, vars) => {
      queryClient.setQueryData(["platform-settings"], (old: PlatformSettings | undefined) =>
        old ? { ...old, ...result.settings } : old,
      );
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      // Every key on this page changes state with it, and so does what the
      // account menu offers.
      queryClient.invalidateQueries({ queryKey: ["all-api-keys"] });
      queryClient.invalidateQueries({ queryKey: ["my-api-keys"] });
      if (result.warning) toast.warning(result.warning, { duration: 10_000 });
      else if (vars.api_key_max_lifetime_days !== undefined) toast.success("Saved");
      // What is now true, and for whom: off, the server stops authenticating
      // every existing key (`database.Settings.APIKeysEnabled`).
      else
        toast.success(
          vars.api_keys_enabled
            ? "Personal API keys are on — everyone can create them"
            : "Personal API keys are off — existing keys stop working",
        );
      if (vars.api_key_max_lifetime_days !== undefined) setDraft(null);
    },
    onError: (error: Error) => toast.error(error.message),
  });

  // Not nothing, and not a permanent skeleton: an inline line in the same slot,
  // so an administrator can tell a failed read from the capability being absent
  // — and so the id the settings index links to still resolves.
  if (error) {
    return (
      <div id="personal-keys" className="mb-4 scroll-mt-20">
        <Notice tone="attention" size="sm">
          Couldn’t read whether personal API keys are enabled. The keys below are unaffected;
          reload to try again.
        </Notice>
      </div>
    );
  }
  if (isLoading || !settings) {
    return <Skeleton id="personal-keys" className="mb-4 h-24 w-full rounded-lg" />;
  }

  const value = draft ?? String(settings.api_key_max_lifetime_days);
  const problem = fieldError("api_key_max_lifetime_days", value, () => value);
  const dirty = draft !== null && Number(value.trim()) !== settings.api_key_max_lifetime_days;
  const spec = NUMBER_FIELDS.api_key_max_lifetime_days;

  return (
    <TableCard id="personal-keys" className="mb-4 scroll-mt-20 divide-y divide-border/60 p-4">
      <SettingRow id="api_keys_enabled">
        <Switch
          id="api_keys_enabled"
          checked={settings.api_keys_enabled}
          disabled={toggle.isPending}
          onCheckedChange={(v) => toggle.mutate({ api_keys_enabled: v })}
        />
      </SettingRow>

      {settings.api_keys_enabled && (
        <SettingRow id="api_key_max_lifetime_days">
          <div className="space-y-1 sm:text-right">
            <div className="flex items-center gap-2">
              <Input
                id="api_key_max_lifetime_days"
                inputMode="numeric"
                className="w-24"
                value={value}
                disabled={toggle.isPending}
                aria-invalid={!!problem}
                aria-describedby={problem ? fieldErrorId("api_key_max_lifetime_days") : undefined}
                onChange={(e) => setDraft(e.target.value)}
              />
              <UnitLabel unit={spec.unit} />
              {(dirty || (toggle.isPending && draft !== null)) && (
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7 text-xs"
                  disabled={!!problem || toggle.isPending}
                  onClick={() =>
                    toggle.mutate({ api_key_max_lifetime_days: Number(value.trim()) })
                  }
                >
                  {toggle.isPending ? "Saving…" : "Save"}
                </Button>
              )}
            </div>
            {problem && (
              <FieldError field="api_key_max_lifetime_days" className="max-w-xs sm:ml-auto">
                {problem}
              </FieldError>
            )}
          </div>
        </SettingRow>
      )}
    </TableCard>
  );
}

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type PlatformSettings } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { UnitLabel } from "@/components/unit-label";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryError } from "@/components/query-error";
import { Section, SectionHeading } from "@/components/section";
import { SettingRow } from "@/components/setting-row";
import { FieldError } from "@/components/field-help";
import { NUMBER_FIELDS, fieldError, type NumberFieldKey } from "@/lib/settings-fields";
import { fieldErrorId } from "@/lib/utils";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/**
 * How long a session lasts, and how long it may sit unused.
 *
 * On Authentication rather than on Settings, for the reason the dormancy pair
 * is (ADR §50 D2): this is the page that shows what it governs — the providers
 * that authenticate people, and the session each sign-in produces. It sits
 * *above* Unused accounts because it is about the session and that one is about
 * the account, which is the order somebody reads them in.
 *
 * Each number has its own Save. They are two decisions rather than one form:
 * an administrator shortening the lifetime is told, in the server's own words,
 * that it signs people out — and a warning that belonged to a batch of edits
 * could not say which one caused it.
 */
export function SessionPolicyEditor() {
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
  const [drafts, setDrafts] = useState<Partial<Record<PolicyKey, string>>>({});

  const save = useMutation({
    mutationFn: api.updatePlatformSettings,
    onSuccess: (
      result: { settings: PlatformSettings; warning: string },
      saved: Parameters<typeof api.updatePlatformSettings>[0],
    ) => {
      queryClient.setQueryData(["platform-settings"], (old: PlatformSettings | undefined) =>
        old ? { ...old, ...result.settings } : old,
      );
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      // A warning is not a failure — it is the server saying whose sessions
      // this just ended. Long, because it is the one thing worth reading here.
      if (result.warning) toast.warning(result.warning, { duration: 12_000 });
      else toast.success("Saved");
      // Only the field that went out. These are two decisions with two Save
      // buttons, so clearing the whole map threw away the other field's typed
      // value — silently, and at the moment the person was told something had
      // been saved.
      setDrafts((d) => {
        const next = { ...d };
        for (const key of Object.keys(saved)) delete next[key as PolicyKey];
        return next;
      });
    },
    onError: (error: Error) => toast.error(error.message),
  });

  const heading = (
    <SectionHeading
      title="Sessions"
      description="Your identity provider can’t end a Toft session, so its length is set here."
    />
  );

  if (readError) {
    return (
      <section id="sessions" className="scroll-mt-20 space-y-3">
        {heading}
        <QueryError
          title="The session policy could not be read"
          error={readError}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  if (isLoading || !settings) {
    return (
      <section id="sessions" className="scroll-mt-20 space-y-3">
        {heading}
        <Skeleton className="h-32 w-full rounded-lg" />
      </section>
    );
  }

  return (
    // A titled card (`Section`), like every other single-card region on the
    // page — the heading above a bare card was the second grammar.
    <Section id="sessions" title="Sessions" description="Your identity provider can’t end a Toft session, so its length is set here.">
      <div className="divide-y divide-border/60 p-4">
        <PolicyField
          id="session_absolute_hours"
          stored={settings.session_absolute_hours}
          draft={drafts.session_absolute_hours}
          onChange={(v) => setDrafts((d) => ({ ...d, session_absolute_hours: v }))}
          onSave={(n) => save.mutate({ session_absolute_hours: n })}
          // Its own save only: the other field keeps its input and its label.
          pending={save.isPending && "session_absolute_hours" in (save.variables ?? {})}
        />
        <PolicyField
          id="session_idle_minutes"
          stored={settings.session_idle_minutes}
          draft={drafts.session_idle_minutes}
          onChange={(v) => setDrafts((d) => ({ ...d, session_idle_minutes: v }))}
          onSave={(n) => save.mutate({ session_idle_minutes: n })}
          pending={save.isPending && "session_idle_minutes" in (save.variables ?? {})}
          note={
            settings.session_idle_minutes === 0
              ? "Idle timeout is off: a session lasts its full lifetime, used or not."
              : undefined
          }
        />
      </div>
    </Section>
  );
}

type PolicyKey = Extract<NumberFieldKey, "session_absolute_hours" | "session_idle_minutes">;

/** One number, its unit, and its own Save. */
function PolicyField({
  id,
  stored,
  draft,
  onChange,
  onSave,
  pending,
  note,
}: {
  id: PolicyKey;
  stored: number;
  draft?: string;
  onChange: (value: string) => void;
  onSave: (value: number) => void;
  pending: boolean;
  note?: string;
}) {
  const value = draft ?? String(stored);
  // No cross-field rule here, so the other draft is never consulted — the
  // signature wants a lookup and this is the honest one to hand it.
  const error = fieldError(id, value, () => value);
  const dirty = draft !== undefined && Number(value.trim()) !== stored;
  const spec = NUMBER_FIELDS[id];

  return (
    <SettingRow id={id} note={note}>
      <div className="space-y-1 sm:text-right">
        <div className="flex items-center gap-2">
          <Input
            id={id}
            inputMode="numeric"
            className="w-24"
            value={value}
            disabled={pending}
            aria-invalid={!!error}
            aria-describedby={error ? fieldErrorId(id) : undefined}
            onChange={(e) => onChange(e.target.value)}
          />
          <UnitLabel unit={spec.unit} />
          {/* Only once there is something to save: an idle Save beside every
              number was a column of disabled buttons. */}
          {(dirty || pending) && (
            <Button
              size="sm"
              variant="outline"
              className="h-7 text-xs"
              disabled={!!error || pending}
              onClick={() => onSave(Number(value.trim()))}
            >
              {pending ? "Saving…" : "Save"}
            </Button>
          )}
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

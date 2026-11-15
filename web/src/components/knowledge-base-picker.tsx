import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ChevronDown, ExternalLink, LockKeyhole } from "lucide-react";

import { api } from "@/lib/api";
import { appUrl } from "@/lib/app-url";
import { useAuth } from "@/hooks/use-auth";
import { Button } from "@/components/ui/button";
import { Section } from "@/components/section";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Command, CommandEmpty, CommandInput, CommandItem, CommandList,
} from "@/components/ui/command";
import { Badge } from "@/components/ui/badge";
import { StatusBadge } from "@/components/status-badge";
import { Notice } from "@/components/notice";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryError } from "@/components/query-error";
import { Textarea } from "@/components/ui/textarea";
import { FieldHelp } from "@/components/field-help";
import { FieldRow } from "@/components/field-row";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import {
  KNOWLEDGE_BASE_DEFAULT_DESCRIPTION, KNOWLEDGE_BASE_DESCRIPTION_MAX,
} from "@/lib/knowledge-base";
import { toneSurface } from "@/lib/tone";

/**
 * Admin → Messages → Knowledge base: the one app this install points everybody
 * at, from a word in the header rather than from the apps list.
 *
 * **The warning is the point of this component, not the picker.** `Authorize`
 * hands a system_admin an owner-shaped answer on every app, so the person
 * choosing is the one person who cannot tell by looking whether anybody else can
 * open what they chose — a still-private handbook resolves perfectly for them
 * and for nobody. Everything else here is a select and a text field; the
 * visibility notice is what stops "I picked it and it works" from being the
 * last thing that happens.
 *
 * **Nothing here reads the app's framework.** No filter, no grouping, no
 * heading, no copy preferring one kind: `static` is what the detector concluded,
 * not what an app is for, and the tools an organization actually writes a
 * handbook with are mostly not it. What decides whether an app may be picked is
 * what already decides whether anybody may read it.
 */

const MAX_LABEL = 32;

export function KnowledgeBasePicker() {
  const { user } = useAuth();
  const queryClient = useQueryClient();

  const [open, setOpen] = useState(false);
  /** null means "no edit in progress" — the stored pick is what is shown. */
  const [draftAppId, setDraftAppId] = useState<string | null>(null);
  const [draftLabel, setDraftLabel] = useState<string | null>(null);
  const [draftDescription, setDraftDescription] = useState<string | null>(null);

  const isAdmin = user?.system_role === "system_admin";

  const pickQuery = useQuery({
    queryKey: ["knowledge-base", "admin"],
    queryFn: api.getAdminKnowledgeBase,
    enabled: isAdmin,
  });
  // The apps list an administrator already has is the whole estate, so the
  // picker needs no request of its own beyond the one this page shares with
  // every other page that lists apps.
  const appsQuery = useQuery({ queryKey: ["apps"], queryFn: () => api.listApps(), enabled: isAdmin });

  const save = useMutation({
    mutationFn: (data: { app_id: string; label: string; description: string }) =>
      api.setKnowledgeBase(data),
    onSuccess: (_data, vars) => {
      setDraftAppId(null);
      setDraftLabel(null);
      setDraftDescription(null);
      // The header reads the resolving key on every route; without this the
      // administrator sets it and sees nothing until a reload.
      queryClient.invalidateQueries({ queryKey: ["knowledge-base"] });
      const app = apps.find((a) => a.id === vars.app_id);
      toast.success(`Everyone who can open ${app?.name ?? "this app"} now has “${vars.label}” in the header`);
    },
    onError: (e) => toast.error(e instanceof Error ? e.message : "Couldn’t save the knowledge base"),
  });

  const clear = useMutation({
    mutationFn: () => api.clearKnowledgeBase(),
    onSuccess: () => {
      setDraftAppId(null);
      setDraftLabel(null);
      setDraftDescription(null);
      queryClient.invalidateQueries({ queryKey: ["knowledge-base"] });
      toast.success("Knowledge base cleared");
    },
    onError: (e) => toast.error(e instanceof Error ? e.message : "Couldn’t clear the knowledge base"),
  });

  if (!isAdmin) return null;

  const apps = appsQuery.data ?? [];
  const stored = pickQuery.data;

  const appId = draftAppId ?? stored?.app_id ?? "";
  const label = draftLabel ?? stored?.label ?? "";
  const description = draftDescription ?? stored?.description ?? "";
  const chosen = apps.find((a) => a.id === appId);

  // Stored, but naming an app that is not in the estate. The settings table has
  // no foreign key, so this is what a deleted app leaves behind — and it is why
  // this card reads what is stored rather than what resolves.
  const dangling = !!stored?.app_id && appsQuery.isSuccess && !apps.some((a) => a.id === stored.app_id);

  const isSet = !!stored?.app_id;
  const dirty = (draftAppId !== null && draftAppId !== (stored?.app_id ?? "")) ||
    (draftLabel !== null && draftLabel !== (stored?.label ?? "")) ||
    (draftDescription !== null && draftDescription !== (stored?.description ?? ""));
  const busy = save.isPending || clear.isPending;

  const pickApp = (id: string) => {
    setOpen(false);
    setDraftAppId(id);
    // Prefilled from the app's name, then stored explicitly — so renaming the
    // app later does not silently rename a word in everybody's chrome.
    if (draftLabel === null || draftLabel === (stored?.label ?? "")) {
      const app = apps.find((a) => a.id === id);
      setDraftLabel((app?.name ?? "").slice(0, MAX_LABEL));
    }
  };

  return (
    <Section
      title="Knowledge base"
      description="One app, such as a handbook, linked from the header on every page."
      actions={
        isSet && !dangling ? (
          <Badge variant="outline" className={toneSurface.ok}>Active</Badge>
        ) : undefined
      }
    >
      <div className="space-y-4 p-4">
        {pickQuery.isError ? (
          <QueryError
            title="The knowledge base setting could not be read"
            error={pickQuery.error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void pickQuery.refetch()}
          />
        ) : pickQuery.isLoading || appsQuery.isLoading ? (
          <Skeleton className="h-24 w-full rounded-lg" />
        ) : (
          <>
            {/* The two single-line fields are setting rows (`FieldRow`); the
                description is prose and keeps the full width under its label. */}
            <FieldRow label="App" htmlFor="knowledge-base-app">
              <Popover open={open} onOpenChange={setOpen}>
                <PopoverTrigger
                  render={
                    <Button
                      id="knowledge-base-app"
                      variant="outline"
                      size="sm"
                      disabled={busy}
                      // Named here rather than by its children: what they say
                      // depends on whether an app is chosen, and one of the
                      // three renderings is a bare id.
                      aria-label="Knowledge base app"
                      className="w-full justify-between font-normal"
                    />
                  }
                >
                  {chosen ? (
                    <span className="truncate">{chosen.name}</span>
                  ) : dangling ? (
                    <span className="truncate font-mono text-xs">{stored?.app_id}</span>
                  ) : (
                    <span className="text-muted-foreground">Choose an app…</span>
                  )}
                  <ChevronDown className="h-3.5 w-3.5 shrink-0 opacity-50" aria-hidden />
                </PopoverTrigger>
                {/* PopoverContent ships its own padding, which fights cmdk. */}
                <PopoverContent align="start" className="w-[--anchor-width] min-w-72 p-0">
                  <Command>
                    <CommandInput placeholder="Search apps…" />
                    <CommandList>
                      <CommandEmpty>No app by that name.</CommandEmpty>
                      {/* One flat list. No grouping and no framework column —
                          the app's type is not an input here. */}
                      {apps.map((a) => (
                        <CommandItem
                          key={a.id}
                          value={`${a.name} ${a.slug}`}
                          onSelect={() => pickApp(a.id)}
                        >
                          <span className="flex-1 truncate">{a.name}</span>
                          <span className="font-mono text-2xs text-muted-foreground">{a.slug}</span>
                        </CommandItem>
                      ))}
                    </CommandList>
                  </Command>
                </PopoverContent>
              </Popover>
            </FieldRow>

            <FieldRow
              label="Label"
              htmlFor="knowledge-base-label"
              help="The word shown in the header. Keep it to one or two words."
            >
              <Input
                id="knowledge-base-label"
                value={label}
                disabled={busy}
                maxLength={MAX_LABEL}
                onChange={(e) => setDraftLabel(e.target.value)}
                placeholder="Knowledge base"
              />
            </FieldRow>

            {/* The sentence under the label on the Docs page, in the
                organization's words rather than the product's. Optional, and
                the placeholder is the default it replaces — so the person
                deciding whether to write one can read what is already there
                instead of having to go and look. */}
            <div className="space-y-2">
              <Label htmlFor="knowledge-base-description">Description</Label>
              <Textarea
                id="knowledge-base-description"
                value={description}
                disabled={busy}
                maxLength={KNOWLEDGE_BASE_DESCRIPTION_MAX}
                rows={2}
                onChange={(e) => setDraftDescription(e.target.value)}
                placeholder={KNOWLEDGE_BASE_DEFAULT_DESCRIPTION}
              />
              <FieldHelp>The line under the label on the Docs page. Blank shows the default.</FieldHelp>
            </div>

            {dangling && (
              <Notice
                tone="attention"
                title="This points at an app that no longer exists"
                action={
                  <Button size="sm" variant="outline" disabled={busy} onClick={() => clear.mutate()}>
                    Clear
                  </Button>
                }
              >
                The header shows no link until you choose another app or clear this.
              </Notice>
            )}

            {/* The warning this component exists for. An administrator reads
                every app, so a still-private handbook works perfectly for them
                and for nobody else. */}
            {chosen && chosen.visibility === "private" && (
              <Notice tone="attention" title="Only you and this app’s collaborators can open this">
                Set {chosen.name} to <strong>Anyone signed in</strong> on its Access tab so everyone
                can open it.
              </Notice>
            )}

            {chosen && (
              <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                <StatusBadge status={chosen.status} />
                {/* The apps list's Locked badge, not a second rendering of it. */}
                {chosen.locked_at && (
                  <Badge variant="outline" className={`gap-1 ${toneSurface.attention}`}>
                    <LockKeyhole className="h-3 w-3" aria-hidden />
                    Locked
                  </Badge>
                )}
                <a
                  href={appUrl(chosen.slug)}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-center gap-1 font-mono transition-colors hover:text-foreground"
                >
                  /apps/{chosen.slug}
                  <ExternalLink className="h-3 w-3" aria-hidden />
                </a>
              </div>
            )}

            <div className="flex items-center gap-2 pt-1">
              <Button
                size="sm"
                disabled={busy || !appId || !label.trim() || (isSet && !dirty)}
                onClick={() =>
                  save.mutate({
                    app_id: appId,
                    label: label.trim(),
                    description: description.trim(),
                  })
                }
              >
                {save.isPending ? "Saving…" : isSet ? "Save" : "Set knowledge base"}
              </Button>
              {isSet && !dangling && (
                <Button size="sm" variant="outline" disabled={busy} onClick={() => clear.mutate()}>
                  {clear.isPending ? "Clearing…" : "Clear"}
                </Button>
              )}
            </div>
          </>
        )}
      </div>
    </Section>
  );
}

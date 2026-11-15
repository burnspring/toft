import { useState } from "react";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, X } from "lucide-react";
import { toast } from "sonner";

import {
  api,
  type AllowedGitSource,
  type GitSourceMode,
  type SourcePolicyInput,
} from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Section } from "@/components/section";
import { Input } from "@/components/ui/input";
import { FieldLabel, Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { FieldHelp } from "@/components/field-help";

/**
 * Admin → Sources.
 *
 * Which git repositories this install will deploy from at all. Four things live
 * here and nothing else does: the mode, the pattern list, the uploads switch,
 * and the impact preview that has to be seen before anything is saved.
 *
 * It saves as a section rather than through the page's pending-changes bar,
 * because the four are one decision: a mode and a list that only mean something
 * together, and an impact figure that is only true of the pair. The preview is
 * that model's version of a confirmation — an inline diff above this section's
 * own button, rather than a dialog interrupting a page-wide save.
 *
 * The copy has one job it must not fail. This constrains where an app's code
 * comes from — its *provenance* — and nothing about what the code then does. A
 * build still runs arbitrary code out of an allowed repository and still
 * installs from public registries. Every sentence here has to survive that
 * distinction, because a control that is oversold is worse than none: it
 * produces confidence rather than safety.
 */
export function SourcePolicyEditor() {
  const queryClient = useQueryClient();
  const { data: stored, isLoading, error, refetch } = useQuery({
    queryKey: ["source-policy"],
    queryFn: api.getSourcePolicy,
  });

  // Null means "not edited yet" — the stored value shows through until then.
  // This card is always mounted, so it cannot seed its state from a `key`.
  const [draft, setDraft] = useState<Draft | null>(null);
  const [entry, setEntry] = useState("");
  const [saving, setSaving] = useState(false);

  const current: Draft = draft ?? {
    mode: stored?.mode ?? "any",
    uploads: stored?.uploads_enabled ?? true,
    patterns: (stored?.patterns ?? []).map((p) => ({ pattern: p.pattern, note: p.note ?? "" })),
  };
  const dirty = draft !== null && stored !== undefined && changed(current, stored);

  // The impact of what is currently typed. Keyed on every field that can
  // *change the answer*, so it re-runs on every such edit and what is on screen
  // is always about the policy the Save button would store — never about the
  // one before the last keystroke.
  //
  // A pattern's `note` is deliberately not in the key. It is an annotation and
  // cannot change which apps comply, and keying on it posted a preview per
  // character while somebody typed a sentence — with `seen` false and Save
  // disabled on `!seen`, so the primary button flickered the whole time. A
  // debounce would only move that flicker; the key is the cause. If the server
  // ever begins validating notes, this has to widen again.
  const previewShape = JSON.stringify({
    mode: current.mode,
    uploads: current.uploads,
    patterns: current.patterns.map((p) => p.pattern),
  });
  const { data: preview, isFetching: previewing } = useQuery({
    queryKey: ["source-policy-preview", previewShape],
    queryFn: () => api.previewSourcePolicy(toInput(current)),
    enabled: dirty,
  });

  const { data: observed } = useQuery({
    queryKey: ["source-policy-observed"],
    queryFn: api.getObservedSources,
  });

  // A skeleton reads as "still working", so a failed read left this editor as a
  // grey bar for ever with nothing to press.
  if (error) {
    return (
      <QueryError
        title="The source policy could not be read"
        error={error}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={() => void refetch()}
      />
    );
  }
  if (isLoading || !stored) {
    return <Skeleton className="h-64 w-full rounded-lg" />;
  }

  const usage = new Map(stored.patterns.map((p) => [p.pattern, p]));
  const invalid = preview?.invalid ?? [];
  // Nothing is saved until the impact of *this* draft has come back. On an
  // install with forty apps, saving blind is how the feature gets turned on
  // once and abandoned.
  const seen = !dirty || (preview !== undefined && !previewing);

  function edit(next: Partial<Draft>) {
    setDraft({ ...current, ...next });
  }

  function addPattern(raw: string) {
    const pattern = raw.trim();
    if (!pattern) return;
    if (current.patterns.some((p) => p.pattern.toLowerCase() === pattern.toLowerCase())) {
      // Said, not swallowed. Clearing the box and adding nothing looks
      // identical to adding it, on a list that decides where this install may
      // deploy from — so somebody would go away believing they had widened it.
      toast.info(`${pattern} is already on the list.`);
      setEntry("");
      return;
    }
    edit({ patterns: [...current.patterns, { pattern, note: "" }] });
    setEntry("");
  }

  async function save() {
    setSaving(true);
    try {
      const result = await api.updateSourcePolicy(toInput(current));
      setDraft(null);
      queryClient.setQueryData(["source-policy"], result.policy);
      queryClient.invalidateQueries({ queryKey: ["source-policy"] });
      // Every creation surface reads this: New App renders a different form
      // under `none`, and the gallery hides its Deploy buttons entirely.
      queryClient.invalidateQueries({ queryKey: ["source-policy-effective"] });
      queryClient.invalidateQueries({ queryKey: ["gallery"] });
      if (result.warning) toast.warning(result.warning, { duration: 12_000 });
      else toast.success("Saved");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Couldn’t save the source policy");
    } finally {
      setSaving(false);
    }
  }

  return (
    // Not a second copy of the page's own subtitle, which already says which
    // repositories this install deploys from and that the policy constrains
    // provenance rather than behaviour. What is left is the half the subtitle
    // has no room for: what an approved repository is still free to do.
    <Section
      id="sources"
      title="Source policy"
      description="This controls where code comes from, not what it does once it runs."
    >
      <div className="space-y-6 p-4">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between sm:gap-8">
          <div className="min-w-0 space-y-1">
            <FieldLabel className="text-sm font-medium">Where apps may come from</FieldLabel>
            <FieldHelp>
              {MODE_HINT[current.mode]}
            </FieldHelp>
          </div>
          {/* Not `ChoiceButtons`, which saves the moment one is chosen —
              this mode is part of a draft that a preview has to be run
              against before it goes. What it borrows is the rendering, down
              to `aria-pressed`. */}
          <div role="group" aria-label="Source policy mode" className="flex shrink-0 flex-wrap gap-1.5">
            {MODES.map((m) => (
              <Button
                key={m.value}
                size="sm"
                variant={m.value === current.mode ? "default" : "outline"}
                aria-pressed={m.value === current.mode}
                className="h-7 text-xs"
                disabled={saving}
                onClick={() => m.value !== current.mode && edit({ mode: m.value })}
              >
                {m.label}
              </Button>
            ))}
          </div>
        </div>

        {current.mode === "allowlist" && (
          <div className="space-y-3 border-t border-border/60 pt-5">
            <div className="space-y-1">
              <FieldLabel className="text-sm font-medium">Approved sources</FieldLabel>
              <FieldHelp>
                A host and optional path. A trailing <code className="font-mono">{"/*"}</code> matches
                everything beneath it; without one, the entry is one repository.
              </FieldHelp>
            </div>

            {current.patterns.length === 0 ? (
              <Notice>
                The list is empty, so nothing can deploy from git. Add a pattern, or choose{" "}
                <span className="font-medium">No git</span>.
              </Notice>
            ) : (
              <ul className="divide-y divide-border/60 rounded-md border border-border/60">
                {current.patterns.map((p) => (
                  <PatternRow
                    key={p.pattern}
                    pattern={p}
                    apps={usage.get(p.pattern)?.apps}
                    invalid={invalid.find((i) => i.pattern === p.pattern)?.error}
                    disabled={saving}
                    onNote={(note) =>
                      edit({
                        patterns: current.patterns.map((x) =>
                          x.pattern === p.pattern ? { ...x, note } : x,
                        ),
                      })
                    }
                    onRemove={() =>
                      edit({ patterns: current.patterns.filter((x) => x.pattern !== p.pattern) })
                    }
                  />
                ))}
              </ul>
            )}

            <form
              className="flex flex-wrap items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                addPattern(entry);
              }}
            >
              <Input
                value={entry}
                onChange={(e) => setEntry(e.target.value)}
                placeholder="github.com/acme/*"
                aria-label="Pattern to add"
                className="h-8 w-72 font-mono text-xs"
                disabled={saving}
              />
              <Button type="submit" size="sm" variant="outline" className="h-8" disabled={saving || !entry.trim()}>
                <Plus className="mr-1 h-3.5 w-3.5" />
                Add
              </Button>
            </form>

            <Suggestions
              sources={observed?.sources ?? []}
              already={current.patterns.map((p) => p.pattern)}
              onAdd={addPattern}
            />
          </div>
        )}

        <div className="flex flex-col gap-3 border-t border-border/60 pt-5 sm:flex-row sm:items-start sm:justify-between sm:gap-8">
          <div className="min-w-0 space-y-1">
            <Label htmlFor="source_uploads_enabled" className="text-sm font-medium">
              Archive uploads
            </Label>
            <FieldHelp>
              Allow creating an app from an uploaded archive.
            </FieldHelp>
            {current.mode !== "any" && current.uploads && (
              <FieldHelp tone="attention">
                Uploads are still on, so a refused developer can upload the same code as an
                archive.
              </FieldHelp>
            )}
            {current.mode === "none" && !current.uploads && (
              <FieldHelp tone="attention">
                No source is allowed. Nobody can create an app until one is turned back on.
              </FieldHelp>
            )}
          </div>
          <Switch
            id="source_uploads_enabled"
            checked={current.uploads}
            disabled={saving}
            onCheckedChange={(v) => edit({ uploads: v })}
          />
        </div>

        {stored.non_compliant > 0 && !dirty && (
          <FieldHelp tone="attention" className="border-t border-border/60 pt-5">
            {stored.non_compliant} app{stored.non_compliant === 1 ? "" : "s"} already deploy
            {stored.non_compliant === 1 ? "s" : ""} from a source this policy doesn’t allow.{" "}
            {stored.non_compliant === 1 ? "It keeps" : "They keep"} running and rollback works, but
            new deploys stop.
          </FieldHelp>
        )}

        {dirty && (
          <Notice tone="neutral" title="Before you save">
            <div className="space-y-3">
            {previewing || !preview ? (
              <Skeleton className="h-10 w-full" />
            ) : (
              <>
                <p className="text-sm text-muted-foreground">
                  {preview.count === 0
                    ? "No app on this install would stop deploying."
                    : `${preview.count} app${preview.count === 1 ? "" : "s"} would stop deploying. ${
                        preview.count === 1 ? "It keeps" : "They keep"
                      } running and serving, and rollback still works.`}
                </p>
                {preview.count > 0 && (
                  <ul className="space-y-1 text-xs">
                    {(preview.non_compliant ?? []).slice(0, 12).map((a) => (
                      <li key={a.app_id} className="flex flex-wrap items-baseline gap-x-2">
                        <span className="font-medium">{a.name}</span>
                        <span className="text-muted-foreground">
                          {a.owner_name ? `· ${a.owner_name}` : ""}
                        </span>
                        <span className="font-mono text-2xs text-muted-foreground">
                          {a.repo_url}
                        </span>
                      </li>
                    ))}
                    {(preview.non_compliant ?? []).length > 12 && (
                      <li className="text-muted-foreground">
                        …and {(preview.non_compliant ?? []).length - 12} more
                      </li>
                    )}
                  </ul>
                )}
                {(preview.canonicalized ?? []).length > 0 && (
                  <div className="text-xs text-muted-foreground">
                    Will be stored as:{" "}
                    {(preview.canonicalized ?? []).map((c, i) => (
                      <span key={c.typed}>
                        {i > 0 && ", "}
                        <code className="font-mono">{c.typed}</code> →{" "}
                        <code className="font-mono">{c.stored}</code>
                      </span>
                    ))}
                  </div>
                )}
                {invalid.length > 0 && (
                  <ul className="space-y-1 text-xs text-destructive">
                    {invalid.map((i) => (
                      <li key={i.pattern}>
                        <code className="font-mono">{i.pattern}</code> — {i.error}
                      </li>
                    ))}
                  </ul>
                )}
              </>
            )}
            </div>
          </Notice>
        )}

        <div className="flex flex-wrap items-center gap-3 border-t border-border/60 pt-5">
          <Button size="sm" disabled={!dirty || !seen || invalid.length > 0 || saving} onClick={save}>
            {saving ? "Saving…" : "Save"}
          </Button>
          {dirty && (
            <Button size="sm" variant="ghost" disabled={saving} onClick={() => setDraft(null)}>
              Discard
            </Button>
          )}
        </div>
      </div>
    </Section>
  );
}

interface DraftPattern {
  pattern: string;
  note: string;
}

interface Draft {
  mode: GitSourceMode;
  uploads: boolean;
  patterns: DraftPattern[];
}

const MODES: { value: GitSourceMode; label: string }[] = [
  { value: "any", label: "Any repository" },
  { value: "allowlist", label: "Approved sources only" },
  { value: "none", label: "No git" },
];

const MODE_HINT: Record<GitSourceMode, string> = {
  any: "Any HTTPS repository (the default).",
  allowlist: "Only repositories matching a pattern below. Anyone refused sees which sources work.",
  none: "No remote git. New app offers an upload instead, if uploads are on.",
};

function toInput(d: Draft): SourcePolicyInput {
  return {
    mode: d.mode,
    uploads_enabled: d.uploads,
    patterns: d.patterns.map((p) => ({ pattern: p.pattern, note: p.note })),
  };
}

function changed(d: Draft, stored: { mode: GitSourceMode; uploads_enabled: boolean; patterns: AllowedGitSource[] }) {
  if (d.mode !== stored.mode || d.uploads !== stored.uploads_enabled) return true;
  if (d.patterns.length !== stored.patterns.length) return true;
  return d.patterns.some((p, i) => p.pattern !== stored.patterns[i].pattern || p.note !== (stored.patterns[i].note ?? ""));
}

/** One entry: the pattern, why it is there, and how many apps it covers — which
 *  is what makes a stale entry findable rather than permanent. */
function PatternRow({
  pattern,
  apps,
  invalid,
  disabled,
  onNote,
  onRemove,
}: {
  pattern: DraftPattern;
  apps?: number;
  invalid?: string;
  disabled: boolean;
  onNote: (note: string) => void;
  onRemove: () => void;
}) {
  return (
    <li className="flex flex-wrap items-center gap-2 px-3 py-2">
      <code className="font-mono text-xs">{pattern.pattern}</code>
      {apps !== undefined && (
        <Badge variant="secondary" className="text-2xs">
          {apps} app{apps === 1 ? "" : "s"}
        </Badge>
      )}
      {invalid && <span className="text-xs text-destructive">{invalid}</span>}
      <Input
        value={pattern.note}
        onChange={(e) => onNote(e.target.value)}
        placeholder="Why this is here"
        aria-label={`Note for ${pattern.pattern}`}
        className="ml-auto h-7 w-56 text-xs"
        disabled={disabled}
      />
      <Button
        size="icon"
        variant="ghost"
        className="h-7 w-7"
        aria-label={`Remove ${pattern.pattern}`}
        disabled={disabled}
        onClick={onRemove}
      >
        <X className="h-3.5 w-3.5" />
      </Button>
    </li>
  );
}

/**
 * What this install already deploys from, at three widths, so a starting list
 * is a few clicks rather than transcription from forty app pages.
 *
 * The widths are offered rather than chosen for the administrator: a whole host
 * is right for an intranet GitLab that serves nothing else, an organization for
 * a shared github.com, and one repository for the third-party tool everybody
 * uses. The platform has no way to know which.
 */
function Suggestions({
  sources,
  already,
  onAdd,
}: {
  sources: { pattern: string; kind: string; apps: number }[];
  already: string[];
  onAdd: (pattern: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const taken = new Set(already.map((p) => p.toLowerCase()));
  const offer = sources.filter((s) => !taken.has(s.pattern.toLowerCase())).slice(0, 24);
  if (offer.length === 0) return null;

  return (
    <div className="pt-1">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className="rounded-sm text-xs font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
      >
        {open ? "Hide" : "Add from what apps already use"}
      </button>
      {open && (
        <div className="mt-2 flex flex-wrap gap-1.5">
          {offer.map((s) => (
            <Button
              key={s.pattern}
              size="sm"
              variant="outline"
              className="h-7 font-mono text-2xs"
              onClick={() => onAdd(s.pattern)}
            >
              {s.pattern}
              <span className="ml-1.5 font-sans text-muted-foreground">
                {s.apps} app{s.apps === 1 ? "" : "s"}
              </span>
            </Button>
          ))}
        </div>
      )}
    </div>
  );
}

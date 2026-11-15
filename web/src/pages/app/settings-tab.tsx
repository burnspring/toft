import { toneText, toneBorder } from "@/lib/tone";
import { isRerenderable } from "@/lib/rerender";
import { isBatchFramework } from "@/lib/frameworks";
import { useContext, useState } from "react";
import { useAppLifecycle } from "@/pages/app/lifecycle";
import { useSectionSpy } from "@/hooks/use-section-spy";
import { Code } from "@/components/code";
import { copyToClipboard } from "@/lib/clipboard";
import { Link, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";

import { Input } from "@/components/ui/input";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";


import { Notice } from "@/components/notice";
import { Section, SectionHeading } from "@/components/section";
import { FIELD_CONTROL, FIELD_ROW, FieldRow } from "@/components/field-row";
import { NotebookRenderSchedule } from "@/components/notebook-render-schedule";
import { RestartScheduleBlock } from "@/components/restart-schedule";
import { TableCard } from "@/components/table-card";

import { EmptyState } from "@/components/empty-state";

import { RelativeTime } from "@/components/relative-time";
import { SearchInput } from "@/components/search-input";

import { TagEditor } from "@/components/tag-editor";
import { DeleteAppDialog } from "@/components/delete-app-dialog";

import { TransferOwnershipDialog } from "@/components/transfer-ownership-dialog";
import { AppOwner } from "@/components/app-owner";
import { formatBytes, formatMB } from "@/lib/bytes";
import { enforcementPhrase, throttledHour } from "@/lib/enforcement";
import { formatMillicores } from "@/lib/time";

import { Trash2, Eye, EyeOff, Plus, X, Users, Lock, LockOpen, Copy, Search } from "lucide-react";

import { cn, fieldErrorId } from "@/lib/utils";

/** The bordered panel the repository editor opens into — a transient editor,
 *  not a setting, which is why it is the one box left on this tab. */
const BLOCK = "rounded-lg border border-border/60 p-3";
/**
 * A section's settings as rows under hairlines — the same rhythm as
 * Admin → Settings (`SettingRow` is `py-4 first:pt-0 last:pb-0` under a
 * `divide-y`). Each setting used to be its own bordered box, which made a
 * section of six read as six cards rather than as one form. Applied to the
 * direct children, so a block that renders nothing leaves no rule behind.
 */
const ROWS =
  "divide-y divide-border/60 [&>*]:py-4 [&>*:first-child]:pt-0 [&>*:last-child]:pb-0";
import { useAuth } from "@/hooks/use-auth";
import { api, isGitSource } from "@/lib/api";
import { envChanges, envSnapshotGrown, hiddenRenames, type EnvFormRow } from "@/lib/env-diff";
import { ModeBadge } from "@/components/mode-badge";
import { executionModeDescription } from "@/lib/execution-mode";
import { runtimeStateExplanation } from "@/lib/runtime-series";
import type {
  App,
  AppEnvVar,
  UpdateAppEnvRequest,
  AppPolicy,
  AppRuntime,
  AppTag,
  AppPermissions,
  DeveloperGitConnection,
  ExecutionMode,
} from "@/lib/api";

import { OtherTabsMatch } from "@/components/other-tabs-match";

import { PendingBar } from "@/components/pending-bar";
import { SectionRail } from "@/components/section-rail";
import { FieldError, FieldHelp } from "@/components/field-help";
import {
  APP_FIELD_CONTROL,
  APP_FIELD_KEYS,
  appFieldError,
  appFieldNote,
  type AppFieldKey,
} from "@/lib/app-settings-fields";

import { VisibleSections } from "@/lib/app-settings-filter";
import { appSectionsOn, appSettingsIn, settingText, type AppSettingsSection } from "@/lib/settings-index";
import { ConfirmPhrase } from "@/components/confirm-phrase";

// ── Settings Tab ──────────────────────────────────────────────

interface EnvRow {
  /**
   * This row's identity for as long as the form is open.
   *
   * Not the key, because the key is the thing being edited and a half-typed one
   * is not unique; not the position, because rows are removed. Row state that
   * was keyed by position — which values had been made visible — followed the
   * *position* when a row above was deleted, so removing one row could reveal
   * the secret somebody had just typed into another.
   */
  id: string;
  key: string;
  value: string;
  sensitive: boolean;
  /** Whether this key exists server-side — a stored sensitive row keeps its value on a blank save. */
  stored: boolean;
  /** A stored sensitive row that holds nothing — its placeholder says “not set”. */
  unset?: boolean;
  /** The stored key this row was seeded from; absent for an added row. */
  seededKey?: string;
}

let envRowSeq = 0;
const nextEnvRowId = () => `env-${++envRowSeq}`;

function envRowsFrom(serverEnvVars: AppEnvVar[]): EnvRow[] {
  return serverEnvVars.map((v) => ({
    id: nextEnvRowId(),
    key: v.key,
    value: v.value ?? "",
    sensitive: v.sensitive,
    stored: true,
    unset: v.unset,
    seededKey: v.key,
  }));
}

/**
 * Pointing an app at a different repository. Administrators only.
 *
 * It exists for one situation and the copy says so: an install that restricts
 * where apps may come from has apps that no longer comply, and without this the
 * only way to move one onto an approved mirror would be to delete it and
 * recreate it — losing its environment variables, storage, schedules, grants
 * and history.
 *
 * Collapsed by default. This is not an edit an owner makes casually, and an
 * always-open field beside the branch box would read like one.
 */
function RepointControl({ appId, app }: { appId: string; app: App }) {
  const { user } = useAuth();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [url, setUrl] = useState(app.repo_url);
  const [saving, setSaving] = useState(false);

  if (user?.system_role !== "system_admin") return null;

  async function save() {
    setSaving(true);
    try {
      const result = (await api.updateApp(appId, { repo_url: url.trim() })) as unknown as {
        message?: string;
      };
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      setOpen(false);
      // The server's note carries what the caller did not ask for: the running
      // release keeps serving, and the stored token is probably wrong for the
      // new host.
      toast.success(result.message ?? "This app now deploys from a different repository", {
        duration: 12_000,
      });
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Couldn’t change the repository");
    } finally {
      setSaving(false);
    }
  }

  if (!open) {
    return (
      <Button
        size="sm"
        variant="ghost"
        className="h-7 px-0 text-xs text-muted-foreground hover:bg-transparent hover:text-foreground"
        onClick={() => setOpen(true)}
      >
        Change repository
      </Button>
    );
  }

  return (
    <div className={cn(BLOCK, "space-y-2")}>
      <Label htmlFor="repo-url-change" className="text-xs font-medium">
        Deploy this app from
      </Label>
      <div className="flex items-center gap-2">
        <Input
          id="repo-url-change"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          className="flex-1 font-mono text-xs"
          placeholder="https://gitlab.internal/dept/app"
        />
        <Button size="sm" disabled={saving || !url.trim() || url.trim() === app.repo_url} onClick={save}>
          {saving ? "Saving…" : "Change"}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          disabled={saving}
          onClick={() => {
            setUrl(app.repo_url);
            setOpen(false);
          }}
        >
          Cancel
        </Button>
      </div>
      <FieldHelp>
        The current deployment keeps serving until the next deploy succeeds. The stored access token
        is kept and may not work on a different host.
      </FieldHelp>
    </div>
  );
}

/**
 * One section of the Settings tab.
 *
 * Three jobs, and each was being done differently by each of the fourteen
 * sections before it existed. It carries the **anchor** the settings index
 * points at, so `/apps/{id}/settings#source` is a real destination. It draws
 * the divider above itself rather than leaving a `<Separator/>` beside it,
 * which is what lets a section that decides it does not exist — Storage with
 * nothing mapped, Gallery on an app that came from a repository — disappear
 * without leaving a rule across the page. And it reads the filter, so the ten
 * components that render one do not each have to take a `visible` prop.
 */
function TabSection({
  id,
  title,
  description,
  tone,
  children,
}: {
  id: string;
  title: string;
  description?: React.ReactNode;
  tone?: "danger";
  children: React.ReactNode;
}) {
  const visible = useContext(VisibleSections);
  if (visible && !visible.has(id)) return null;
  // A card per section, as Admin → Settings draws its groups: the two settings
  // surfaces had been one shape in their rows and two in their sections, a
  // ruled column here beside a stack of cards there. The danger zone is the
  // same card with the danger border, rather than a bordered box inside one.
  return (
    <Section
      id={id}
      title={title}
      description={description}
      className={tone === "danger" ? toneBorder.danger : undefined}
    >
      {/* `empty:hidden`: a section whose body renders nothing (an uploaded
          app's Source & deploys) is its header alone, not a padded blank. */}
      <div className="p-4 empty:hidden">{children}</div>
    </Section>
  );
}

/** Everything one drafted field needs to draw itself. */
interface DraftField {
  id: string;
  value: string;
  dirty: boolean;
  error: string | null;
  note?: string;
  disabled: boolean;
  onChange: (v: string) => void;
  onSubmit: () => void;
}

/**
 * A field that batches, and the two things it has to say that a Save button
 * beside it used to say by being enabled: this one has been touched, and this
 * one cannot be saved as typed.
 *
 * The dot's space is reserved whether or not it is drawn, so a row does not
 * shift under the cursor as it becomes dirty. Enter saves the whole draft
 * rather than this field, which is the same promise the bar at the bottom
 * makes.
 */
function DraftInput({
  label,
  field,
  hint,
  type,
  placeholder,
  className,
}: {
  label: string;
  field: DraftField;
  hint?: string;
  type?: string;
  placeholder?: string;
  className?: string;
}) {
  const describedBy = field.error ? `${field.id}-error` : undefined;
  // The same row as Admin → Settings (`FieldRow`): the name and its help on
  // the left, the field in a fixed column on the right, so the two settings
  // surfaces are one form rather than two.
  return (
    <div className={FIELD_ROW}>
      <div className="min-w-0 space-y-1">
        <div className="flex items-center gap-2">
          <Label htmlFor={field.id} className="text-sm font-medium">{label}</Label>
          <span className="flex w-2 justify-center" aria-hidden>
            {field.dirty && (
              <span
                className={cn(
                  "h-1.5 w-1.5 rounded-full",
                  field.error ? "bg-destructive" : "bg-primary",
                )}
              />
            )}
          </span>
          {field.dirty && <span className="sr-only">Unsaved</span>}
        </div>
        {hint && <FieldHelp>{hint}</FieldHelp>}
        {field.note && <FieldHelp tone="attention">{field.note}</FieldHelp>}
      </div>
      <div className={cn(FIELD_CONTROL, "flex-col items-stretch gap-1 sm:items-end")}>
        <Input
          id={field.id}
          type={type}
          value={field.value}
          placeholder={placeholder}
          disabled={field.disabled}
          aria-invalid={!!field.error}
          aria-describedby={describedBy}
          className={cn("w-full", className)}
          onChange={(e) => field.onChange(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") field.onSubmit();
          }}
        />
        {field.error && (
          <FieldError field={field.id}>
            {field.error}
          </FieldError>
        )}
      </div>
    </div>
  );
}

/** A section's own words, plus every word of every setting it holds. */
function sectionText(section: AppSettingsSection): string {
  return `${section.title} ${section.blurb} ${section.keywords ?? ""} ${appSettingsIn(section)
    .map(settingText)
    .join(" ")}`.toLowerCase();
}

/**
 * A titled sub-region that is not a row — a multi-field editor or a list,
 * which a 20rem control column would cramp — stacked at the section's width.
 * No border of its own: under `ROWS` the hairline above it is the boundary.
 */
function SubBlock({
  title,
  description,
  className,
  children,
}: {
  title: string;
  description?: React.ReactNode;
  className?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className={cn("space-y-3", className)}>
      <div className="space-y-1">
        <p className="text-sm font-medium text-foreground">{title}</p>
        {description && <div className="text-xs text-muted-foreground">{description}</div>}
      </div>
      {children}
    </div>
  );
}

/**
 * `FieldRow`'s shape for a row with no form control to label — a read-only
 * value, or a button — so it lines up with the switches beside it.
 */
function InfoRow({
  title,
  help,
  extra,
  children,
}: {
  title: string;
  help?: React.ReactNode;
  extra?: React.ReactNode;
  children?: React.ReactNode;
}) {
  return (
    <div className={FIELD_ROW}>
      <div className="min-w-0 space-y-1">
        <p className="text-sm font-medium text-foreground">{title}</p>
        {help && <FieldHelp>{help}</FieldHelp>}
        {extra}
      </div>
      <div className={FIELD_CONTROL}>{children}</div>
    </div>
  );
}

export function SettingsTab({ appId, app, serverEnvVars, hasGitToken, gitConnection, appTags, policy, runtime, ownerLogin }: { appId: string; app: App; serverEnvVars: AppEnvVar[]; hasGitToken: boolean; gitConnection: DeveloperGitConnection | null; appTags: AppTag[]; policy?: AppPolicy; runtime?: AppRuntime; ownerLogin?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  /** Raw text per edited field. A key absent here means "as stored". */
  const [drafts, setDrafts] = useState<Partial<Record<AppFieldKey, string>>>({});
  const [autoToggling, setAutoToggling] = useState(false);
  const [savingToken, setSavingToken] = useState(false);
  const [envVars, setEnvVars] = useState<EnvRow[]>(envRowsFrom(serverEnvVars));
  // Saving sends the whole form, and a key omitted from it is deleted — so a
  // form seeded before a build added variables (e.g. from an environment
  // defaults file) must not be saved stale. Re-seed from the server whenever
  // the form is untouched; once the user edits, their draft wins — but keys
  // the server grew *after* the form was seeded (a deploy's environment
  // defaults, an edit in another tab) are appended to the draft, or saving it
  // would silently delete them. Keys the user removed or renamed stay in the
  // seeded set, so they don't reappear.
  const [envDirty, setEnvDirty] = useState(false);
  // Adjusted during render rather than in an effect, so no frame draws the
  // stale table: `envSeed` is what the form was last reconciled against.
  const envSignature = JSON.stringify(serverEnvVars);
  const [envSeed, setEnvSeed] = useState(() => ({
    signature: envSignature,
    dirty: envDirty,
    keys: new Set(serverEnvVars.map((v) => v.key)),
    // The server variables the rows were seeded from — what `envPayload`
    // diffs against, so a row nobody touched is never sent back stale.
    vars: serverEnvVars,
  }));
  if (envSeed.signature !== envSignature || envSeed.dirty !== envDirty) {
    if (!envDirty) {
      setEnvVars(envRowsFrom(serverEnvVars));
      setEnvSeed({
        signature: envSignature,
        dirty: envDirty,
        keys: new Set(serverEnvVars.map((v) => v.key)),
        vars: serverEnvVars,
      });
    } else {
      const fresh = serverEnvVars.filter((v) => !envSeed.keys.has(v.key));
      const keys = new Set(envSeed.keys);
      fresh.forEach((v) => keys.add(v.key));
      if (fresh.length > 0) {
        setEnvVars((prev) => {
          const drafted = new Set(prev.map((r) => r.key));
          return [...prev, ...envRowsFrom(fresh.filter((v) => !drafted.has(v.key)))];
        });
      }
      setEnvSeed({
        signature: envSignature,
        dirty: envDirty,
        keys,
        vars: envSnapshotGrown(envSeed.vars, fresh),
      });
    }
  }
  /** Which rows have had their typed value revealed, by row id. */
  const [visibleEnvRows, setVisibleEnvRows] = useState<Set<string>>(new Set());
  const [newEnvKey, setNewEnvKey] = useState("");
  const [newEnvValue, setNewEnvValue] = useState("");
  const [newEnvSensitive, setNewEnvSensitive] = useState(false);
  const [deleteDialogOpen, setDeleteDialogOpen] = useState(false);
  const [transferDialogOpen, setTransferDialogOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [query, setQuery] = useState("");
  const [activeSection, setActiveSection] = useState(appSectionsOn("settings")[0].id);

  // UI decisions come from the introspection endpoint, never re-derived here.
  const { data: perms } = useQuery<AppPermissions>({
    queryKey: ["app-permissions", appId],
    queryFn: () => api.getAppPermissions(appId),
  });

  /* ---------------------------------------------------------------- *
   * The draft
   *
   * Five fields collect here and go out as one request from the bar at
   * the bottom of the page. The switches beside them deliberately do
   * not: the server's answer to a toggle belongs to the toggle, and
   * environment variables are one decision saved as a set.
   *
   * The git credential is in the draft and is still write-only. What is
   * stored never comes back, so its "stored" value is the empty string
   * and anything typed is a change — which also means blank can only
   * ever mean *leave it alone*, never *delete it*. Removing one is the
   * Remove button beside the notice, an act of its own.
   * ---------------------------------------------------------------- */

  const gitSource = isGitSource(app);
  /** A gallery extension takes its code from the catalog, so the branch it
   * nominally tracks is not what deploys it — and the server refuses an
   * auto-deploy switch on one with a 400. */
  const galleryLinkedExtension = policy?.gallery?.kind === "extension";
  const storedField: Record<AppFieldKey, string> = {
    name: app.name,
    slug: app.slug,
    branch: app.branch,
    repo_subdir: app.repo_subdir ?? "",
    git_token: "",
    releases_kept: app.releases_kept ? String(app.releases_kept) : "",
  };

  // A field counts as pending only while it is on screen. Typing a version
  // count and then switching automatic cleanup off must not leave a change
  // queued behind a row nobody can see — the same rule Admin → Settings
  // applies to the job figures.
  const liveFields = APP_FIELD_KEYS.filter(
    (k) =>
      (gitSource || (k !== "branch" && k !== "repo_subdir" && k !== "git_token")) &&
      (app.auto_delete_releases || k !== "releases_kept"),
  );

  const draftOf = (key: AppFieldKey) => drafts[key] ?? storedField[key];
  const isDirty = (key: AppFieldKey) =>
    drafts[key] !== undefined && drafts[key]!.trim() !== storedField[key].trim();
  const dirtyFields = liveFields.filter(isDirty);
  const brokenFields = dirtyFields.filter((k) => appFieldError(k, draftOf(k)) !== null);

  async function saveAll() {
    // The bar counts the environment table too, so this has to be able to save
    // it. Before, a person who edited a variable and a field saw "1 unsaved
    // change" over two, pressed Save, was told it worked, and walked away
    // having lost the variable.
    const withEnv = envDirty && duplicateEnvKeys.size === 0 && blockedRenames.length === 0;
    if ((dirtyFields.length === 0 && !withEnv) || brokenFields.length > 0 || saving) return;
    const payload: Record<string, string | number> = {};
    for (const key of dirtyFields) {
      const raw = draftOf(key).trim();
      if (key === "releases_kept") payload.releases_kept = raw === "" ? 0 : Number(raw);
      else payload[key] = raw;
    }
    const env = withEnv ? envPayload() : null;
    setSaving(true);
    try {
      // Two routes, the fields first: the environment has its own (a merge
      // naming only what changed), and a field the server refuses should
      // stop the save before a variable lands without it.
      if (dirtyFields.length > 0) await api.updateApp(appId, payload);
      if (env) await api.updateAppEnv(appId, env);
      // Only the drafts this request carried: a field typed while it was in
      // flight, or one hidden behind a switch, is still somebody's intent.
      setDrafts((d) => {
        const next = { ...d };
        for (const key of dirtyFields) delete next[key];
        return next;
      });
      // Re-read first, so the re-seed reads post-save state rather than
      // flashing the pre-save values.
      await queryClient.invalidateQueries({ queryKey: ["app", appId] });
      if (withEnv) clearEnvDraft();
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      // One message, plus the one thing a batched save would otherwise stop
      // saying: a new tracked branch on an app that deploys itself is about to
      // build something.
      const branchWillDeploy = dirtyFields.includes("branch") && app.auto_deploy;
      toast.success(
        "Saved" +
          (branchWillDeploy ? `. ${draftOf("branch").trim()} deploys on the next check` : "") +
          // The sentence the env path owns, kept when the bar is what saved it.
          (env ? ". Environment variables apply on the next restart" : ""),
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save");
    } finally {
      setSaving(false);
    }
  }

  /** Everything one drafted field needs, in one call. */
  function field(key: AppFieldKey): DraftField {
    const value = draftOf(key);
    return {
      id: APP_FIELD_CONTROL[key],
      value,
      dirty: isDirty(key),
      error: appFieldError(key, value),
      note: appFieldNote(key, value, storedField[key]),
      disabled: saving,
      onChange: (v: string) => setDrafts((d) => ({ ...d, [key]: v })),
      onSubmit: saveAll,
    };
  }

  /**
   * The environment as one request body, built in one place.
   *
   * Two writers send it — this section's own buttons and the page's pending
   * bar — so the diff is `envChanges`'s, and a key typed into the add row but
   * never "+"-ed is still an intent to save it. Null when nothing changed.
   */
  function envPayload(): UpdateAppEnvRequest | null {
    const rows: EnvFormRow[] = envVars.map((v) => ({
      key: v.key,
      value: v.value,
      sensitive: v.sensitive,
      seededKey: v.seededKey,
    }));
    if (newEnvKey.trim()) rows.push({ key: newEnvKey, value: newEnvValue, sensitive: newEnvSensitive });
    // Against the snapshot the form was seeded from, not the live prop: the
    // page re-reads the app every 15 s, and a row whose stored value moved
    // under an open form would otherwise be sent back with the old one.
    return envChanges(envSeed.vars, rows);
  }

  /** Discarding the env draft is clearing the flag: the effect above re-seeds
   *  `envVars` from the server whenever it is false. */
  function clearEnvDraft() {
    setEnvDirty(false);
    setNewEnvKey("");
    setNewEnvValue("");
    setNewEnvSensitive(false);
  }

  /**
   * Saving the environment, and — on the primary button — applying it.
   *
   * Two buttons rather than one, and deliberately not an automatic restart on
   * save. Heroku and Fly restart on a config change because their unit is one
   * variable set by one command; this is a multi-row form with an explicit
   * Save, and silently bouncing somebody's serving app on a form submit is a
   * larger act than the button admits. So it is offered, which is what Render
   * and Railway do and what this product's confirmation policy already says.
   *
   * *Rebuild and deploy* is deliberately not a third option: since a start
   * assembles the whole description and reads the variables live, a rebuild is
   * never required for a configuration change, and offering it would teach the
   * more expensive habit.
   */
  async function saveEnvVars(thenRestart = false) {
    if (duplicateEnvKeys.size > 0) {
      toast.error(
        `${[...duplicateEnvKeys].join(", ")} appears more than once. Remove or rename the duplicate.`,
      );
      return;
    }
    if (blockedRenames.length > 0) {
      toast.error(`Type the value of ${blockedRenames.join(", ")} again: a hidden value can’t be carried to a new name`);
      return;
    }
    setSaving(true);
    try {
      const env = envPayload();
      if (env) await api.updateAppEnv(appId, env);
      await queryClient.invalidateQueries({ queryKey: ["app", appId] });
      clearEnvDraft();
      if (thenRestart) {
        // The save landed; the restart is its own act and reports its own
        // outcome. Not folded into one toast, because a save that succeeded
        // and a restart that did not are two different things to be told.
        toast.success("Saved");
        await handleRestart();
        return;
      }
      // No "redeploy to apply them" any more: it named an action the person
      // then had to go and find, in a toast that was gone in four seconds. The
      // notice on Overview is what carries this now, and it survives a reload.
      toast.success("Saved. Environment variables apply on the next restart");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save environment variables");
    } finally {
      setSaving(false);
    }
  }

  async function toggleAutoDeploy(enabled: boolean) {
    setAutoToggling(true);
    try {
      await api.updateApp(appId, { auto_deploy: enabled });
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        enabled
          ? `Auto-deploy on. New commits on ${app.branch} deploy automatically`
          : "Auto-deploy off",
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t update auto-deploy");
    } finally {
      setAutoToggling(false);
    }
  }

  /**
   * Removing the stored credential, which is not a field edit.
   *
   * Blank in the draft means *keep what is stored* — it has to, because a
   * write-only field's stored value is never on screen to compare against. So
   * clearing one is its own act, and it saves itself the way a switch does.
   */
  async function removeGitToken() {
    setSavingToken(true);
    try {
      await api.updateApp(appId, { git_token: "" });
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      setDrafts((d) => ({ ...d, git_token: "" }));
      toast.success("Git access token removed");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t update git access");
    } finally {
      setSavingToken(false);
    }
  }

  // Restart, from the one hook the header's ⋯ menu also calls — including its
  // 202 handling and its `["restart", appId]` key, which the header's status
  // pill watches. This tab's only remaining caller is Save-and-restart below.
  const { restart: handleRestart, restarting } = useAppLifecycle(appId, app);

  function addEnvVar() {
    if (!newEnvKey.trim()) return;
    setEnvDirty(true);
    setEnvVars((prev) => [
      ...prev,
      { id: nextEnvRowId(), key: newEnvKey, value: newEnvValue, sensitive: newEnvSensitive, stored: false },
    ]);
    setNewEnvKey("");
    setNewEnvValue("");
    setNewEnvSensitive(false);
  }

  function removeEnvVar(id: string) {
    setEnvDirty(true);
    setEnvVars((prev) => prev.filter((v) => v.id !== id));
    setVisibleEnvRows((prev) => {
      if (!prev.has(id)) return prev;
      const next = new Set(prev);
      next.delete(id);
      return next;
    });
  }

  function toggleEnvSensitive(id: string) {
    setEnvDirty(true);
    setEnvVars((prev) => prev.map((v) => (v.id === id ? { ...v, sensitive: !v.sensitive } : v)));
  }

  function toggleEnvVisibility(id: string) {
    setVisibleEnvRows((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  /**
   * Names typed twice.
   *
   * The form goes out as one object, so two rows sharing a name cannot both
   * survive it: the later one silently won and the earlier one vanished on
   * save. Named rather than merged, because which of the two values the person
   * meant is not something this page can know — and the add row counts, since
   * a key typed there and never "+"-ed is still saved.
   */
  const envKeysTyped = [...envVars.map((v) => v.key.trim()), newEnvKey.trim()].filter(Boolean);
  const duplicateEnvKeys = new Set(
    envKeysTyped.filter((k, i) => envKeysTyped.indexOf(k) !== i),
  );
  /**
   * Rows renamed from a stored hidden variable with nothing typed. The form
   * never holds a hidden value, so saving one would delete the secret and
   * store an empty value under the new name.
   */
  const blockedRenames = hiddenRenames(
    envSeed.vars,
    envVars.map((v) => ({ key: v.key, value: v.value, sensitive: v.sensitive, seededKey: v.seededKey })),
  );
  const blockedRenameSet = new Set(blockedRenames);

  function editEnvRow(id: string, patch: Partial<EnvRow>) {
    setEnvDirty(true);
    setEnvVars((prev) => prev.map((v) => (v.id === id ? { ...v, ...patch } : v)));
  }

  // Which sections the filter leaves standing. A section matches on its own
  // words as well as its settings', so "versions" brings back the whole card
  // rather than only the rows with that word in them — the same rule Admin →
  // Settings applies, from the same index.
  const q = query.trim().toLowerCase();
  const allSections = appSectionsOn("settings");
  const matchedSections = q ? allSections.filter((s) => sectionText(s).includes(q)) : allSections;
  const visibleSections = q ? new Set(matchedSections.map((s) => s.id)) : null;
  const matchCount = q
    ? allSections
        .flatMap(appSettingsIn)
        .filter((entry) => settingText(entry).toLowerCase().includes(q)).length
    : 0;
  const nothingMatches = matchedSections.length === 0;
  useSectionSpy(
    matchedSections.map((s) => s.id),
    setActiveSection,
  );

  return (
    <div className={cn("space-y-6 pt-4", (dirtyFields.length > 0 || envDirty) && "pb-24")}>
      {/* The box this tab did not have. Fourteen sections in a column half the
          width of the page was the same finding ADR §50 made about the admin
          section, and a filter is the one mechanism that made thirteen
          sections survivable there. */}
      {/* Top right, over the rail, where Admin → Settings puts its own. */}
      <div className="flex max-w-[61.5rem] justify-end">
        <SearchInput
          placeholder="Filter this app’s settings…"
          value={query}
          onChange={setQuery}
          debounceMs={0}
          className="w-full sm:w-64"
        />
      </div>

      {q && (
        <div className="space-y-2">
          <p className="text-sm text-muted-foreground" aria-live="polite">
            {nothingMatches
              ? `Nothing on this tab matches “${query}”.`
              : matchCount === 0
                ? `${matchedSections.length} section${matchedSections.length === 1 ? "" : "s"} match “${query}”.`
                : `${matchCount} setting${matchCount === 1 ? "" : "s"} in ${matchedSections.length} section${
                    matchedSections.length === 1 ? "" : "s"
                  } match “${query}”.`}{" "}
            <button
              type="button"
              onClick={() => setQuery("")}
              className="rounded-sm text-primary hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
            >
              Clear
            </button>
          </p>
          {/* The consumer that changes an answer: this tab does not hold
              visibility, and until now said so by denying it existed. */}
          <OtherTabsMatch appId={appId} query={query} tab="settings" />
        </div>
      )}

      {nothingMatches && (
        <EmptyState
          icon={<Search className="h-6 w-6" />}
          title="No setting by that name on this tab"
          description="Try “branch”, “sleep”, “deployments” or “environment”."
          action={
            <Button variant="secondary" size="sm" onClick={() => setQuery("")}>
              Clear filter
            </Button>
          }
        />
      )}

      <VisibleSections.Provider value={visibleSections}>
      {/* Content and rail as one block — 48rem, the gap, the 11rem rail —
          so the rail sits beside the column it indexes, as it does on Admin →
          Settings. Pinned to the container's edge instead, it floated 250px
          away from the content on a wide window: the measure decides the
          column, and the rail follows the column. */}
      <div className="flex max-w-[61.5rem] gap-10">
      {/* `max-w-3xl`, not `max-w-xl`. The cap is what decides where the rail
          lands: `flex-1` cannot grow past it, so a 576px column put the rail at
          x=720 in a 1,232px row and left 440 pixels of nothing to the right of
          it — the rail floating mid-canvas rather than pinned to an edge. This
          is the measure Admin → Settings already resolves to, so the product's
          two settings surfaces are now the same shape. It is still a measure:
          a label-and-field pair at the full 1,232 scans worse, not better. */}
      <div className="min-w-0 max-w-3xl flex-1 space-y-8">
      <TabSection id="identity" title="Identity">
        {/* The same hairline rows as every other section here: stacked with
            no rhythm, Slug's help line ran straight into the Hostname label. */}
        <div className={ROWS}>
        <DraftInput label="App name" field={field("name")} />
        <DraftInput
          label="Slug"
          field={field("slug")}
          hint="The end of this app’s address. The old address stops working immediately."
        />
        <HostnameBlock appId={appId} app={app} />
        {/* Read-only, and after the two editable names on purpose: those are
            what this app is called, and this is the name nothing else can
            change. It is the one identifier that survives a rename, which is
            why the log and the metrics use it instead. */}
        <FieldRow
          label="App ID"
          htmlFor="app-id"
          help="Never changes. Logs, metrics and audit entries use it."
        >
          <div className="flex w-full items-center gap-2">
            <Input
              id="app-id"
              readOnly
              value={appId}
              onFocus={(e) => e.target.select()}
              className="flex-1 font-mono text-xs md:text-xs"
            />
            <Button variant="outline" size="icon" aria-label="Copy app ID" onClick={() => void copyToClipboard(appId, "app ID")}>
              <Copy className="h-4 w-4" />
            </Button>
          </div>
        </FieldRow>

        {/* A titled group under a hairline, not a bordered box: the rows
            are the same label-and-control shape as the fields above, and a
            box inside the section made them read as a different kind of
            thing. The help line is the one fact the reader cannot guess —
            these save on the pick, not through the page's bar. The heading
            is the editor's, because only it knows whether anything here can
            be picked at all. */}
        <div className="space-y-3">
          <TagEditor appId={appId} appTags={appTags} canEdit={!!perms?.can_edit} />
        </div>
        </div>
      </TabSection>

      {/* Every app has a source, including one that was uploaded rather than
          cloned — and an app whose code arrives from the gallery has one that
          is neither, which is why the gallery panel is *here* rather than a
          section of its own. "Updated from the gallery rather than by tracking
          a branch, which is why auto-deploy is unavailable while it is linked"
          is a statement about where this app's code comes from, and it belongs
          against the branch field it disables. */}
      <TabSection
        id="source"
        title="Source & deploys"
        description={
          isGitSource(app) ? (
            <span className="block truncate font-mono" title={app.repo_url}>
              {app.repo_url}
            </span>
          ) : (
            "Uploaded. This app tracks no repository."
          )
        }
      >
        {policy && !policy.source_allowed && <Notice>{policy.source_reason}</Notice>}

        {isGitSource(app) && (
          <>
            <RepointControl appId={appId} app={app} />

            <DraftInput
              label="Tracked branch"
              field={field("branch")}
              className="font-mono"
              hint="Redeploys and auto-deploys build the tip of this branch."
            />

            <DraftInput
              label="Folder"
              field={field("repo_subdir")}
              className="font-mono"
              hint="The folder of the repository this app is built from. Blank is the root."
            />

            {/* A linked extension is updated from the gallery, not from a
                branch, and the server refuses this with a 400. The switch was
                fully live while the panel below it said the feature was
                unavailable — the two disagreed, and the one a person believes
                is the one they can throw. The reason is beside the control
                rather than in a `title`, because a disabled control is not
                focusable and swallows the hover. */}
            {galleryLinkedExtension && (
              <Notice tone="info" title="Auto-deploy is off while this app is linked to the gallery">
                Unlink it below to track a branch.
              </Notice>
            )}
            <FieldRow
              label="Auto-deploy on push"
              htmlFor="auto-deploy"
              help={
                <>
                  Deploys new commits on <span className="font-mono">{app.branch}</span> within
                  about a minute.
                </>
              }
            >
              <Switch
                id="auto-deploy"
                checked={app.auto_deploy}
                disabled={autoToggling || galleryLinkedExtension}
                onCheckedChange={toggleAutoDeploy}
              />
            </FieldRow>

            {/* Which credential is in force, and only one of them is.
                A connection wins over a pasted token, so showing the token
                field beside a connection-backed app would offer a fix for a
                failure that field cannot cause. */}
            {gitConnection ? (
              <Notice
                tone="ok"
                title={`Cloned through ${gitConnection.display_name}`}
              >
                An administrator manages this credential on Admin → Sources.
              </Notice>
            ) : (
            <div className="space-y-2">
              {hasGitToken && (
                <Notice
                  tone="ok"
                  title="A token is stored"
                  action={
                    <Button variant="ghost" size="sm" disabled={savingToken} onClick={removeGitToken}>
                      Remove
                    </Button>
                  }
                />
              )}
              <DraftInput
                label="Git access token"
                field={field("git_token")}
                type="password"
                className="font-mono text-xs"
                placeholder={hasGitToken ? "•••••••• (unchanged)" : "e.g. github_pat_…"}
                hint={
                  hasGitToken
                    ? "For a private repository. Leave blank to keep the stored token."
                    : "For a private repository: a token with read access, stored encrypted."
                }
              />
            </div>
            )}
          </>
        )}

        {/* The other thing that makes this app rebuild itself, under the
            first. It used to sit on Overview, where "what makes this app
            rebuild itself" had two answers on two tabs — and where a hub that
            states "no tile mutates" held four write paths. */}
        {isRerenderable(app.framework) && (
          <div className="space-y-3 border-t border-border/60 pt-4">
            <SectionHeading
              level={3}
              title="Re-render on a schedule"
              description="Render this document again without a deploy."
            />
            <NotebookRenderSchedule appId={appId} />
          </div>
        )}

        <GalleryBlock appId={appId} policy={policy} />
      </TabSection>

      <TabSection id="runtime" title="Runtime">
        <div className={ROWS}>
        <RuntimeVersionBlock app={app} runtime={runtime} />
        <ExecutionModeBlock appId={appId} app={app} />
        <SleepBlock appId={appId} app={app} policy={policy} />
        {/* A process to restart: not a batch app, a static site or a
            document, which detect refuses [restart] on. */}
        {!isBatchFramework(app.framework) && app.framework !== "static" && !isRerenderable(app.framework) && (
          <RestartScheduleBlock appId={appId} shared={app.execution_mode !== "isolated"} />
        )}
        <MemoryLimitBlock appId={appId} app={app} policy={policy} />
        <CPULimitBlock appId={appId} app={app} policy={policy} />
        <ViewerIdentityBlock appId={appId} app={app} policy={policy} />
        <ReleaseRetentionBlock appId={appId} app={app} policy={policy} field={field("releases_kept")} />

        <AttachedCredentials appId={appId} />

        <SubBlock
          title="Environment variables"
          description="Lock a value to hide it once saved. Leave a hidden value blank to keep it; type a new one to unhide it."
        >
        <div className="space-y-2">
          {envVars.map((ev) => (
            <div key={ev.id} className="flex items-center gap-2">
              <Input
                value={ev.key}
                onChange={(e) => editEnvRow(ev.id, { key: e.target.value })}
                placeholder="KEY"
                aria-label="Variable name"
                aria-invalid={duplicateEnvKeys.has(ev.key.trim())}
                className="flex-1 font-mono text-xs"
              />
              <Input
                type={ev.sensitive && !visibleEnvRows.has(ev.id) ? "password" : "text"}
                value={ev.value}
                onChange={(e) => editEnvRow(ev.id, { value: e.target.value })}
                placeholder={
                  blockedRenameSet.has(ev.key.trim())
                    ? "type the value again"
                    : ev.sensitive && ev.stored
                      ? ev.unset
                        ? "not set"
                        : "•••••••• (unchanged)"
                      : "value"
                }
                aria-label={ev.key.trim() ? `Value of ${ev.key.trim()}` : "Variable value"}
                className="flex-1 font-mono text-xs"
              />
              {ev.sensitive && (
                <Button variant="ghost" size="icon-sm" aria-label={visibleEnvRows.has(ev.id) ? "Hide value" : "Show typed value"} onClick={() => toggleEnvVisibility(ev.id)}>
                  {visibleEnvRows.has(ev.id) ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                </Button>
              )}
              {/* Why the lock can't open on a stored value is said in the
                  block's description, in words everybody sees — not in a
                  `title`, which a disabled control can't deliver. The
                  button's own name carries it too. */}
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={
                    ev.sensitive
                      ? ev.stored && !ev.value
                        ? "Sensitive. Type a new value before making it visible."
                        : "Sensitive — make it visible"
                      : "Visible — mark it sensitive"
                  }
                  disabled={ev.sensitive && ev.stored && !ev.value}
                  onClick={() => toggleEnvSensitive(ev.id)}
                >
                  {ev.sensitive ? <Lock className="h-3.5 w-3.5" /> : <LockOpen className="h-3.5 w-3.5 text-muted-foreground" />}
                </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={ev.key.trim() ? `Remove ${ev.key.trim()}` : "Remove variable"}
                onClick={() => removeEnvVar(ev.id)}
              >
                <X className="h-3.5 w-3.5" />
              </Button>
            </div>
          ))}
          <div className="flex items-center gap-2">
            <Input value={newEnvKey} onChange={(e) => setNewEnvKey(e.target.value)} placeholder="NEW_KEY" aria-label="New variable name" className="flex-1 font-mono text-xs" onKeyDown={(e) => { if (e.key === "Enter") addEnvVar(); }} />
            <Input type={newEnvSensitive ? "password" : "text"} value={newEnvValue} onChange={(e) => setNewEnvValue(e.target.value)} placeholder="value" aria-label="New variable value" className="flex-1 font-mono text-xs" onKeyDown={(e) => { if (e.key === "Enter") addEnvVar(); }} />
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={newEnvSensitive ? "Sensitive — click to make visible" : "Visible — click to mark sensitive"}
              title={newEnvSensitive ? "Sensitive — click to make visible" : "Visible — click to mark sensitive"}
              onClick={() => setNewEnvSensitive((s) => !s)}
            >
              {newEnvSensitive ? <Lock className="h-3.5 w-3.5" /> : <LockOpen className="h-3.5 w-3.5 text-muted-foreground" />}
            </Button>
            <Button variant="outline" size="icon-sm" aria-label="Add variable" onClick={addEnvVar}>
              <Plus className="h-3.5 w-3.5" />
            </Button>
          </div>
          {newEnvKey.trim() !== "" && !duplicateEnvKeys.has(newEnvKey.trim()) && (
            <p className="text-xs text-muted-foreground">
              <span className="font-mono">{newEnvKey.trim()}</span> isn’t in the list yet. Save
              includes it.
            </p>
          )}
          {duplicateEnvKeys.size > 0 && (
            <Notice tone="danger" title="The same name is used twice">
              <span className="font-mono">{[...duplicateEnvKeys].join(", ")}</span> appears more
              than once. Remove or rename the duplicate.
            </Notice>
          )}
          {blockedRenames.length > 0 && (
            <Notice tone="danger" title="A hidden value needs typing again">
              <span className="font-mono">{blockedRenames.join(", ")}</span>{" "}
              {blockedRenames.length === 1 ? "was" : "were"} renamed from a hidden variable. Type
              its value again: a hidden value can’t be carried to a new name.
            </Notice>
          )}
          <div className="flex flex-wrap items-center gap-2">
            {/* The one act the page's bar can't do. Saving alone is the bar's
                "Save changes" — a second button for it was a second save path
                with different words. */}
            <Button
              size="sm"
              onClick={() => saveEnvVars(true)}
              disabled={
                saving ||
                restarting ||
                !envDirty ||
                !!app.locked_at ||
                duplicateEnvKeys.size > 0 ||
                blockedRenames.length > 0
              }
            >
              {restarting ? "Restarting…" : saving ? "Saving…" : "Save and restart"}
            </Button>
          </div>
          {app.locked_at ? (
            <Notice tone="neutral">
              The app is locked, so it can’t be restarted. Saved changes apply once it’s unlocked.
            </Notice>
          ) : (
            <p className="text-xs text-muted-foreground">
              A restart applies the values without a rebuild.
            </p>
          )}
        </div>
        </SubBlock>
        </div>
      </TabSection>

      <TabSection
        id="notifications"
        title="Notifications"
      >
        <div className={ROWS}>
          <FailureNotificationBlock appId={appId} app={app} />
          <IntegrationsBlock appId={appId} policy={policy} />
        </div>
      </TabSection>

      {/* Storage used to sit here beside it. It is on Overview now: a mount is
          a fact about the app rather than a setting on it, and this tab is the
          one you open to change something. */}
      <TabSection id="data" title="Data">
        <SavedDataBlock appId={appId} app={app} canEdit={perms?.can_edit ?? false} />
      </TabSection>

      <TabSection id="danger" title="Danger zone" tone="danger">
        <div className="space-y-4">
          {perms?.can_transfer_ownership && (
            <>
              <div className="flex items-center justify-between gap-4">
                <div className="min-w-0">
                  <p className="text-sm font-medium">Transfer ownership</p>
                  {/* Who owns it now, which this page did not say anywhere.
                      Deciding to hand an app on without being told who holds it
                      is the one question this row was missing, and for a
                      group-owned app the answer is a team and its size rather
                      than the name of whoever set it up. */}
                  <p className="min-w-0 text-xs text-muted-foreground">
                    Owned by{" "}
                    <AppOwner
                      ownerId={app.owner_id}
                      ownerGroupId={app.owner_group_id}
                      ownerLogin={ownerLogin}
                      className="text-foreground"
                    />
                    . You stay on as an editor.
                  </p>
                </div>
                <Button variant="outline" size="sm" onClick={() => setTransferDialogOpen(true)}>
                  <Users className="h-3.5 w-3.5 mr-1.5" /> Transfer…
                </Button>
              </div>
              <Separator />
            </>
          )}
          {/* Start, restart and stop used to be two of these rows. They are in
              the header's ⋯ menu now, beside Redeploy and on every tab — the
              product's own risk table puts all three at *no confirmation*
              because each is reversible and the control beside it is the undo,
              and filing them under a red heading beside Transfer and Delete
              said the opposite. A warning that cries wolf about a restart is a
              warning nobody reads before a deletion.

              What is left here is the two acts the heading was written for:
              both irreversible, both unbounded, both taking the app's history
              with them. */}
          {app.locked_at && (
            <Notice tone="danger" title="Locked by an administrator">
              Starting, restarting and deploying are refused. The Access tab says who locked it and why.
            </Notice>
          )}
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm font-medium">Delete app</p>
              <p className="text-xs text-muted-foreground">Permanently remove this app and all data.</p>
            </div>
            <Button variant="destructive" size="sm" onClick={() => setDeleteDialogOpen(true)}>
              <Trash2 className="h-3.5 w-3.5 mr-1.5" /> Delete app…
            </Button>
          </div>
        </div>
      </TabSection>
      </div>

      {/* The recovered width. 576 of 1,232 pixels were in use for 2.9 screens
          of scrolling; the right half of the window was empty from the first
          section to the last. It is not for making the fields wider — a
          label-and-field pair at 1,232 pixels scans worse, not better. */}
      {!nothingMatches && <SectionRail sections={matchedSections} active={activeSection} />}
      </div>
      </VisibleSections.Provider>

      <DeleteAppDialog
        app={{ id: appId, name: app.name, slug: app.slug }}
        open={deleteDialogOpen}
        onOpenChange={setDeleteDialogOpen}
        onDeleted={() => navigate("/")}
      />

      <TransferOwnershipDialog
        app={{ id: appId, name: app.name, slug: app.slug }}
        open={transferDialogOpen}
        onOpenChange={setTransferDialogOpen}
      />

      {/* One save affordance where there were five, and the page-wide answer
          to *am I saved?* that none of the five could give. */}
      <PendingBar
        count={dirtyFields.length + (envDirty ? 1 : 0)}
        broken={
          brokenFields.length + (duplicateEnvKeys.size > 0 || blockedRenames.length > 0 ? 1 : 0)
        }
        saving={saving}
        onDiscard={() => {
          setDrafts({});
          clearEnvDraft();
        }}
        onSave={saveAll}
      />
    </div>
  );
}

/**
 * Where this app runs.
 *
 * Admin-only, and gated twice: the platform must offer isolated execution at
 * all (Admin → Settings), and only then may an app be promoted. Shown to
 * non-admins as a read-only fact, because *where my app runs* is worth knowing
 * even when you cannot change it.
 *
 * Deliberately a setting rather than a redeploy: a density decision that is
 * expensive to reverse is one people avoid making, and getting it wrong should
 * cost a toggle rather than a migration.
 */
/**
 * Which interpreter is running this app's code.
 *
 * Read-only, and the only place a developer can see the answer without reading
 * a build log — which retention eventually takes away. It matters most where a
 * pin is a *range*: `>=3.12,<3.14` honestly resolves to 3.13 today and to 3.12
 * on a rebuild months later, and this is the line that says which one is
 * actually serving.
 *
 * The full version rather than the series the census counts by. An estate needs
 * 3.13 folded together or the count fragments; an owner is asking about their
 * own two releases, where the patch is the difference.
 *
 * Absent is never rendered as "the default". Each of the four things this can
 * be says what it is: a version, a static app that runs no interpreter, a
 * release that recorded none, and an app with no release at all.
 */
function RuntimeVersionBlock({ app, runtime }: { app: App; runtime?: AppRuntime }) {
  // The build is what records it, so an app that has never had one has no
  // answer rather than a missing one — and this page already says so loudly
  // elsewhere. Checked before the states below because `unresolved` would
  // otherwise read as "we built it and did not write the version down".
  const neverDeployed = !app.current_release;

  return (
    <InfoRow
      title="Language and version"
      help="What the serving deployment was built with. A version range can resolve differently on redeploy."
      extra={
        runtime && !neverDeployed && runtime.state !== "resolved" ? (
          <FieldHelp>{runtimeStateExplanation(runtime.state)}</FieldHelp>
        ) : undefined
      }
    >
      {neverDeployed ? (
        <p className="text-xs text-muted-foreground">Nothing deployed yet.</p>
      ) : !runtime ? (
        <p className="text-xs text-muted-foreground">Couldn’t read the version for this app.</p>
      ) : (
        <p className="font-mono text-sm text-foreground">
          {runtime.state === "resolved" ? `${runtime.label} ${runtime.version}` : runtime.label}
        </p>
      )}
    </InfoRow>
  );
}

function ExecutionModeBlock({ appId, app }: { appId: string; app: App }) {
  const { user } = useAuth();
  const queryClient = useQueryClient();
  const isAdmin = user?.system_role === "system_admin";

  // Only admins can read platform settings; for everyone else this stays
  // undefined and the section renders read-only.
  const { data: settings } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
    enabled: isAdmin,
    retry: false,
  });

  const mutation = useMutation({
    mutationFn: (mode: ExecutionMode) => api.updateApp(appId, { execution_mode: mode }),
    onSuccess: (_r, mode) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        mode === "isolated"
          ? "Promoted to isolated. The change takes effect on the next deploy."
          : "Returned to shared. The change takes effect on the next deploy.",
      );
    },
    // The server's 422 explains what would make this possible; showing it
    // verbatim is more useful than any message this component could invent.
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  const isolated = app.execution_mode === "isolated";
  const canPromote = Boolean(settings?.isolated_execution_enabled);
  const blockedReason =
    settings && !settings.isolated_execution_enabled
      ? settings.isolated_blocked_reason ||
        "Isolated execution is off on this install. Turn it on in Admin \u2192 Settings."
      : undefined;

  // Absent, not disabled, where it cannot be used: a shared app on an install
  // that offers no isolation (or whose reader may not choose) has nothing here
  // to act on, and shared is the ordinary case. An isolated app always shows it.
  if (!isolated && !(isAdmin && canPromote)) return null;

  return (
    <FieldRow
      label={
        <>
          Isolated execution{" "}
          {/* The same chip the apps list uses, so the two pages say where
              an app runs in one vocabulary rather than two. */}
          <ModeBadge mode={app.execution_mode} className="ml-1 align-middle" />
        </>
      }
      htmlFor="execution-mode"
      help={executionModeDescription[app.execution_mode]}
      extra={
        <>
          {isAdmin && blockedReason && isolated && <FieldHelp>{blockedReason}</FieldHelp>}
          {!isAdmin && <FieldHelp>Only an administrator can change this.</FieldHelp>}
          {isAdmin && canPromote && !isolated && (
            <FieldHelp>
              Isolated apps cost more and start slower. Use it for background jobs or per-app CPU and memory limits.
            </FieldHelp>
          )}
          {/* The desired mode and the mode the running release was started
              under are different facts, and the gap between them is the whole
              reason changing this setting disturbs nothing. Saying so beats
              leaving someone to wonder why the badge changed and the app did
              not. */}
          {app.running_mode && app.running_mode !== app.execution_mode && (
            <FieldHelp tone="attention">
              The serving deployment runs {app.running_mode}. The change applies on the next deploy.
            </FieldHelp>
          )}
        </>
      }
    >
      <Switch
        id="execution-mode"
        checked={isolated}
        disabled={!isAdmin || (!canPromote && !isolated) || mutation.isPending}
        onCheckedChange={(v) => mutation.mutate(v ? "isolated" : "shared")}
        aria-label="Isolated execution"
      />
    </FieldRow>
  );
}

/**
 * What the app has saved for itself, and the way to throw it away.
 *
 * The app writes one JSON document over `/api/v1/app/data` and it outlives
 * every restart, redeploy and rollback (ADR §36). This section is deliberately
 * *not* a viewer or an editor for it: the app is its only writer, and a second
 * one on this page would be an unversioned editor for state the app believes it
 * owns. What an owner needs is the other three facts — that something is
 * stored, how big it is, when it last changed — and a way out.
 *
 * The way out is the whole reason this exists. An app that saves a document its
 * own startup code cannot read has wedged itself in a way **no redeploy fixes**,
 * because surviving a redeploy is the point of the store; without a Clear button
 * the only remedy is deleting the app.
 *
 * **Shown whether or not anything is stored**, which is where this parts company
 * with StorageSection beside it. That section hides when empty because storage
 * is an administrator's decision and an owner reading about one they do not
 * have can do nothing with the fact. This capability is free to every app and
 * the owner's own code is what turns it on, so an empty state here has a real
 * next step — and "nothing saved" is an answer, where an absent section is
 * indistinguishable from a Toft that does not have the feature.
 */
function SavedDataBlock({ appId, app, canEdit }: { appId: string; app: App; canEdit: boolean }) {
  const [clearOpen, setClearOpen] = useState(false);
  const { data, isLoading } = useQuery({
    queryKey: ["app-data", appId],
    queryFn: () => api.getAppData(appId),
    retry: false,
  });

  // The endpoint is only unreachable on a build older than the feature; nothing
  // is gained by rendering a section that cannot answer.
  if (isLoading || !data) return null;
  const saved = data.version > 0;

  return (
    <>
      {/* A row like the switches above it: what is stored on the left, the
          one act on it on the right. One line of help either way — empty, it
          is the call that fills it, because the capability needs no switch. */}
      <InfoRow
        title="Saved data"
        help={
          saved ? (
            <>
              {formatBytes(data.bytes)}
              {data.updated_at && (
                <>
                  {" "}
                  · saved <RelativeTime iso={data.updated_at} />
                </>
              )}
              . One JSON document the app keeps across redeploys and rollbacks.
            </>
          ) : (
            <>
              Nothing saved yet. The app saves one JSON document with{" "}
              <Code className="text-2xs">toft.data.set(&#123;…&#125;)</Code>; it survives
              redeploys and rollbacks.
            </>
          )
        }
        extra={
          canEdit &&
          saved && (
            <FieldHelp>Clear it if the app can’t read what it saved. A redeploy won’t fix that.</FieldHelp>
          )
        }
      >
        {canEdit && saved && (
          <Button variant="outline" size="sm" onClick={() => setClearOpen(true)}>
            <Trash2 className="h-3.5 w-3.5 mr-1.5" /> Clear
          </Button>
        )}
      </InfoRow>

      <ClearSavedDataDialog app={{ id: appId, name: app.name, slug: app.slug }} open={clearOpen} onOpenChange={setClearOpen} />
    </>
  );
}

/**
 * Type-to-confirm, because nothing keeps a copy.
 *
 * The confirmation policy's top tier: this is irreversible in the strict sense
 * — the platform stores one document per app and the reset deletes it. The copy
 * says what survives, because "clear" beside an app's settings reads to somebody
 * as though it might mean the app.
 */
function ClearSavedDataDialog({
  app,
  open,
  onOpenChange,
}: {
  app: { id: string; name: string; slug: string };
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [confirmText, setConfirmText] = useState("");
  const [clearing, setClearing] = useState(false);

  async function handleClear() {
    setClearing(true);
    try {
      await api.resetAppData(app.id);
      queryClient.invalidateQueries({ queryKey: ["app-data", app.id] });
      toast.success(`Cleared the data ${app.name} had saved`);
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t clear saved data");
    } finally {
      setClearing(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) setConfirmText(""); onOpenChange(o); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Clear the data {app.name} saved?</DialogTitle>
          <DialogDescription>
            Only the app’s saved JSON document is removed, and no copy is kept. Type{" "}
            <ConfirmPhrase text={app.slug} /> to confirm.
          </DialogDescription>
        </DialogHeader>
        <Input
          value={confirmText}
          onChange={(e) => setConfirmText(e.target.value)}
          placeholder={app.slug}
          aria-label={`Type ${app.slug} to confirm clearing this app’s data`}
          className="font-mono"
          autoFocus
        />
        <DialogFooter>
          <Button
            variant="outline"
            disabled={clearing}
            onClick={() => {
              setConfirmText("");
              onOpenChange(false);
            }}
          >
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={confirmText !== app.slug || clearing}
            onClick={handleClear}
          >
            {clearing ? "Clearing…" : "Clear saved data"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Release retention — the owner's half, and the platform's, in one place.
 *
 * The owner decides whether old versions are deleted and how many to keep. An
 * administrator caps the number. When the two disagree the page says so and
 * keeps the owner's figure: it is stored as typed, and applies again if the cap
 * is raised. Refusing the number instead would be tidier to implement and would
 * quietly discard what someone asked for.
 */
function ReleaseRetentionBlock({
  appId,
  app,
  policy,
  field,
}: {
  appId: string;
  app: App;
  policy?: AppPolicy;
  /** "Deployments to keep" is one of the five that batch; the switch above it is
   *  not, which is the split this whole section is a small illustration of. */
  field: DraftField;
}) {
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: (data: { auto_delete_releases?: boolean; releases_kept?: number }) =>
      api.updateApp(appId, data),
    onSuccess: (_r, data) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        data.auto_delete_releases === false
          ? "Automatic cleanup off. Old deployments are kept until you delete them."
          : data.auto_delete_releases
            ? "Old deployments are deleted automatically"
            : "Saved",
      );
    },
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  const autoDelete = app.auto_delete_releases;

  return (
    // Two rows, returned bare so the section's `ROWS` rules between them:
    // the switch (which saves on the throw) and the number it governs (which
    // batches through the page's bar).
    <>
      <FieldRow
        label="Delete old deployments automatically"
        htmlFor="auto-delete-releases"
        help={
          autoDelete
            ? "Each deploy deletes deployments beyond the number below, with their build logs."
            : "When off, nothing is deleted. Deploys are refused once the app hits the install’s limit."
        }
        extra={
          <>
            {/* The conflict, stated rather than hidden. The owner's number
                survives: it is what applies again if an administrator raises
                the limit. */}
            {policy?.releases_kept_conflict && (
              <FieldHelp tone="attention">
                You asked to keep {policy.requested_releases_kept}, but the install’s limit is{" "}
                {policy.max_versions_per_app}, so {policy.effective_releases_kept} are kept.
              </FieldHelp>
            )}
            {policy?.deploys_blocked && (
              <p className={`text-xs ${toneText.danger}`}>{policy.blocked_reason}</p>
            )}
            {/* The administrator's limit, shown whether or not it currently
                bites — an owner deciding whether to turn cleanup off needs to
                know what they would be running into. */}
            {policy && policy.max_versions_per_app > 0 && (
              <FieldHelp>
                This install’s limit: {policy.max_versions_per_app} deployments per app
                {policy.versions !== undefined && `. This app has ${policy.versions}`}.
              </FieldHelp>
            )}
          </>
        }
      >
        <Switch
          id="auto-delete-releases"
          checked={autoDelete}
          disabled={mutation.isPending}
          onCheckedChange={(v) => mutation.mutate({ auto_delete_releases: v })}
          aria-label="Delete old deployments automatically"
        />
      </FieldRow>

      {autoDelete && (
        <DraftInput
          label="Deployments to keep"
          field={field}
          className="w-24"
          placeholder={String(policy?.effective_releases_kept ?? 3)}
          hint="Includes the serving deployment. Leave blank for the install’s default."
        />
      )}
    </>
  );
}

/**
 * The app's own hostname: an administrator's to set, an owner's to read.
 *
 * The name and the switch save separately — a name usually exists before its
 * DNS does — and both on their own, with no dialog: either change is
 * reversible, and the move it queues is a rebuild of the code already running.
 */
function HostnameBlock({ appId, app }: { appId: string; app: App }) {
  const queryClient = useQueryClient();
  const { data: user } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const isAdmin = user?.system_role === "system_admin";
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const value = draft ?? app.hostname ?? "";

  const mutation = useMutation({
    mutationFn: (body: { hostname?: string; hostname_enabled?: boolean }) => api.updateApp(appId, body),
    onSuccess: (_r, body) => {
      setDraft(null);
      setError(null);
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      if (body.hostname_enabled === true) toast.success("This app is moving to its hostname");
      else if (body.hostname_enabled === false) toast.success("This app is moving back to its Toft address");
      else toast.success("Saved");
    },
    onError: (e: Error) => {
      setError(e.message);
      toast.error(e.message, { duration: 8000 });
    },
  });

  // Nothing to say to an owner whose app has none.
  if (!isAdmin && !app.hostname) return null;

  const dirty = draft !== null && draft.trim() !== (app.hostname ?? "");
  // Two rows: the name, and the switch that serves the app at it. A bare
  // switch at the end of a text field reads as "enable this field", and its
  // only name was an aria-label nobody sighted could read.
  return (
    <>
      <FieldRow
        label="Hostname"
        htmlFor="app-hostname"
        help="Recorded here, served only once switched on below. Changing it rebuilds the app."
        extra={
          <>
            {error && <FieldError field="app-hostname">{error}</FieldError>}
            {!isAdmin && <FieldHelp>Only an administrator can change this.</FieldHelp>}
          </>
        }
      >
        <div className="flex w-full items-center gap-2">
          <Input
            id="app-hostname"
            value={value}
            placeholder="reports.example.com"
            readOnly={!isAdmin}
            aria-invalid={!!error}
            aria-describedby={error ? fieldErrorId("app-hostname") : undefined}
            onChange={(e) => setDraft(e.target.value)}
            className="flex-1 font-mono text-xs md:text-xs"
          />
          {isAdmin && dirty && (
            <Button
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => mutation.mutate({ hostname: value.trim() })}
            >
              {mutation.isPending ? "Saving…" : "Save"}
            </Button>
          )}
        </div>
      </FieldRow>
      <FieldRow
        label="Serve at this hostname"
        htmlFor="app-hostname-enabled"
        help={
          !app.hostname
            ? "Save a hostname first, and point its DNS here."
            : app.hostname_enabled
              ? "Served at the name’s root; the old address redirects. Switching rebuilds the app."
              : "When off, the app answers at its /apps/ address only. Switching rebuilds the app."
        }
      >
        <Switch
          id="app-hostname-enabled"
          checked={!!app.hostname_enabled}
          disabled={!isAdmin || !app.hostname || dirty || mutation.isPending}
          onCheckedChange={(v) => mutation.mutate({ hostname_enabled: v })}
        />
      </FieldRow>
    </>
  );
}

/**
 * Idle sleep, as it applies to this one app.
 *
 * Read-only to an owner and settable by an administrator, and it names *which*
 * exemption is in force rather than only that one is. The difference is
 * actionable: an isolated app is awake because of where it runs, and an
 * administrator's exemption is that administrator's to lift.
 */
function SleepBlock({ appId, app, policy }: { appId: string; app: App; policy?: AppPolicy }) {
  const queryClient = useQueryClient();
  const { data: user } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const isAdmin = user?.system_role === "system_admin";

  const mutation = useMutation({
    mutationFn: (neverSleep: boolean) => api.updateApp(appId, { never_sleep: neverSleep }),
    onSuccess: (_r, neverSleep) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        neverSleep
          ? "This app stays running between visits"
          : "This app follows the install’s idle timeout again",
      );
    },
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  // Nothing to say on an install where nobody sleeps: an optional capability
  // that is off is absent, not shown greyed out.
  if (!policy || policy.idle_sleep_minutes <= 0) return null;

  const EXEMPTION_COPY: Record<string, string> = {
    isolated: "Isolated apps don’t sleep on this install.",
    admin: "An administrator has exempted this app. It stays running between visits.",
  };

  return (
    // The label names the switch, and the line under it says what applies
    // and who decided. "Sleeps when idle" over a switch that was off read as
    // an app that does not sleep, while it slept on the install's timeout.
    <FieldRow
      label="Never sleep"
      htmlFor="never-sleep"
      help={
        policy.sleeps
          ? policy.sleep_window
            ? `Sleeps after ${policy.idle_sleep_minutes} minutes without a request, only between ${policy.sleep_window}. Woken when that window ends, or by the next visit.`
            : `Sleeps after ${policy.idle_sleep_minutes} minutes without a request — this install’s idle timeout. The next visit wakes it.`
          : (policy.sleep_exemption && EXEMPTION_COPY[policy.sleep_exemption]) ||
            "This app is not slept."
      }
      extra={!isAdmin && <FieldHelp>Only an administrator can change this.</FieldHelp>}
    >
      {/* The switch reads "never sleep", so it is on exactly when the
          administrator's exemption is. It is disabled — not hidden — for the
          other exemptions, because the reason beside it explains it and a
          vanishing control would read as a bug. */}
      <Switch
        id="never-sleep"
        checked={app.never_sleep}
        disabled={!isAdmin || mutation.isPending || policy.sleep_exemption === "isolated"}
        onCheckedChange={(v) => mutation.mutate(v)}
        aria-label="Never sleep"
      />
    </FieldRow>
  );
}

/**
 * This app's own memory ceiling.
 *
 * Read-only to an owner and settable by an administrator, for a sharper reason
 * than idle sleep: a ceiling an app can raise for itself is not a ceiling. The
 * owner still sees the number, because "why does this keep restarting" is their
 * question and the answer is on this page.
 *
 * Empty means the install's figure, never "uncapped" — there is no value here
 * that takes the cap off, and the copy says which number is standing in.
 * MB or GB, because megabytes fits a 256 MB cap and gigabytes fits a 4 GB one.
 */
function MemoryLimitBlock(
  { appId, app, policy }: { appId: string; app: App; policy?: AppPolicy },
) {
  const queryClient = useQueryClient();
  const { data: user } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const isAdmin = user?.system_role === "system_admin";

  const [unit, setUnit] = useState<"MB" | "GB">(() =>
    app.memory_limit_mb >= 1024 && app.memory_limit_mb % 1024 === 0 ? "GB" : "MB",
  );
  const stored = (u: "MB" | "GB") =>
    app.memory_limit_mb === 0
      ? ""
      : String(u === "GB" ? Math.round((app.memory_limit_mb / 1024) * 100) / 100 : app.memory_limit_mb);
  const [draft, setDraft] = useState(() => stored(
    app.memory_limit_mb >= 1024 && app.memory_limit_mb % 1024 === 0 ? "GB" : "MB",
  ));

  const mutation = useMutation({
    mutationFn: (mb: number) => api.updateApp(appId, { memory_limit_mb: mb }),
    onSuccess: (_r, mb) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        mb === 0
          ? "This app follows the install’s memory limit again"
          : "Saved. Applies the next time this app starts",
      );
    },
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  const typed = draft.trim();
  const asMB = typed === "" ? 0 : Math.round(Number(typed) * (unit === "GB" ? 1024 : 1));
  const broken = typed !== "" && (!Number.isFinite(Number(typed)) || asMB < 64 || asMB > 1048576);
  const dirty = asMB !== app.memory_limit_mb;
  const held = enforcementPhrase(useLimitFacts(appId)?.memory_limit.enforcement);

  return (
    // The platform figure is printed rather than implied — `/system/settings`,
    // where it lives, 404s to the owner reading this — and comes on the app's
    // read-only policy block.
    <FieldRow
      label="Memory limit"
      htmlFor="memory-limit"
      help={
        <>
          {app.memory_limit_mb > 0
            ? "Going over it stops the process with no error in its log."
            : (policy?.platform_memory_limit_mb ?? 0) > 0
              ? `Blank uses the install default of ${formatMB(policy!.platform_memory_limit_mb)}.`
              : "Blank uses the install default."}
          {app.memory_limit_mb > 0 && (policy?.platform_memory_limit_mb ?? 0) > 0 &&
            ` The install default is ${formatMB(policy!.platform_memory_limit_mb)}.`}
          {held && ` ${held}`}
          {isAdmin
            ? " Applies on the next start."
            : " Only an administrator can change this."}
        </>
      }
    >
      <div className="flex flex-col items-stretch gap-1 sm:items-end">
        <div className="flex items-center gap-2">
          <Input
            id="memory-limit"
            inputMode="decimal"
            className="w-24"
            // A bare figure in the unit beside it: "200 MB" in front of an
            // "MB" picker said the unit twice, and in GB said the wrong one.
            placeholder={
              (policy?.platform_memory_limit_mb ?? 0) > 0
                ? String(
                    unit === "GB"
                      ? +(policy!.platform_memory_limit_mb / 1024).toFixed(2)
                      : policy!.platform_memory_limit_mb,
                  )
                : "Default"
            }
            value={draft}
            disabled={!isAdmin || mutation.isPending}
            aria-invalid={broken}
            aria-describedby={broken ? fieldErrorId("memory-limit") : undefined}
            onChange={(e) => setDraft(e.target.value)}
          />
          <Select
            value={unit}
            disabled={!isAdmin || mutation.isPending}
            onValueChange={(v) => {
              const next = v as "MB" | "GB";
              setUnit(next);
              setDraft(stored(next));
            }}
          >
            <SelectTrigger className="w-20" aria-label="Unit">
              <SelectValue>{() => unit}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="MB">MB</SelectItem>
              <SelectItem value="GB">GB</SelectItem>
            </SelectContent>
          </Select>
          {/* Only once edited — the self-saving-number rule. */}
          {dirty && (
            <Button
              size="sm"
              variant="outline"
              disabled={!isAdmin || mutation.isPending || broken}
              onClick={() => mutation.mutate(asMB)}
            >
              {mutation.isPending ? "Saving…" : "Save"}
            </Button>
          )}
        </div>
        {broken && (
          <FieldError field="memory-limit">
            Must be between 64 MB and 1,024 GB.
          </FieldError>
        )}
      </div>
    </FieldRow>
  );
}

/**
 * How this app's limits are held, and its last hour at the CPU limit — from
 * the consumption read, which every reader of the app may make.
 */
function useLimitFacts(appId: string) {
  const { data } = useQuery({
    queryKey: ["app-consumption", appId, 1],
    queryFn: () => api.getAppConsumption(appId, 1),
  });
  return data;
}

/**
 * This app's own CPU limit, by the memory limit's rules: an administrator's,
 * read-only to an owner, empty meaning the install's figure. An amount of CPU
 * time — one core is a second of CPU every second, on any of the machine's
 * cores — never a set of cores.
 */
function CPULimitBlock(
  { appId, app, policy }: { appId: string; app: App; policy?: AppPolicy },
) {
  const queryClient = useQueryClient();
  const { data: user } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const isAdmin = user?.system_role === "system_admin";
  const facts = useLimitFacts(appId);
  const stored = app.cpu_limit_millicores > 0 ? String(app.cpu_limit_millicores / 1000) : "";
  const [draft, setDraft] = useState(stored);

  const mutation = useMutation({
    mutationFn: (m: number) => api.updateApp(appId, { cpu_limit_millicores: m }),
    onSuccess: (_r, m) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["app-consumption", appId] });
      toast.success(
        m === 0
          ? "This app follows the install’s CPU limit again"
          : "Saved. Applies the next time this app starts",
      );
    },
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  const typed = draft.trim();
  const asMillicores = typed === "" ? 0 : Math.round(Number(typed) * 1000);
  const broken =
    typed !== "" && (!Number.isFinite(Number(typed)) || asMillicores < 50 || asMillicores > 64000);
  const dirty = asMillicores !== app.cpu_limit_millicores;
  const platform = policy?.platform_cpu_limit_millicores ?? 0;
  const hour = throttledHour(facts?.cpu_throttled_last_hour);
  const held = enforcementPhrase(facts?.cpu_limit.enforcement);

  return (
    <FieldRow
      label="CPU limit"
      htmlFor="cpu-limit"
      help={
        <>
          {app.cpu_limit_millicores > 0
            ? "Over it the app is slowed, not restarted."
            : platform > 0
              ? `Blank uses the install default of ${formatMillicores(platform)}.`
              : "Blank leaves it unlimited, the install default."}
          {held && ` ${held}`}
          {hour && ` Held at it for ${hour.held} of ${hour.work} minutes of work in the last hour (${hour.percent}%).`}
          {isAdmin ? " Applies on the next start." : " Only an administrator can change this."}
        </>
      }
    >
      <div className="flex flex-col items-stretch gap-1 sm:items-end">
        <div className="flex items-center gap-2">
          <Input
            id="cpu-limit"
            inputMode="decimal"
            className="w-24"
            placeholder={platform > 0 ? String(platform / 1000) : "Default"}
            value={draft}
            disabled={!isAdmin || mutation.isPending}
            aria-invalid={broken}
            aria-describedby={broken ? fieldErrorId("cpu-limit") : undefined}
            onChange={(e) => setDraft(e.target.value)}
          />
          <span className="w-20 text-sm text-muted-foreground">cores</span>
          {dirty && (
            <Button
              size="sm"
              variant="outline"
              disabled={!isAdmin || mutation.isPending || broken}
              onClick={() => mutation.mutate(asMillicores)}
            >
              {mutation.isPending ? "Saving…" : "Save"}
            </Button>
          )}
        </div>
        {broken && (
          <FieldError field="cpu-limit">
            Must be between 0.05 and 64 cores.
          </FieldError>
        )}
      </div>
    </FieldRow>
  );
}

/**
 * Whether this app may learn who is looking at it.
 *
 * On the app's own settings rather than the install's integrations page,
 * because an owner has to be able to see its state and this is where they will
 * look. Rendered disabled for anyone who is not an administrator, with copy
 * that says so — a control somebody can see and cannot move has to explain
 * itself, or it reads as a bug.
 *
 * The copy's job is to say what being on *discloses*, and to whom. "Viewer
 * identity" on its own sounds like a setting; "this app can see the name, email
 * address and group memberships of everyone who opens it" is the decision an
 * administrator is actually taking.
 */
function ViewerIdentityBlock({ appId, app, policy }: { appId: string; app: App; policy?: AppPolicy }) {
  const queryClient = useQueryClient();
  const { data: user } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const isAdmin = user?.system_role === "system_admin";

  const mutation = useMutation({
    mutationFn: (on: boolean) => api.updateApp(appId, { viewer_identity: on }),
    onSuccess: (_r, on) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        on
          ? "This app can now see who is viewing it"
          : "This app can no longer see who is viewing it",
      );
    },
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  // Absent for a viewer, who is not shown the setting at all — the field is
  // simply not in the policy block for them.
  if (!policy || policy.viewer_identity === undefined) return null;
  const on = policy.viewer_identity;
  // A static app and a rendered document are served as files: no process is
  // there to ask who is looking, so the server refuses turning it on
  // (static_app, document_app). Off stays reachable for one already on.
  const servedAsFiles = app.framework === "static" || isRerenderable(app.framework);
  const cannotTurnOn = servedAsFiles && !on;

  return (
    <FieldRow
      label="Can see who is viewing"
      htmlFor="viewer-identity"
      help={
        on
          ? "The app’s code can read the name, email and groups of everyone who opens it."
          : "When off, the app’s code sees every visitor as anonymous."
      }
      extra={
        <>
          {app.visibility === "public" && (
            <FieldHelp>Email addresses are withheld while this app is public.</FieldHelp>
          )}
          {servedAsFiles && (
            <FieldHelp>
              {app.framework === "static" ? "A static app" : "A rendered document"} is served as files,
              with no process to ask who is viewing it.
            </FieldHelp>
          )}
          {!isAdmin && !servedAsFiles && <FieldHelp>Only an administrator can change this.</FieldHelp>}
        </>
      }
    >
      {/* Disabled rather than hidden for a non-administrator: the owner needs
          to see whether it is on to know why toft.who() is failing, and a
          vanishing control would read as a bug. */}
      <Switch
        id="viewer-identity"
        checked={on}
        disabled={!isAdmin || cannotTurnOn || mutation.isPending}
        onCheckedChange={(v) => mutation.mutate(v)}
        aria-label="Can see who is viewing"
      />
    </FieldRow>
  );
}

/**
 * Whether this app's people are emailed when it breaks.
 *
 * Deliberately not an administrator's switch, unlike idle sleep: an exemption
 * from sleep spends the operator's shared memory, and this spends nothing. It
 * belongs to whoever maintains the app.
 *
 * The copy has one job beyond the label — saying that turning it off changes
 * only the email. Every failure is still recorded and still listed on the
 * Logs tab, and an owner who thought otherwise would be switching off the
 * wrong thing.
 */
function FailureNotificationBlock({ appId, app }: { appId: string; app: App }) {
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: (on: boolean) => api.updateApp(appId, { notify_failures: on }),
    onSuccess: (_r, on) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(
        on
          ? "This app’s owners and editors will be emailed when it breaks"
          : "Failure email is off for everyone on this app. Failures still appear on the Logs tab.",
      );
    },
    onError: (error: Error) => toast.error(error.message, { duration: 8000 }),
  });

  return (
    // Not "Email me": the switch is the app's, everyone who maintains it is
    // emailed, and turning it off turns it off for all of them.
    <FieldRow
      label="Email this app’s owners and editors when it breaks"
      htmlFor="notify-failures"
      help="Failed deploys, broken credentials, crashes and failed jobs. One email per problem."
    >
      <Switch
        id="notify-failures"
        checked={app.notify_failures}
        disabled={mutation.isPending}
        onCheckedChange={(v) => mutation.mutate(v)}
        aria-label="Email this app’s maintainers when it breaks"
      />
    </FieldRow>
  );
}

/**
 * Where this app came from, when it came from the gallery.
 *
 * The five states this can be in are distinguished rather than collapsed,
 * because each wants a different sentence and three of them are easy to
 * mistake for "up to date": an item withdrawn from the catalog, two versions
 * that cannot be compared, and an example, which has no update channel at all
 * and never will.
 *
 * Updating is an administrator's action and deploys onto *this* app, so the
 * usual deploy machinery — release history, rollback — is what repairs a bad
 * one. Detaching is consequential but recoverable, so it confirms in a dialog
 * that says what survives rather than asking anyone to type a slug.
 */
function GalleryBlock({ appId, policy }: { appId: string; policy?: AppPolicy }) {
  const { user } = useAuth();
  const isAdmin = user?.system_role === "system_admin";
  const queryClient = useQueryClient();
  const [confirmDetach, setConfirmDetach] = useState(false);
  const gallery = policy?.gallery;

  const update = useMutation({
    mutationFn: () => api.updateFromGallery(appId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["deployments", appId] });
      queryClient.invalidateQueries({ queryKey: ["gallery"] });
      toast.success(`Updating to ${gallery?.catalog_version}`);
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const detach = useMutation({
    mutationFn: () => api.detachFromGallery(appId),
    onSuccess: (result) => {
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["gallery"] });
      setConfirmDetach(false);
      toast.success(result.message);
    },
    onError: (err: Error) => toast.error(err.message),
  });

  if (!gallery) return null;

  const isExtension = gallery.kind === "extension";

  return (
    <>
      <SubBlock
        title="Gallery"
        className="border-t border-border/60 pt-4"
        description={
          isExtension
            ? "A gallery extension. Redeploy builds the version the gallery lists now."
            : "Deployed from a gallery example. It’s yours to change."
        }
      >
        <dl className="space-y-1.5 text-xs">
          <div className="flex justify-between gap-3">
            <dt className="text-muted-foreground">Item</dt>
            <dd className="truncate">{gallery.name || gallery.item_id}</dd>
          </div>
          <div className="flex justify-between gap-3">
            <dt className="text-muted-foreground">Running</dt>
            <dd className="font-mono">
              {gallery.installed_version ||
                (gallery.version_not_recorded ? "not recorded" : "not built yet")}
            </dd>
          </div>
          {gallery.source && (
            <div className="flex justify-between gap-3">
              <dt className="text-muted-foreground">Source</dt>
              <dd className="truncate font-mono" title={gallery.source}>
                {gallery.source}
              </dd>
            </div>
          )}
        </dl>

        {gallery.retired && (
          <Notice>
            This item is no longer in the gallery. The app keeps running, but updates can’t be checked.
          </Notice>
        )}

        {gallery.version_unknown && !gallery.retired && (
          <Notice>
            {gallery.version_not_recorded
              ? `This app’s gallery version wasn’t recorded. Redeploy to install ${gallery.catalog_version} and record it.`
              : `Can’t compare the running version with the gallery’s (${gallery.catalog_version}).`}
          </Notice>
        )}

        {gallery.update_available && (
          <Notice
            action={
              isAdmin ? (
                <Button size="sm" disabled={update.isPending} onClick={() => update.mutate()}>
                  {update.isPending ? "Starting…" : `Update to ${gallery.catalog_version}`}
                </Button>
              ) : undefined
            }
          >
            Version <span className="font-mono">{gallery.catalog_version}</span> is available.
            {isAdmin
              ? " Updating is a deploy like any other, so you can roll back."
              : " An administrator applies it."}
          </Notice>
        )}

        <div>
          <Button
            size="sm"
            variant="outline"
            disabled={detach.isPending}
            onClick={() => setConfirmDetach(true)}
          >
            Unlink from the gallery
          </Button>
          <p className="mt-1.5 text-xs text-muted-foreground">
            {isExtension
              ? "Stops gallery updates and lets this app track a branch of its own."
              : "Removes the record of which example this came from. Nothing else changes."}
          </p>
        </div>
      </SubBlock>

      <Dialog open={confirmDetach} onOpenChange={setConfirmDetach}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Unlink this app from the gallery?</DialogTitle>
            <DialogDescription>
              The app keeps running with its address, settings and history.{" "}
              {isExtension
                ? "It stops getting gallery updates."
                : "The gallery stops counting it as your copy of that example."}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={detach.isPending} onClick={() => setConfirmDetach(false)}>
              Cancel
            </Button>
            <Button disabled={detach.isPending} onClick={() => detach.mutate()}>
              {detach.isPending ? "Unlinking…" : "Unlink"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

/**
 * A pointer, not a second copy of the setting.
 *
 * Where this app's messages go is chosen on the Integrations tab, which is the
 * one editable home for it — the same destination carries both Toft's
 * notices about this app and the app's own messages, and two editable copies of
 * one setting is how they come to disagree.
 *
 * It still says the thing ADR §20 requires: a platform decision about this app
 * is visible on this app. An administrator having switched an integration off
 * is not something an owner should have to infer from silence.
 */
function IntegrationsBlock({ appId, policy }: { appId: string; policy?: AppPolicy }) {
  const rows = policy?.integrations ?? [];
  if (rows.length === 0) return null;
  // One block, one row per integration, and the pointer said once. It was a
  // card per integration each ending "Change it on the Integrations tab.",
  // which on an app with three said it three times.
  return (
    <SubBlock
      title="Also sends through"
      description={
        <>
          Change these on the{" "}
          <Link to={`/apps/${appId}/integrations`} className="underline underline-offset-2 hover:text-foreground">
            Integrations tab
          </Link>
          .
        </>
      }
    >
      <ul className="space-y-1.5 text-sm">
        {rows.map((row) => (
          <li key={row.kind} className="flex flex-wrap items-baseline gap-x-2">
            <span className="font-medium text-foreground">{row.title}</span>
            <span className="text-xs text-muted-foreground">
              {row.enabled
                ? row.destination ?? ""
                : "Turned off for this app by an administrator"}
            </span>
          </li>
        ))}
      </ul>
    </SubBlock>
  );
}

// ── Credentials an administrator attached ───────────────────

/**
 * What an administrator gave this app, and nothing more.
 *
 * Read-only, and **absent on an app with none** — an install that has never
 * used the feature sees no section at all. The keys are named because an owner
 * debugging a connection failure must not have to guess where SNOWFLAKE_USER
 * came from, and because without them they would re-declare it and be refused
 * by a rule they cannot see.
 *
 * There is no value here and no way to reveal one: none is returned by any
 * route, to anybody. The copy says who to ask rather than offering a control
 * that would only ever refuse.
 */
function AttachedCredentials({ appId }: { appId: string }) {
  const { data } = useQuery({
    queryKey: ["app", appId],
    queryFn: () => api.getApp(appId),
  });
  const credentials = data?.credentials ?? [];
  if (credentials.length === 0) return null;

  return (
    <SubBlock
      title="Credentials"
      description="Set by an administrator. The app gets them as environment variables."
    >
      <TableCard className="divide-y divide-border/60" flat>
        {credentials.map((c) => (
          <div key={c.id} className="px-3 py-2">
            <div className="flex flex-wrap items-baseline gap-x-2">
              <span className="text-sm font-medium text-foreground">{c.name}</span>
              {c.description && (
                <span className="text-xs text-muted-foreground">{c.description}</span>
              )}
            </div>
            <p className="mt-0.5 font-mono text-2xs text-muted-foreground">
              {c.keys.join(", ")}
            </p>
          </div>
        ))}
      </TableCard>
      <p className="text-xs text-muted-foreground">
        Ask an administrator to change or remove one. Detaching restarts this app.
      </p>
    </SubBlock>
  );
}

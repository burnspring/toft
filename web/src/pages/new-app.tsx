import { useState } from "react";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { toneText } from "@/lib/tone";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@/lib/api";
import type { ExecutionMode, RemovedEntry } from "@/lib/api";
import { executionModeDescription } from "@/lib/execution-mode";
import { ModeBadge } from "@/components/mode-badge";
import { removedSummary } from "@/lib/removed";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { FieldLabel, Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { PageHeader } from "@/components/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import { PublisherAccessGate } from "@/components/publisher-access-gate";
import {
  ArrowLeft, ArrowRight, Plus, X, Rocket, GitBranch, Check,
  Upload, Loader2, Lock, LockOpen,
} from "lucide-react";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { ArchiveDropzone } from "@/components/archive-dropzone";
import { RepositoryPicker } from "@/components/repository-picker";
import { FieldHelp } from "@/components/field-help";
import { Section } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { AppNameFields } from "@/components/app-name-fields";

function slugFromRepoUrl(url: string): string {
  try {
    const cleaned = url.replace(/\.git$/, "");
    const parts = cleaned.split("/");
    return (parts[parts.length - 1] || "")
      .toLowerCase()
      .replace(/[^a-z0-9-]/g, "-")
      .replace(/-+/g, "-")
      .replace(/^-|-$/g, "");
  } catch {
    return "";
  }
}

function slugFromFilename(name: string): string {
  return name
    .replace(/\.(zip|tar\.gz|tgz)$/i, "")
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");
}

type SourceType = "git" | "upload";

export function NewAppPage() {
  const navigate = useNavigate();
  const location = useLocation();
  // What this account may do, asked at the point of need rather than trusted
  // from the session's role claim — a promotion an admin just approved is not
  // in the token until the next sign-in.
  const {
    data: caps,
    isLoading: capsLoading,
    error: capsError,
    refetch: refetchCaps,
  } = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.getCapabilities,
    retry: false,
  });
  const canCreate = caps?.can_create_objects ?? false;
  // Where this install allows code to come from. Read here so the form can say
  // what *would* work rather than refusing after the fact: a developer who
  // pastes a URL and is turned away with no alternative goes to Slack.
  const { data: policy } = useQuery({
    queryKey: ["source-policy-effective"],
    queryFn: api.getEffectiveSourcePolicy,
    retry: false,
  });
  // The connected git hosts this person may point an app at. An install with
  // none gets an empty list and the form below is byte for byte what it always
  // was — absent, not dormant.
  const { data: connectionData } = useQuery({
    queryKey: ["my-git-connections"],
    queryFn: api.listMyGitConnections,
  });
  const connections = connectionData?.connections ?? [];

  const gitAllowed = policy?.git_allowed ?? true;
  const uploadsAllowed = policy?.uploads_enabled ?? true;

  // The install's settings, read only so the mode control can be drawn. This
  // route is administrator-only and 404s for everybody else — which is exactly
  // the population allowed to choose a mode, so no new route is needed and a
  // failed read means no control, which fails closed.
  const { data: platformSettings } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
    retry: false,
  });
  const mayChooseMode = Boolean(
    platformSettings?.isolated_possible && platformSettings?.isolated_execution_enabled,
  );
  // What this install would make of a create that names no mode, for this
  // caller. From the capabilities response rather than the settings one,
  // because a developer who may not read the settings is still about to make
  // an app in whatever this says.
  const bornAs: ExecutionMode = caps?.new_app_execution_mode ?? "shared";
  // Null until somebody actually decides, which is what keeps the field off
  // the request for everyone who did not.
  const [chosenMode, setChosenMode] = useState<ExecutionMode | null>(null);
  const executionMode: ExecutionMode = chosenMode ?? bornAs;

  const [chosenSource, setChosenSource] = useState<SourceType>("git");
  // Derived rather than synced: with only one source permitted there is nothing
  // to choose, and an effect that corrected the state after the fact would
  // render one frame of a form nobody may submit.
  const sourceType: SourceType = !gitAllowed ? "upload" : !uploadsAllowed ? "git" : chosenSource;
  const setSourceType = setChosenSource;
  const [repoUrl, setRepoUrl] = useState("");
  const [branch, setBranch] = useState("main");
  const [subdir, setSubdir] = useState("");
  const [gitToken, setGitToken] = useState("");
  // Seeded from the query, which is where the picker's sign-in lands: the host
  // redirects to the callback, the callback redirects back here naming the
  // connection, and the form re-opens on the picker rather than on a blank
  // choice the person already made.
  const [searchParams, setSearchParams] = useSearchParams();
  // Which connected git host this app clones through, or "" for none.
  //
  // Choosing one is what makes the token field go away: an app on a connection
  // carries no credential of its own, which is the whole promise. The URL field
  // stays either way — a connection with a repository picker still lets a
  // repository be typed, and a picked one meets exactly the same check.
  const [connectionId, setConnectionId] = useState(searchParams.get("connection") ?? "");
  // Derived *after* the state it reads. A `const` used above its declaration is
  // a temporal-dead-zone error at runtime and nothing the type-checker or the
  // linter objects to — the page renders the error boundary and says
  // "Cannot access 'M' before initialization".
  const connection = connections.find((c) => c.id === connectionId);
  const [archiveFile, setArchiveFile] = useState<File | null>(null);

  const [appName, setAppName] = useState("");
  const [slug, setSlug] = useState("");
  const [envVars, setEnvVars] = useState<{ key: string; value: string; sensitive: boolean }[]>([{ key: "", value: "", sensitive: false }]);
  const [deploying, setDeploying] = useState(false);
  // A hundred-megabyte archive takes a minute, and a reload during it loses
  // the upload with nothing said. The browser's own prompt is the only thing
  // that can ask at that point — and now that the step survives a reload, so
  // is it the only thing that can ask about the rest of the form.
  useUnloadGuard(
    deploying ||
      repoUrl.trim() !== "" ||
      archiveFile !== null ||
      appName.trim() !== "" ||
      envVars.some((v) => v.key.trim() !== "" || v.value.trim() !== ""),
    deploying ? "This app is still being created." : "There is unsaved work on this form.",
  );
  const [deployError, setDeployError] = useState("");
  const queryClient = useQueryClient();

  /**
   * Names typed twice.
   *
   * The variables go out as one object, so two rows sharing a name cannot both
   * survive the create: the later silently wins and the earlier is never
   * stored. The app's own settings form refuses the same thing for the same
   * reason — this is the other door onto one decision.
   */
  const envKeysTyped = envVars.map((v) => v.key.trim()).filter(Boolean);
  const duplicateEnvKeys = new Set(envKeysTyped.filter((k, i) => envKeysTyped.indexOf(k) !== i));

  /**
   * The name derived from the source, and it follows the source until somebody
   * types over it.
   *
   * `if (!appName)` only filled a blank one, so going Back, pasting a different
   * repository and coming forward again left the *first* repository's name on
   * an app about to be created from the second — the one case where the
   * derivation is most likely to be wrong and least likely to be re-read.
   * `derivedName` remembers what was suggested, so a name the person typed is
   * never overwritten and a name this page invented always is.
   */
  const [derivedName, setDerivedName] = useState("");

  function handleNextFromSource() {
    const derived =
      sourceType === "git"
        ? slugFromRepoUrl(repoUrl)
        : archiveFile
          ? slugFromFilename(archiveFile.name)
          : "";
    if (derived && (appName === "" || appName === derivedName)) {
      setAppName(derived);
      setDerivedName(derived);
    }
    setStep(2);
  }

  async function handleDeploy() {
    setDeploying(true);
    setDeployError("");

    const envObj: Record<string, string> = {};
    const envSensitive: Record<string, boolean> = {};
    envVars.forEach((v) => {
      if (!v.key.trim()) return;
      envObj[v.key] = v.value;
      if (v.sensitive) envSensitive[v.key] = true;
    });
    const hasEnv = Object.keys(envObj).length > 0;

    try {
      let result: { app: { id: string; name: string; slug: string }; removed?: RemovedEntry[] };
      if (sourceType === "upload" && archiveFile) {
        result = await api.uploadApp(archiveFile, {
          name: appName || undefined,
          slug: slug || undefined,
          env_vars: hasEnv ? envObj : undefined,
          env_sensitive: hasEnv ? envSensitive : undefined,
          execution_mode: chosenMode ?? undefined,
        });
      } else {
        result = await api.createApp({
          repo_url: repoUrl,
          branch: branch.trim() || "main",
          repo_subdir: subdir.trim() || undefined,
          name: appName || undefined,
          slug: slug || undefined,
          env_vars: hasEnv ? envObj : undefined,
          env_sensitive: hasEnv ? envSensitive : undefined,
          git_token: connectionId ? undefined : gitToken.trim() || undefined,
          git_connection_id: connectionId || undefined,
          // Only a deliberate choice is sent. Naming a mode is
          // administrator-only, and a request that echoes the default back
          // would be refused for everybody else.
          execution_mode: chosenMode ?? undefined,
        });
      }
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      // The same word the Gallery's deploy says, so both doors onto creating an
      // app give the same feedback. A removal rides along, because the platform
      // changed what was shipped and nobody watching this screen would
      // otherwise know until they read a build log.
      const removed = removedSummary(result.removed);
      const deployingName = `Deploying “${result.app.name || result.app.slug}”`;
      toast.success(removed ? `${deployingName} — ${removed}` : deployingName);
      navigate(`/apps/${result.app.id}`);
    } catch (err) {
      // Said once, in the live Notice beside the button, where it stays while
      // the person decides what to change. A toast saying the same thing was
      // the failure shown twice.
      const message = err instanceof Error ? err.message : "Couldn’t create this app";
      setDeployError(message);
      setDeploying(false);
    }
  }

  function addEnvVar() {
    setEnvVars([...envVars, { key: "", value: "", sensitive: false }]);
  }

  function removeEnvVar(index: number) {
    setEnvVars(envVars.filter((_, i) => i !== index));
  }

  function updateEnvVar(index: number, field: "key" | "value", value: string) {
    const updated = [...envVars];
    updated[index] = { ...updated[index], [field]: value };
    setEnvVars(updated);
  }

  function toggleEnvSensitive(index: number) {
    setEnvVars(envVars.map((v, i) => (i === index ? { ...v, sensitive: !v.sensitive } : v)));
  }

  const canProceedFromSource =
    sourceType === "git" ? repoUrl.trim() !== "" : archiveFile !== null;

  /**
   * The step is in the URL, so the browser Back gesture the rail teaches walks
   * the rail instead of leaving the page and discarding everything typed.
   *
   * Deliberately not `useUrlParam`: each Next has to be a *history* entry, so
   * there is no `{ replace: true }` here and the hook's replace-by-default is
   * the wrong behaviour for this one control.
   *
   * **Back is the browser's Back** when the entry before this one is the step
   * it goes to — which the Next that pushed it records in the history state.
   * Pushing for Back too left the browser's Back going *forward* to the step
   * just left. Arriving some other way (a pasted `?step=3`), it replaces.
   *
   * Derived, never trusted. `?step=3` arrives in pasted URLs, and a clamp back
   * to 1 while the source is incomplete is what stops an empty form reaching
   * the deploy button. Declared below `canProceedFromSource` for the reason
   * `connection` is declared below `connectionId`.
   */
  const requested = Math.min(Math.max(Number(searchParams.get("step")) || 1, 1), 3);
  const step = canProceedFromSource ? requested : 1;
  const setStep = (n: number) =>
    setSearchParams(
      (p) => {
        const o = new URLSearchParams(p);
        o.set("step", String(n));
        return o;
      },
      { state: { wizardFrom: step } },
    );
  const backTo = (n: number) => {
    const from = (location.state as { wizardFrom?: number } | null)?.wizardFrom;
    if (from === n) {
      navigate(-1);
      return;
    }
    setSearchParams(
      (p) => {
        const o = new URLSearchParams(p);
        o.set("step", String(n));
        return o;
      },
      { replace: true },
    );
  };

  const sourceLabel =
    sourceType === "git" ? repoUrl : archiveFile?.name || "—";

  const steps = [
    { number: 1, label: "Source" },
    { number: 2, label: "Configure" },
    { number: 3, label: "Review" },
  ];

  // The gate goes here rather than on the buttons that lead here: this route is
  // the point of need, and it is also the one a bookmark or the command palette
  // can reach without passing a button at all.
  if (!canCreate) {
    return (
      <div className="mx-auto max-w-xl py-4">
        <PageHeader title="New app" />
        {capsLoading ? (
          <Skeleton className="h-64 w-full rounded-lg" />
        ) : capsError ? (
          // `canCreate` is false whenever the read fails, so a request that
          // never landed put a publisher who already has the role in front of a
          // panel offering to ask an administrator for it.
          <QueryError
            title="Your permissions could not be read"
            error={capsError}
            fallback="Creating apps needs the publisher or administrator role."
            onRetry={() => void refetchCaps()}
          />
        ) : (
          <PublisherAccessGate />
        )}
      </div>
    );
  }

  // Both sources switched off. A form that cannot succeed is worse than a
  // sentence explaining who to ask, and this install has deliberately closed
  // every way in — that is a decision to report, not an error to work around.
  if (!gitAllowed && !uploadsAllowed) {
    return (
      <div className="mx-auto max-w-xl py-4">
        <PageHeader title="New app" />
        <Notice tone="info" size="md" title="No app source is allowed on this install">
          Git and archive uploads are both off. Ask an administrator to turn one on.
        </Notice>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-xl py-4">
      <PageHeader title="New app" />
      {/* An ordered list, because it is one: three steps in sequence, and the
          markup was three divs whose only statement of where you are was a
          fill and a font weight. `aria-current="step"` is the fact; the
          `sr-only` word is the one a filled circle with a tick cannot say. */}
      <nav aria-label="Progress" className="mb-8 -mt-4">
        <ol className="flex items-center gap-2">
          {steps.map((s, i) => (
            <li
              key={s.number}
              className="flex items-center gap-2"
              aria-current={step === s.number ? "step" : undefined}
            >
              {/* Completed is filled with a check; current is outlined so the
                  two states read apart at a glance. */}
              <div className={`flex h-7 w-7 items-center justify-center rounded-full text-xs font-medium ${
                step > s.number ? "bg-primary text-primary-foreground" :
                step === s.number ? "border-2 border-primary bg-card text-primary" :
                "bg-muted text-muted-foreground"
              }`}>
                {step > s.number ? <Check className="h-3.5 w-3.5" /> : s.number}
              </div>
              <span className={`text-sm ${step >= s.number ? "text-foreground font-medium" : "text-muted-foreground"}`}>
                {s.label}
                {step > s.number && <span className="sr-only"> (done)</span>}
              </span>
              {i < steps.length - 1 && <div className="mx-0.5 h-px w-3 bg-border sm:mx-2 sm:w-8" />}
            </li>
          ))}
        </ol>
      </nav>

      {/* Said out loud when the step changes. The strip above is a rendering,
          and a reader who cannot see it got no signal at all that the form
          under their cursor had been replaced. */}
      <p aria-live="polite" className="sr-only">
        Step {step} of {steps.length}: {steps[step - 1]?.label}
      </p>

      {step === 1 && (
        <Section title="Source" titleHidden>
          <div className="space-y-5 p-6">
            {/* Source type toggle. A source this install does not permit is
                absent rather than shown-and-disabled: there is nothing for the
                reader to act on, and a greyed tab invites them to try. */}
            <div
              role="group"
              aria-label="Where the code comes from"
              className={`flex gap-2 ${gitAllowed && uploadsAllowed ? "" : "hidden"}`}
            >
              <button
                type="button"
                aria-pressed={sourceType === "git"}
                onClick={() => setSourceType("git")}
                className={`flex flex-1 items-center justify-center gap-2 rounded-md border px-4 py-2.5 text-sm font-medium transition-colors ${
                  sourceType === "git"
                    ? "border-primary bg-primary/5 text-primary"
                    : "border-border text-muted-foreground hover:bg-muted/50"
                }`}
              >
                <GitBranch className="h-4 w-4" />
                Git repository
              </button>
              <button
                type="button"
                aria-pressed={sourceType === "upload"}
                onClick={() => setSourceType("upload")}
                className={`flex flex-1 items-center justify-center gap-2 rounded-md border px-4 py-2.5 text-sm font-medium transition-colors ${
                  sourceType === "upload"
                    ? "border-primary bg-primary/5 text-primary"
                    : "border-border text-muted-foreground hover:bg-muted/50"
                }`}
              >
                <Upload className="h-4 w-4" />
                Upload an archive
              </button>
            </div>

            {sourceType === "git" && (
              <div className="space-y-4">
                {connections.length > 0 && (
                  <div className="space-y-1.5">
                    <Label htmlFor="git-connection">Deploy through</Label>
                    <Select value={connectionId} onValueChange={(v) => setConnectionId(v ?? "")}>
                      <SelectTrigger id="git-connection">
                        <SelectValue>
                          {(v) =>
                            v
                              ? (connections.find((c) => c.id === v)?.display_name ??
                                "Choose a connection")
                              : "This app’s own access token"
                          }
                        </SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="">This app’s own access token</SelectItem>
                        {connections.map((c) => (
                          <SelectItem key={c.id} value={c.id}>
                            {c.display_name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {connection && connection.coverage.length > 0 && (
                      <FieldHelp>
                        Covers{" "}
                        {connection.coverage.map((pattern, i) => (
                          <span key={pattern}>
                            {i > 0 && ", "}
                            <code className="font-mono">{pattern}</code>
                          </span>
                        ))}
                        . Other repositories are refused.
                      </FieldHelp>
                    )}
                  </div>
                )}
                {/* The picker, where the connection can sign this person in.
                    The URL field below survives either way: a repository the
                    picker never lists can still be typed, which is why the
                    picker filters and refuses nothing. */}
                {connection?.picker && (
                  <RepositoryPicker
                    connectionId={connection.id}
                    connectionName={connection.display_name}
                    onPick={(repo) => {
                      setRepoUrl(repo.clone_url.replace(/\.git$/, ""));
                      if (repo.default_branch) setBranch(repo.default_branch);
                    }}
                  />
                )}
                <div className="space-y-1.5">
                  <Label htmlFor="repo-url">Repository URL</Label>
                  <Input id="repo-url" className="font-mono" placeholder="https://github.com/org/repo" value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} />
                  {policy?.mode === "allowlist" && (
                    <FieldHelp>
                      Approved sources:{" "}
                      {(policy.patterns ?? []).length === 0 ? (
                        <span className={toneText.attention}>
                          none yet. Ask an administrator to add the repository.
                        </span>
                      ) : (
                        (policy.patterns ?? []).map((p, i) => (
                          <span key={p}>
                            {i > 0 && ", "}
                            <code className="font-mono">{p}</code>
                          </span>
                        ))
                      )}
                    </FieldHelp>
                  )}
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="branch">Branch</Label>
                  <Input id="branch" className="font-mono" placeholder="main" value={branch} onChange={(e) => setBranch(e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="repo-subdir">Folder (optional)</Label>
                  <Input
                    id="repo-subdir"
                    className="font-mono"
                    placeholder="apps/web"
                    value={subdir}
                    onChange={(e) => setSubdir(e.target.value)}
                    aria-describedby="repo-subdir-help"
                  />
                  <FieldHelp id="repo-subdir-help">
                    For a repository that holds several apps. Blank builds the whole repository.
                  </FieldHelp>
                </div>
                {connectionId ? (
                  <Notice tone="info" size="sm" title="No access token needed">
                    This app clones through {connection?.display_name ?? "the connected host"}.
                  </Notice>
                ) : (
                  <div className="space-y-1.5">
                    <Label htmlFor="git-token">Access token (private repositories)</Label>
                    {/* The placeholder is the shape of the value, like the
                        two fields above it. It used to be the instruction
                        "leave blank for public repositories", set in the mono
                        face — which is the face for what a person copies, so
                        at a glance it read as though the platform expected
                        that literal string. The instruction is a sentence and
                        belongs in the sentence below. */}
                    <Input
                      id="git-token"
                      type="password"
                      placeholder="ghp_…"
                      value={gitToken}
                      onChange={(e) => setGitToken(e.target.value)}
                    />
                    <FieldHelp>
                      A read-access token. Leave blank for a public repository.
                    </FieldHelp>
                  </div>
                )}
              </div>
            )}

            {sourceType === "upload" && (
              <div className="space-y-4">
                <div className="space-y-1.5">
                  <FieldLabel>Archive</FieldLabel>
                  <ArchiveDropzone file={archiveFile} onFile={setArchiveFile} />
                </div>
              </div>
            )}

            <Separator />
            <div className="flex justify-between">
              <Button variant="outline" onClick={() => navigate("/")}>Cancel</Button>
              <Button onClick={handleNextFromSource} disabled={!canProceedFromSource}>
                Next <ArrowRight className="ml-1.5 h-4 w-4" />
              </Button>
            </div>
          </div>
        </Section>
      )}

      {step === 2 && (
        <Section title="Configure" titleHidden>
          <div className="space-y-5 p-6">
            <div className="space-y-4">
              <AppNameFields
                idPrefix="app"
                name={appName}
                onNameChange={setAppName}
                slug={slug}
                onSlugChange={setSlug}
              />
              {/*
                Absent rather than disabled where this install offers no
                isolated mode, and absent entirely for anyone who may not
                choose. A greyed control on a creation form invites somebody to
                go and fix something in the middle of creating an app, and the
                place that owns that sentence is Admin → Settings.
              */}
              {mayChooseMode && (
                <div className="space-y-1.5">
                  <div className="flex items-center justify-between gap-4">
                    <Label htmlFor="execution-mode">Isolated execution</Label>
                    <Switch
                      id="execution-mode"
                      checked={executionMode === "isolated"}
                      onCheckedChange={(on) => setChosenMode(on ? "isolated" : "shared")}
                    />
                  </div>
                  <FieldHelp>
                    {executionModeDescription[executionMode]}
                  </FieldHelp>
                  {executionMode === "isolated" && (
                    <FieldHelp>
                      Costs more. A cold start takes tens of seconds.
                    </FieldHelp>
                  )}
                </div>
              )}
            </div>
            <Separator />
            <div className="space-y-3">
              <div className="flex items-center justify-between">
                <FieldLabel>Environment variables</FieldLabel>
                <Button variant="ghost" size="sm" onClick={addEnvVar} className="h-7 gap-1 text-xs text-muted-foreground">
                  <Plus className="h-3 w-3" /> Add
                </Button>
              </div>
              {envVars.map((envVar, index) => (
                <div key={index} className="flex items-center gap-2">
                  <Input
                    placeholder="KEY"
                    value={envVar.key}
                    onChange={(e) => updateEnvVar(index, "key", e.target.value)}
                    aria-label="Variable name"
                    aria-invalid={duplicateEnvKeys.has(envVar.key.trim())}
                    className="font-mono text-sm"
                  />
                  <Input
                    type={envVar.sensitive ? "password" : "text"}
                    placeholder="value"
                    value={envVar.value}
                    onChange={(e) => updateEnvVar(index, "value", e.target.value)}
                    aria-label={envVar.key.trim() ? `Value of ${envVar.key.trim()}` : "Variable value"}
                    className="font-mono text-sm"
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => toggleEnvSensitive(index)}
                    className="h-9 w-9 shrink-0 p-0 text-muted-foreground"
                    aria-pressed={envVar.sensitive}
                    aria-label={`Mark ${envVar.key.trim() || "variable"} sensitive`}
                    title={envVar.sensitive ? "Sensitive: hidden once saved. Click to make visible." : "Visible in settings. Click to mark sensitive."}
                  >
                    {envVar.sensitive ? <Lock className="h-4 w-4" /> : <LockOpen className="h-4 w-4" />}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => removeEnvVar(index)}
                    className="h-9 w-9 shrink-0 p-0 text-muted-foreground hover:text-destructive"
                    aria-label={envVar.key.trim() ? `Remove ${envVar.key.trim()}` : "Remove variable"}
                  >
                    <X className="h-4 w-4" />
                  </Button>
                </div>
              ))}
              <FieldHelp>
                Use the lock to mark a value sensitive. It is hidden once saved and can’t be shown again.
              </FieldHelp>
              {duplicateEnvKeys.size > 0 && (
                <Notice live tone="danger" title="The same name is used twice">
                  <span className="font-mono">{[...duplicateEnvKeys].join(", ")}</span> appears more
                  than once. Only one value would be kept.
                </Notice>
              )}
            </div>
            <Separator />
            <div className="flex justify-between">
              <Button variant="outline" onClick={() => backTo(1)}>
                <ArrowLeft className="mr-1.5 h-4 w-4" /> Back
              </Button>
              <Button onClick={() => setStep(3)} disabled={duplicateEnvKeys.size > 0}>
                Next <ArrowRight className="ml-1.5 h-4 w-4" />
              </Button>
            </div>
          </div>
        </Section>
      )}

      {step === 3 && (
        <Section title="Review and deploy" titleHidden>
          <div className="space-y-5 p-6">
            <TableCard flat className="p-4">
              <div className="flex flex-col gap-3 text-sm">
                <div className="flex justify-between">
                  <span className="text-muted-foreground">Source</span>
                  <span className="text-foreground">
                    {sourceType === "git" ? "Git repository" : "Archive upload"}
                  </span>
                </div>
                <Separator />
                {/* Wraps rather than truncates: this is the step where the
                    address is checked, and a cut-off URL cannot be. */}
                <div className="flex justify-between gap-4">
                  <span className="shrink-0 text-muted-foreground">
                    {sourceType === "git" ? "Repository" : "File"}
                  </span>
                  <span className="min-w-0 break-all text-right font-mono text-foreground">
                    {sourceLabel}
                  </span>
                </div>
                {sourceType === "git" && (
                  <>
                    <Separator />
                    <div className="flex justify-between">
                      <span className="text-muted-foreground">Branch</span>
                      {/* What will actually be built. A cleared box deploys
                          the default, and the review said nothing where the
                          name of the branch belongs. */}
                      <span className="font-mono text-foreground">
                        {branch.trim() || (
                          <span className="font-sans text-muted-foreground">main (the default)</span>
                        )}
                      </span>
                    </div>
                    {/* Absent for the ordinary case, a whole repository; stated
                        when only a folder of it will be built. */}
                    {subdir.trim() && (
                      <>
                        <Separator />
                        <div className="flex justify-between">
                          <span className="text-muted-foreground">Folder</span>
                          <span className="font-mono text-foreground">{subdir.trim().replace(/^\/+|\/+$/g, "")}</span>
                        </div>
                      </>
                    )}
                  </>
                )}
                <Separator />
                <div className="flex justify-between">
                  <span className="text-muted-foreground">Name</span>
                  {/* A blank name is sent as nothing and the server derives
                      one, so "—" said there would be no name. Each default
                      reads the same way: muted, saying where it comes from. */}
                  <span className="text-foreground">
                    {appName || <span className="text-muted-foreground">derived from the source</span>}
                  </span>
                </div>
                <Separator />
                <div className="flex justify-between">
                  <span className="text-muted-foreground">Slug</span>
                  <span className="font-mono text-foreground">{slug || <span className="font-sans text-muted-foreground">derived from the name</span>}</span>
                </div>
                {/*
                  Only when the app will be isolated. A row reading "Shared" on
                  every creation forever is a permanent advertisement for a
                  distinction most readers do not have — but a developer about
                  to wait tens of seconds longer for a deploy that behaves
                  differently is owed the fact, whether or not they chose it.
                */}
                {executionMode === "isolated" && (
                  <>
                    <Separator />
                    <div className="flex items-start justify-between gap-4">
                      <span className="text-muted-foreground">Mode</span>
                      <span className="flex flex-col items-end gap-1">
                        <ModeBadge mode={executionMode} />
                        {chosenMode === null && (
                          <span className="text-xs text-muted-foreground">
                            set by this install’s default
                          </span>
                        )}
                      </span>
                    </div>
                  </>
                )}
                {envVars.some((v) => v.key.trim()) && (
                  <>
                    <Separator />
                    <div className="flex justify-between">
                      <span className="text-muted-foreground">Environment variables</span>
                      <span className="text-foreground">{envVars.filter((v) => v.key.trim()).length} set</span>
                    </div>
                  </>
                )}
              </div>
            </TableCard>
            {deployError && (
              <Notice live tone="danger" size="md">{deployError}</Notice>
            )}
            <Separator />
            <div className="flex justify-between">
              <Button variant="outline" onClick={() => backTo(2)} disabled={deploying}>
                <ArrowLeft className="mr-1.5 h-4 w-4" /> Back
              </Button>
              <Button onClick={handleDeploy} disabled={deploying}>
                {deploying ? (
                  <>
                    <Loader2 className="mr-1.5 h-4 w-4 animate-spin" />
                    {/* Naming both halves on the upload path, because the
                        archive is what the wait is: a hundred megabytes leaves
                        a button reading "Deploying…" for a minute before
                        anything is being deployed. */}
                    {sourceType === "upload" ? "Uploading and deploying…" : "Deploying…"}
                  </>
                ) : (
                  <>
                    <Rocket className="mr-1.5 h-4 w-4" />
                    Deploy now
                  </>
                )}
              </Button>
            </div>
          </div>
        </Section>
      )}
    </div>
  );
}

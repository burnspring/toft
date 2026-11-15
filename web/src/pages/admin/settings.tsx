import { useEffect, useState } from "react";
import { RelativeTime } from "@/components/relative-time";
import { fieldErrorId } from "@/lib/utils";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { toneText, toneSurface, type Tone } from "@/lib/tone";
import { Link, Navigate, useLocation } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpCircle, RefreshCw, Search } from "lucide-react";
import { toast } from "sonner";

import {
  api,
  kernelHoldsMemory,
  type GalleryResponse,
  type GallerySync,
  type ExecutionMode,
  type PlatformSettings,
  type UpdatePlatformSettings,
} from "@/lib/api";
import { executionModeLabel } from "@/lib/execution-mode";
import { Switch } from "@/components/ui/switch";
import { Input } from "@/components/ui/input";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Button } from "@/components/ui/button";
import { ChoiceButtons } from "@/components/choice-buttons";
import { UnitLabel } from "@/components/unit-label";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { EmptyState } from "@/components/empty-state";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { OtherPagesMatch } from "@/components/other-pages-match";
import { FieldError } from "@/components/field-help";
import { Section } from "@/components/section";
import { SettingRow } from "@/components/setting-row";
import { PendingBar } from "@/components/pending-bar";
import { SectionRail } from "@/components/section-rail";
import { Code } from "@/components/code";
import { FilterTerm } from "@/lib/filter-term";
import { settingHidden } from "@/lib/settings-visibility";
import { formatExactTime } from "@/lib/time";
import { knownTimeZones } from "@/lib/schedule";
import { useAuth } from "@/hooks/use-auth";
import {
  MOVED_ANCHORS,
  SETTING,
  SETTINGS_SECTIONS,
  settingText as entryText,
  settingsIn,
  type SettingsSection,
} from "@/lib/settings-index";
import {
  CONTROL_KIND,
  NUMBER_FIELDS,
  NUMBER_KEYS,
  TEXT_KEYS,
  fieldError,
  isTextKey,
  normalize,
  securityTxtDaysLeft,
  type FieldKey,
  type NumberFieldKey,
  type SettingsPageKey,
  type SwitchKey,
  type TextFieldKey,
} from "@/lib/settings-fields";

/** The display zone's suggestions: UTC first, then this browser's list. */
const TIME_ZONE_SUGGESTIONS = ["UTC", ...knownTimeZones().filter((z) => z !== "UTC")];

/* ------------------------------------------------------------------ *
 * The index
 *
 * Every setting's copy, and every section's, now lives in
 * `lib/settings-index.ts` — install-wide rather than page-scoped. This
 * page is one of its three consumers and no longer its owner: the filter
 * box, the "on this page" rail and the rows all read from there, and so
 * does the command palette, which is what makes a setting on Email or
 * Telemetry findable by name at all.
 *
 * What the page renders, as what control, and the bounds each field is
 * held to are in `lib/settings-fields.ts` beside it — the form contract,
 * which the guard test also reads. Indexing a key the index has never
 * heard of does not compile, because `SETTING` is typed on the ids the
 * index declares.
 * ------------------------------------------------------------------ */

/** Looked up by id rather than by position, so reordering the page cannot
 *  quietly put one section's rows under another's heading. */
const SECTION = Object.fromEntries(SETTINGS_SECTIONS.map((s) => [s.id, s])) as Record<
  string,
  SettingsSection
>;

/* ------------------------------------------------------------------ *
 * The page
 * ------------------------------------------------------------------ */

/**
 * Admin → Settings.
 *
 * Two kinds of thing appear here and the page keeps them visibly apart.
 * What the install *can* do is read-only: the topology, whether a cluster is
 * configured, which runner images exist. An operator sets those in deployment
 * configuration and no button here changes them. What the platform *offers* is
 * editable, and each row says what turning it on commits to.
 *
 * Three decisions shape the layout. Everything lives on one page with a filter
 * box, because an administrator who knows a setting by name should not have to
 * guess which tab hides it. The long reasoning is folded behind a disclosure on
 * each row, so the page can be scanned and still explains itself when asked.
 * And the numbers batch: edits collect in a pending set and go out as one PUT
 * from the bar at the bottom, which is also what makes a half-typed change
 * visible instead of silently lost on the way to another page.
 *
 * The switches deliberately do *not* batch. Each is a single deliberate act
 * with a consequence the server answers with — apps stranded in isolated mode,
 * live keys stopped — and that warning belongs to the toggle that caused it,
 * not to a save of six unrelated things.
 */

export function AdminSettingsPage() {
  const queryClient = useQueryClient();
  const { data: settings, isLoading, error, refetch } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
  });

  const [query, setQuery] = useState("");
  /** Raw text per edited field. A key absent here means "as stored". */
  const [drafts, setDrafts] = useState<Partial<Record<FieldKey, string>>>({});
  const [active, setActive] = useState(SETTINGS_SECTIONS[0].id);
  const { hash } = useLocation();

  // Honour `#section` on arrival — the same hook every indexed page uses, so a
  // `route#anchor` from ⌘K lands on the setting rather than at the top of the
  // page that holds it.
  useHashScroll(!!settings);

  const { refresh: refreshAuth } = useAuth();

  function applyResult(result: { settings: PlatformSettings; warning: string }) {
    queryClient.setQueryData(["platform-settings"], (old: PlatformSettings | undefined) =>
      old ? { ...old, ...result.settings } : old,
    );
    queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
    // The gallery mode lives on this page and decides whether the Gallery
    // exists at all — its nav item, its ⌘K entry, its route. Both of those read
    // `["gallery"]`, which is cached for five minutes, so switching the mode
    // here and then looking for the tab found the old answer for as long as
    // that lasted. Invalidated unconditionally: the query 404s where the
    // gallery is off, which is the answer either way.
    queryClient.invalidateQueries({ queryKey: ["gallery"] });
  }

  // A switch: sent on its own, the moment it is thrown.
  const toggle = useMutation({
    mutationFn: (data: UpdatePlatformSettings) =>
      api.updatePlatformSettings({ ...data, updated_at: settings?.updated_at }),
    onSuccess: (result) => {
      applyResult(result);
      // A warning is not a failure: turning isolated execution off leaves
      // running apps alone, and the admin should be told that plainly.
      if (result.warning) toast.warning(result.warning, { duration: 10_000 });
      else toast.success("Saved");
    },
    onError: (error: Error) => toast.error(error.message),
  });

  // The pending set: everything typed, in one request.
  const save = useMutation({
    mutationFn: (data: UpdatePlatformSettings) =>
      api.updatePlatformSettings({ ...data, updated_at: settings?.updated_at }),
    onSuccess: (result, sent) => {
      applyResult(result);
      setDrafts({});
      // The administrator's own pages read the default too, unless they chose.
      if (sent.display_time_zone !== undefined) void refreshAuth();
      if (result.warning) toast.warning(result.warning, { duration: 10_000 });
      else toast.success("Saved");
    },
    onError: (error: Error) => toast.error(error.message),
  });

  const busy = toggle.isPending || save.isPending;

  /**
   * Above the loading guard, because the filter and the rail below have to read
   * it and one of them feeds a hook. The rule is `settingHidden`.
   */
  function hidden(id: string): boolean {
    return settingHidden(id, settings);
  }

  // Which sections and rows the filter leaves standing. A section matches on
  // its own words too, so "jobs" brings back the whole card rather than
  // only the rows with that word in them.
  const q = query.trim().toLowerCase();

  /**
   * What this install actually puts on screen.
   *
   * The filter and the rail read the *index*, which is every setting the
   * product has, while the page renders a subset of it: the job figures go
   * with background jobs, and the Gallery section only exists where the
   * gallery does. So the rail listed a section that scrolled nowhere and the
   * filter counted rows with no control behind them — an install with jobs off
   * could be told "3 settings match" and shown none of them. One predicate
   * now, read by the page, the rail and the count alike.
   */
  const shownIn = (sectionId: string) => settingsIn(sectionId).filter((e) => !hidden(e.id));
  const sectionExists = (sectionId: string) =>
    sectionId === "gallery" ? (settings?.gallery_available ?? false) : shownIn(sectionId).length > 0;
  const existingSections = SETTINGS_SECTIONS.filter((s) => sectionExists(s.id));

  const sectionText = (s: SettingsSection) =>
    `${s.title} ${s.blurb} ${s.keywords ?? ""} ${shownIn(s.id).map(entryText).join(" ")}`.toLowerCase();
  const matchedSections = q
    ? existingSections.filter((s) => sectionText(s).includes(q))
    : existingSections;
  const visibleSections = new Set(matchedSections.map((s) => s.id));
  const matchesRow = (id: SettingsPageKey, sectionId: string) =>
    !q || settingText(id).toLowerCase().includes(q) || sectionOwnText(sectionId).includes(q);
  const matchCount = q
    ? existingSections.flatMap((s) => shownIn(s.id)).filter((entry) =>
        entryText(entry).toLowerCase().includes(q),
      ).length
    : 0;

  // Scroll-spy for the rail. Keyed on the id list so filtering re-arms it, and
  // read from the DOM rather than from an observer because the answer wanted is
  // "which heading did I last scroll past", which is exactly this.
  const railKey = matchedSections.map((s) => s.id).join(",");
  useEffect(() => {
    const ids = railKey ? railKey.split(",") : [];
    if (ids.length === 0) return;
    function onScroll() {
      let current = ids[0];
      for (const id of ids) {
        const el = document.getElementById(id);
        if (el && el.getBoundingClientRect().top <= 96) current = id;
      }
      setActive(current);
    }
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, [railKey]);

  // An anchor whose section has moved lands where the section went. Six of the
  // thirteen did (ADR §50 D2), and every link to one of them — in the docs, in
  // another page's copy, in a bookmark — still means what it meant. Below the
  // hooks, because a redirect is a render and the hooks above it are not
  // optional.
  const moved = MOVED_ANCHORS[hash.replace(/^#/, "")];
  if (moved) return <Navigate to={moved} replace />;

  // Before the skeleton, and after the redirect. A skeleton is the one state
  // that reads as "still working", so a failed read left twenty-six settings as
  // three grey bars with no message, no Retry and nothing that re-fires --
  // `retry: 1, refetchOnWindowFocus: false` -- so the only escape was guessing
  // that a reload might help.
  if (error) {
    return (
      <div>
        <PageHeader title="Settings" />
        <QueryError
          title="The install’s settings could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  if (isLoading || !settings) {
    return (
      <div>
        <PageHeader title="Settings" />
        <div className="space-y-6">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-40 w-full rounded-lg" />
          ))}
        </div>
      </div>
    );
  }

  const isolatedBlocked = !settings.isolated_possible;

  // A field only counts as pending while it is on screen: editing a job
  // retention figure and then switching jobs off must not leave a change
  // queued behind a row nobody can see. `dormant_lock_days` is not in this
  // list at all any more — its row is on Authentication, and saves itself.
  const liveKeys: FieldKey[] = [...TEXT_KEYS, ...NUMBER_KEYS].filter(
    (k) => k !== "dormant_lock_days" && !hidden(k),
  );

  const draftOf = (key: FieldKey) => drafts[key] ?? String(settings[key]);

  const isDirty = (key: FieldKey) => {
    const raw = drafts[key];
    if (raw === undefined) return false;
    if (isTextKey(key)) return normalize(key, raw) !== settings[key];
    const n = Number(raw.trim());
    if (raw.trim() === "" || !Number.isInteger(n)) return true;
    return n !== settings[key];
  };

  const dirtyKeys = liveKeys.filter(isDirty);
  const brokenKeys = dirtyKeys.filter((k) => fieldError(k, draftOf(k), draftOf) !== null);

  function saveAll() {
    if (dirtyKeys.length === 0 || brokenKeys.length > 0 || busy) return;
    const numbers: Partial<Record<NumberFieldKey, number>> = {};
    const texts: Partial<Record<TextFieldKey, string>> = {};
    for (const key of dirtyKeys) {
      if (isTextKey(key)) texts[key] = normalize(key, draftOf(key));
      else numbers[key] = Number(draftOf(key).trim());
    }
    save.mutate({ ...numbers, ...texts });
  }

  /** Everything a number or text row needs, in one call. */
  function field(key: FieldKey) {
    const value = draftOf(key);
    return {
      id: key,
      value,
      dirty: isDirty(key),
      error: fieldError(key, value, draftOf),
      disabled: busy,
      onChange: (v: string) => setDrafts((d) => ({ ...d, [key]: v })),
      onSubmit: saveAll,
    };
  }

  /** What a row says under its summary, when this install has something to add. */
  function note(id: SettingsPageKey): string | undefined {
    switch (id) {
      case "isolated_execution_enabled":
        if (isolatedBlocked) return settings!.isolated_blocked_reason;
        return settings!.isolated_app_count > 0
          ? `${settings!.isolated_app_count} app${settings!.isolated_app_count === 1 ? "" : "s"} running isolated. Turning this off leaves them running.`
          : undefined;
      case "background_jobs_enabled":
        if (settings!.isolated_execution_enabled) return "Runs execute in the cluster.";
        return settings!.host_jobs_possible
          ? "Runs execute on this server."
          : "Needs isolated execution, which is off above.";
      case "default_execution_mode":
        // Deliberately not `isolated_blocked_reason` here, which the row
        // directly above already prints in full: two identical paragraphs
        // stacked is how a page teaches people to stop reading it. Point up
        // instead, as the jobs switch does.
        if (isolatedBlocked) {
          return "Needs isolated execution, which this install can’t offer.";
        }
        if (!settings!.isolated_execution_enabled) {
          return "Needs isolated execution, which is off above. New apps are created shared.";
        }
        return undefined;
      case "max_versions_per_app":
        return settings!.apps_over_version_cap > 0
          ? `${settings!.apps_over_version_cap} app${
              settings!.apps_over_version_cap === 1 ? " has" : "s have"
            } cleanup off and ${
              settings!.apps_over_version_cap === 1 ? "is" : "are"
            } at this limit. The next deploy is refused.`
          : undefined;
      case "public_base_url":
        // What blank falls back to, named: the field is an override, and
        // without the value under it clearing it was a leap.
        return normalize("public_base_url", draftOf("public_base_url")) === ""
          ? `Using ${settings!.env_base_url}, from BASE_URL.`
          : `Overrides ${settings!.env_base_url}, from BASE_URL.`;
      case "install_name": {
        // The result rather than a description of it: the origin line every
        // message will carry, rendered from the draft as it is typed.
        if (fieldError("install_name", draftOf("install_name"), draftOf)) return undefined;
        const name = normalize("install_name", draftOf("install_name")) || "Toft";
        return `Messages will say: Sent by ${name} at ${settings!.effective_base_url}`;
      }
      case "display_time_zone": {
        // What a time will look like, from the draft as it is typed.
        const raw = draftOf("display_time_zone");
        if (fieldError("display_time_zone", raw, draftOf)) return undefined;
        const zone = normalize("display_time_zone", raw);
        return zone === ""
          ? "Each person sees their own browser’s zone unless they choose one."
          : `Times will read like ${formatExactTime(new Date().toISOString(), /^utc$/i.test(zone) ? "UTC" : zone)} for anyone who hasn’t chosen.`;
      }
      case "environment_tag": {
        const name = normalize("environment_tag", draftOf("environment_tag"));
        return name !== "" && !fieldError("environment_tag", draftOf("environment_tag"), draftOf)
          ? `Telemetry will report deployment.environment = ${name}.`
          : undefined;
      }
      // The Execution notes. They were five notices at the foot of the card,
      // each naming the field it was about; under the field, they need not.
      case "app_memory_limit_mb":
        return Number(draftOf("app_memory_limit_mb").trim()) === 0
          ? `Using the install default: ${
              settings!.env_app_memory_limit_mb > 0
                ? memoryPhrase(settings!.env_app_memory_limit_mb)
                : "no limit"
            }.`
          : undefined;
      case "max_concurrent_builds":
        if (Number(draftOf("max_concurrent_builds").trim()) > settings!.max_concurrent_builds_ceiling) {
          return `The most this install allows is ${settings!.max_concurrent_builds_ceiling}. A larger number can’t be saved.`;
        }
        return deferringNote(id);
      case "auto_deploy_poll_seconds":
        if (!settings!.auto_deploy_poller_running) {
          return "Polling is off for this install, so this has no effect. Webhooks still deploy.";
        }
        return deferringNote(id);
      case "app_cpu_request_millicores":
      case "app_cpu_limit_millicores":
      case "job_cpu_limit_millicores":
        return deferringNote(id);
      case "job_memory_limit_mb":
        return Number(draftOf("job_memory_limit_mb").trim()) === 0
          ? `Using the install default: ${memoryPhrase(settings!.env_job_memory_limit_mb)}.`
          : undefined;
      case "idle_sleep_minutes":
        return Number(draftOf("idle_sleep_minutes").trim()) > 0 && settings!.isolated_app_count > 0
          ? settings!.isolated_app_count === 1
            ? "1 app runs isolated and follows the isolated timeout instead."
            : `${settings!.isolated_app_count} apps run isolated and follow the isolated timeout instead.`
          : undefined;
      case "isolated_sleep_window_enabled":
        return settings!.isolated_sleep_window_enabled &&
          Number(draftOf("isolated_idle_sleep_minutes").trim()) === 0
          ? "Has no effect while isolated apps never sleep."
          : undefined;
      case "isolated_sleep_window_start":
        return settings!.display_time_zone
          ? `In ${settings!.display_time_zone}, this install’s time zone.`
          : "In UTC. Set this install’s time zone to read it in another.";
      default:
        return undefined;
    }
  }

  function noteToneOf(id: SettingsPageKey): Tone | undefined {
    if (id === "max_versions_per_app") return "attention";
    if (
      id === "max_concurrent_builds" &&
      Number(draftOf("max_concurrent_builds").trim()) > settings!.max_concurrent_builds_ceiling
    ) {
      return "attention";
    }
    return undefined;
  }

  /** What a 0 is deferring to, for a dial whose zero defers to a number this
   *  page can show — read from the draft, so it changes as 0 is typed. The
   *  summary already says that 0 defers; the note says to what, never again
   *  that it does. */
  function deferringNote(id: SettingsPageKey): string | undefined {
    const d = deferring.find((x) => x.key === id);
    return d ? `Using the install default: ${d.using}.` : undefined;
  }

  /** Whether the install, rather than the administrator, is what says no. */
  function switchDisabled(id: SwitchKey): boolean {
    if (id === "isolated_execution_enabled") return isolatedBlocked;
    if (id === "background_jobs_enabled") {
      return !settings!.isolated_execution_enabled && !settings!.host_jobs_possible;
    }
    return false;
  }

  /** The control itself, by the kind this page declares the setting to be. */
  function control(id: SettingsPageKey) {
    switch (CONTROL_KIND[id]) {
      case "number":
        return <NumberControl {...field(id as NumberFieldKey)} />;
      case "memory":
        return <MemoryControl {...field(id as NumberFieldKey)} />;
      case "cpu":
        return <CpuControl {...field(id as NumberFieldKey)} />;
      case "text": {
        // Each of the six says what a good value looks like rather than
        // sitting empty: a placeholder is the cheapest documentation on a
        // page of free-text boxes, and `mailto:` is the half everybody
        // forgets.
        const text: Record<TextFieldKey, { width: string; placeholder: string }> = {
          public_base_url: { width: "w-full", placeholder: settings!.env_base_url },
          garth_url: { width: "w-full", placeholder: "https://garth.example.com" },
          environment_tag: { width: "w-full", placeholder: "stage" },
          install_name: { width: "w-full", placeholder: "Toft" },
          display_time_zone: { width: "w-full", placeholder: "Each person’s browser" },
          security_txt_contact: { width: "w-full", placeholder: "mailto:security@example.com" },
          security_txt_expires: { width: "w-full", placeholder: "2027-01-31" },
          security_txt_policy: { width: "w-full", placeholder: "https://example.com/security" },
          security_txt_languages: { width: "w-full", placeholder: "en, de" },
          r_package_repository: {
            width: "w-full",
            placeholder: settings!.env_r_package_repository,
          },
          isolated_sleep_window_start: { width: "w-full", placeholder: "19:00" },
          isolated_sleep_window_end: { width: "w-full", placeholder: "07:00" },
        };
        const spec = text[id as TextFieldKey];
        return (
          <TextControl
            {...field(id as TextFieldKey)}
            width={spec.width}
            placeholder={spec.placeholder}
            suggestions={id === "display_time_zone" ? TIME_ZONE_SUGGESTIONS : undefined}
          />
        );
      }
      // Two live keys, both returning explicitly. There is no fallthrough:
      // `dependency_policy` is a choice too, but it is filed on
      // /admin/dependencies, and a default arm here would let this page draw a
      // control it does not own the moment a third key appears.
      case "choice":
        if (id === "gallery_deploy_mode") {
          return (
            <ModeChoice
              value={settings!.gallery_deploy_mode}
              disabled={busy}
              onChange={(v) => toggle.mutate({ gallery_deploy_mode: v })}
            />
          );
        }
        if (id === "log_level") {
          return (
            <LogLevelChoice
              value={settings!.log_level}
              envLevel={settings!.env_log_level}
              effective={settings!.effective_log_level}
              disabled={busy}
              onChange={(v) => toggle.mutate({ log_level: v })}
            />
          );
        }
        if (id === "default_execution_mode") {
          return (
            <DefaultExecutionModeChoice
              value={settings!.default_execution_mode}
              disabled={busy}
              isolatedOffered={!isolatedBlocked && settings!.isolated_execution_enabled}
              onChange={(v) => toggle.mutate({ default_execution_mode: v })}
            />
          );
        }
        return null;
      case "switch": {
        const key = id as SwitchKey;
        return (
          <Switch
            id={key}
            checked={settings![key]}
            disabled={switchDisabled(key) || busy}
            // A computed key, because the row is generated: the payload is one
            // setting either way, which is the whole contract a switch has.
            onCheckedChange={(v) => toggle.mutate({ [key]: v } as UpdatePlatformSettings)}
          />
        );
      }
    }
  }

  /**
   * One section's rows, in index order.
   *
   * Generated rather than written out, so that being in the index *is* being
   * on the page. The alternative — a card per section with its rows spelled
   * out — is how a setting came to have copy, a place in the filter's count
   * and no control anywhere on screen.
   */
  function rows(sectionId: string) {
    return settingsIn(sectionId)
      .map((entry) => entry.id as SettingsPageKey)
      .filter((id) => !hidden(id))
      .map((id) => (
        <SettingRow
          key={id}
          id={id}
          visible={matchesRow(id, sectionId)}
          note={note(id)}
          noteTone={noteToneOf(id)}
        >
          {control(id)}
        </SettingRow>
      ));
  }

  // The dials whose zero defers to a number this page can show, each naming
  // itself. Assembled here rather than written into the notice as four
  // conditions, because a notice that says "a 0 above" leaves the reader
  // counting rows to work out which field it means.
  const deferring: { key: SettingsPageKey; label: string; using: string }[] = [];
  if (settings) {
    const zero = (k: NumberFieldKey) => !hidden(k) && Number(draftOf(k).trim()) === 0;
    if (zero("app_cpu_request_millicores")) {
      deferring.push({
        key: "app_cpu_request_millicores",
        label: "CPU reserved per app",
        using: `${settings.env_app_cpu_request_cores} core${settings.env_app_cpu_request_cores === 1 ? "" : "s"}`,
      });
    }
    if (zero("app_cpu_limit_millicores")) {
      deferring.push({
        key: "app_cpu_limit_millicores",
        label: "CPU limit per app",
        using:
          settings.env_app_cpu_limit_millicores > 0
            ? `${settings.env_app_cpu_limit_cores} core${settings.env_app_cpu_limit_cores === 1 ? "" : "s"}`
            : "this install sets no CPU limit, so apps are not throttled",
      });
    }
    if (zero("job_cpu_limit_millicores")) {
      deferring.push({
        key: "job_cpu_limit_millicores",
        label: "CPU per job run",
        using:
          settings.env_job_cpu_limit_millicores > 0
            ? `${settings.env_job_cpu_limit_cores} core${settings.env_job_cpu_limit_cores === 1 ? "" : "s"}`
            : "this install sets no CPU limit, so job runs are not throttled",
      });
    }
    if (zero("max_concurrent_builds")) {
      deferring.push({
        key: "max_concurrent_builds",
        label: "Builds at once",
        using: `${settings.env_max_concurrent_builds} at once`,
      });
    }
    if (zero("auto_deploy_poll_seconds")) {
      deferring.push({
        key: "auto_deploy_poll_seconds",
        label: "Auto-deploy check interval",
        using: settings.auto_deploy_poller_running
          ? `every ${settings.env_auto_deploy_poll_seconds} seconds`
          : "polling is off for this install",
      });
    }
  }

  const nothingMatches = matchedSections.length === 0;

  return (
    <div className={dirtyKeys.length > 0 ? "pb-24" : undefined}>
      <PageHeader
        title="Settings"
        actions={
          <SearchInput
            placeholder="Filter settings…"
            value={query}
            onChange={setQuery}
            debounceMs={0}
            className="w-64"
          />
        }
      />

      {q && (
        <p className="-mt-4 mb-6 text-sm text-muted-foreground" aria-live="polite">
          {nothingMatches
            ? `Nothing matches “${query}”.`
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
      )}

      {/* The third consumer of the index: what this page cannot answer for.
          Under the count when there are local matches too, because "4 here"
          and "3 elsewhere" are both true and the second one is the half this
          page used to deny. */}
      {q && !nothingMatches && (
        <OtherPagesMatch query={query} route="/admin/settings" className="-mt-4 mb-6" />
      )}

      {nothingMatches ? (
        <div className="space-y-4">
          <EmptyState
            icon={<Search className="h-6 w-6" />}
            title="No setting by that name on this page"
            description="Try another word, or use the search box in the header to search every page."
            action={
              <Button variant="secondary" size="sm" onClick={() => setQuery("")}>
                Clear filter
              </Button>
            }
          />
          <OtherPagesMatch query={query} route="/admin/settings" />
        </div>
      ) : (
        <FilterTerm.Provider value={q}>
        <div className="flex gap-10">
          {/* Capped at the measure the app's Settings tab uses: uncapped,
              SettingRow's `justify-between` put ~800px between a label and
              its control at 1440. */}
          <div className="min-w-0 max-w-3xl flex-1 space-y-6">
            {visibleSections.has("address") && (
              <Section
                id={SECTION.address.id}
                title={SECTION.address.title}
                description={SECTION.address.blurb}
              >
                <div className="space-y-4 p-4">
                  {settings.observed_base_url &&
                    settings.observed_base_url !== settings.effective_base_url && (
                      <Notice
                        action={
                          <Button
                            size="sm"
                            variant="secondary"
                            disabled={busy}
                            onClick={() =>
                              setDrafts((d) => ({
                                ...d,
                                public_base_url: settings.observed_base_url ?? "",
                              }))
                            }
                          >
                            Use this address
                          </Button>
                        }
                      >
                        You reached this install at{" "}
                        <code className="font-mono">{settings.observed_base_url}</code>, but links
                        in email point at{" "}
                        <code className="font-mono">{settings.effective_base_url}</code>. Those links
                        may not work.
                      </Notice>
                    )}

                  <div className="divide-y divide-border/60">{rows("address")}</div>

                  {/* The apps origin, read-only: it is a listener and a
                      certificate, and the Address above does not move it.
                      Shown only where it differs, so a single-origin install
                      is not told the same address twice. */}
                  {settings.apps_base_url && settings.apps_base_url !== settings.effective_base_url && (
                    <Row label="Apps are served at">
                      <span className="text-right text-xs">
                        <span className="font-mono">{settings.apps_base_url}</span>
                        <span className="block text-muted-foreground">
                          From <Code>APPS_BASE_URL</Code> in the install’s configuration
                        </span>
                      </span>
                    </Row>
                  )}
                </div>
              </Section>
            )}

            {visibleSections.has("security") && (
              <Section
                id={SECTION.security.id}
                title={SECTION.security.title}
                description={SECTION.security.blurb}
              >
                <div className="space-y-4 p-4">
                  <SecurityTxtState settings={settings} draftOf={draftOf} />
                  {rows("security")}
                </div>
              </Section>
            )}

            {visibleSections.has("execution") && (
              <Section
                id={SECTION.execution.id}
                title={SECTION.execution.title}
                description={SECTION.execution.blurb}
              >
                <div className="divide-y divide-border/60 p-4">
                  {rows("execution")}
                </div>
              </Section>
            )}

            {visibleSections.has("jobs") && (
              <Section
                id={SECTION.jobs.id}
                title={SECTION.jobs.title}
                description={SECTION.jobs.blurb}
              >
                <div className="divide-y divide-border/60 p-4">
                  {/* Read-only, like the topology: where runs go is a fact
                      about how this install was deployed, not a setting. */}
                  <div className="space-y-2 pb-4 text-sm">
                    <Row label="Where runs go">
                      <span>{JOB_PLACEMENT_WORDS[settings.job_placement] ?? "Jobs can’t run here"}</span>
                    </Row>
                    {settings.job_placement === "this machine" && settings.shared_containment && (
                      <Row label="Memory limits">
                        <span>
                          {kernelHoldsMemory(settings.shared_containment)
                            ? JOB_MEMORY_WORDS.cgroup
                            : JOB_MEMORY_WORDS.watchdog}
                        </span>
                      </Row>
                    )}
                  </div>
                  {rows("jobs")}
                </div>
                <SectionFootnote>{SECTION.jobs.footnote}</SectionFootnote>
              </Section>
            )}

            {visibleSections.has("retention") && (
              <Section
                id={SECTION.retention.id}
                title={SECTION.retention.title}
                description={SECTION.retention.blurb}
              >
                <div className="divide-y divide-border/60 p-4">
                  {rows("retention")}
                </div>
              </Section>
            )}

            {visibleSections.has("gallery") && (
              <Section
                id={SECTION.gallery.id}
                title={SECTION.gallery.title}
                description={SECTION.gallery.blurb}
              >
                <div className="divide-y divide-border/60 p-4">
                  {rows("gallery")}
                  <CatalogPanel settings={settings} />
                </div>
              </Section>
            )}

          </div>

          <SectionRail sections={matchedSections} active={active} label="Settings sections" />
        </div>
        </FilterTerm.Provider>
      )}

      <PendingBar
        count={dirtyKeys.length}
        broken={brokenKeys.length}
        saving={save.isPending}
        onDiscard={() => setDrafts({})}
        onSave={saveAll}
      />
    </div>
  );
}

/** Where runs go, as the settings document reports it. */
const JOB_PLACEMENT_WORDS: Record<string, string> = {
  cluster: "Jobs run on the cluster",
  "this machine": "Jobs run on this machine",
};

/** How a run on this machine is held to its memory limit. */
const JOB_MEMORY_WORDS: Record<string, string> = {
  cgroup: "Enforced by the kernel",
  watchdog: "Sampled by a watchdog",
};

function settingText(id: SettingsPageKey) {
  return entryText(SETTING[id]);
}

function sectionOwnText(id: string) {
  const s = SECTION[id];
  return s ? `${s.title} ${s.blurb} ${s.keywords ?? ""}`.toLowerCase() : "";
}

/* ------------------------------------------------------------------ *
 * Pieces
 * ------------------------------------------------------------------ */

/**
 * A section's closing line, under the rows and outside the rule that divides
 * them.
 *
 * What is left of `SectionCard`, which was this page's own third way of
 * titling a raised panel — a `Card`, a `CardTitle` at 16px, a
 * `CardDescription` and a divided `CardContent`, which is `Section` with a
 * different heading size. The seven call sites always passed a title and a
 * blurb, so they pass them to `Section` now and the private component goes
 * rather than being promoted into a shared one, which would only have made it
 * the fourth.
 */
function SectionFootnote({ children }: { children?: React.ReactNode }) {
  if (!children) return null;
  return <div className="px-4 pb-4 text-xs text-muted-foreground">{children}</div>;
}

/**
 * The gallery's three values, as three buttons rather than a dropdown: there
 * are exactly three, each needs a sentence, and a menu would hide two of them
 * behind a click.
 *
 * Saved the moment it is chosen, like a switch — it is one deliberate act, and
 * the server's answer belongs to the choice that caused it.
 */
function ModeChoice({
  value,
  disabled,
  onChange,
}: {
  value: PlatformSettings["gallery_deploy_mode"];
  disabled: boolean;
  onChange: (v: PlatformSettings["gallery_deploy_mode"]) => void;
}) {
  // `ChoiceButtons`, not a fourth copy of its rendering. The copy carried the
  // chosen value in the button fill alone — a colour and a shape doing the
  // whole job of the state the control exists to report — because what was
  // borrowed was the look and not the `aria-pressed`.
  return (
    <ChoiceButtons
      label="What the gallery offers"
      value={value}
      disabled={disabled}
      onChange={onChange}
      options={[
        { value: "off", label: "Off", hint: "No gallery, and nothing fetched." },
        { value: "extensions_only", label: "Extensions", hint: "Administrators only." },
        { value: "all", label: "Extensions and examples", hint: "Anyone who may publish." },
      ]}
    />
  );
}

/**
 * Shared or isolated, for apps that do not yet exist.
 *
 * A choice rather than a switch because the two values are the product's own
 * vocabulary for where an app runs, and "isolated by default: on" would be a
 * third way of saying one of them.
 *
 * `isolatedOffered` disables the option rather than hiding it: an administrator
 * reading this row is entitled to know the setting has a second value, and the
 * row's note carries the reason it is not available — never a `title`, which a
 * disabled control cannot deliver to anybody.
 */
function DefaultExecutionModeChoice({
  value,
  disabled,
  isolatedOffered,
  onChange,
}: {
  value: ExecutionMode;
  disabled: boolean;
  isolatedOffered: boolean;
  onChange: (v: ExecutionMode) => void;
}) {
  return (
    <ChoiceButtons
      label="Execution mode for new apps"
      value={value}
      disabled={disabled}
      onChange={onChange}
      options={[
        {
          value: "shared",
          label: executionModeLabel.shared,
          hint: "A process inside Toft’s own footprint.",
        },
        {
          value: "isolated",
          label: executionModeLabel.isolated,
          hint: "Its own workload, from the app’s first build.",
          disabled: !isolatedOffered,
        },
      ]}
    />
  );
}

/**
 * How much the platform says about itself.
 *
 * A choice rather than a text field, because there are four answers and a typo
 * in one of them is an afternoon spent wondering why debug did nothing. It saves
 * on the click like every other choice on this page: it is one deliberate act
 * whose consequence is immediate, not one of six edits batched behind a bar.
 *
 * "Install" is the empty value and it is a real option rather than a way to
 * clear the field — an administrator who raised the level to look at something
 * needs a way back to whatever the install was started with, and naming that
 * level on the button is the difference between putting it back and guessing.
 */
function LogLevelChoice({
  value,
  envLevel,
  effective,
  disabled,
  onChange,
}: {
  value: string;
  envLevel: string;
  effective: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  // A select, not five buttons: in the 20rem control column the strip wrapped
  // and left Error alone on a second line. Saved on the pick, as the buttons
  // were. The sentence under it is about the level actually in effect, which
  // the install's own setting can decide.
  const options = [
    { value: "", label: `Default (${envLevel || "info"})` },
    { value: "debug", label: "Debug" },
    { value: "info", label: "Info" },
    { value: "warn", label: "Warn" },
    { value: "error", label: "Error" },
  ];
  const labelOf = (v: string) => options.find((o) => o.value === v)?.label ?? v;
  return (
    <div className="w-full space-y-1 sm:text-right">
      <div className="flex sm:justify-end">
        <Select
          value={value || "default"}
          onValueChange={(v) => onChange(v === "default" ? "" : (v as string))}
          disabled={disabled}
        >
          <SelectTrigger className="w-44" aria-label="Platform log level">
            <SelectValue>{(v) => labelOf(v === "default" ? "" : (v as string))}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {options.map((o) => (
              <SelectItem key={o.value || "default"} value={o.value || "default"}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <p className="text-xs text-muted-foreground">
        {effective === "debug"
          ? "Logging at debug now. Put it back when you’re done."
          : `Logging at ${effective || "info"} now.`}
      </p>
    </div>
  );
}

/**
 * The catalog's own state, and the one control that acts on it.
 *
 * Refreshing lives here rather than in the Gallery page's header because
 * everything it changes is on this card — the address, where that address came
 * from, and when the document was last fetched — and because it is an
 * install-wide administrative action on a page publishers never see. The
 * Gallery keeps a retry beside a failure, which is the one place the reason to
 * press it is already on screen.
 *
 * "Updates available" is derived from the same response the Gallery page and
 * the nav badge read, so the three can never disagree, and nothing about it is
 * stored: an extension stops being behind the moment its app is updated or
 * deleted. There is deliberately no mail — an add-on having a newer version is
 * neither a failure nor a decision about a person (ADR §28).
 */
function CatalogPanel({ settings }: { settings: PlatformSettings }) {
  const queryClient = useQueryClient();
  const off = settings.gallery_deploy_mode === "off";

  // The same query key the Gallery page uses, so on a session that has touched
  // either this costs no request at all. It 404s while the gallery is off,
  // which is what `enabled` accounts for.
  const { data: gallery } = useQuery<GalleryResponse>({
    queryKey: ["gallery"],
    queryFn: api.getGallery,
    retry: false,
    staleTime: 5 * 60_000,
    enabled: !off,
  });

  const sync = useMutation({
    mutationFn: api.syncGallery,
    onSuccess: (result: GallerySync) => {
      queryClient.invalidateQueries({ queryKey: ["gallery"] });
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      if (result.status === "ok") toast.success("Catalog refreshed");
      else toast.warning(result.error || "Couldn’t refresh the catalog");
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const extensions = (gallery?.items ?? []).filter((i) => i.kind === "extension");
  const installed = extensions.reduce((n, item) => n + (item.installed?.length ?? 0), 0);
  const behind = extensions.filter((item) => item.update_available).length;

  return (
    <>
      <div className="space-y-3 pt-4 text-sm">
        <Row label="Catalog address">
          <span className="max-w-md truncate font-mono text-xs" title={settings.gallery_catalog_url}>
            {settings.gallery_catalog_url}
          </span>
        </Row>
        <Row label="Set by">
          <Badge variant="secondary">
            {settings.gallery_catalog_source === "environment" ? "GALLERY_CATALOG_URL" : "Built in"}
          </Badge>
        </Row>
        <Row label="Last refreshed">
          <div className="flex items-center gap-3">
            <SyncStatus sync={settings.gallery_sync} mode={settings.gallery_deploy_mode} />
            {/* Absent rather than disabled while the gallery is off. A
                greyed button with the reason in a `title` is a control that
                cannot deliver its own explanation — and the row above already
                says the gallery is off. */}
            <Button
              variant="outline"
              size="sm"
              className="h-7 shrink-0 text-xs"
              disabled={sync.isPending}
              hidden={off}
              onClick={() => sync.mutate()}
            >
              <RefreshCw className={`h-3.5 w-3.5 ${sync.isPending ? "animate-spin" : ""}`} />
              {sync.isPending ? "Refreshing…" : "Refresh now"}
            </Button>
          </div>
        </Row>

        {/* Absent when no extension is installed: "up to date" about nothing is
            a row that reads as a fact and carries none. */}
        {!off && installed > 0 && (
          <Row label="Updates available">
            {behind > 0 ? (
              <Link
                to="/gallery"
                className={`inline-flex items-center gap-1.5 rounded-sm hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 ${toneText.attention}`}
              >
                <ArrowUpCircle className="h-3.5 w-3.5 shrink-0" />
                {behind} installed extension{behind === 1 ? "" : "s"} behind the catalog
              </Link>
            ) : (
              <span className="text-muted-foreground">
                All {installed} installed extension{installed === 1 ? "" : "s"} up to date
              </span>
            )}
          </Row>
        )}
      </div>

      {!off && (
        <p className="max-w-prose pt-4 text-xs text-muted-foreground">
          Refreshed every six hours.
        </p>
      )}
    </>
  );
}

/** The last refresh, in the words that tell an empty catalog from an
 *  unreachable one. */
function SyncStatus({
  sync,
  mode,
}: {
  sync: PlatformSettings["gallery_sync"];
  mode: PlatformSettings["gallery_deploy_mode"];
}) {
  if (mode === "off") return <span className="text-muted-foreground">Not fetched while off</span>;
  if (!sync || !sync.fetched_at) return <span className="text-muted-foreground">Not yet</span>;

  const when = <RelativeTime iso={sync.fetched_at} />;
  if (sync.status === "ok") {
    return (
      <span className="text-right text-xs">
        {when}{" "}
        <span className="text-muted-foreground">
          · {sync.item_count} item{sync.item_count === 1 ? "" : "s"}
          {sync.dropped_count > 0 && `, ${sync.dropped_count} unreadable`}
        </span>
      </span>
    );
  }
  return (
    <span className="text-right text-xs">
      <Badge variant="outline" className={toneSurface.attention}>
        {sync.status === "too_new" ? "Needs a newer Toft" : "Failed"}
      </Badge>
      <span className="ml-2 text-muted-foreground">{when}</span>
      {/* The whole message, not a hover. This is the server's own words about
          why the catalog would not refresh — the one thing an administrator
          came here to read. */}
      {sync.error && (
        <span className="mt-1 block max-w-md font-mono text-2xs break-words text-muted-foreground">
          {sync.error}
        </span>
      )}
    </span>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </div>
  );
}

interface ControlProps {
  id: string;
  value: string;
  dirty: boolean;
  error: string | null;
  disabled?: boolean;
  onChange: (v: string) => void;
  onSubmit: () => void;
}

function NumberControl({ id, value, dirty, error, disabled, onChange, onSubmit }: ControlProps) {
  const spec = NUMBER_FIELDS[id as NumberFieldKey];
  return (
    <ControlShell id={id} dirty={dirty} error={error}>
      <Input
        id={id}
        inputMode="numeric"
        className={spec.width ?? "w-24"}
        value={value}
        disabled={disabled}
        aria-invalid={!!error}
        aria-describedby={error ? fieldErrorId(id) : undefined}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => e.key === "Enter" && onSubmit()}
      />
      <UnitLabel unit={spec.unit} />
    </ControlShell>
  );
}

/**
 * A memory ceiling: a number, and the unit it is written in.
 *
 * The draft is always megabytes — the unit the row holds, and the one every
 * other part of this page compares against — so switching the unit changes what
 * is *displayed* and never what is pending. That is why the unit lives in this
 * component and not in the drafts map: a unit is how somebody reads a number,
 * not a second thing to save.
 *
 * The unit it opens on is derived from the value rather than remembered, so an
 * install capped at 16 GB shows 16 and one capped at 256 MB shows 256, with
 * nothing stored to say so.
 */
function MemoryControl({ id, value, dirty, error, disabled, onChange, onSubmit }: ControlProps) {
  const mb = Number(value.trim());
  const [unit, setUnit] = useState<"MB" | "GB">(() =>
    Number.isInteger(mb) && mb >= 1024 && mb % 1024 === 0 ? "GB" : "MB",
  );

  // What the box shows. Derived from the draft on every render rather than held
  // as a second piece of state, because two states holding one number is how a
  // field ends up disagreeing with what is about to be saved.
  // …except while somebody is typing in GB. Recomputing the shown text from
  // MB on every keystroke ate the decimal point: "1." is 1 GB, which is 1024
  // MB, which shows as "1". So the typed text is kept while the field has
  // focus, and only a value that parses fully is converted and sent on.
  const [typed, setTyped] = useState<string | null>(null);
  const shown =
    typed ?? (unit === "GB" && Number.isFinite(mb) ? String(round2(mb / 1024)) : value);

  return (
    <ControlShell id={id} dirty={dirty} error={error}>
      <Input
        id={id}
        inputMode="decimal"
        className="w-24"
        value={shown}
        disabled={disabled}
        aria-invalid={!!error}
        aria-describedby={error ? fieldErrorId(id) : undefined}
        onBlur={() => setTyped(null)}
        onChange={(e) => {
          const text = e.target.value;
          if (unit === "GB") setTyped(text);
          const n = Number(text.trim());
          if (text.trim() === "" || !Number.isFinite(n)) return onChange(text);
          onChange(unit === "GB" ? String(Math.round(n * 1024)) : String(n));
        }}
        onKeyDown={(e) => e.key === "Enter" && onSubmit()}
      />
      <Select
        value={unit}
        onValueChange={(v) => {
          setTyped(null);
          setUnit(v as "MB" | "GB");
        }}
        disabled={disabled}
      >
        <SelectTrigger className="w-20" aria-label="Unit">
          {/* base-ui's Value renders the stored value; here the two happen to
              be the same, and it is spelled out anyway so the trigger is never
              at the mercy of what the value string turns out to be. */}
          <SelectValue>{() => unit}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="MB">MB</SelectItem>
          <SelectItem value="GB">GB</SelectItem>
        </SelectContent>
      </Select>
    </ControlShell>
  );
}

/**
 * A CPU figure: millicores in the draft, cores on the screen. The same shape
 * as MemoryControl's GB — the draft is the unit the row holds, and the typed
 * text is kept while the field has focus so "0." is not eaten mid-keystroke.
 */
function CpuControl({ id, value, dirty, error, disabled, onChange, onSubmit }: ControlProps) {
  const m = Number(value.trim());
  const [typed, setTyped] = useState<string | null>(null);
  const shown =
    typed ?? (value.trim() !== "" && Number.isFinite(m) ? String(Math.round(m) / 1000) : value);
  return (
    <ControlShell id={id} dirty={dirty} error={error}>
      <Input
        id={id}
        inputMode="decimal"
        className="w-24"
        value={shown}
        disabled={disabled}
        aria-invalid={!!error}
        aria-describedby={error ? fieldErrorId(id) : undefined}
        onBlur={() => setTyped(null)}
        onChange={(e) => {
          const text = e.target.value;
          setTyped(text);
          const n = Number(text.trim());
          if (text.trim() === "" || !Number.isFinite(n)) return onChange(text);
          onChange(String(Math.round(n * 1000)));
        }}
        onKeyDown={(e) => e.key === "Enter" && onSubmit()}
      />
      <UnitLabel unit="cores" />
    </ControlShell>
  );
}

/** Two decimal places at most, and no trailing zeroes: 0.5 rather than 0.50,
 *  and 0.33 rather than a third of a gigabyte written out. */
function round2(n: number): number {
  return Math.round(n * 100) / 100;
}

/** A stored ceiling said the way somebody would say it out loud. */
function memoryPhrase(mb: number): string {
  return mb >= 1024 && mb % 1024 === 0 ? `${mb / 1024} GB` : `${mb} MB`;
}

function TextControl({
  id,
  value,
  dirty,
  error,
  disabled,
  onChange,
  onSubmit,
  width,
  placeholder,
  suggestions,
}: ControlProps & { width: string; placeholder?: string; suggestions?: string[] }) {
  return (
    <ControlShell id={id} dirty={dirty} error={error}>
      <Input
        id={id}
        className={width}
        value={value}
        placeholder={placeholder}
        list={suggestions ? `${id}-suggestions` : undefined}
        disabled={disabled}
        aria-invalid={!!error}
        aria-describedby={error ? fieldErrorId(id) : undefined}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => e.key === "Enter" && onSubmit()}
      />
      {suggestions && (
        <datalist id={`${id}-suggestions`}>
          {suggestions.map((z) => (
            <option key={z} value={z}>
              {z}
            </option>
          ))}
        </datalist>
      )}
    </ControlShell>
  );
}

/**
 * The control, its unit, and the two things a batched form has to say that a
 * per-field Save button said by being enabled: this one has been touched, and
 * this one cannot be saved as typed. The dot's space is always reserved so a
 * row does not shift as it becomes dirty.
 */
function ControlShell({
  id,
  dirty,
  error,
  children,
}: {
  id: string;
  dirty: boolean;
  error: string | null;
  children: React.ReactNode;
}) {
  return (
    <div className="w-full space-y-1 sm:text-right">
      <div className="flex items-center gap-2 sm:justify-end">
        {children}
        <span className="flex w-2 justify-center" aria-hidden>
          {dirty && (
            <span
              className={`h-1.5 w-1.5 rounded-full ${error ? "bg-destructive" : "bg-primary"}`}
            />
          )}
        </span>
        {dirty && <span className="sr-only">Unsaved</span>}
      </div>
      <FieldError field={id} className="max-w-xs sm:ml-auto">
        {error}
      </FieldError>
    </div>
  );
}


/**
 * What this install is publishing, above the four boxes that decide it.
 *
 * The section's whole risk is a file that is correct today and names a
 * departed colleague in eighteen months, so the state leads: whether anything
 * is served at all, and — the part the RFC's field exists for — how long is
 * left. Read from the *drafts*, so clearing the contact says "this stops
 * publishing" before the save rather than after it.
 */
function SecurityTxtState({
  settings,
  draftOf,
}: {
  settings: PlatformSettings;
  draftOf: (k: FieldKey) => string;
}) {
  const published = settings.security_txt_contact.trim() !== "";
  const willPublish = draftOf("security_txt_contact").trim() !== "";
  const daysLeft = published ? securityTxtDaysLeft(settings.security_txt_expires) : null;

  if (published && willPublish && daysLeft !== null && daysLeft <= 0) {
    return (
      <Notice tone="attention" title="The published security.txt has expired">
        It expired on <code className="font-mono">{settings.security_txt_expires}</code>. Set a new
        date, or clear the contact to stop publishing.
      </Notice>
    );
  }
  if (published && willPublish && daysLeft !== null && daysLeft <= 30) {
    return (
      <Notice tone="attention">
        The published security.txt expires in {daysLeft} day{daysLeft === 1 ? "" : "s"}. Check that{" "}
        <code className="font-mono">{settings.security_txt_contact}</code> still works, then set a
        new date.
      </Notice>
    );
  }
  if (published) {
    return (
      <p className="text-xs text-muted-foreground">
        Published at <code className="font-mono">/.well-known/security.txt</code>
        {daysLeft !== null && <> — good for another {daysLeft} day{daysLeft === 1 ? "" : "s"}</>}.
      </p>
    );
  }
  return (
    <p className="text-xs text-muted-foreground">
      Not published. Add a contact to publish{" "}
      <code className="font-mono">/.well-known/security.txt</code>.
    </p>
  );
}

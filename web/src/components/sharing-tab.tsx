import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Building2, Crown, Globe, Loader2, LockKeyhole, Users, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { UserAvatar } from "@/components/user-avatar";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import type { App, AppPermissions, AppVisibility, Grant } from "@/lib/api";
import { grantRoleLabel, systemRoleLabel } from "@/lib/utils";
import { toneSurface, toneText } from "@/lib/tone";
import { Section } from "@/components/section";
import { AppAccessRequests } from "@/components/app-access-requests";
import { SectionRail } from "@/components/section-rail";
import { useSectionSpy } from "@/hooks/use-section-spy";
import { appSectionsOn } from "@/lib/settings-index";
import { TableCard } from "@/components/table-card";
import { QueryError } from "@/components/query-error";
import { LockAppDialog } from "@/components/lock-app-dialog";
import { FieldHelp } from "@/components/field-help";

const VISIBILITY_RANK: Record<AppVisibility, number> = { private: 0, authenticated: 1, public: 2 };

/** Mirrors the server’s `noteLimit` exactly. The server refuses past it rather
 *  than truncating — a share that went out carrying words its author never
 *  wrote is worse than one that was refused — so the field stops there too. */
const NOTE_LIMIT = 500;

const VISIBILITY_OPTIONS: Array<{
  value: AppVisibility;
  title: string;
  description: string;
  icon: typeof Globe;
}> = [
  { value: "private", title: "Restricted", description: "Only the people and groups listed below", icon: LockKeyhole },
  { value: "authenticated", title: "Anyone signed in", description: "Anyone who can sign in to Toft", icon: Building2 },
  { value: "public", title: "Public", description: "Anyone with the link, without signing in", icon: Globe },
];

/**
 * The platform lock, on the tab that answers "who can reach this app".
 *
 * Two audiences, and the split is the whole component:
 *
 *   - An **administrator** gets the control. Locking asks for a message and a
 *     confirmation, because it stops the app and turns every visitor away;
 *     unlocking is a single reversible click and takes neither.
 *   - **Everyone else** gets the fact, and only when there is one. An owner
 *     whose app has gone dark must be able to find out why without filing a
 *     ticket, and the answer — who locked it, when, and in their words — is
 *     the one thing this page can tell them that nothing else can. On an
 *     unlocked app they see nothing at all: a control they cannot use is
 *     clutter, and one they could mistake for theirs is worse.
 */
function LockSection({ appId, app }: { appId: string; app: App }) {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const isAdmin = user?.system_role === "system_admin";
  const locked = !!app.locked_at;
  // A lock the dependency policy placed by itself: nobody was present, the
  // owner was told rather than asked, and the way back is a waiver.
  const byPolicy = locked && app.locked_source === "dependency";

  const [confirmOpen, setConfirmOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  if (!isAdmin && !locked) return null;

  async function unlock() {
    setBusy(true);
    try {
      await api.unlockApp(appId);
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["quiet-apps"] });
      // Says what did *not* happen, because the obvious assumption is wrong:
      // unlocking restores permission, not the process.
      toast.success("App unlocked — it is stopped until somebody starts it");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t unlock the app");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Section id="lock"
        title="Platform lock"
        description="Locking stops the app and blocks visitors, deploys and jobs. Nothing is deleted.">
      <div className="space-y-3 p-4">

      {locked ? (
        <Notice
          tone="danger"
          title={byPolicy ? "This app was locked by the dependency policy" : "This app is locked"}
          action={
            isAdmin ? (
              <Button variant="outline" size="sm" disabled={busy} onClick={unlock}>
                {busy ? "Unlocking…" : "Unlock"}
              </Button>
            ) : undefined
          }
        >
          {/* The administrator's own words when there are any. When there are
              not, saying so is better than showing the install-wide default
              here and letting it read as something chosen for this app. */}
          {app.locked_message ? (
            <p>“{app.locked_message}”</p>
          ) : (
            <p>Visitors see this install’s standard message for a locked app.</p>
          )}
          <p className="mt-1 text-muted-foreground">
            Locked <RelativeTime iso={app.locked_at!} />
            {/* A lock the platform placed has nobody to name: `locked_by` is a
                foreign key into users, and the platform has no row there. The
                source is what tells the two apart. */}
            {byPolicy
              ? " by this install’s dependency policy"
              : app.locked_by
                ? ` by ${app.locked_by}`
                : ""}
            .{" "}
            {isAdmin
              ? "Unlocking restores access but does not start the app."
              : "Only an administrator can lift this."}
          </p>
          {/* **Unlocking is not the end of it**, and an administrator has to
              read that before they press the button rather than discover it on
              the next pass. A consequence that can be walked past is not one,
              so the way to mean it is the mechanism the policy already has. */}
          {byPolicy && isAdmin && (
            <p className="mt-1 text-muted-foreground">
              It will be locked again while the finding matches. To allow it, grant a waiver on
              the Dependencies tab.
            </p>
          )}
        </Notice>
      ) : (
        <div className="flex items-center justify-between gap-4">
          <p className="min-w-0 text-sm text-muted-foreground">
            This app is not locked.
          </p>
          <Button variant="outline" size="sm" onClick={() => setConfirmOpen(true)}>
            <LockKeyhole className="mr-1.5 h-3.5 w-3.5" /> Lock app…
          </Button>
        </div>
      )}

      {/* One dialog for this decision wherever it is taken: the same copy
          about what survives, the same optional message, the same invalidated
          queries as the quiet list's inline Lock. */}
      <LockAppDialog
        appId={appId}
        appName={app.name}
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
      />
      </div>
    </Section>
  );
}

/**
 * Public, and the install does not publish.
 *
 * An install-wide decision the owner cannot undo, so there is no "here is how
 * to undo it" line — the honest next step is a person, and inventing a control
 * here would send an owner around a loop that ends where it started.
 *
 * It renders on a **public** app only. Every app on the install carries the
 * flag, but on a private one there is nothing being withheld and nothing to
 * say; the closed Public button says the rest.
 *
 * `attention`, not `danger`: the app is serving everybody who signs in, exactly
 * as an `authenticated` one does.
 */
function PublicAppsTurnedOff({ app }: { app: App }) {
  if (!app.public_withheld || app.visibility !== "public") return null;

  return (
    <Notice tone="attention" size="sm" title="This install doesn’t publish apps">
      <p>
        An administrator turned off public access for every app, so only people who sign in can
        open this one. It becomes public again if that is turned back on.
      </p>
    </Notice>
  );
}

/** Sharing panel: visibility, grants, and who-can-do-what. Every control is
 * gated by the introspection endpoint — permissions are never derived here. */
export function SharingTab({
  appId,
  app,
}: {
  appId: string;
  app: App;
}) {
  const queryClient = useQueryClient();
  const [confirmVisibility, setConfirmVisibility] = useState<AppVisibility | null>(null);
  const [savingVisibility, setSavingVisibility] = useState(false);
  const [savingListable, setSavingListable] = useState(false);

  const [activeSection, setActiveSection] = useState("");
  // Every section this tab renders, in the order it renders them. Two of the
  // five were unknown to the index until now, which is why this could not have
  // had a rail before.
  const accessSections = appSectionsOn("access");
  useSectionSpy(
    accessSections.map((s) => s.id),
    setActiveSection,
  );

  const {
    data: perms, isLoading, error: permsError, refetch: refetchPerms,
  } = useQuery<AppPermissions>({
    queryKey: ["app-permissions", appId],
    queryFn: () => api.getAppPermissions(appId),
  });


  async function applyVisibility(v: AppVisibility) {
    setSavingVisibility(true);
    try {
      await api.updateApp(appId, { visibility: v });
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      toast.success(
        v === "public"
          ? "Anyone with the link can open this app"
          : v === "authenticated"
            ? "Anyone signed in can open this app"
            : "Only people you’ve shared it with can open this app",
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t change visibility");
    } finally {
      setSavingVisibility(false);
      setConfirmVisibility(null);
    }
  }

  // No confirmation either way. Un-listing narrows — it takes disclosure away,
  // which is the direction requestVisibility below already applies immediately —
  // and re-listing gives back only what the install was offering anyway. Nothing
  // here is destructive and the switch is its own undo.
  async function applyListable(v: boolean) {
    setSavingListable(true);
    try {
      await api.updateApp(appId, { listable: v });
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      // What is now true, and who it is true of.
      toast.success(
        v
          ? "Everyone signed in can see that this app exists"
          : "Only people you’ve given access can see that this app exists",
      );
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : "Couldn’t change who can see that this app exists",
      );
    } finally {
      setSavingListable(false);
    }
  }

  function requestVisibility(v: AppVisibility) {
    if (v === app.visibility || savingVisibility) return;
    // Widening exposure warrants a stop; narrowing applies immediately.
    if (VISIBILITY_RANK[v] > VISIBILITY_RANK[app.visibility]) {
      setConfirmVisibility(v);
    } else {
      applyVisibility(v);
    }
  }

  // Before the skeleton: a failed read left this tab as three grey bars for
  // ever, which reads as "still working" and offers nothing to press.
  if (permsError) {
    return (
      <div className="pt-4">
        <QueryError
          title="What you may do on this app could not be read"
          error={permsError}
          fallback="Reading it needs access to the app."
          onRetry={() => void refetchPerms()}
        />
      </div>
    );
  }
  if (isLoading || !perms) {
    return (
      <div className="space-y-3 pt-4">
        {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-20 w-full" />)}
      </div>
    );
  }

  // `max-w-3xl` is the same measure as the Settings tab. These two are the app's
  // form-shaped tabs and they were 576 and 672 pixels wide, so moving between
  // them looked like the page resizing under a tab strip that did not.
  //
  // And now the same wayfinding. Two tabs of one page, of comparable height,
  // disagreed about whether a long stack needs any: Settings had a rail and
  // this had five sections in a bare column, so somebody sent to "the
  // embedding setting" scrolled the whole tab to find it. No filter box here —
  // five sections is a list to look down, not one to search.
  return (
    <div className="flex max-w-[61.5rem] gap-10 pt-4">
      <div className="min-w-0 max-w-3xl flex-1 space-y-8">
      {/* The anchor the app-scoped settings index points at. Visibility is the
          one setting on this app that is not on its Settings tab, which is
          exactly why the index has to be able to say so. */}
      <Section id="visibility"
          title="Who can open it"
          description="Viewing means opening the deployed app. Editing and deploying are granted below.">
        <div className="space-y-3 p-4">
        <div className="grid gap-2 sm:grid-cols-3">
          {VISIBILITY_OPTIONS.map((opt) => {
            const active = app.visibility === opt.value;
            const Icon = opt.icon;
            // Public, on an install that does not publish. Disabled rather than
            // hidden: an owner who came here to publish needs to see that the
            // option exists and has been closed, not to wonder where it went.
            // The reason is the Notice below, never a `title` — a disabled
            // control is not focusable and swallows the hover.
            //
            // Not disabled on an app that already *is* public, because that
            // button is the app's own state and greying out what the card above
            // still shows as selected is the page contradicting itself.
            const closed = opt.value === "public" && !!app.public_withheld && !active;
            return (
              <button
                key={opt.value}
                type="button"
                disabled={closed || !perms.can_change_visibility || savingVisibility}
                onClick={() => requestVisibility(opt.value)}
                aria-pressed={active}
                className={`flex h-full flex-col items-start justify-start rounded-lg border p-3 text-left transition-colors disabled:cursor-not-allowed disabled:opacity-60 ${
                  active
                    ? "border-primary bg-primary/5 ring-1 ring-primary"
                    : "border-border/60 hover:border-border hover:bg-muted/40"
                }`}
              >
                <Icon className={`h-4 w-4 ${active ? "text-primary" : "text-muted-foreground"}`} />
                <p className="mt-1.5 text-sm font-medium text-foreground">{opt.title}</p>
                <p className="mt-0.5 text-xs text-muted-foreground">{opt.description}</p>
              </button>
            );
          })}
        </div>
        <PublicAppsTurnedOff app={app} />
        {/* Why the Public card is greyed out on an app that is not public —
            the one case the notices above deliberately say nothing about,
            because nothing is being withheld from an app that never asked.
            Here rather than in a `title`: a disabled button cannot be hovered
            or focused, so that explanation would reach nobody. Same register
            and same place as the ownership line under it. */}
        {app.public_withheld && app.visibility !== "public" && (
          <p className="text-xs text-muted-foreground">
            An administrator has turned off public access for all apps.
          </p>
        )}
        {/* The owner's half of the install-wide `list` rung (ADR §90).

            **Absent above `private`, not disabled.** A public or signed-in-
            visible app is already listable to everyone who could open it, so
            the switch would do nothing there — and a control that does nothing
            is worse than no control, whether it is greyed out with a `title`
            nobody can reach or live and inert. Absent, not dormant. The stored
            value is kept, so an owner who un-lists a private app, publishes it
            and comes back finds their own answer waiting.

            Inside the visibility section rather than below it, because it is
            the same question one notch weaker: those three buttons say who may
            *open* this app, and this says who may know it is here. */}
        {app.visibility === "private" && (
          <div className="flex items-start justify-between gap-8 border-t border-border/60 pt-3">
            <div className="min-w-0 space-y-1">
              <Label htmlFor="app-listable" className="text-sm font-medium">
                Show this app in everyone’s list
              </Label>
              <FieldHelp>
                When on, everyone signed in sees it listed but can’t open it until you share it.
              </FieldHelp>
            </div>
            <Switch
              id="app-listable"
              checked={app.listable}
              disabled={!perms.can_change_visibility || savingListable}
              onCheckedChange={(v) => void applyListable(v)}
            />
          </div>
        )}
        {!perms.can_change_visibility && (
          <FieldHelp>Only the owner can change visibility.</FieldHelp>
        )}
        </div>
      </Section>

      {/* Below visibility, because it is the same question taken to zero: the
          three buttons above choose *which* audience may open this app, and a
          lock says none of them may. It is on this tab rather than in the
          Settings danger zone for that reason — somebody asking "why can’t
          anyone open this?" comes here first, checks visibility, and would
          otherwise find nothing that explains it.

          It is separated from the controls above rather than sitting among
          them, because it is not the same *kind* of control: visibility and
          grants are the owner's, and this is an administrator's decision the
          owner cannot undo. Rendering it as a fourth visibility button would
          promise the owner a switch they cannot throw. */}
      <LockSection appId={appId} app={app} />

      {/* Who may wrap this app in their own page, below the people questions
          because it is a different kind of answer: visibility and grants say
          *who* may reach it, and this says which pages may put it inside
          themselves. It sits on this tab rather than Settings for the reason
          visibility does — it is the same permission, and somebody asking "who
          can get at this app" comes here. */}
      <EmbeddingSection appId={appId} app={app} canEdit={perms.can_change_visibility} />

      {/* The queue before the roster: somebody waiting is more urgent than the
          list of people who already have it, and the two answer the same
          question a day apart. It renders itself away when nobody is waiting,
          so an app nobody has asked for gains nothing on the page. */}
      {perms.can_approve_access_requests && <AppAccessRequests appId={appId} />}

      {perms.can_manage_grants && (
        <GrantsSection appId={appId} canNotify={perms.can_notify_on_share} />
      )}
      </div>

      <SectionRail
        sections={accessSections}
        active={activeSection}
        label="Access sections"
      />

      <Dialog open={!!confirmVisibility} onOpenChange={(o) => { if (!o) setConfirmVisibility(null); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {confirmVisibility === "public" ? "Make this app public?" : "Open to anyone signed in?"}
            </DialogTitle>
            <DialogDescription>
              {confirmVisibility === "public"
                ? `Anyone with the link can open ${app.name}, with no sign-in.`
                : `Everyone who signs in to Toft can open ${app.name}.`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={savingVisibility}
              onClick={() => setConfirmVisibility(null)}
            >
              Cancel
            </Button>
            <Button
              disabled={savingVisibility}
              onClick={() => confirmVisibility && applyVisibility(confirmVisibility)}
            >
              {savingVisibility
                ? "Saving…"
                : confirmVisibility === "public"
                  ? "Make public"
                  : "Open to anyone signed in"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// ── Grants ──────────────────────────────────────────────────

/**
 * `canNotify` is a **capability**, not a permission: it says a message would
 * actually go out — email configured, switched on, and the app audience not
 * silenced by an administrator. Where it is false the whole control is absent
 * rather than disabled, because a ticked box that sends nothing is a control
 * promising mail, which is the one thing this product's copy may not do.
 */
/**
 * Which pages may put this app inside an iframe.
 *
 * The default is not "nobody" and it is not "anyone": with an empty list the
 * platform sends `frame-ancestors 'self' <this install>`, so this Toft can
 * embed the app — which is what the preview on the app page is — and no other
 * site can. The copy has to say that, because a person reading an empty list
 * will otherwise read it as either extreme and both are wrong.
 *
 * Each add and each remove saves immediately, like a switch rather than a form:
 * there is nothing to batch, and the server's refusal belongs to the origin
 * that caused it while the person still has it in front of them.
 */
function EmbeddingSection({
  appId,
  app,
  canEdit,
}: {
  appId: string;
  app: App;
  canEdit: boolean;
}) {
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const origins = app.frame_ancestors ?? [];

  async function save(next: string[], done: string) {
    setSaving(true);
    try {
      await api.updateApp(appId, { frame_ancestors: next });
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(done);
      return true;
    } catch (err) {
      // The server’s own words: it refuses a path, a bare hostname and a
      // wildcard for three different reasons, and each one tells the person
      // what they believed that was not true.
      toast.error(err instanceof Error ? err.message : "Couldn’t save who may embed this app", {
        duration: 8000,
      });
      return false;
    } finally {
      setSaving(false);
    }
  }

  async function add() {
    const origin = draft.trim();
    if (!origin || saving) return;
    if (await save([...origins, origin], `${origin} can now embed this app`)) setDraft("");
  }

  // `scroll-mt-20` like every other anchored section: this was the one without
  // it, and `lib/settings-index.ts` deep-links to it, so a ⌘K jump landed with
  // the heading under the sticky header.
  return (
    <Section id="embedding"
        title="Embedding"
        description="Other sites that may show this app in a frame.">
      <div className="space-y-3 p-4">

      <div className="space-y-3">
        {canEdit && (
          <div className="space-y-1.5">
            <Label htmlFor="embed-origin" className="text-xs font-medium">
              Allow another site
            </Label>
            <div className="flex gap-2">
              <Input
                id="embed-origin"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    add();
                  }
                }}
                placeholder="https://intranet.example.com"
                className="font-mono text-xs md:text-xs"
                disabled={saving}
              />
              <Button onClick={add} disabled={saving || !draft.trim()}>
                {saving ? "Saving…" : "Add"}
              </Button>
            </div>
            <FieldHelp>
              Scheme and host, no path. A leading wildcard is allowed, as in{" "}
              <span className="font-mono">https://*.example.com</span>.
            </FieldHelp>
          </div>
        )}
        {origins.length === 0 ? (
          <p className="text-xs text-muted-foreground">No other site can embed this app.</p>
        ) : (
          <ul className="space-y-1.5">
            {origins.map((origin) => (
              <li key={origin} className="flex items-center justify-between gap-3">
                <span className="font-mono text-xs text-foreground break-all">{origin}</span>
                {canEdit && (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={saving}
                    aria-label={`Remove ${origin}`}
                    onClick={() =>
                      save(
                        origins.filter((o) => o !== origin),
                        `${origin} can no longer embed this app`,
                      )
                    }
                  >
                    <X className="h-3.5 w-3.5" />
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}


        {!canEdit && origins.length > 0 && (
          <FieldHelp>Only the owner can change this.</FieldHelp>
        )}
      </div>
      </div>
    </Section>
  );
}

function GrantsSection({ appId, canNotify }: { appId: string; canNotify: boolean }) {
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [pendingKey, setPendingKey] = useState<string | null>(null);
  const [notify, setNotify] = useState(false);
  const [note, setNote] = useState("");

  const { data: grants = [], isLoading, error, refetch } = useQuery<Grant[]>({
    queryKey: ["app-grants", appId],
    queryFn: () => api.listGrants(appId),
  });

  const q = search.trim();
  const usersQuery = useQuery({
    queryKey: ["directory-users", q],
    queryFn: () => api.searchDirectoryUsers(q),
    enabled: q.length >= 2,
  });
  const groupsQuery = useQuery({
    queryKey: ["directory-groups", q],
    queryFn: () => api.searchDirectoryGroups(q),
    enabled: q.length >= 2,
  });
  const userResults = usersQuery.data ?? [];
  const groupResults = groupsQuery.data ?? [];
  // Three states. `data = []` while the search is in flight and `data = []`
  // after it failed both said "No matching people or groups." — a claim about
  // the directory made from a request that had not answered, or had not
  // worked. See `directory-picker.tsx`, which is the same three states.
  const searching = usersQuery.isFetching || groupsQuery.isFetching;
  const searchFailed = usersQuery.error ?? groupsQuery.error;

  const granted = new Set(grants.map((g) => `${g.principal_type}:${g.principal_id}`));
  const userCandidates = userResults.filter((u) => !granted.has(`user:${u.id}`));
  const groupCandidates = groupResults.filter((g) => !granted.has(`group:${g.id}`));

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["app-grants", appId] });
  }

  /**
   * Adding somebody, and — for a person, and only when asked — telling them.
   *
   * The toast reports what actually happened rather than what was asked for.
   * The server names every reason a notification was skipped, and two of them
   * are things nobody could have known before pressing the button: the
   * directory picker carries no email address by design, so "they have no
   * address on file" is discoverable only here. A toast that said "emailed
   * them" either way would be the one lie this feature can tell.
   */
  async function addGrant(principalType: "user" | "group", principalId: string, name: string) {
    setPendingKey(`${principalType}:${principalId}`);
    const asked = notify && principalType === "user";
    try {
      const result = await api.putGrant(
        appId, principalType, principalId, "viewer",
        asked ? { notify: true, note: note.trim() || undefined } : undefined,
      );
      refresh();
      setSearch("");
      setNote("");
      const added = `Added ${name} as ${grantRoleLabel("viewer")}`;
      if (result.notified) {
        toast.success(`${added}, and emailed them`);
      } else if (result.notify_skipped === "no_address") {
        toast.warning(`${added} — they have no email address on file, so nothing was sent`);
      } else if (result.notify_skipped === "email_off") {
        toast.warning(`${added} — email is off on this install, so nothing was sent`);
      } else {
        toast.success(added);
      }
    } catch (err) {
      // Backend 422s (viewer-exclusion, group-role, locked target, an overlong
      // note) verbatim.
      toast.error(err instanceof Error ? err.message : `Couldn’t add ${name}`);
    } finally {
      setPendingKey(null);
    }
  }

  async function changeRole(g: Grant, role: string) {
    if (role === g.role) return;
    setPendingKey(`${g.principal_type}:${g.principal_id}`);
    try {
      await api.putGrant(appId, g.principal_type as "user" | "group", g.principal_id, role);
      refresh();
      // The label, not the raw value: the stored word is lowercase, and "a
      // editor" is what interpolating it directly would have said.
      toast.success(`${g.display_name}’s role is now ${grantRoleLabel(role)}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t change ${g.display_name}’s role`);
    } finally {
      setPendingKey(null);
    }
  }

  async function removeGrant(g: Grant) {
    setPendingKey(`${g.principal_type}:${g.principal_id}`);
    try {
      await api.deleteGrant(appId, g.principal_type as "user" | "group", g.principal_id);
      refresh();
      toast.success(`Removed ${g.display_name}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t remove ${g.display_name}`);
    } finally {
      setPendingKey(null);
    }
  }

  return (
    <Section id="people"
        title="People and groups"
        description={<>Choose <em>List only</em> to show the app without letting them open it.</>}
    >
      <div className="space-y-3 p-4">

      <div className="space-y-1.5">
        <Label htmlFor="grant-search" className="sr-only">Add people or groups</Label>
        <Input
          id="grant-search"
          placeholder="Add people or groups by name…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          autoComplete="off"
        />

        {/* Under the search box rather than in a dialog of its own, because it
            is a property of *this* sharing session: somebody adding three
            people to a new app writes the note once. Groups ignore it — a group
            is a set of humans and not an actor, and there is no one person to
            write to. */}
        {canNotify && (
          <div className="space-y-2 pt-1">
            <label className="flex items-center gap-2 text-xs text-muted-foreground">
              <Checkbox
                checked={notify}
                onCheckedChange={(on) => setNotify(on === true)}
              />
              Email people about this when you add them
            </label>
            {notify && (
              <div className="space-y-1">
                <Label htmlFor="grant-note" className="sr-only">
                  A note to send with it
                </Label>
                <Textarea
                  id="grant-note"
                  rows={2}
                  maxLength={NOTE_LIMIT}
                  value={note}
                  onChange={(e) => setNote(e.target.value)}
                  placeholder="Add a note (optional)"
                />
                <FieldHelp>
                  Sent in your name with a link to the app. Groups aren’t emailed.
                </FieldHelp>
              </div>
            )}
          </div>
        )}

        {/* How many matched, said out loud — see `directory-picker.tsx`. */}
        <p aria-live="polite" className="sr-only">
          {q.length < 2
            ? ""
            : searchFailed
              ? "The directory could not be searched."
              : searching
                ? "Searching…"
                : `${userCandidates.length + groupCandidates.length} matches`}
        </p>
        {q.length >= 2 && (
          <div className="max-h-48 divide-y divide-border/60 overflow-y-auto rounded-md border border-border/60">
            {searchFailed ? (
              <p className={`p-2.5 text-xs ${toneText.danger}`}>
                {searchFailed instanceof Error && searchFailed.message
                  ? searchFailed.message
                  : "The directory could not be searched."}
              </p>
            ) : searching ? (
              <p className="p-2.5 text-xs text-muted-foreground">Searching…</p>
            ) : userCandidates.length === 0 && groupCandidates.length === 0 ? (
              <p className="p-2.5 text-xs text-muted-foreground">No matching people or groups.</p>
            ) : (
              <>
                {userCandidates.map((u) => (
                  <button
                    key={u.id}
                    type="button"
                    disabled={pendingKey === `user:${u.id}`}
                    className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:opacity-50"
                    onClick={() => addGrant("user", u.id, u.login)}
                  >
                    <UserAvatar id={u.id} login={u.login} avatarUrl={u.avatar_url} size="xs" />
                    <span className="font-medium">{u.login}</span>
                    <span className="ml-auto text-xs text-muted-foreground">{systemRoleLabel(u.system_role)}</span>
                  </button>
                ))}
                {groupCandidates.map((g) => (
                  <button
                    key={g.id}
                    type="button"
                    disabled={pendingKey === `group:${g.id}`}
                    className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:opacity-50"
                    onClick={() => addGrant("group", g.id, g.name)}
                  >
                    <Users className="h-4 w-4 text-muted-foreground" />
                    <span className="font-medium">{g.name}</span>
                    <span className="ml-auto text-xs text-muted-foreground">
                      group · {g.eligible_members} {g.eligible_members === 1 ? "person" : "people"}
                    </span>
                  </button>
                ))}
              </>
            )}
          </div>
        )}
      </div>

      {isLoading ? (
        <Skeleton className="h-24 w-full" />
      ) : error ? (
        // An empty grant list is a claim about who can reach this app, and a
        // failed read is not entitled to make it — least of all on the tab
        // somebody opens to check exactly that.
        <QueryError
          title="This app’s grants could not be read"
          error={error}
          fallback="Reading who an app is shared with needs the right to manage its access."
          onRetry={() => void refetch()}
        />
      ) : (
        <TableCard flat className="divide-y divide-border/60">
          {grants.map((g) => {
            const key = `${g.principal_type}:${g.principal_id}`;
            const busy = pendingKey === key;
            return (
              <div key={key} className="flex items-center gap-2.5 px-3 py-2">
                {g.principal_type === "group" ? (
                  <Users className="h-5 w-5 text-muted-foreground" />
                ) : (
                  <UserAvatar id={g.principal_id} name={g.display_name} avatarUrl={g.avatar_url} size="xs" />
                )}
                <span className="text-sm font-medium text-foreground">{g.display_name}</span>
                {g.principal_type === "group" && (
                  <span className="text-xs text-muted-foreground">group</span>
                )}

                <div className="ml-auto flex items-center gap-1.5">
                  {g.role === "owner" ? (
                    <Badge variant="outline" className={`text-xs ${toneSurface.info}`}>
                      <Crown className="mr-1 h-3 w-3" /> Owner
                    </Badge>
                  ) : (
                    <Select value={g.role} onValueChange={(v) => changeRole(g, v as string)}>
                      <SelectTrigger size="sm" disabled={busy} aria-label={`Role for ${g.display_name}`}>
                        {/* base-ui's Value renders the raw value, not the
                            chosen item's text — format it here. */}
                        <SelectValue>{(v) => grantRoleLabel(v as string)}</SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="list">List only</SelectItem>
                        <SelectItem value="viewer">Viewer</SelectItem>
                        <SelectItem value="editor">Editor</SelectItem>
                      </SelectContent>
                    </Select>
                  )}
                  {g.role !== "owner" && (
                    <Button
                      variant="ghost"
                      size="icon-xs"
                      aria-label={`Remove ${g.display_name}`}
                      disabled={busy}
                      onClick={() => removeGrant(g)}
                    >
                      {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <X className="h-3.5 w-3.5" />}
                    </Button>
                  )}
                </div>
              </div>
            );
          })}
        </TableCard>
      )}
      </div>
    </Section>
  );
}

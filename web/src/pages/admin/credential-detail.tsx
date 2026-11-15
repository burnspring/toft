import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, MoreHorizontal, Plus, Unlink, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Code } from "@/components/code";
import { EmptyState } from "@/components/empty-state";
import { Notice } from "@/components/notice";
import { ParentCrumb } from "@/components/parent-crumb";
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { toneText } from "@/lib/tone";
import { cn } from "@/lib/utils";
import { RelativeTime } from "@/components/relative-time";
import { Fact, Section } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { FieldHelp } from "@/components/field-help";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { api, type App, type Credential, type CredentialApp } from "@/lib/api";

/**
 * One credential, and the apps holding it.
 *
 * The inventory used to be the whole feature, and it could name a count and
 * nothing else: `GET /admin/credentials` carried `app_count`, so the page knew
 * that three apps held the warehouse password and never which three. An
 * administrator deciding whether to rotate one — the question the count was
 * put there to answer — had to open app pages until they found them, and a
 * detach could only be performed from the app's own side by somebody who had
 * already guessed which app it was. The delete refusal named the apps, which
 * made the error message the best surface in the feature.
 *
 * So the apps are a read of their own (`GET /admin/credentials/{id}/apps`) and
 * this is the page that makes them actionable. The inventory keeps the count:
 * every credential's apps is a fan-out nobody browsing the list asked for.
 *
 * **Still no value, and there is nothing to reveal**: the store has no read
 * that produces one. What this page adds is who holds it and who decided —
 * accountability, which is the only claim the feature is allowed to make.
 */
export function AdminCredentialDetailPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [rotating, setRotating] = useState(false);
  const [deleting, setDeleting] = useState(false);

  // The inventory's own query, reused: arriving from the list renders this
  // instantly from cache, and a deep link fetches the same list once. There is
  // no per-credential read to add — the apps below are keyed by id, and the id
  // is what this lookup is for.
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["credentials"],
    queryFn: () => api.listCredentials(),
    retry: false,
  });
  const credential = (data?.credentials ?? []).find((c) => c.id === id);

  useDocumentTitle(credential?.name);
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["credentials"] });
    queryClient.invalidateQueries({ queryKey: ["credential-apps", id] });
  };

  // The crumb renders above every branch, not only the one that succeeded.
  // This route is reachable from one row of one table, so it is the only exit
  // the page ever names.
  if (error) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/credentials" parent="Credentials" />
        <QueryError
          title="This credential could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  if (isLoading) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/credentials" parent="Credentials" />
        <Skeleton className="h-6 w-48" />
        <Skeleton className="h-20 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (!credential) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/credentials" parent="Credentials" />
        <EmptyState
          icon={<KeyRound className="h-6 w-6" />}
          title="No such credential"
          description="It may have been deleted."
          action={
            <Link
              to="/admin/credentials"
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              Back to Credentials
            </Link>
          }
        />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <ParentCrumb to="/admin/credentials" parent="Credentials" current={credential.name} />

      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight">{credential.name}</h1>
          {/* An empty description used to render nothing at all, so the page
              did not say the slot existed and the only way to find out was to
              open the menu. Absent is a state worth drawing. */}
          {credential.description ? (
            <p className="mt-0.5 whitespace-pre-wrap text-sm text-muted-foreground">
              {credential.description}
            </p>
          ) : (
            <button
              type="button"
              className="mt-0.5 text-sm text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
              onClick={() => setEditing(true)}
            >
              Add a description
            </button>
          )}
        </div>

        <div className="flex shrink-0 items-center gap-2">
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Actions for ${credential.name}`}
                >
                  <MoreHorizontal className="h-4 w-4" />
                </Button>
              }
            />
            <DropdownMenuContent align="end">
              {/* First, because it is the one act here that takes nothing away:
                  no app is detached, nothing restarts, and no attached app is
                  marked as running something stale.

                  **"Edit details", not "Rename".** It edits the name *and* the
                  description, and calling it Rename hid the description behind
                  a word that named half of it — which is exactly how somebody
                  ends up asking where the description field went. */}
              <DropdownMenuItem onClick={() => setEditing(true)}>Edit details…</DropdownMenuItem>
              <DropdownMenuItem onClick={() => setRotating(true)}>
                Replace values…
              </DropdownMenuItem>
              <DropdownMenuItem variant="destructive" onClick={() => setDeleting(true)}>
                Delete…
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <TableCard className="p-4">
        {/* No Apps count: the table below lists them, and "0" over "No app
            holds this yet" was one fact said twice. */}
        <dl className="grid grid-cols-2 gap-4 sm:grid-cols-3">
          <Fact label="Supplies">
            <span className="font-mono text-xs">{credential.keys.join(", ")}</span>
          </Fact>
          <Fact label="Created">
            <RelativeTime iso={credential.created_at} />
          </Fact>
          {/* Last changed, which on a credential is the question "when was this
              rotated" — the one fact an organization asks that the app owner's
              own variable could never answer. */}
          <Fact label="Last changed">
            <RelativeTime iso={credential.updated_at} />
          </Fact>
        </dl>
      </TableCard>

      <AppsSection credential={credential} onChanged={refresh} />

      {editing && (
        <EditCredentialDialog
          key={`edit-${credential.id}`}
          credential={credential}
          onOpenChange={() => setEditing(false)}
          onSaved={refresh}
        />
      )}
      {rotating && (
        <ReplaceValuesDialog
          key={`values-${credential.id}`}
          credential={credential}
          onOpenChange={() => setRotating(false)}
          onSaved={refresh}
        />
      )}
      {deleting && (
        <DeleteCredentialDialog
          credential={credential}
          onOpenChange={() => setDeleting(false)}
          onDeleted={() => {
            refresh();
            navigate("/admin/credentials");
          }}
        />
      )}
    </div>
  );
}

// ── Apps ────────────────────────────────────────────────────

/**
 * Which apps hold this, and the two acts that change that.
 *
 * **Attaching gets no confirmation and detaching does.** They are not mirror
 * images: an attach adds a name to an environment that nothing is reading yet
 * and takes effect at the app's next start, while a detach restarts somebody
 * else's running app on the spot — which is exactly the tier the confirmation
 * policy puts "stop somebody else's app" at, for the same reason. It is
 * reversible for the administrator and it is other people's users who notice.
 */
function AppsSection({
  credential,
  onChanged,
}: {
  credential: Credential;
  onChanged: () => void;
}) {
  const queryClient = useQueryClient();
  const [attaching, setAttaching] = useState(false);
  const [detaching, setDetaching] = useState<CredentialApp | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["credential-apps", credential.id],
    queryFn: () => api.listCredentialApps(credential.id),
  });
  const apps = data?.apps ?? [];

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["credential-apps", credential.id] });
    onChanged();
  }

  return (
    <Section
      title="Apps"
      description="Attaching applies at the app’s next start. Detaching restarts the app now."
      actions={
        <Button size="sm" onClick={() => setAttaching(true)}>
          <Plus className="mr-1.5 h-4 w-4" />
          Attach to an app
        </Button>
      }
    >
      {isLoading ? (
        <div className="p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      ) : error ? (
        <div className="p-4">
          {/* "No app holds this" is what somebody reads before deleting a
              credential, so a failed read must never be printed as one. */}
          <QueryError
            title="The apps could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      ) : apps.length === 0 ? (
        <EmptyState size="inline" title="No app holds this yet" />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="w-full">App</TableHead>
              {/* Who decided, because an attach is an administrator's act with
                  an audit row and this is the surface where that is legible. */}
              <TableHead className="hidden @2xl:table-cell">Attached by</TableHead>
              <TableHead className="hidden @xl:table-cell">Attached</TableHead>
              <TableHead className="w-[40px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {apps.map((a) => (
              <TableRow key={a.app_id}>
                <TableCell className="max-w-0">
                  <Link
                    to={`/apps/${a.app_id}`}
                    className="block truncate text-sm text-primary hover:underline"
                  >
                    {a.app_name}
                  </Link>
                  <span className="block truncate font-mono text-2xs text-muted-foreground">
                    {a.app_slug}
                  </span>
                </TableCell>
                <TableCell className="hidden text-sm @2xl:table-cell">
                  {a.attached_by || <span className="text-muted-foreground">—</span>}
                </TableCell>
                <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                  <RelativeTime iso={a.attached_at} />
                </TableCell>
                <TableCell>
                  {/* Unlink, not Trash2: detaching deletes nothing. */}
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Detach ${credential.name} from ${a.app_name}`}
                    onClick={() => setDetaching(a)}
                  >
                    <Unlink className="h-3.5 w-3.5" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      {attaching && (
        <AttachCredentialDialog
          credential={credential}
          attached={apps.map((a) => a.app_id)}
          onOpenChange={() => setAttaching(false)}
          onAttached={refresh}
        />
      )}
      {detaching && (
        <DetachCredentialDialog
          key={detaching.app_id}
          credential={credential}
          app={detaching}
          onOpenChange={() => setDetaching(null)}
          onDetached={refresh}
        />
      )}
    </Section>
  );
}

/**
 * Giving one app the credential.
 *
 * A name the app already sets is refused by the server rather than resolved by
 * precedence at start, and the refusal names the key. That message is better
 * than anything this dialog could compose, so it is passed straight through.
 */
export function AttachCredentialDialog({
  credential,
  attached,
  onOpenChange,
  onAttached,
}: {
  credential: Credential;
  /** App ids that already hold it, so the picker cannot offer a no-op. */
  attached?: string[];
  onOpenChange: (o: boolean) => void;
  onAttached: () => void;
}) {
  const queryClient = useQueryClient();
  const [appId, setAppId] = useState("");
  const [saving, setSaving] = useState(false);

  const { data: all = [], isLoading, error, refetch } = useQuery<App[]>({
    queryKey: ["apps"],
    queryFn: () => api.listApps(),
  });
  const held = new Set(attached ?? []);
  const apps = all.filter((a) => !held.has(a.id));
  const selected = apps.find((a) => a.id === appId);

  async function attach() {
    if (!appId) return;
    setSaving(true);
    try {
      await api.attachCredential(appId, credential.id);
      // The app's own page carries its credentials, so it is stale now too.
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      toast.success(`“${credential.name}” attached to “${selected?.name ?? "the app"}”`, {
        description: "It reaches the app’s environment at its next start.",
      });
      onAttached();
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t attach “${credential.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent scrollable>
        <DialogHeader>
          <DialogTitle>Attach {credential.name} to an app</DialogTitle>
          <DialogDescription>
            The app receives {credential.keys.length === 1 ? "this name" : "these names"} as
            environment variables at its next start. Its owner cannot read the values back,
            change them, or take them to another app.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <span className="text-sm font-medium">Supplies</span>
            <p className="font-mono text-xs text-muted-foreground">
              {credential.keys.join(", ")}
            </p>
          </div>

          {error ? (
            <QueryError
              title="Apps could not be read"
              error={error}
              fallback="Attaching needs the list of apps on this install."
              onRetry={() => void refetch()}
            />
          ) : isLoading ? (
            <Skeleton className="h-9 w-full" />
          ) : apps.length === 0 ? (
            <EmptyState
              title={all.length === 0 ? "No apps yet" : "Every app already has it"}
              description={
                all.length === 0
                  ? "Once somebody deploys an app, you can give it this credential."
                  : undefined
              }
            />
          ) : (
            <div className="space-y-1.5">
              <Label htmlFor="attach-app">App</Label>
              <Select value={appId} onValueChange={(v) => setAppId(v ?? "")}>
                <SelectTrigger id="attach-app" className="w-full">
                  {/* base-ui's Value renders the raw value — here, the app's id. */}
                  <SelectValue>
                    {(v) => apps.find((a) => a.id === v)?.name ?? "Choose an app…"}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {apps.map((a) => (
                    <SelectItem key={a.id} value={a.id}>
                      {a.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>
                A name the app already sets is refused, not overwritten.
              </FieldHelp>
            </div>
          )}
        </DialogBody>

        <DialogFooter>
          <Button variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button disabled={saving || !appId} onClick={() => void attach()}>
            {saving ? "Attaching…" : "Attach"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Taking it away, which restarts the app.
 *
 * A plain confirm rather than a bare click: the act is reversible and audited,
 * but it stops somebody else's running app to do it, and that is the tier the
 * policy puts "stop somebody else's app" at. The dialog's job is to say the
 * restart out loud before it happens, not to ask whether the administrator
 * meant to revoke access.
 */
function DetachCredentialDialog({
  credential,
  app,
  onOpenChange,
  onDetached,
}: {
  credential: Credential;
  app: CredentialApp;
  onOpenChange: (o: boolean) => void;
  onDetached: () => void;
}) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);

  async function detach() {
    setBusy(true);
    try {
      await api.detachCredential(app.app_id, credential.id);
      queryClient.invalidateQueries({ queryKey: ["app", app.app_id] });
      toast.success(`“${credential.name}” detached from “${app.app_name}”`, {
        description: "The app is restarting without it.",
      });
      onDetached();
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t detach “${credential.name}”`);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open onOpenChange={(o) => { if (!busy) onOpenChange(o); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Detach {credential.name} from {app.app_name}?</DialogTitle>
          <DialogDescription>
            <span className="font-mono text-xs">{credential.keys.join(", ")}</span> stops
            reaching that app. The app restarts now.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={busy} onClick={() => void detach()}>
            {busy ? "Detaching…" : "Detach and restart"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ── Edit the details ────────────────────────────────────────

/**
 * What a credential is called, and what it is for.
 *
 * Two fields and no confirmation: the name is a label an administrator attaches
 * by and an app owner reads on their own page. It is never a key, never a path,
 * and never anything a running workload holds — so no app is detached, nothing
 * restarts, and nothing is marked as carrying an unapplied change. It is the
 * one write on this page that cannot take anything away.
 *
 * **Separate from replacing the values**, rather than one form with everything
 * on it. Those are the two acts an administrator comes here for and they have
 * opposite consequences: this one changes nothing, and the other one tells
 * every attached app it is running something stale. A form that did both would
 * have to describe both, and its description would be true half the time.
 */
export function EditCredentialDialog({
  credential,
  onOpenChange,
  onSaved,
}: {
  credential: Credential;
  onOpenChange: (o: boolean) => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(credential.name);
  const [description, setDescription] = useState(credential.description ?? "");
  const [saving, setSaving] = useState(false);

  const dirty =
    name.trim() !== credential.name || description.trim() !== (credential.description ?? "");

  async function submit() {
    if (!name.trim() || !dirty) return;
    setSaving(true);
    try {
      await api.updateCredential(credential.id, {
        name: name.trim(),
        description: description.trim(),
      });
      toast.success("Saved");
      onSaved();
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t save “${credential.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={(o) => { if (!saving) onOpenChange(o); }}>
      <DialogContent scrollable>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving) void submit();
          }}
        >
        <DialogHeader>
          <DialogTitle>Edit {credential.name}</DialogTitle>
          <DialogDescription>
            Attached apps are unaffected.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="edit-credential-name">Name</Label>
            <Input
              id="edit-credential-name"
              value={name}
              className="font-mono"
              onChange={(e) => setName(e.target.value)}
            />
            <FieldHelp>
              What app owners see. Name what it reaches, like <Code>warehouse-prod</Code>.
            </FieldHelp>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="edit-credential-description">Description</Label>
            <Textarea
              id="edit-credential-description"
              rows={3}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Optional — what this reaches"
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="edit-credential-keys">Supplies</Label>
            <Input
              id="edit-credential-keys"
              readOnly
              value={credential.keys.join(", ")}
              onFocus={(e) => e.target.select()}
              className="font-mono text-xs"
            />
            <FieldHelp>
              To change the names, replace the values instead.
            </FieldHelp>
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || !dirty || !name.trim()}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ── Replace the values ──────────────────────────────────────

/** A stored name keeps its value when left blank; a new one needs a value. */
type Pair = { key: string; value: string; stored: boolean };

/**
 * Rotation.
 *
 * **A blank value keeps what is stored**, as on every other write-only field in
 * the product: each stored name arrives as a row that already says what it is,
 * so rotating one password on a three-name credential is typing one value. The
 * stored ones cannot be shown, so they are sent back as names to keep
 * (`keep`), never as values. Dropping a name is its own act — the × on its row
 * — because a blank that meant *delete* was the trap this replaced: it removed
 * the two names nobody retyped from every attached app at its next start.
 */
function ReplaceValuesDialog({
  credential,
  onOpenChange,
  onSaved,
}: {
  credential: Credential;
  onOpenChange: (o: boolean) => void;
  onSaved: () => void;
}) {
  const [pairs, setPairs] = useState<Pair[]>(
    credential.keys.length
      ? credential.keys.map((k) => ({ key: k, value: "", stored: true }))
      : [{ key: "", value: "", stored: false }],
  );
  const [saving, setSaving] = useState(false);

  const removed = credential.keys.filter((k) => !pairs.some((p) => p.stored && p.key === k));
  // A new row with nothing in it is an "Add another" nobody used, not an error.
  const fresh = pairs.filter((p) => !p.stored && (p.key.trim() || p.value !== ""));
  const incomplete = fresh.filter((p) => !p.key.trim() || p.value === "");
  const names = pairs.filter((p) => p.stored || p.key.trim()).map((p) => p.key.trim());
  const duplicate = names.find((n, i) => names.indexOf(n) !== i);
  const values = [
    ...pairs.filter((p) => p.stored && p.value !== ""),
    ...fresh.filter((p) => p.key.trim() && p.value !== ""),
  ];
  const keep = pairs.filter((p) => p.stored && p.value === "").map((p) => p.key);
  const changed = values.length > 0 || removed.length > 0;
  const empty = values.length + keep.length === 0;
  const problem = duplicate
    ? `${duplicate} is listed twice.`
    : incomplete.length > 0
      ? "Every new name needs a name and a value."
      : empty
        ? "A credential needs at least one name."
        : null;

  async function save() {
    setSaving(true);
    try {
      await api.updateCredential(credential.id, {
        values: Object.fromEntries(values.map((p) => [p.key.trim(), p.value])),
        keep,
      });
      toast.success("Values replaced", {
        description: "Attached apps pick them up at their next start.",
      });
      onSaved();
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t replace the values in “${credential.name}”`);
    } finally {
      setSaving(false);
    }
  }

  const update = (i: number, patch: Partial<Pair>) =>
    setPairs(pairs.map((x, j) => (i === j ? { ...x, ...patch } : x)));

  return (
    <Dialog open onOpenChange={(o) => { if (!saving) onOpenChange(o); }}>
      <DialogContent scrollable>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving && changed && !problem) void save();
          }}
        >
          <DialogHeader>
            <DialogTitle>Replace the values in {credential.name}</DialogTitle>
            <DialogDescription>
              These become environment variables in every attached app. Values are encrypted and
              never shown again.
            </DialogDescription>
          </DialogHeader>

          <DialogBody className="space-y-4">
            <Notice tone="attention" size="sm">
              {credential.app_count === 0
                ? "Nothing is attached to this yet, so nothing is affected."
                : `${credential.app_count} ${credential.app_count === 1 ? "app carries" : "apps carry"} this. Nothing restarts; they pick up the new values at their next start.`}
            </Notice>

            <div className="space-y-2">
              <span className="text-sm font-medium">Values</span>
              <p className="text-xs text-muted-foreground">
                Leave a value blank to keep it. Remove a name with × to drop it from attached apps.
              </p>
              {pairs.map((p, i) => (
                <div key={i} className="flex items-center gap-2">
                  <Input
                    aria-label={`Name ${i + 1}`}
                    value={p.key}
                    placeholder="SNOWFLAKE_PASSWORD"
                    className="font-mono text-xs"
                    // A stored name is the thing being rotated, not a field:
                    // renaming it is dropping it and adding another.
                    readOnly={p.stored}
                    onChange={(e) => update(i, { key: e.target.value })}
                  />
                  <Input
                    aria-label={p.stored ? `New value for ${p.key}` : `Value ${i + 1}`}
                    type="password"
                    value={p.value}
                    placeholder={p.stored ? "•••••••• (unchanged)" : "The value"}
                    className="font-mono text-xs"
                    onChange={(e) => update(i, { value: e.target.value })}
                  />
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon-xs"
                    aria-label={`Remove ${p.key.trim() || `name ${i + 1}`}`}
                    onClick={() => setPairs(pairs.filter((_, j) => j !== i))}
                  >
                    <X className="h-3.5 w-3.5" />
                  </Button>
                </div>
              ))}
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setPairs([...pairs, { key: "", value: "", stored: false }])}
              >
                <Plus className="h-4 w-4" />
                Add another
              </Button>
              {removed.length > 0 && (
                <Notice tone="attention" size="sm" live>
                  {removed.join(", ")} will be removed from every attached app.
                </Notice>
              )}
              {problem && changed && (
                <p role="alert" className={cn("text-xs", toneText.danger)}>
                  {problem}
                </p>
              )}
            </div>
          </DialogBody>

          <DialogFooter>
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || !changed || !!problem}>
              {saving ? "Replacing…" : "Replace values"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ── Delete ──────────────────────────────────────────────────

/**
 * A plain confirm, not type-to-confirm: irreversible, but bounded to one named
 * row. **While any app still holds it the dialog offers no Delete at all** —
 * the server would refuse it, and a destructive button in front of a
 * guaranteed refusal is a question whose only answer is an error toast. The
 * page already knows the count, so the dialog says what has to happen first.
 */
export function DeleteCredentialDialog({
  credential,
  onOpenChange,
  onDeleted,
}: {
  credential: Credential;
  onOpenChange: (o: boolean) => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const held = credential.app_count;

  async function remove() {
    setBusy(true);
    try {
      await api.deleteCredential(credential.id);
      onDeleted();
      onOpenChange(false);
      toast.success(`“${credential.name}” deleted`);
    } catch (err) {
      // The server names the apps when it refuses; that message is better than
      // anything this dialog could compose.
      toast.error(err instanceof Error ? err.message : `Couldn’t delete “${credential.name}”`);
    } finally {
      setBusy(false);
    }
  }

  if (held > 0) {
    return (
      <Dialog open onOpenChange={onOpenChange}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{credential.name} can’t be deleted yet</DialogTitle>
            <DialogDescription>
              {held} {held === 1 ? "app still holds" : "apps still hold"} this. Detach{" "}
              {held === 1 ? "it" : "them"} first.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => onOpenChange(false)}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={(o) => { if (!busy) onOpenChange(o); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete “{credential.name}”?</DialogTitle>
          <DialogDescription>
            The stored values can’t be recovered.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={busy} onClick={() => void remove()}>
            {busy ? "Deleting…" : "Delete"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

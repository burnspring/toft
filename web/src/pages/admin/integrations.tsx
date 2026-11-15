import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, Download, Plus, Send, Trash2, X } from "lucide-react";
import { toast } from "sonner";
import { Link } from "react-router-dom";

import { LicenseLock } from "@/components/license-lock";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { Notice } from "@/components/notice";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { PageHeader } from "@/components/page-header";
import { Separator } from "@/components/ui/separator";
import {
  api,
  type IntegrationConnection,
  type IntegrationKind,
  type IntegrationSettings,
} from "@/lib/api";
import { DestinationSelect } from "@/components/destination-select";
import { toneSurface } from "@/lib/tone";
import { Section, SectionHeading } from "@/components/section";
import { RelativeTime } from "@/components/relative-time";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { TableCard } from "@/components/table-card";
import { UnreadableCredentialNotice } from "@/components/unreadable-credential";
import { FieldError, FieldHelp } from "@/components/field-help";
import { FieldRow } from "@/components/field-row";
import { fieldErrorId } from "@/lib/utils";
import { credentialPlaceholder } from "@/lib/credentials";
import { useHashScroll } from "@/hooks/use-hash-scroll";

/**
 * A server refusal, ended so the sentence after it does not run into it.
 *
 * The API's messages deliberately carry no trailing full stop — they are
 * composed to be quoted inside other sentences — so a page that puts one after
 * another has to supply the break itself.
 */
function sentence(text: string): string {
  const trimmed = text.trim();
  if (!trimmed) return "";
  return /[.!?]$/.test(trimmed) ? trimmed : trimmed + ".";
}

/**
 * Integrations — the outbound surface, as a catalog rather than a page per
 * provider.
 *
 * What is on screen mirrors what the binary knows: the catalog comes from the
 * server (`appint.Known`), so a kind this build does not have cannot be
 * rendered and a kind it does have cannot be forgotten. The old page hard-coded
 * a list with three "not available yet" rows in it; that list drifted the day
 * anything shipped.
 *
 * Three things the copy has to carry, because getting them wrong is how people
 * misconfigure this:
 *
 *   - **A connection is welded to its scope.** The workspace, the site, the URL
 *     is fixed when it is made. Changing it under live apps would re-point them
 *     silently, so a new scope is a new connection.
 *   - **An app has no integration until it is attached** on that app's own tab.
 *     Not "off" — absent.
 *   - **Detaching reaches nothing already written.** Where a kind leaves files
 *     behind, this page is where an administrator can see what is out there.
 */
export function AdminIntegrationsPage() {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState<string | null>(null);
  const [adding, setAdding] = useState<IntegrationKind | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["integration-connections"],
    queryFn: api.listIntegrationConnections,
    retry: false,
  });
  // `/admin/integrations#slack` from ⌘K lands on the card, once there is one.
  useHashScroll(!!data);

  const header = (
    <PageHeader
      title="Integrations"
      description="Chat, ticketing and file services this install and its apps can send to."
    />
  );

  if (error) {
    return (
      <div className="space-y-6">
        {header}
        <QueryError
          title="Integrations could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  if (isLoading || !data) {
    return (
      <div>
        {header}
        <Skeleton className="h-64 w-full rounded-lg" />
      </div>
    );
  }

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["integration-connections"] });
    // An app's own tab reads the same facts to decide whether a card exists.
    queryClient.invalidateQueries({ queryKey: ["app-integrations"] });
  };

  return (
    <div className="space-y-8">
      {header}

      <LicenseLock feature="integrations" />

      <Section
        id="connections"
        title="Connections"
        description="One credential for one place. Each connection is attached to apps separately."
        className="divide-y divide-border/60 overflow-hidden"
      >
        {data.connections.length === 0 ? (
          <EmptyState
            size="inline"
            title="Nothing is connected yet"
            description="Add one below, then attach it to an app on that app’s Integrations tab."
          />
        ) : (
          data.connections.map((conn) => {
            const kind = data.catalog.find((k) => k.name === conn.kind);
            return (
              <ConnectionRow
                key={conn.id}
                conn={conn}
                kind={kind}
                open={open === conn.id}
                onToggle={() => setOpen(open === conn.id ? null : conn.id)}
                onSaved={refresh}
              />
            );
          })
        )}
      </Section>

      <Section
        id="catalog"
        title="What you can connect"
        className="divide-y divide-border/60 overflow-hidden"
      >
        {data.catalog.map((kind) => (
          <div key={kind.name} className="flex items-center gap-3 px-4 py-3">
            <div className="min-w-0 flex-1">
              <span className="text-sm font-medium">{kind.title}</span>
              <p className="text-xs text-muted-foreground">{kind.summary}</p>
            </div>
            {kind.leaves_residue && (
              <Badge variant="outline" className={`text-2xs ${toneSurface.attention}`}>
                Leaves files behind
              </Badge>
            )}
            <Button size="sm" variant="outline" onClick={() => setAdding(kind)}>
              <Plus className="mr-1.5 h-3.5 w-3.5" />
              Connect
            </Button>
          </div>
        ))}
      </Section>

      <LedgerSettings />

      {adding && (
        <AddConnectionDialog
          key={adding.name}
          kind={adding}
          onClose={() => setAdding(null)}
          onCreated={() => {
            setAdding(null);
            refresh();
          }}
        />
      )}
    </div>
  );
}

/**
 * One connection: what it reaches, whether apps and the platform may use it,
 * and what it has left in somebody else's system.
 *
 * The status word carries the state that matters at a glance — an administrator
 * scanning this page is asking "is anything on", and making them expand each one
 * to find out is the failure mode a list like this has.
 */
function ConnectionRow({
  conn,
  kind,
  open,
  onToggle,
  onSaved,
}: {
  conn: IntegrationConnection;
  kind?: IntegrationKind;
  open: boolean;
  onToggle: () => void;
  onSaved: () => void;
}) {
  // A credential this install cannot decrypt is not a connection, however on
  // the switch is — and "no status is ever a lie" is the rule this row would
  // otherwise break at exactly the moment somebody needs to believe it.
  // Mounted on first open and kept mounted after, hidden when closed: the
  // panel holds typed edits, and unmounting it on collapse — or when another
  // row opened — threw them away without a word.
  const [opened, setOpened] = useState(open);
  if (open && !opened) setOpened(true);

  const status = conn.secret_unreadable
    ? { label: "Credential unreadable", tone: "danger" as const }
    : !conn.for_apps && !conn.for_platform
      ? { label: "Off", tone: "attention" as const }
      : { label: "On", tone: "ok" as const };

  return (
    <div>
      <button
        type="button"
        onClick={onToggle}
        className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-muted/40"
        aria-expanded={open}
      >
        <ChevronDown
          className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${open ? "" : "-rotate-90"}`}
        />
        <div className="min-w-0 flex-1">
          <span className="text-sm font-medium">{conn.name}</span>
          {/* The kind only when the name is not already it — a connection
              left at its default name read "Email" over "Email". */}
          {(() => {
            const facts = [
              conn.name.trim().toLowerCase() === conn.title.toLowerCase() ? "" : conn.title,
              conn.scope_label ?? "",
              conn.apps > 0 ? `${conn.apps} app${conn.apps === 1 ? "" : "s"}` : "",
            ].filter(Boolean);
            return facts.length > 0 ? (
              <p className="truncate text-xs text-muted-foreground">{facts.join(" · ")}</p>
            ) : null;
          })()}
          {conn.description && (
            <p className="truncate text-xs text-muted-foreground">{conn.description}</p>
          )}
        </div>
        <Badge variant="outline" className={`text-2xs ${toneSurface[status.tone]}`}>
          {status.label}
        </Badge>
      </button>
      {opened && (
        <div className="border-t border-border/60 bg-muted/20 p-4" hidden={!open}>
          <ConnectionPanel conn={conn} kind={kind} onSaved={onSaved} />
        </div>
      )}
    </div>
  );
}

function ConnectionPanel({
  conn,
  kind,
  onSaved,
}: {
  conn: IntegrationConnection;
  kind?: IntegrationKind;
  onSaved: () => void;
}) {
  const [name, setName] = useState(conn.name);
  const [description, setDescription] = useState(conn.description ?? "");
  const [credentials, setCredentials] = useState<Record<string, string>>({});
  const [destinationId, setDestinationId] = useState(conn.default_destination.id);
  const [forApps, setForApps] = useState(conn.for_apps);
  const [forPlatform, setForPlatform] = useState(conn.for_platform);
  const [audiences, setAudiences] = useState<string[]>(conn.platform_audiences);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [confirmTokens, setConfirmTokens] = useState(false);

  const sameAudiences =
    audiences.length === conn.platform_audiences.length &&
    audiences.every((a) => conn.platform_audiences.includes(a));
  const dirty =
    name !== conn.name ||
    description.trim() !== (conn.description ?? "") ||
    Object.values(credentials).some((v) => v !== "") ||
    destinationId !== conn.default_destination.id ||
    forApps !== conn.for_apps ||
    forPlatform !== conn.for_platform ||
    !sameAudiences;
  useUnloadGuard(dirty, "A connection has unsaved changes");

  // Only fetched for a kind that enumerates — there is nothing to ask a welded
  // connection about, and an error toast on first load would read as a broken
  // page.
  const { data: dests, error: destsError } = useQuery({
    queryKey: ["connection-destinations", conn.id],
    queryFn: () => api.listConnectionDestinations(conn.id),
    enabled: !!kind?.enumerates && !conn.secret_unreadable,
    retry: false,
  });

  const { data: placements } = useQuery({
    queryKey: ["connection-placements", conn.id],
    queryFn: () => api.listConnectionPlacements(conn.id),
    enabled: !!kind?.leaves_residue && conn.placements > 0,
    retry: false,
  });

  const save = useMutation({
    mutationFn: () =>
      api.updateIntegrationConnection(conn.id, {
        name: name.trim(),
        description: description.trim(),
        credentials: Object.keys(credentials).length ? credentials : undefined,
        default_destination_id: destinationId,
        default_destination_label:
          dests?.destinations.find((d) => d.id === destinationId)?.label ??
          (destinationId === conn.default_destination.id ? conn.default_destination.label : ""),
        for_apps: forApps,
        for_platform: forPlatform,
        platform_audiences: audiences,
      }),
    onSuccess: () => {
      setCredentials({});
      toast.success("Saved");
      onSaved();
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  // A file-only kind is tested by the one thing it can do: a one-line text
  // file, toft-test.txt, lands in the default destination.
  const carriesFile = kind?.actions[0]?.carries === "file";
  const test = useMutation({
    mutationFn: () => api.testIntegrationConnection(conn.id),
    onSuccess: () =>
      toast.success(
        `${carriesFile ? "Uploaded toft-test.txt to" : "Sent to"} ${conn.default_destination.label || conn.default_destination.id || conn.scope_label || "this connection"}`,
      ),
    // The far side's own words, which is the sentence an administrator
    // debugging this actually needs.
    onError: (e: Error) => toast.error(e.message, { duration: 10000 }),
  });

  // The token switch saves on the throw rather than with the form: turning it
  // on is the one change here that widens what an app holds, so it has a
  // confirm of its own, and a confirm in front of a draft would be a question
  // about nothing yet.
  const tokens = useMutation({
    mutationFn: (on: boolean) =>
      api.updateIntegrationConnection(conn.id, { name: conn.name, issue_tokens: on }),
    onSuccess: (_, on) => {
      setConfirmTokens(false);
      toast.success(
        on
          ? `Apps on “${conn.name}” can get access tokens`
          : `Apps on “${conn.name}” can no longer get access tokens`,
      );
      onSaved();
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  const remove = useMutation({
    mutationFn: () => api.deleteIntegrationConnection(conn.id),
    onSuccess: () => {
      setConfirmDelete(false);
      toast.success(`“${conn.name}” removed`);
      onSaved();
    },
    onError: (e: Error) => {
      setConfirmDelete(false);
      toast.error(e.message, { duration: 10000 });
    },
  });

  if (!kind) {
    return (
      <Notice tone="attention" size="sm" title="This connection is for an integration this version does not have">
        It was made by a newer Toft. Nothing sends until you upgrade.
      </Notice>
    );
  }

  const menuUnavailable = !!destsError || (dests?.destinations.length ?? 0) === 0;

  return (
    <div className="space-y-6">
      <TableCard flat>
        <div className="space-y-5 p-4">
          <div className="space-y-2">
            <Label htmlFor={`name-${conn.id}`}>Name</Label>
            <Input
              id={`name-${conn.id}`}
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="max-w-sm"
            />
          </div>

          <DescriptionField
            id={`description-${conn.id}`}
            kind={kind}
            value={description}
            onChange={setDescription}
          />
          {!conn.description && kind.description_missing && (
            <Notice tone="attention" size="sm" title="Nobody is named as this connection’s owner">
              {kind.description_missing}
            </Notice>
          )}

          <div className="space-y-1">
            <Label>{kind.scope_label[0].toUpperCase() + kind.scope_label.slice(1)}</Label>
            <p className="font-mono text-xs text-foreground">
              {conn.scope_label || conn.scope_id || "—"}
            </p>
            {/* Welded: changing it would re-point every app on it silently. */}
            <FieldHelp>
              Set when the connection was made. To use another {kind.scope_label}, add a new
              connection.
            </FieldHelp>
          </div>

          {kind.credentials.map((field) => (
            <div key={field.key} className="space-y-2">
              <Label htmlFor={`${conn.id}-${field.key}`}>{field.label}</Label>
              <Input
                id={`${conn.id}-${field.key}`}
                type={field.secret ? "password" : "text"}
                value={credentials[field.key] ?? ""}
                onChange={(e) =>
                  setCredentials({ ...credentials, [field.key]: e.target.value })
                }
                placeholder={
                  field.secret
                    ? credentialPlaceholder(conn.secret_set, conn.secret_unreadable, "")
                    : ""
                }
                className="max-w-sm font-mono text-xs"
                autoComplete="off"
              />
              {field.help && <FieldHelp>{field.help}</FieldHelp>}
            </div>
          ))}
          {conn.secret_unreadable && <UnreadableCredentialNotice what="credential" />}

          {kind.destination_label && (
            <div className="space-y-2">
              <Label htmlFor={`dest-${conn.id}`}>
                Default {kind.destination_label}
              </Label>
              {kind.enumerates && !menuUnavailable ? (
                <DestinationSelect
                  id={`dest-${conn.id}`}
                  value={destinationId}
                  onChange={setDestinationId}
                  destinations={dests?.destinations ?? []}
                  placeholder={`Choose a ${kind.destination_label}…`}
                  hash={kind.name === "slack"}
                />
              ) : (
                <Input
                  id={`dest-${conn.id}`}
                  value={destinationId}
                  onChange={(e) => setDestinationId(e.target.value)}
                  className="max-w-sm font-mono text-xs"
                />
              )}
              {kind.enumerates && menuUnavailable && (
                <FieldHelp tone="attention">
                  {sentence(
                    destsError
                      ? (destsError as Error).message
                      : `Nothing to choose from yet — Toft can only reach ${kind.destination_label}s it has been given access to`,
                  )}{" "}
                  {kind.name === "teamsbot"
                    ? "Add a team or chat below first."
                    : "Paste its ID instead."}
                </FieldHelp>
              )}
              <FieldHelp>
                Where {kind.leaves_residue ? "a file lands" : "a message goes"} when an app
                hasn’t chosen one.
              </FieldHelp>
            </div>
          )}

          <Separator />

          <div className="flex items-start justify-between gap-6">
            <div className="space-y-1">
              <Label htmlFor={`apps-${conn.id}`} className="text-sm font-medium">
                Apps can send through this
              </Label>
              <FieldHelp>
                Attach it on an app’s Integrations tab. When off, every attached app stops
                sending immediately.
              </FieldHelp>
            </div>
            <Switch
              id={`apps-${conn.id}`}
              checked={forApps}
              onCheckedChange={setForApps}
              aria-label="Apps can send through this connection"
            />
          </div>

          {kind.actions.some((a) => a.name === "token") && (
            <div className="space-y-3">
              <div className="flex items-start justify-between gap-6">
                <div className="space-y-1">
                  <Label htmlFor={`tokens-${conn.id}`} className="text-sm font-medium">
                    Let apps get access tokens
                  </Label>
                  <FieldHelp>
                    Apps call {kind.name === "gdrive" ? "Google’s" : "Microsoft’s"} own SDK
                    with a short-lived token. It reaches the whole grant, not only their folder.
                  </FieldHelp>
                </div>
                <Switch
                  id={`tokens-${conn.id}`}
                  checked={conn.issue_tokens}
                  disabled={tokens.isPending}
                  onCheckedChange={(on) => (on ? setConfirmTokens(true) : tokens.mutate(false))}
                  aria-label="Let apps get access tokens"
                />
              </div>
              {conn.issue_tokens && conn.token_problem && (
                <Notice tone="danger" size="sm" title="Apps can’t get a token from this connection">
                  {conn.token_problem}
                </Notice>
              )}
            </div>
          )}

          {kind.platform_audiences.length > 0 && (
            <div className="space-y-3 rounded-lg border border-border/60 p-3">
              <div className="flex items-start justify-between gap-6">
                <div className="space-y-1">
                  <p className="text-sm font-medium">Toft’s own notifications</p>
                  <FieldHelp>
                    Platform notifications, sent here as well as by email.
                  </FieldHelp>
                </div>
                <Switch
                  checked={forPlatform}
                  onCheckedChange={setForPlatform}
                  aria-label="Toft’s own notifications"
                />
              </div>
              {forPlatform &&
                kind.platform_audiences.map((audience) => (
                  <div
                    key={audience}
                    className="flex items-start justify-between gap-6 border-t border-border/60 pt-3"
                  >
                    <div className="space-y-1">
                      <p className="text-sm font-medium">
                        {audience === "admin"
                          ? "For administrators"
                          : "About apps"}
                      </p>
                      <p className="text-xs text-muted-foreground">
                        {audience === "admin"
                          ? "Publisher access requests and source changes. These always go to the default destination."
                          : "Failures and recoveries, in each app’s own destination. When off, every app is silenced."}
                      </p>
                    </div>
                    <Switch
                      checked={audiences.includes(audience)}
                      onCheckedChange={(v) =>
                        setAudiences(
                          v
                            ? [...audiences, audience]
                            : audiences.filter((a) => a !== audience),
                        )
                      }
                      aria-label={
                        audience === "admin"
                          ? "Post notifications for administrators"
                          : "Post notifications about apps"
                      }
                    />
                  </div>
                ))}
            </div>
          )}

          <div className="flex flex-wrap items-center gap-2 pt-1">
            <Button size="sm" onClick={() => save.mutate()} disabled={save.isPending || !dirty || description.trim().length > MAX_DESCRIPTION}>
              {save.isPending ? "Saving…" : "Save"}
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => test.mutate()}
              // The test uses the saved connection, so it waits while there are
              // edits: a success against the stale copy read as proof of them.
              disabled={test.isPending || conn.secret_unreadable || dirty}
            >
              <Send className="mr-1.5 h-3.5 w-3.5" />
              {test.isPending
                ? carriesFile
                  ? "Uploading…"
                  : "Sending…"
                : carriesFile
                  ? "Upload a test file"
                  : "Send a test message"}
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => setConfirmDelete(true)}
              disabled={remove.isPending}
            >
              <Trash2 className="mr-1.5 h-3.5 w-3.5" />
              Remove…
            </Button>
          </div>
          {dirty && (
            <p className="text-xs text-muted-foreground">
              Save before sending a test.
            </p>
          )}
        </div>
      </TableCard>

      {kind.name === "teamsbot" && <TeamsBotPlaces conn={conn} />}

      {kind.leaves_residue && (
        <TableCard flat>
          <div className="border-b border-border/60 p-4">
            <SectionHeading
              level={3}
              title="What apps have put there"
              description="Every file Toft placed. Toft never removes them, even when an app is detached or deleted."
            />
          </div>
          {conn.placements === 0 ? (
            <EmptyState size="inline" title="No files yet" />
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>File</TableHead>
                  <TableHead className="hidden @xl:table-cell">App</TableHead>
                  <TableHead className="hidden @3xl:table-cell">Where</TableHead>
                  <TableHead className="w-[160px]">Placed</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(placements?.placements ?? []).map((p) => (
                  <TableRow key={p.id}>
                    <TableCell className="font-medium">
                      {p.item_url ? (
                        <a
                          href={p.item_url}
                          target="_blank"
                          rel="noreferrer"
                          className="text-primary hover:underline"
                        >
                          {p.name}
                        </a>
                      ) : (
                        p.name
                      )}
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                      {p.app_name || "—"}
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                      {p.destination.label || p.destination.id || "—"}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      <RelativeTime iso={p.placed_at} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </TableCard>
      )}

      <Dialog
        open={confirmTokens}
        onOpenChange={(o) => { if (!tokens.isPending) setConfirmTokens(o); }}
      >
        <DialogContent scrollable className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Let apps on “{conn.name}” get access tokens?</DialogTitle>
            <DialogDescription>
              A token reaches everything this connection’s grant reaches
              {kind.name === "sharepoint" ? " — the whole site" : ""}, not only the folder
              chosen for each app. Each one works for up to an hour, even after this is
              switched off or an app is detached.
            </DialogDescription>
          </DialogHeader>
          <DialogBody>
            {conn.app_names.length === 0 ? (
              <p className="text-sm text-muted-foreground">No apps are attached yet.</p>
            ) : (
              <div className="space-y-2">
                <p className="text-sm">
                  {conn.app_names.length === 1 ? "This app" : `These ${conn.app_names.length} apps`}{" "}
                  can then reach each other’s folders:
                </p>
                <ul className="list-disc space-y-0.5 pl-5 text-sm">
                  {conn.app_names.map((n, i) => (
                    <li key={`${n}-${i}`}>{n || "Deleted app"}</li>
                  ))}
                </ul>
              </div>
            )}
          </DialogBody>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={tokens.isPending}
              onClick={() => setConfirmTokens(false)}
            >
              Cancel
            </Button>
            <Button onClick={() => tokens.mutate(true)} disabled={tokens.isPending}>
              {tokens.isPending ? "Turning on…" : "Turn on"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* While apps are attached the server refuses the remove, so the dialog
          says what has to happen first and offers no destructive button. */}
      <Dialog
        open={confirmDelete}
        onOpenChange={(o) => { if (!remove.isPending) setConfirmDelete(o); }}
      >
        <DialogContent>
          {conn.apps > 0 ? (
            <>
              <DialogHeader>
                <DialogTitle>{conn.name} can’t be removed yet</DialogTitle>
                <DialogDescription>
                  {conn.apps} {conn.apps === 1 ? "app is" : "apps are"} attached to it. Detach{" "}
                  {conn.apps === 1 ? "it" : "them"} on each app’s Integrations tab first.
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button variant="outline" onClick={() => setConfirmDelete(false)}>
                  Close
                </Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <DialogHeader>
                <DialogTitle>Remove “{conn.name}”?</DialogTitle>
                <DialogDescription>
                  Toft forgets the credential. Messages and files already sent are untouched
                  {conn.placements > 0
                    ? ` — the ${conn.placements} file${conn.placements === 1 ? "" : "s"} above stay where they are.`
                    : "."}
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button
                  variant="outline"
                  disabled={remove.isPending}
                  onClick={() => setConfirmDelete(false)}
                >
                  Cancel
                </Button>
                <Button
                  variant="destructive"
                  onClick={() => remove.mutate()}
                  disabled={remove.isPending}
                >
                  {remove.isPending ? "Removing…" : "Remove"}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}

/** Adding a connection: the credential, and the one place it is allowed to
 * reach. The scope is asked for here and nowhere else, because it is welded. */
/**
 * Where a Teams bot may post. The bot is told nothing by Teams — Toft has no
 * inbound endpoint — so an administrator installs the app in Teams and pastes
 * the team's or chat's link here; the server saves it only once Teams answers
 * for it, which it does only where the app is installed.
 */
function TeamsBotPlaces({ conn }: { conn: IntegrationConnection }) {
  const queryClient = useQueryClient();
  const [link, setLink] = useState("");
  const [chatName, setChatName] = useState("");
  const isChat = /\/l\/chat\//.test(link);

  const { data, error, isLoading, refetch } = useQuery({
    queryKey: ["integration-places", conn.id],
    queryFn: () => api.listIntegrationPlaces(conn.id),
    retry: false,
  });
  const changed = () => {
    queryClient.invalidateQueries({ queryKey: ["integration-places", conn.id] });
    queryClient.invalidateQueries({ queryKey: ["connection-destinations", conn.id] });
  };
  const add = useMutation({
    mutationFn: () => api.addIntegrationPlace(conn.id, link.trim(), chatName.trim() || undefined),
    onSuccess: (p) => {
      setLink("");
      setChatName("");
      toast.success(`Added “${p.name}”`);
      changed();
    },
    onError: (e: Error) => toast.error(e.message, { duration: 10000 }),
  });
  const remove = useMutation({
    mutationFn: (placeId: string) => api.deleteIntegrationPlace(conn.id, placeId),
    onSuccess: () => {
      toast.success("Removed");
      changed();
    },
    onError: (e: Error) => toast.error(e.message, { duration: 10000 }),
  });

  return (
    <TableCard flat>
      <div className="space-y-3 border-b border-border/60 p-4">
        <SectionHeading
          level={3}
          title="Where this bot can post"
          description="Install the Toft app in Teams, add it to a team or chat, then paste that team’s or chat’s link here."
          actions={
            <a
              href={api.teamsAppPackageUrl(conn.id)}
              download
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              <Download className="mr-1.5 h-3.5 w-3.5" />
              Download Teams app
            </a>
          }
        />
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (link.trim() && !add.isPending) add.mutate();
          }}
        >
          <div className="min-w-0 flex-1 space-y-1.5">
            <Label htmlFor={`place-link-${conn.id}`}>Team or chat link</Label>
            <Input
              id={`place-link-${conn.id}`}
              value={link}
              onChange={(e) => setLink(e.target.value)}
              placeholder="https://teams.microsoft.com/l/team/…"
              className="font-mono text-xs md:text-xs"
              autoComplete="off"
            />
          </div>
          {isChat && (
            <div className="w-full space-y-1.5 sm:w-56">
              <Label htmlFor={`place-name-${conn.id}`}>Chat name</Label>
              <Input
                id={`place-name-${conn.id}`}
                value={chatName}
                onChange={(e) => setChatName(e.target.value)}
                autoComplete="off"
              />
            </div>
          )}
          <Button type="submit" size="sm" disabled={!link.trim() || add.isPending}>
            {add.isPending ? "Checking…" : "Add"}
          </Button>
        </form>
        <FieldHelp>
          On a team: ⋯ › Get link to team. In a chat: ⋯ › Copy link. A channel’s link won’t
          do — every channel of the team is offered once the team is added.
        </FieldHelp>
      </div>
      {error ? (
        <div className="p-4">
          <QueryError
            title="The places could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => refetch()}
          />
        </div>
      ) : isLoading ? (
        <div className="p-4">
          <Skeleton className="h-8 w-full" />
        </div>
      ) : (data?.places ?? []).length === 0 ? (
        <EmptyState
          size="inline"
          title="No teams or chats yet"
          description="Apps can’t be given a Teams channel until one is added here."
        />
      ) : (
        <ul className="divide-y divide-border/60">
          {(data?.places ?? []).map((p) => (
            <li key={p.id} className="flex items-center gap-3 px-4 py-2.5">
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium">{p.name}</p>
                <p className="truncate text-xs text-muted-foreground">
                  {p.kind === "team" ? "Team" : "Group chat"}
                  {!p.reachable && " · Toft was removed from it in Teams"}
                </p>
              </div>
              {!p.reachable && (
                <Badge variant="outline" className={`text-2xs ${toneSurface.attention}`}>
                  Not installed
                </Badge>
              )}
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={`Remove ${p.name}`}
                disabled={remove.isPending}
                onClick={() => remove.mutate(p.id)}
              >
                <X className="h-3.5 w-3.5" />
              </Button>
            </li>
          ))}
        </ul>
      )}
    </TableCard>
  );
}

/** The bound the server holds a description to. */
const MAX_DESCRIPTION = 1000;

/**
 * A connection's description: optional, editable, the administrator's own
 * words. The kind says what is worth writing; Teams asks who owns the
 * workflow, because nothing Toft can read says so.
 */
function DescriptionField({
  id,
  kind,
  value,
  onChange,
}: {
  id: string;
  kind: IntegrationKind;
  value: string;
  onChange: (v: string) => void;
}) {
  const tooLong = value.trim().length > MAX_DESCRIPTION;
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>Description (optional)</Label>
      <Textarea
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        rows={2}
        aria-invalid={tooLong || undefined}
        aria-describedby={tooLong ? fieldErrorId(id) : undefined}
      />
      {tooLong ? (
        <FieldError field={id}>
          At most {MAX_DESCRIPTION} characters — this is {value.trim().length}.
        </FieldError>
      ) : (
        <FieldHelp>
          {kind.description_hint ?? "What this connection is for, and who to ask about it."}
        </FieldHelp>
      )}
    </div>
  );
}

function AddConnectionDialog({
  kind,
  onClose,
  onCreated,
}: {
  kind: IntegrationKind;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState(kind.title);
  const [description, setDescription] = useState("");
  const [scopeId, setScopeId] = useState("");
  const [fields, setFields] = useState<Record<string, string>>({});

  const create = useMutation({
    mutationFn: () => {
      const config: Record<string, string> = {};
      const credentials: Record<string, string> = {};
      kind.credentials.forEach((f) => {
        const v = (fields[f.key] ?? "").trim();
        if (!v) return;
        if (f.secret) credentials[f.key] = v;
        else config[f.key] = v;
      });
      return api.createIntegrationConnection({
        kind: kind.name,
        name: name.trim(),
        description: description.trim(),
        scope_id: scopeId.trim(),
        config,
        credentials,
      });
    },
    onSuccess: () => {
      toast.success(`${kind.title} connected`);
      onCreated();
    },
    onError: (e: Error) => toast.error(e.message, { duration: 10000 }),
  });

  const missing =
    kind.credentials.some((f) => f.required && !(fields[f.key] ?? "").trim()) ||
    description.trim().length > MAX_DESCRIPTION;

  return (
    <Dialog open onOpenChange={(v) => { if (!v && !create.isPending) onClose(); }}>
      <DialogContent scrollable>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!create.isPending && !missing) create.mutate();
          }}
        >
        <DialogHeader>
          <DialogTitle>Connect {kind.title}</DialogTitle>
          <DialogDescription>{kind.summary}</DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-5">
          <div className="space-y-2">
            <Label htmlFor="new-conn-name">Name</Label>
            <Input
              id="new-conn-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={kind.title}
            />
            <FieldHelp>
              How people tell two {kind.title} connections apart.
            </FieldHelp>
          </div>

          <DescriptionField
            id="new-conn-description"
            kind={kind}
            value={description}
            onChange={setDescription}
          />

          {kind.scope_hint && (
            <div className="space-y-2">
              <Label htmlFor="new-conn-scope">
                {kind.scope_label[0].toUpperCase() + kind.scope_label.slice(1)}
              </Label>
              <Input
                id="new-conn-scope"
                value={scopeId}
                onChange={(e) => setScopeId(e.target.value)}
                className="font-mono text-xs"
                autoComplete="off"
              />
              <FieldHelp>{kind.scope_hint}</FieldHelp>
            </div>
          )}

          {kind.credentials.map((f) => (
            <div key={f.key} className="space-y-2">
              <Label htmlFor={`new-conn-${f.key}`}>{f.label}</Label>
              <Input
                id={`new-conn-${f.key}`}
                type={f.secret ? "password" : "text"}
                value={fields[f.key] ?? ""}
                onChange={(e) => setFields({ ...fields, [f.key]: e.target.value })}
                className="font-mono text-xs"
                autoComplete="off"
              />
              {f.help && <FieldHelp>{f.help}</FieldHelp>}
            </div>
          ))}

          {kind.leaves_residue && (
            <Notice tone="attention" size="sm" title="This one leaves files behind">
              Files apps write stay in your {kind.scope_label}. Toft lists them on the connection
              and never removes them.
            </Notice>
          )}
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={create.isPending} onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={create.isPending || missing}>
            {create.isPending ? "Connecting…" : "Connect"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * How long the record is kept.
 *
 * The ledger's own settings, and only the ledger's — the per-kind switches are
 * the connections themselves. What is here governs the record rather than the
 * capability, which is why it survived the catalog: retention is about how long
 * the install keeps evidence, and that is not a property of any one integration.
 */
/**
 * The bounds, as a sentence rather than as clauses on a disabled button.
 *
 * Local for now; `lib/settings-fields.ts` is where it belongs, and moving it
 * there means giving `retention_days` an entry in the settings index with an
 * anchor and a control id — a larger change than this one.
 */
function retentionError(value: string): string | null {
  const t = value.trim();
  const n = Number(t);
  if (t === "" || !Number.isInteger(n) || n < 30 || n > 3650) {
    return "Must be a whole number from 30 to 3650.";
  }
  return null;
}

function LedgerSettings() {
  const queryClient = useQueryClient();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["integration-settings"],
    queryFn: api.getIntegrationSettings,
    retry: false,
  });
  /** The typed retention, or `null` for untouched. The two were one value
   * before — an empty box meant "as stored" — which made the field impossible
   * to clear: every keystroke that emptied it put the stored number straight
   * back. */
  const [days, setDays] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (body: Partial<IntegrationSettings>) => api.updateIntegrationSettings(body),
    onSuccess: (_res, sent) => {
      queryClient.invalidateQueries({ queryKey: ["integration-settings"] });
      // Only when the retention went out: the switch saves through this same
      // mutation on the throw, and clearing here then threw away a typed,
      // unsaved number under a "Saved" toast.
      if ("retention_days" in sent) setDays(null);
      toast.success("Saved");
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  if (error) {
    return (
      <QueryError
        title="The message log settings could not be read"
        error={error}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={() => void refetch()}
      />
    );
  }
  if (isLoading || !data) return <Skeleton className="h-48 w-full rounded-lg" />;

  const daysError = retentionError(days ?? String(data.retention_days));

  return (
    <Section
      id="log"
      title="Message log"
      description={
        <>
          Every message an app sends is recorded with its recipients. See the{" "}
          <Link to="/admin/integration-log" className="underline underline-offset-2 hover:text-foreground">
            Integration log
          </Link>
          .
        </>
      }
    >
      <div className="divide-y divide-border/60 px-4 [&>*]:py-4">
        <FieldRow
          label="Keep message bodies"
          htmlFor="integrations-retain-bodies"
          help="When off, the log keeps a checksum, not the text. Bodies are cleared after 30 days either way."
        >
          <Switch
            id="integrations-retain-bodies"
            checked={data.retain_bodies}
            onCheckedChange={(v) => save.mutate({ retain_bodies: v })}
            disabled={save.isPending}
            aria-label="Keep message bodies"
          />
        </FieldRow>

        <FieldRow
          label="Keep the log for"
          htmlFor="retention-days"
          help="At least 30 days. A record of a file placed somewhere is kept regardless."
          extra={<FieldError field="retention-days">{daysError}</FieldError>}
        >
          <div className="flex items-center gap-2">
            <Input
              id="retention-days"
              type="number"
              min={30}
              max={3650}
              className="w-24"
              value={days ?? String(data.retention_days)}
              aria-invalid={!!daysError}
              aria-describedby={daysError ? fieldErrorId("retention-days") : undefined}
              onChange={(e) => setDays(e.target.value)}
            />
            <span className="text-sm text-muted-foreground">days</span>
            {days !== null && Number(days) !== data.retention_days && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => days !== null && save.mutate({ retention_days: Number(days) })}
                // Seven clauses became two, and the six bounds checks that used
                // to be spelled here are the sentence beside the field instead.
                // A disabled control whose reason is nowhere reachable is what
                // the quality floor names by name.
                //
                // `days === null` is *untouched*, and it has to be tested for
                // rather than folded into the comparison: `Number(null)` is 0,
                // so on an untouched card the button was live and posted a
                // retention of zero days.
                disabled={
                  save.isPending ||
                  days === null ||
                  !!daysError ||
                  Number(days) === data.retention_days
                }
              >
                {save.isPending ? "Saving…" : "Save"}
              </Button>
            )}
          </div>
        </FieldRow>
      </div>
    </Section>
  );
}

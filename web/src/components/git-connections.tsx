import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, Copy, Loader2, MoreHorizontal, Plus } from "lucide-react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import { copyToClipboard } from "@/lib/clipboard";
import type {
  GitConnection, GitConnectionApp, GitConnectionInput, GitConnectionWithSecret,
} from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { FieldLabel, Label } from "@/components/ui/label";
import { Link } from "react-router-dom";
import { Textarea } from "@/components/ui/textarea";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { Notice } from "@/components/notice";
import { SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { toneSurface, toneText } from "@/lib/tone";
import { formatDay } from "@/lib/time";
import { RelativeTime } from "@/components/relative-time";
import { UnreadableCredentialNotice } from "@/components/unreadable-credential";
import { FieldHelp } from "@/components/field-help";
import { credentialPlaceholder } from "@/lib/credentials";
import { OnceOnlySecret } from "@/components/once-only-secret";

/**
 * Admin → Sources, the connections half.
 *
 * The source policy above says which repositories this install will deploy
 * from; this says how it reaches them. They are one page because they are one
 * question, and this is the half that removes a credential from every app that
 * would otherwise carry its own.
 *
 * What the section has to make visible is what the weaker credential gives up —
 * provenance, reach, expiry — because an administrator choosing a pasted token
 * over a GitHub App should be able to see what they chose. The reach sentence
 * comes from the server, derived from the token's own prefix, so nobody is
 * asked a question about a value the platform is already holding.
 */
export function GitConnectionsSection() {
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<GitConnection | null>(null);
  const [removing, setRemoving] = useState<GitConnection | null>(null);
  // The webhook secret just minted, by setting the webhook up or replacing its
  // secret — the only moment the server ever sends it.
  const [minted, setMinted] = useState<GitConnectionWithSecret | null>(null);

  const {
    data, isLoading, error, refetch,
  } = useQuery({
    queryKey: ["git-connections"],
    queryFn: api.listGitConnections,
  });
  const connections = data?.connections ?? [];

  const [testing, setTesting] = useState<string | null>(null);

  /**
   * One authenticated call to the host, against the credential this install
   * actually holds.
   *
   * A refused credential is a **result**, not a thrown error: the server
   * answers 200 with `ok: false` and the sentence naming the field to fix, so
   * the failure path here is a `toast.error` carrying the server's words rather
   * than a generic "couldn't test". Only a request that could not be made at
   * all lands in `catch`.
   *
   * The refetch is not cosmetic. The server records the result on the row, so
   * the health line below is the same fact the toast just gave — and a pass
   * clears a stale failure that was sitting there from a webhook delivery.
   */
  async function test(conn: GitConnection) {
    setTesting(conn.id);
    try {
      const result = await api.testGitConnection(conn.id);
      if (result.ok) {
        toast.success(result.summary);
      } else {
        toast.error(result.summary);
      }
      refetch();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t test the connection");
    } finally {
      setTesting(null);
    }
  }

  /**
   * Sets the webhook up, or replaces its secret. No confirm: the webhook only
   * shortens the wait, the poller deploys either way, and the new secret is on
   * screen the moment the old one stops working.
   */
  async function rotateSecret(conn: GitConnection) {
    try {
      setMinted(await api.rotateGitConnectionWebhookSecret(conn.id));
      refetch();
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : `Couldn’t set up the webhook for “${conn.display_name}”`,
      );
    }
  }

  /** No confirm, for the same reason: pushes still deploy, on the next poll. */
  async function disableWebhook(conn: GitConnection) {
    try {
      await api.disableGitConnectionWebhook(conn.id);
      toast.success(`Webhook off for “${conn.display_name}” — pushes deploy on the next poll`);
      refetch();
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : `Couldn’t turn off the webhook for “${conn.display_name}”`,
      );
    }
  }

  async function setEnabled(conn: GitConnection, enabled: boolean) {
    try {
      await api.updateGitConnection(conn.id, { enabled });
      // The switch's toast says what is now true, and what it did not do:
      // disabling stops the next clone and never stops a running app.
      toast.success(
        enabled
          ? `“${conn.display_name}” is connected again`
          : `“${conn.display_name}” is disabled — running apps keep running; new deploys through it fail`,
      );
      refetch();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t change “${conn.display_name}”`);
    }
  }

  return (
    <section id="connections" className="scroll-mt-20 space-y-3">
      <SectionHeading
        title="Connected git hosts"
        description={
          <>Connect a git host once, so apps don’t each carry their own access token.</>
        }
        // Not while the list is empty: the empty state carries the one
        // button, and two primaries for one act is none.
        actions={
          connections.length > 0 ? (
            <Button size="sm" onClick={() => setAdding(true)}>
              <Plus className="mr-1.5 size-4" aria-hidden="true" />
              Connect a host
            </Button>
          ) : undefined
        }
      />

      {error ? (
        <QueryError
          error={error}
          title="Connected git hosts could not be read"
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => refetch()}
        />
      ) : isLoading ? (
        <Skeleton className="h-32 w-full" />
      ) : connections.length === 0 ? (
        <TableCard className="p-0">
          <EmptyState
            size="inline"
            title="No connected git hosts"
            action={
              <Button size="sm" onClick={() => setAdding(true)}>
                Connect a host
              </Button>
            }
          />
        </TableCard>
      ) : (
        <TableCard className="overflow-x-auto">
          {/* `table-fixed` with declared widths, and `whitespace-normal` on the
              three cells that hold sentences. TableCell defaults to
              `whitespace-nowrap`, which is right for a slug and wrong for what
              a credential reaches — without both, that sentence runs straight
              across the columns after it. */}
          <Table className="table-fixed">
            <TableHeader>
              <TableRow>
                <TableHead className="w-[30%]">Connection</TableHead>
                <TableHead className="hidden w-[34%] @xl:table-cell">Credential</TableHead>
                <TableHead className="hidden w-[22%] @3xl:table-cell">Covers</TableHead>
                <TableHead className="w-16">Apps</TableHead>
                <TableHead className="w-12 text-right">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {connections.map((conn) => (
                <ConnectionRow
                  key={conn.id}
                  conn={conn}
                  testing={testing === conn.id}
                  onTest={() => test(conn)}
                  onEdit={() => setEditing(conn)}
                  onToggle={(enabled) => setEnabled(conn, enabled)}
                  onRotate={() => rotateSecret(conn)}
                  onDisableWebhook={() => disableWebhook(conn)}
                  onRemove={() => setRemoving(conn)}
                />
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      <AddConnectionDialog
        open={adding}
        onOpenChange={setAdding}
        onAdded={() => refetch()}
      />
      {editing && (
        <AddConnectionDialog
          // Keyed on the row, so opening a second connection remounts the form
          // rather than syncing props into state after the fact.
          key={editing.id}
          open
          connection={editing}
          onOpenChange={() => setEditing(null)}
          onAdded={() => refetch()}
        />
      )}
      {minted && (
        <OnceOnlySecret
          open
          title={`Webhook secret for “${minted.display_name}”`}
          secret={minted.webhook_secret}
          label="Webhook secret"
          description="This is the one time it can be shown. If it is lost, replace it from the connection’s menu."
          onDismiss={() => setMinted(null)}
        >
          <div className="space-y-1.5">
            <Label htmlFor="git-webhook-url">Webhook URL</Label>
            <div className="flex gap-2">
              <Input id="git-webhook-url" readOnly value={minted.webhook_url} className="font-mono text-xs" />
              <Button
                variant="outline"
                size="icon"
                aria-label="Copy the webhook URL"
                onClick={() => void copyToClipboard(minted.webhook_url, "Webhook URL")}
              >
                <Copy className="h-4 w-4" />
              </Button>
            </div>
            <FieldHelp>
              Add both as a push webhook at {KIND_LABEL[minted.kind] ?? minted.kind}, and a push
              deploys in seconds instead of on the next poll. The host has to be able to reach this
              address; if it cannot, turn the webhook off — nothing else depends on it.
            </FieldHelp>
          </div>
        </OnceOnlySecret>
      )}
      <RemoveConnectionDialog
        connection={removing}
        onOpenChange={() => setRemoving(null)}
        onRemoved={() => refetch()}
      />
    </section>
  );
}

const METHOD_LABEL: Record<string, string> = {
  app: "GitHub App",
  token: "Access token",
};

const KIND_LABEL: Record<string, string> = {
  github: "GitHub",
  gitlab: "GitLab",
};

function ConnectionRow({
  conn,
  testing,
  onTest,
  onEdit,
  onToggle,
  onRotate,
  onDisableWebhook,
  onRemove,
}: {
  conn: GitConnection;
  testing: boolean;
  onTest: () => void;
  onEdit: () => void;
  onToggle: (enabled: boolean) => void;
  onRotate: () => void;
  onDisableWebhook: () => void;
  onRemove: () => void;
}) {
  const expired =
    conn.credential_expires_at !== undefined &&
    new Date(conn.credential_expires_at) < new Date();

  return (
    <TableRow>
      <TableCell className="align-top whitespace-normal">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium">{conn.display_name}</span>
            <Badge variant="outline" className="text-2xs">
              {KIND_LABEL[conn.kind] ?? conn.kind}
            </Badge>
            {conn.enabled ? null : (
              <Badge variant="outline" className={`text-2xs ${toneSurface.neutral}`}>
                Disabled
              </Badge>
            )}
            {conn.picker ? (
              <Badge variant="outline" className={`text-2xs ${toneSurface.ok}`}>
                Repository picker
              </Badge>
            ) : null}
          </div>
          <p className="truncate font-mono text-2xs text-muted-foreground">{conn.base_url}</p>
          {/* The webhook is opt-in and buys a minute. Off is the default and a
              supported state — most organizations let no git host call in, and
              polling is what deploys either way — so the URL is offered only
              once an administrator has set one up. */}
          {conn.webhook_secret_set ? (
            <button
              type="button"
              onClick={() => copyToClipboard(conn.webhook_url, "Webhook URL")}
              className="flex items-center gap-1.5 text-left text-2xs text-muted-foreground transition-colors hover:text-foreground"
            >
              <Copy className="size-3 shrink-0" aria-hidden="true" />
              Webhook on · copy its URL
            </button>
          ) : (
            <p className="text-2xs text-muted-foreground">Polls for new commits · no webhook</p>
          )}
          {conn.clone_secret_unreadable ? (
            <UnreadableCredentialNotice
              what="This connection’s credential"
              replace={
                <>
                  Choose <strong className="font-medium">Edit…</strong> and type
                  the credential again.
                </>
              }
            />
          ) : null}
          {/* The outbound clone credential's health, which only the Test
              button writes — a webhook delivery proves the secret, not the
              credential, and does not touch it. A failure wins over a
              success: `last_ok_at` survives a later failure by design, and
              showing both would let a green tick sit under a red one. */}
          {testing ? (
            <p className="flex items-center gap-1.5 text-2xs text-muted-foreground">
              <Loader2 className="size-3 shrink-0 animate-spin" aria-hidden="true" />
              Testing…
            </p>
          ) : conn.last_error ? (
            <Notice tone="danger" size="sm">
              {conn.last_error}
            </Notice>
          ) : conn.last_ok_at ? (
            <p className="flex items-center gap-1.5 text-2xs text-muted-foreground">
              <CheckCircle2 className={`size-3 shrink-0 ${toneText.ok}`} aria-hidden="true" />
              Reached <RelativeTime iso={conn.last_ok_at} />
            </p>
          ) : null}
        </div>
      </TableCell>

      <TableCell className="hidden align-top whitespace-normal @xl:table-cell">
        <div className="min-w-0 space-y-1">
          <div className="text-sm">{METHOD_LABEL[conn.auth_method] ?? conn.auth_method}</div>
          {conn.reach ? (
            <p className="text-2xs leading-relaxed text-muted-foreground">{conn.reach}</p>
          ) : null}
          {conn.credential_owner ? (
            <p className="text-2xs text-muted-foreground">
              Created by <span className="font-mono">{conn.credential_owner}</span>
            </p>
          ) : null}
          {conn.credential_expires_at ? (
            <p className={`text-2xs ${expired ? toneText.danger : "text-muted-foreground"}`}>
              {expired ? "Expired " : "Expires "}
              {formatDay(conn.credential_expires_at)}
            </p>
          ) : null}
        </div>
      </TableCell>

      <TableCell className="hidden align-top whitespace-normal @3xl:table-cell">
        {conn.coverage.length > 0 ? (
          <ul className="space-y-0.5">
            {conn.coverage.map((pattern) => (
              <li key={pattern} className="truncate font-mono text-2xs">{pattern}</li>
            ))}
          </ul>
        ) : (
          <p className="text-2xs leading-relaxed text-muted-foreground">
            Wherever the App is installed on GitHub.
          </p>
        )}
      </TableCell>

      <TableCell className="align-top text-sm tabular-nums">{conn.app_count}</TableCell>

      <TableCell className="align-top text-right">
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <Button variant="ghost" size="icon" aria-label={`Actions for ${conn.display_name}`}>
                <MoreHorizontal className="size-4" aria-hidden="true" />
              </Button>
            }
          />
          <DropdownMenuContent align="end">
            {/* Above the switch, because it is the thing an administrator came
                here to do before deciding whether to throw one. */}
            <DropdownMenuItem disabled={testing} onClick={onTest}>
              {testing ? "Testing…" : "Test"}
            </DropdownMenuItem>
            <DropdownMenuItem onClick={onEdit}>Edit…</DropdownMenuItem>
            {conn.webhook_secret_set ? (
              <>
                <DropdownMenuItem onClick={onRotate}>Replace webhook secret…</DropdownMenuItem>
                <DropdownMenuItem onClick={onDisableWebhook}>Turn off webhook</DropdownMenuItem>
              </>
            ) : (
              <DropdownMenuItem onClick={onRotate}>Set up webhook…</DropdownMenuItem>
            )}
            <DropdownMenuItem onClick={() => onToggle(!conn.enabled)}>
              {conn.enabled ? "Disable" : "Enable"}
            </DropdownMenuItem>
            <DropdownMenuItem variant="destructive" onClick={onRemove}>
              Remove
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </TableCell>
    </TableRow>
  );
}

const BLANK: GitConnectionInput = {
  kind: "github",
  display_name: "",
  base_url: "",
  auth_method: "token",
  app_id: "",
  installation_id: "",
  client_id: "",
  client_secret: "",
  clone_secret: "",
  credential_owner: "",
  coverage: [],
};

/**
 * One dialog for both doors, because they are the same form.
 *
 * Editing exists at all because a connection's credential is the thing every
 * deploy through it clones with, and it expires, gets rotated by the host, or
 * becomes unreadable after a key change — and until this, nothing on this page
 * could replace it. The only PATCH the UI ever sent was the enable flag, and
 * the notice beside an unreadable credential told an administrator to "type
 * the credential again" into a table cell with no field in it. The recovery
 * was to re-point every app onto a new connection first, because Remove is
 * refused while any app deploys through this one.
 *
 * The credential itself stays write-only: the response carries `*_set`
 * booleans and nothing else, so an empty field means *keep the stored one* and
 * says so, and coverage — a security boundary — goes out in full rather than
 * as a partial, so the server gates it as a real change.
 */
function AddConnectionDialog({
  open,
  onOpenChange,
  onAdded,
  connection,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onAdded: () => void;
  /** Present ⇒ edit that connection rather than create a new one. The caller
   *  keys this element on its id, so switching rows remounts the form. */
  connection?: GitConnection;
}) {
  const [form, setForm] = useState<GitConnectionInput>(
    connection
      ? {
          kind: connection.kind,
          display_name: connection.display_name,
          base_url: connection.base_url,
          auth_method: connection.auth_method,
          app_id: connection.app_id ?? "",
          installation_id: connection.installation_id ?? "",
          client_id: connection.client_id ?? "",
          client_secret: "",
          clone_secret: "",
          credential_owner: connection.credential_owner ?? "",
          coverage: [],
        }
      : BLANK,
  );
  const [coverage, setCoverage] = useState(connection ? connection.coverage.join("\n") : "");
  const [expires, setExpires] = useState(
    connection?.credential_expires_at
      ? connection.credential_expires_at.slice(0, 10)
      : "",
  );
  const [saving, setSaving] = useState(false);

  function set<K extends keyof GitConnectionInput>(key: K, value: GitConnectionInput[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  async function submit() {
    setSaving(true);
    const body = {
      ...form,
      coverage: coverage.split("\n").map((v) => v.trim()).filter(Boolean),
      credential_expires_at: expires ? new Date(expires).toISOString() : null,
    };
    try {
      if (connection) {
        await api.updateGitConnection(connection.id, body);
        toast.success("Saved");
        onOpenChange(false);
        onAdded();
        return;
      }
      const created = await api.createGitConnection(body);
      toast.success(`Connected “${created.display_name}”`);
      onOpenChange(false);
      setForm(BLANK);
      setCoverage("");
      setExpires("");
      onAdded();
    } catch (err) {
      toast.error(
        err instanceof Error
          ? err.message
          : connection
            ? "Couldn’t save the connection"
            : "Couldn’t connect the host",
      );
    } finally {
      setSaving(false);
    }
  }

  const isApp = form.auth_method === "app";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {connection ? `Edit ${connection.display_name}` : "Connect a git host"}
          </DialogTitle>
          <DialogDescription>
            Apps on this connection clone with its credential, not their own.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="conn-name">Name</Label>
            <Input
              id="conn-name"
              value={form.display_name}
              onChange={(e) => set("display_name", e.target.value)}
              placeholder="Acme GitHub"
            />
            <FieldHelp>
              The name failed deploys and administrators will see.
            </FieldHelp>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="conn-kind">Host</Label>
              <Select
                value={form.kind}
                // The host is what the stored credential is *for*. Changing it
                // on an existing connection is a different connection.
                disabled={!!connection}
                onValueChange={(v) => {
                  set("kind", v as GitConnectionInput["kind"]);
                  if (v === "gitlab") set("auth_method", "token");
                }}
              >
                <SelectTrigger id="conn-kind">
                  <SelectValue>{(v) => (v ? KIND_LABEL[String(v)] : "Choose a host")}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="github">GitHub</SelectItem>
                  <SelectItem value="gitlab">GitLab</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1.5">
              {/* GitLab has one method, so there is nothing to choose: a fact,
                  not a disabled select with its reason nowhere. */}
              {form.kind === "gitlab" ? (
                <>
                  <FieldLabel>Authenticates with</FieldLabel>
                  <p className="flex h-9 items-center text-sm text-foreground">Access token</p>
                </>
              ) : (
                <>
                  <Label htmlFor="conn-method">Authenticates with</Label>
                  <Select
                    value={form.auth_method}
                    onValueChange={(v) => set("auth_method", v as GitConnectionInput["auth_method"])}
                  >
                    <SelectTrigger id="conn-method">
                      <SelectValue>
                        {(v) => (v ? METHOD_LABEL[String(v)] : "Choose a method")}
                      </SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="app">GitHub App</SelectItem>
                      <SelectItem value="token">Access token</SelectItem>
                    </SelectContent>
                  </Select>
                </>
              )}
            </div>
          </div>

          {/* Background, so help under the field rather than a Notice, which
              is for a state. */}
          <FieldHelp className="-mt-2">
            {form.kind === "gitlab"
              ? "GitLab connects with an access token. Developers paste a repository URL, not a token."
              : isApp
                ? "A GitHub App gives short-lived, per-repository credentials and a repository picker. Creating one needs org-owner rights."
                : "An access token is tied to the account that created it, and it expires. Developers paste a repository URL, not a token."}
          </FieldHelp>

          <div className="space-y-1.5">
            <Label htmlFor="conn-base">Host address</Label>
            <Input
              id="conn-base"
              value={form.base_url}
              onChange={(e) => set("base_url", e.target.value)}
              placeholder={form.kind === "gitlab" ? "https://gitlab.com" : "https://github.com"}
            />
            <FieldHelp>
              Leave blank for the public host, or enter a self-managed server.
            </FieldHelp>
          </div>

          {isApp ? (
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="conn-app-id">App id</Label>
                <Input
                  id="conn-app-id"
                  value={form.app_id}
                  onChange={(e) => set("app_id", e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="conn-install-id">Installation id</Label>
                <Input
                  id="conn-install-id"
                  value={form.installation_id}
                  onChange={(e) => set("installation_id", e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="conn-client-id">OAuth client id</Label>
                <Input
                  id="conn-client-id"
                  value={form.client_id}
                  onChange={(e) => set("client_id", e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="conn-client-secret">OAuth client secret</Label>
                <Input
                  id="conn-client-secret"
                  type="password"
                  value={form.client_secret}
                  placeholder={
                    connection
                      ? credentialPlaceholder(connection.client_secret_set, false, "")
                      : undefined
                  }
                  onChange={(e) => set("client_secret", e.target.value)}
                />
                {connection && connection.client_secret_set && (
                  <FieldHelp>
                    Leave it blank to keep the stored one.
                  </FieldHelp>
                )}
              </div>
            </div>
          ) : null}

          <div className="space-y-1.5">
            <Label htmlFor="conn-secret">
              {isApp ? "App private key" : "Access token"}
            </Label>
            <Textarea
              id="conn-secret"
              value={form.clone_secret}
              placeholder={
                connection
                  ? credentialPlaceholder(
                      connection.clone_secret_set,
                      connection.clone_secret_unreadable,
                      "",
                    )
                  : undefined
              }
              onChange={(e) => set("clone_secret", e.target.value)}
              rows={isApp ? 4 : 1}
              className="font-mono text-2xs"
            />
            <FieldHelp>
              Stored encrypted and never shown again.
              {connection && connection.clone_secret_set
                ? " Leave it blank to keep the stored one."
                : ""}
            </FieldHelp>
          </div>

          {isApp ? null : (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="conn-coverage">Repositories this connection covers</Label>
                <Textarea
                  id="conn-coverage"
                  value={coverage}
                  onChange={(e) => setCoverage(e.target.value)}
                  rows={3}
                  placeholder={"acme/*\nacme-labs/payroll"}
                  className="font-mono text-2xs"
                />
                <FieldHelp>
                  One per line. Apps can only point at repositories that match.
                </FieldHelp>
              </div>

              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="conn-owner">Created by</Label>
                  <Input
                    id="conn-owner"
                    value={form.credential_owner}
                    onChange={(e) => set("credential_owner", e.target.value)}
                    placeholder="platform-bot"
                  />
                  <FieldHelp>
                    The account the token belongs to.
                  </FieldHelp>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="conn-expires">Expires</Label>
                  <Input
                    id="conn-expires"
                    type="date"
                    value={expires}
                    onChange={(e) => setExpires(e.target.value)}
                  />
                </div>
              </div>
            </>
          )}
        </DialogBody>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={saving}>
            {connection
              ? saving
                ? "Saving…"
                : "Save changes"
              : saving
                ? "Connecting…"
                : "Connect"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Removal is refused while apps still deploy through the connection, so the
 * dialog reads that list first and says so before anybody presses anything.
 * Disabling is the verb that is never refused, and it is named here because
 * this is where somebody discovers they wanted it.
 */
function RemoveConnectionDialog({
  connection,
  onOpenChange,
  onRemoved,
}: {
  connection: GitConnection | null;
  onOpenChange: (v: boolean) => void;
  onRemoved: () => void;
}) {
  const [removing, setRemoving] = useState(false);

  const { data } = useQuery({
    queryKey: ["git-connection-apps", connection?.id],
    queryFn: () => api.listGitConnectionApps(connection!.id),
    enabled: connection !== null,
  });
  const apps: GitConnectionApp[] = data?.apps ?? [];

  async function submit() {
    if (!connection) return;
    setRemoving(true);
    try {
      await api.deleteGitConnection(connection.id);
      toast.success(`Removed “${connection.display_name}”`);
      onOpenChange(false);
      onRemoved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t remove “${connection.display_name}”`);
    } finally {
      setRemoving(false);
    }
  }

  return (
    <Dialog open={connection !== null} onOpenChange={onOpenChange}>
      <DialogContent scrollable>
        <DialogHeader>
          <DialogTitle>Remove {connection?.display_name}?</DialogTitle>
          <DialogDescription>
            {apps.length > 0
              ? "Apps still deploy through it. Move them to another connection, or disable it to stop new deploys without touching running apps."
              : "No app deploys through it. Its credential is deleted from this install."}
          </DialogDescription>
        </DialogHeader>

        {apps.length > 0 ? (
          <DialogBody>
            <ul className="space-y-1">
              {/* Each one opens where it is moved — its Settings tab. A list
                  of slugs that linked nowhere was a count nobody could act on. */}
              {apps.map((app) => (
                <li key={app.id} className="font-mono text-2xs">
                  <Link to={`/apps/${app.id}/settings`} className="text-primary hover:underline">
                    {app.slug}
                  </Link>
                </li>
              ))}
            </ul>
          </DialogBody>
        ) : null}

        <DialogFooter>
          {/* While apps deploy through it the server refuses, so there is no
              destructive button — the list above is the way forward. */}
          <Button variant="outline" disabled={removing} onClick={() => onOpenChange(false)}>
            {apps.length > 0 ? "Close" : "Cancel"}
          </Button>
          {apps.length === 0 && (
            <Button variant="destructive" onClick={submit} disabled={removing}>
              {removing ? "Removing…" : "Remove connection"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

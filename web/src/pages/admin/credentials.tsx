import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ChevronRight, KeyRound, MoreHorizontal, Plus, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Code } from "@/components/code";
import { EmptyState } from "@/components/empty-state";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { PageHeader } from "@/components/page-header";
import { QueryError } from "@/components/query-error";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { TableCard } from "@/components/table-card";
import { FieldHelp } from "@/components/field-help";
import {
  AttachCredentialDialog, DeleteCredentialDialog, EditCredentialDialog,
} from "@/pages/admin/credential-detail";
import { api, type Credential } from "@/lib/api";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/**
 * Credentials an administrator holds and attaches to apps.
 *
 * **The claim this page is allowed to make is accountability, never
 * containment.** App code can read an injected value trivially, and an owner
 * who wants it can print it. What the copy may say is that the credential is
 * the operator's — the owner cannot read it back, change it, or take it to
 * another app — that rotating it is one place, and that who reaches the
 * warehouse is an administrator's decision. What it may not say, in any
 * wording, is that the app owner cannot get at the value.
 *
 * Values are never rendered here, because they are never returned. There is no
 * "reveal" and there is nothing to reveal: the store has no read that produces
 * one.
 *
 * **This is the inventory and not the record.** A row leads to
 * `/admin/credentials/{id}`, which is where the apps holding one are named and
 * where rotating and detaching happen. What stays here is the count: every
 * credential's apps is a fan-out nobody browsing the list asked for, and an
 * editor on a row was how this page ended up able to state a number it could
 * not break down.
 *
 * The exception is the two fields this page itself renders. A row shows a name
 * and a description, so the row owes a way to edit them — moving that behind
 * "Open" made the description unreachable from the only surface that displays
 * it, which is how somebody ends up asking where the field went.
 */
export function AdminCredentialsPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Credential | null>(null);
  const [attaching, setAttaching] = useState<Credential | null>(null);
  const [editingDetails, setEditingDetails] = useState<Credential | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["credentials"],
    queryFn: () => api.listCredentials(),
  });
  const credentials = data?.credentials ?? [];

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["credentials"] });
    queryClient.invalidateQueries({ queryKey: ["credential-apps"] });
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Credentials"
        description="Secrets you attach to apps. The app’s code reads them; its owner can’t see or change them."
        actions={
          <Button size="sm" onClick={() => setCreating(true)}>
            <Plus className="mr-1.5 h-4 w-4" />
            New credential
          </Button>
        }
      />

      {error ? (
        <QueryError
          title="Credentials could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : isLoading ? (
        <TableCard>
          <div className="space-y-2 p-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        </TableCard>
      ) : credentials.length === 0 ? (
        <EmptyState
          icon={<KeyRound className="h-6 w-6" />}
          title="No credentials"
          description="Create one to give an app a secret its owner shouldn’t hold."
          action={
            <Button size="sm" onClick={() => setCreating(true)}>
              <Plus className="mr-1.5 h-4 w-4" />
              New credential
            </Button>
          }
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">Name</TableHead>
                <TableHead className="hidden @xl:table-cell">Supplies</TableHead>
                <TableHead className="text-right">Apps</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {credentials.map((c) => (
                <TableRow
                  key={c.id}
                  className="cursor-pointer"
                  onClick={() => navigate(`/admin/credentials/${c.id}`)}
                >
                  <TableCell className="max-w-0">
                    {/* The name stays a real link: a row handler is a
                        convenience for a pointer, and it is the link that a
                        keyboard reaches, that a middle click opens in a tab,
                        and that says where it goes before you commit to it. */}
                    <Link
                      to={`/admin/credentials/${c.id}`}
                      className="block truncate font-medium text-primary hover:underline"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {c.name}
                    </Link>
                    {c.description && (
                      <span className="block truncate text-xs text-muted-foreground">
                        {c.description}
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="hidden @xl:table-cell">
                    <span className="font-mono text-xs text-muted-foreground">
                      {c.keys.join(", ")}
                    </span>
                  </TableCell>
                  <TableCell className="text-right text-sm text-muted-foreground">
                    <span className="inline-flex items-center gap-1">
                      {c.app_count}
                      <ChevronRight className="h-3.5 w-3.5" />
                    </span>
                  </TableCell>
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Actions for ${c.name}`}
                          >
                            <MoreHorizontal className="h-4 w-4" />
                          </Button>
                        }
                      />
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => navigate(`/admin/credentials/${c.id}`)}>
                          Open
                        </DropdownMenuItem>
                        {/* The row's own name and description, edited from the
                            row. This menu briefly offered neither — Open,
                            Attach, Delete — which left the description
                            reachable only from a menu item on another page
                            called "Rename". A row that renders a field owes a
                            way to edit it. */}
                        <DropdownMenuItem onClick={() => setEditingDetails(c)}>
                          Edit details…
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setAttaching(c)}>
                          Attach to an app…
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          variant="destructive"
                          onClick={() => setDeleting(c)}
                        >
                          Delete…
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      {creating && (
        <CreateCredentialDialog onOpenChange={() => setCreating(false)} onSaved={refresh} />
      )}
      {editingDetails && (
        <EditCredentialDialog
          key={editingDetails.id}
          credential={editingDetails}
          onOpenChange={() => setEditingDetails(null)}
          onSaved={refresh}
        />
      )}
      {attaching && (
        <AttachCredentialDialog
          key={attaching.id}
          credential={attaching}
          onOpenChange={() => setAttaching(null)}
          onAttached={refresh}
        />
      )}
      {deleting && (
        <DeleteCredentialDialog
          key={deleting.id}
          credential={deleting}
          onOpenChange={() => setDeleting(null)}
          onDeleted={refresh}
        />
      )}
    </div>
  );
}

// ── Create ──────────────────────────────────────────────────

type Pair = { key: string; value: string };

/**
 * A new credential: a name, a note, and the pairs it supplies.
 *
 * Creating is the only write left on the inventory, because it is the only one
 * with no row to open yet. Renaming, rotating and detaching are the credential's
 * own page — where the apps that would be affected are on screen while you
 * decide.
 */
function CreateCredentialDialog({
  onOpenChange, onSaved,
}: {
  onOpenChange: (o: boolean) => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [pairs, setPairs] = useState<Pair[]>([{ key: "", value: "" }]);
  const [saving, setSaving] = useState(false);

  const filled = pairs.filter((p) => p.key.trim() && p.value !== "");
  const canSave = !!name.trim() && filled.length > 0;

  async function save() {
    setSaving(true);
    try {
      await api.createCredential({
        name: name.trim(),
        description,
        values: Object.fromEntries(filled.map((p) => [p.key.trim(), p.value])),
      });
      toast.success(`Credential “${name.trim()}” created`);
      onSaved();
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t create “${name.trim()}”`);
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
            if (!saving && canSave) void save();
          }}
        >
        <DialogHeader>
          <DialogTitle>New credential</DialogTitle>
          <DialogDescription>
            Each key becomes an environment variable in the apps it’s attached to. Values are
            never shown again.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="credential-name">Name</Label>
            <Input
              id="credential-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="warehouse-prod"
              className="font-mono"
            />
            <FieldHelp>
              What app owners see. Name what it reaches, like <Code>warehouse-prod</Code>.
            </FieldHelp>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="credential-description">Description</Label>
            <Input
              id="credential-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Read-only service account on the reporting warehouse"
            />
          </div>

          <div className="space-y-2">
            <span className="text-sm font-medium">Values</span>
            {pairs.map((p, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  aria-label={`Key ${i + 1}`}
                  value={p.key}
                  placeholder="SNOWFLAKE_PASSWORD"
                  className="font-mono text-xs"
                  onChange={(e) =>
                    setPairs(pairs.map((x, j) => (i === j ? { ...x, key: e.target.value } : x)))
                  }
                />
                <Input
                  aria-label={`Value ${i + 1}`}
                  type="password"
                  value={p.value}
                  placeholder="••••••••"
                  className="font-mono text-xs"
                  onChange={(e) =>
                    setPairs(pairs.map((x, j) => (i === j ? { ...x, value: e.target.value } : x)))
                  }
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  aria-label={`Remove ${p.key.trim() || `value ${i + 1}`}`}
                  disabled={pairs.length === 1}
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
              onClick={() => setPairs([...pairs, { key: "", value: "" }])}
            >
              <Plus className="h-4 w-4" />
              Add another
            </Button>
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || !canSave}>
            {saving ? "Creating…" : "Create"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

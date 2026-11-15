import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, User, Users } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { DirectoryPicker } from "@/components/directory-picker";
import { EmptyState } from "@/components/empty-state";
import { FieldHelp } from "@/components/field-help";
import { QueryError } from "@/components/query-error";
import { Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { api } from "@/lib/api";
import type { StorageDefault, StorageDefaultLevel } from "@/lib/api";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/** What each level confers, said before the click (ADR § Storage somebody else
 *  can attach): a delegation on a store confers its data plane, a default's
 *  included, so `full` is upload and delete from a browser as well as a mount. */
const LEVEL_CONFERS: Record<StorageDefaultLevel, string> = {
  read: "Attach read-only, and browse and download every store.",
  full: "Attach read-write, and upload, replace and delete in every store.",
};
const LEVEL_LABEL: Record<StorageDefaultLevel, string> = { read: "Read", full: "Full" };

/**
 * Admin › Storage › Default delegations: who every new resource is delegated
 * to when it is created.
 *
 * A default writes ordinary delegations, so each one is then on its
 * resource's page to revoke or lower; this list only decides future creates —
 * and, when asked, the resources that already exist.
 */
export function StorageDefaultsSection({ onChanged }: { onChanged: () => void }) {
  const queryClient = useQueryClient();
  const [level, setLevel] = useState<StorageDefaultLevel>("read");
  const [applyExisting, setApplyExisting] = useState(false);
  const [pendingKey, setPendingKey] = useState<string | null>(null);
  const [toRemove, setToRemove] = useState<StorageDefault | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["storage-defaults"],
    queryFn: api.listStorageDefaults,
  });
  const defaults = data ?? [];
  const held = new Set(defaults.map((d) => `${d.subject_type}:${d.subject_id}`));

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["storage-defaults"] });
    queryClient.invalidateQueries({ queryKey: ["storage-managers"] });
    queryClient.invalidateQueries({ queryKey: ["storage-mappings"] });
    queryClient.invalidateQueries({ queryKey: ["app-storage"] });
    queryClient.invalidateQueries({ queryKey: ["stores"] });
    onChanged();
  }

  async function add(kind: "user" | "group", id: string, name: string) {
    setPendingKey(`${kind}:${id}`);
    try {
      const res = await api.putStorageDefault({
        subject_type: kind, subject_id: id, max_access: level, apply_existing: applyExisting,
      });
      refresh();
      const applied = res.applied?.length ?? 0;
      const differing = res.differing ?? [];
      toast.success(`New storage is delegated to “${name}”`, {
        description: applyExisting
          ? `Applied to ${applied} existing resource${applied === 1 ? "" : "s"}` +
            (differing.length
              ? `; left ${differing.map((d) => `${d.resource} at ${d.max_access}`).join(", ")} as they were`
              : "")
          : "Resources that already exist are unchanged.",
      });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t add “${name}” as a default`);
    } finally {
      setPendingKey(null);
    }
  }

  return (
    <Section
      id="defaults"
      title="Default delegations"
      description="Who every new resource is delegated to when it is created. Each becomes an ordinary delegation on that resource."
    >
      {error ? (
        <div className="p-4">
          <QueryError
            title="The default delegations could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      ) : isLoading ? (
        <div className="p-4">
          <Skeleton className="h-16 w-full" />
        </div>
      ) : (
        <>
          <div className="grid gap-4 p-4 sm:max-w-lg sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="default-level">Up to</Label>
              <Select value={level} onValueChange={(v) => setLevel(v as StorageDefaultLevel)}>
                <SelectTrigger id="default-level" className="w-full">
                  <SelectValue>{(v) => (v ? LEVEL_LABEL[v as StorageDefaultLevel] : "Choose…")}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {(["read", "full"] as const).map((l) => (
                    <SelectItem key={l} value={l}>{LEVEL_LABEL[l]}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>{LEVEL_CONFERS[level]}</FieldHelp>
              <div className="flex items-center gap-2 pt-1">
                <Checkbox
                  id="default-apply"
                  checked={applyExisting}
                  onCheckedChange={(v) => setApplyExisting(v === true)}
                />
                <Label htmlFor="default-apply" className="font-normal">Also on existing resources</Label>
              </div>
            </div>
            <DirectoryPicker
              id="default-search"
              label="Add people or groups"
              exclude={(k, id) => held.has(`${k}:${id}`)}
              pendingKey={pendingKey ?? undefined}
              rowSuffix={() => `add with ${LEVEL_LABEL[level].toLowerCase()}`}
              onPick={(k, id, label) => void add(k, id, label)}
            />
          </div>

          {defaults.length === 0 ? (
            <div className="border-t border-border/60">
              <EmptyState size="inline" title="No default delegations" description="A new resource is attachable only by administrators until someone is delegated on it." />
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="w-full">Person or group</TableHead>
                  <TableHead>Up to</TableHead>
                  <TableHead className="hidden @xl:table-cell">Delegated on</TableHead>
                  <TableHead className="w-[40px]"><span className="sr-only">Actions</span></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {defaults.map((d) => (
                  <TableRow key={d.id}>
                    <TableCell className="max-w-0">
                      <span className="flex items-center gap-2 truncate">
                        {d.subject_type === "group" ? (
                          <Users className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
                        ) : (
                          <User className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
                        )}
                        <span className="truncate font-medium">{d.subject_name || d.subject_id}</span>
                        {d.subject_type === "group" && <span className="text-xs text-muted-foreground">group</span>}
                      </span>
                    </TableCell>
                    <TableCell className="text-sm">{LEVEL_LABEL[d.max_access]}</TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                      {d.delegations} resource{d.delegations === 1 ? "" : "s"}
                    </TableCell>
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger
                          render={
                            <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${d.subject_name || d.subject_id}`}>
                              <MoreHorizontal className="h-4 w-4" />
                            </Button>
                          }
                        />
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem variant="destructive" onClick={() => setToRemove(d)}>
                            Remove…
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}

      <RemoveDefaultDialog
        key={toRemove?.id}
        entry={toRemove}
        onOpenChange={() => setToRemove(null)}
        onRemoved={refresh}
      />
    </Section>
  );
}

/**
 * Removing a default: it stops future creates, and revokes nothing unless
 * asked — a settings edit that restarted every production app would be a
 * surprise. Asked, it revokes only the delegations defaults wrote, and the apps
 * that restarts are named here before the click, as a single revoke names them.
 */
function RemoveDefaultDialog({
  entry, onOpenChange, onRemoved,
}: {
  entry: StorageDefault | null;
  onOpenChange: () => void;
  onRemoved: () => void;
}) {
  const [revoke, setRevoke] = useState(false);
  const [pending, setPending] = useState(false);
  const name = entry ? entry.subject_name || entry.subject_id : "";

  async function submit() {
    if (!entry) return;
    setPending(true);
    try {
      const res = await api.deleteStorageDefault(`${entry.subject_type}:${entry.subject_id}`, revoke);
      toast.success(`“${name}” is no longer a default`, {
        description: revoke ? res.note : "Its delegations on existing resources stay.",
      });
      onRemoved();
      onOpenChange();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t remove “${name}”`);
    } finally {
      setPending(false);
    }
  }

  return (
    <Dialog open={!!entry} onOpenChange={(o) => { if (!o && !pending) onOpenChange(); }}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Remove {name} as a default?</DialogTitle>
          <DialogDescription>
            New resources will no longer be delegated to {name}. Its delegations on existing
            resources stay unless you revoke them too.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="space-y-3">
          {entry && entry.delegations > 0 && (
            <div className="flex items-start gap-2">
              <Checkbox id="default-revoke" checked={revoke} onCheckedChange={(v) => setRevoke(v === true)} />
              <Label htmlFor="default-revoke" className="font-normal leading-snug">
                Also revoke it on the {entry.delegations} resource{entry.delegations === 1 ? "" : "s"} a
                default delegated
              </Label>
            </div>
          )}
          {revoke && entry && entry.apps.length > 0 && (
            <div className="text-sm">
              <p className="text-muted-foreground">These apps lose the storage and are restarted:</p>
              <ul className="mt-1 space-y-0.5">
                {entry.apps.map((a) => (
                  <li key={a.id}>
                    <Link to={`/apps/${a.id}`} className="text-primary hover:underline">{a.name}</Link>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="outline" disabled={pending} onClick={onOpenChange}>Cancel</Button>
          <Button variant="destructive" disabled={pending} onClick={() => void submit()}>
            {pending ? "Removing…" : revoke ? "Remove and revoke" : "Remove"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

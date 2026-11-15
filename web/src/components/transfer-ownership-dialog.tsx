import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ApiError, api, refusalCodeOf } from "@/lib/api";
import { Notice } from "@/components/notice";
import type { DirectoryGroup, DirectoryUser, OwnerPrincipalType } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toneText } from "@/lib/tone";
import { Users } from "lucide-react";
import { systemRoleLabel } from "@/lib/utils";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { ConfirmPhrase } from "@/components/confirm-phrase";

interface TransferOwnershipDialogProps {
  app: { id: string; name: string; slug: string };
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/** The same label the Access tab's grant search uses, plus the one fact this
 *  picker needs: a viewer can't own apps. */
function ownerRoleLabel(role: string): string {
  return role === "viewer"
    ? `${systemRoleLabel(role)} — can’t own apps`
    : systemRoleLabel(role);
}

/** Whichever kind of principal is about to receive the app. */
type Target =
  | { kind: "user"; user: DirectoryUser }
  | { kind: "group"; group: DirectoryGroup };

const targetLabel = (t: Target) => (t.kind === "user" ? t.user.login : t.group.name);

/**
 * Pick a person or a group, type the slug, transfer. The previous owner is
 * demoted to editor server-side in the same transaction.
 *
 * **This is the first of the two places group ownership is introduced**, and
 * the reason it is here is that "this should not be one person's" is the
 * thought already being had. It earns its introduction at exactly this moment
 * and nowhere else.
 *
 * Which is also why the group half is conditional on there being groups.
 * Nothing prompts anyone to create one, and on an install with none this is a
 * person picker that says nothing about groups at all — no empty section, no
 * disabled tab, no "no groups yet" placeholder. Absent, not dormant.
 */
export function TransferOwnershipDialog({ app, open, onOpenChange }: TransferOwnershipDialogProps) {
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [target, setTarget] = useState<Target | null>(null);
  const [confirmText, setConfirmText] = useState("");
  const [transferring, setTransferring] = useState(false);
  // The storage the app's toft.toml declares that the chosen owner cannot
  // attach — the 409 the transfer answers, listed in place because the fix is
  // a delegation per resource, not a retry.
  const [storageNeeds, setStorageNeeds] = useState<{ resource: string; folder?: string; access: string }[] | null>(null);

  const searching = open && !target && search.trim().length >= 2;

  const usersQuery = useQuery<DirectoryUser[]>({
    queryKey: ["directory-users", search],
    queryFn: () => api.searchDirectoryUsers(search),
    enabled: searching,
  });
  const users = usersQuery.data ?? [];

  // Whether this install has groups at all. One cheap list read, and it is what
  // decides whether the word "group" appears in this dialog.
  const { data: allGroups = [] } = useQuery({
    queryKey: ["groups"],
    queryFn: api.listGroups,
    enabled: open,
    staleTime: 5 * 60_000,
  });
  const installHasGroups = allGroups.length > 0;

  const groupsQuery = useQuery<DirectoryGroup[]>({
    queryKey: ["directory-groups", search],
    queryFn: () => api.searchDirectoryGroups(search),
    enabled: searching && installHasGroups,
  });
  const groups = groupsQuery.data ?? [];

  function reset() {
    setSearch("");
    setTarget(null);
    setConfirmText("");
    setStorageNeeds(null);
  }

  async function handleTransfer() {
    if (!target) return;
    const type: OwnerPrincipalType = target.kind;
    const id = target.kind === "user" ? target.user.id : target.group.id;
    setTransferring(true);
    try {
      await api.transferOwnership(app.id, id, type);
      toast.success(`Ownership of “${app.name}” transferred to ${targetLabel(target)}`);
      queryClient.invalidateQueries({ queryKey: ["app", app.id] });
      queryClient.invalidateQueries({ queryKey: ["app-permissions", app.id] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["groups"] });
      onOpenChange(false);
      reset();
    } catch (err) {
      if (refusalCodeOf(err) === "storage_not_attachable_by_new_owner" && err instanceof ApiError) {
        const list = err.body["storage"];
        setStorageNeeds(Array.isArray(list) ? (list as { resource: string; folder?: string; access: string }[]) : []);
      }
      toast.error(err instanceof Error ? err.message : `Couldn’t transfer “${app.name}”`);
    } finally {
      setTransferring(false);
    }
  }

  const nothingMatched = users.length === 0 && groups.length === 0;
  // Three states: a search in flight and a search that failed both used to
  // render "No matching people or groups." — on the one dialog whose whole
  // job is finding somebody to hand an app to.
  const inFlight = usersQuery.isFetching || groupsQuery.isFetching;
  const searchFailed = usersQuery.error ?? groupsQuery.error;

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) reset(); onOpenChange(o); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Transfer ownership</DialogTitle>
          <DialogDescription>
            Hand <span className="font-medium text-foreground">{app.name}</span> to another{" "}
            {installHasGroups ? "publisher or to a group" : "publisher"}. The current owner stays on
            the app as an editor — remove them on Access to take that away. Only the new owner or an
            administrator can transfer it back.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="transfer-search">New owner</Label>
            <Input
              id="transfer-search"
              placeholder={installHasGroups ? "Search people and groups…" : "Search by username…"}
              value={target ? targetLabel(target) : search}
              onChange={(e) => { setTarget(null); setSearch(e.target.value); }}
              autoComplete="off"
              autoFocus
            />
          </div>

          {/* How many matched, said out loud — see `directory-picker.tsx`. */}
          <p aria-live="polite" className="sr-only">
            {!searching
              ? ""
              : searchFailed
                ? "The directory could not be searched."
                : inFlight
                  ? "Searching…"
                  : `${users.length + groups.length} matches`}
          </p>
          {searching && (
            <div className="max-h-52 divide-y divide-border/60 overflow-y-auto rounded-md border border-border/60">
              {searchFailed ? (
                <p className={`p-2.5 text-xs ${toneText.danger}`}>
                  {searchFailed instanceof Error && searchFailed.message
                    ? searchFailed.message
                    : "The directory could not be searched."}
                </p>
              ) : inFlight ? (
                <p className="p-2.5 text-xs text-muted-foreground">Searching…</p>
              ) : nothingMatched ? (
                <p className="p-2.5 text-xs text-muted-foreground">
                  {installHasGroups ? "No matching people or groups." : "No matching people."}
                </p>
              ) : (
                <>
                  {users.map((u) => {
                    const isViewer = u.system_role === "viewer";
                    return (
                      <button
                        key={u.id}
                        type="button"
                        disabled={isViewer}
                        className="flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:cursor-not-allowed disabled:opacity-50"
                        onClick={() => { setStorageNeeds(null); setTarget({ kind: "user", user: u }); }}
                      >
                        <span className="min-w-0 truncate font-medium">{u.login}</span>
                        <span className="shrink-0 text-xs text-muted-foreground">
                          {ownerRoleLabel(u.system_role)}
                        </span>
                      </button>
                    );
                  })}
                  {groups.map((g) => {
                    // An empty group is shown rather than filtered out, and the
                    // reason travels with it. Hiding it would leave somebody
                    // searching for a group they know exists; a transfer to it
                    // would be refused server-side anyway (§9.3).
                    const empty = g.eligible_members === 0;
                    return (
                      <button
                        key={g.id}
                        type="button"
                        disabled={empty}
                        className="flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:cursor-not-allowed disabled:opacity-50"
                        onClick={() => { setStorageNeeds(null); setTarget({ kind: "group", group: g }); }}
                      >
                        <span className="flex min-w-0 items-center gap-1.5">
                          <Users className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
                          <span className="truncate font-medium">{g.name}</span>
                        </span>
                        <span className="shrink-0 text-xs text-muted-foreground">
                          {empty
                            ? "nobody who could administer it"
                            : `${g.eligible_members} ${g.eligible_members === 1 ? "member" : "members"}`}
                        </span>
                      </button>
                    );
                  })}
                </>
              )}
            </div>
          )}

          {/* The blast radius, before the transfer rather than after it. Every
              member of this group becomes an owner of this app. */}
          {target?.kind === "group" && (
            <p className="text-xs text-muted-foreground">
              <span className="font-medium text-foreground">
                {target.group.eligible_members}{" "}
                {target.group.eligible_members === 1 ? "person" : "people"}
              </span>{" "}
              will own {app.name}. Membership decides it from then on — adding someone to{" "}
              {target.group.name} gives them the app, and removing them takes it away on their next
              request.
            </p>
          )}

          <div className="space-y-1.5">
            <p className="text-sm font-medium">
              <Label htmlFor="transfer-confirm" className="inline">Type</Label>{" "}
              <ConfirmPhrase text={app.slug} /> to confirm
            </p>
            <Input
              id="transfer-confirm"
              value={confirmText}
              onChange={(e) => setConfirmText(e.target.value)}
              className="font-mono"
              autoComplete="off"
            />
          </div>
        </div>

        {storageNeeds && target && (
          <Notice tone="danger" title={`${targetLabel(target)} can’t attach this app’s storage`} live>
            <p>
              This app’s toft.toml declares storage its owner must be able to attach. An administrator
              lets the new owner attach each of these, or the entry comes out of toft.toml first.
            </p>
            <ul className="mt-2 space-y-1">
              {storageNeeds.map((n) => (
                <li key={`${n.resource}/${n.folder ?? ""}`}>
                  <span className="font-mono">{n.folder ? `${n.resource}/${n.folder}` : n.resource}</span>{" "}
                  at {n.access}
                </li>
              ))}
            </ul>
          </Notice>
        )}

        <DialogFooter>
          <Button
            variant="outline"
            disabled={transferring}
            onClick={() => { reset(); onOpenChange(false); }}
          >
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={!target || confirmText !== app.slug || transferring}
            onClick={handleTransfer}
          >
            {transferring ? "Transferring…" : "Transfer ownership"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

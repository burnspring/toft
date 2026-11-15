import { Link } from "react-router-dom";
import { useCallback, useEffect, useState, useMemo } from "react";
import { useUrlParam } from "@/hooks/use-list-state";
import { LicenseLock } from "@/components/license-lock";
import { copyToClipboard } from "@/lib/clipboard";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, MoreHorizontal, Plus, Trash2, Users, UsersRound, X } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/notice";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { UserAvatar } from "@/components/user-avatar";
import { Skeleton } from "@/components/ui/skeleton";
import { SearchInput } from "@/components/search-input";
import { EmptyState } from "@/components/empty-state";
import { rowActivation } from "@/lib/row-activation";
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { StatusBadge } from "@/components/status-badge";
import { SectionHeading } from "@/components/section";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { RelativeTime } from "@/components/relative-time";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import type { Group, GroupDetail, HeldStorageMapping } from "@/lib/api";
import { HeldStorageNotice } from "@/components/held-storage-notice";
import { heldStorageMappings } from "@/lib/held-storage";
import { systemRoleLabel } from "@/lib/utils";
import { toneText } from "@/lib/tone";
import { TableCard } from "@/components/table-card";
import { FieldHelp } from "@/components/field-help";

/** Groups bundle users for app sharing — a group can be granted a role on an
 * app, or own one. Admins and publishers manage rosters; only admins delete.
 *
 * `?q=` is the search and `?group=` opens that group's dialog — the address
 * `AppOwner` links a group owner to, so the link lands on the group rather
 * than one search away from it. */
export function GroupsTab() {
  const { user: me } = useAuth();
  const canManage = me?.system_role === "system_admin" || me?.system_role === "publisher";
  const isAdmin = me?.system_role === "system_admin";

  const [search, setSearch] = useUrlParam("q");
  const [linkedGroupId, setLinkedGroupId] = useUrlParam("group");
  const [createOpen, setCreateOpen] = useState(false);
  const [chosenGroup, setGroupToManage] = useState<Group | null>(null);
  const [groupToDelete, setGroupToDelete] = useState<Group | null>(null);

  const { data: groups = [], isLoading, error, refetch } = useQuery<Group[]>({
    queryKey: ["groups"],
    queryFn: api.listGroups,
  });

  const groupToManage = chosenGroup ?? groups.find((g) => g.id === linkedGroupId) ?? null;
  function closeManage() {
    setGroupToManage(null);
    if (linkedGroupId) setLinkedGroupId("");
  }

  const filtered = useMemo(
    () => groups.filter((g) => g.name.toLowerCase().includes(search.toLowerCase())),
    [groups, search],
  );

  return (
    <div className="space-y-4">
      <LicenseLock feature="groups" />

      <div className="flex items-center gap-4 flex-wrap">
        <SearchInput placeholder="Search groups…" value={search} onChange={setSearch} className="w-full sm:max-w-sm" />
        {canManage && (
          <Button size="sm" className="ml-auto" onClick={() => setCreateOpen(true)}>
            <Plus className="mr-1.5 h-3.5 w-3.5" /> New group
          </Button>
        )}
      </div>

      {isLoading ? (
        <TableCard className="p-4 space-y-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="flex items-center gap-3">
              <Skeleton className="h-7 w-7 rounded-full" />
              <Skeleton className="h-4 w-40" />
              <Skeleton className="ml-auto h-4 w-20" />
            </div>
          ))}
        </TableCard>
      ) : error ? (
        // Before the empty check, always: "No groups yet. Create a group to
        // share apps with a whole team at once." is an invitation to act, at
        // the moment nothing can be done.
        <QueryError
          title="Groups could not be read"
          error={error}
          fallback="Reload the page. If it keeps failing, tell an administrator."
          onRetry={() => void refetch()}
        />
      ) : filtered.length === 0 ? (
        <EmptyState
          icon={<UsersRound className="h-6 w-6" />}
          title={search ? `No groups match “${search}”` : "No groups yet"}
          description={
            search
              ? undefined
              : canManage
                ? "Create a group to share apps with a whole team at once."
                : "Groups bundle users for app sharing."
          }
          action={
            search ? (
              <Button variant="secondary" size="sm" onClick={() => setSearch("")}>
                Clear
              </Button>
            ) : undefined
          }
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">Group</TableHead>
                <TableHead>Members</TableHead>
                {/* Owns is what makes the two lifecycle refusals legible before
                    somebody hits one: a group that owns apps cannot be deleted
                    and cannot lose its last eligible member. */}
                <TableHead className="hidden w-[110px] @xl:table-cell">Owns</TableHead>
                <TableHead className="hidden w-[190px] @3xl:table-cell">Failure emails</TableHead>
                <TableHead className="hidden w-[130px] @md:table-cell">Created</TableHead>
                <TableHead className="w-[50px]"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((group) => (
                <TableRow
                  key={group.id}
                  {...rowActivation(`Manage ${group.name}`, () => setGroupToManage(group))}
                >
                  <TableCell className="max-w-0">
                    <div className="flex min-w-0 items-center gap-2.5">
                      <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-primary/10">
                        <Users className="h-3.5 w-3.5 text-primary" />
                      </div>
                      <div className="min-w-0">
                        <span className="block truncate font-medium text-foreground">{group.name}</span>
                        {/* Created, at the width that drops its column. */}
                        <span className="block text-2xs text-muted-foreground @md:hidden">
                          Created <RelativeTime iso={group.created_at} />
                        </span>
                      </div>
                    </div>
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {group.member_count ?? 0} {group.member_count === 1 ? "member" : "members"}
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                    {/* Openable. A group that owns apps cannot be deleted, and
                        the refusal named a number with no way to reach the
                        apps behind it — so the one thing an administrator has
                        to do next (transfer them, or find out whether they
                        still matter) started with guessing which they are. */}
                    {group.owns_apps ? (
                      <Link
                        to={`/?scope=all&owner=group:${group.id}`}
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                      >
                        {group.owns_apps} {group.owns_apps === 1 ? "app" : "apps"}
                      </Link>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                    {group.email ? (
                      <span className="truncate font-mono text-xs">{group.email}</span>
                    ) : group.owns_apps ? (
                      <span className="text-xs">its members</span>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @md:table-cell">
                    <RelativeTime iso={group.created_at} />
                  </TableCell>
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    <DropdownMenu>
                      {/* The `render` prop, not a nested `<Button>`: the
                          trigger is itself a button, so this was a button
                          inside a button — invalid, and the browser's repair of
                          it is what left the inner one unreachable by Tab. */}
                      <DropdownMenuTrigger
                        render={
                          <Button
                            variant="ghost"
                            size="icon-xs"
                            aria-label={`Actions for ${group.name}`}
                          />
                        }
                      >
                        <MoreHorizontal className="h-4 w-4" />
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setGroupToManage(group)}>
                          <Users className="mr-2 h-3.5 w-3.5" />
                          {canManage ? "Manage members" : "View members"}
                        </DropdownMenuItem>
                        {isAdmin && (
                          <DropdownMenuItem variant="destructive"
                            onClick={() => setGroupToDelete(group)}
                          >
                            <Trash2 className="mr-2 h-3.5 w-3.5" /> Delete group…
                          </DropdownMenuItem>
                        )}
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      <CreateGroupDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={(g) => setGroupToManage(g)}
      />
      {groupToManage && (
        <GroupMembersDialog
          isAdmin={isAdmin}
          group={groupToManage}
          canManage={canManage}
          open={!!groupToManage}
          onOpenChange={(o) => { if (!o) closeManage(); }}
        />
      )}
      {groupToDelete && (
        <DeleteGroupDialog
          group={groupToDelete}
          open={!!groupToDelete}
          onOpenChange={(o) => { if (!o) setGroupToDelete(null); }}
        />
      )}
    </div>
  );
}

// ── Create ──────────────────────────────────────────────────

function CreateGroupDialog({
  open, onOpenChange, onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (g: Group) => void;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);

  async function create() {
    const trimmed = name.trim();
    if (!trimmed || saving) return;
    setSaving(true);
    try {
      const group = await api.createGroup(trimmed);
      queryClient.invalidateQueries({ queryKey: ["groups"] });
      toast.success(`Created “${group.name}”`);
      setName("");
      onOpenChange(false);
      onCreated(group); // drop straight into adding members
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t create the group");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!saving) { setName(""); onOpenChange(o); } }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New group</DialogTitle>
          <DialogDescription>
            Share apps with a group, and everyone in it gets that access.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => { e.preventDefault(); create(); }}
          className="space-y-1.5"
        >
          <Label htmlFor="group-name">Name</Label>
          <Input
            id="group-name"
            placeholder="e.g. Data Science"
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoComplete="off"
            autoFocus
          />
        </form>
        <DialogFooter>
          <Button variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button disabled={saving || !name.trim()} onClick={create}>
            {saving ? "Creating…" : "Create group"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Where this group's apps' failures are reported.
 *
 * Its own small self-saving field rather than a row in a batched form, because
 * it is one decision and the page it sits on has no other save. Empty is a
 * meaningful value and not a blank: it puts the group back on member expansion.
 * There is no way to make a group's apps notify nobody, deliberately.
 *
 * Not a credential — it reads back in the clear, exactly like the
 * administrator notification address on Admin → Email, and unlike anything
 * stored as a hash.
 */
/**
 * What the group is called.
 *
 * The only rename a group has: there is no slug beside it, and the id
 * underneath — which is what every grant, every owned app and every audit row
 * actually points at — never moves. So this changes a label and nothing else,
 * which is why it saves in place with a toast rather than through a dialog
 * asking whether you meant it.
 *
 * Its own Save, like the address below it, because they are two decisions on a
 * dialog that has no form and no other save.
 *
 * The name is unique across the install, so this is the one field here that can
 * come back refused. The server's own words are what the toast carries — it
 * names the collision, and a generic "couldn’t save" would leave somebody
 * retyping the same name.
 */
function GroupNameField({
  group,
  onDirtyChange,
}: {
  group: Group;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [value, setValue] = useState(group.name);
  const [saving, setSaving] = useState(false);
  const dirty = value.trim() !== group.name && value.trim() !== "";
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  async function save() {
    if (!dirty) return;
    setSaving(true);
    try {
      await api.renameGroup(group.id, value.trim());
      queryClient.invalidateQueries({ queryKey: ["groups"] });
      queryClient.invalidateQueries({ queryKey: ["group", group.id] });
      // Every surface that renders a group by name: the sharing panel's
      // principal picker, an app's access list, the people page.
      queryClient.invalidateQueries({ queryKey: ["group-search"] });
      toast.success(`Renamed to “${value.trim()}”`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t rename this group");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-1.5">
      <Label htmlFor={`group-name-${group.id}`}>Group name</Label>
      <div className="flex items-center gap-2">
        <Input
          id={`group-name-${group.id}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter" && dirty) save(); }}
          autoComplete="off"
        />
        <Button variant="outline" size="sm" disabled={!dirty || saving} onClick={save}>
          {saving ? "Saving…" : "Save"}
        </Button>
      </div>
      <FieldHelp>
        Members, grants and owned apps are kept.
      </FieldHelp>
    </div>
  );
}

function GroupAddressField({
  group,
  onDirtyChange,
}: {
  group: Group;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [value, setValue] = useState(group.email ?? "");
  const [saving, setSaving] = useState(false);
  const dirty = value.trim() !== (group.email ?? "");
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  async function save() {
    setSaving(true);
    try {
      await api.setGroupEmail(group.id, value.trim());
      queryClient.invalidateQueries({ queryKey: ["groups"] });
      queryClient.invalidateQueries({ queryKey: ["group", group.id] });
      toast.success(value.trim() ? "Saved" : "Address cleared — members will be told instead");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save the address");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-1.5">
      <Label htmlFor={`group-email-${group.id}`}>Failure emails go to</Label>
      <div className="flex items-center gap-2">
        <Input
          id={`group-email-${group.id}`}
          type="email"
          className="font-mono text-xs"
          placeholder="oncall@example.com"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter" && dirty) save(); }}
          autoComplete="off"
        />
        <Button variant="outline" size="sm" disabled={!dirty || saving} onClick={save}>
          {saving ? "Saving…" : "Save"}
        </Button>
      </div>
      <FieldHelp>
        A team alias or ticket queue. Leave blank to email every member.
      </FieldHelp>
    </div>
  );
}

/**
 * The group's id, which is a wire parameter and not an internal detail.
 *
 * `POST /apps` presented with a deploy key requires `owner_group_id`, and a
 * deploy key cannot reach `GET /groups` to resolve a name to an id — the route
 * is not in `deployKeyAllowedRoutes` and a test asserts it never will be. Apps
 * have `by-slug` for exactly this and groups have no equivalent, so the id has
 * to arrive out of band, pasted by a person. Until this field existed the only
 * ways to get one were the API and psql.
 *
 * Administrators only, like the address above it: minting a deploy key is an
 * admin action, so nobody else is holding the question this answers.
 */
function GroupIdField({ group }: { group: Group }) {

  return (
    <div className="space-y-1.5">
      <Label htmlFor={`group-id-${group.id}`}>Group ID</Label>
      <div className="flex items-center gap-2">
        <Input
          id={`group-id-${group.id}`}
          readOnly
          value={group.id}
          onFocus={(e) => e.target.select()}
          // The md: variant is load-bearing, not a duplicate. Input's own base
          // class ends in `md:text-sm`, which outranks a bare `text-2xs` at
          // >=768px on source order, so the plain class is inert on a desktop
          // and 36 characters clip at the right edge of this dialog. A
          // truncated id is worse than a small one — it is the one field here
          // somebody reads character by character.
          className="flex-1 font-mono text-2xs md:text-2xs"
        />
        <Button variant="outline" size="icon" aria-label="Copy group ID" onClick={() => void copyToClipboard(group.id, `ID of ${group.name}`)}>
          <Copy className="h-4 w-4" />
        </Button>
      </div>
      <FieldHelp>
        Use as <code className="font-mono">owner_group_id</code> when a deploy key creates an app.
      </FieldHelp>
    </div>
  );
}

// ── Members ─────────────────────────────────────────────────

function GroupMembersDialog({
  group, canManage, isAdmin, open, onOpenChange,
}: {
  group: Group;
  canManage: boolean;
  isAdmin: boolean;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const owns = group.owns_apps ?? 0;
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [pendingId, setPendingId] = useState<string | null>(null);
  // The name and the address save on their own buttons, so closing with one
  // edited lost it silently. Which are edited, and whether the reader has
  // already been told once.
  const [dirty, setDirty] = useState<{ name: boolean; address: boolean }>({ name: false, address: false });
  const [warned, setWarned] = useState(false);
  const onNameDirty = useCallback((d: boolean) => setDirty((p) => (p.name === d ? p : { ...p, name: d })), []);
  const onAddressDirty = useCallback((d: boolean) => setDirty((p) => (p.address === d ? p : { ...p, address: d })), []);
  const unsaved = dirty.name || dirty.address;
  function requestClose() {
    if (unsaved && !warned) {
      setWarned(true);
      return;
    }
    onOpenChange(false);
  }

  const { data: detail, isLoading } = useQuery<GroupDetail>({
    queryKey: ["group", group.id],
    queryFn: () => api.getGroup(group.id),
    enabled: open,
  });
  const members = detail?.members;

  const q = search.trim();
  const candidatesQuery = useQuery({
    queryKey: ["directory-users", q],
    queryFn: () => api.searchDirectoryUsers(q),
    enabled: canManage && q.length >= 2,
  });
  const candidates = candidatesQuery.data ?? [];
  // Three states — see `directory-picker.tsx`, which is this same picker with
  // the same rule. `data = []` while the search is in flight and after it
  // failed both read as "No matching people."
  const searching = candidatesQuery.isFetching;
  const searchFailed = candidatesQuery.error;
  const memberIds = new Set((members ?? []).map((m) => m.id));
  const addable = candidates.filter((u) => !memberIds.has(u.id));

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["group", group.id] });
    queryClient.invalidateQueries({ queryKey: ["groups"] });
  }

  async function add(userId: string, login: string) {
    setPendingId(userId);
    try {
      await api.addGroupMember(group.id, userId);
      refresh();
      setSearch("");
      toast.success(`Added ${login} to “${group.name}”`);
    } catch (err) {
      // Backend 409s the 10-groups-per-user cap verbatim.
      toast.error(err instanceof Error ? err.message : `Couldn’t add ${login}`);
    } finally {
      setPendingId(null);
    }
  }

  async function remove(userId: string, login: string) {
    setPendingId(userId);
    try {
      await api.removeGroupMember(group.id, userId);
      refresh();
      toast.success(`Removed ${login} from “${group.name}” — their sessions refresh now`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t remove ${login}`);
    } finally {
      setPendingId(null);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => (o ? onOpenChange(true) : requestClose())}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{group.name}</DialogTitle>
          <DialogDescription>
            Members inherit every app access this group is granted
            {owns > 0 && (
              <>
                , and own {owns === 1 ? "the app" : `all ${owns} apps`} it owns
              </>
            )}
            .
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          {isAdmin && <GroupNameField group={group} onDirtyChange={onNameDirty} />}
          {isAdmin && <GroupAddressField group={group} onDirtyChange={onAddressDirty} />}
          {isAdmin && <GroupIdField group={group} />}

          {canManage && (
            <div className="space-y-1.5">
              <Label htmlFor="member-search" className="sr-only">Add members</Label>
              <Input
                id="member-search"
                placeholder="Add people by name…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                autoComplete="off"
              />
              {/* How many matched, said out loud. */}
              <p aria-live="polite" className="sr-only">
                {q.length < 2
                  ? ""
                  : searchFailed
                    ? "The directory could not be searched."
                    : searching
                      ? "Searching…"
                      : `${addable.length} matches`}
              </p>
              {q.length >= 2 && (
                <div className="max-h-40 divide-y divide-border/60 overflow-y-auto rounded-md border border-border/60">
                  {searchFailed ? (
                    <p className={`p-2.5 text-xs ${toneText.danger}`}>
                      {searchFailed instanceof Error && searchFailed.message
                        ? searchFailed.message
                        : "The directory could not be searched."}
                    </p>
                  ) : searching ? (
                    <p className="p-2.5 text-xs text-muted-foreground">Searching…</p>
                  ) : addable.length === 0 ? (
                    <p className="p-2.5 text-xs text-muted-foreground">No matching people.</p>
                  ) : (
                    addable.map((u) => (
                      <button
                        key={u.id}
                        type="button"
                        disabled={pendingId === u.id}
                        className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:opacity-50"
                        onClick={() => add(u.id, u.login)}
                      >
                        <UserAvatar id={u.id} login={u.login} avatarUrl={u.avatar_url} size="xs" />
                        <span className="font-medium">{u.login}</span>
                        <span className="ml-auto text-xs text-muted-foreground">{systemRoleLabel(u.system_role)}</span>
                      </button>
                    ))
                  )}
                </div>
              )}
            </div>
          )}

          {isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : !members ? (
            <Notice tone="neutral" size="sm">
              The member list is visible to administrators and the group’s own members.
            </Notice>
          ) : members.length === 0 ? (
            <EmptyState size="inline" title="No members yet" />
          ) : (
            <TableCard className="max-h-64 divide-y divide-border/60 overflow-y-auto" flat>
              {members.map((m) => (
                <div key={m.id} className="flex items-center gap-2.5 px-3 py-2">
                  <UserAvatar id={m.id} login={m.login} avatarUrl={m.avatar_url} size="xs" />
                  <span className="text-sm font-medium text-foreground">{m.login}</span>
                  {m.email && <span className="min-w-0 truncate text-xs text-muted-foreground">{m.email}</span>}
                  {m.source === "directory" && (
                    // Info, not attention: the platform did this, and it is
                    // working as configured rather than something anybody needs
                    // to act on.
                    <StatusBadge
                      status="directory"
                      tone="info"
                      label={m.source_provider_id || "Directory"}
                    />
                  )}
                  {canManage && m.source !== "directory" && (
                    <Button
                      variant="ghost"
                      size="icon-xs"
                      className="ml-auto"
                      aria-label={`Remove ${m.login}`}
                      disabled={pendingId === m.id}
                      onClick={() => remove(m.id, m.login)}
                    >
                      <X className="h-3.5 w-3.5" />
                    </Button>
                  )}
                </div>
              ))}
            </TableCard>
          )}

          {(members ?? []).some((m) => m.source === "directory") && (
            // A disabled button explained by a `title` is unreachable by every
            // route, so the reason goes beside the list.
            <Notice tone="info" size="sm" title="Some members come from a directory">
              The sign-in provider manages them. Detach the claim under Directory to stop it.
            </Notice>
          )}

          {isAdmin && <GroupDirectorySection group={group} members={members ?? []} />}
        </DialogBody>
        {/* One way out that says it is one. Every change above has already
            saved — the name and the address on their own buttons, members and
            claims on the click — except an edited name or address, which this
            stops once to say. */}
        <DialogFooter className="items-center">
          {warned && unsaved && (
            <p role="status" className={`mr-auto text-xs ${toneText.attention}`}>
              {dirty.name && dirty.address
                ? "The name and address aren’t saved."
                : dirty.name
                  ? "The name isn’t saved."
                  : "The address isn’t saved."}{" "}
              Close again to discard.
            </p>
          )}
          <Button onClick={requestClose}>Done</Button>
        </DialogFooter>
      </DialogContent>
      </Dialog>
    );
  }

  // ── Directory ───────────────────────────────────────────────

  /**
   * Where a group's membership may come from a directory (ADR §51).
   *
   * Administrators only, and absent rather than off for everyone else: a group
   * with no mapping is an ordinary group, and this section renders as an empty
   * state rather than as a feature nobody switched on.
   *
   * The rule the copy has to carry is the one that made the feature permissible
   * at all — a mapped group may hold access and may never *own* an app.
   */
  function GroupDirectorySection({
    group,
    members,
  }: {
    group: Group;
    members: NonNullable<GroupDetail["members"]>;
  }) {
    const queryClient = useQueryClient();
    const [providerId, setProviderId] = useState("");
    const [claimValue, setClaimValue] = useState("");
    const [saving, setSaving] = useState(false);
    /** A detach waiting on its confirm. Consequential but recoverable: every
     *  member that provider added leaves the group, and its app access, now. */
    const [toDetach, setToDetach] = useState<{ provider_id: string; claim_value: string } | null>(null);
    const leaving = toDetach
      ? members.filter((m) => m.source === "directory" && m.source_provider_id === toDetach.provider_id).length
      : 0;

    const { data, isLoading, error, refetch } = useQuery({
      queryKey: ["group-claim-mappings", group.id],
      queryFn: () => api.listGroupClaimMappings(group.id),
    });
    const { data: providers } = useQuery({
      queryKey: ["auth-providers-admin"],
      queryFn: () => api.listAuthProviders(),
    });
    const mappings = data?.mappings ?? [];

    // A password provider authenticates one local account and asserts no claims.
    const claimable = (providers?.providers ?? []).filter((p) => p.kind !== "password");

    function refresh() {
      queryClient.invalidateQueries({ queryKey: ["group-claim-mappings", group.id] });
      queryClient.invalidateQueries({ queryKey: ["group", group.id] });
      queryClient.invalidateQueries({ queryKey: ["groups"] });
    }

    async function attach() {
      if (!providerId || !claimValue.trim()) return;
      setSaving(true);
      try {
        await api.addGroupClaimMapping(group.id, providerId, claimValue.trim());
        setClaimValue("");
        refresh();
        // Nothing joins now: the reconciler adds a member when they next sign
        // in with the claim, and "now fills" read as instant.
        toast.success(
          `Members with the “${claimValue.trim()}” claim will join “${group.name}” when they next sign in`,
        );
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "Couldn’t attach the claim");
      } finally {
        setSaving(false);
      }
    }

    async function detach(m: { provider_id: string; claim_value: string }) {
      setSaving(true);
      try {
        await api.removeGroupClaimMapping(group.id, m.provider_id, m.claim_value);
        refresh();
        toast.success(`“${m.claim_value}” detached — the members it added are removed now`);
        setToDetach(null);
      } catch (err) {
        toast.error(err instanceof Error ? err.message : `Couldn’t detach “${m.claim_value}”`);
      } finally {
        setSaving(false);
      }
    }

    return (
      <div className="space-y-2 border-t border-border/60 pt-3">
        <SectionHeading
          level={3}
          title="Directory"
          description="Fill this group from a sign-in provider’s group claim. It then can’t own apps."
        />

        {error ? (
          <QueryError
            title="This group’s claim mappings could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        ) : isLoading ? (
          <Skeleton className="h-10 w-full" />
        ) : mappings.length === 0 ? (
          <EmptyState
            size="inline"
            title="No claims attached"
            description="Attach a claim value to let a provider manage membership."
          />
        ) : (
          <TableCard className="divide-y divide-border/60" flat>
            {mappings.map((m) => (
              <div key={`${m.provider_id}:${m.claim_value}`} className="flex items-center gap-2.5 px-3 py-2">
                <span className="font-mono text-xs text-foreground">{m.claim_value}</span>
                <span className="text-xs text-muted-foreground">from {m.provider_id}</span>
                <Button
                  variant="ghost"
                  size="icon-xs"
                  className="ml-auto"
                  aria-label={`Detach ${m.claim_value}`}
                  disabled={saving}
                  onClick={() => setToDetach(m)}
                >
                  <X className="h-3.5 w-3.5" />
                </Button>
              </div>
            ))}
          </TableCard>
        )}

        <div className="flex flex-wrap items-center gap-2">
          <Select value={providerId} onValueChange={(v) => setProviderId(v ?? "")}>
            <SelectTrigger className="w-44">
              <SelectValue>
                {(v) => (v ? claimable.find((p) => p.id === v)?.display_name ?? v : "Provider…")}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {claimable.map((p) => (
                <SelectItem key={p.id} value={p.id}>{p.display_name}</SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Input
            className="w-48 font-mono"
            placeholder="Claim value…"
            value={claimValue}
            onChange={(e) => setClaimValue(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") void attach(); }}
          />
          <Button size="sm" variant="secondary" disabled={saving || !providerId || !claimValue.trim()} onClick={() => void attach()}>
            {saving && !toDetach ? "Attaching…" : "Attach"}
          </Button>
        </div>

        <Dialog open={toDetach !== null} onOpenChange={(o) => { if (!o && !saving) setToDetach(null); }}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Detach “{toDetach?.claim_value}”?</DialogTitle>
              <DialogDescription>
                {leaving === 0
                  ? `Nobody in ${group.name} came from this provider, so no one leaves.`
                  : `${leaving} member${leaving === 1 ? "" : "s"} added by ${toDetach?.provider_id} leave${leaving === 1 ? "s" : ""} ${group.name} and lose${leaving === 1 ? "s" : ""} its app access now.`}{" "}
                Attaching the claim again adds them back as they sign in.
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" disabled={saving} onClick={() => setToDetach(null)}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                disabled={saving}
                onClick={() => toDetach && void detach(toDetach)}
              >
                {saving ? "Detaching…" : "Detach"}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </div>
    );
  }

  // ── Delete ──────────────────────────────────────────────────

  function DeleteGroupDialog({
    group, open, onOpenChange,
  }: {
    group: Group;
    open: boolean;
    onOpenChange: (o: boolean) => void;
  }) {
    const queryClient = useQueryClient();
    const [deleting, setDeleting] = useState(false);
    const [held, setHeld] = useState<HeldStorageMapping[] | null>(null);
    const owns = group.owns_apps ?? 0;

    async function destroy() {
      setDeleting(true);
      try {
        await api.deleteGroup(group.id);
        queryClient.invalidateQueries({ queryKey: ["groups"] });
        toast.success(`Deleted “${group.name}”`);
        onOpenChange(false);
      } catch (err) {
        const mappings = heldStorageMappings(err);
        if (mappings) setHeld(mappings);
        toast.error(err instanceof Error ? err.message : `Couldn’t delete “${group.name}”`);
      } finally {
        setDeleting(false);
      }
    }

    return (
      <Dialog open={open} onOpenChange={(o) => { if (!deleting) onOpenChange(o); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {group.name}?</DialogTitle>
            <DialogDescription>
              {owns
                ? "A group that owns apps can’t be deleted."
                : "Every app shared with this group loses that access immediately. This can’t be undone."}
            </DialogDescription>
          </DialogHeader>
          {/* A group that owns apps cannot be deleted, and the server refuses it
              regardless — an app whose owning group is gone is unadministrable.
              Said here rather than delivered as a 409, so the button is honest
              about what it will do before it is pressed. */}
          {held && <HeldStorageNotice subject={group.name} mappings={held} />}
          {!!owns && (
            <Notice
              tone="danger"
              title="This group owns apps"
              // The way out, offered rather than described. The notice named a
              // number and an act, and left finding the apps to the reader.
              action={
                <Link
                  to={`/?scope=all&owner=group:${group.id}`}
                  className="font-medium whitespace-nowrap hover:underline"
                >
                  {owns === 1 ? "Show it" : "Show them"}
                </Link>
              }
            >
              Transfer {owns === 1 ? "its app" : `its ${owns} apps`} to someone else first, from each
              app’s Settings tab.
            </Notice>
          )}
          <DialogFooter>
            {/* With apps owned the server refuses, so there is no destructive
                button to press — only the way out above, and Close. */}
            <Button variant="outline" disabled={deleting} onClick={() => onOpenChange(false)}>
              {owns ? "Close" : "Cancel"}
            </Button>
            {!owns && (
              <Button variant="destructive" disabled={deleting} onClick={destroy}>
                {deleting ? "Deleting…" : "Delete group"}
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
    </Dialog>
  );
}

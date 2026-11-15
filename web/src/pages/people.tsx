import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Download, UserCog, Users } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { UserAvatar } from "@/components/user-avatar";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { TablePagination } from "@/components/table-pagination";
import { SearchInput } from "@/components/search-input";
import { FilterBar } from "@/components/filter-bar";
import { FacetChips, FacetFilter } from "@/components/facet-filter";
import { useUrlParam, useUrlParams } from "@/hooks/use-list-state";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { StatusBadge } from "@/components/status-badge";
import { SortableHead } from "@/components/sortable-head";
import { TableCard } from "@/components/table-card";
import { nextSort, type SortState } from "@/lib/sort";
import { usersToXlsx } from "@/lib/export-xlsx";
import { GroupsTab } from "@/components/groups-tab";
import { MakeAdminDialog } from "@/components/make-admin-dialog";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import type { User } from "@/lib/api";
import { systemRoleInSentence, systemRoleLabel } from "@/lib/utils";

export function PeoplePage() {
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "groups" ? "groups" : "users";

  /**
   * How accounts actually come to exist on *this* install.
   *
   * The subtitle used to assert "Accounts are created automatically on first
   * sign-in" whatever the providers were set to — while the same install's own
   * refusal message told people the opposite: "you do not have an account on
   * this platform and accounts are not created automatically. Ask an
   * administrator to set one up for you." A page cannot state a policy it does
   * not read; auto-provisioning is per provider, and an administrator turning
   * it off should not leave a page here contradicting them.
   *
   * A read that a non-admin cannot make, so it fails quietly to the sentence
   * that is true either way rather than showing an error on a page about
   * something else.
   */
  const { data: providers } = useQuery({
    queryKey: ["auth-providers", "admin"],
    queryFn: api.listAuthProviders,
    retry: false,
  });
  // Three answers now, because a provider may create an account and still not
  // let it in. "Enough to get one" would be false on an install whose only
  // provisioning provider queues new accounts — the same contradiction this
  // sentence was rewritten to stop telling.
  const provisioners = (providers?.providers ?? []).filter((p) => p.enabled && p.auto_provision);
  const provisioning =
    providers === undefined
      ? ""
      : provisioners.some((p) => p.default_role !== "pending")
        ? "Signing in creates an account."
        : provisioners.length > 0
          ? "Signing in creates an account that waits under Pending."
          : "Accounts must be added by an administrator.";

  return (
    <div>
      <PageHeader
        title="People"
        description={provisioning || undefined}
      />
      <Tabs
        value={tab}
        onValueChange={(v) => setParams(v === "groups" ? { tab: "groups" } : {}, { replace: true })}
      >
        <TabsList variant="line">
          <TabsTrigger value="users">Users</TabsTrigger>
          <TabsTrigger value="groups">Groups</TabsTrigger>
        </TabsList>
        <TabsContent value="users" className="pt-4">
          <UsersTab />
        </TabsContent>
        <TabsContent value="groups" className="pt-4">
          <GroupsTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}

// ── Users ───────────────────────────────────────────────────

const roleFilters = [
  { label: "Admin", value: "system_admin" },
  { label: "Publisher", value: "publisher" },
  { label: "Viewer", value: "viewer" },
];

// Dormant is a filter rather than a status: it is not a state stored on the
// row, it is the "last used" column compared to the install's threshold, and an
// account can be dormant and locked at the same time.
const statusFilters = [
  { label: "All", value: "all" },
  { label: "Active", value: "active" },
  // Pending is the one filter here that is a queue rather than a lens: these
  // accounts exist, cannot sign in, and are waiting on somebody on this page.
  // It is the deep link the activation notice sends an administrator to.
  { label: "Pending", value: "pending" },
  { label: "Locked", value: "locked" },
  { label: "Dormant", value: "dormant" },
];

/** The time columns are the sortable ones: "who used this last" and "who is
 * new" are the questions this table gets asked. `last_active_at` is always
 * present, so unlike the sign-in column it was replacing there is no
 * never-happened case to sort to the bottom. */
type SortKey = "created_at" | "last_active_at";

function UsersTab() {
  const { user: me } = useAuth();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  // The narrowing lives in the URL (`use-list-state`), replaced rather than
  // pushed: a narrowed directory is something to send somebody, and the
  // activation notice's link to `?status=pending` is one of these.
  const [search, setSearch] = useUrlParam("q");
  const [roleFilter, setRoleFilter] = useUrlParams("role");
  const [rawStatus, setStatusFilter] = useUrlParam("status", "all");
  const statusFilter = statusFilters.some((f) => f.value === rawStatus) ? rawStatus : "all";
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(25);
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "created_at", dir: "asc" });
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [exporting, setExporting] = useState(false);
  // The one role change that confirms first — see MakeAdminDialog.
  const [toPromote, setToPromote] = useState<User | null>(null);

  function toggleSort(key: SortKey) {
    setSort((s) => nextSort(s, key));
    setPage(1);
  }

  const { data: users = [], isLoading, error, refetch } = useQuery<User[]>({
    queryKey: ["users"],
    queryFn: api.listUsers,
    retry: false,
  });

  // The dormancy threshold, so this page can say which accounts the sweep
  // considers unused. Read whether or not the sweep is enabled — the whole
  // point of the column is to be able to answer that question before turning
  // the feature on. A failed read leaves the filter comparing against nothing,
  // which is why `dormantCutoff` is null rather than a guessed default: a
  // guessed threshold would put "Dormant" beside accounts nobody is going to
  // touch.
  const { data: platformSettings } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
    retry: false,
  });
  // Who is waiting on an answer, so a row that is about to be promoted says
  // so. The endpoint's own default is pending; there is no client-side history
  // filter to drift.
  const { data: pendingPromotions } = useQuery({
    queryKey: ["promotion-requests", "pending"],
    queryFn: () => api.adminListPromotionRequests("pending"),
    retry: false,
  });
  const askedToPublish = new Set(
    (pendingPromotions ?? []).map((r) => r.requester_id),
  );

  const dormantDays = platformSettings?.dormant_lock_days ?? null;
  const dormantSweepOn = platformSettings?.dormant_lock_enabled ?? false;
  // The clock is read once, on mount: the threshold is whole days, so a page
  // left open for an afternoon counts the same accounts.
  const [now] = useState(() => Date.now());
  const dormantCutoff = dormantDays === null ? null : now - dormantDays * 86_400_000;

  function isDormant(u: User) {
    if (dormantCutoff === null || !u.last_active_at) return false;
    return Date.parse(u.last_active_at) < dormantCutoff;
  }

  /**
   * Activation is the exception to "actions live on the account record".
   *
   * Locking is there because it ends live sessions and has to state what
   * survives first. This is the opposite act on an account that has no sessions
   * to end, and it is what this filter *is*: a queue an administrator works
   * through, which is not a queue if each row costs a page load. So the role is
   * chosen and applied in place, and the account record carries the same
   * control for whoever arrives from a link.
   */
  async function activate(user: User, role: "publisher" | "viewer") {
    setPendingId(user.id);
    try {
      await api.activateUser(user.id, role);
      queryClient.invalidateQueries({ queryKey: ["users"] });
      toast.success(`${user.login} can sign in now, as ${systemRoleInSentence(role)}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t activate the account");
    } finally {
      setPendingId(null);
    }
  }

  // Locking lives on the account record, not here: it ends live sessions, so
  // it states what survives before it happens.
  async function changeRole(
    user: User,
    role: "system_admin" | "publisher" | "viewer",
    confirmed = false,
  ) {
    if (role === user.system_role) return;
    if (role === "system_admin" && !confirmed) {
      setToPromote(user);
      return;
    }
    setPendingId(user.id);
    try {
      await api.updateUserRole(user.id, role);
      queryClient.invalidateQueries({ queryKey: ["users"] });
      toast.success(`${user.login} is now ${systemRoleInSentence(role)} — their sessions refresh now`);
    } catch (err) {
      // Backend 409s the last-admin invariant verbatim.
      toast.error(err instanceof Error ? err.message : "Couldn’t change the role");
    } finally {
      setPendingId(null);
      setToPromote(null);
    }
  }

  /**
   * Everything except the status axis, so the tab strip's counts can be taken
   * from it.
   *
   * "Dormant (0)" has to mean zero dormant accounts among the ones searched and
   * narrowed by role, or the count sits above a table that visibly disagrees
   * with it — the same split the apps list makes, for the same reason.
   */
  const scoped = useMemo(() => {
    const q = search.toLowerCase();
    return users.filter(
      (u) =>
        (u.login.toLowerCase().includes(q) ||
          (u.name ?? "").toLowerCase().includes(q) ||
          u.email.toLowerCase().includes(q)) &&
        (roleFilter.length === 0 || roleFilter.includes(u.system_role ?? "")),
    );
  }, [users, search, roleFilter]);

  const matchesStatus = useCallback(
    (u: User, status: string) =>
      status === "all" ? true :
      status === "locked" ? !!u.locked_at :
      status === "pending" ? !!u.pending_activation_at :
      status === "dormant" ? isDormant(u) :
      // Active means it can actually be used, so an account nobody has
      // activated is not one — it would otherwise sit under "Active" being
      // refused at every sign-in.
      !u.locked_at && !u.pending_activation_at,
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [dormantCutoff],
  );

  const filtered = useMemo(() => {
    const rows = scoped.filter((u) => matchesStatus(u, statusFilter));
    const at = (u: User) =>
      sort.key === "created_at"
        ? Date.parse(u.created_at)
        : u.last_active_at ? Date.parse(u.last_active_at) : 0;
    return rows.sort((a, b) => (sort.dir === "asc" ? at(a) - at(b) : at(b) - at(a)));
  }, [scoped, statusFilter, matchesStatus, sort]);

  const statusCounts = useMemo(() => {
    const c: Record<string, number> = {};
    for (const f of statusFilters) c[f.value] = scoped.filter((u) => matchesStatus(u, f.value)).length;
    return c;
  }, [scoped, matchesStatus]);


  const paged = filtered.slice((page - 1) * pageSize, page * pageSize);

  // Exports what the table currently shows across all pages, not one page.
  async function exportUsers() {
    setExporting(true);
    try {
      await usersToXlsx(filtered);
      toast.success("Users exported");
    } catch {
      toast.error("Couldn’t build the Excel file — try again");
    } finally {
      setExporting(false);
    }
  }

  if (error) {
    return (
      <QueryError
        title="The directory could not be read"
        error={error}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={() => void refetch()}
      />
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4 flex-wrap">
        <SearchInput placeholder="Search by name, login or email…" value={search} onChange={(v) => { setSearch(v); setPage(1); }} className="w-full sm:max-w-sm" />
        {/* Role is a facet: status is the primary axis, the counted strip
            below — "Dormant (4)" is the answer somebody opens this page to
            get. A second chip row between the search and the strip made three
            layers of navigation over one table. */}
        <FacetFilter
          label="Role"
          icon={<UserCog />}
          options={roleFilters.map((f) => ({ key: f.value, label: f.label, prefix: "Role:" }))}
          value={roleFilter}
          onChange={(v) => { setRoleFilter(v); setPage(1); }}
          placeholder="Filter by role…"
        />
        <div className="ml-auto flex items-center gap-3">
          {/* Only when the filters have taken something away. The whole count
              is on the pagination line under the table — "Showing 1 to 8 of
              8" — in the grammar every other table in the product uses, and a
              page that states its count twice in two grammars teaches neither.
              What survives here is the number that line cannot give: how much
              of the directory the filters removed. */}
          {!isLoading && filtered.length !== users.length && (
            <p className="text-sm text-muted-foreground">
              {filtered.length} of {users.length} users
            </p>
          )}
          <Button
            variant="ghost"
            size="sm"
            disabled={exporting || isLoading || filtered.length === 0}
            onClick={exportUsers}
          >
            <Download className="mr-1.5 h-3.5 w-3.5" />
            {exporting ? "Exporting…" : "Export to Excel"}
          </Button>
        </div>
      </div>

      <FacetChips
        chips={roleFilter.map((r) => ({
          option: { key: r, label: roleFilters.find((f) => f.value === r)?.label ?? r, prefix: "Role:" },
          onRemove: () => { setRoleFilter(roleFilter.filter((x) => x !== r)); setPage(1); },
        }))}
        onClear={() => { setRoleFilter([]); setPage(1); }}
      />

      <FilterBar
        label="Filter accounts by status"
        options={statusFilters.map((f) => ({
          ...f,
          label: `${f.label} (${statusCounts[f.value] ?? 0})`,
        }))}
        value={statusFilter}
        onChange={(v) => { setStatusFilter(v); setPage(1); }}
      />

      {/* The queue's own sentence. Above the table for the reason the dormancy
          one is: it says what the rows *are*, and these look like ordinary
          accounts with an ordinary role until somebody says that none of it is
          in effect yet. */}
      {statusFilter === "pending" && (
        <Notice
          tone="info"
          title="These accounts can’t sign in yet"
          action={
            <Link to="/admin/authentication" className="text-primary hover:underline">
              Sign-in providers
            </Link>
          }
        >
          Their sign-ins are refused until you activate them with a role.
        </Notice>
      )}

      {/* Above the table, not below it: it says what the list *means*, and a
          reader who has already scanned the rows has been given the numbers
          without the sentence that qualifies them. What "dormant" means here,
          and whether anything is acting on it — the same list reads two
          completely different ways depending on that second fact. */}
      {statusFilter === "dormant" && dormantDays !== null && (
        <Notice
          tone={dormantSweepOn ? "attention" : "info"}
          title={
            dormantSweepOn
              ? `Accounts unused for ${dormantDays} days — these are being locked`
              : `Accounts unused for ${dormantDays} days — nothing is locking them`
          }
          action={
            <Link to="/admin/authentication#dormant" className="text-primary hover:underline">
              Account settings
            </Link>
          }
        >
          {dormantSweepOn
            ? "Administrators are never locked automatically; review any listed here by hand."
            : "Turning locking on would lock everyone listed here except administrators."}
        </Notice>
      )}

      {isLoading ? (
        <TableCard className="p-4 space-y-3">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="flex items-center gap-3">
              <Skeleton className="h-7 w-7 rounded-full" />
              <Skeleton className="h-4 w-40" />
              <Skeleton className="h-4 w-56" />
              <Skeleton className="ml-auto h-4 w-20" />
            </div>
          ))}
        </TableCard>
      ) : filtered.length === 0 ? (
        // An empty dormant list is an answer, not a dead end. "Try adjusting
        // your filters" tells someone auditing this install to go looking for
        // something that is not there — the finding *is* that nothing is
        // there, and it is the good outcome.
        statusFilter === "dormant" && !search && roleFilter.length === 0 ? (
          <EmptyState
            icon={<Users className="h-6 w-6" />}
            title="No unused accounts"
            description={
              dormantDays === null
                ? "Every account has been used recently."
                : `Every account has been used in the last ${dormantDays} days.`
            }
          />
        ) : (
          <EmptyState
            icon={<Users className="h-6 w-6" />}
            title="No users match"
            description={`None of the ${users.length} user${users.length === 1 ? "" : "s"} match your search and filters.`}
            action={
              <Button
                variant="secondary"
                size="sm"
                onClick={() => {
                  setSearch("");
                  setRoleFilter([]);
                  setStatusFilter("all");
                  setPage(1);
                }}
              >
                Clear search and filters
              </Button>
            }
          />
        )
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">User</TableHead>
                {/* Column priority. User, Role and Status stay: this page
                    exists to answer "who is this and what may they do". */}
                <TableHead className="hidden @3xl:table-cell">Email</TableHead>
                <TableHead>Role</TableHead>
                <TableHead className="hidden w-[100px] @md:table-cell">Status</TableHead>
                <SortableHead label="Joined" sortKey="created_at" sort={sort} onSort={toggleSort} className="hidden w-[130px] @4xl:table-cell" />
                {/* "Last used", not "Last sign-in", and the difference is the
                    whole reason this column changed: a personal API key
                    authenticates its owner without minting a session, so an
                    account running a nightly job signed in once and looked
                    abandoned here. The sign-in on its own is still on the
                    account record, where there is room to show both. */}
                <SortableHead label="Last used" sortKey="last_active_at" sort={sort} onSort={toggleSort} className="hidden w-[130px] @xl:table-cell" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {paged.map((user) => {
                const isMe = user.id === me?.id;
                return (
                  // The whole row opens the account record; the controls
                  // inside it stop the click so they still act in place.
                  <TableRow
                    key={user.id}
                    className="cursor-pointer"
                    onClick={() => navigate(`/people/${user.id}`)}
                  >
                    {/* User takes the leftover and truncates in it; the login
                        beside a name yields first. */}
                    <TableCell className="max-w-0">
                      <div className="flex min-w-0 items-center gap-2.5">
                        <UserAvatar id={user.id} login={user.login} name={user.name} avatarUrl={user.avatar_url} />
                        <Link
                          to={`/people/${user.id}`}
                          onClick={(e) => e.stopPropagation()}
                          className="min-w-0 truncate font-medium text-foreground hover:text-primary hover:underline"
                        >
                          {user.name || user.login}
                        </Link>
                        {user.name && (
                          <span className="hidden min-w-0 truncate text-xs text-muted-foreground @md:inline">{user.login}</span>
                        )}
                        {isMe && <Badge variant="secondary" className="shrink-0 text-2xs">You</Badge>}
                      </div>
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">{user.email}</TableCell>
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      {user.pending_activation_at ? (
                        // The role this account will hold, chosen at the moment
                        // it is let in. It is a role picker that commits rather
                        // than one that saves: nothing about this account is
                        // decided until the button beside it is pressed.
                        <ActivateCell
                          user={user}
                          pending={pendingId === user.id}
                          onActivate={activate}
                        />
                      ) : isMe ? (
                        // Changing your own role revokes your own session
                        // mid-flight; another admin has to do it.
                        // A neutral badge: blue text read as a link.
                        <Badge variant="secondary" className="ml-2.5 text-xs font-normal">
                          {systemRoleLabel(user.system_role)}
                        </Badge>
                      ) : (
                        <Select
                          value={user.system_role}
                          onValueChange={(v) => changeRole(user, v as "system_admin" | "publisher" | "viewer")}
                        >
                          {/* Quiet until reached for — read as the value, drawn as
                              a control on hover and focus. Eight outlined
                              pickers down a column made the directory look like
                              a form waiting to be filled in, when changing a
                              role is the rare act on this page. */}
                          <SelectTrigger
                            size="sm"
                            disabled={pendingId === user.id}
                            aria-label={`Role for ${user.login}`}
                            className="border-transparent bg-transparent shadow-none hover:border-border hover:bg-card focus-visible:border-ring aria-expanded:border-border"
                          >
                            {/* base-ui's Value renders the raw value, not the
                                chosen item's text — format it here. */}
                            <SelectValue>{(v) => systemRoleLabel(v as string)}</SelectValue>
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="system_admin">Admin</SelectItem>
                            <SelectItem value="publisher">Publisher</SelectItem>
                            <SelectItem value="viewer">Viewer</SelectItem>
                          </SelectContent>
                        </Select>
                      )}
                      {/* They asked. An administrator changing a role from this
                          row could not see that, so the queue held requests for
                          roles already granted. Not a control — the decision,
                          with its reason dialog, stays on the queue. */}
                      {askedToPublish.has(user.id) && (
                        <Link
                          to={`/people/${user.id}`}
                          className="mt-1 flex text-2xs text-muted-foreground hover:underline"
                        >
                          Asked to publish
                        </Link>
                      )}
                    </TableCell>
                    <TableCell className="hidden @md:table-cell">
                      <StatusBadge
                        status={
                          user.locked_at
                            ? "locked"
                            : user.pending_activation_at
                              ? "pending_activation"
                              : "active"
                        }
                      />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @4xl:table-cell">
                      <RelativeTime iso={user.created_at} />
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                      {user.last_active_at ? <RelativeTime iso={user.last_active_at} /> : "—"}
                      {/* Said out loud where it would otherwise read as a
                          contradiction: a clock of two days beside an account
                          that has never signed in is an API key, not a bug. */}
                      {user.last_key_use_at && !user.last_login_at && (
                        <span className="block text-2xs">via API key</span>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableCard>
      )}

      {!isLoading && filtered.length > 0 && (
        <TablePagination
          total={filtered.length}
          page={page}
          pageSize={pageSize}
          onPage={setPage}
          onPageSize={setPageSize}
        />
            )}

      {toPromote && (
        <MakeAdminDialog
          login={toPromote.login}
          open
          pending={pendingId === toPromote.id}
          onConfirm={() => void changeRole(toPromote, "system_admin", true)}
          onOpenChange={(o) => !o && setToPromote(null)}
        />
      )}
    </div>
  );
}

/**
 * The queue's one control: which role this account will hold, and the button
 * that lets it in.
 *
 * Two steps rather than one, deliberately. A select that activated on change
 * would make "Viewer" — the value already on screen — unreachable without
 * choosing something else first, and would turn a misclick into somebody's
 * access. Administrator is not offered: the API refuses it, and promoting is a
 * separate act with its own row on the account record.
 */
function ActivateCell({
  user,
  pending,
  onActivate,
}: {
  user: User;
  pending: boolean;
  onActivate: (user: User, role: "publisher" | "viewer") => void;
}) {
  const [role, setRole] = useState<"publisher" | "viewer">("viewer");
  return (
    <div className="flex items-center gap-1.5">
      <Select value={role} onValueChange={(v) => setRole((v as "publisher" | "viewer") ?? "viewer")}>
        <SelectTrigger size="sm" disabled={pending} aria-label={`Role to activate ${user.login} as`}>
          <SelectValue>{(v) => systemRoleLabel(v as string)}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="viewer">Viewer</SelectItem>
          <SelectItem value="publisher">Publisher</SelectItem>
        </SelectContent>
      </Select>
      <Button
        size="sm"
        disabled={pending}
        onClick={() => onActivate(user, role)}
        aria-label={`Activate ${user.login}`}
      >
        {pending ? "Activating…" : "Activate"}
      </Button>
    </div>
  );
}

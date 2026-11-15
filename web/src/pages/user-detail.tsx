import { ParentCrumb } from "@/components/parent-crumb";
import { HeldStorageNotice } from "@/components/held-storage-notice";
import { heldStorageMappings } from "@/lib/held-storage";
import { toneBorder } from "@/lib/tone";
import { Fragment, useState } from "react";
import { useNavigate, useParams, Link } from "react-router-dom";
import { buttonVariants } from "@/components/ui/button-variants";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ChevronDown, Lock, LockOpen, MoreHorizontal, Trash2, Users,
} from "lucide-react";
import { toast } from "sonner";
import { UserAvatar } from "@/components/user-avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { AccessTable } from "@/components/access-table";
import { UserAppUsageSection } from "@/components/user-app-usage";
import { AccountFacts } from "@/components/account-facts";
import { Notice } from "@/components/notice";
import { KeyStateBadge } from "@/components/key-state-badge";
import { KeyScopeBadge } from "@/components/key-scope-badge";
import { KeyUsagePanel } from "@/components/key-usage-panel";
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { RelativeTime } from "@/components/relative-time";
import { Section, SectionHeading } from "@/components/section";
import { StatusBadge } from "@/components/status-badge";
import { EmptyState } from "@/components/empty-state";
import { FieldRow } from "@/components/field-row";
import { Switch } from "@/components/ui/switch";
import { TableCard } from "@/components/table-card";
import { MakeAdminDialog } from "@/components/make-admin-dialog";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import type { HeldStorageMapping, User, UserApiKey } from "@/lib/api";
import { systemRoleInSentence, systemRoleLabel } from "@/lib/utils";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { ConfirmPhrase } from "@/components/confirm-phrase";

/**
 * Admin account record for one user: who they are, when they joined, when they
 * last signed in, which groups they belong to, which apps they reach and why,
 * the external logins that reach the account, and the personal API keys that
 * act as it while nobody is signed in. Read-mostly — the two writes
 * (role, lock) are the same ones the People list offers, kept here so an admin
 * reviewing an account does not have to go back to change it.
 */
export function UserDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { user: me } = useAuth();
  const [pending, setPending] = useState(false);
  const [confirmLock, setConfirmLock] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [expandedKey, setExpandedKey] = useState<string | null>(null);
  const [keyBusy, setKeyBusy] = useState<string | null>(null);
  const [keyToRevoke, setKeyToRevoke] = useState<UserApiKey | null>(null);
  const [movingApps, setMovingApps] = useState(false);
  // The one role change that confirms first — see MakeAdminDialog.
  const [confirmPromote, setConfirmPromote] = useState(false);

  /** Both views of one key. Invalidated together so they cannot disagree. */
  function invalidateKeys() {
    queryClient.invalidateQueries({ queryKey: ["user-api-keys", id] });
    queryClient.invalidateQueries({ queryKey: ["all-api-keys"] });
  }

  async function toggleKeyDisabled(key: UserApiKey) {
    const disabling = key.state !== "disabled";
    setKeyBusy(key.id);
    try {
      await api.adminUpdateApiKey(key.id, { disabled: disabling });
      toast.success(disabling ? `“${key.name}” disabled — calls using it now fail` : `“${key.name}” enabled`);
      invalidateKeys();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t change “${key.name}”`);
    } finally {
      setKeyBusy(null);
    }
  }

  async function revokeKey(key: UserApiKey) {
    setKeyBusy(key.id);
    try {
      await api.adminDeleteApiKey(key.id);
      toast.success(`“${key.name}” revoked`);
      setKeyToRevoke(null);
      invalidateKeys();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t revoke “${key.name}”`);
    } finally {
      setKeyBusy(null);
    }
  }

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["user", id],
    queryFn: () => api.getUser(id!),
    enabled: !!id,
    retry: false,
  });

  // The access report is the one place that computes an effective role per
  // app; the detail page reads it rather than re-deriving access from grants.
  const { data: access, error: accessError, refetch: refetchAccess } = useQuery({
    queryKey: ["user-access", id],
    queryFn: () => api.getUserAccess(id!),
    enabled: !!id,
    retry: false,
  });

  // The credentials behind the clock. Its own query rather than a field on the
  // account record: it is admin-only where the record's own read is not quite
  // the same gate, and it stays answerable while personal keys are switched
  // off, which the record has no opinion about.
  const { data: keys, error: keysError, refetch: refetchKeys } = useQuery({
    queryKey: ["user-api-keys", id],
    queryFn: () => api.listUserApiKeys(id!),
    enabled: !!id,
    retry: false,
  });

  const user = data?.user;
  useDocumentTitle(user ? user.name || user.login : undefined);
  const isMe = !!user && user.id === me?.id;
  const locked = !!user?.locked_at;
  const awaitingActivation = !!user?.pending_activation_at;

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["user", id] });
    queryClient.invalidateQueries({ queryKey: ["users"] });
  }

  async function changeRole(role: "system_admin" | "publisher" | "viewer", confirmed = false) {
    if (!user || role === user.system_role) return;
    if (role === "system_admin" && !confirmed) {
      setConfirmPromote(true);
      return;
    }
    setPending(true);
    try {
      await api.updateUserRole(user.id, role);
      refresh();
      toast.success(`${user.login} is now ${systemRoleInSentence(role)} — their sessions refresh now`);
    } catch (err) {
      // The backend 409s the last-admin invariant verbatim.
      toast.error(err instanceof Error ? err.message : "Couldn’t change the role");
    } finally {
      setPending(false);
      setConfirmPromote(false);
    }
  }

  // The same act the People page's queue performs, for whoever arrives here
  // from a link instead. One implementation of the decision would be better;
  // one *call* is all that is shared, because the two surfaces differ in
  // nothing else.
  async function activate(role: "publisher" | "viewer") {
    if (!user) return;
    setPending(true);
    try {
      await api.activateUser(user.id, role);
      refresh();
      toast.success(`${user.login} can sign in now, as ${systemRoleInSentence(role)}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t activate the account");
    } finally {
      setPending(false);
    }
  }

  async function setScanner(next: boolean) {
    if (!user) return;
    setPending(true);
    try {
      await api.setScanner(user.id, next);
      refresh();
      toast.success(
        next
          ? `${user.login} is the scanner account — its scans are not counted as usage`
          : `${user.login} is no longer a scanner account`
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t change the scanner mark");
    } finally {
      setPending(false);
    }
  }

  async function setLocked(next: boolean) {
    if (!user) return;
    setPending(true);
    try {
      if (next) {
        await api.lockUser(user.id);
      } else {
        await api.unlockUser(user.id);
      }
      refresh();
      setConfirmLock(false);
      toast.success(
        next
          ? `Locked ${user.login} — their sessions end now`
          : `Unlocked ${user.login} — they can sign in again`
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t ${next ? "lock" : "unlock"} the account`);
    } finally {
      setPending(false);
    }
  }

  if (error) {
    return (
      <QueryError
        title="This account could not be read"
        error={error}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={() => void refetch()}
      />
    );
  }

  if (isLoading || !user) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-4 w-40" />
        <Skeleton className="h-12 w-72" />
        <Skeleton className="h-28 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  // Personally owned, which is the set that blocks deleting the account.
  // Deliberately not "apps they hold owner on" — that now includes everything
  // owned by every group they belong to, and counting those here would put the
  // number back that group ownership exists to remove.
  const ownedApps = (access?.explicit ?? []).filter((e) =>
    e.sources.some((s) => s.kind === "owner")
  ).length;
  const ownedViaGroup = (access?.explicit ?? []).filter((e) =>
    e.sources.some((s) => s.kind === "owner_via_group")
  ).length;

  return (
    <div className="space-y-6">
      <ParentCrumb to="/people" parent="People" current={user.login} />

      <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-3">
          <UserAvatar id={user.id} login={user.login} name={user.name} avatarUrl={user.avatar_url} size="lg" />
          <div>
            <div className="flex items-center gap-2.5">
              <h1 className="text-2xl font-semibold tracking-tight">{user.name || user.login}</h1>
              <StatusBadge
                status={locked ? "locked" : awaitingActivation ? "pending_activation" : "active"}
              />
              {isMe && <Badge variant="secondary" className="text-2xs">You</Badge>}
            </div>
            <p className="mt-0.5 text-sm text-muted-foreground">
              {user.name && user.name !== user.login ? `${user.login} · ${user.email}` : user.email}
            </p>
          </div>
        </div>

        <div className="flex items-center gap-2">
          {isMe ? (
            // Changing your own role or locking yourself revokes your own
            // session mid-flight; another admin has to do it.
            <span className="text-sm text-muted-foreground">
              Another admin manages your role and access.
            </span>
          ) : awaitingActivation ? (
            // No role picker while an account is in the queue: its role is
            // chosen by the act of letting it in, and two controls writing the
            // same field with only one of them able to admit anybody is a
            // choice nobody can read. The lock stays — it is how an
            // administrator says "not this person" without deleting the row,
            // and it survives an activation.
            <>
              <ActivateControl pending={pending} onActivate={activate} />
              {locked ? (
                <Button variant="outline" size="sm" disabled={pending} onClick={() => setLocked(false)}>
                  <LockOpen className="mr-1.5 h-3.5 w-3.5" />
                  {pending ? "Unlocking…" : "Unlock account"}
                </Button>
              ) : (
                <Button variant="outline" size="sm" disabled={pending} onClick={() => setConfirmLock(true)}>
                  <Lock className="mr-1.5 h-3.5 w-3.5" />
                  Lock account…
                </Button>
              )}
            </>
          ) : (
            <>
              <Select
                value={user.system_role}
                onValueChange={(v) => changeRole(v as "system_admin" | "publisher" | "viewer")}
              >
                <SelectTrigger size="sm" disabled={pending} aria-label={`Role for ${user.login}`}>
                  {/* base-ui's Value renders the raw value, not the chosen
                      item's text — format it here. */}
                  <SelectValue>{(v) => systemRoleLabel(v as string)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="system_admin">Admin</SelectItem>
                  <SelectItem value="publisher">Publisher</SelectItem>
                  <SelectItem value="viewer">Viewer</SelectItem>
                </SelectContent>
              </Select>
              {locked ? (
                <Button variant="outline" size="sm" disabled={pending} onClick={() => setLocked(false)}>
                  <LockOpen className="mr-1.5 h-3.5 w-3.5" />
                  {pending ? "Unlocking…" : "Unlock account"}
                </Button>
              ) : (
                <Button variant="outline" size="sm" disabled={pending} onClick={() => setConfirmLock(true)}>
                  <Lock className="mr-1.5 h-3.5 w-3.5" />
                  Lock account…
                </Button>
              )}
              {/* Delete is not here any more — it is the danger region at the
                  bottom of the page. It is irreversible and it sat one
                  tab-stop from a role picker and a lock, both of which are
                  routine and both of which undo. The app page puts its
                  equivalent behind a red heading at the end of a page for the
                  same reason, and two pages of the same shape should not carry
                  two different models of risk. */}
            </>
          )}
        </div>
      </div>

      {/* Says what the badge only names. An account in the queue looks like a
          working one on this page — it has a role, a joined date, an identity
          — and the one fact that matters about it is that none of that is in
          effect yet. */}
      {awaitingActivation && (
        <Notice
          tone="info"
          title="This account is waiting to be activated"
          live
        >
          Every sign-in is refused until you activate it with a role.
          {locked && " It is also locked, and activating it does not unlock it."}
        </Notice>
      )}

      {/* The second of the two places group ownership is introduced, and the
          only one where the product may propose the concept to somebody who has
          not used it. This is the moment the whole feature exists for: an
          administrator is leaving, they own an estate, and moving it to a team
          is the answer rather than N transfers. */}
      {!isMe && ownedApps > 0 && (
        <Notice
          tone="attention"
          title={`${user.login} owns ${ownedApps} app${ownedApps === 1 ? "" : "s"}`}
          action={
            <Button variant="secondary" size="sm" onClick={() => setMovingApps(true)}>
              Move them…
            </Button>
          }
        >
          The account can’t be deleted while it owns apps. Move{" "}
          {ownedApps === 1 ? "it" : "them"} to another person or a group.
        </Notice>
      )}

      <MoveAppsDialog
        key={user.id}
        user={user}
        count={ownedApps}
        open={movingApps}
        onOpenChange={setMovingApps}
      />

      <AccountFacts
        user={user}
        groupCount={data.groups.length}
        ownedApps={ownedApps}
        ownedViaGroup={ownedViaGroup}
      />

      {!isMe && (
        <Section title="Scanner account" id="scanner">
          <div className="p-4">
            <FieldRow
              label="Mark as a scanner"
              htmlFor="user-scanner"
              help="Can read Scan targets; its X-Toft-Scan requests aren’t counted as usage. Reaches only apps it’s granted."
            >
              <Switch
                id="user-scanner"
                checked={!!user.scanner_since}
                disabled={pending}
                onCheckedChange={(v) => void setScanner(v)}
              />
            </FieldRow>
          </div>
        </Section>
      )}

      <Section
        title="Apps"
        description="Change access on each app’s Access tab."
      >
        <AccessTable
          report={access}
          voice="other"
          error={accessError}
          onRetry={() => void refetchAccess()}
        />
      </Section>

      {/* What they may open, above; what they did open, here. */}
      <UserAppUsageSection userId={user.id} />

      <Section
        title="Groups"
        description="Adding or removing a group signs them out."
        // A pointer that is present and clickable on a populated list too, not
        // only in the empty state's last sentence. Membership for one person is
        // still edited from the group's side; this is the route to it.
        actions={
          <Link to="/people?tab=groups" className={buttonVariants({ variant: "outline", size: "sm" })}>
            Manage groups
          </Link>
        }
      >
        {data.groups.length === 0 ? (
          <EmptyState size="inline" title="Not a member of any group" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Group</TableHead>
                <TableHead className="w-[180px]">Added</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.groups.map((g) => (
                <TableRow key={g.id}>
                  <TableCell className="font-medium text-foreground">{g.name}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {g.added_at ? <RelativeTime iso={g.added_at} /> : "Unknown"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>

      {/* Whether they asked, when, and who decided — beside the `<Select>` that
          grants it. Absent on an install nobody has asked on. No approve or
          deny here: a deny needs its reason dialog, and a third copy of the
          decision is exactly the drift that produced this section. */}
      {data.promotion_requests?.length > 0 && (
        <Section
          title="Publisher access"
        >
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Asked</TableHead>
                <TableHead className="w-[110px]">State</TableHead>
                <TableHead className="hidden w-[140px] @md:table-cell">Decided by</TableHead>
                <TableHead>Reason</TableHead>
                <TableHead className="w-[90px] text-right">
                  <span className="sr-only">Open</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.promotion_requests.map((req) => (
                <TableRow key={req.id}>
                  <TableCell className="text-xs text-muted-foreground">
                    <RelativeTime iso={req.created_at} />
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={req.status} />
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @md:table-cell">
                    {req.approver_login || "—"}
                  </TableCell>
                  <TableCell className="text-xs whitespace-normal text-muted-foreground">
                    {req.reason || "—"}
                  </TableCell>
                  <TableCell className="text-right">
                    <Link
                      to={`/admin/publisher-requests?request=${req.id}`}
                      className="text-xs font-medium hover:underline"
                    >
                      Open
                    </Link>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Section>
      )}

      {data.identities.length > 0 && (
        <Section
          title="Sign-in"
          description={
            <>
              Remove a provider on{" "}
              <Link to="/admin/authentication" className="underline underline-offset-2 hover:text-foreground">
                Admin → Authentication
              </Link>
              .
            </>
          }
        >
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Provider</TableHead>
                <TableHead className="w-full">Account</TableHead>
                <TableHead className="hidden w-[150px] @xl:table-cell">First seen</TableHead>
                <TableHead className="w-[150px]">Last used</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.identities.map((i) => (
                <TableRow key={`${i.provider_id}-${i.login}`}>
                  {/* Its own width: a provider name is short, and `max-w-0`
                      with no width collapsed it to "Dex (lo…". */}
                  <TableCell className="font-medium text-foreground">
                    {i.provider_name}
                  </TableCell>
                  <TableCell className="max-w-0 truncate text-sm text-muted-foreground">
                    {i.name ? `${i.name} · ` : ""}{i.email || i.login || "—"}
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                    <RelativeTime iso={i.created_at} />
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {i.last_login_at ? <RelativeTime iso={i.last_login_at} /> : "Never"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Section>
      )}

      {/* Beneath Sign-in, because it answers the other half of the same
          question. Sign-in is how a person reaches this account; these are the
          credentials that reach it when nobody is present — and on an account
          whose clock reads "via API key", this section is where that sentence
          becomes something an administrator can act on. */}
      <Section
        title="API keys"
        description={
          <>
            Keys act as this account. Every key is on{" "}
            <Link to="/admin/api-keys" className="underline underline-offset-2 hover:text-foreground">
              Admin → API keys
            </Link>
            .
          </>
        }
      >
        {keysError ? (
          <div className="p-4">
            <QueryError
              title="Their API keys could not be read"
              error={keysError}
              fallback={ADMIN_ONLY_FALLBACK}
              onRetry={() => void refetchKeys()}
            />
          </div>
        ) : !keys ? (
          <div className="space-y-2 p-4">
            {[0, 1].map((i) => <Skeleton key={i} className="h-10 w-full" />)}
          </div>
        ) : keys.keys.length === 0 ? (
          <EmptyState size="inline" title="No personal API keys" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead className="hidden w-[140px] @xl:table-cell">Key</TableHead>
                <TableHead className="w-[110px]">State</TableHead>
                <TableHead className="hidden w-[100px] @2xl:table-cell">Scope</TableHead>
                <TableHead className="hidden w-[130px] @4xl:table-cell">Expires</TableHead>
                <TableHead className="hidden w-[130px] @md:table-cell">Last used</TableHead>
                <TableHead className="hidden w-[130px] @3xl:table-cell">From</TableHead>
                <TableHead className="hidden w-[110px] text-right @2xl:table-cell">
                  Calls ({keys.days}d)
                </TableHead>
                <TableHead className="w-12">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.keys.map((key) => (
                <Fragment key={key.id}>
                  <TableRow>
                    <TableCell className="font-medium text-foreground">
                      {/* The row expands rather than navigating: the usage is
                          about this key, and this page is already the place
                          the account is being judged from. */}
                      <button
                        type="button"
                        className="flex items-center gap-1.5 text-left hover:text-primary"
                        aria-expanded={expandedKey === key.id}
                        onClick={() => setExpandedKey(expandedKey === key.id ? null : key.id)}
                      >
                        <ChevronDown
                          className={`h-3.5 w-3.5 text-muted-foreground transition-transform ${
                            expandedKey === key.id ? "" : "-rotate-90"
                          }`}
                        />
                        {key.name}
                      </button>
                    </TableCell>
                    <TableCell className="hidden font-mono text-xs text-muted-foreground @xl:table-cell">
                      {key.key_prefix ? `${key.key_prefix}…` : "—"}
                    </TableCell>
                    <TableCell><KeyStateBadge state={key.state} /></TableCell>
                    <TableCell className="hidden @2xl:table-cell">
                      <KeyScopeBadge scope={key.scope} />
                    </TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground @4xl:table-cell">
                      {key.expires_at ? <RelativeTime iso={key.expires_at} /> : "Never"}
                    </TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground @md:table-cell">
                      {key.last_used_at ? <RelativeTime iso={key.last_used_at} /> : "Never"}
                    </TableCell>
                    <TableCell className="hidden font-mono text-xs text-muted-foreground @3xl:table-cell">
                      {key.last_used_ip || "—"}
                    </TableCell>
                    <TableCell className="hidden text-right text-xs text-muted-foreground @2xl:table-cell">
                      {key.requests.toLocaleString()}
                    </TableCell>
                    <TableCell>
                      {/* The record that shows the evidence now holds the act.
                          It used to expand a key into its usage chart and then
                          send an administrator to an install-wide table with a
                          search box no link can seed. Both mutations are
                          key-id scoped and already existed. */}
                      <DropdownMenu>
                        <DropdownMenuTrigger
                          render={
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              aria-label={`Actions for ${key.name}`}
                            >
                              <MoreHorizontal className="h-4 w-4" />
                            </Button>
                          }
                        />
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem
                            disabled={keyBusy === key.id || key.state === "expired"}
                            onClick={() => void toggleKeyDisabled(key)}
                          >
                            {key.state === "disabled" ? "Enable" : "Disable"}
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            variant="destructive"
                            disabled={keyBusy === key.id}
                            onClick={() => setKeyToRevoke(key)}
                          >
                            Revoke…
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                  {expandedKey === key.id && (
                    <TableRow className="hover:bg-transparent">
                      <TableCell colSpan={9} className="bg-muted/30 p-4">
                        <KeyUsagePanel
                          fetchUsage={(bucket, days) =>
                            api.getUserApiKeyUsage(id!, key.id, bucket, days)
                          }
                          queryKey={["user-api-key-usage", id!, key.id]}
                        />
                      </TableCell>
                    </TableRow>
                  )}
                </Fragment>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>

      {/* Revoke is irreversible but the blast radius is one named credential,
          so a plain confirm — the same dialog the install-wide table uses,
          because a second wording of the same act is how the two drift. */}
      <Dialog open={keyToRevoke !== null} onOpenChange={(o) => !o && setKeyToRevoke(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke “{keyToRevoke?.name}”?</DialogTitle>
            <DialogDescription>
              Anything using this key stops working immediately, and this can’t be undone. To pause
              it instead, disable it.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={keyBusy !== null} onClick={() => setKeyToRevoke(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={keyBusy !== null}
              onClick={() => keyToRevoke && void revokeKey(keyToRevoke)}
            >
              {keyBusy ? "Revoking…" : "Revoke key"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* The danger region, in the shape the app page uses: last on the page,
          named for what it is, and holding only what cannot be undone. */}
      {!isMe && (
        <div className="space-y-2">
          <SectionHeading title="Danger zone" tone="danger" />
          <TableCard className={`p-4 ${toneBorder.danger}`}>
            <div className="flex flex-wrap items-center justify-between gap-4">
              <div className="min-w-0">
                <p className="text-sm font-medium">Delete this account</p>
                <p className="text-xs text-muted-foreground">
                  {ownedApps > 0
                    ? `${user.login} owns ${ownedApps} app${ownedApps === 1 ? "" : "s"}. Transfer or delete ${ownedApps === 1 ? "it" : "them"} first.`
                    : `Removes ${user.login} and their grants and keys. The audit log keeps their name.`}
                </p>
              </div>
              <Button
                variant="destructive"
                size="sm"
                disabled={pending || ownedApps > 0 || !access}
                onClick={() => setConfirmDelete(true)}
              >
                <Trash2 className="mr-1.5 h-3.5 w-3.5" />
                Delete account…
              </Button>
            </div>
          </TableCard>
        </div>
      )}

      <MakeAdminDialog
        login={user.login}
        open={confirmPromote}
        pending={pending}
        onConfirm={() => void changeRole("system_admin", true)}
        onOpenChange={setConfirmPromote}
      />

      <LockUserDialog
        user={user}
        open={confirmLock}
        pending={pending}
        onOpenChange={setConfirmLock}
        onConfirm={() => setLocked(true)}
      />

      <DeleteUserDialog
        user={user}
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        onDeleted={() => {
          queryClient.invalidateQueries({ queryKey: ["users"] });
          navigate("/people");
        }}
      />
    </div>
  );
}

/** Locking is recoverable but it takes effect immediately and kills sessions,
 * so it states what survives before it happens. Unlocking needs no dialog. */
function LockUserDialog({
  user, open, pending, onOpenChange, onConfirm,
}: {
  user: User;
  open: boolean;
  pending: boolean;
  onOpenChange: (v: boolean) => void;
  onConfirm: () => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Lock {user.login}?</DialogTitle>
          <DialogDescription>
            They are signed out now and can’t sign in until you unlock them. Their apps, keys and
            grants stay.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={pending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="destructive" onClick={onConfirm} disabled={pending}>
            {pending ? "Locking…" : "Lock account"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Deleting an account is irreversible, so it is type-to-confirm and it says
 * exactly what survives. The button that opens it is disabled while the
 * account still owns apps — the API refuses that case anyway. */
function DeleteUserDialog({
  user, open, onOpenChange, onDeleted,
}: {
  user: User;
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onDeleted: () => void;
}) {
  const [confirmText, setConfirmText] = useState("");
  const [deleting, setDeleting] = useState(false);
  const [held, setHeld] = useState<HeldStorageMapping[] | null>(null);

  async function submit() {
    setDeleting(true);
    try {
      await api.deleteUser(user.id);
      toast.success(`Deleted “${user.login}”`);
      onOpenChange(false);
      onDeleted();
    } catch (err) {
      // The API 409s with the reason — apps still owned, the last admin, or
      // storage this account's delegations attach, which is listed in place.
      const mappings = heldStorageMappings(err);
      if (mappings) setHeld(mappings);
      toast.error(err instanceof Error ? err.message : "Couldn’t delete the account");
    } finally {
      setDeleting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) setConfirmText(""); onOpenChange(o); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete {user.login}?</DialogTitle>
          <DialogDescription>
            Removes the account, its sign-ins, groups and direct shares. The audit log stays. To
            only stop them signing in, lock the account instead. Type{" "}
            <ConfirmPhrase text={user.login} /> to confirm.
          </DialogDescription>
        </DialogHeader>
        <Input
          value={confirmText}
          onChange={(e) => setConfirmText(e.target.value)}
          placeholder={user.login}
          aria-label={`Type ${user.login} to confirm removing this account`}
          className="font-mono"
          autoFocus
        />
        {held && <HeldStorageNotice subject={user.login} mappings={held} />}
        <DialogFooter>
          <Button variant="outline" disabled={deleting} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="destructive"
            disabled={confirmText !== user.login || deleting}
            onClick={submit}
          >
            {deleting ? "Deleting…" : "Delete account"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Move every app somebody owns personally to one new owner.
 *
 * The offboarding action, and the second place group ownership is introduced.
 * It offers a group only when the install has one — nothing here prompts anyone
 * to create their first, and on an install with none this is a person picker
 * that never says the word.
 *
 * Not atomic across apps and it says so: each transfer is its own transaction,
 * because the alternative is one transaction holding a write lock on two
 * hundred apps and all their grant rows. A partial result is recoverable by
 * running it again, which the copy has to be honest about rather than implying
 * an all-or-nothing that is not there.
 */
function MoveAppsDialog({
  user, count, open, onOpenChange,
}: {
  user: User;
  count: number;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [target, setTarget] = useState<
    { kind: "user"; id: string; label: string } | { kind: "group"; id: string; label: string } | null
  >(null);
  const [moving, setMoving] = useState(false);
  /** Type-to-confirm, asking for the *container's* name. Transfer of ownership
   *  is the policy's top tier and the single-app path honours it; this one —
   *  strictly larger, an entire estate in one press — had no confirmation at
   *  all and an ordinary default button. The larger, less reversible act was
   *  the cheaper one, and an administrator reaches both on one afternoon. */
  const [typed, setTyped] = useState("");

  const q = search.trim();
  const searching = open && !target && q.length >= 2;

  const { data: groupsOnInstall = [] } = useQuery({
    queryKey: ["groups"],
    queryFn: api.listGroups,
    enabled: open,
    staleTime: 5 * 60_000,
  });
  const installHasGroups = groupsOnInstall.length > 0;

  const { data: people = [] } = useQuery({
    queryKey: ["directory-users", q],
    queryFn: () => api.searchDirectoryUsers(q),
    enabled: searching,
  });
  const { data: groups = [] } = useQuery({
    queryKey: ["directory-groups", q],
    queryFn: () => api.searchDirectoryGroups(q),
    enabled: searching && installHasGroups,
  });

  async function move() {
    if (!target) return;
    setMoving(true);
    try {
      const result = await api.transferUserApps(user.id, target.id, target.kind);
      queryClient.invalidateQueries({ queryKey: ["user", user.id] });
      queryClient.invalidateQueries({ queryKey: ["user-access", user.id] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["groups"] });
      toast.success(
        `Moved ${result.transferred} app${result.transferred === 1 ? "" : "s"} to ${target.label}`,
      );
      onOpenChange(false);
      setTarget(null);
      setSearch("");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t move the apps");
    } finally {
      setMoving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!moving) onOpenChange(o); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            Move {count} app{count === 1 ? "" : "s"} from {user.login}
          </DialogTitle>
          <DialogDescription>
            Apps {user.login} owns personally move to one new owner, and {user.login} stays on
            each as an editor. If one fails, run this again to finish the rest.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="move-search">New owner</Label>
            <Input
              id="move-search"
              placeholder={installHasGroups ? "Search people and groups…" : "Search by username…"}
              value={target ? target.label : search}
              onChange={(e) => { setTarget(null); setSearch(e.target.value); }}
              autoComplete="off"
              autoFocus
            />
          </div>

          {searching && (
            <div className="max-h-52 divide-y divide-border/60 overflow-y-auto rounded-md border border-border/60">
              {people.length === 0 && groups.length === 0 ? (
                <p className="p-2.5 text-xs text-muted-foreground">Nothing matches.</p>
              ) : (
                <>
                  {people
                    .filter((u) => u.id !== user.id)
                    .map((u) => (
                      <button
                        key={u.id}
                        type="button"
                        disabled={u.system_role === "viewer"}
                        className="flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:cursor-not-allowed disabled:opacity-50"
                        onClick={() => setTarget({ kind: "user", id: u.id, label: u.login })}
                      >
                        <span className="min-w-0 truncate font-medium">{u.login}</span>
                        <span className="shrink-0 text-xs text-muted-foreground">
                          {u.system_role === "viewer" ? "viewer — cannot own apps" : "person"}
                        </span>
                      </button>
                    ))}
                  {groups.map((g) => (
                    <button
                      key={g.id}
                      type="button"
                      disabled={g.eligible_members === 0}
                      className="flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:cursor-not-allowed disabled:opacity-50"
                      onClick={() => setTarget({ kind: "group", id: g.id, label: g.name })}
                    >
                      <span className="flex min-w-0 items-center gap-1.5">
                        <Users className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
                        <span className="truncate font-medium">{g.name}</span>
                      </span>
                      <span className="shrink-0 text-xs text-muted-foreground">
                        {g.eligible_members === 0
                          ? "nobody who could administer it"
                          : `${g.eligible_members} ${g.eligible_members === 1 ? "member" : "members"}`}
                      </span>
                    </button>
                  ))}
                </>
              )}
            </div>
          )}

          {target?.kind === "group" && (
            <p className="text-xs text-muted-foreground">
              Every member of {target.label} becomes an owner of all {count} app
              {count === 1 ? "" : "s"}.
            </p>
          )}
          <div className="space-y-1.5">
            <p className="text-sm font-medium">
              <Label htmlFor="move-confirm" className="inline">Type</Label>{" "}
              <ConfirmPhrase text={user.login} /> to confirm
            </p>
            <Input
              id="move-confirm"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              placeholder={user.login}
              autoComplete="off"
              className="font-mono"
            />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" disabled={moving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={!target || typed !== user.login || moving}
            onClick={move}
          >
            {moving ? "Moving…" : `Move ${count} app${count === 1 ? "" : "s"}`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Letting an account in, and choosing what it may do, in one act.
 *
 * The same two-step shape the People queue uses: pick, then press. A select
 * that committed on change would make Viewer — the value already showing —
 * unreachable without choosing something else first, and would turn a misclick
 * into somebody's access. Administrator is not offered because the API refuses
 * it; promoting is the role picker this control replaces, one activation later.
 */
function ActivateControl({
  pending,
  onActivate,
}: {
  pending: boolean;
  onActivate: (role: "publisher" | "viewer") => void;
}) {
  const [role, setRole] = useState<"publisher" | "viewer">("viewer");
  return (
    <>
      <Select value={role} onValueChange={(v) => setRole((v as "publisher" | "viewer") ?? "viewer")}>
        <SelectTrigger size="sm" disabled={pending} aria-label="Role to activate this account as">
          <SelectValue>{(v) => systemRoleLabel(v as string)}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="viewer">Viewer</SelectItem>
          <SelectItem value="publisher">Publisher</SelectItem>
        </SelectContent>
      </Select>
      <Button size="sm" disabled={pending} onClick={() => onActivate(role)}>
        {pending ? "Activating…" : "Activate account"}
      </Button>
    </>
  );
}

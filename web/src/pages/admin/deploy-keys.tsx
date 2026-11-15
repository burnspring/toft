import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { OnceOnlySecret } from "@/components/once-only-secret";
import { KeyRound, MoreHorizontal, Plus, UserX } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { TableCard } from "@/components/table-card";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { SearchInput } from "@/components/search-input";
import { FilterBar } from "@/components/filter-bar";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { KeyStateBadge } from "@/components/key-state-badge";
import { api } from "@/lib/api";
import type { DeployKey, DeployKeyScope } from "@/lib/api";
import { toneSurface } from "@/lib/tone";
import { Badge } from "@/components/ui/badge";
import { FieldHelp } from "@/components/field-help";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/**
 * Its own constant, not Admin → API keys' with a row appended.
 *
 * `Revoked` cannot occur there: a personal key's revoke hard-deletes the row,
 * while a deploy key's stamps `revoked_at` — because `deploy_key:{id}` has to
 * keep resolving to a name in the audit log. Two constants that differ for a
 * stated reason, so the difference is not later "fixed".
 */
const stateFilters = [
  { label: "All", value: "all" },
  { label: "Active", value: "active" },
  { label: "Disabled", value: "disabled" },
  { label: "Revoked", value: "revoked" },
  { label: "Expired", value: "expired" },
];

const EXPIRY_OPTIONS = [
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "1 year" },
  { value: "0", label: "Never" },
];

const expiryLabel = (v: string) => EXPIRY_OPTIONS.find((o) => o.value === v)?.label ?? "90 days";

/** The two scopes, and the sentence under each is the whole of the choice.
 *
 *  Deploy is first and is the default: it is what the class meant before there
 *  were two, and every key minted before this existed still has it. */
const SCOPE_OPTIONS: { value: DeployKeyScope; label: string; hint: string }[] = [
  {
    value: "deploy",
    label: "Deploy",
    hint: "Create, upload, deploy, roll back, restart and stop, and read everything below.",
  },
  {
    value: "read",
    label: "Read",
    hint: "List apps and groups, read deployment history and download a release. It cannot deploy anything.",
  },
];

const scopeLabel = (v: string) => SCOPE_OPTIONS.find((o) => o.value === v)?.label ?? "Deploy";

/** A scope is not a state, so it is not a `StatusBadge`: nothing is happening
 *  to a key because it is read-scoped. `neutral` is a recorded fact with no
 *  valence, which is exactly what this is — and `info` reads as the platform
 *  acting, which a scope somebody chose at mint is not. */
function ScopeBadge({ scope }: { scope: DeployKeyScope }) {
  return (
    <Badge variant="outline" className={toneSurface.neutral}>
      {scopeLabel(scope)}
    </Badge>
  );
}

/**
 * Admin → Deploy keys.
 *
 * The third credential class, and the page exists because the first two do not
 * cover the case: an app key is a grant on one app, a personal key *is* a
 * person and dies with them, and a promotion pipeline is neither. Before this,
 * the field fix was to share one administrator's personal token — which breaks
 * the day they leave and puts their name on every deploy in the audit log until
 * then.
 *
 * Mirrors Admin → API keys deliberately: same table shape, same state
 * vocabulary, same once-only disclosure, same search box and counted state
 * strip. The differences are the ones that matter and each is stated on the
 * page rather than left to be discovered — these keys belong to nobody,
 * "created by someone who has left" is a prompt to rotate rather than a reason
 * the key stopped working, and revoking one *stamps* the row rather than
 * deleting it, so this list has a Revoked state that the personal one cannot.
 */
export function AdminDeployKeysPage() {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState("90");
  const [scope, setScope] = useState<DeployKeyScope>("deploy");
  const [mayAttach, setMayAttach] = useState(false);
  const [minted, setMinted] = useState<{ name: string; secret: string; scope: DeployKeyScope } | null>(null);
  const [toRevoke, setToRevoke] = useState<DeployKey | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  // In the URL, as on Admin → API keys: a narrowed list is a link.
  const [search, setSearch] = useUrlParam("q");
  const [stateFilter, setStateFilter] = useUrlParam("state", "all");
  const [, setParams] = useSearchParams();
  // One write, not two setters: each reads the URL it was rendered with.
  const clearNarrowing = () =>
    setParams((prev) => {
      const out = new URLSearchParams(prev);
      out.delete("q");
      out.delete("state");
      return out;
    }, { replace: true });

  const { data: keys, isLoading, error, refetch } = useQuery({
    queryKey: ["deploy-keys"],
    queryFn: api.listDeployKeys,
  });

  async function create() {
    const trimmed = name.trim();
    if (!trimmed) return;
    setBusy("create");
    try {
      const result = await api.createDeployKey(trimmed, expiry === "0" ? 0 : Number(expiry), scope,
        scope === "deploy" && mayAttach);
      queryClient.invalidateQueries({ queryKey: ["deploy-keys"] });
      setCreating(false);
      setName("");
      setScope("deploy");
      setMayAttach(false);
      setExpiry("90");
      setMinted({ name: result.key.name, secret: result.secret, scope: result.key.scope });
      toast.success(`Deploy key “${result.key.name}” created`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t create “${trimmed}”`);
    } finally {
      setBusy(null);
    }
  }

  async function setDisabled(key: DeployKey, disabled: boolean) {
    setBusy(key.id);
    try {
      await api.setDeployKeyDisabled(key.id, disabled);
      queryClient.invalidateQueries({ queryKey: ["deploy-keys"] });
      toast.success(disabled ? `“${key.name}” disabled` : `“${key.name}” enabled`);
    } catch (err) {
      toast.error(
        err instanceof Error
          ? err.message
          : `Couldn’t ${disabled ? "disable" : "enable"} “${key.name}”`,
      );
    } finally {
      setBusy(null);
    }
  }

  // Reversible and audited, so no dialog: the toast says what is now true.
  async function setKeyMayAttach(key: DeployKey, on: boolean) {
    setBusy(key.id);
    try {
      await api.setDeployKeyMayAttach(key.id, on);
      queryClient.invalidateQueries({ queryKey: ["deploy-keys"] });
      toast.success(on ? `“${key.name}” may now attach storage` : `“${key.name}” no longer attaches storage`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t change “${key.name}”`);
    } finally {
      setBusy(null);
    }
  }

  async function revoke(key: DeployKey) {
    setBusy(key.id);
    try {
      await api.revokeDeployKey(key.id);
      queryClient.invalidateQueries({ queryKey: ["deploy-keys"] });
      setToRevoke(null);
      toast.success(`“${key.name}” revoked`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t revoke “${key.name}”`);
    } finally {
      setBusy(null);
    }
  }

  const all = keys ?? [];
  const q = search.trim().toLowerCase();
  const searched = q
    ? all.filter(
        (k) =>
          k.name.toLowerCase().includes(q) ||
          (k.key_prefix ?? "").toLowerCase().includes(q) ||
          (k.created_by_login ?? "").toLowerCase().includes(q),
      )
    : all;
  const rows = stateFilter === "all" ? searched : searched.filter((k) => k.state === stateFilter);
  // From every key, not the narrowed rows: a search or a state tab must not
  // make the warning vanish or change its count.
  const orphaned = all.filter((k) => k.minter_left && k.state === "active");

  return (
    <div>
      <PageHeader
        title="Deploy keys"
        description="Credentials for a build pipeline. A deploy key belongs to nobody, so it outlives whoever created it."
        actions={
          <Button size="sm" onClick={() => setCreating(true)}>
            <Plus className="mr-1.5 h-4 w-4" />
            New deploy key
          </Button>
        }
      />

      {orphaned.length > 0 && (
        <Notice tone="attention" title="Created by someone who has left" className="mb-6">
          {orphaned.length === 1
            ? `“${orphaned[0].name}” was created by an account that no longer exists.`
            : `${orphaned.length} active keys were created by accounts that no longer exist.`}{" "}
          They still work. Rotate them when you can.
        </Notice>
      )}

      {/* Revocation here is a soft stamp, so a revoked key stays in this table
          for the life of the install — and the page offered no way to narrow
          it, so the pipeline credentials that still work were interleaved with
          every one ever retired. The confirmation policy's fourth clause: a new
          row state gets its matching list filter in the same change. */}
      {!error && !isLoading && all.length > 0 && (
        <>
          <div className="mb-3">
            <SearchInput
              value={search}
              onChange={setSearch}
              placeholder="Search by name, prefix or creator…"
              className="w-full sm:max-w-sm"
            />
          </div>
          <div className="mb-4">
            <FilterBar
              label="Filter keys by state"
              options={stateFilters.map((f) => ({
                ...f,
                label: `${f.label} (${
                  f.value === "all"
                    ? searched.length
                    : searched.filter((k) => k.state === f.value).length
                })`,
              }))}
              value={stateFilter}
              onChange={setStateFilter}
            />
          </div>
        </>
      )}

      {error ? (
        <QueryError
          title="Deploy keys could not be read"
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
      ) : rows.length === 0 && all.length > 0 ? (
        <EmptyState
          icon={<KeyRound className="h-6 w-6" />}
          title="No deploy keys match"
          action={
            <Button variant="outline" size="sm" onClick={clearNarrowing}>
              Clear
            </Button>
          }
        />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<KeyRound className="h-6 w-6" />}
          title="No deploy keys"
          description="A deploy key lets a build pipeline create and deploy apps without acting as a person."
          action={
            <Button size="sm" onClick={() => setCreating(true)}>
              <Plus className="mr-1.5 h-4 w-4" />
              New deploy key
            </Button>
          }
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">Name</TableHead>
                <TableHead className="hidden @xl:table-cell">Key</TableHead>
                <TableHead>Scope</TableHead>
                <TableHead>State</TableHead>
                <TableHead className="hidden @3xl:table-cell">Created by</TableHead>
                <TableHead className="hidden @xl:table-cell">Last used</TableHead>
                <TableHead className="hidden @3xl:table-cell">Expires</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="max-w-0 truncate font-medium">{k.name}</TableCell>
                  <TableCell className="hidden font-mono text-xs text-muted-foreground @xl:table-cell">
                    {k.key_prefix}…
                  </TableCell>
                  <TableCell>
                    <span className="inline-flex flex-wrap items-center gap-1">
                      <ScopeBadge scope={k.scope} />
                      {k.may_attach && (
                        <Badge variant="outline" className="text-2xs">Attaches storage</Badge>
                      )}
                    </span>
                  </TableCell>
                  <TableCell><KeyStateBadge state={k.state} /></TableCell>
                  <TableCell className="hidden @3xl:table-cell">
                    {k.minter_left ? (
                      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
                        <UserX className="h-3.5 w-3.5" aria-hidden />
                        Deleted account
                      </span>
                    ) : (
                      <span className="text-sm">{k.created_by_login || "—"}</span>
                    )}
                  </TableCell>
                  <TableCell className="hidden @xl:table-cell text-sm text-muted-foreground">
                    {k.last_used_at ? <RelativeTime iso={k.last_used_at} /> : "Never"}
                  </TableCell>
                  <TableCell className="hidden @3xl:table-cell text-sm text-muted-foreground">
                    {k.expires_at ? <RelativeTime iso={k.expires_at} /> : "Never"}
                  </TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={
                          <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${k.name}`}>
                            <MoreHorizontal className="h-4 w-4" />
                          </Button>
                        }
                      />
                      <DropdownMenuContent align="end">
                        {k.state !== "revoked" && (
                          <DropdownMenuItem
                            disabled={busy === k.id}
                            onClick={() => setDisabled(k, k.state !== "disabled")}
                          >
                            {k.state === "disabled" ? "Enable" : "Disable"}
                          </DropdownMenuItem>
                        )}
                        {k.state !== "revoked" && k.scope === "deploy" && (
                          <DropdownMenuItem
                            disabled={busy === k.id}
                            onClick={() => setKeyMayAttach(k, !k.may_attach)}
                          >
                            {k.may_attach ? "Stop attaching storage" : "Allow attaching storage"}
                          </DropdownMenuItem>
                        )}
                        <DropdownMenuItem
                          variant="destructive"
                          disabled={busy === k.id || k.state === "revoked"}
                          onClick={() => setToRevoke(k)}
                        >
                          Revoke…
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

      {/* Create */}
      <Dialog open={creating} onOpenChange={(o) => { if (busy !== "create") setCreating(o); }}>
        <DialogContent>
          <form
            className="contents"
            onSubmit={(e) => {
              e.preventDefault();
              if (busy !== "create") void create();
            }}
          >
          <DialogHeader>
            <DialogTitle>New deploy key</DialogTitle>
            <DialogDescription>
              Name it after the pipeline that will use it.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="dk-name">Name</Label>
              <Input
                id="dk-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="nightly promotion pipeline"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="dk-scope">Scope</Label>
              <Select value={scope} onValueChange={(v) => setScope(v as DeployKeyScope)}>
                <SelectTrigger id="dk-scope">
                  <SelectValue>{(v) => scopeLabel(v as string)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {SCOPE_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>
                {SCOPE_OPTIONS.find((o) => o.value === scope)?.hint} Fixed once created — to change
                it, create another key and revoke this one.
              </FieldHelp>
            </div>
            {scope === "deploy" && (
              <div className="space-y-1.5">
                <div className="flex items-center justify-between gap-4">
                  <Label htmlFor="dk-may-attach">May attach storage</Label>
                  <Switch id="dk-may-attach" checked={mayAttach} onCheckedChange={(v) => setMayAttach(v)} />
                </div>
                <FieldHelp>
                  Its deploys may add storage an app’s toft.toml declares, when the app’s owner can attach it.
                </FieldHelp>
              </div>
            )}
            <div className="space-y-1.5">
              <Label htmlFor="dk-expiry">Expires</Label>
              <Select value={expiry} onValueChange={(v) => setExpiry(v as string)}>
                <SelectTrigger id="dk-expiry">
                  <SelectValue>{(v) => expiryLabel(v as string)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {EXPIRY_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={busy === "create"}
              onClick={() => setCreating(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!name.trim() || busy === "create"}>
              {busy === "create" ? "Creating…" : "Create key"}
            </Button>
          </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* The secret, once — and Escape no longer throws it away. */}
      {minted && (
        <OnceOnlySecret
          open
          title={`Copy the key for “${minted.name}” now`}
          secret={minted.secret}
          label="The new deploy key"
          onDismiss={() => setMinted(null)}
        >
          <FieldHelp>
            Give it to the pipeline as <code className="font-mono">TOFT_TOKEN</code>.{" "}
            {minted.scope === "read" ? (
              <>
                It can read apps and download releases, but can’t deploy.
              </>
            ) : (
              <>
                Every app it creates must name its owning group as{" "}
                <code className="font-mono">owner_group_id</code>. Find group IDs on{" "}
                <Link to="/people?tab=groups" className="underline underline-offset-2 hover:text-foreground">
                  People → Groups
                </Link>
                .
              </>
            )}
          </FieldHelp>
        </OnceOnlySecret>
      )}
      {/* Revoke — irreversible, so it is confirmed and says so. */}
      <Dialog open={!!toRevoke} onOpenChange={(o) => { if (!o) setToRevoke(null); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke “{toRevoke?.name}”?</DialogTitle>
            <DialogDescription>
              It stops working immediately and can’t be re-enabled. Anything deploying with it will
              fail.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={busy === toRevoke?.id}
              onClick={() => setToRevoke(null)}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={busy === toRevoke?.id}
              onClick={() => toRevoke && revoke(toRevoke)}
            >
              {busy === toRevoke?.id ? "Revoking…" : "Revoke key"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

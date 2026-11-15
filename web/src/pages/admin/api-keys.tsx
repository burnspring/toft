import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, MoreHorizontal } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { TableCard } from "@/components/table-card";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { SearchInput } from "@/components/search-input";
import { FilterBar } from "@/components/filter-bar";
import { RelativeTime } from "@/components/relative-time";
import { KeyStateBadge } from "@/components/key-state-badge";
import { KeyScopeBadge } from "@/components/key-scope-badge";
import { PersonalKeysSwitch } from "@/components/personal-keys-switch";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { api } from "@/lib/api";
import type { UserApiKey } from "@/lib/api";

const stateFilters = [
  { label: "All", value: "all" },
  { label: "Active", value: "active" },
  { label: "Disabled", value: "disabled" },
  { label: "Expired", value: "expired" },
];

/**
 * Every personal API key on the install.
 *
 * This is an incident screen. When someone reports a leaked token, the
 * questions are "whose is it, what has it been doing, and how do I stop it" —
 * so the table leads with the owner and sorts by most recently used, and both
 * actions are one menu away. It deliberately keeps working when personal keys
 * are switched off install-wide, because that switch is often the first thing
 * an administrator reaches for and the keys still need finding afterwards.
 */
/** One sentence about this page, said the same way on every branch.
 *  Each of these files hand-wrote a second, shorter one for its error and
 *  loading paths, so the page described itself one way to whoever loaded it
 *  and another to whoever was refused. */
const PAGE_DESCRIPTION =
  "Every personal API key on this install. Disabling is instant and reversible; revoking isn’t.";

export function AdminApiKeysPage() {
  const queryClient = useQueryClient();
  // In the URL, because this is the incident screen: "the disabled keys" is a
  // link somebody pastes into a channel, not a set of buttons to describe.
  const [search, setSearch] = useUrlParam("q");
  const [stateFilter, setStateFilter] = useUrlParam("state", "all");
  const [, setParams] = useSearchParams();
  // One write, not two setters: each reads the URL it was rendered with, so
  // clearing the search and then the state left the search behind.
  const clearNarrowing = () =>
    setParams((prev) => {
      const out = new URLSearchParams(prev);
      out.delete("q");
      out.delete("state");
      return out;
    }, { replace: true });
  const [toRevoke, setToRevoke] = useState<UserApiKey | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["all-api-keys"],
    queryFn: () => api.listAllApiKeys(30),
  });
  // `/admin/api-keys#personal-keys` from ⌘K, and from the redirect that used
  // to be `/admin/settings#access`.
  useHashScroll();

  const keys = data?.keys ?? [];
  const window = data?.days ?? 30;
  const q = search.trim().toLowerCase();
  // Split so the tab strip's counts can be taken from the rows the *search*
  // has already narrowed to. State is left out because the tabs are what
  // select it — the same shape the apps list uses.
  const searched = keys.filter((k) => {
    if (!q) return true;
    return (
      k.name.toLowerCase().includes(q) ||
      (k.owner_login ?? "").toLowerCase().includes(q) ||
      k.key_prefix.toLowerCase().includes(q)
    );
  });
  const shown =
    stateFilter === "all" ? searched : searched.filter((k) => k.state === stateFilter);

  function invalidate() {
    queryClient.invalidateQueries({ queryKey: ["all-api-keys"] });
    queryClient.invalidateQueries({ queryKey: ["my-api-keys"] });
    // The account record lists the same keys. Revoking one here and leaving
    // it on the owner's page is how an administrator comes to believe a
    // credential is still live.
    queryClient.invalidateQueries({ queryKey: ["user-api-keys"] });
  }

  async function toggleDisabled(key: UserApiKey) {
    const disabling = key.state !== "disabled";
    setBusy(key.id);
    try {
      await api.adminUpdateApiKey(key.id, { disabled: disabling });
      toast.success(
        disabling ? `“${key.name}” disabled` : `“${key.name}” enabled`
      );
      invalidate();
    } catch (err) {
      toast.error(
        err instanceof Error
          ? err.message
          : `Couldn’t ${disabling ? "disable" : "enable"} “${key.name}”`,
      );
    } finally {
      setBusy(null);
    }
  }

  async function revoke(key: UserApiKey) {
    setBusy(key.id);
    try {
      await api.adminDeleteApiKey(key.id);
      toast.success(`“${key.name}” revoked`);
      setToRevoke(null);
      invalidate();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t revoke “${key.name}”`);
    } finally {
      setBusy(null);
    }
  }

  if (error) {
    return (
      <div>
        <PageHeader title="API keys" description={PAGE_DESCRIPTION} />
        <QueryError
          title="API keys could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="API keys"
        description={PAGE_DESCRIPTION}
      />

      {/* The switch that governs every row below it, on the page that shows
          what it governs (ADR §50). Throwing it stops all of them *now*, and
          the list of what stopped is what you should be looking at. */}
      <PersonalKeysSwitch />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder="Search by owner, name or prefix…"
          className="w-full sm:max-w-sm"
        />
      </div>

      {/* The primary axis: a counted tab strip, like every other list's. The
          counts are over the rows the search has already narrowed to, or
          "Disabled (4)" would sit above a table of one. */}
      <div className="mb-4">
        <FilterBar
          label="Filter keys by state"
          options={stateFilters.map((f) => ({
            ...f,
            label: `${f.label} (${f.value === "all" ? searched.length : searched.filter((k) => k.state === f.value).length})`,
          }))}
          value={stateFilter}
          onChange={setStateFilter}
        />
      </div>

      {isLoading ? (
        <TableCard>
          <div className="space-y-2 p-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        </TableCard>
      ) : keys.length === 0 ? (
        <EmptyState
          icon={<KeyRound className="h-6 w-6" />}
          title="No API keys"
          description="People create their own under Credentials, while the switch above is on."
        />
      ) : shown.length === 0 ? (
        <EmptyState
          icon={<KeyRound className="h-6 w-6" />}
          title="No API keys match"
          action={
            <Button variant="outline" size="sm" onClick={clearNarrowing}>
              Clear
            </Button>
          }
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              {/* A column plan, which this table had none of while its mirror
                  — the deploy keys table — has had one all along. Owner, name
                  and state stay: who holds a credential and whether it works
                  is the whole question. The prefix, the last address and the
                  call count yield first; "last used" holds on longest of the
                  three, because a key nobody has used is the one being looked
                  for. Nothing hidden here is the only route to itself — the
                  prefix and the address are both repeated under the name. */}
              <TableRow className="hover:bg-transparent">
                <TableHead>Owner</TableHead>
                <TableHead>Name</TableHead>
                <TableHead className="hidden @3xl:table-cell">Key</TableHead>
                <TableHead>State</TableHead>
                {/* What the credential may do, beside whether it still works.
                    On an incident screen that ranks above where it was last
                    used and how much traffic it has made: "which of these
                    could delete an app" is the question this page exists for,
                    and before the scope existed the answer was "all of them". */}
                <TableHead className="hidden @2xl:table-cell">Scope</TableHead>
                <TableHead className="hidden @xl:table-cell">Last used</TableHead>
                <TableHead className="hidden @4xl:table-cell">From</TableHead>
                <TableHead className="hidden text-right @md:table-cell">Calls ({window}d)</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((key) => (
                <TableRow key={key.id}>
                  <TableCell className="font-medium">
                    {key.owner_login ? (
                      <Link to={`/people/${key.user_id}`} className="hover:text-primary">
                        {key.owner_login}
                      </Link>
                    ) : (
                      <span className="text-muted-foreground">Deleted account</span>
                    )}
                  </TableCell>
                  <TableCell className="text-sm">
                    {key.name}
                    {/* What the hidden columns held, at the widths that hide
                        them — each restatement on its own column's container
                        tier, because a column hides on the card's width and a
                        `lg:` hides on the browser's. Scope had no restatement
                        at all, so what a leaked key may do was unreadable
                        below `@2xl` on the page that exists to answer it. */}
                    <span className="mt-0.5 flex flex-wrap gap-x-2 font-mono text-2xs text-muted-foreground @4xl:hidden">
                      {key.key_prefix && <span className="@3xl:hidden">{key.key_prefix}…</span>}
                      {key.scope === "read" && (
                        <span className="font-sans @2xl:hidden">read-scoped</span>
                      )}
                      {key.last_used_ip && <span className="@4xl:hidden">{key.last_used_ip}</span>}
                    </span>
                  </TableCell>
                  <TableCell className="hidden font-mono text-xs text-muted-foreground @3xl:table-cell">
                    {key.key_prefix ? `${key.key_prefix}…` : "—"}
                  </TableCell>
                  <TableCell>
                    <KeyStateBadge state={key.state} />
                  </TableCell>
                  <TableCell className="hidden @2xl:table-cell">
                    <KeyScopeBadge scope={key.scope} />
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">
                    {key.last_used_at ? <RelativeTime iso={key.last_used_at} /> : "Never"}
                  </TableCell>
                  <TableCell className="hidden font-mono text-xs text-muted-foreground @4xl:table-cell">
                    {key.last_used_ip || "—"}
                  </TableCell>
                  <TableCell className="hidden text-right text-xs text-muted-foreground @md:table-cell">
                    {key.requests.toLocaleString()}
                  </TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={
                          <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${key.name}`}>
                            <MoreHorizontal className="h-4 w-4" />
                          </Button>
                        }
                      />
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem
                          disabled={busy === key.id || key.state === "expired"}
                          onClick={() => toggleDisabled(key)}
                        >
                          {key.state === "disabled" ? "Enable" : "Disable"}
                        </DropdownMenuItem>
                        <DropdownMenuItem variant="destructive"
                          disabled={busy === key.id}
                          onClick={() => setToRevoke(key)}
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

      <Dialog open={toRevoke !== null} onOpenChange={(o) => !o && setToRevoke(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke “{toRevoke?.name}”?</DialogTitle>
            <DialogDescription>
              This is {toRevoke?.owner_login ?? "another person"}’s key. Anything using it stops
              working immediately, and it can’t be recovered. Disabling it can be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={busy !== null} onClick={() => setToRevoke(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={busy !== null}
              onClick={() => toRevoke && revoke(toRevoke)}
            >
              {busy ? "Revoking…" : "Revoke key"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

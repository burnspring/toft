import { Fragment, useState } from "react";
import { Disclosure } from "@/components/disclosure";
import { Code } from "@/components/code";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, MoreHorizontal, ChevronDown } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
import { SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { FilterBar } from "@/components/filter-bar";
import { RelativeTime } from "@/components/relative-time";
import { KeyStateBadge } from "@/components/key-state-badge";
import { KeyScopeBadge } from "@/components/key-scope-badge";
import { KeyUsagePanel } from "@/components/key-usage-panel";
import { api, statusOf } from "@/lib/api";
import type { UserApiKey, UserKeyScope } from "@/lib/api";
import { OnceOnlySecret } from "@/components/once-only-secret";
import { FieldHelp } from "@/components/field-help";
import { useUrlParam } from "@/hooks/use-list-state";

/** Expiry choices. "Never" is offered but not the default: most automation
 * outlives the person who remembers minting it. */
const EXPIRY_CHOICES = [
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "1 year" },
  { value: "0", label: "Never" },
];

/** The choices an install's cap leaves (`api_key_max_lifetime_days`; 0 is
 *  none). Never and anything longer go, and the cap itself is offered as the
 *  longest, so the form cannot ask for a key the server would refuse. */
function expiryChoices(maxDays: number) {
  if (maxDays <= 0) return EXPIRY_CHOICES;
  const within = EXPIRY_CHOICES.filter((c) => c.value !== "0" && Number(c.value) <= maxDays);
  return within.some((c) => Number(c.value) === maxDays)
    ? within
    : [...within, { value: String(maxDays), label: `${maxDays} days` }];
}

/** Ninety days where the cap allows it, otherwise the longest it does. */
function defaultExpiry(maxDays: number) {
  return maxDays <= 0 || maxDays >= 90 ? "90" : expiryChoices(maxDays).at(-1)!.value;
}

const stateFilters = [
  { label: "All", value: "all" },
  { label: "Active", value: "active" },
  { label: "Disabled", value: "disabled" },
  { label: "Expired", value: "expired" },
];

/** The two scopes. Full is first and is the default: it is what a key meant
 *  before there were two, and every key created before this has it.
 *
 *  The read hint is the sentence this feature is worse than nothing without.
 *  A key that changes nothing reads as "safe to leak", and it is not — a leaked
 *  read key is a full copy of everything its owner can see, source archives and
 *  logs included. The same sentence is in the admin guide and the user guide. */
const SCOPE_CHOICES: { value: UserKeyScope; label: string; hint: string }[] = [
  {
    value: "full",
    label: "Full",
    hint: "Everything you can do, except manage keys and access.",
  },
  {
    value: "read",
    label: "Read",
    hint: "Changes nothing in Toft, but reads all you can, source and logs included.",
  },
];

const scopeLabel = (v: string) => SCOPE_CHOICES.find((c) => c.value === v)?.label ?? "Full";

/**
 * The Credentials tab — what acts as you when you are not here.
 *
 * This was `/account/api-keys`, a page of its own, and it is unchanged in
 * behaviour: the same list, filters, create dialog, once-only reveal, usage
 * panel and revoke confirmation. What it gains by being here is context — it
 * sits beside the account the keys act as, and beside the sign-in providers
 * that answer the other half of the same question.
 *
 * The tab itself is absent when personal keys are switched off install-wide;
 * `MePage` decides that from this same query, so an install with the capability
 * off shows no empty table and no explanation of a feature it does not have.
 */
export function MeCredentialsTab() {
  const queryClient = useQueryClient();
  const [stateFilter, setStateFilter] = useUrlParam("state", "all");
  const [creating, setCreating] = useState(false);
  const [toRevoke, setToRevoke] = useState<UserApiKey | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  // The freshly minted secret, held until dismissed. There is no second chance
  // to read it — only its SHA-256 was stored.
  const [minted, setMinted] = useState<{ name: string; secret: string; scope: UserKeyScope } | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["my-api-keys"],
    queryFn: api.listMyApiKeys,
  });

  const keys = data?.keys ?? [];
  const window = data?.days ?? 30;
  const shown = stateFilter === "all" ? keys : keys.filter((k) => k.state === stateFilter);

  function invalidate() {
    queryClient.invalidateQueries({ queryKey: ["my-api-keys"] });
    // An administrator's inventory shows the same rows.
    queryClient.invalidateQueries({ queryKey: ["all-api-keys"] });
  }

  // Reversible, so it acts immediately and reports afterwards — no dialog.
  async function toggleDisabled(key: UserApiKey) {
    const disabling = key.state !== "disabled";
    setBusy(key.id);
    try {
      await api.updateMyApiKey(key.id, { disabled: disabling });
      toast.success(
        disabling
          ? `“${key.name}” disabled — calls using it now fail`
          : `“${key.name}” enabled`
      );
      invalidate();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t change “${key.name}”`);
    } finally {
      setBusy(null);
    }
  }

  async function revoke(key: UserApiKey) {
    setBusy(key.id);
    try {
      await api.deleteMyApiKey(key.id);
      toast.success(`“${key.name}” revoked`);
      setToRevoke(null);
      invalidate();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t revoke “${key.name}”`);
    } finally {
      setBusy(null);
    }
  }

  // **404 only.** The capability being off makes these routes absent rather
  // than refused, so there is nothing to retry and this is the honest answer.
  // Every *other* failure is a failure, and answering a 500 or a dropped
  // connection with a policy statement about the install told a person to go
  // and ask an administrator for something they already have. In practice
  // MePage has dropped the tab on the 404 before this renders.
  if (error && statusOf(error) === 404) {
    return (
      <EmptyState
        icon={<KeyRound className="h-6 w-6" />}
        title="API keys are not available on this install"
        description="An administrator can turn them on under Admin → API keys."
      />
    );
  }
  if (error) {
    return (
      <QueryError
        title="Your API keys could not be read"
        error={error}
        fallback="Reload the page. If it keeps failing, tell an administrator."
        onRetry={() => void refetch()}
      />
    );
  }

  return (
    <div className="space-y-4">
      <SectionHeading
        title="API keys"
        description="Let a script act as you, without a browser."
        // Not while there are none: the empty state carries the one button.
        actions={
          keys.length > 0 ? (
            <Button size="sm" onClick={() => setCreating(true)}>
              <Plus className="mr-1.5 h-4 w-4" />
              New key
            </Button>
          ) : undefined
        }
      />

      {/* A dialog rather than a banner: this tab's state goes with the tab,
          and a banner holding a once-only secret was lost to the person
          clicking Security to check something. */}
      {minted && (
        <OnceOnlySecret
          open
          title={`Copy the key for “${minted.name}” now`}
          secret={minted.secret}
          label="Your new API key"
          onDismiss={() => setMinted(null)}
        >
          <p className="text-xs text-muted-foreground">
            {minted.scope === "read"
              ? "It can read anything you can read until you revoke it."
              : "It acts as you until you revoke it."}
          </p>
        </OnceOnlySecret>
      )}

      {/* The primary axis, and therefore a counted tab strip rather than a
          chip row: "Expired (3)" is the answer somebody opens this tab for,
          and a filter that cannot tell you how many are on the other side of
          it is a filter you have to click to read. */}
      {keys.length > 0 && (
        <div className="mb-4">
          <FilterBar
            label="Filter keys by state"
            options={stateFilters.map((f) => ({
              ...f,
              label: `${f.label} (${f.value === "all" ? keys.length : keys.filter((k) => k.state === f.value).length})`,
            }))}
            value={stateFilter}
            onChange={setStateFilter}
          />
        </div>
      )}

      {isLoading ? (
        <TableCard>
          <div className="space-y-2 p-4">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        </TableCard>
      ) : keys.length === 0 ? (
        // On a card, not bare on the page grey.
        <TableCard>
          <EmptyState
            icon={<KeyRound className="h-6 w-6" />}
            title="No API keys yet"
            description="Create one for a script or pipeline. You’ll see it once."
            action={
              <Button size="sm" onClick={() => setCreating(true)}>
                <Plus className="mr-1.5 h-4 w-4" />
                New key
              </Button>
            }
          />
        </TableCard>
      ) : shown.length === 0 ? (
        <TableCard>
          <EmptyState
            icon={<KeyRound className="h-6 w-6" />}
            title="No keys match"
            description={`None of your ${keys.length} key${keys.length === 1 ? "" : "s"} match this filter.`}
            action={
              <Button variant="secondary" size="sm" onClick={() => setStateFilter("all")}>
                Clear
              </Button>
            }
          />
        </TableCard>
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                {/* Name and state stay: which key, and whether it still works.
                    The prefix, the dates and the count yield, and each of them
                    is repeated under the name at the width that drops it —
                    which the row's own expansion would not cover, because a
                    key that has never been used has nothing to expand. */}
                <TableHead>Name</TableHead>
                <TableHead className="hidden @3xl:table-cell">Key</TableHead>
                <TableHead>State</TableHead>
                <TableHead className="hidden @2xl:table-cell">Scope</TableHead>
                <TableHead className="hidden @4xl:table-cell">Expires</TableHead>
                <TableHead className="hidden @xl:table-cell">Last used</TableHead>
                <TableHead className="hidden text-right @md:table-cell">Calls ({window}d)</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((key) => (
                <Fragment key={key.id}>
                  <TableRow>
                    <TableCell className="font-medium">
                      <button
                        className="flex items-center gap-1.5 text-left hover:text-primary"
                        onClick={() => setExpanded(expanded === key.id ? null : key.id)}
                        aria-expanded={expanded === key.id}
                      >
                        <ChevronDown
                          className={`h-3.5 w-3.5 text-muted-foreground transition-transform ${
                            expanded === key.id ? "" : "-rotate-90"
                          }`}
                        />
                        {key.name}
                      </button>
                      {/* Each restatement hides at its own column's tier, on
                          the card's width like the column. Read on the
                          viewport, the ladders crossed: the prefix was printed
                          twice between 1024px and the card's `@3xl`, and the
                          expiry — whose column is `@4xl`, wider than this tab
                          ever gets — was the only one with no bound at all. */}
                      <span className="mt-0.5 flex flex-wrap gap-x-2 text-2xs text-muted-foreground @4xl:hidden">
                        {key.key_prefix && (
                          <span className="font-mono @3xl:hidden">{key.key_prefix}…</span>
                        )}
                        <span className="@4xl:hidden">
                          {key.expires_at ? (
                            <>
                              expires <RelativeTime iso={key.expires_at} />
                            </>
                          ) : (
                            "never expires"
                          )}
                        </span>
                        <span className="@xl:hidden">
                          {key.last_used_at ? (
                            <>
                              used <RelativeTime iso={key.last_used_at} />
                            </>
                          ) : (
                            "never used"
                          )}
                        </span>
                        {key.scope === "read" && <span className="@2xl:hidden">read-scoped</span>}
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
                    <TableCell className="hidden text-xs text-muted-foreground @4xl:table-cell">
                      {key.expires_at ? <RelativeTime iso={key.expires_at} /> : "Never"}
                    </TableCell>
                    <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">
                      {key.last_used_at ? <RelativeTime iso={key.last_used_at} /> : "Never"}
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
                  {expanded === key.id && (
                    <TableRow className="hover:bg-transparent">
                      <TableCell colSpan={8} className="bg-muted/30 p-4">
                        <KeyUsagePanel
                          fetchUsage={(bucket, days) => api.getMyApiKeyUsage(key.id, bucket, days)}
                          queryKey={["my-api-key-usage", key.id]}
                        />
                      </TableCell>
                    </TableRow>
                  )}
                </Fragment>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      <UsingKeysHelp />

      <CreateKeyDialog
        maxDays={data?.max_lifetime_days ?? 0}
        open={creating}
        onOpenChange={setCreating}
        onCreated={(name, secret, scope) => {
          setMinted({ name, secret, scope });
          setCreating(false);
          invalidate();
        }}
      />

      <Dialog open={toRevoke !== null} onOpenChange={(o) => !o && setToRevoke(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke “{toRevoke?.name}”?</DialogTitle>
            <DialogDescription>
              Anything using this key stops working immediately, and this can’t be undone. To pause
              it instead, disable it.
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

function CreateKeyDialog({
  maxDays,
  open,
  onOpenChange,
  onCreated,
}: {
  maxDays: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (name: string, secret: string, scope: UserKeyScope) => void;
}) {
  const [name, setName] = useState("");
  const choices = expiryChoices(maxDays);
  const [expiry, setExpiry] = useState(() => defaultExpiry(maxDays));
  const [scope, setScope] = useState<UserKeyScope>("full");
  const [saving, setSaving] = useState(false);

  async function submit() {
    const trimmed = name.trim();
    if (!trimmed) return;
    setSaving(true);
    try {
      const { secret } = await api.createMyApiKey(trimmed, Number(expiry), scope);
      toast.success(`Key “${trimmed}” created`);
      setName("");
      setExpiry(defaultExpiry(maxDays));
      setScope("full");
      onCreated(trimmed, secret, scope);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t create the key");
    } finally {
      setSaving(false);
    }
  }

  return (
    // Keyed so reopening the dialog starts from the defaults rather than from
    // whatever the last attempt left behind.
    <Dialog key={open ? "open" : "closed"} open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>New API key</DialogTitle>
          <DialogDescription>
            You’ll see the key once, right after you create it.
          </DialogDescription>
        </DialogHeader>
        {/* Bounded: three labelled selects, a paragraph under each and a
            conditional warning under the last. Unbounded on a short window
            that ran past the viewport, and `DialogContent` is
            `position: fixed`, so Create key was unreachable. */}
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving) void submit();
          }}
        >
        <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="key-name">Name</Label>
              <Input
                id="key-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. release pipeline"
              />
              <FieldHelp>
                Name it after where it will be used.
              </FieldHelp>
            </div>
            <div className="space-y-2">
              <Label htmlFor="key-scope">Scope</Label>
              <Select value={scope} onValueChange={(v) => setScope((v as UserKeyScope) ?? "full")}>
                <SelectTrigger id="key-scope">
                  <SelectValue>{(v) => scopeLabel(v as string)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {SCOPE_CHOICES.map((c) => (
                    <SelectItem key={c.value} value={c.value}>
                      {c.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>
                {SCOPE_CHOICES.find((c) => c.value === scope)?.hint} It can’t be changed later.
              </FieldHelp>
            </div>
            <div className="space-y-2">
              <Label htmlFor="key-expiry">Expires</Label>
              <Select value={expiry} onValueChange={(v) => setExpiry(v ?? defaultExpiry(maxDays))}>
                <SelectTrigger id="key-expiry">
                  <SelectValue>
                    {(v) => choices.find((c) => c.value === v)?.label ?? "Choose…"}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {choices.map((c) => (
                    <SelectItem key={c.value} value={c.value}>
                      {c.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {expiry === "0" && (
                <FieldHelp tone="attention">
                  A key that never expires stays valid until someone revokes it.
                </FieldHelp>
              )}
              {maxDays > 0 && (
                <FieldHelp>This install allows keys to last at most {maxDays} days.</FieldHelp>
              )}
            </div>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || !name.trim()}>
            {saving ? "Creating…" : "Create key"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** What to actually do with the key. Without this the page hands someone a
 * secret and no way to use it. */
function UsingKeysHelp() {
  return (
    <Disclosure label="Using a key" className="mt-8">
      <TableCard className="space-y-3 p-4" flat>
        <p className="text-sm text-muted-foreground">
          Send it as a bearer token or <Code>X-API-Key</Code>. It works on the platform API and on
          apps you can open.
        </p>
        <pre className="overflow-x-auto rounded-md bg-muted p-3 font-mono text-xs">
{`curl -H "Authorization: Bearer toftu_…" \\
     ${window.location.origin}/api/v1/apps`}
        </pre>
        <p className="text-xs text-muted-foreground">
          Managing keys, roles, groups and access needs a browser session:{" "}
          <Badge variant="outline" className="ml-0.5">403</Badge>{" "}
          <Code>session_required</Code>.
        </p>
        <p className="text-xs text-muted-foreground">
          A read-scoped key is refused changes with{" "}
          <Badge variant="outline" className="ml-0.5">403</Badge>{" "}
          <Code>key_read_scope</Code>.
        </p>
      </TableCard>
    </Disclosure>
  );
}

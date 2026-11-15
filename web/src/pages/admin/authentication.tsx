import { useState } from "react";
import { copyToClipboard } from "@/lib/clipboard";
import { redirectURIPreview } from "@/lib/redirect-uri";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Copy,
  KeyRound,
  Lock,
  MoreHorizontal,
  Plus,
  ShieldAlert,
  Users,
} from "lucide-react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import type { AuthProvider, AuthProviderInput, AuthProviderPreset, DiscoveryDoc } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
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
import { LicenseLock } from "@/components/license-lock";
import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { systemRoleLabel } from "@/lib/utils";
import { Notice } from "@/components/notice";
import { OnceOnlySecret } from "@/components/once-only-secret";
import { toneSurface } from "@/lib/tone";
import { Section, SectionHeading } from "@/components/section";
import { Disclosure } from "@/components/disclosure";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { AnonymousAccessEditor } from "@/components/anonymous-access-editor";
import { AppListingEditor } from "@/components/app-listing-editor";
import { DormantLockEditor } from "@/components/dormant-lock-editor";
import { SessionPolicyEditor } from "@/components/session-policy-editor";
import { TableCard } from "@/components/table-card";
import { UnreadableCredentialNotice } from "@/components/unreadable-credential";
import { FieldHelp } from "@/components/field-help";
import { credentialPlaceholder } from "@/lib/credentials";
import { TrustedIssuersEditor } from "@/components/trusted-issuers-editor";

/**
 * Admin → Authentication.
 *
 * Enterprise SSO is the feature and it leads the page: one corporate identity
 * provider, described by its issuer, with the provisioning policy attached to
 * it. Social logins are a convenience for small installs, so they sit in a
 * collapsed section at the bottom — each one still needs its own client
 * credentials and its own switch before anybody sees it.
 *
 * Client secrets are write-only here, because that is what the API allows: it
 * accepts a secret and thereafter reports only whether one is stored.
 */
/** One description on every branch, so a refused page says what a loaded one says. */
const PAGE_DESCRIPTION = "Any OpenID Connect provider works.";

export function AdminAuthenticationPage() {
  const queryClient = useQueryClient();
  const [adding, setAdding] = useState<AuthProviderPreset | null>(null);
  const [editing, setEditing] = useState<AuthProvider | null>(null);
  const [toDelete, setToDelete] = useState<AuthProvider | null>(null);
  const [scimFor, setScimFor] = useState<AuthProvider | null>(null);
  const [disablingLocal, setDisablingLocal] = useState(false);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["auth-providers"],
    queryFn: api.listAuthProviders,
    retry: false,
  });
  // `/admin/authentication#sso` from ⌘K, once the sections exist.
  useHashScroll(!!data);

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["auth-providers"] });
    // The login page reads a different endpoint; a provider that was just
    // enabled should appear there without a reload.
    queryClient.invalidateQueries({ queryKey: ["login-options"] });
  };

  // Plain derivations: these lists are a handful of items long, so memoizing
  // them would cost more in dependency bookkeeping than it saves.
  const allProviders = data?.providers ?? [];
  // Local password sign-in is a way in but not an identity provider, and almost
  // nothing in the SSO table applies to it — no issuer, no client credentials, no
  // provisioning policy. It gets its own section below.
  const providers = allProviders.filter((p) => p.kind !== "password");
  const localSignIn = allProviders.find((p) => p.kind === "password");
  const presets = data?.presets ?? [];
  const enterprisePresets = presets.filter((p) => p.category === "enterprise");
  const socialPresets = presets.filter((p) => p.category === "social");
  const configuredIds = new Set(providers.map((p) => p.id));

  // A switch's toast says what is now true, and the row beside it is what says
  // so. "Okta enabled" was reported over a row reading *Incomplete* or *Secret
  // unreadable* — the switch is on, and the provider is still not on the login
  // page. `login_ready` is the server's own answer to "can somebody sign in
  // with this", and it is the fact the toast now carries.
  async function toggle(provider: AuthProvider, enabled: boolean) {
    try {
      const saved = await api.updateAuthProvider(provider.id, { enabled });
      if (!enabled) {
        toast.success(`${provider.display_name} is off the login page`);
      } else if (saved.login_ready) {
        toast.success(`${provider.display_name} is on the login page`);
      } else {
        toast.warning(`${provider.display_name} is switched on, and not on the login page yet`, {
          description: saved.secret_unreadable
            ? "Its secret cannot be decrypted by this install’s key. Paste it again."
            : "It still needs a client ID and secret.",
        });
      }
      refresh();
    } catch (err) {
      toast.error(
        err instanceof Error
          ? err.message
          : `Couldn’t switch ${provider.display_name} ${enabled ? "on" : "off"}`,
      );
    }
  }

  if (error) {
    return (
      <div>
        <PageHeader title="Authentication" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The sign-in providers could not be read"
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
        title="Authentication"
        description={PAGE_DESCRIPTION}
        actions={
          <Button size="sm" disabled={isLoading} onClick={() => setAdding(enterprisePresets[0] ?? null)}>
            <Plus className="mr-1.5 h-4 w-4" />
            Add provider
          </Button>
        }
      />

      <LicenseLock feature="identity_providers" />

      <div className="space-y-8">
        <section id="sso" className="scroll-mt-20 space-y-3">
          <SectionHeading title="Single sign-on" />
          {isLoading ? (
            <TableCard className="space-y-3 p-4">
              {Array.from({ length: 3 }).map((_, i) => (
                <div key={i} className="flex items-center gap-3">
                  <Skeleton className="h-4 w-40" />
                  <Skeleton className="h-4 w-24" />
                  <Skeleton className="ml-auto h-4 w-16" />
                </div>
              ))}
            </TableCard>
          ) : providers.length === 0 ? (
            <EmptyState
              icon={<KeyRound className="h-6 w-6" />}
              title="No sign-in provider configured"
              description="Add your organization’s identity provider. Until then, only the built-in administrator can sign in."
              action={
                <Button size="sm" disabled={isLoading} onClick={() => setAdding(enterprisePresets[0] ?? null)}>
                  <Plus className="mr-1.5 h-4 w-4" />
                  Add provider
                </Button>
              }
            />
          ) : (
            <TableCard>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead className="w-full">Provider</TableHead>
                    <TableHead className="hidden @xl:table-cell">Type</TableHead>
                    <TableHead className="hidden @md:table-cell">Accounts</TableHead>
                    {/* Whether this provider is on the login page is the one
                        column that never yields: it is the answer the page is
                        read for. */}
                    <TableHead>Sign-in</TableHead>
                    <TableHead className="w-[50px]" />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {providers.map((p) => (
                    <TableRow key={p.id}>
                      <TableCell className="max-w-0">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium">{p.display_name}</span>
                          {!p.managed && (
                            <Badge
                              variant="outline"
                              className="gap-1 border-border/60 text-2xs text-muted-foreground"
                            >
                              <Lock className="h-3 w-3" />
                              From configuration
                            </Badge>
                          )}
                          {/* Who this provider lets in is not visible from its
                              name, and "authenticates fine, still refused" is
                              the support question it produces. */}
                          {!p.auto_provision && (
                            <Badge
                              variant="outline"
                              className="border-border/60 text-2xs text-muted-foreground"
                            >
                              Existing accounts only
                            </Badge>
                          )}
                          {/* A feed that has never called is the shape of
                              every SCIM misconfiguration, and it is invisible
                              on the IdP's side too. Attention rather than
                              danger: nothing is broken, something is
                              unfinished. */}
                          {p.scim_token_set_at && (
                            <Badge
                              variant="outline"
                              className={`text-2xs ${p.scim_last_call_at ? "border-border/60 text-muted-foreground" : toneSurface.attention}`}
                            >
                              {p.scim_last_call_at ? "Directory feed" : "Feed never used"}
                            </Badge>
                          )}
                        </div>
                        <p className="truncate font-mono text-xs text-muted-foreground">
                          {p.issuer || p.id}
                        </p>
                      </TableCell>
                      <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                        {p.kind_label}
                      </TableCell>
                      <TableCell className="hidden text-sm text-muted-foreground @md:table-cell">
                        <span className="inline-flex items-center gap-1.5">
                          <Users className="h-3.5 w-3.5" />
                          {p.user_count}
                        </span>
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          {/* The control, not a menu item. Taking a provider
                              off the login page is the act somebody opens this
                              page to perform — an IdP being replaced, a client
                              secret rotated, an outage traced to the door — and
                              it was the second item of a ⋯ menu and a checkbox
                              inside an edit form, next to the default-role
                              picker, where it saved only when the whole form
                              did. A provider Toft does not own is
                              read-only, so it gets the badge alone — and a
                              managed one gets a badge only when it says more
                              than the switch: on beside an on switch was the
                              ordinary case stated twice. */}
                          {p.managed && (
                            <Switch
                              checked={p.enabled}
                              aria-label={`${p.display_name} on the login page`}
                              onCheckedChange={(on) => void toggle(p, on)}
                            />
                          )}
                          {p.managed && (p.login_ready || !p.enabled) ? null : p.login_ready ? (
                            <Badge variant="outline" className={`text-2xs ${toneSurface.ok}`}>
                              On the login page
                            </Badge>
                          ) : p.enabled && p.secret_unreadable ? (
                            // Not "Incomplete", which reads as a form somebody
                            // never finished. This provider was configured and
                            // working; the key that decrypts its secret is gone.
                            <Badge variant="outline" className={`text-2xs ${toneSurface.danger}`}>
                              Secret unreadable
                            </Badge>
                          ) : p.enabled ? (
                            <Badge
                              variant="outline"
                              className={`text-2xs ${toneSurface.attention}`}
                            >
                              Incomplete
                            </Badge>
                          ) : (
                            <Badge variant="outline" className="text-2xs text-muted-foreground">
                              Off the login page
                            </Badge>
                          )}
                        </div>
                      </TableCell>
                      <TableCell>
                        {p.managed ? (
                          <DropdownMenu>
                            <DropdownMenuTrigger
                              render={
                                <Button
                                  variant="ghost"
                                  size="icon-sm"
                                  aria-label={`Actions for ${p.display_name}`}
                                >
                                  <MoreHorizontal className="h-4 w-4" />
                                </Button>
                              }
                            />
                            <DropdownMenuContent align="end">
                              <DropdownMenuItem onClick={() => setEditing(p)}>Edit…</DropdownMenuItem>
                              <DropdownMenuItem onClick={() => setScimFor(p)}>
                                {p.scim_token_set_at ? "Directory feed…" : "Set up directory feed…"}
                              </DropdownMenuItem>
                              <DropdownMenuItem
                                variant="destructive"
                                onClick={() => setToDelete(p)}
                              >
                                Remove…
                              </DropdownMenuItem>
                            </DropdownMenuContent>
                          </DropdownMenu>
                        ) : (
                          <span className="text-xs text-muted-foreground">—</span>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableCard>
          )}

        </section>

        <LocalSignInSection provider={localSignIn} onDisable={() => setDisablingLocal(true)} />

        <SocialLoginSection
          presets={socialPresets}
          configuredIds={configuredIds}
          onConfigure={setAdding}
        />

        {/* Last, and on this page rather than on Settings (ADR §50): what
            happens to the session a sign-in produces, and then what happens to
            the account itself. The session first — it is the shorter clock and
            the one somebody is asking about when they come here.

            Above both, whether a session is required at all: everything else on
            this page decides who may be here, and this decides whether being
            here is the price of opening anything. Somebody who has just
            finished configuring a provider is entitled to find out on the same
            page whether it can be walked past. */}
        {/* One card for the two audiences an app reaches without a grant —
            anonymous visitors, then everyone signed in, the smaller half: it
            discloses names and makes nothing readable. Two cards of one
            switch each was the page's heaviest chrome for its lightest rows. */}
        <TrustedIssuersEditor providers={allProviders} />

        <Section id="audiences" title="Who can see apps without a grant">
          <div className="divide-y divide-border/60">
            <AnonymousAccessEditor bare />
            <AppListingEditor bare />
          </div>
        </Section>

        <SessionPolicyEditor />

        <DormantLockEditor />
      </div>

      <ProviderDialog
        key={`add-${adding?.id ?? "none"}`}
        preset={adding}
        presets={presets}
        listed={allProviders}
        open={adding !== null}
        onOpenChange={(v) => !v && setAdding(null)}
        onSaved={refresh}
      />
      <ProviderDialog
        key={`edit-${editing?.id ?? "none"}`}
        provider={editing}
        presets={presets}
        listed={allProviders}
        open={editing !== null}
        onOpenChange={(v) => !v && setEditing(null)}
        onSaved={refresh}
      />
      <RemoveProviderDialog
        provider={toDelete}
        onOpenChange={() => setToDelete(null)}
        onRemoved={refresh}
      />
      {scimFor && (
        <SCIMFeedDialog
          key={scimFor.id}
          provider={scimFor}
          onOpenChange={() => setScimFor(null)}
          onChanged={refresh}
        />
      )}
      <DisableLocalSignInDialog
        provider={disablingLocal ? localSignIn : undefined}
        onOpenChange={() => setDisablingLocal(false)}
        onDisabled={refresh}
      />
    </div>
  );
}

/**
 * The built-in administrator.
 *
 * Its own section rather than a row in the table above, because it is the one way
 * in that is not an identity provider: no issuer, no client credentials, no
 * provisioning policy, and a password this install stores. It sits below the SSO
 * table for the same reason social login does — it is what an install uses before
 * SSO works, and what it falls back on when SSO breaks.
 *
 * Switching it on is deliberately absent from this page. It is a way in that
 * bypasses the identity provider, so turning it back on takes a shell on the
 * host; a door a stolen admin session can reopen was never shut. What the UI owes
 * someone in that state is the exact command, which is the whole content of the
 * disabled case.
 */
function LocalSignInSection({
  provider,
  onDisable,
}: {
  provider?: AuthProvider;
  onDisable: () => void;
}) {
  // Absent from the API means an install whose database predates it. Nothing to
  // report, and inventing a section for it would be a lie about what exists.
  if (!provider) return null;

  return (
    <Section
      id="local"
      title="Built-in administrator"
      description="A local account for setup and recovery when single sign-on isn’t working."
    >
      <div className="p-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <span className="font-medium">{provider.display_name}</span>
              {provider.enabled ? (
                <Badge
                  variant="outline"
                  className={`text-2xs ${toneSurface.ok}`}
                >
                  On the login page
                </Badge>
              ) : (
                <Badge variant="outline" className="text-2xs text-muted-foreground">
                  Off the login page
                </Badge>
              )}
            </div>
          </div>

          {provider.enabled && (
            <Button variant="outline" size="sm" onClick={onDisable}>
              Switch off…
            </Button>
          )}
        </div>

        {provider.enabled ? (
          <Notice className="mt-4" icon={<ShieldAlert className="h-4 w-4 shrink-0" />}>
            Switch this off once an administrator can sign in with single sign-on. If it still has
            the default password, change it now.
          </Notice>
        ) : (
          <Notice tone="neutral" icon={null} className="mt-4" title="To switch it back on, on the server:">
            <code className="font-mono">toft local-admin enable</code>
            <p className="mt-1.5">It sets a new password and prints it once.</p>
          </Notice>
        )}
      </div>
    </Section>
  );
}

/**
 * Confirmed, not typed: nothing is destroyed and the CLI brings it back. What the
 * dialog owes the administrator is the part they cannot discover afterwards —
 * that "back" means a shell on the server, not this page.
 */
function DisableLocalSignInDialog({
  provider,
  onOpenChange,
  onDisabled,
}: {
  provider?: AuthProvider;
  onOpenChange: (v: boolean) => void;
  onDisabled: () => void;
}) {
  const [saving, setSaving] = useState(false);

  async function submit() {
    if (!provider) return;
    setSaving(true);
    try {
      await api.updateAuthProvider(provider.id, { enabled: false });
      toast.success("The built-in administrator is off the login page");
      onOpenChange(false);
      onDisabled();
    } catch (err) {
      // The server refuses while no administrator can sign in another way, and
      // its refusal explains which condition failed — pass it through verbatim.
      toast.error(err instanceof Error ? err.message : "Couldn’t switch it off");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={provider !== undefined} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Switch off the built-in administrator?</DialogTitle>
          <DialogDescription>
            The form leaves the login page. The account is kept, but only{" "}
            <code className="font-mono">toft local-admin enable</code> on the server can turn it
            back on.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={saving}>
            {saving ? "Switching off…" : "Switch off"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Social login, deliberately quiet: a collapsed section, small type, and no
 * call to action competing with the SSO table above. Most enterprise installs
 * will never open it.
 */
function SocialLoginSection({
  presets,
  configuredIds,
  onConfigure,
}: {
  presets: AuthProviderPreset[];
  configuredIds: Set<string>;
  onConfigure: (p: AuthProviderPreset) => void;
}) {
  const available = presets.filter((p) => !configuredIds.has(p.id));

  return (
    <Section
      title="Social sign-in"
      description="Anyone with one of these accounts can sign in unless you restrict the email domain."
    >
      <div className="p-4">
        <Disclosure label="Google, GitHub, Microsoft, GitLab, Slack">
          {available.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              Every social provider is already configured above.
            </p>
          ) : (
            <div className="grid gap-2 sm:grid-cols-2">
              {available.map((p) => (
                <TableCard
                  flat
                  key={p.id}
                  className="flex items-center justify-between gap-3 px-3 py-2"
                >
                  <div className="min-w-0">
                    <p className="text-sm font-medium">{p.name}</p>
                    <p className="truncate text-xs text-muted-foreground">
                      {p.kind === "github" ? "OAuth 2.0" : "OpenID Connect"}
                    </p>
                  </div>
                  <Button size="sm" variant="outline" onClick={() => onConfigure(p)}>
                    Set up…
                  </Button>
                </TableCard>
              ))}
            </div>
          )}
        </Disclosure>
      </div>
    </Section>
  );
}

/**
 * One dialog for both adding and editing.
 *
 * The form's shape follows what the provider actually needs: an issuer for
 * OIDC, nothing but credentials for GitHub, and the endpoint overrides hidden
 * until someone has a provider whose discovery document is wrong.
 */
function ProviderDialog({
  provider,
  preset,
  presets,
  listed,
  open,
  onOpenChange,
  onSaved,
}: {
  provider?: AuthProvider | null;
  preset?: AuthProviderPreset | null;
  presets: AuthProviderPreset[];
  /** Every provider the server listed — where a new one's redirect URI base is read. */
  listed: AuthProvider[];
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onSaved: () => void;
}) {
  const editing = !!provider;
  const base = provider ?? null;
  const seed = preset ?? null;

  // Keyed remount (see the caller) is what resets these, so plain state is
  // enough — no effect syncing props into state.
  const [presetId, setPresetId] = useState(seed?.id ?? "");
  const [id, setId] = useState(base?.id ?? seed?.id ?? "");
  const [kind, setKind] = useState(base?.kind ?? seed?.kind ?? "oidc");
  const [displayName, setDisplayName] = useState(base?.display_name ?? seed?.name ?? "");
  const [issuer, setIssuer] = useState(base?.issuer ?? seed?.issuer ?? "");
  const [clientId, setClientId] = useState(base?.client_id ?? "");
  const [clientSecret, setClientSecret] = useState("");
  const [scopes, setScopes] = useState(base?.scopes ?? seed?.scopes?.join(" ") ?? "");
  const [authURL, setAuthURL] = useState(base?.auth_url ?? "");
  const [tokenURL, setTokenURL] = useState(base?.token_url ?? "");
  const [userinfoURL, setUserinfoURL] = useState(base?.userinfo_url ?? "");
  const [allowedDomains, setAllowedDomains] = useState(base?.allowed_domains ?? "");
  const [defaultRole, setDefaultRole] = useState(base?.default_role ?? "viewer");
  const [linkByEmail, setLinkByEmail] = useState(base?.link_by_verified_email ?? false);
  const [autoProvision, setAutoProvision] = useState(base?.auto_provision ?? true);
  const [groupsClaim, setGroupsClaim] = useState(base?.groups_claim ?? "");
  const [allowedEmails, setAllowedEmails] = useState(base?.allowed_emails ?? "");
  const [permittedGroups, setPermittedGroups] = useState(base?.permitted_login_groups ?? "");
  const [groupsSeparator, setGroupsSeparator] = useState(base?.groups_separator ?? "");
  const [enabled, setEnabled] = useState(base?.enabled ?? true);
  const [discovery, setDiscovery] = useState<DiscoveryDoc | null>(null);
  const [probing, setProbing] = useState(false);
  const [saving, setSaving] = useState(false);

  const activePreset = presets.find((p) => p.id === presetId) ?? seed;
  const redirectURI =
    base?.redirect_uri ?? redirectURIPreview(listed, id, window.location.origin);

  function applyPreset(nextId: string) {
    const p = presets.find((x) => x.id === nextId);
    setPresetId(nextId);
    if (!p) return;
    setKind(p.kind);
    setId(p.id);
    setDisplayName(p.name);
    setIssuer(p.issuer ?? "");
    setScopes(p.scopes?.join(" ") ?? "");
    setDiscovery(null);
  }

  async function probe() {
    setProbing(true);
    setDiscovery(null);
    try {
      const doc = await api.discoverIssuer(issuer.trim());
      setDiscovery(doc);
      toast.success("Discovery document found");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t read the discovery document");
    } finally {
      setProbing(false);
    }
  }

  async function submit() {
    const payload: AuthProviderInput = {
      id: id.trim(),
      kind,
      display_name: displayName.trim(),
      // Only a new provider says it. On an existing one the row's switch is
      // the one writer (see the form below), and the snapshot this form opened
      // with would re-flip a switch somebody threw since.
      ...(!editing && { enabled }),
      client_id: clientId.trim(),
      issuer: kind === "oidc" ? issuer.trim() : "",
      auth_url: authURL.trim(),
      token_url: tokenURL.trim(),
      userinfo_url: userinfoURL.trim(),
      scopes: scopes.trim(),
      allowed_domains: allowedDomains.trim(),
      default_role: defaultRole,
      link_by_verified_email: linkByEmail,
      auto_provision: autoProvision,
      groups_claim: groupsClaim.trim(),
      allowed_emails: allowedEmails.trim(),
      permitted_login_groups: permittedGroups.trim(),
      // Deliberately not trimmed: a separator may legitimately be a space, and
      // trimming would turn that into “no separator” on a field somebody filled
      // in on purpose.
      groups_separator: groupsSeparator,
    };
    // An empty secret means "keep the stored one" on the way through the API,
    // which is only meaningful when editing.
    if (clientSecret.trim()) payload.client_secret = clientSecret.trim();

    setSaving(true);
    try {
      if (editing) {
        await api.updateAuthProvider(provider!.id, payload);
        toast.success("Saved");
      } else {
        const created = await api.createAuthProvider(payload);
        // The server's URI is the one it will send; if the preview could not
        // know it, say which to register.
        if (created.redirect_uri && created.redirect_uri !== redirectURI) {
          toast.warning(`Added “${payload.display_name}” — register ${created.redirect_uri} at the provider`, {
            duration: 15_000,
          });
        } else {
          toast.success(`Added “${payload.display_name}”`);
        }
      }
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : `Couldn’t save “${payload.display_name}”`,
      );
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{editing ? `Edit ${provider!.display_name}` : "Add sign-in provider"}</DialogTitle>
          <DialogDescription>
            Register the redirect URI below at your provider, then paste its client ID and secret.
          </DialogDescription>
        </DialogHeader>

        {/* The body is what scrolls. The whole dialog scrolling took the
            footer's Save button off the screen with it. */}
        <DialogBody className="space-y-4">
          {!editing && (
            <div className="space-y-1.5">
              <Label htmlFor="provider-preset">Provider</Label>
              <Select value={presetId} onValueChange={(v) => applyPreset(v ?? "")}>
                <SelectTrigger id="provider-preset" className="w-full">
                  {/* base-ui's Value renders the raw value — here, the
                      preset's id. Name it. */}
                  <SelectValue>
                    {(v) => presets.find((p) => p.id === v)?.name ?? "Choose a provider…"}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {presets.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {activePreset?.hint && (
                <FieldHelp>
                  {activePreset.hint}
                  {activePreset.docs_url && (
                    <>
                      {" "}
                      <a
                        href={activePreset.docs_url}
                        target="_blank"
                        rel="noreferrer"
                        className="underline underline-offset-2 hover:text-foreground"
                      >
                        Vendor docs
                      </a>
                    </>
                  )}
                </FieldHelp>
              )}
            </div>
          )}

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="provider-name">Display name</Label>
              <Input
                id="provider-name"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                placeholder="Acme SSO"
              />
              <FieldHelp>Shown on the login button.</FieldHelp>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="provider-id">ID</Label>
              <Input
                id="provider-id"
                value={id}
                onChange={(e) => setId(e.target.value.toLowerCase())}
                placeholder="sso"
                disabled={editing}
                className="font-mono"
              />
              <FieldHelp>
                {editing
                  ? "Part of the redirect URI, so it can’t change."
                  : "Lowercase. Part of the redirect URI, so it can’t change later."}
              </FieldHelp>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="redirect-uri">Redirect URI</Label>
            <div className="flex gap-2">
              <Input id="redirect-uri" readOnly value={redirectURI} className="font-mono text-xs" />
              <Button
                variant="outline"
                size="icon"
                aria-label="Copy redirect URI"
                onClick={() => {
                  void copyToClipboard(redirectURI, "redirect URI");
                }}
              >
                <Copy className="h-4 w-4" />
              </Button>
            </div>
            <FieldHelp>
              Must match exactly at the provider, including any trailing slash.
            </FieldHelp>
          </div>

          {kind === "oidc" && (
            <div className="space-y-1.5">
              <Label htmlFor="provider-issuer">Issuer URL</Label>
              <div className="flex gap-2">
                <Input
                  id="provider-issuer"
                  value={issuer}
                  onChange={(e) => {
                    setIssuer(e.target.value);
                    setDiscovery(null);
                  }}
                  placeholder={activePreset?.issuer_template ?? "https://idp.example.com"}
                  className="font-mono text-xs"
                />
                <Button variant="outline" onClick={probe} disabled={!issuer.trim() || probing}>
                  {probing ? "Testing…" : "Test"}
                </Button>
              </div>
              {discovery ? (
                <Notice live tone="ok" title="Discovery document read">
                  <p className="truncate font-mono">{discovery.authorization_endpoint}</p>
                  <p className="truncate font-mono">{discovery.token_endpoint}</p>
                  <p className="truncate font-mono">{discovery.userinfo_endpoint}</p>
                </Notice>
              ) : (
                <FieldHelp>
                  Test it before saving, or a wrong issuer shows up only at the first sign-in.
                </FieldHelp>
              )}
            </div>
          )}

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="provider-client-id">Client ID</Label>
              <Input
                id="provider-client-id"
                value={clientId}
                onChange={(e) => setClientId(e.target.value)}
                className="font-mono text-xs"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="provider-secret">Client secret</Label>
              <Input
                id="provider-secret"
                type="password"
                value={clientSecret}
                onChange={(e) => setClientSecret(e.target.value)}
                placeholder={credentialPlaceholder(
                  base?.secret_set ?? false,
                  base?.secret_unreadable ?? false,
                  "",
                )}
                className="font-mono text-xs"
              />
              {base?.secret_unreadable ? (
                <UnreadableCredentialNotice what="client secret" />
              ) : (
                base?.secret_set && (
                  <FieldHelp>Leave blank to keep the stored one.</FieldHelp>
                )
              )}
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="provider-domains">Allowed email domains</Label>
            <Input
              id="provider-domains"
              value={allowedDomains}
              onChange={(e) => setAllowedDomains(e.target.value)}
              placeholder="acme.com, acme.co.uk"
            />
            <FieldHelp>
              {activePreset?.multi_tenant
                ? "Strongly recommended. When empty, anyone with an account at this provider can sign in."
                : "Comma-separated. When empty, any account at this provider can sign in."}
            </FieldHelp>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="provider-emails">Allowed email addresses</Label>
            <Input
              id="provider-emails"
              value={allowedEmails}
              onChange={(e) => setAllowedEmails(e.target.value)}
              placeholder="contractor@other.com"
            />
            <FieldHelp>
              Comma-separated. Lets in these people as well as the domains above.
            </FieldHelp>
          </div>

          {kind === "oidc" && (
            <div className="space-y-1.5">
              <Label htmlFor="provider-permitted-groups">Allowed groups</Label>
              <Input
                id="provider-permitted-groups"
                value={permittedGroups}
                onChange={(e) => setPermittedGroups(e.target.value)}
                placeholder="data-team, platform"
                className="font-mono text-xs"
              />
              <FieldHelp>
                Comma-separated claim values from the provider. When empty, every group can sign in.
              </FieldHelp>
              {permittedGroups.trim() !== "" && (
                <Notice tone="attention" title="A sign-in carrying no group claim will be refused">
                  <p className="leading-relaxed">
                    If the provider stops sending{" "}
                    <span className="font-mono">{groupsClaim.trim() || "groups"}</span>, everyone using it
                    is locked out. Check that it arrives first.
                  </p>
                </Notice>
              )}
            </div>
          )}

          <div className="flex items-start justify-between gap-3 rounded-md border border-border/60 p-3">
            <div className="space-y-0.5">
              <Label htmlFor="provider-auto-provision" className="text-sm">
                Create accounts on first sign-in
              </Label>
              <FieldHelp>
                {/* Text, not a link: a link here navigated away and threw
                    away every field typed into this dialog. */}
                When off, only people who already have an account can sign in. Edit what
                refused people see on Admin → Messages.
              </FieldHelp>
            </div>
            <Switch
              id="provider-auto-provision"
              checked={autoProvision}
              onCheckedChange={setAutoProvision}
            />
          </div>

          {/* One column once the Enabled switch has gone: an empty half
              beside a paragraph is not a layout. */}
          <div className={`grid gap-3 ${editing ? "" : "sm:grid-cols-2"}`}>
            <div className="space-y-1.5">
              <Label htmlFor="default-role">Role for new accounts</Label>
              <Select
                value={defaultRole}
                onValueChange={(v) => setDefaultRole(v ?? "viewer")}
                disabled={!autoProvision}
              >
                <SelectTrigger id="default-role" className="w-full">
                  {/* base-ui's Value renders the raw value, not the chosen
                      item's text — format it here. */}
                  <SelectValue>{(v) => systemRoleLabel(v as string)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="viewer">Viewer</SelectItem>
                  <SelectItem value="publisher">Publisher</SelectItem>
                  {/* The third answer, and the one that is not a role: the
                      account is created and cannot be used until somebody
                      lets it in. */}
                  <SelectItem value="pending">Pending activation</SelectItem>
                </SelectContent>
              </Select>
              <FieldHelp>
                {!autoProvision
                  ? "No effect while this provider does not create accounts."
                  : defaultRole === "pending"
                    ? "New accounts can’t sign in until you activate them under People."
                    : "Set once, when the account is created."}
              </FieldHelp>
            </div>
            {/* On a new provider this is the only place to say it. On an
                existing one it is not: the row's own switch is, and it saves
                when it is thrown rather than when the form is. A second copy
                here meant an administrator could open this form to fix an
                issuer, and take the provider off the login page by saving a
                stale checkbox. */}
            {!editing && (
              <div className="space-y-1.5">
                <Label htmlFor="provider-enabled">Enabled</Label>
                <div className="flex h-9 items-center">
                  <Switch id="provider-enabled" checked={enabled} onCheckedChange={setEnabled} />
                </div>
                <FieldHelp>
                  When off, it stays off the login page.
                </FieldHelp>
              </div>
            )}
          </div>

          <div className="flex items-start justify-between gap-3 rounded-md border border-border/60 p-3">
            <div className="space-y-0.5">
              <Label htmlFor="provider-link" className="text-sm">
                Link to existing accounts by verified email
              </Label>
              <FieldHelp>
                When on, a sign-in takes over an existing account with the same email. Leave off
                unless you trust this provider.
              </FieldHelp>
            </div>
            <Switch id="provider-link" checked={linkByEmail} onCheckedChange={setLinkByEmail} />
          </div>

          <Disclosure label="Advanced" size="xs">
            <div className="space-y-3 rounded-md border border-border/60 p-3">
              <div className="space-y-1.5">
                <Label htmlFor="provider-scopes">Scopes</Label>
                <Input
                  id="provider-scopes"
                  value={scopes}
                  onChange={(e) => setScopes(e.target.value)}
                  placeholder={kind === "github" ? "read:user user:email" : "openid profile email"}
                  className="font-mono text-xs"
                />
                <FieldHelp>
                  Space-separated. Empty uses the defaults shown.
                </FieldHelp>
              </div>
              {kind === "oidc" && (
                <>
                  <FieldHelp>
                    Endpoint overrides. Leave blank to use what the issuer publishes.
                  </FieldHelp>
                  <div className="space-y-1.5">
                    <Label htmlFor="provider-auth-url">Authorization endpoint</Label>
                    <Input
                      id="provider-auth-url"
                      value={authURL}
                      onChange={(e) => setAuthURL(e.target.value)}
                      className="font-mono text-xs"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="provider-token-url">Token endpoint</Label>
                    <Input
                      id="provider-token-url"
                      value={tokenURL}
                      onChange={(e) => setTokenURL(e.target.value)}
                      className="font-mono text-xs"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="provider-userinfo-url">Userinfo endpoint</Label>
                    <Input
                      id="provider-userinfo-url"
                      value={userinfoURL}
                      onChange={(e) => setUserinfoURL(e.target.value)}
                      className="font-mono text-xs"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="provider-groups-claim">Groups claim</Label>
                    <Input
                      id="provider-groups-claim"
                      value={groupsClaim}
                      onChange={(e) => setGroupsClaim(e.target.value)}
                      placeholder="groups"
                      className="font-mono text-xs"
                    />
                    <FieldHelp>
                      Most providers need <span className="font-mono">groups</span> added to the
                      scopes above to send it.
                    </FieldHelp>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="provider-groups-separator">Groups separator</Label>
                    <Input
                      id="provider-groups-separator"
                      value={groupsSeparator}
                      onChange={(e) => setGroupsSeparator(e.target.value)}
                      placeholder="(none)"
                      className="font-mono text-xs"
                    />
                    <FieldHelp>
                      Only for a provider that sends the list as one string. Most need it empty.
                    </FieldHelp>
                  </div>
                </>
              )}
            </div>
          </Disclosure>
        </DialogBody>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={submit}
            disabled={saving || !id.trim() || !displayName.trim() || !clientId.trim()}
          >
            {saving ? "Saving…" : editing ? "Save changes" : "Add provider"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Removal is confirmed but not typed: it takes nobody's account away, and
 * re-adding the provider under the same id restores sign-in exactly as it was.
 * Disabling is the reversible option and the dialog says so.
 */
function RemoveProviderDialog({
  provider,
  onOpenChange,
  onRemoved,
}: {
  provider: AuthProvider | null;
  onOpenChange: (v: boolean) => void;
  onRemoved: () => void;
}) {
  const [removing, setRemoving] = useState(false);

  async function submit() {
    if (!provider) return;
    setRemoving(true);
    try {
      await api.deleteAuthProvider(provider.id);
      toast.success(`Removed ${provider.display_name}`);
      onOpenChange(false);
      onRemoved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t remove the provider");
    } finally {
      setRemoving(false);
    }
  }

  return (
    <Dialog open={provider !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Remove “{provider?.display_name}”?</DialogTitle>
          <DialogDescription>
            {provider?.user_count
              ? `${provider.user_count} account${provider.user_count === 1 ? "" : "s"} can no longer sign in through it. No account is deleted.`
              : "Nobody signs in through it yet."}{" "}
            To keep it but hide it from the login page, switch it off instead.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={removing} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={submit} disabled={removing}>
            {removing ? "Removing…" : "Remove provider"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ── The directory feed ──────────────────────────────────────

/**
 * Where an administrator mints the credential a directory presents to
 * `/scim/v2`, and reads back the address to paste beside it.
 *
 * The token is shown **once**. It is not readable afterwards and minting again
 * rotates rather than reveals, so the copy has to make that clear before the
 * dialog closes rather than after.
 *
 * What the feed may do is stated here rather than left to be inferred, because
 * it is narrower than an administrator would assume from the word SCIM: it
 * deactivates and reactivates accounts and moves group membership, and it
 * cannot create an account, delete one, or change what anybody may do.
 */
function SCIMFeedDialog({
  provider, onOpenChange, onChanged,
}: {
  provider: AuthProvider;
  onOpenChange: (o: boolean) => void;
  onChanged: () => void;
}) {
  const [minted, setMinted] = useState<{ token: string; base_url: string } | null>(null);
  const [busy, setBusy] = useState(false);
  /** Which act is waiting on a confirm, if either. */
  const [confirming, setConfirming] = useState<"revoke" | "mint" | null>(null);

  async function mint() {
    setBusy(true);
    try {
      const res = await api.mintSCIMToken(provider.id);
      setMinted(res);
      onChanged();
      toast.success("Token created — copy it now");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t create the token");
    } finally {
      setBusy(false);
    }
  }

  async function revoke() {
    setBusy(true);
    try {
      await api.revokeSCIMToken(provider.id);
      setMinted(null);
      onChanged();
      toast.success("Feed switched off — the directory’s next call is refused");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t switch the feed off");
    } finally {
      setBusy(false);
    }
  }

  const base = `${window.location.origin}/scim/v2`;

  // The token is a once-only credential, so it goes through the one dialog in
  // the product that refuses to dismiss itself. It used to be an `<Input>`
  // inside this dialog, which Escape, a backdrop click and a ✕ all closed —
  // destroying a credential that exists, is authorized, and can never be read
  // again. The base URL comes with it, because pasting one without the other
  // at the provider configures nothing.
  if (minted) {
    return (
      <OnceOnlySecret
        open
        title={`Directory feed token for ${provider.display_name}`}
        secret={minted.token}
        label="SCIM token"
        onDismiss={() => setMinted(null)}
      >
        <div className="space-y-1.5">
          <Label htmlFor="scim-minted-base">Base URL</Label>
          <div className="flex gap-2">
            <Input id="scim-minted-base" readOnly value={base} className="font-mono text-xs" />
            <Button
              variant="outline"
              size="icon"
              aria-label="Copy the SCIM base URL"
              onClick={() => void copyToClipboard(base, "base URL")}
            >
              <Copy className="h-4 w-4" />
            </Button>
          </div>
          <FieldHelp>
            Paste both at {provider.display_name}.
          </FieldHelp>
        </div>
      </OnceOnlySecret>
    );
  }

  // A second branch inside the same DialogContent rather than a nested Dialog:
  // this was the one credential in the product that could be revoked *and*
  // rotated from a single unconfirmed click, while every other credential
  // revoke goes through a plain confirm — the tier the policy table assigns to
  // "revoke a key (any of the three kinds)". Both acts stop the directory's
  // next call to /scim/v2.
  if (confirming) {
    const replacing = confirming === "mint";
    return (
      <Dialog open onOpenChange={onOpenChange}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {replacing ? "Replace the token in use?" : "Switch the directory feed off?"}
            </DialogTitle>
            <DialogDescription>
              {replacing
                ? `The old token stops working now. ${provider.display_name}’s calls fail until you paste the new one there.`
                : `${provider.display_name}’s next call is refused. Accounts and groups here are kept.`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={busy} onClick={() => setConfirming(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => {
                setConfirming(null);
                if (replacing) void mint();
                else void revoke();
              }}
            >
              {replacing ? "Replace the token" : "Switch off"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Directory feed for {provider.display_name}</DialogTitle>
          <DialogDescription>
            A SCIM 2.0 feed that lets this directory deactivate accounts and manage membership of
            its mapped groups.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-1.5">
          <Label htmlFor="scim-base">Base URL</Label>
          <div className="flex gap-2">
            <Input id="scim-base" readOnly value={base} className="font-mono text-xs" />
            <Button
              variant="outline"
              size="icon"
              aria-label="Copy the SCIM base URL"
              onClick={() => void copyToClipboard(base, "base URL")}
            >
              <Copy className="h-4 w-4" />
            </Button>
          </div>
        </div>

        {provider.scim_token_set_at ? (
          <Notice
            tone={provider.scim_last_call_at ? "ok" : "attention"}
            size="sm"
            title={provider.scim_last_call_at ? "A feed is configured" : "Configured, and never used"}
          >
            {provider.scim_last_call_at
              ? "Creating another token replaces the one in use."
              : "No call has arrived yet. Check the provider’s connector, base URL and token."}
          </Notice>
        ) : (
          <FieldHelp>
            No token yet.
          </FieldHelp>
        )}

        <DialogFooter>
          {provider.scim_token_set_at && (
            <Button variant="outline" disabled={busy} onClick={() => setConfirming("revoke")}>
              Switch off
            </Button>
          )}
          {/* One click while there is nothing to lose; a confirm once there is
              a live token, because then this button is a rotation. */}
          <Button
            disabled={busy}
            onClick={() => (provider.scim_token_set_at ? setConfirming("mint") : void mint())}
          >
            {busy ? "Creating…" : provider.scim_token_set_at ? "Create a new token" : "Create a token"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

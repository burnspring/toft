import { Navigate, useParams, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { UserAvatar } from "@/components/user-avatar";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { QueryError } from "@/components/query-error";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { api, statusOf } from "@/lib/api";
import { systemRoleLabel } from "@/lib/utils";
import { MeOverviewTab } from "@/pages/me/overview";
import { MeAccessTab } from "@/pages/me/access";
import { MeCredentialsTab } from "@/pages/me/credentials";
import { MeStorageTab } from "@/pages/me/storage";
import { MeSecurityTab } from "@/pages/me/security";

/**
 * `/me` — the one page whose subject is the person reading it.
 *
 * Everything this platform knew about somebody was already legible on
 * `/people/:id`, which that person cannot open: it is administrator-only
 * deliberately, and the subject of a record is not exempt from that gate. This
 * page is the other side of it — the same facts, read by the account they are
 * about, through routes that take no id.
 *
 * **Not** `/people/:id` with an `isMe` branch. That page's premise is
 * administrator-only, and widening it so non-administrators may read a filtered
 * subset would put "may I see this" in the render path of every row. The reads
 * differ, the gate differs, and the writes differ. What they share is
 * components — `AccountFacts`, `AccessTable`, `SourceChip` — which is the level
 * at which sharing is safe.
 *
 * Tabs rather than one long scroll because the sections answer different
 * questions and only one is ever urgent; tabs rather than four routes because
 * three of them are a single card each, and a nav entry per card is how
 * `/account/…` became a tree in the first place.
 */

const VALID_TABS = ["overview", "access", "storage", "credentials", "security"] as const;
type TabValue = (typeof VALID_TABS)[number];

export function MePage() {
  const { tab } = useParams<{ tab?: string }>();
  const navigate = useNavigate();
  useDocumentTitle("Your account");

  const { data: account, isLoading, error, refetch } = useQuery({
    queryKey: ["my-account"],
    queryFn: api.getMyAccount,
    retry: false,
  });

  // The access report is the one place that computes an effective role per app.
  // This page reads it; it never re-derives reach from grants.
  const { data: access, error: accessError, refetch: refetchAccess } = useQuery({
    queryKey: ["my-access"],
    queryFn: api.getMyAccess,
    retry: false,
  });

  /**
   * One query drives both whether the Credentials tab exists and what is in it.
   *
   * When personal keys are switched off install-wide, every `/me/api-keys`
   * route refuses, and the invariant is *absent, not dormant* — so the tab is
   * not there at all rather than there and empty. Reading that from the same
   * query the tab body uses means the two cannot disagree.
   */
  const { data: keys, error: keysError } = useQuery({
    queryKey: ["my-api-keys"],
    queryFn: api.listMyApiKeys,
    retry: false,
  });
  // **404 is the only answer that means absent.** `!keysError` treated every
  // failure as the capability being off, so a 500 or a dropped connection took
  // the tab away and the body said "API keys are not available on this
  // install" — a policy statement about the install, made from a network
  // error. Any other refusal keeps the tab, where `QueryError` says what
  // actually happened in the server's own words. And an unanswered read is the
  // third state: neither, so the tab cannot appear and then vanish.
  const keysAvailable = keys !== undefined || (!!keysError && statusOf(keysError) !== 404);

  /**
   * The same query the tab body reads, and the same three states the header
   * nav used to honour when Storage was a link up there: absent (404 — no
   * signer, no store uploads), present-but-empty (200 with nothing in it —
   * this person holds no grant), and not-answered-yet, which offers nothing,
   * so the tab cannot appear and then vanish under the pointer.
   */
  const { data: stores, error: storesError } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
  });
  const storageAvailable = !storesError && (stores?.stores.length ?? 0) > 0;

  const requested = (tab ?? "overview") as TabValue;
  const valid = VALID_TABS.includes(requested) ? requested : "overview";
  // A tab that is not offered on this install is not a destination. Landing on
  // it — from a bookmark made before the switch was thrown, or from the
  // /account/api-keys redirect — resolves to Overview rather than to nothing.
  const unavailable =
    (valid === "credentials" && !keysAvailable) || (valid === "storage" && !storageAvailable);
  const currentTab: TabValue = unavailable ? "overview" : valid;

  // Once the answer is in, an unavailable tab gives up its address too —
  // Overview under `/me/storage` is a page that disagrees with its URL. While
  // the read is unanswered nothing moves, so a tab that will exist is never
  // bounced away from.
  const knownUnavailable =
    requested !== "overview" &&
    (!VALID_TABS.includes(requested) ||
      (requested === "credentials" && !!keysError && statusOf(keysError) === 404) ||
      (requested === "storage" && (!!storesError || (stores !== undefined && stores.stores.length === 0))));

  function onTabChange(next: string) {
    navigate(next === "overview" ? "/me" : `/me/${next}`);
  }

  if (knownUnavailable) return <Navigate to="/me" replace />;

  if (error) {
    return (
      <QueryError
        title="Your account could not be read"
        error={error}
        fallback="Sign in again to see your account."
        onRetry={() => void refetch()}
      />
    );
  }

  if (isLoading || !account) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-12 w-72" />
        <Skeleton className="h-9 w-80" />
        <Skeleton className="h-28 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  const user = account.user;

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <UserAvatar id={user.id} login={user.login} name={user.name} avatarUrl={user.avatar_url} size="lg" />
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            <h1 className="truncate text-2xl font-semibold tracking-tight">
              {user.name || user.login}
            </h1>
            {user.system_role && (
              <Badge variant="secondary" className="text-2xs">
                {systemRoleLabel(user.system_role)}
              </Badge>
            )}
          </div>
          <p className="mt-0.5 truncate text-sm text-muted-foreground">
            {/* The login only where the heading is a different name: an
                account whose name is its login read "admin-one" twice. */}
            {user.name && user.name !== user.login ? `${user.login} · ${user.email}` : user.email}
          </p>
        </div>
      </div>

      <Tabs value={currentTab} onValueChange={onTabChange}>
        <TabsList variant="line">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="access">Access</TabsTrigger>
          {storageAvailable && <TabsTrigger value="storage">Storage</TabsTrigger>}
          {keysAvailable && <TabsTrigger value="credentials">Credentials</TabsTrigger>}
          <TabsTrigger value="security">Security</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="pt-4">
          <MeOverviewTab account={account} access={access} />
        </TabsContent>
        <TabsContent value="access" className="pt-4">
          <MeAccessTab access={access} error={accessError} onRetry={() => void refetchAccess()} />
        </TabsContent>
        {storageAvailable && (
          <TabsContent value="storage" className="pt-4">
            {/* Reads the same query key as the gate above, so this is the
                cached result rather than a second request. */}
            <MeStorageTab />
          </TabsContent>
        )}
        <TabsContent value="security" className="pt-4">
          <MeSecurityTab account={account} />
        </TabsContent>
        {keysAvailable && (
          <TabsContent value="credentials" className="pt-4">
            {/* Reads the same query key, so this is the cached result rather
                than a second request. */}
            <MeCredentialsTab />
          </TabsContent>
        )}
      </Tabs>
    </div>
  );
}

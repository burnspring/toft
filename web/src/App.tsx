import { BrowserRouter, Routes, Route, Navigate, Link } from "react-router-dom";
import { ShieldAlert } from "lucide-react";
import { Fragment, useState, useEffect, useCallback, lazy, Suspense } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "sonner";
import { AuthContext } from "@/hooks/use-auth";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import { loadAppUrlConfig } from "@/lib/app-url";
import { displayTimeZone, resolveDisplayTimeZone, setDisplayTimeZone } from "@/lib/time";
import type { User } from "@/lib/api";
import { RouteAnnouncer } from "@/components/route-announcer";
import { AdminLayout } from "@/components/layout/admin-layout";
import { ErrorBoundary } from "@/components/error-boundary";
import { EmptyState } from "@/components/empty-state";
import { buttonVariants } from "@/components/ui/button-variants";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { NotFoundPage } from "@/pages/not-found";
import { LoginPage } from "@/pages/login";
import { CliSignInPage } from "@/pages/cli-sign-in";

// Lazy, for the reason the admin half below is: everybody who signs in loads
// the app shell, and none of them will ever see this page. A static import
// would put a marketing page in the first paint of an operations console.
const LandingPage = lazy(() =>
  import("@/pages/landing").then((m) => ({ default: m.LandingPage })),
);

/**
 * The admin half, loaded when somebody opens it.
 *
 * Twenty-seven pages, and nobody who is not an administrator will ever render
 * one — the same argument that already made the landing page lazy, applied to
 * the largest thing in the bundle rather than the smallest. The SPA shipped as
 * a single 1.57 MB chunk with exactly one lazy route.
 *
 * `AdminLayout` stays eager: it is the shell those pages render into, and a
 * spinner where the rail should be on every admin navigation is worse than the
 * bytes it saves.
 */
const lazily = <T extends string>(
  load: () => Promise<Record<string, unknown>>,
  name: T,
) => lazy(() => load().then((m) => ({ default: m[name] as React.ComponentType })));

/**
 * The signed-in half, for the same reason one level up (ADR §124).
 *
 * The admin split left every other page eager, so `/login` — the first page of
 * every session, and the only one an anonymous visitor reaches — still fetched
 * the dashboard, the app detail page and the shell around them before it could
 * paint: 0.9 MB of script, about 670 KB of it unused there. The login page, the
 * CLI approval screen and the not-found page stay eager; they are small, and
 * each is the whole of the screen it is on.
 */
const Layout = lazily(() => import("@/components/layout/layout"), "Layout");
const DashboardPage = lazily(() => import("@/pages/dashboard"), "DashboardPage");
const NewAppPage = lazily(() => import("@/pages/new-app"), "NewAppPage");
const ConnectRedirectPage = lazily(() => import("@/pages/connect-redirect"), "ConnectRedirectPage");
const AppDetailPage = lazily(() => import("@/pages/app-detail"), "AppDetailPage");
const GalleryPage = lazily(() => import("@/pages/gallery"), "GalleryPage");
const StoreBrowserPage = lazily(() => import("@/pages/store-browser"), "StoreBrowserPage");
const AdminStoreBrowserPage = lazily(() => import("@/pages/store-browser"), "AdminStoreBrowserPage");
const PeoplePage = lazily(() => import("@/pages/people"), "PeoplePage");
const UserDetailPage = lazily(() => import("@/pages/user-detail"), "UserDetailPage");
const DocumentationPage = lazily(() => import("@/pages/documentation"), "DocumentationPage");
const TagBrowsePage = lazily(() => import("@/pages/tag-browse"), "TagBrowsePage");
const MePage = lazily(() => import("@/pages/me"), "MePage");

const AdminHomePage = lazily(() => import("@/pages/admin/home"), "AdminHomePage");
const AdminInfoPage = lazily(() => import("@/pages/admin/info"), "AdminInfoPage");
const AdminSettingsPage = lazily(() => import("@/pages/admin/settings"), "AdminSettingsPage");
const AdminUsagePage = lazily(() => import("@/pages/admin/usage"), "AdminUsagePage");
const AdminReachPage = lazily(() => import("@/pages/admin/reach"), "AdminReachPage");
const AdminCompliancePage = lazily(() => import("@/pages/admin/compliance"), "AdminCompliancePage");
const AdminScanTargetsPage = lazily(() => import("@/pages/admin/scan-targets"), "AdminScanTargetsPage");
const AdminStoragePage = lazily(() => import("@/pages/admin/storage"), "AdminStoragePage");
const AdminStorageDetailPage = lazily(() => import("@/pages/admin/storage-detail"), "AdminStorageDetailPage");
const AdminTagsPage = lazily(() => import("@/pages/admin/tags"), "AdminTagsPage");
const AdminAuthenticationPage = lazily(() => import("@/pages/admin/authentication"), "AdminAuthenticationPage");
const AdminAuditPage = lazily(() => import("@/pages/admin/audit"), "AdminAuditPage");
const AdminChecksPage = lazily(() => import("@/pages/admin/checks"), "AdminChecksPage");
const AdminAutomationPage = lazily(() => import("@/pages/admin/automation"), "AdminAutomationPage");
const AdminProcessesPage = lazily(() => import("@/pages/admin/processes"), "AdminProcessesPage");
const AdminCredentialsPage = lazily(() => import("@/pages/admin/credentials"), "AdminCredentialsPage");
const AdminCredentialDetailPage = lazily(() => import("@/pages/admin/credential-detail"), "AdminCredentialDetailPage");
const AdminIntegrationsPage = lazily(() => import("@/pages/admin/integrations"), "AdminIntegrationsPage");
const AdminTelemetryPage = lazily(() => import("@/pages/admin/telemetry"), "AdminTelemetryPage");
const AdminDependenciesPage = lazily(() => import("@/pages/admin/dependencies"), "AdminDependenciesPage");
const AdminFeaturesPage = lazily(() => import("@/pages/admin/features"), "AdminFeaturesPage");
const AdminIntegrationLogPage = lazily(() => import("@/pages/admin/integration-log"), "AdminIntegrationLogPage");
const AdminApiKeysPage = lazily(() => import("@/pages/admin/api-keys"), "AdminApiKeysPage");
const AdminDeployKeysPage = lazily(() => import("@/pages/admin/deploy-keys"), "AdminDeployKeysPage");
const AdminEmailPage = lazily(() => import("@/pages/admin/email"), "AdminEmailPage");
const AdminPublisherRequestsPage = lazily(() => import("@/pages/admin/publisher-requests"), "AdminPublisherRequestsPage");
const AdminMessagesPage = lazily(() => import("@/pages/admin/messages"), "AdminMessagesPage");
const AdminAnnouncementComposePage = lazily(() => import("@/pages/admin/announcement-compose"), "AdminAnnouncementComposePage");
const AdminSourcesPage = lazily(() => import("@/pages/admin/sources"), "AdminSourcesPage");

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: false } },
});

function AuthProvider({ children }: { children: React.ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  // The display zone (ADR §154), set before the user is, so the first render
  // of a signed-in page already formats in it.
  const [zone, setZone] = useState<string | undefined>(undefined);

  const adopt = useCallback((u: User | null) => {
    const z = resolveDisplayTimeZone(u?.time_zone);
    setDisplayTimeZone(z);
    setZone(displayTimeZone());
    setUser(u);
  }, []);

  useEffect(() => {
    api.getMe()
      .then(adopt)
      .catch(() => adopt(null))
      .finally(() => setLoading(false));
  }, [adopt]);

  const refresh = useCallback(async () => {
    try {
      adopt(await api.getMe());
    } catch {
      // Keep what is on screen: a failed re-read is not a sign-out.
    }
  }, [adopt]);

  const logout = useCallback(async () => {
    await api.logout().catch(() => {});
    setUser(null);
    // `signed_out` holds the login page still. On an install with one
    // provider it otherwise forwards at once, and an identity provider whose
    // own session is still alive answers without a prompt — so signing out
    // signed you straight back in.
    window.location.href = "/login?signed_out=1";
  }, []);

  return (
    <AuthContext.Provider value={{ user, loading, logout, refresh }}>
      {/* Keyed on the zone: the formatters are plain functions read during
          render, so a changed zone has to re-render everything that printed
          a time. It changes on a saved choice, not on navigation; the query
          cache lives in the client and survives. */}
      <Fragment key={zone ?? "browser"}>{children}</Fragment>
    </AuthContext.Provider>
  );
}

/**
 * The one wait every entry to the app makes: do we have a session yet.
 *
 * It says so. A spinning ring and nothing else is a screen that announces
 * nothing at all — on the one screen where the person has no other cue, before
 * any page has rendered, and where the answer might be a redirect to a sign-in
 * provider they are about to be sent to.
 */
function AuthGateSpinner({ label = "Checking your session…" }: { label?: string }) {
  return (
    <div
      role="status"
      aria-live="polite"
      className="flex h-screen flex-col items-center justify-center gap-3 bg-background"
    >
      <div
        aria-hidden="true"
        className="h-6 w-6 animate-spin rounded-full border-2 border-muted-foreground border-t-primary"
      />
      <p className="text-sm text-muted-foreground">{label}</p>
    </div>
  );
}

/**
 * `/` has two answers, and this element is the only place that chooses.
 *
 * Signed in it is the dashboard, at the address it has always had; anonymous it
 * is the landing page, rendered here rather than redirected to, so there is no
 * second address to keep in step and no frame in which the wrong page shows.
 * The decision resolves inside the auth check the app already waits on, so it
 * costs no round trip of its own.
 *
 * `ProtectedRoute` is untouched by this and no longer sees `/` at all — which
 * is why the `?redirect=` rule below still reads as it did: every path except
 * this one still carries where the visitor was going.
 */
function RootRoute() {
  const { user, loading } = useAuth();

  if (loading) return <AuthGateSpinner />;
  if (user) return <ShellRoute />;

  // Its own boundary, for the reason /login has one: on the one page an
  // anonymous visitor can reach, a white screen is indistinguishable from a
  // broken install.
  return (
    <ErrorBoundary>
      <Suspense fallback={<AuthGateSpinner label="Loading…" />}>
        <LandingPage />
      </Suspense>
    </ErrorBoundary>
  );
}

/**
 * The app shell, fetched once per session. The fallback is the auth gate's
 * own screen, so a first load reads as one wait rather than two different ones.
 */
function ShellRoute() {
  return (
    <Suspense fallback={<AuthGateSpinner label="Loading…" />}>
      <Layout />
    </Suspense>
  );
}

function ProtectedRoute({ children }: { children: React.ReactNode }) {
  const { user, loading } = useAuth();

  if (loading) return <AuthGateSpinner />;

  // Carry where they were trying to go. A link in a notification email is the
  // case that made this matter: an administrator following one is, by
  // definition, not signed in yet, and landing them on the dashboard means
  // hunting for the row the email was about. The server validates the target
  // against its origin allowlist, so a site-relative path is all that is sent.
  if (!user) {
    const target = window.location.pathname + window.location.search;
    const to =
      target === "/" ? "/login" : `/login?redirect=${encodeURIComponent(target)}`;
    return <Navigate to={to} replace />;
  }
  return <>{children}</>;
}

/**
 * An administrator's page, reached by somebody who is not one — almost always
 * through a pasted link. It says so in place, inside the Layout, rather than
 * redirecting to the apps list, where a silent bounce read as a broken link.
 */
function AdminRoute({ children }: { children: React.ReactNode }) {
  const { user } = useAuth();
  if (user?.system_role !== "system_admin") return <AdminOnlyPage />;
  return <>{children}</>;
}

function AdminOnlyPage() {
  useDocumentTitle("Administrators only");
  return (
    <EmptyState
      icon={<ShieldAlert className="h-6 w-6" />}
      title="This page is for administrators"
      description="Your account can’t open it. If you need what it shows, ask an administrator."
      action={
        <Link to="/" className={buttonVariants({ variant: "secondary", size: "sm" })}>
          Go to your apps
        </Link>
      }
    />
  );
}

export default function App() {
  // Deployed apps live on their own origin; fetch it before rendering links.
  useEffect(() => {
    loadAppUrlConfig();
  }, []);

  return (
    <QueryClientProvider client={queryClient}>
      <Toaster richColors position="bottom-right" />
      <AuthProvider>
        <BrowserRouter>
          {/* Above `Routes`, and that position is the whole point.
              `Layout` is mounted from two places — `RootRoute` for `/`, and the
              `ProtectedRoute` wrapper for everything else — so React treats
              them as different positions in the tree and unmounts one to mount
              the other. An announcer living inside `Layout` was therefore
              remounted on exactly the navigations the top nav makes, and its
              first-render guard, which exists to stay quiet on the page
              somebody has just loaded, swallowed them. Here it mounts once for
              the session and every navigation is a re-render rather than a
              remount. It also now covers the pages outside `Layout` entirely —
              login, the landing page, the CLI approval screen — which were
              announcing nothing at all. */}
          <RouteAnnouncer />
          <Routes>
            {/* The login page gets a boundary of its own rather than sitting
                under the Layout's: it is the one page an unauthenticated
                visitor can reach, and a blank screen there is a locked door. */}
            <Route
              path="/login"
              element={
                <ErrorBoundary>
                  <LoginPage />
                </ErrorBoundary>
              }
            />
            {/* `toft login`'s approval screen. Outside the Layout on purpose:
                it is one decision about one credential, and a nav bar invites
                the reader to be somewhere else. Protected, because the browser
                session *is* the authorization for the mint. */}
            <Route
              path="/cli"
              element={
                <ProtectedRoute>
                  <ErrorBoundary>
                    <CliSignInPage />
                  </ErrorBoundary>
                </ProtectedRoute>
              }
            />
            {/* `/` is its own branch, because it is the one address whose
                answer depends on who is asking. `RootRoute` renders the Layout
                for a signed-in visitor — so the dashboard below is inside the
                same shell as every other page — and the landing page for
                everybody else. Note it never reaches `ProtectedRoute`: an
                anonymous visitor to `/` is not redirected anywhere. */}
            <Route path="/" element={<RootRoute />}>
              <Route index element={<DashboardPage />} />
            </Route>
            <Route
              element={
                <ProtectedRoute>
                  <ShellRoute />
                </ProtectedRoute>
              }
            >
              <Route path="/apps/new" element={<NewAppPage />} />
              <Route path="/apps/:id" element={<AppDetailPage />} />
              <Route path="/apps/:id/:tab" element={<AppDetailPage />} />
              <Route path="/gallery" element={<GalleryPage />} />
              {/* rsconnect's post-deploy address, in Posit Connect's shape;
                  the app is named in the fragment (ADR §109). */}
              <Route path="/connect" element={<ConnectRedirectPage />} />
              {/* Storage — a store's objects, for the person a grant was made
                  for. No role gate: the gate is the grant and it is the
                  server's, which is also why an administrator sees nothing here
                  until they grant themselves. The second route's wildcard is
                  the folder path within the store. */}
              {/* Moved onto the page about the reader (ADR §52). The address
                  it had for a year still works, because a route that moves
                  keeps its old one working. */}
              <Route path="/storage" element={<Navigate to="/me/storage" replace />} />
              <Route path="/storage/:slug/*" element={<StoreBrowserPage />} />
              <Route path="/tags/:tagSlug" element={<TagBrowsePage />} />
              <Route path="/tags/:tagSlug/:valueSlug" element={<TagBrowsePage />} />
              {/* People — the user directory and the account records behind
                  it — is administrators only, matching the endpoints it reads. */}
              <Route path="/people" element={<AdminRoute><PeoplePage /></AdminRoute>} />
              <Route path="/people/:id" element={<AdminRoute><UserDetailPage /></AdminRoute>} />
              <Route path="/docs" element={<DocumentationPage />} />
              {/* The one page whose subject is the person reading it. `/me`
                  rather than `/account/…`: that names a settings tree and this
                  is not one — three of its sections are read-only and the
                  fourth is a credential inventory. It also matches the API
                  namespace these tabs read from. */}
              <Route path="/me" element={<MePage />} />
              <Route path="/me/:tab" element={<MePage />} />
              {/* API keys were a page of their own and are now a tab. The
                  redirect is not optional: that URL is in bookmarks and in the
                  avatar menu of every open tab. Same precedent as
                  /admin/job-runs and /admin/scheduled when Automation merged. */}
              <Route
                path="/account/api-keys"
                element={<Navigate to="/me/credentials" replace />}
              />
              {/* One Suspense for the whole admin half rather than one per
                  route: the boundary is where the chunk is fetched, and the
                  rail outside it stays on screen while it arrives. */}
              <Route
                path="/admin"
                element={
                  <AdminRoute>
                    <AdminLayout />
                  </AdminRoute>
                }
              >
                {/* `/admin` is the directory; Info is a destination in it
                    (ADR §50 D5). */}
                <Route index element={<AdminHomePage />} />
                <Route path="info" element={<AdminInfoPage />} />
                <Route path="settings" element={<AdminSettingsPage />} />
                <Route path="email" element={<AdminEmailPage />} />
                {/* Metrics and Usage were the same question in two units
                    (ADR §50 D7). The host gauges are on Processes, the per-app
                    figures are joined into Usage, and a bookmark still lands
                    somewhere. */}
                <Route path="metrics" element={<Navigate to="/admin/usage" replace />} />
                <Route path="usage" element={<AdminUsagePage />} />
                {/* Moved into System info and Processes (ADR §50); the old
                    addresses still land on the section. */}
                <Route path="estate" element={<Navigate to="/admin/info#languages" replace />} />
                <Route path="estate/targets" element={<AdminScanTargetsPage />} />
                <Route path="reach" element={<AdminReachPage />} />
                <Route path="compliance" element={<AdminCompliancePage />} />
                <Route path="storage" element={<AdminStoragePage />} />
                <Route path="storage/:slug" element={<AdminStorageDetailPage />} />
                <Route path="storage/:slug/files/*" element={<AdminStoreBrowserPage />} />
                <Route path="tags" element={<AdminTagsPage />} />
                <Route path="authentication" element={<AdminAuthenticationPage />} />
                <Route path="publisher-requests" element={<AdminPublisherRequestsPage />} />
                <Route path="audit" element={<AdminAuditPage />} />
                <Route path="checks" element={<AdminChecksPage />} />
                <Route path="queue" element={<Navigate to="/admin/processes#queue" replace />} />
                <Route path="automation" element={<AdminAutomationPage />} />
                {/* Job Runs and Scheduled Content were two pages until ADR §33
                    merged them; a bookmark, an old email or a link in a doc
                    still lands somewhere. */}
                <Route path="job-runs" element={<Navigate to="/admin/automation" replace />} />
                <Route path="scheduled" element={<Navigate to="/admin/automation" replace />} />
                <Route path="processes" element={<AdminProcessesPage />} />
                <Route path="credentials" element={<AdminCredentialsPage />} />
                <Route path="credentials/:id" element={<AdminCredentialDetailPage />} />
                <Route path="integrations" element={<AdminIntegrationsPage />} />
                <Route path="telemetry" element={<AdminTelemetryPage />} />
                <Route path="dependencies" element={<AdminDependenciesPage />} />
                <Route path="features" element={<AdminFeaturesPage />} />
                {/* Toft is free (ADR §32): there is no license to show, and an old
                    bookmark lands on what this build offers. */}
                <Route path="license" element={<Navigate to="/admin/features" replace />} />
                {/* Two sections of Settings that became pages (ADR §50 D2).
                    Old `/admin/settings#sources` and `#messages` links are
                    redirected by the Settings page itself. */}
                <Route path="messages" element={<AdminMessagesPage />} />
                {/* One announcement: a compose box while it is a draft, and a
                    record once it has been sent. */}
                <Route
                  path="messages/announcement/:id"
                  element={<AdminAnnouncementComposePage />}
                />
                <Route path="sources" element={<AdminSourcesPage />} />
            <Route path="integration-log" element={<AdminIntegrationLogPage />} />
                <Route path="api-keys" element={<AdminApiKeysPage />} />
                <Route path="deploy-keys" element={<AdminDeployKeysPage />} />
              </Route>
              {/* Inside the Layout, so a mistyped URL keeps its nav and its
                  search box — the two things somebody who has landed on the
                  wrong address actually wants. */}
              <Route path="*" element={<NotFoundPage />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </AuthProvider>
    </QueryClientProvider>
  );
}

import { Suspense } from "react";
import { Outlet, Link, useLocation } from "react-router-dom";
import { cn, mayPublish } from "@/lib/utils";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/use-auth";
import { api, statusOf } from "@/lib/api";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { UserAvatar } from "@/components/user-avatar";
import { Button } from "@/components/ui/button";
import { LogOut, Search, KeyRound, Menu, UserRound, ExternalLink } from "lucide-react";
import { ToftMark } from "@/components/toft-mark";
import { AnnouncementBanner } from "@/components/announcement-banner";
import { ConnectionBanner } from "@/components/connection-banner";
import { CommandPalette } from "@/components/command-palette";
import { StoreTransferTray } from "@/components/store-transfer-tray";
import { ErrorBoundary } from "@/components/error-boundary";
import { Skeleton } from "@/components/ui/skeleton";
import { useScrollRestoration } from "@/hooks/use-scroll-restoration";
import { DeployActivity } from "@/components/deploy-activity";
import { toneSurface } from "@/lib/tone";
import { appUrl } from "@/lib/app-url";

/**
 * `external` is the one entry that leaves the SPA — the install's own knowledge
 * base, which is a deployed app on the apps origin rather than a route here.
 *
 * It is a flag on the data rather than a branch in the desktop strip because
 * the same array draws the phone menu, and a nav item that exists on one width
 * and not the other is the bug that shape prevents.
 */
type NavItem = {
  label: string;
  href: string;
  match: (p: string) => boolean;
  gallery?: boolean;
  adminOnly?: boolean;
  /** Present only where there is something to read, or for the administrator
   *  who installs the first one. Same rule as `gallery`. */
  docs?: boolean;
  external?: true;
};

const navItems: NavItem[] = [
  { label: "Apps", href: "/", match: (p: string) => p === "/" || p.startsWith("/apps") },
  // The Gallery link is present only when the gallery is: the query below 404s
  // while it is off and for anyone who may not publish, and a link to a page
  // that explains it is not for you is worse than no link.
  { label: "Gallery", href: "/gallery", match: (p: string) => p.startsWith("/gallery"), gallery: true },
  // People is the user directory and the group admin; both are admin-only
  // server-side, so a link for anyone else led to an "administrators only"
  // page and nothing more.
  // Storage is not here: it is a tab on /me, because what one person reaches
  // is a fact about that person and the header is for what everybody has.
  { label: "People", href: "/people", match: (p: string) => p.startsWith("/people"), adminOnly: true },
  { label: "Admin", href: "/admin", match: (p: string) => p.startsWith("/admin"), adminOnly: true },
  // The Gallery rule applied to its neighbour rather than contradicted beside
  // it: on a fresh install the guides are not installed, so a non-admin
  // carried a permanent nav entry to a page that told them to go and ask
  // somebody else. An administrator keeps the link — they are the one who
  // installs the first guide.
  { label: "Documentation", href: "/docs", match: (p: string) => p.startsWith("/docs"), docs: true },
];

export function Layout() {
  const { user, logout } = useAuth();
  const location = useLocation();
  // A new route starts at the top and a Back returns to where it was — neither
  // of which a client-side navigation does on its own.
  useScrollRestoration();

  // The same query key the Gallery page uses, so this costs one request rather
  // than one per navigation.
  const { data: gallery } = useQuery({
    queryKey: ["gallery"],
    queryFn: api.getGallery,
    retry: false,
    staleTime: 5 * 60_000,
    // Only for somebody who may publish: the server refuses everybody else
    // (403), and asking anyway left that refusal on every viewer's page.
    enabled: mayPublish(user?.system_role),
  });

  /**
   * The install's own knowledge base, resolved for this person.
   *
   * The server answers `{}` when nothing is picked, when the picked app has
   * been deleted, and when this caller may not read it — the same bytes for all
   * three, so there is nothing to branch on here beyond "is there a slug".
   * That is the Gallery rule applied without modification: a link to something
   * that would explain it is not for you is worse than no link.
   */
  const { data: knowledgeBase } = useQuery({
    queryKey: ["knowledge-base"],
    queryFn: api.getKnowledgeBase,
    retry: false,
    staleTime: 5 * 60_000,
  });

  // The same key the Documentation page reads, so gating the link costs one
  // cached request rather than one per navigation.
  const { data: documentation } = useQuery({
    queryKey: ["documentation"],
    queryFn: api.listDocumentation,
    retry: false,
    staleTime: 5 * 60_000,
  });

  /**
   * Whether personal keys exist on this install, on the same query `/me` reads.
   *
   * The account menu offered "API keys" unconditionally, including on the
   * installs where the capability is off — which is the default — so the
   * most-used item in the menu led to a tab that is not there, and `/me`
   * resolved it back to Overview. *Absent, not dormant*: the routes 404 when
   * the capability is off, and that 404 is the only answer that means absent.
   * An unanswered read offers nothing, so the item cannot appear and then
   * vanish under the pointer.
   */
  const { data: myKeys, error: myKeysError } = useQuery({
    queryKey: ["my-api-keys"],
    queryFn: api.listMyApiKeys,
    retry: false,
    staleTime: 5 * 60_000,
  });
  const keysAvailable =
    myKeys !== undefined || (!!myKeysError && statusOf(myKeysError) !== 404);

  // How many installed extensions are behind the catalog. Administrators only,
  // because extensions are theirs alone — and derived from the same response
  // the page renders, so the badge cannot disagree with what is on it.
  const behind = (gallery?.items ?? []).filter((i) => i.update_available).length;

  const visibleNavItems: NavItem[] = [
    ...navItems.filter(
      (item) =>
        (!item.adminOnly || user?.system_role === "system_admin") &&
        (!item.gallery || gallery) &&
        // Everybody has documentation now — the guides are links — so the item
        // is absent only when the operator switched the links off and there is
        // no knowledge base either: the one state in which /docs is empty.
        // Rendered on the answer, never on !isLoading, so it cannot flash.
        (!item.docs ||
          (documentation !== undefined && (documentation.url !== null || !!knowledgeBase?.slug))),
    ),
    // Last, after Documentation. Two book-ish words adjacent is the honest cost
    // of this order, and it is the right one: the manual this binary ships
    // keeps its name and its place, and what was added is the organization's.
    ...(knowledgeBase?.slug
      ? [{
          label: knowledgeBase.label || "Knowledge base",
          href: appUrl(knowledgeBase.slug),
          match: () => false,
          external: true as const,
        }]
      : []),
  ];

  return (
    <div className="min-h-screen bg-background">
      {/* Every page begins with a wordmark, the nav links, a search box and an
          avatar. Somebody on a keyboard or a screen reader tabbed through all
          of it on every navigation to reach the thing they came for. Visible
          only on focus, which is the one time it is useful. */}
      <a
        href="#main"
        className="sr-only rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-[60]"
      >
        Skip to content
      </a>
      {/* One sticky container rather than two elements competing at z-50. The
          banner was rendered *above* the sticky header and was not itself
          sticky, so a `critical` announcement — the one level the code goes out
          of its way to make undismissible — was gone on the first flick of the
          wheel, while the header it sat above stayed pinned all session. */}
      <div className="sticky top-0 z-50">
        <ConnectionBanner />
        <AnnouncementBanner />
        <header className="border-b border-border/60 bg-card shadow-[0_1px_2px_0_rgba(0,0,0,0.03)]">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-4 px-4 sm:px-6 md:gap-8">
          <Link to="/" className="flex items-center gap-2 font-semibold tracking-tight text-foreground">
            <ToftMark className="h-5 w-5 text-primary" />
            <span>Toft</span>
          </Link>

          {/* The nav is a flat row of up to five links beside a wordmark, a
              search box and an avatar, in a bar with no wrapping — which
              overflows a phone. Below lg it becomes a menu; above it, nothing
              changes.

              It used to turn on at md, and at exactly 768 the row it joined
              was 868 pixels wide: the whole page scrolled sideways by a
              hundred, on every route, because the strip arrived before there
              was room for it. That is the same mistake the tables under it
              were making, and it is worth naming once — a breakpoint is a
              claim about how much space there is, and the claim has to be
              true at the width it is made. */}
          <nav aria-label="Main" className="hidden items-center gap-0.5 lg:flex">
            {visibleNavItems.map((item) => {
              const isActive = item.match(location.pathname);
              // The only entry in this row that is not a route. The glyph is
              // what makes the new tab honest rather than a surprise, and it
              // is the same pairing an app's own "Open" link uses.
              if (item.external) {
                return (
                  <a
                    key={item.href}
                    href={item.href}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="relative inline-flex items-center gap-1 px-3 py-4 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-ring"
                  >
                    {item.label}
                    <ExternalLink className="h-3 w-3 opacity-60" aria-hidden />
                  </a>
                );
              }
              return (
                <Link
                  key={item.href}
                  to={item.href}
                  // Which link is current was carried by colour and an
                  // underline bar — neither of which reaches anyone who cannot
                  // see them.
                  aria-current={isActive ? "page" : undefined}
                  className={`relative px-3 py-4 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-ring ${
                    isActive
                      ? "text-foreground"
                      : "text-muted-foreground hover:text-foreground"
                  }`}
                >
                  {item.label}
                  {item.gallery && behind > 0 && (
                    <span
                      className={`ml-1.5 inline-flex h-4 min-w-4 items-center justify-center rounded-full border px-1 text-2xs font-semibold ${toneSurface.attention}`}
                      title={`${behind} installed extension${behind === 1 ? " has" : "s have"} an update available`}
                    >
                      {behind}
                      <span className="sr-only"> — {behind} update{behind === 1 ? "" : "s"} available</span>
                    </span>
                  )}
                  {isActive && (
                    <span className="absolute inset-x-3 -bottom-px h-0.5 rounded-full bg-primary" />
                  )}
                </Link>
              );
            })}
          </nav>

          <div className="ml-auto flex items-center gap-2 sm:gap-3">
            {/* Absent unless something is deploying — see DeployActivity. */}
            <DeployActivity />
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button variant="ghost" size="icon-sm" aria-label="Navigation" className="lg:hidden" />
                }
              >
                <Menu className="h-4 w-4" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-48">
                {/* The current page is marked here too. The desktop strip has
                    said which page you are on since it existed; the phone menu
                    was a list of nine identical rows, which is the one width
                    where the header cannot say it either. */}
                {visibleNavItems.map((item) => item.external ? (
                  <DropdownMenuItem
                    key={item.href}
                    // Named on the anchor itself: the row's text is supplied
                    // by DropdownMenuItem's children, which this element does
                    // not statically carry.
                    render={<a href={item.href} target="_blank" rel="noopener noreferrer" aria-label={item.label} />}
                    className="cursor-pointer"
                  >
                    {item.label}
                    <ExternalLink className="ml-auto h-3 w-3 opacity-60" aria-hidden />
                  </DropdownMenuItem>
                ) : (
                  <DropdownMenuItem
                    key={item.href}
                    render={<Link to={item.href} />}
                    aria-current={item.match(location.pathname) ? "page" : undefined}
                    // The accent fill the admin nav uses for "you are here"; a
                    // weight change alone was invisible at a glance.
                    className={cn(
                      "cursor-pointer",
                      item.match(location.pathname) && "bg-accent font-medium text-accent-foreground",
                    )}
                  >
                    {item.label}
                    {/* The same chip the desktop strip draws, not bare amber
                        digits. */}
                    {item.gallery && behind > 0 && (
                      <span
                        className={`ml-auto inline-flex h-4 min-w-4 items-center justify-center rounded-full border px-1 text-2xs font-semibold ${toneSurface.attention}`}
                      >
                        {behind}
                        <span className="sr-only"> — {behind} update{behind === 1 ? "" : "s"} available</span>
                      </span>
                    )}
                  </DropdownMenuItem>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>
            <button
              type="button"
              onClick={() => window.dispatchEvent(new CustomEvent("open-command-palette"))}
              className="flex items-center gap-2 rounded-md border border-border/60 bg-muted/40 px-2.5 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              // The visible word leads the name, so "click Search" reaches it.
              aria-label={`Search (${/mac/i.test(navigator.platform) ? "⌘K" : "Ctrl K"})`}
            >
              <Search className="h-3.5 w-3.5" />
              <span className="hidden sm:inline">Search</span>
              <kbd className="hidden rounded border border-border bg-card px-1 font-mono text-2xs sm:inline">
                {/mac/i.test(navigator.platform) ? "⌘K" : "Ctrl K"}
              </kbd>
            </button>
            <DropdownMenu>
              {/* Named, because an avatar is two initials and a colour: to a
                  screen reader the account menu was a button with no name at
                  all, and it is the only route to Sign out. */}
              <DropdownMenuTrigger
                aria-label={user?.login ? `Account menu for ${user.login}` : "Account menu"}
                className="rounded-full outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              >
                <UserAvatar id={user?.id} login={user?.login} name={user?.name} avatarUrl={user?.avatar_url} size="md" className="cursor-pointer" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-48">
                <div className="px-2 py-1.5">
                  <p className="text-sm font-medium">{user?.login}</p>
                  <p className="text-xs text-muted-foreground">{user?.email}</p>
                </div>
                <DropdownMenuSeparator />
                <DropdownMenuItem render={<Link to="/me" />} className="cursor-pointer">
                  <UserRound className="mr-2 h-4 w-4" />
                  Your account
                </DropdownMenuItem>
                {/* Credentials keeps its own entry rather than being buried a tab
                    deep, under the tab's own name. It is the most-used item in this menu, and demoting it
                    to make room for its own parent is a regression dressed as
                    tidying. Present only where the tab is: see above. */}
                {keysAvailable && (
                  <DropdownMenuItem
                    render={<Link to="/me/credentials" />}
                    className="cursor-pointer"
                  >
                    <KeyRound className="mr-2 h-4 w-4" />
                    Credentials
                  </DropdownMenuItem>
                )}
                <DropdownMenuSeparator />
                {/* A plain item: signing out is the most reversible act in the
                    product, and red in a menu is the shape kept for delete. */}
                <DropdownMenuItem onClick={logout} className="cursor-pointer">
                  <LogOut className="mr-2 h-4 w-4" />
                  Sign out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </div>
        </header>
      </div>

      {/* Inside <main>, not around it: a page that throws keeps the header and
          the nav, so the way out of a broken page is a click rather than a
          typed URL.

          The admin console takes a wider cap, and the reason is arithmetic
          rather than taste: it is the only section that spends 232 pixels of
          the row on a rail, so under the shared `max-w-7xl` its content column
          settled at 998 and stopped there — on a 1,920 monitor as surely as on
          a 1,280 laptop. Every table in the section was therefore deciding
          what it could afford against a number that had already stopped
          moving, which is why they scrolled sideways on screens with room to
          spare. The rail is chrome, not content; it is paid for out of the
          margin, so that an admin page's content column is the same 1,232
          measure every other page's is. The header keeps `max-w-7xl` on
          purpose (ADR §86): widening it here would slide the nav 80px under
          the pointer on the click into or out of Admin. */}
      <main
        id="main"
        className={cn(
          "mx-auto px-4 py-6 sm:px-6 sm:py-8",
          location.pathname.startsWith("/admin") ? "max-w-[94.5rem]" : "max-w-7xl",
        )}
      >
        <ErrorBoundary>
          {/* The pages are lazy (App.tsx, ADR §124); the nav stays on screen
              while one arrives, as the admin rail does in AdminLayout. */}
          <Suspense
            fallback={
              <div className="space-y-4">
                <Skeleton className="h-8 w-64" />
                <Skeleton className="h-64 w-full" />
              </div>
            }
          >
            <Outlet />
          </Suspense>
        </ErrorBoundary>
      </main>
      {/* These four are outside `<main>` and were outside the boundary with it,
          which is the case the boundary exists for turned inside out: a throw
          in the command palette or the transfer tray blanked the *whole* app,
          nav and all, from a component that is not even the page. Their own
          boundary, keyed on nothing, because none of them is a route and a
          reset would only re-throw. */}
      <ErrorBoundary>
        <CommandPalette />
        {/* Above every route rather than on the storage page, because a
            transfer does not stop when somebody navigates away from the page
            that started it — and a tray that unmounted would be a cancel button
            wearing a different word. It renders nothing when nothing is
            transferring. */}
        <StoreTransferTray />
      </ErrorBoundary>
    </div>
  );
}

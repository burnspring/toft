import { Suspense, useEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";
import { Outlet, Link, useLocation, useNavigate } from "react-router-dom";
import {
  Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { useAdminGroups } from "@/hooks/use-admin-groups";
import { Skeleton } from "@/components/ui/skeleton";


export function AdminLayout() {
  const location = useLocation();
  const navigate = useNavigate();
  // The sidebar's entries only: an item with `sidebar: false` is reached by
  // name from ⌘K and by the link on the page that holds it.
  const adminGroups = useAdminGroups().map((g) => ({
    ...g,
    items: g.items.filter((i) => i.sidebar !== false),
  }));

  function matches(href: string) {
    if (href === "/admin") return location.pathname === "/admin";
    return location.pathname.startsWith(href);
  }

  const items = adminGroups.flatMap((g) => g.items);
  // **Longest match wins, and only one item is ever current.** Two destinations
  // can share a prefix — `/admin/estate` and `/admin/estate/targets` — and a
  // plain `startsWith` lights both, which puts `aria-current="page"` on two
  // links and tells a screen reader the reader is in two places. The mobile
  // select already resolved this the same way; the sidebar did not, and it
  // stayed invisible only because no two entries overlapped until now.
  const active = items
    .filter((i) => matches(i.href))
    .sort((a, b) => b.href.length - a.href.length)[0];
  const isActive = (href: string) => href === active?.href;
  // Only the page itself, not a detail route under it (a credential, a storage
  // resource), which carries its own tables.
  const formPage = active?.measure === "form" && location.pathname === active.href;
  const current = active ?? items[0];

  const railRef = useRef<HTMLDivElement>(null);
  const [moreBelow, setMoreBelow] = useState(false);
  function measureRail() {
    const rail = railRef.current;
    if (!rail) return;
    setMoreBelow(rail.scrollTop + rail.clientHeight < rail.scrollHeight - 1);
  }
  // The rail's height moves with the window and with the groups (which arrive
  // with a query), so "is there more below" is re-asked on either.
  useEffect(() => {
    const rail = railRef.current;
    if (!rail) return;
    const measure = () => setMoreBelow(rail.scrollTop + rail.clientHeight < rail.scrollHeight - 1);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(rail);
    for (const child of rail.children) ro.observe(child);
    return () => ro.disconnect();
  }, [adminGroups.length]);
  // Landing on a destination below the fold — /admin/audit on a laptop — must
  // show it highlighted. Scrolled by hand rather than `scrollIntoView`, which
  // would move the document as well when the rail is not the only scroller.
  const activeHref = active?.href;
  useEffect(() => {
    const rail = railRef.current;
    const link = rail?.querySelector<HTMLElement>('a[aria-current="page"]');
    if (!rail || !link) return;
    const top = link.getBoundingClientRect().top - rail.getBoundingClientRect().top + rail.scrollTop;
    if (top < rail.scrollTop) rail.scrollTop = top - 8;
    else if (top + link.offsetHeight > rail.scrollTop + rail.clientHeight)
      rail.scrollTop = top + link.offsetHeight - rail.clientHeight + 8;
  }, [activeHref]);

  return (
    <div className="flex flex-col gap-6 md:flex-row md:gap-10">
      {/* Twenty-seven destinations is twenty-seven tab stops between the skip
          link and the first control on the page. Scoped to the rail's own
          breakpoint, since below md the rail is a single Select and the extra
          stop buys nothing. */}
      <a
        href="#admin-content"
        className="sr-only rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground focus:not-sr-only focus:mb-2 focus:inline-block md:focus:inline-block"
      >
        Skip to page content
      </a>
      {/* Twenty-seven destinations. As a sidebar that is a scannable column; as
          `flex-wrap` on a phone it was four rows of undifferentiated chips
          filling the screen before any of the page a person came for. Below md
          it is a select — one control, the current page showing, every
          destination one tap away and none of them in the way. */}
      <div className="md:hidden">
        <Select value={current.href} onValueChange={(v) => navigate(v as string)}>
          <SelectTrigger className="w-full" aria-label="Admin section">
            <SelectValue>{(v) => items.find((i) => i.href === v)?.label ?? "Admin"}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {adminGroups.map((group) => (
              <SelectGroup key={group.label}>
                <SelectLabel>{group.label}</SelectLabel>
                {group.items.map((item) => (
                  <SelectItem key={item.href} value={item.href}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectGroup>
            ))}
          </SelectContent>
        </Select>
      </div>

      <nav aria-label="Admin sections" className="hidden shrink-0 md:block md:w-48">
        {/* **Pinned, and its own scroll container.** A sticky element taller
            than the viewport does not travel with the page: it pins at `top`
            and its overflow is revealed only when the page's foot arrives, so
            on a laptop a third of the destinations sat out of reach until the
            bottom of a long page (and every short page grew to the rail's
            height). Bounded to the viewport and scrolling itself instead — and
            saying so, which an earlier bounded rail did not: a thin visible
            scrollbar, a fade at the foot while there is more below, and the
            current page scrolled into view on every navigation (ADR §94). */}
        {/* **A group's name and a destination must not be told apart by letter
            case alone.** They had the same muted colour and the same left
            edge, so the name read as a smaller link. Now four things separate
            them: a destination is full-contrast text with an icon, the name is
            muted with none, a rule opens each group after the first, and the
            name sits closer to its own items than to the group above. */}
        <div
          ref={railRef}
          onScroll={measureRail}
          className="flex flex-col gap-2.5 overscroll-contain [scrollbar-width:thin] md:sticky md:top-20 md:max-h-[calc(100vh-8rem)] md:overflow-y-auto"
        >
          {adminGroups.map((group, i) => (
            <div key={group.label} className={i > 0 ? "border-t border-border/60 pt-2.5" : undefined}>
              <p className="mb-0.5 px-2.5 text-xs font-medium text-muted-foreground">
                {group.label}
              </p>
              <ul className="flex flex-col">
                {group.items.map((item) => {
                  const Icon = item.icon;
                  const current = isActive(item.href);
                  return (
                    <li key={item.href}>
                      <Link
                        to={item.href}
                        aria-current={current ? "page" : undefined}
                        className={`flex h-7 items-center gap-2 rounded-md px-2.5 text-sm transition-colors ${
                          current
                            ? "bg-accent font-medium text-accent-foreground"
                            : "text-foreground hover:bg-muted/60"
                        }`}
                      >
                        <Icon
                          aria-hidden
                          className={`h-4 w-4 shrink-0 ${current ? "text-primary" : "text-muted-foreground"}`}
                        />
                        {item.label}
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
          {moreBelow && (
            <div
              aria-hidden
              className="pointer-events-none sticky bottom-0 -mt-10 h-8 shrink-0 bg-linear-to-t from-background to-transparent"
            />
          )}
        </div>
      </nav>
      <div
        id="admin-content"
        tabIndex={-1}
        className={cn("min-w-0 flex-1", formPage && "max-w-3xl")}
      >
        {/* The admin pages are lazy (see App.tsx), so the chunk is fetched
            here — with the rail still on screen beside it, which is why the
            boundary is inside the layout rather than around it. */}
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
      </div>
    </div>
  );
}

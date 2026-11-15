import { useEffect, useMemo, useState, type ReactNode } from "react";
import { mayPublish } from "@/lib/utils";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AppWindow, BookMarked, BookOpen, ExternalLink, HardDrive, LayoutGrid, Plus, Rocket, SlidersHorizontal, UserRound, Users } from "lucide-react";
import { isBatchFramework } from "@/lib/frameworks";
import {
  Command, CommandDialog, CommandGroup, CommandInput, CommandItem, CommandList, CommandSeparator,
} from "@/components/ui/command";
import { api } from "@/lib/api";
import { appUrl } from "@/lib/app-url";
import { useAdminGroups } from "@/hooks/use-admin-groups";
import { scoreCommand } from "@/lib/command-score";
import {
  APP_SETTINGS_INDEX,
  SETTINGS_INDEX,
  appSettingHref,
  settingHref,
  settingPageLabel,
  settingText,
} from "@/lib/settings-index";
import { recentAppIds } from "@/lib/recents";
import { useAuth } from "@/hooks/use-auth";
import type { AppWithMetrics } from "@/lib/api";

/** Text entry has the keyboard: a bare-key shortcut must not steal from it. */
function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
}

/**
 * One row of the palette, as data.
 *
 * Built as a list rather than written as JSX because the ranking has to see
 * every row at once: `cmdk` scores *within a group* and renders groups in DOM
 * order, so before this an entry in a later group could never outrank one in
 * an earlier group however much better it matched. See `lib/command-score.ts`
 * for what that cost.
 */
interface PaletteItem {
  key: string;
  /** The heading this row is filed under while browsing, and the muted label
   *  beside it while searching. */
  group: string;
  label: string;
  /** The muted text at the end of the row, where the row has something to add
   *  beyond its group — an app's slug, a setting's page. */
  hint?: ReactNode;
  /** The same, as a string the search can see. `hint` is a node so it can
   *  carry markup; the scorer needs the words. */
  hintText?: string;
  icon: ReactNode;
  /** Everything the search should match beyond the label. */
  keywords?: string;
  select: () => void;
}

const ICON = "mr-2 h-4 w-4 text-muted-foreground";

/** Global ⌘K / "/" palette: jump to pages, apps, and common actions. */
export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const navigate = useNavigate();
  const { user } = useAuth();
  const isAdmin = user?.system_role === "system_admin";
  const adminGroups = useAdminGroups(isAdmin);

  const { data: apps = [] } = useQuery<AppWithMetrics[]>({
    queryKey: ["apps"],
    queryFn: () => api.listApps(),
    enabled: open,
    staleTime: 30_000,
  });

  // Shares the layout's query key, so opening the palette costs nothing extra.
  // Undefined means the gallery is not part of this install — off, or the
  // viewer may not publish — and the entry is then absent rather than dead.
  const { data: gallery } = useQuery({
    queryKey: ["gallery"],
    queryFn: api.getGallery,
    retry: false,
    staleTime: 5 * 60_000,
    // Only for somebody who may publish: the server refuses everybody else
    // (403), and asking anyway left that refusal on every viewer's page.
    enabled: mayPublish(user?.system_role),
  });

  // The same signal /me uses to decide whether the Storage tab exists at all.
  // Same key, so a session that has opened /me pays nothing; `enabled` matches
  // the palette's own policy for the reads it only needs while open.
  const { data: storeData, error: storesError } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
    enabled: open,
    staleTime: 5 * 60_000,
  });
  const stores = storesError ? [] : (storeData?.stores ?? []);

  // The same key and the same gate as the header's Documentation link (the
  // gate itself is below, once the knowledge base is read): a palette entry
  // the nav has removed is a page offered to somebody it says is not there.
  const { data: documentation } = useQuery({
    queryKey: ["documentation"],
    queryFn: api.listDocumentation,
    retry: false,
    staleTime: 5 * 60_000,
  });

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((o) => !o);
        return;
      }
      // Bare "/" opens the palette, but never while the user is typing —
      // it's a legal character in every text field on the site.
      if (e.key === "/" && !e.metaKey && !e.ctrlKey && !e.altKey && !isTypingTarget(e.target)) {
        e.preventDefault();
        setOpen(true);
      }
    }
    function onOpenEvent() {
      setOpen(true);
    }
    document.addEventListener("keydown", onKeyDown);
    window.addEventListener("open-command-palette", onOpenEvent);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("open-command-palette", onOpenEvent);
    };
  }, []);

  // Shared with the header on the key it already holds, so opening the palette
  // costs no request. `{}` when nothing is picked, when the app is gone, and
  // when this person may not read it.
  const { data: knowledgeBase } = useQuery({
    queryKey: ["knowledge-base"],
    queryFn: api.getKnowledgeBase,
    retry: false,
    staleTime: 5 * 60_000,
  });
  const docsAvailable =
    documentation !== undefined && (documentation.url !== null || !!knowledgeBase?.slug);

  // Read once per open rather than on every render: this is the one list whose
  // order changes underneath the component while it is on screen.
  const recentIds = open ? recentAppIds() : [];
  const recent = recentIds
    .map((id) => apps.find((a) => a.id === id))
    .filter((a): a is AppWithMetrics => !!a);

  function run(action: () => void) {
    setOpen(false);
    setQuery("");
    action();
  }

  const items: PaletteItem[] = [];

  // Above everything while browsing, because reopening what you just had is
  // the most common thing anybody does here. Absent until there is a history —
  // an empty "Recent" heading teaches nothing.
  for (const app of recent) {
    items.push({
      key: `recent-${app.id}`,
      group: "Recent",
      label: app.name,
      // Mono and bare, as a slug is everywhere else in the product.
      hint: <span className="font-mono">{app.slug}</span>,
      hintText: app.slug,
      icon: <Rocket className={ICON} />,
      keywords: `recent ${app.slug}`,
      select: () => navigate(`/apps/${app.id}`),
    });
  }

  for (const app of apps) {
    items.push({
      key: `app-${app.id}`,
      group: "Apps",
      label: app.name,
      // Mono and bare, as a slug is everywhere else in the product.
      hint: <span className="font-mono">{app.slug}</span>,
      hintText: app.slug,
      icon: <Rocket className={ICON} />,
      keywords: app.slug,
      select: () => navigate(`/apps/${app.id}`),
    });
  }

  items.push({
    key: "new-app",
    group: "Actions",
    label: "New app",
    icon: <Plus className={ICON} />,
    select: () => navigate("/apps/new"),
  });
  // The most recently deployed app this reader can actually open, by name.
  // `apps[0]` was whatever order the endpoint returned, and could be a batch
  // app (no pages) or a list-rung row (opens to a refusal).
  const latest = apps
    .filter((a) => a.can_open && !isBatchFramework(a.framework) && a.last_deployed_at)
    .sort((a, b) => Date.parse(b.last_deployed_at!) - Date.parse(a.last_deployed_at!))[0];
  if (latest) {
    items.push({
      key: "open-latest",
      group: "Actions",
      label: `Open ${latest.name}`,
      hint: "latest deploy",
      hintText: "latest deploy",
      icon: <ExternalLink className={ICON} />,
      keywords: "open latest app",
      select: () => window.open(appUrl(latest.slug), "_blank", "noopener,noreferrer"),
    });
  }

  items.push({
    key: "go-apps",
    group: "Go to",
    label: "Apps",
    icon: <AppWindow className={ICON} />,
    select: () => navigate("/"),
  });
  // Absent while the gallery is off, exactly as the nav item is — the palette
  // must not be a second way to reach a page that is not part of this install.
  if (gallery) {
    items.push({
      key: "go-gallery",
      group: "Go to",
      label: "Gallery",
      icon: <LayoutGrid className={ICON} />,
      select: () => navigate("/gallery"),
    });
  }
  // People only. Admin and Activity used to be hand-written here too, and the
  // generated block below already renders both from `adminGroups` — so typing
  // "activity" returned two identical rows and "admin" two labels for one URL.
  // People is not an admin destination and is not in that list, which is why it
  // stays.
  if (isAdmin) {
    items.push({
      key: "go-people",
      group: "Go to",
      label: "People",
      icon: <Users className={ICON} />,
      select: () => navigate("/people"),
    });
  }
  // Storage is no longer in the header at all — it is a tab on the page about
  // the reader — so this is now the shortest way to it for somebody who holds a
  // grant, rather than a convenience beside a link.
  //
  // Gated the way Gallery above it is, and for the same reason: /me resolves an
  // unavailable tab to Overview without rewriting the address, so an
  // unconditional row was a dead end for anybody holding no grant. And named
  // "Your storage", because the bare word belongs to the admin inventory — two
  // rows labelled Storage going to two different subjects is the collision.
  if (stores.length > 0) {
    items.push({
      key: "go-storage",
      group: "Go to",
      label: "Your storage",
      icon: <HardDrive className={ICON} />,
      keywords: "storage stores files buckets uploads folders you reach",
      select: () => navigate("/me/storage"),
    });
  }
  items.push({
    key: "go-me",
    group: "Go to",
    label: "Your account",
    icon: <UserRound className={ICON} />,
    keywords: "me account profile access credentials security",
    select: () => navigate("/me"),
  });
  if (docsAvailable) {
    items.push({
      key: "go-docs",
      group: "Go to",
      label: "Documentation",
      icon: <BookOpen className={ICON} />,
      // The word people type. It was labelled "Docs" and carried nothing else,
      // so "documentation" — the name of the page, the nav item and the route
      // — scored zero against it.
      keywords: "docs documentation guides manual help reference",
      select: () => navigate("/docs"),
    });
  }
  // The install's own knowledge base, when it has one and this person can open
  // it. The only item in this palette that leaves the SPA, which is why it
  // opens a window rather than navigating.
  if (knowledgeBase?.slug) {
    const slug = knowledgeBase.slug;
    items.push({
      key: "go-kb",
      group: "Go to",
      label: knowledgeBase.label || "Knowledge base",
      icon: <BookMarked className={ICON} />,
      keywords: "knowledge base handbook guides",
      select: () => window.open(appUrl(slug), "_blank", "noopener,noreferrer"),
    });
  }

  // All admin destinations, from the same list the sidebar renders. Three were
  // hand-copied here before, so the palette could reach Activity but not
  // Telemetry, Storage or Automation.
  if (isAdmin) {
    for (const group of adminGroups) {
      for (const item of group.items) {
        const Icon = item.icon;
        items.push({
          key: `admin-${item.href}`,
          group: `Admin · ${group.label}`,
          label: item.label,
          icon: <Icon className={ICON} />,
          keywords: `admin ${group.label}`,
          select: () => navigate(item.href),
        });
      }
    }
  }

  // Every setting on this install, by the name an administrator knows it by,
  // wherever it is rendered. The palette reached twenty-one pages and zero
  // settings before the index was promoted out of the Settings page: "smtp"
  // found nothing, on an install with an SMTP host. Each one navigates to
  // `route#anchor`, which lands on the setting rather than on the page.
  if (isAdmin) {
    for (const entry of SETTINGS_INDEX) {
      items.push({
        key: `setting-${entry.id}`,
        group: "Settings",
        label: entry.label,
        hint: settingPageLabel(entry.route),
        hintText: settingPageLabel(entry.route),
        icon: <SlidersHorizontal className={ICON} />,
        keywords: `setting ${settingText(entry)}`,
        select: () => navigate(settingHref(entry)),
      });
    }
  }

  // An app's own settings, for the apps already in scope — the one you are
  // looking at, and the ones you looked at before it. Every setting × every app
  // is a cross product that would drown every other group here; *these* apps'
  // settings by name is the useful half and the cheap one, and it needs no
  // request of its own because `recent` is already resolved against the apps
  // query above.
  //
  // Not gated on the administrator role, unlike the install-wide group: these
  // are an owner's settings on their own app, and the page they land on
  // authorizes for itself.
  for (const app of recent) {
    for (const entry of APP_SETTINGS_INDEX) {
      items.push({
        key: `app-setting-${app.id}-${entry.id}`,
        group: `${app.name} · Settings`,
        label: entry.label,
        // The app's name, not the tab's: with several apps in Recents,
        // "Auto-deploy" three times over is three identical rows, and which app
        // it is is the thing that tells them apart.
        hint: app.name,
        hintText: app.name,
        icon: <SlidersHorizontal className={ICON} />,
        keywords: `setting ${app.name} ${app.slug} ${settingText(entry)}`,
        select: () => navigate(appSettingHref(app.id, entry)),
      });
    }
  }

  /**
   * The whole list ranked, when there is something to rank it by.
   *
   * Empty query keeps the groups: browsing wants Recent at the top and the
   * admin console in its four named sections, and there is nothing to score.
   * The moment somebody types, one order is the right order and it is by how
   * well each row answers — the group name comes with the row as a label.
   */
  const ranked = useMemo(() => {
    const q = query.trim();
    if (!q) return null;
    return items
      .map((item) => ({
        item,
        score: scoreCommand(item.label, `${item.group} ${item.hintText ?? ""} ${item.keywords ?? ""}`, q),
      }))
      .filter((r) => r.score > 0)
      .sort((a, b) => b.score - a.score)
      .map((r) => r.item);
    // `items` is rebuilt every render from queries and props; depending on it
    // would rank on every keystroke *and* every poll. The inputs that can
    // change the answer are the query and the data behind the rows.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query, apps, recent.length, gallery, stores.length, docsAvailable, knowledgeBase, isAdmin]);

  const grouped = groupsOf(items);

  /** One row. `group` is passed while searching, where the heading it would
   *  have sat under has to travel with the row instead — and is dropped when
   *  it would only repeat the hint, which is what "Settings Settings" was. */
  function row(item: PaletteItem, group?: string) {
    const tail = group && group !== item.hintText ? group : undefined;
    return (
      <CommandItem key={item.key} value={item.key} onSelect={() => run(item.select)}>
        {item.icon}
        {/* The identity grows and the group sits on the right edge. Not
            `ml-auto` on the group: `CommandItem` already ends in a check icon
            carrying one, and two auto margins share the slack between them —
            which put the group label at a different x on every row.
            **Both halves truncate.** An app whose name is one unbreakable
            fifty-character token pushed the slug across the group label; a
            flex item's implicit minimum is its content, so `min-w-0` on each
            is what lets either give way. */}
        <span className="flex min-w-0 flex-1 items-center gap-2">
          <span className="min-w-0 truncate">{item.label}</span>
          {item.hint && (
            <span className="min-w-0 truncate text-xs text-muted-foreground">{item.hint}</span>
          )}
        </span>
        {tail && <span className="shrink-0 pl-3 text-xs text-muted-foreground">{tail}</span>}
      </CommandItem>
    );
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={(o) => { setOpen(o); if (!o) setQuery(""); }}
      title="Command palette"
      description="Jump to an app or page"
      // The product's fastest way anywhere, so not the 384px confirm-dialog
      // default: wide enough for an app and its hint on one line, tall enough
      // that the Actions group is not cut at the fold, and raised so the
      // taller list still clears the foot of a laptop screen.
      className="top-[15vh] sm:max-w-xl"
    >
      {/* `shouldFilter={false}`: the filtering and the ordering are both above,
          across every group at once, which is the thing cmdk's own scoring
          cannot do. It keeps the keyboard, the roving focus and the roles. */}
      <Command shouldFilter={false}>
        <CommandInput
          placeholder="Search apps and pages…"
          value={query}
          onValueChange={setQuery}
        />
        <CommandList className="max-h-[min(60vh,28rem)]">
          {ranked ? (
            ranked.length === 0 ? (
              // Ours, not `CommandEmpty`: that one renders on cmdk's own count
              // of matching items, and cmdk is no longer doing the matching.
              <p className="py-6 text-center text-sm">No results found.</p>
            ) : (
              <CommandGroup>{ranked.map((item) => row(item, item.group))}</CommandGroup>
            )
          ) : (
            grouped.map(({ group, rows }, i) => (
              <div key={group}>
                {i > 0 && <CommandSeparator />}
                <CommandGroup heading={group}>{rows.map((item) => row(item))}</CommandGroup>
              </div>
            ))
          )}
        </CommandList>
        <div
          aria-hidden
          className="flex items-center gap-3 border-t border-border/60 px-3 py-2 text-2xs text-muted-foreground"
        >
          <span><Kbd>↑</Kbd> <Kbd>↓</Kbd> to move</span>
          <span><Kbd>↵</Kbd> to open</span>
          <span><Kbd>esc</Kbd> to close</span>
        </div>
      </Command>
    </CommandDialog>
  );
}

/** A key, in the header search button's chip. */
function Kbd({ children }: { children: string }) {
  return (
    <kbd className="rounded border border-border bg-card px-1 font-mono text-2xs">{children}</kbd>
  );
}

/** The items in their headings, in first-seen order — which is the order they
 *  were pushed in, and the order this palette has always browsed in. */
function groupsOf(items: PaletteItem[]): { group: string; rows: PaletteItem[] }[] {
  const out: { group: string; rows: PaletteItem[] }[] = [];
  for (const item of items) {
    const last = out[out.length - 1];
    if (last && last.group === item.group) last.rows.push(item);
    else {
      const existing = out.find((g) => g.group === item.group);
      if (existing) existing.rows.push(item);
      else out.push({ group: item.group, rows: [item] });
    }
  }
  return out;
}

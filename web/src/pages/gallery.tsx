import { useState } from "react";
import { useUrlParam, useUrlParams } from "@/hooks/use-list-state";
import { RelativeTime } from "@/components/relative-time";
import { toneText } from "@/lib/tone";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpCircle, CheckCircle2, Code2, ExternalLink, LayoutGrid, RefreshCw } from "lucide-react";
import { toast } from "sonner";

import { api, statusOf, type GalleryItem, type GalleryResponse, type GallerySync } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { TableCard } from "@/components/table-card";
import { AppNameFields } from "@/components/app-name-fields";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { FrameworkMark } from "@/components/framework-mark";
import { AppIcon } from "@/components/app-icon";
import { GALLERY_ICONS } from "@/lib/gallery-icons";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { FacetChips, FacetFilter } from "@/components/facet-filter";
import { FilterBar } from "@/components/filter-bar";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { Notice } from "@/components/notice";
import { FieldHelp } from "@/components/field-help";

/**
 * The Gallery.
 *
 * Two kinds of entry, and the page never lets them blur. An **extension** is
 * administrator-only, comes from a repository, and can be updated in place when
 * the catalog names a newer version. An **example** is a snapshot anyone who may
 * publish can take, and is never updated — which is why no card ever offers one
 * an update, and why the copy calls it a copy.
 *
 * The four degraded states are distinguished deliberately: no items match the
 * filters; the catalog is empty; the catalog could not be fetched (with its
 * age); and the catalog needs a newer Toft. "Nothing here" and "we could
 * not reach it" are different facts and an administrator has to be able to tell
 * which one they are looking at.
 */
export function GalleryPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const { data, isLoading, error, refetch } = useQuery<GalleryResponse>({
    queryKey: ["gallery"],
    queryFn: api.getGallery,
    retry: false,
  });

  // The narrowing lives in the URL, replaced rather than pushed, like every
  // other list's (`hooks/use-list-state.ts`).
  const [search, setSearch] = useUrlParam("q");
  const [category, setCategory] = useUrlParam("kind", "all");
  /**
   * Language is a facet now, not a second tab strip.
   *
   * This page carried two single-select filter rows, in two different shapes,
   * with an **All** control in each — one a row of solid pills, the other an
   * underlined tab strip, stacked directly on top of one another. Two controls
   * labelled All, adjacent, is a page asking somebody to work out which All
   * they mean.
   *
   * So it takes the shape the rest of the product's list pages use: the primary
   * axis is the counted tab strip, and anything else narrowing it is a facet
   * dropdown beside the search box. A facet with nothing chosen means *any*,
   * which is where the second All went.
   */
  const [languages, setLanguages] = useUrlParams("language");
  const [deploying, setDeploying] = useState<GalleryItem | null>(null);

  const items = data?.items ?? [];

  // Derived inline rather than memoized: the catalog is a few dozen entries,
  // and a useMemo over `data?.items ?? []` only trips the deps lint.
  const q = search.trim().toLowerCase();
  // Everything except the category axis, because the tabs are what select it —
  // and the counts have to come from *this*, not from the whole catalog. They
  // were taken from every item, so a search for "csv" left "Examples (34)"
  // sitting above two cards.
  const scoped = items.filter((item) => {
    const haystack = `${item.name} ${item.summary} ${item.description} ${(item.tags ?? []).join(" ")}`;
    return (
      (!q || haystack.toLowerCase().includes(q)) &&
      (languages.length === 0 || languages.includes(item.language))
    );
  });
  const filtered = scoped.filter((item) => category === "all" || item.kind === category);

  const counts: Record<string, number> = { all: scoped.length };
  for (const item of scoped) counts[item.kind] = (counts[item.kind] ?? 0) + 1;

  // The facet's own options carry counts from everything *except* the language
  // axis, for the same reason the tab counts exclude the category axis: a count
  // that already had this filter applied would read zero for every language you
  // have not chosen.
  const beforeLanguage = items.filter((item) => {
    const haystack = `${item.name} ${item.summary} ${item.description} ${(item.tags ?? []).join(" ")}`;
    return !q || haystack.toLowerCase().includes(q);
  });
  const languageCounts = new Map<string, number>();
  for (const item of beforeLanguage) {
    if (item.language) languageCounts.set(item.language, (languageCounts.get(item.language) ?? 0) + 1);
  }
  const languageOptions = [...languageCounts.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([language, count]) => ({ key: language, label: `${language} (${count})` }));

  const sync = useMutation({
    mutationFn: api.syncGallery,
    onSuccess: (result: GallerySync) => {
      queryClient.invalidateQueries({ queryKey: ["gallery"] });
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      if (result.status === "ok") toast.success("Catalog refreshed");
      else toast.warning(result.error || "Couldn’t refresh the catalog");
    },
    onError: (err: Error) => toast.error(err.message),
  });

  if (isLoading) {
    return (
      <div>
        <PageHeader title="Gallery" description={PAGE_DESCRIPTION} />
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-52 w-full rounded-lg" />
          ))}
        </div>
      </div>
    );
  }

  // The gallery is absent rather than disabled when an administrator has
  // switched it off, so the API 404s and there is nothing to soften. **Only a
  // 404**: every other failure was reported the same way, so a catalog the
  // install could not reach — the outage this feature is most likely to have —
  // read as a setting somebody had turned off, and sent an administrator to
  // Settings to switch on something already on.
  //
  // And the 404 says both of its causes. The same endpoint 404s for anybody who
  // may not publish, so "switched off" told a viewer following a shared link a
  // policy the page does not read.
  if (statusOf(error) === 404) {
    return (
      <div>
        <PageHeader title="Gallery" description={PAGE_DESCRIPTION} />
        <EmptyState
          icon={<LayoutGrid className="h-6 w-6" />}
          title="The gallery isn’t available to you"
          description="It’s either switched off on this install or limited to people who can publish."
        />
      </div>
    );
  }

  if (error || !data) {
    return (
      <div>
        <PageHeader title="Gallery" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The gallery could not be read"
          error={error}
          fallback="Reload the page. If it keeps failing, tell an administrator."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const categories = [
    { label: "All", value: "all" },
    ...(data.admin ? [{ label: "Extensions", value: "extension" }] : []),
    { label: "Examples", value: "example" },
  ];

  return (
    <div>
      {/* No Refresh button in the header. Refreshing the catalog is an
          install-wide administrative action on state — the address, where it
          came from, when it was last fetched — that lives in Admin → Settings,
          and this page is a place to deploy things. What stays here is the
          retry beside a failure, where the reason to press it is on screen. */}
      <PageHeader title="Gallery" description={PAGE_DESCRIPTION} />

      <div className="space-y-5">
        {/* One banner rather than the same sentence on every card. The catalog
            stays browsable because it has reference value; what is gone is
            every Deploy button. */}
        {data.source_blocked && (
          <Notice size="md">{data.source_blocked}</Notice>
        )}

        <SyncNotice
          sync={data.sync}
          admin={data.admin}
          onRetry={() => sync.mutate()}
          retrying={sync.isPending}
        />

        {items.length > 0 && (
          <>
            <div className="flex flex-wrap items-center gap-4">
              <SearchInput
                placeholder="Search the gallery…"
                value={search}
                onChange={setSearch}
                className="w-full sm:max-w-sm"
              />
              {languageOptions.length > 1 && (
                <FacetFilter
                  label="Language"
                  icon={<Code2 />}
                  options={languageOptions}
                  value={languages}
                  onChange={setLanguages}
                  placeholder="Find a language…"
                />
              )}
            </div>

            <FacetChips
              chips={languages.map((l) => ({
                option: languageOptions.find((o) => o.key === l) ?? { key: l, label: l },
                onRemove: () => setLanguages(languages.filter((v) => v !== l)),
              }))}
              onClear={() => setLanguages([])}
            />

            {categories.length > 2 && (
              <FilterBar
                label="Filter the catalog by kind"
                options={categories.map((c) => ({
                  ...c,
                  label: `${c.label} (${counts[c.value] ?? 0})`,
                }))}
                value={category}
                onChange={setCategory}
              />
            )}
          </>
        )}

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {filtered.map((item) => (
            <ItemCard
              key={item.id}
              item={item}
              showKind={category === "all"}
              onDeploy={() => setDeploying(item)}
            />
          ))}
        </div>

        {items.length === 0 ? (
          <CatalogEmptyState
            sync={data.sync}
            admin={data.admin}
            onRetry={() => sync.mutate()}
            retrying={sync.isPending}
          />
        ) : (
          filtered.length === 0 && (
            <EmptyState
              title="No entries match"
              description="Nothing in the catalog matches this search and these filters."
              action={
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => {
                    setSearch("");
                    setCategory("all");
                    setLanguages([]);
                  }}
                >
                  Clear
                </Button>
              }
            />
          )
        )}
      </div>

      {deploying && (
        <DeployDialog
          key={deploying.id}
          item={deploying}
          open
          onOpenChange={(o) => !o && setDeploying(null)}
          onDeployed={(name, id) => {
            setDeploying(null);
            queryClient.invalidateQueries({ queryKey: ["gallery"] });
            queryClient.invalidateQueries({ queryKey: ["apps"] });
            // The wizard says the same, so both doors onto a new app answer alike.
            toast.success(`Deploying “${name}”`);
            navigate(`/apps/${id}`);
          }}
        />
      )}
    </div>
  );
}

const PAGE_DESCRIPTION =
  "Extensions and examples from the Toft team. Each one deploys as an app you own.";

/* ------------------------------------------------------------------ *
 * Cards
 * ------------------------------------------------------------------ */

/** The vendored mark for a catalog language, keyed as `framework-marks.ts` is. */
const LANGUAGE_MARK: Record<string, string> = {
  python: "python",
  go: "go",
  javascript: "node",
  typescript: "node",
  node: "node",
  r: "r",
  static: "static",
};

function ItemCard({
  item,
  showKind,
  onDeploy,
}: {
  item: GalleryItem;
  /** False when the strip has already narrowed to one kind: the badge would
   *  say on every card what the tab above them says once. */
  showKind: boolean;
  onDeploy: () => void;
}) {
  const installed = item.installed ?? [];
  const isExtension = item.kind === "extension";
  const mark = LANGUAGE_MARK[(item.language ?? "").toLowerCase()];
  const Glyph = item.icon ? GALLERY_ICONS[item.icon] : undefined;

  return (
    <Card className="py-0">
      <CardContent className="flex h-full flex-col p-4">
        <div className="mb-2 flex items-start gap-3">
          {/* The language's own mark rather than the name's initials on one
              of twenty-four hues: the initials repeated the name beside them,
              and the colours meant nothing. An item in a language with no
              mark keeps the initials. An item that names its own glyph in
              the catalog gets that instead: an extension is a tool, and its
              language is the least interesting thing about it. */}
          {Glyph ? (
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg border border-border/60 bg-card text-primary">
              <Glyph className="size-5" aria-hidden="true" />
            </div>
          ) : mark ? (
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg border border-border/60 bg-card">
              <FrameworkMark id={mark} className="size-5" />
            </div>
          ) : (
            <AppIcon name={item.name} size="md" />
          )}
          <div className="min-w-0 flex-1">
            <h3 className="truncate font-semibold text-foreground">{item.name}</h3>
            {/* Language in words, then the version. Not the author: every
                item in this catalog is the Toft team's, so it said the same
                thing on twenty-four cards. */}
            <p className="text-xs text-muted-foreground">
              {[item.language, item.version].filter(Boolean).join(" · ")}
            </p>
          </div>
          {/* Neutral, both of them. Which of the two a gallery item is, is a
              fact about the row and not a state it is in — and a fact's badge
              stays neutral. The filled indigo put ten brand chips on a grid
              that already had twenty-six brand buttons, so nothing on the page
              read as primary. The word carries the difference, which is what
              words are for. */}
          {showKind && (
            <Badge variant="secondary" className="text-2xs">
              {isExtension ? "Extension" : "Example"}
            </Badge>
          )}
        </div>

        <p className="mb-3 line-clamp-3 flex-1 text-sm text-muted-foreground">
          {item.summary || item.description}
        </p>

        {cardTags(item).length > 0 && (
          <div className="mb-3 flex flex-wrap items-center gap-1.5">
            {cardTags(item).map((tag) => (
              <Badge key={tag} variant="secondary" className="text-2xs">
                {tag}
              </Badge>
            ))}
          </div>
        )}

        <InstalledNote item={item} />

        <div className="flex items-center justify-between gap-2 border-t border-border/60 pt-3">
          <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
            {sourceLink(item) && (
              <a
                href={sourceLink(item)}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 truncate rounded-sm hover:text-foreground hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
              >
                <ExternalLink className="h-3 w-3 shrink-0" />
                <span className="truncate">Source</span>
              </a>
            )}
          </div>
          {/* Outline, because twenty-six of these on one grid are twenty-six
              primaries, which is none. The grid is a list to read; the page's
              one filled button is the Deploy in the item's own dialog, where
              somebody has chosen an item and is confirming. The card's hover
              carries the affordance until then. */}
          <Button
            variant="outline"
            size="sm"
            className="h-7 shrink-0 text-xs"
            disabled={!item.deployable}
            onClick={onDeploy}
          >
            {installed.length > 0 ? "Deploy another copy" : "Deploy"}
          </Button>
        </div>

        {!item.deployable && item.blocked_reason && (
          <p className="mt-2 text-xs text-muted-foreground">{item.blocked_reason}</p>
        )}
      </CardContent>
    </Card>
  );
}

/**
 * The tags a card shows: never one that repeats a chip already on the card
 * (its kind, its language), and at most three. Search still reads all of them.
 */
function cardTags(item: GalleryItem): string[] {
  const shown = new Set(
    [item.kind, item.language, "example", "extension"]
      .filter(Boolean)
      .map((t) => String(t).toLowerCase()),
  );
  return (item.tags ?? []).filter((t) => !shown.has(t.toLowerCase())).slice(0, 3);
}

/**
 * Where a card's Source link points.
 *
 * `source_url` first, because it is the field that means "somewhere a person
 * can read this". An extension falls back to the repository it is cloned from,
 * which is the same place. An example has neither unless the catalog supplies
 * one: its `zip_url` is an archive that downloads rather than opens, so the
 * link is simply absent rather than pointing at a file save dialog.
 */
function sourceLink(item: GalleryItem): string {
  return item.source_url || (item.kind === "extension" ? item.repo_url : "");
}

/**
 * What the deploy dialog names as the source: for an extension the ref is part
 * of it, because under ADR §28 D3 a ref is the whole identity of a version.
 * This is the address the platform will actually fetch — deliberately not the
 * card's Source link, which is where a person goes to read the code.
 */
function galleryItemSource(item: GalleryItem): string {
  return item.kind === "extension" ? `${item.repo_url} @ ${item.ref}` : item.zip_url;
}

/**
 * What this viewer already has. An extension's installs are an install-wide
 * fact; an example's are personal, which is why the two sentences differ.
 *
 * An update is only ever offered for an extension — an example is a snapshot,
 * and there is nothing to offer.
 */
function InstalledNote({ item }: { item: GalleryItem }) {
  const installed = item.installed ?? [];
  if (installed.length === 0) return null;

  return (
    <TableCard flat className="mb-4 space-y-1.5 p-2.5">
      {installed.map((inst) => (
        <div key={inst.app_id} className="flex items-center justify-between gap-2 text-xs">
          <Link
            to={`/apps/${inst.app_id}`}
            className="inline-flex min-w-0 items-center gap-1.5 rounded-sm text-foreground hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            {inst.update_available ? (
              <ArrowUpCircle className={`h-3.5 w-3.5 shrink-0 ${toneText.attention}`} />
            ) : (
              <CheckCircle2 className={`h-3.5 w-3.5 shrink-0 ${toneText.ok}`} />
            )}
            <span className="truncate">{inst.name}</span>
          </Link>
          <span className="shrink-0 text-muted-foreground">
            {inst.update_available
              ? `${inst.version || "—"} → ${item.version}`
              : inst.version_not_recorded
                ? "version not recorded"
                : inst.version_unknown
                  ? "version unknown"
                  : inst.version || "building"}
          </span>
        </div>
      ))}
    </TableCard>
  );
}

/* ------------------------------------------------------------------ *
 * The catalog's own state
 * ------------------------------------------------------------------ */

/** A banner for the two states where what is on screen is not the whole
 *  catalog. Silent when the last refresh worked.
 *
 *  It carries the only Refresh control on this page, and only for an
 *  administrator: a publisher looking at a stale catalog cannot fetch one, and
 *  a button that 403s is worse than the sentence explaining who can. */
function SyncNotice({
  sync,
  admin,
  onRetry,
  retrying,
}: {
  sync: GallerySync;
  admin: boolean;
  onRetry: () => void;
  retrying: boolean;
}) {
  if (sync.status === "ok" || sync.status === "") return null;

  const line =
    sync.status === "too_new"
      ? "The catalog needs a newer version of Toft."
      : sync.status === "unparseable"
        ? "The catalog could not be read."
        : "The catalog could not be fetched. This is the last copy.";

  return (
    <Notice
      action={
        admin ? (
          <Button variant="outline" size="sm" disabled={retrying} onClick={onRetry}>
            <RefreshCw className={`h-3.5 w-3.5 ${retrying ? "animate-spin" : ""}`} />
            {retrying ? "Retrying…" : "Retry"}
          </Button>
        ) : undefined
      }
    >
      <p>
        {line} {sync.fetched_at && <>Last checked <RelativeTime iso={sync.fetched_at} />.</>}
      </p>
      {admin && sync.error && <p className="mt-1 font-mono opacity-80">{sync.error}</p>}
      {!admin && (
        <p className="mt-1 opacity-80">An administrator can retry from Admin → Settings.</p>
      )}
    </Notice>
  );
}

/** Distinguishes "the catalog lists nothing you may deploy" from "we have
 *  never managed to fetch it", because they need different actions. */
function CatalogEmptyState({
  sync,
  admin,
  onRetry,
  retrying,
}: {
  sync: GallerySync;
  admin: boolean;
  onRetry: () => void;
  retrying: boolean;
}) {
  if (sync.status === "" || !sync.fetched_at) {
    return (
      <EmptyState
        title="The catalog has not been fetched yet"
        description={
          admin
            ? "It is fetched shortly after start-up and every six hours after that."
            : "It is fetched shortly after start-up. Check back in a minute."
        }
        action={
          admin ? (
            <Button variant="secondary" size="sm" disabled={retrying} onClick={onRetry}>
              <RefreshCw className={`h-3.5 w-3.5 ${retrying ? "animate-spin" : ""}`} />
              {retrying ? "Fetching…" : "Fetch it now"}
            </Button>
          ) : undefined
        }
      />
    );
  }
  if (sync.status !== "ok") {
    return (
      <EmptyState
        icon={<LayoutGrid className="h-6 w-6" />}
        title="Nothing to show from the catalog"
        description="The banner above says what went wrong."
      />
    );
  }
  return (
    <EmptyState
      icon={<LayoutGrid className="h-6 w-6" />}
      title="Nothing available here"
      description={
        admin
          ? "Nothing in the catalog can be deployed here. Examples may be off in Admin → Settings."
          : "The catalog lists no examples at the moment."
      }
    />
  );
}

/* ------------------------------------------------------------------ *
 * Deploying
 * ------------------------------------------------------------------ */

/**
 * The deploy dialog. It says what will be created and where the code comes
 * from before it creates anything — under this design a ref is the whole
 * identity of an extension version, so the ref is shown rather than hidden.
 *
 * The environment fields are a *request* from the catalog, not a grant: they
 * become ordinary app environment variables, and nothing about the entry gives
 * the app anything it would not otherwise have.
 */
function DeployDialog({
  item,
  open,
  onOpenChange,
  onDeployed,
}: {
  item: GalleryItem;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDeployed: (name: string, appID: string) => void;
}) {
  const installed = item.installed ?? [];
  const [name, setName] = useState(installed.length > 0 ? `${item.name} (copy)` : item.name);
  const [slug, setSlug] = useState("");
  const [env, setEnv] = useState<Record<string, string>>(() =>
    Object.fromEntries((item.env ?? []).map((f) => [f.name, f.default])),
  );

  const missing = (item.env ?? []).filter((f) => f.required && !env[f.name]?.trim());

  const deploy = useMutation({
    mutationFn: () =>
      api.deployFromGallery(item.id, {
        name: name.trim(),
        slug: slug.trim(),
        env_vars: env,
        // Which of them the catalog asked for as a secret. The values are
        // encrypted at rest either way; this is what stops the app's Settings
        // tab rendering a connection string back to whoever opens it.
        env_sensitive: Object.fromEntries(
          (item.env ?? []).map((f) => [f.name, !!f.sensitive]),
        ),
      }),
    onSuccess: (result) => onDeployed(result.app.name || result.app.slug, result.app.id),
    onError: (err: Error) => toast.error(err.message || `Couldn’t deploy “${item.name}”`),
  });
  const blocked = !name.trim() || missing.length > 0;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* Header, body, footer, with only the body scrolling, so the title
          and the Deploy button never leave the screen on an item with several
          environment fields. */}
      <DialogContent scrollable className="sm:max-w-lg">
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!deploy.isPending && !blocked) deploy.mutate();
          }}
        >
        <DialogHeader>
          <DialogTitle>Deploy {item.name}</DialogTitle>
          <DialogDescription>
            {item.kind === "extension"
              ? "Creates an app you own. Updates come from the gallery."
              : "Creates an app you own from a copy of this example. It is never updated."}
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          {item.description && (
            <p className="text-sm text-muted-foreground">{item.description}</p>
          )}

          <TableCard flat>
          <dl className="space-y-1.5 p-3 text-xs">
            <div className="flex justify-between gap-3">
              <dt className="text-muted-foreground">Version</dt>
              <dd className="font-mono">{item.version}</dd>
            </div>
            <div className="flex justify-between gap-3">
              <dt className="text-muted-foreground">Source</dt>
              {/* It wraps rather than truncating. This is the one address the
                  dialog exists to state — where the code about to run on this
                  install comes from — and behind an ellipsis the only way to
                  read it was a hover `title`, which a keyboard and a touch
                  screen both have no way to reach. `min-w-0` is still what
                  bounds the flex item; `break-all` is what makes the overflow
                  a second line instead of a wider row. */}
              <dd className="min-w-0 break-all font-mono">
                {galleryItemSource(item)}
              </dd>
            </div>
          </dl>
          </TableCard>

          {installed.length > 0 && (
            <Notice>
              {item.kind === "extension"
                ? `Already installed (${installed
                    .map((i) => i.name)
                    .join(", ")}). Deploying again creates a second copy.`
                : `You already have a copy of this example (${installed
                    .map((i) => i.name)
                    .join(", ")}). Deploying again creates another.`}
            </Notice>
          )}

          {item.notes && <p className="text-xs text-muted-foreground">{item.notes}</p>}

          <AppNameFields
            idPrefix="gallery"
            name={name}
            onNameChange={setName}
            slug={slug}
            onSlugChange={setSlug}
            namePlaceholder={item.name}
            slugPlaceholder={item.suggested_slug || undefined}
            autoFocusName
          />

          {(item.env ?? []).length > 0 && (
            <div className="space-y-3 border-t border-border/60 pt-3">
              <div>
                <h3 className="text-sm font-semibold">Environment variables</h3>
                <FieldHelp>
                  Saved as environment variables. Secrets are never shown again.
                </FieldHelp>
              </div>
              {(item.env ?? []).map((f) => (
                <div key={f.name} className="space-y-1.5">
                  <Label htmlFor={`env-${f.name}`} className="font-mono text-xs">
                    {f.name}
                    {/* A red asterisk is a colour and a glyph carrying the
                        whole of "you have to fill this in". The word goes
                        with it, and the field says so itself. */}
                    {f.required && (
                      <>
                        <span aria-hidden className="ml-1 text-destructive">*</span>
                        <span className="sr-only"> (required)</span>
                      </>
                    )}
                  </Label>
                  <Input
                    id={`env-${f.name}`}
                    type={f.sensitive ? "password" : "text"}
                    autoComplete={f.sensitive ? "new-password" : undefined}
                    required={f.required}
                    value={env[f.name] ?? ""}
                    onChange={(e) => setEnv((v) => ({ ...v, [f.name]: e.target.value }))}
                  />
                  {f.description && (
                    <FieldHelp>{f.description}</FieldHelp>
                  )}
                </div>
              ))}
            </div>
          )}
        </DialogBody>

        <DialogFooter className="flex-col items-stretch gap-2 sm:flex-row sm:items-center">
          {/* Why Deploy is disabled, in text beside it. A disabled control is
              not focusable and swallows the pointer, so a `title` on it is
              unreachable by every route — and this was a button that looked
              idle for a reason the dialog stated nowhere. */}
          {!deploy.isPending && blocked && (
            <p className="mr-auto text-xs text-muted-foreground">
              {!name.trim()
                ? "Give the app a name to deploy it."
                : `Fill in ${missing.map((f) => f.name).join(", ")} to deploy it.`}
            </p>
          )}
          <Button
            type="button"
            variant="outline"
            disabled={deploy.isPending}
            onClick={() => onOpenChange(false)}
          >
            Cancel
          </Button>
          <Button type="submit" disabled={deploy.isPending || blocked}>
            {deploy.isPending ? "Deploying…" : "Deploy"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

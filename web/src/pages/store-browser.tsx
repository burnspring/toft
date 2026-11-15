import { useMemo, useRef, useState } from "react";
import { copyToClipboard } from "@/lib/clipboard";
import { RelativeTime } from "@/components/relative-time";
import { Link, useParams } from "react-router-dom";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  ChevronRight,
  Download,
  Folder,
  FolderPlus,
  MoreHorizontal,
  Upload,
  X,
} from "lucide-react";

import { EmptyState } from "@/components/empty-state";
import { Notice } from "@/components/notice";
import { PageHeader } from "@/components/page-header";
import { ParentCrumb } from "@/components/parent-crumb";
import { Section } from "@/components/section";
import { StorageUsage } from "@/components/storage-usage";
import { QueryError } from "@/components/query-error";
import { SearchInput } from "@/components/search-input";
import { SortableHead } from "@/components/sortable-head";
import { TableCard } from "@/components/table-card";
import { rowActivation } from "@/lib/row-activation";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, type StoreObjectSummary } from "@/lib/api";
import { formatBytes } from "@/lib/bytes";
import {
  accessLabel,
  STORE_CAVEAT,
  accessBadgeClass,
  storeAffordances,
  storeLeafOf,
  storePathFromWildcard,
} from "@/lib/storage";
import { MAX_DELETE_PER_ACTION, downloadObject, storeUploads } from "@/lib/store-actions";
import { formatDateTime } from "@/lib/time";
import { cn } from "@/lib/utils";
import { nextSort, type SortState } from "@/lib/sort";
import { DeleteObjectsDialog } from "@/components/store-delete-dialog";

type SortKey = "name" | "size" | "modified";

/**
 * One store's objects.
 *
 * Three things about this screen are consequences of S3 rather than choices,
 * and the page takes them rather than hiding them: a folder is a common prefix
 * and not a thing, there is no rename and no move, and deleting a folder is
 * deleting the objects under it. `STORE_CAVEAT` says the second one once, at
 * the top, in the words the admin page already uses.
 */
/**
 * Two addresses, one page. `/storage/:slug/*` is the reader's, reached from
 * `/me/storage` and an app's Storage block; `/admin/storage/:slug/files/*` is
 * the same browser rendered inside `AdminLayout`, so an administrator who
 * opened *Browse files* keeps the sidebar and has a crumb back to the resource
 * rather than landing on a page with no way back into Admin.
 */
export function AdminStoreBrowserPage() {
  return <StoreBrowserPage admin />;
}

export function StoreBrowserPage({ admin = false }: { admin?: boolean }) {
  const { slug = "", "*": wildcard = "" } = useParams();
  /**
   * The path this page is showing, **slash-terminated or empty** — never
   * anything in between.
   *
   * Everything below concatenates: a key is `path + name`, a child folder is
   * `path + folder`, an upload lands at `path + relative`. A path carrying no
   * trailing slash makes every one of those off by a separator, which does not
   * show up as an error — it shows up as a signature over the wrong key. The
   * URL cannot carry the slash (a route ending in one is a different route to
   * every router), so it is put back here, once, at the only place the two
   * representations meet.
   */
  const path = storePathFromWildcard(wildcard);

  /**
   * Keyed on the folder, because *all* of this page's state is about the folder
   * it is showing. Selection, the detail panel, the filter and a pending folder
   * are each meaningless one level down, and the router reuses this component
   * across folders — so without the key a selection made in one folder follows
   * the person into the next, where the rows it names are not on screen and the
   * Delete bar above them counts things they cannot see.
   */
  const base = admin ? `/admin/storage/${slug}/files` : `/storage/${slug}`;
  const parent = admin
    ? { to: `/admin/storage/${slug}`, label: slug }
    : { to: "/me/storage", label: "Storage" };
  return (
    <StoreBrowser key={`${slug}:${path}`} slug={slug} path={path} base={base} parent={parent} />
  );
}

function StoreBrowser({
  slug,
  path,
  base,
  parent,
}: {
  slug: string;
  path: string;
  /** Where this store's root is on this route — every folder link hangs off it. */
  base: string;
  /** The page a reader came from, which the crumb names. */
  parent: { to: string; label: string };
}) {
  const queryClient = useQueryClient();

  const [flat, setFlat] = useState(false);
  const [filter, setFilter] = useState("");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "name", dir: "asc" });
  const [selected, setSelected] = useState<Set<string>>(new Set());
  /** The last checkbox touched, which a shift-click extends from. A ref rather
   *  than state: it changes nothing on screen by itself. */
  const anchor = useRef<string | null>(null);
  const [detail, setDetail] = useState<StoreObjectSummary | null>(null);
  /** A folder that does not exist yet: the next upload writes into it, and if
   * nothing is uploaded it was never real. No zero-byte marker is ever written
   * — the listing already skips the ones other tools leave, and writing them
   * here would make the platform the source of the litter. */
  const [pendingFolder, setPendingFolder] = useState<string | null>(null);
  /** The name being typed, before it becomes the pending path above. */
  const [folderDraft, setFolderDraft] = useState<string | null>(null);
  /** The objects a delete is being confirmed for, named by their **full key**
   * — relative to the store's root, which is what the presign endpoints
   * resolve against. Carrying the rows rather than the names is what lets the
   * dialog total a folder it never listed. */
  const [deleting, setDeleting] = useState<StoreObjectSummary[] | null>(null);
  /** An upload that would replace something, waiting to be confirmed. */
  const [replacing, setReplacing] = useState<{
    files: Array<{ file: File; relative: string }>;
    colliding: string[];
  } | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const { data: storeList, error: storesError, refetch: refetchStores } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
  });
  /**
   * The store this page is about. `GET /store` answers per *scope*, so a
   * caller granted two folders of one store has two rows here — they are the
   * same store, and the name is what this is used for.
   */
  const store = storeList?.stores.find((s) => s.slug === slug);

  const listing = useInfiniteQuery({
    queryKey: ["store-objects", slug, path, flat],
    queryFn: ({ pageParam }) =>
      api.listStoreObjects(slug, { path, flat, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
    retry: false,
    enabled: Boolean(slug),
  });

  const loaded = listing.data?.pages ?? [];
  /** Listing rows carry names relative to the listed prefix; every action this
   * page takes needs the full key. The two are named apart so that a site
   * handing one where the other belongs reads wrong. */
  const objects = loaded
    .flatMap((p) => p.objects)
    .map((o) => ({ ...o, name: path + o.name }));
  const folders = loaded.flatMap((p) => p.prefixes ?? []);
  /**
   * The controls come from the level the *server* resolved for the folder on
   * screen, never from the store-wide one. Inside a folder somebody holds
   * `full` on, delete is offered; one level up, where they hold nothing, it is
   * absent — which is the same answer the presign route would give, arrived at
   * without a refused request to discover it.
   */
  const here = listing.data?.pages[0]?.access;
  const affordances = here ? storeAffordances(here) : null;
  const uploadInto = path + (pendingFolder ?? "");

  const shownFolders = useMemo(
    () => folders.filter((f) => f.toLowerCase().includes(filter.toLowerCase())),
    [folders, filter],
  );
  const shownObjects = useMemo(() => {
    const match = objects.filter((o) =>
      storeLeafOf(o.name, path).toLowerCase().includes(filter.toLowerCase()),
    );
    const dir = sort.dir === "asc" ? 1 : -1;
    return [...match].sort((a, b) => {
      if (sort.key === "size") return (a.size - b.size) * dir;
      if (sort.key === "modified")
        return (Date.parse(a.last_modified) - Date.parse(b.last_modified)) * dir;
      return a.name.localeCompare(b.name) * dir;
    });
  }, [objects, filter, sort, path]);
  /** Whether the header box is on, off, or in between — scoped to what is on
   *  screen, which is the set a person can see and is agreeing to. */
  const someShownSelected = shownObjects.some((o) => selected.has(o.name));
  const allShownSelected =
    shownObjects.length > 0 && shownObjects.every((o) => selected.has(o.name));

  // `nextSort`, not a second copy of it: same column flips, a new column
  // starts descending, and a table that decided that for itself is a table
  // that will one day decide it differently.
  const toggleSort = (key: SortKey) => setSort((s) => nextSort(s, key));

  /** Hand the queue a set of files, having said what this page already knows
   * about them. */
  function send(files: Array<{ file: File; relative: string }>) {
    if (files.length === 0) return;
    storeUploads.enqueue(
      files.map(({ file, relative }) => ({
        storeSlug: slug,
        path,
        name: uploadInto + relative,
        file,
      })),
    );
    setPendingFolder(null);
  }

  /**
   * What `canReplace` is for.
   *
   * The affordance table has always said whether this person's grant may
   * replace an existing object, and nothing read it — so at `write` a
   * collision was discovered by the server, after the browser had hashed the
   * whole file and pushed all of it, as a 412 at the very end; and at
   * `full` an object was replaced with no more ceremony than a drag.
   * Neither is a rung of the ladder being enforced wrongly — the condition is
   * signed into the URL and stays the authority — they are the page failing to
   * say something it knew.
   *
   * **Best effort, and it says so rather than pretending otherwise.** Only the
   * objects this folder has loaded can be compared against, and somebody else
   * may write between here and the PUT. So this never *permits* anything: it
   * refuses early where the grant could not have replaced, and asks where it
   * could.
   */
  function enqueue(files: Array<{ file: File; relative: string }>) {
    if (files.length === 0) return;
    const known = new Set(objects.map((o) => o.name));
    const colliding = files.filter((f) => known.has(uploadInto + f.relative));
    if (colliding.length === 0) {
      send(files);
      return;
    }
    if (!affordances?.canReplace) {
      const fresh = files.filter((f) => !known.has(uploadInto + f.relative));
      toast.error(
        colliding.length === 1
          ? `${colliding[0].relative} is already here, and your access can’t replace it.`
          : `${colliding.length} of these are already here, and your access can’t replace them.`,
        { description: fresh.length > 0 ? `The other ${fresh.length} were queued.` : undefined },
      );
      send(fresh);
      return;
    }
    setReplacing({ files, colliding: colliding.map((f) => uploadInto + f.relative) });
  }

  async function download(name: string) {
    try {
      toast.success(`Downloading “${await downloadObject(slug, name)}”`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t prepare that download");
    }
  }

  /** Deleting a folder is deleting the objects under it, and only when they
   * have all been counted. A delete that reported success having removed the
   * first thousand of a folder is the worst outcome available here. */
  async function deleteFolder(folder: string) {
    try {
      const page = await api.listStoreObjects(slug, { path: path + folder, flat: true });
      if (page.next_cursor) {
        toast.error(
          `${folder} has too many files to delete at once. Open it and delete them in batches.`,
        );
        return;
      }
      if (page.objects.length > MAX_DELETE_PER_ACTION) {
        toast.error(
          `${folder} holds ${page.objects.length} files, and one action deletes at most ${MAX_DELETE_PER_ACTION}. Open it and delete them in batches.`,
        );
        return;
      }
      if (page.objects.length === 0) {
        toast.error(`${folder} is empty`);
        return;
      }
      setDeleting(page.objects.map((o) => ({ ...o, name: path + folder + o.name })));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t read that folder");
    }
  }

  if (storesError) {
    return (
      <div className="space-y-6">
        <ParentCrumb to={parent.to} parent={parent.label} />
        <QueryError
          title="This storage could not be read"
          error={storesError}
          fallback="Reload the page. If it keeps failing, ask whoever gave you access."
          onRetry={() => void refetchStores()}
        />
      </div>
    );
  }

  if (storeList && !store) {
    return (
      <div className="space-y-6">
        <ParentCrumb to={parent.to} parent={parent.label} />
        <PageHeader title="Storage" />
        <TableCard>
          <EmptyState
            title="No such storage"
            description="It may have been removed, or your access withdrawn."
            action={
              <Link to={parent.to} className="text-sm font-medium text-primary hover:underline">
                Back to {parent.label}
              </Link>
            }
          />
        </TableCard>
      </div>
    );
  }

  const segments = path.split("/").filter(Boolean);

  return (
    <div>
      <div className="mb-6">
        <ParentCrumb to={parent.to} parent={parent.label} current="Files" />
      </div>
      <PageHeader
        title={store?.name ?? slug}
        // The resource's own description. The read-whole caveat is an app
        // developer's constraint, and it moved to the section written for
        // them below — every viewer who only downloads read it here.
        description={store?.description || undefined}
        badge={
          // The level *here*, so the chip and the controls beneath it cannot
          // disagree. At a root somebody reaches only through folder grants
          // there is no level and no chip — they hold nothing in this folder,
          // and a chip saying otherwise would be the page's own opinion.
          here && (
            <Badge variant="outline" className={cn("text-2xs", accessBadgeClass(here))}>
              {accessLabel(here)}
            </Badge>
          )
        }
      />

      {/* Breadcrumbs from the store's root. The root segment is the store's
          name — never the bucket and never the prefix, neither of which this
          page is ever told. */}
      <nav aria-label="Breadcrumb" className="mb-4 flex flex-wrap items-center gap-1 text-sm">
        <Link to={base} className="text-primary hover:underline">
          {store?.name ?? slug}
        </Link>
        {segments.map((seg, i) => (
          <span key={i} className="flex items-center gap-1">
            <ChevronRight className="h-3.5 w-3.5 text-muted-foreground" />
            {i === segments.length - 1 ? (
              <span className="text-foreground">{seg}</span>
            ) : (
              <Link
                to={`${base}/${segments.slice(0, i + 1).join("/")}`}
                className="text-primary hover:underline"
              >
                {seg}
              </Link>
            )}
          </span>
        ))}
        {pendingFolder && (
          <span className="flex items-center gap-1">
            <ChevronRight className="h-3.5 w-3.5 text-muted-foreground" />
            <span className="text-muted-foreground italic">
              {pendingFolder.replace(/\/$/, "")}
            </span>
            <button
              type="button"
              onClick={() => setPendingFolder(null)}
              aria-label="Discard the new folder"
              className="rounded p-0.5 text-muted-foreground hover:text-foreground"
            >
              <X className="h-3 w-3" />
            </button>
          </span>
        )}
      </nav>

      {pendingFolder && (
        <Notice tone="info" className="mb-4">
          Upload a file to create <span className="font-mono">{pendingFolder}</span>, or discard it.
        </Notice>
      )}

      <div className="mb-4 flex flex-wrap items-center gap-2">
        <SearchInput
          placeholder="Filter files…"
          value={filter}
          onChange={setFilter}
          className="w-56"
        />
        {/* A labelled switch, whose state is its own: one button naming the
            mode it would switch *to* left a reader unable to tell which view
            was on. Not a filter chip — it changes the shape of the listing,
            not how much of it shows, so it has no count to give. */}
        <div className="flex items-center gap-2">
          <Switch id="store-flat" checked={flat} onCheckedChange={setFlat} />
          <Label htmlFor="store-flat" className="text-sm font-normal">
            All files in one list
          </Label>
        </div>
        <div className="flex-1" />
        {affordances?.canUpload && (
          <>
            {folderDraft === null ? (
              <Button variant="outline" size="sm" onClick={() => setFolderDraft("")}>
                <FolderPlus className="mr-1.5 h-3.5 w-3.5" />
                New folder
              </Button>
            ) : (
              // Inline rather than a dialog: naming a folder that does not exist
              // yet is an edit to the breadcrumb beside it, and a modal over a
              // table to type one word is ceremony.
              <form
                className="flex items-center gap-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  const clean = folderDraft.replace(/^\/+|\/+$/g, "").trim();
                  if (clean) setPendingFolder(`${clean}/`);
                  setFolderDraft(null);
                }}
              >
                <Input
                  value={folderDraft}
                  onChange={(e) => setFolderDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Escape") setFolderDraft(null);
                  }}
                  placeholder="Folder name"
                  aria-label="Folder name"
                  className="h-8 w-44"
                  autoFocus
                />
                <Button type="submit" variant="outline" size="sm" disabled={!folderDraft.trim()}>
                  Create
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => setFolderDraft(null)}
                >
                  Cancel
                </Button>
              </form>
            )}
            <Button size="sm" onClick={() => fileInput.current?.click()}>
              <Upload className="mr-1.5 h-3.5 w-3.5" />
              Upload
            </Button>
            <input
              ref={fileInput}
              type="file"
              multiple
              className="hidden"
              onChange={(e) => {
                enqueue(
                  Array.from(e.target.files ?? []).map((file) => ({ file, relative: file.name })),
                );
                e.target.value = "";
              }}
            />
          </>
        )}
      </div>

      {selected.size > 0 && affordances?.canDelete && (
        <TableCard flat className="mb-4 flex flex-wrap items-center gap-3 px-3 py-2">
          <span className="text-sm text-foreground">{selected.size} selected</span>
          <Button
            variant="destructive"
            size="sm"
            onClick={() => setDeleting(objects.filter((o) => selected.has(o.name)))}
          >
            Delete…
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>
            Clear
          </Button>
        </TableCard>
      )}

      {/* The detail column is reserved only when there is a detail to put in
          it. Held open unconditionally, it took 20rem off the table while
          the toolbar above kept the full width — so Upload floated over
          blank canvas beside the card it acts on. */}
      <div className={cn("grid gap-4", detail && "lg:grid-cols-[minmax(0,1fr)_20rem]")}>
        {/* A drop target and nothing else — no click handler, no keyboard
            behaviour to mirror. Dropping files is a mouse gesture with no
            keyboard equivalent anywhere, and the Upload button above is the
            route that does not need one. */}
        {/* eslint-disable-next-line jsx-a11y/no-static-element-interactions */}
        <div
          onDragOver={(e) => {
            if (affordances?.canUpload) e.preventDefault();
          }}
          onDrop={(e) => {
            if (!affordances?.canUpload) return;
            e.preventDefault();
            void collectDrop(e.dataTransfer).then(enqueue);
          }}
        >
          <TableCard>
            {listing.isLoading ? (
              <div className="space-y-2 p-4">
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
              </div>
            ) : listing.error ? (
              <QueryError
                title="This folder could not be read"
                error={listing.error}
                fallback="Reload the page. If it keeps failing, ask whoever gave you access."
                onRetry={() => void listing.refetch()}
              />
            ) : shownObjects.length === 0 && shownFolders.length === 0 ? (
              <EmptyState
                icon={<Folder className="h-6 w-6" />}
                title={filter ? "No files match" : "Nothing here yet"}
                description={
                  filter
                    ? "The filter searches what is loaded. Load more, or show all files in one list."
                    : affordances?.canUpload
                      ? "Drag files here, or use Upload."
                      : "Your access is read-only."
                }
                action={
                  filter ? (
                    <Button variant="secondary" size="sm" onClick={() => setFilter("")}>
                      Clear
                    </Button>
                  ) : undefined
                }
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    {affordances?.canDelete && (
                      <TableHead className="w-10">
                        {/* Select-all, and the reason it has to exist: one
                            action deletes at most a hundred objects, this page
                            tells people to "delete in batches", and the only
                            way to build a batch was a hundred clicks. Scoped
                            to what is on screen — the filter and the pages
                            loaded — because that is the set a person can see
                            and is agreeing to. */}
                        <Checkbox
                          aria-label={
                            allShownSelected ? "Clear the selection" : "Select every file here"
                          }
                          checked={allShownSelected}
                          indeterminate={someShownSelected && !allShownSelected}
                          onCheckedChange={(on) =>
                            setSelected((prev) => {
                              const next = new Set(prev);
                              for (const o of shownObjects) {
                                if (on) next.add(o.name);
                                else next.delete(o.name);
                              }
                              return next;
                            })
                          }
                        />
                      </TableHead>
                    )}
                    <SortableHead label="Name" sortKey="name" sort={sort} onSort={toggleSort} />
                    <SortableHead
                      label="Size"
                      sortKey="size"
                      sort={sort}
                      onSort={toggleSort}
                      className="hidden text-right @md:table-cell"
                      align="right"
                    />
                    <SortableHead
                      label="Modified"
                      sortKey="modified"
                      sort={sort}
                      onSort={toggleSort}
                      className="hidden @xl:table-cell"
                    />
                    <TableHead className="w-10">
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {/* Folders first, and only in the folder view: a flat
                      listing has no levels to have one in. */}
                  {shownFolders.map((folder) => (
                    <TableRow key={folder}>
                      {affordances?.canDelete && <TableCell />}
                      <TableCell>
                        <Link
                          to={`${base}/${path}${folder}`.replace(/\/+$/, "")}
                          className="flex items-center gap-2 font-medium text-primary hover:underline"
                        >
                          <Folder className="h-4 w-4 shrink-0" />
                          {folder.replace(/\/$/, "")}
                        </Link>
                      </TableCell>
                      <TableCell className="hidden text-right text-muted-foreground @md:table-cell">
                        —
                      </TableCell>
                      <TableCell className="hidden text-muted-foreground @xl:table-cell">—</TableCell>
                      <TableCell>
                        {affordances?.canDelete && (
                          <DropdownMenu>
                            <DropdownMenuTrigger
                              render={
                                <Button
                                  variant="ghost"
                                  size="icon"
                                  className="h-7 w-7"
                                  aria-label={`Actions for the folder ${folder}`}
                                >
                                  <MoreHorizontal className="h-4 w-4" />
                                </Button>
                              }
                            />
                            <DropdownMenuContent align="end">
                              <DropdownMenuItem variant="destructive" onClick={() => void deleteFolder(folder)}>
                                Delete folder…
                              </DropdownMenuItem>
                            </DropdownMenuContent>
                          </DropdownMenu>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}

                  {shownObjects.map((obj) => (
                    <TableRow
                      key={obj.name}
                      {...rowActivation(`Details of ${storeLeafOf(obj.name, path)}`, () =>
                        setDetail(obj),
                      )}
                    >
                      {affordances?.canDelete && (
                        <TableCell onClick={(e) => e.stopPropagation()}>
                          <Checkbox
                            checked={selected.has(obj.name)}
                            aria-label={`Select ${storeLeafOf(obj.name, path)}`}
                            // Shift extends from the last box touched, the way
                            // every file list does. Read off the event rather
                            // than held in state so it cannot go stale, and
                            // guarded on the anchor still being on screen —
                            // the filter and the pages loaded both move under
                            // it.
                            onClick={(e) => {
                              if (!e.shiftKey || anchor.current === null) return;
                              const from = shownObjects.findIndex((o) => o.name === anchor.current);
                              const to = shownObjects.findIndex((o) => o.name === obj.name);
                              if (from < 0 || to < 0) return;
                              e.preventDefault();
                              const on = !selected.has(obj.name);
                              setSelected((prev) => {
                                const next = new Set(prev);
                                for (let i = Math.min(from, to); i <= Math.max(from, to); i++) {
                                  if (on) next.add(shownObjects[i].name);
                                  else next.delete(shownObjects[i].name);
                                }
                                return next;
                              });
                            }}
                            onCheckedChange={(on) => {
                              anchor.current = obj.name;
                              setSelected((prev) => {
                                const next = new Set(prev);
                                if (on) next.add(obj.name);
                                else next.delete(obj.name);
                                return next;
                              });
                            }}
                          />
                        </TableCell>
                      )}
                      <TableCell className="font-medium text-foreground">
                        <span className="block truncate" title={obj.name}>
                          {storeLeafOf(obj.name, path)}
                        </span>
                      </TableCell>
                      <TableCell className="hidden text-right text-muted-foreground @md:table-cell">
                        {formatBytes(obj.size)}
                      </TableCell>
                      <TableCell className="hidden text-muted-foreground @xl:table-cell">
                        <RelativeTime iso={obj.last_modified} />
                      </TableCell>
                      <TableCell onClick={(e) => e.stopPropagation()}>
                        <DropdownMenu>
                          <DropdownMenuTrigger
                            render={
                              <Button
                                variant="ghost"
                                size="icon"
                                className="h-7 w-7"
                                aria-label={`Actions for ${storeLeafOf(obj.name, path)}`}
                              >
                                <MoreHorizontal className="h-4 w-4" />
                              </Button>
                            }
                          />
                          <DropdownMenuContent align="end">
                            <DropdownMenuItem onClick={() => void download(obj.name)}>
                              Download
                            </DropdownMenuItem>
                            <DropdownMenuItem
                              onClick={() => {
                                void copyToClipboard(obj.name, "name");
                              }}
                            >
                              Copy name
                            </DropdownMenuItem>
                            {affordances?.canDelete && (
                              <DropdownMenuItem variant="destructive" onClick={() => setDeleting([obj])}>
                                Delete…
                              </DropdownMenuItem>
                            )}
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}

            {listing.hasNextPage && (
              <div className="flex flex-col items-center gap-1 border-t border-border/60 px-4 py-3">
                {/* Said out loud, because a sort over one page of a store with
                    a hundred thousand objects orders a thousandth of it and
                    looks exactly like a sort of the whole thing. */}
                <p className="text-2xs text-muted-foreground">
                  Sorting and filtering apply to what is loaded. There is more beyond this.
                </p>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={listing.isFetchingNextPage}
                  onClick={() => void listing.fetchNextPage()}
                >
                  {listing.isFetchingNextPage ? "Loading…" : "Load more"}
                </Button>
              </div>
            )}
          </TableCard>
        </div>

        {detail && (
          <TableCard className="h-fit p-4">
            <div className="flex items-start justify-between gap-2">
              <h2 className="text-sm font-semibold break-all text-foreground">
                {storeLeafOf(detail.name, path)}
              </h2>
              <button
                type="button"
                onClick={() => setDetail(null)}
                aria-label="Close details"
                className="rounded p-0.5 text-muted-foreground hover:text-foreground"
              >
                <X className="h-4 w-4" />
              </button>
            </div>
            <dl className="mt-3 space-y-2 text-xs">
              <div>
                <dt className="text-muted-foreground">Size</dt>
                <dd className="text-foreground">
                  {formatBytes(detail.size)}{" "}
                  <span className="text-muted-foreground">
                    ({detail.size.toLocaleString()} bytes)
                  </span>
                </dd>
              </div>
              <div>
                <dt className="text-muted-foreground">Modified</dt>
                <dd className="text-foreground">
                  <RelativeTime iso={detail.last_modified} />{" "}
                  <span className="text-muted-foreground">({formatDateTime(detail.last_modified)})</span>
                </dd>
              </div>
              {detail.etag && (
                <div>
                  <dt className="text-muted-foreground">ETag</dt>
                  <dd className="font-mono break-all text-foreground">
                    {detail.etag.replace(/"/g, "")}
                  </dd>
                </div>
              )}
            </dl>
            <div className="mt-4 flex flex-wrap gap-2">
              <Button size="sm" variant="outline" onClick={() => void download(detail.name)}>
                <Download className="mr-1.5 h-3.5 w-3.5" />
                Download
              </Button>
              {affordances?.canDelete && (
                <Button size="sm" variant="destructive" onClick={() => setDeleting([detail])}>
                  Delete…
                </Button>
              )}
            </div>
          </TableCard>
        )}
      </div>

      {/* The commands for *this* folder at *this* level, so what someone
          reads here is what they paste: the name is the store’s, the folder
          the one they are standing in, and a command their grant refuses is
          not on the list. */}
      {store && (
        <Section
          className="mt-6"
          title="From the terminal or code"
        >
          <p className="border-b border-border/60 px-4 py-3 text-xs text-muted-foreground">
            {STORE_CAVEAT}
          </p>
          <StorageUsage
            name={store.slug}
            kind={store.kind}
            folder={path}
            access={here}
            className="p-4"
          />
        </Section>
      )}

      {replacing && (
        <Dialog open onOpenChange={(open) => { if (!open) setReplacing(null); }}>
          <DialogContent className="sm:max-w-lg">
            <DialogHeader>
              <DialogTitle>
                {replacing.colliding.length === 1
                  ? "Replace this file?"
                  : `Replace ${replacing.colliding.length} files?`}
              </DialogTitle>
              <DialogDescription>
                The current version can’t be recovered. The rest of the upload goes ahead either way.
              </DialogDescription>
            </DialogHeader>
            <ul className="max-h-52 overflow-y-auto rounded-md border border-border/60 bg-muted/30 p-2 font-mono text-xs">
              {replacing.colliding.map((name) => (
                <li key={name} className="truncate py-0.5 text-foreground">
                  {name}
                </li>
              ))}
            </ul>
            <DialogFooter>
              <Button variant="outline" onClick={() => setReplacing(null)}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                onClick={() => {
                  send(replacing.files);
                  setReplacing(null);
                }}
              >
                Replace and upload
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}

      {deleting && store && (
        <DeleteObjectsDialog
          key={deleting.map((o) => o.name).join("|")}
          storeSlug={slug}
          storeName={store.name}
          objects={deleting}
          open
          onOpenChange={(open) => {
            if (!open) setDeleting(null);
          }}
          onDeleted={() => {
            setSelected(new Set());
            setDetail(null);
            void queryClient.invalidateQueries({ queryKey: ["store-objects", slug] });
          }}
        />
      )}
    </div>
  );
}

/**
 * What was dropped, as files with the paths they had.
 *
 * A folder drop is the one place the folder illusion pays for itself: the entry
 * API gives the relative path each file had on the person’s disk, and that path
 * becomes the object’s name — which is exactly how `2026/08/sales.parquet`
 * comes to exist without anybody typing it. Browsers without the entry API drop
 * the folder silently, so a plain file list is the floor.
 */
async function collectDrop(dt: DataTransfer): Promise<Array<{ file: File; relative: string }>> {
  const out: Array<{ file: File; relative: string }> = [];

  const entries = Array.from(dt.items)
    .map((item) => (item.kind === "file" ? item.webkitGetAsEntry?.() : null))
    .filter(Boolean) as FileSystemEntry[];

  if (entries.length === 0) {
    return Array.from(dt.files).map((file) => ({ file, relative: file.name }));
  }

  async function walk(entry: FileSystemEntry, prefix: string): Promise<void> {
    if (entry.isFile) {
      const file = await new Promise<File>((resolve, reject) =>
        (entry as FileSystemFileEntry).file(resolve, reject),
      );
      out.push({ file, relative: prefix + entry.name });
      return;
    }
    const reader = (entry as FileSystemDirectoryEntry).createReader();
    // readEntries answers at most a hundred at a time and signals the end with
    // an empty batch, which is the one part of this API that bites.
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve, reject) =>
        reader.readEntries(resolve, reject),
      );
      if (batch.length === 0) break;
      for (const child of batch) await walk(child, `${prefix}${entry.name}/`);
    }
  }

  for (const entry of entries) await walk(entry, "");
  return out;
}

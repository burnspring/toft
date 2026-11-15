import { ParentCrumb } from "@/components/parent-crumb";
import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Copy, HardDrive, MoreHorizontal, Plus, ShieldAlert, User, Users, X,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
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
import { Code } from "@/components/code";
import { EmptyState } from "@/components/empty-state";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { Fact, Section, SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { DeleteStorageResourceDialog } from "@/components/delete-storage-dialog";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { api, statusOf } from "@/lib/api";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import type {
  App, StorageAccess, StorageManagedApp, StorageManager, StorageMapping,
  StorageResource, StoreGrant, StoreSubjectType,
} from "@/lib/api";
import {
  BACKING_LABEL, KIND_LABEL, accessBadgeClass, accessLabel, accessLevels, accessSummary,
  mountableAccessLevels, unmountableLevelsNote,
  ceilingSummary, grantLevels, grantSummary, hasDataPlane, manageSummary, mappingRefusal,
  normalizeFolder, storageFolders,
} from "@/lib/storage";
import { DirectoryPicker } from "@/components/directory-picker";
import { StorageFolderField } from "@/components/storage-folder-field";
import { FieldHelp } from "@/components/field-help";
import { StorageUsage } from "@/components/storage-usage";
import { copyToClipboard } from "@/lib/clipboard";
import { resourceWhere } from "@/lib/storage-usage";
import { cn } from "@/lib/utils";

/**
 * One storage resource, and everything attached to it.
 *
 * The three audiences — the apps that mount it, the people who reach it
 * without one, and the delegates who may attach it for themselves — used to be
 * three dialogs opened from three count columns on the inventory, one of them
 * containing a fourth dialog and another a fifth. Each is a small editor with
 * a search field, a table of rows with per-row selects, and a paragraph of
 * consequence to read before touching any of it; none of that is what a modal
 * is for, and stacking two of them made the *page* the thing you could not
 * find your way around.
 *
 * They are sections of a page now, in order of consequence: what an app gets,
 * then who else can reach it, then who else can hand it out. The prose that
 * was crammed into three dialog descriptions and a footnote on the inventory
 * lives with the section it governs.
 */
export function AdminStorageDetailPage() {
  const { slug = "" } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [toDelete, setToDelete] = useState<StorageResource | null>(null);
  const [renaming, setRenaming] = useState(false);

  // The inventory's own query, reused: navigating from the list renders this
  // instantly from cache, and a deep link fetches the same list once. There is
  // no per-resource read to add — every sub-list below is keyed by id, and the
  // id is what this lookup is for.
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["storage-resources"],
    queryFn: api.listStorageResources,
    retry: false,
  });
  const resource = (data?.resources ?? []).find((r) => r.slug === slug);

  const { data: reach } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
  });
  const browsable =
    !!resource && hasDataPlane(resource) &&
    (reach?.stores ?? []).some((s) => s.slug === resource.slug);

  useDocumentTitle(resource?.name);
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["storage-resources"] });

  // The crumb renders above every branch, not only the one that succeeded.
  // This route is not in the admin nav, not on Admin home, and reachable only
  // from one row of the inventory — so the crumb is the only exit the page ever
  // names, and a failed read and a cold load are the two states in which a
  // reader is most stuck. Both had no crumb at all.
  //
  // The refusal sentence is for a 403 or a 404 alone, as on the inventory: a
  // 500 or a dropped connection drawn as "not available here" tells an
  // administrator they are not one.
  if (error && (statusOf(error) === 403 || statusOf(error) === 404)) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/storage" parent="Storage" />
        <EmptyState
          icon={<ShieldAlert className="h-6 w-6" />}
          title={
            statusOf(error) === 404
              ? "App storage isn’t set up on this install"
              : "App storage is for system administrators"
          }
          description={
            statusOf(error) === 404
              ? "It needs a cluster storage class or a mounted filesystem."
              : undefined
          }
        />
      </div>
    );
  }
  if (error) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/storage" parent="Storage" />
        <QueryError
          title="The storage inventory could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  if (isLoading) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/storage" parent="Storage" />
        <Skeleton className="h-6 w-48" />
        <Skeleton className="h-20 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (!resource) {
    return (
      <div className="space-y-6">
        <ParentCrumb to="/admin/storage" parent="Storage" />
        <EmptyState
          icon={<HardDrive className="h-6 w-6" />}
          title="No such resource"
          description="It may have been deleted."
          action={
            <Link to="/admin/storage" className={buttonVariants({ variant: "outline", size: "sm" })}>
              Back to Storage
            </Link>
          }
        />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <ParentCrumb to="/admin/storage" parent="Storage" current={resource.name} />

      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0">
          {/* The name is an identifier people copy into code, so it is set in
              the mono voice; the description is the prose beside it. */}
          <h1 className="font-mono text-2xl font-semibold tracking-tight">{resource.name}</h1>
          {resource.description && (
            <p className="mt-0.5 max-w-prose whitespace-pre-line text-sm text-muted-foreground">
              {resource.description}
            </p>
          )}
        </div>

        <div className="flex shrink-0 items-center gap-2">
          {browsable && (
            <Link
              to={`/admin/storage/${resource.slug}/files`}
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              Browse files
            </Link>
          )}
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8"
                  aria-label={`Actions for ${resource.name}`}
                >
                  <MoreHorizontal className="h-4 w-4" />
                </Button>
              }
            />
            <DropdownMenuContent align="end">
              {/* First, because it is the one item here that takes nothing
                  away: no mapping moves, no grant changes, nothing restarts. */}
              <DropdownMenuItem onClick={() => setRenaming(true)}>
                Edit description…
              </DropdownMenuItem>
              <DropdownMenuItem variant="destructive" onClick={() => setToDelete(resource)}>
                Delete…
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <TableCard className="p-4">
        <dl className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          <Fact label="Kind">{KIND_LABEL[resource.kind]}</Fact>
          <Fact label="Backing">{BACKING_LABEL[resource.backing]}</Fact>
          {/* Fixed at creation, and it decides what every mapping below may
              be — so it is a fact here, not a setting. */}
          <Fact label="Attached">{resource.partitioned ? "By folder only" : "Whole or by folder"}</Fact>
          <Fact label="Created"><RelativeTime iso={resource.created_at} /></Fact>
        </dl>
      </TableCard>

      <Section title="Where it lives" description="For whoever looks after the file system or bucket.">
        <dl className="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2">
          {resourceWhere(resource, data?.drives ?? [], data?.stores_mounted ?? true).map((f) => (
            <Fact key={f.label} label={f.label}>
              <span className="flex min-w-0 items-center gap-1">
                <Code className="truncate px-1.5 text-xs">{f.value}</Code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-6 w-6 shrink-0"
                  aria-label={`Copy ${f.label.toLowerCase()}`}
                  onClick={() => void copyToClipboard(f.value, f.label.toLowerCase())}
                >
                  <Copy className="h-3 w-3" />
                </Button>
              </span>
            </Fact>
          ))}
        </dl>
      </Section>

      <Section
        title="Use it"
        description={
          hasDataPlane(resource)
            ? "From a terminal, for anyone with a grant, and from an app it is attached to."
            : "From an app it is attached to. People can’t reach this one from a terminal."
        }
      >
        <StorageUsage
          name={resource.name}
          kind={resource.kind}
          cli={hasDataPlane(resource)}
          className="p-4"
        />
      </Section>

      <AppsSection resource={resource} onChanged={refresh} />
      <PeopleSection resource={resource} />
      <DelegatesSection resource={resource} onChanged={refresh} />

      {/* Keyed on the resource, so the form seeds from it rather than syncing
          props into state with an effect. */}
      {renaming && (
        <RenameResourceDialog
          key={resource.id}
          resource={resource}
          onOpenChange={() => setRenaming(false)}
          onRenamed={refresh}
        />
      )}
      <DeleteStorageResourceDialog
        resource={toDelete}
        drives={data?.drives ?? []}
        onOpenChange={() => setToDelete(null)}
        onDeleted={() => {
          refresh();
          navigate("/admin/storage");
        }}
      />
    </div>
  );
}

/**
 * What a resource is for — the one thing about it that changes after create.
 *
 * The name is the identifier apps and the CLI type, so it is fixed (ADR §71)
 * and is the dialog's title rather than a field. No confirmation: a description
 * changes no mapping, no grant and no mount, and nothing restarts.
 */
function RenameResourceDialog({
  resource,
  onOpenChange,
  onRenamed,
}: {
  resource: StorageResource;
  onOpenChange: (v: boolean) => void;
  onRenamed: () => void;
}) {
  const [description, setDescription] = useState(resource.description ?? "");
  const [saving, setSaving] = useState(false);

  const dirty = description.trim() !== (resource.description ?? "");

  async function submit() {
    if (!dirty) return;
    setSaving(true);
    try {
      await api.renameStorageResource(resource.id, { description: description.trim() });
      toast.success("Saved");
      onOpenChange(false);
      onRenamed();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t save the description of “${resource.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={(o) => { if (!saving) onOpenChange(o); }}>
      <DialogContent scrollable>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving) void submit();
          }}
        >
        <DialogHeader>
          <DialogTitle>
            Edit description of <span className="font-mono">{resource.name}</span>
          </DialogTitle>
          <DialogDescription>
            The name can’t change: apps and the CLI use it. Nothing restarts.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="rename-storage-description">Description</Label>
            <Textarea
              id="rename-storage-description"
              rows={5}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="What it’s for, and what to know before deleting it"
            />
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || !dirty}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Which apps mount this, and on what terms.
 *
 * Mapping and unmapping are reversible and audited, so neither gets a
 * confirmation step — the app finds out at its next start — a restart or a deploy — either way, which is
 * what the section's own description says.
 */
function AppsSection({
  resource, onChanged,
}: {
  resource: StorageResource;
  onChanged: () => void;
}) {
  const queryClient = useQueryClient();
  const [adding, setAdding] = useState(false);
  // The attachment in flight, so a double click sends one detach rather than
  // two, the second of which toasted an error.
  const [detaching, setDetaching] = useState<string | null>(null);

  const { data: mappings = [], isLoading, error, refetch } = useQuery({
    queryKey: ["storage-mappings", resource.id],
    queryFn: () => api.listStorageMappings(resource.id),
  });

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["storage-mappings", resource.id] });
    // Each app's own Storage block reads its attachments under its own key.
    queryClient.invalidateQueries({ queryKey: ["app-storage"] });
    onChanged();
  }

  async function detach(m: StorageMapping) {
    setDetaching(m.id);
    try {
      // The row's own folder, so unmapping one of an app's two mounts leaves
      // the other. Passing nothing here would detach every folder that app
      // holds of this resource, which is what a Trash icon on one row must not
      // do.
      const res = await api.deleteStorageMapping(resource.id, m.app_id, m.folder ?? "");
      toast.success(`Detached from “${m.app_name}”`, {
        description: res.note ?? "The app loses it at its next restart or deploy.",
      });
      refresh();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t detach “${m.app_name}”`);
    } finally {
      setDetaching(null);
    }
  }

  return (
    <Section
      title="Apps"
      description="Changes apply when each app next restarts or deploys."
      actions={
        <Button size="sm" onClick={() => setAdding(true)}>
          <Plus className="mr-1.5 h-4 w-4" />
          Attach to an app…
        </Button>
      }
    >
      {resource.partitioned && !isLoading && !error && mappings.length > 0 && (
        <FolderList mappings={mappings} />
      )}
      {isLoading ? (
        <div className="p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      ) : error ? (
        <div className="p-4">
          {/* "No app mounts this" is the answer somebody reads before deleting
              a resource, so a failed read must never be printed as one. */}
          <QueryError
            title="The attached apps could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      ) : mappings.length === 0 ? (
        <EmptyState size="inline" title="No app is attached to this yet" />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>App</TableHead>
              <TableHead>Path</TableHead>
              {/* Which directory of the resource is behind that path. Two rows
                  for one app are two folders, and without this column they read
                  as a duplicate. */}
              <TableHead className="hidden @xl:table-cell">Folder</TableHead>
              <TableHead className="w-[130px]">Access</TableHead>
              {/* Which authority wrote the row, because it decides what a
                  revocation later takes: revoking a delegation removes the
                  mappings it made and nothing else, and an administrator
                  editing one of them makes it theirs. */}
              <TableHead className="hidden w-[150px] @3xl:table-cell">Attached by</TableHead>
              <TableHead className="w-[40px]"><span className="sr-only">Detach</span></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {mappings.map((m) => (
              <TableRow key={m.id}>
                <TableCell>
                  <Link to={`/apps/${m.app_id}`} className="text-sm text-primary hover:underline">
                    {m.app_name}
                  </Link>
                </TableCell>
                <TableCell className="font-mono text-xs">{m.path}</TableCell>
                <TableCell className="hidden font-mono text-xs text-muted-foreground @xl:table-cell">
                  {m.folder ? `${m.folder}/` : "—"}
                </TableCell>
                <TableCell>
                  <Badge variant="outline" className={cn("text-xs", accessBadgeClass(m.access))}>
                    {accessLabel(m.access)}
                  </Badge>
                </TableCell>
                <TableCell className="hidden text-sm @3xl:table-cell">
                  {m.source === "manifest" && (
                    <span className="block text-xs text-muted-foreground">From toft.toml, held by</span>
                  )}
                  {m.authority === "admin" ? (
                    <span className="text-muted-foreground">An administrator</span>
                  ) : (
                    <span className="flex items-center gap-1.5">
                      {m.authority.startsWith("group:") ? (
                        <Users className="h-3 w-3 text-muted-foreground" aria-hidden />
                      ) : (
                        <User className="h-3 w-3 text-muted-foreground" aria-hidden />
                      )}
                      {m.authority_name || m.authority}
                      {m.authority.startsWith("group:") && (
                        <span className="text-xs text-muted-foreground">group</span>
                      )}
                    </span>
                  )}
                </TableCell>
                <TableCell>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="h-7 w-7"
                    aria-label={`Detach from ${m.app_name}`}
                    disabled={detaching === m.id}
                    onClick={() => void detach(m)}
                  >
                    <X className="h-3.5 w-3.5" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <AddMappingDialog
        open={adding}
        resource={resource}
        mapped={mappings.map((m) => m.app_id)}
        onOpenChange={setAdding}
        onSaved={refresh}
      />
    </Section>
  );
}

/**
 * A partitioned resource's folders, each with the apps sharing it — the
 * question an administrator brings before mapping another app: is there
 * already a folder for this, and who is on it. Each row is one gate.
 */
function FolderList({ mappings }: { mappings: StorageMapping[] }) {
  return (
    <div className="border-b border-border/60 px-4 py-3">
      <SectionHeading level={3} title="Folders" />
      <ul className="mt-2 space-y-1 text-sm">
        {storageFolders(mappings).map((f) => (
          <li key={f.folder} className="flex flex-wrap items-baseline gap-x-2">
            <span className="font-mono text-xs">{f.folder}/</span>
            <span className="text-muted-foreground">
              {f.apps.map((a, i) => (
                <span key={a.id}>
                  {i > 0 && ", "}
                  <Link to={`/apps/${a.id}`} className="text-primary hover:underline">
                    {a.name}
                  </Link>
                </span>
              ))}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function AddMappingDialog({
  open, resource, mapped, onOpenChange, onSaved,
}: {
  open: boolean;
  resource: StorageResource;
  mapped: string[];
  onOpenChange: (v: boolean) => void;
  onSaved: () => void;
}) {
  const [appId, setAppId] = useState("");
  const [path, setPath] = useState(`/data/${resource.slug}`);
  const [folder, setFolder] = useState("");
  const [access, setAccess] = useState<StorageAccess>("read");
  const [saving, setSaving] = useState(false);
  const [refusal, setRefusal] = useState("");

  const { data: apps = [] } = useQuery<App[]>({
    queryKey: ["apps"],
    queryFn: () => api.listApps(),
    enabled: open,
  });
  // The page's own inventory read, so this costs no request.
  const { data: inventory } = useQuery({
    queryKey: ["storage-resources"],
    queryFn: api.listStorageResources,
    enabled: open,
  });
  const storesMounted = inventory?.stores_mounted ?? true;
  const unmountedStore = resource.kind === "store" && !storesMounted;

  const candidates = useMemo(
    () => apps.filter((a) => !mapped.includes(a.id)),
    [apps, mapped],
  );
  const selected = apps.find((a) => a.id === appId);
  const unmountableNote = unmountableLevelsNote(resource.kind, resource.backing);

  async function submit() {
    if (!appId || !path.trim()) return;
    setSaving(true);
    try {
      const result = await api.putStorageMapping(resource.id, appId, {
        path: path.trim(),
        access,
        folder: folder.trim(),
      });
      toast.success(`Attached to “${selected?.name ?? "the app"}”`, { description: result.note });
      setRefusal("");
      onOpenChange(false);
      onSaved();
    } catch (err) {
      // A refusal of *what* is being mapped — the folder, or the app's count —
      // is said under the folder, where it is answered.
      if (mappingRefusal(err)) setRefusal((err as Error).message);
      else
        toast.error(
          err instanceof Error
            ? err.message
            : `Couldn’t attach “${resource.name}” to “${selected?.name ?? "the app"}”`,
        );
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving) void submit();
          }}
        >
        <DialogHeader>
          <DialogTitle>Attach {resource.name} to an app</DialogTitle>
          <DialogDescription>
            The app gets a folder path. No credential or SDK needed.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="mapping-app">App</Label>
              <Select value={appId} onValueChange={(v) => setAppId(v ?? "")}>
                <SelectTrigger id="mapping-app" className="w-full">
                  {/* base-ui's Value renders the raw value — here, the app's
                      id. Name it. */}
                  <SelectValue>
                    {(v) => candidates.find((a) => a.id === v)?.name ?? "Choose an app…"}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {candidates.map((a) => (
                    <SelectItem key={a.id} value={a.id}>
                      {a.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {/* Asked of the resource rather than of the install, because that
                  is the precise question: a resource under a host mount can be
                  mounted by a shared workload, and one bound in a cluster cannot.
                  Said before the mapping is made rather than discovered at the
                  app's next deploy, which is where it would otherwise surface.
                  A store is exempt: a shared app reaches it over signed URLs. */}
              {selected && selected.execution_mode !== "isolated" && resource.kind !== "store" &&
                !resource.storage_root && (
                <FieldHelp tone="attention">
                  {selected.name} runs shared, and this install has no storage for shared apps. Its
                  next deploy will fail unless you make it isolated.
                </FieldHelp>
              )}
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="mapping-path">Path in the app</Label>
              <Input
                id="mapping-path"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                className="font-mono text-sm"
              />
              {unmountedStore ? (
                <FieldHelp>
                  Used only if the app runs isolated. Here it reaches the store through the SDK.
                </FieldHelp>
              ) : (
                <FieldHelp>
                  An absolute path. Code can also look it up by name through{" "}
                  <code className="text-xs">TOFT_STORAGE</code>.
                </FieldHelp>
              )}
            </div>

            <StorageFolderField
              id="mapping-folder"
              partitioned={resource.partitioned}
              value={folder}
              onChange={(v) => {
                setFolder(v);
                setRefusal("");
              }}
              refusal={refusal}
            />

            <div className="space-y-1.5">
              <Label htmlFor="mapping-access">Access</Label>
              <Select value={access} onValueChange={(v) => setAccess(v as StorageAccess)}>
                <SelectTrigger id="mapping-access" className="w-full">
                  {/* base-ui's Value renders the raw value, not the chosen
                      item's text — format it here. */}
                  <SelectValue>{(v) => accessLabel(v as StorageAccess)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {/* The levels this backing can be *mounted* at, which on an
                      Azure Blob store is two of the four. A rendering of the
                      server's refusal rather than a second opinion about it —
                      the attach refuses either way, and this is what stops the
                      refusal being how somebody finds out. */}
                  {mountableAccessLevels(resource.kind, resource.backing).map((level) => (
                    <SelectItem key={level} value={level}>
                      {accessLabel(level)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>
                {accessSummary(resource.kind, access)}
              </FieldHelp>
              {/* A sentence rather than a disabled item: a disabled option is not
                  focusable and swallows the pointer, so an explanation attached
                  to one is unreachable by every route. */}
              {unmountableNote && (
                <Notice tone="attention">
                  {unmountableNote}
                </Notice>
              )}
            </div>
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={saving || !appId || !path.trim() || (resource.partitioned && !folder.trim())}
          >
            {saving ? "Attaching…" : "Attach"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Who may reach this from *outside* — a person with a grant, no app and no
 * mount.
 *
 * A mapping and a grant answer different questions: a mapping says where a
 * resource appears on an app's filesystem and takes effect at that app's next
 * deploy, and a grant says who may push a file in from a laptop or a nightly
 * cron and takes effect on the next URL minted. A store no app mounts still
 * has an audience, and a page that showed only one of the two described a
 * resource as unused right up until somebody deleted it.
 *
 * **Where a grant is impossible, the section says so.** A volume backed by EFS
 * or a host mount has nothing to sign for, and the inventory used to carry
 * that fact as an em dash whose explanation was in a `title` attribute — which
 * no keyboard and no screen reader could reach.
 */
function PeopleSection({ resource }: { resource: StorageResource }) {
  const queryClient = useQueryClient();
  /** Which folder a new grant lands on. Empty is the whole store. */
  const [grantFolder, setGrantFolder] = useState("");
  /** And at what level. It used to be a literal in the click handler, so a
   *  single-click add committed at an unlabelled default — and the two
   *  sections on this page defaulted differently. */
  const [grantLevel, setGrantLevel] = useState<StorageAccess>("write");
  const [pendingKey, setPendingKey] = useState<string | null>(null);
  const enabled = hasDataPlane(resource);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["store-grants", resource.id],
    queryFn: () => api.listStoreGrants(resource.id),
    enabled,
  });
  const grants = data?.grants ?? [];

  // Held *at the folder being granted into*: somebody with a root grant can
  // still be given a narrower one, and somebody granted `analytics/` is not
  // already granted `finance/`. Keying the set on the scope is what keeps the
  // picker from hiding a person who has no grant where you are looking — and
  // it is why `DirectoryPicker` takes a predicate rather than a set of ids.
  const held = new Set(grants.map((g) => `${g.subject_type}:${g.subject_id}:${g.folder ?? ""}`));
  const scopeKey = normalizeFolder(grantFolder);

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["store-grants", resource.id] });
    queryClient.invalidateQueries({ queryKey: ["storage-resources"] });
  }

  // The folder is an argument and has no default. A grant is identified by its
  // scope as much as by its subject — the same rule `revoke()` states — and the
  // row control used to reach a closure over the *add form's* folder, so
  // changing one person's level on `finance/` while the form sat empty wrote a
  // root grant and toasted success.
  async function grant(
    subjectType: StoreSubjectType, subjectId: string, name: string, access: StorageAccess,
    folder: string,
  ) {
    const scope = normalizeFolder(folder);
    setPendingKey(`${subjectType}:${subjectId}`);
    try {
      await api.putStoreGrant(resource.id, {
        subject_type: subjectType, subject_id: subjectId, access, folder: scope,
      });
      refresh();
      toast.success(
        scope
          ? `${name} now has ${accessLabel(access)} on ${resource.name}/${scope}/`
          : `${name} now has ${accessLabel(access)} on ${resource.name}`,
        { description: grantSummary(access) },
      );
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t give “${name}” access`);
    } finally {
      setPendingKey(null);
    }
  }

  async function revoke(g: StoreGrant) {
    setPendingKey(`${g.subject_type}:${g.subject_id}`);
    try {
      // The scope, not the subject: a folder grant and a root grant are
      // separate rows, granted separately and audited separately, so revoking
      // one leaves the other standing.
      await api.deleteStoreGrant(resource.id, g.subject_type, g.subject_id, g.folder);
      refresh();
      toast.success(`Revoked “${g.subject_name || g.subject_id}”’s access`, {
        description: "Links already issued work for up to an hour.",
      });
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : `Couldn’t revoke “${g.subject_name || g.subject_id}”’s access`,
      );
    } finally {
      setPendingKey(null);
    }
  }

  if (!enabled) {
    return (
      <Section
        title="People"
        description="Who can reach this without an app, from a laptop or a scheduled script."
      >
        <div className="p-4">
          <Notice tone="neutral" size="sm" title="This resource takes no grants">
            {BACKING_LABEL[resource.backing]} can only be read by an app it’s attached to. For storage
            people can browse, use an object store.
          </Notice>
        </div>
      </Section>
    );
  }

  return (
    <Section
      title="People"
      description={
        <>
          Who can upload and download without an app, with{" "}
          <code className="font-mono">toft store</code> or the storage browser.
        </>
      }
    >
      {/* Grants can be recorded on an install that cannot yet mint URLs, so the
          section says so rather than letting somebody discover it from a 404 in
          the CLI. */}
      {data && !data.uploads_enabled && (
        <div className="px-4 pt-4">
          <Notice tone="attention" size="sm" title="Uploads are not switched on here">
            Access granted here won’t work until whoever runs Toft turns on store uploads.
          </Notice>
        </div>
      )}

      {error ? (
        <div className="p-4">
          <QueryError
            title="Who can reach this could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      ) : isLoading ? (
        <div className="p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      ) : (
        <>
          <div className="grid gap-4 p-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="grant-store-folder">Folder</Label>
              <Input
                id="grant-store-folder"
                value={grantFolder}
                onChange={(e) => setGrantFolder(e.target.value)}
                placeholder="the whole store"
                className="font-mono text-sm"
              />
              <FieldHelp>
                Grant one folder instead of the whole store. It never narrows a wider grant.
              </FieldHelp>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="grant-store-level">Access</Label>
              <Select value={grantLevel} onValueChange={(v) => setGrantLevel(v as StorageAccess)}>
                <SelectTrigger id="grant-store-level" className="w-full">
                  <SelectValue>{(v) => accessLabel(v as StorageAccess)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {grantLevels().map((l) => (
                    <SelectItem key={l} value={l}>
                      {accessLabel(l)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>
                What everyone you add below gets.
              </FieldHelp>
            </div>

            <DirectoryPicker
              id="grant-store-search"
              label="Add people or groups"
              exclude={(kind, id) => held.has(`${kind}:${id}:${scopeKey}`)}
              pendingKey={pendingKey ?? undefined}
              rowSuffix={() => `add with ${accessLabel(grantLevel).toLowerCase()}`}
              onPick={(kind, id, label) => grant(kind, id, label, grantLevel, grantFolder)}
            />
          </div>

          {grants.length === 0 ? (
            <div className="border-t border-border/60">
              <EmptyState size="inline" title="Nobody can reach this from outside yet" />
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Person or group</TableHead>
                  <TableHead className="w-[150px]">Access</TableHead>
                  <TableHead className="w-[40px]"><span className="sr-only">Revoke</span></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {grants.map((g) => (
                  <TableRow key={g.id}>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        {g.subject_type === "group" ? (
                          <Users className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
                        ) : (
                          <User className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
                        )}
                        <span className="font-medium">{g.subject_name || g.subject_id}</span>
                        {g.subject_type === "group" && (
                          <span className="text-xs text-muted-foreground">group</span>
                        )}
                      </div>
                      {g.subject_email && (
                        <div className="text-xs text-muted-foreground">{g.subject_email}</div>
                      )}
                      {g.folder && (
                        <div className="font-mono text-2xs text-muted-foreground">{g.folder}/</div>
                      )}
                    </TableCell>
                    <TableCell>
                      <Select
                        value={g.access}
                        onValueChange={(v) =>
                          grant(g.subject_type, g.subject_id,
                            g.subject_name || g.subject_id, v as StorageAccess, g.folder ?? "")
                        }
                      >
                        <SelectTrigger
                          className="h-8 w-full"
                          aria-label={`Access for ${g.subject_name || g.subject_id}`}
                        >
                          {/* base-ui’s Value renders the stored word, not the
                              chosen item’s text — format it here. */}
                          <SelectValue>{(v) => accessLabel(v as StorageAccess)}</SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          {grantLevels().map((level) => (
                            <SelectItem key={level} value={level}>
                              {accessLabel(level)}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-7 w-7"
                        disabled={pendingKey === `${g.subject_type}:${g.subject_id}`}
                        aria-label={`Revoke access for ${g.subject_name || g.subject_id}`}
                        onClick={() => revoke(g)}
                      >
                        <X className="h-3.5 w-3.5" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}
    </Section>
  );
}

/**
 * Who can attach this resource without being an administrator.
 *
 * The section whose *removal* is the feature: revoking a delegation deletes the
 * mappings it wrote and restarts the apps that were carrying them. So each row
 * carries the apps it put a mount on — by name, because the administrator is
 * about to restart those apps and a number does not let them warn anybody.
 */
function DelegatesSection({
  resource, onChanged,
}: {
  resource: StorageResource;
  onChanged: () => void;
}) {
  const queryClient = useQueryClient();
  /** The ceiling a new delegation lands at — a control now rather than a
   *  literal in the click handler. */
  const [delegateLevel, setDelegateLevel] = useState<StorageAccess>("read");
  const [pendingKey, setPendingKey] = useState<string | null>(null);
  const [toRevoke, setToRevoke] = useState<StorageManager | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["storage-managers", resource.id],
    queryFn: () => api.listStorageManagers(resource.id),
  });
  const managers = data?.managers ?? [];
  const kind = resource.kind;

  // A delegation stays whole-resource: a delegate may *create* a folder-scoped
  // mapping, but who may attach is a fact about the resource. So this set is
  // keyed on the subject alone, unlike the grants section's.
  const held = new Set(managers.map((m) => `${m.subject_type}:${m.subject_id}`));

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["storage-managers", resource.id] });
    // The mappings list carries who attached each row, and a revocation removes
    // some of them — so it is stale the moment this changes.
    queryClient.invalidateQueries({ queryKey: ["storage-mappings", resource.id] });
    queryClient.invalidateQueries({ queryKey: ["app-storage"] });
    // And a delegation on a store confers its data plane, so what this
    // administrator can browse may have just changed too.
    queryClient.invalidateQueries({ queryKey: ["stores"] });
    onChanged();
  }

  async function delegate(
    subjectType: StoreSubjectType, subjectId: string, name: string, maxAccess: StorageAccess,
  ) {
    setPendingKey(`${subjectType}:${subjectId}`);
    try {
      await api.putStorageManager(resource.id, {
        subject_type: subjectType, subject_id: subjectId, max_access: maxAccess,
      });
      refresh();
      toast.success(`“${name}” can now attach “${resource.name}”`, {
        description: ceilingSummary(resource.kind, maxAccess),
      });
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : `Couldn’t let “${name}” attach “${resource.name}”`,
      );
    } finally {
      setPendingKey(null);
    }
  }

  return (
    <Section
      title="Delegates"
      description={
        <>
          {manageSummary({ kind, hasDataPlane: !!data?.has_data_plane })}
        </>
      }
    >
      {error ? (
        <div className="p-4">
          <QueryError
            title="Who can attach this could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      ) : isLoading ? (
        <div className="p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      ) : (
        <>
          <div className="grid gap-4 p-4 sm:max-w-lg sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="delegate-level">Up to</Label>
              <Select
                value={delegateLevel}
                onValueChange={(v) => setDelegateLevel(v as StorageAccess)}
              >
                <SelectTrigger id="delegate-level" className="w-full">
                  <SelectValue>{(v) => accessLabel(v as StorageAccess)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {accessLevels(kind).map((l) => (
                    <SelectItem key={l} value={l}>
                      {accessLabel(l)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {/* Seeded at the narrowest rung, deliberately. Raising one is a
                  select away; a delegation that arrived at `full` because that
                  was convenient is the escalation this feature exists to
                  bound. It is a control rather than a footnote now, because a
                  single-click add committing at an unstated default is what it
                  used to be — and the two sections of this page defaulted
                  differently. */}
              <FieldHelp>
                The most access everyone you add below can attach with.
              </FieldHelp>
            </div>

            <DirectoryPicker
              id="delegate-search"
              label="Add people or groups"
              exclude={(k, id) => held.has(`${k}:${id}`)}
              pendingKey={pendingKey ?? undefined}
              rowSuffix={() => `add with ${accessLabel(delegateLevel).toLowerCase()}`}
              onPick={(k, id, label) => delegate(k, id, label, delegateLevel)}
            />
          </div>

          {managers.length === 0 ? (
            <div className="border-t border-border/60">
              <EmptyState size="inline" title="Only administrators can attach this" />
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Person or group</TableHead>
                  <TableHead className="w-[150px]">Up to</TableHead>
                  <TableHead>Attached to</TableHead>
                  <TableHead className="w-[40px]"><span className="sr-only">Revoke</span></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {managers.map((m) => (
                  <TableRow key={m.id}>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        {m.subject_type === "group" ? (
                          <Users className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
                        ) : (
                          <User className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
                        )}
                        <span className="font-medium">{m.subject_name || m.subject_id}</span>
                        {m.subject_type === "group" && (
                          <span className="text-xs text-muted-foreground">group</span>
                        )}
                      </div>
                      {m.subject_email && (
                        <div className="text-xs text-muted-foreground">{m.subject_email}</div>
                      )}
                    </TableCell>
                    <TableCell>
                      <Select
                        value={m.max_access}
                        onValueChange={(v) =>
                          delegate(m.subject_type, m.subject_id,
                            m.subject_name || m.subject_id, v as StorageAccess)
                        }
                      >
                        <SelectTrigger
                          className="h-8 w-full"
                          aria-label={`Highest access for ${m.subject_name || m.subject_id}`}
                        >
                          {/* base-ui’s Value renders the stored word, not the
                              chosen item’s text — format it here. */}
                          <SelectValue>{(v) => accessLabel(v as StorageAccess)}</SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          {accessLevels(kind).map((level) => (
                            <SelectItem key={level} value={level}>
                              {accessLabel(level)}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </TableCell>
                    <TableCell className="text-sm">
                      {m.apps.length === 0 ? (
                        <span className="text-muted-foreground">Nothing yet</span>
                      ) : (
                        <span className="flex flex-wrap gap-x-1.5 gap-y-0.5">
                          {m.apps.map((a) => (
                            <Link
                              key={a.id}
                              to={`/apps/${a.id}`}
                              className="text-primary hover:underline"
                            >
                              {a.name}
                            </Link>
                          ))}
                        </span>
                      )}
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-7 w-7"
                        disabled={pendingKey === `${m.subject_type}:${m.subject_id}`}
                        aria-label={`Revoke ${m.subject_name || m.subject_id}…`}
                        onClick={() => setToRevoke(m)}
                      >
                        <X className="h-3.5 w-3.5" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}

      <RevokeManagerDialog
        key={toRevoke?.id}
        resource={resource}
        manager={toRevoke}
        onOpenChange={() => setToRevoke(null)}
        onRevoked={refresh}
      />
    </Section>
  );
}

/**
 * Revoking, with the blast radius named before the click.
 *
 * A plain confirm dialog rather than type-to-confirm: nothing is destroyed, the
 * data is retained, and the delegation can be granted again. But it is not a
 * bare action-plus-toast either — that is what the confirmation policy reserves
 * for a change reaching only the thing in front of you, and this one restarts
 * apps belonging to other people.
 */
function RevokeManagerDialog({
  resource, manager, onOpenChange, onRevoked,
}: {
  resource: StorageResource;
  manager: StorageManager | null;
  onOpenChange: () => void;
  onRevoked: () => void;
}) {
  const [saving, setSaving] = useState(false);
  const apps: StorageManagedApp[] = manager?.apps ?? [];
  const who = manager?.subject_name || manager?.subject_id || "";

  async function revoke() {
    if (!manager) return;
    setSaving(true);
    try {
      const result = await api.deleteStorageManager(
        resource.id, manager.subject_type, manager.subject_id);
      toast.success(`Revoked “${who}”`, { description: result.note });
      onOpenChange();
      onRevoked();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t revoke “${who}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={!!manager} onOpenChange={onOpenChange}>
      <DialogContent scrollable>
        <DialogHeader>
          <DialogTitle>Revoke {who}?</DialogTitle>
          <DialogDescription>
            They stop being able to attach {resource.name}
            {resource.kind === "store" && ", and stop being able to reach its files"}. The data
            itself is untouched.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          {apps.length > 0 ? (
            <Notice
              tone="attention"
              size="sm"
              title={`${apps.length} app${apps.length === 1 ? "" : "s"} will be restarted`}
            >
              <p>
                What {who} attached is detached, and each app below restarts on its{" "}
                <strong>current deployment</strong> without it.
              </p>
              <ul className="mt-2 space-y-0.5">
                {apps.map((a) => (
                  <li key={a.id}>
                    <span className="font-medium">{a.name}</span>{" "}
                    <span className="font-mono text-xs">{a.path}</span>
                  </li>
                ))}
              </ul>
            </Notice>
          ) : (
            <p className="text-sm text-muted-foreground">
              They have not attached this to anything, so no app is restarted.
            </p>
          )}
        </DialogBody>

        <DialogFooter>
          <Button variant="outline" onClick={onOpenChange} disabled={saving}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={revoke} disabled={saving}>
            {saving ? "Revoking…" : "Revoke"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { HardDrive, MoreHorizontal, Plus, ShieldAlert } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Checkbox } from "@/components/ui/checkbox";
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
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { EmptyState } from "@/components/empty-state";
import { DeleteStorageResourceDialog } from "@/components/delete-storage-dialog";
import { api, statusOf } from "@/lib/api";
import type {
  StorageBacking, StorageDrive, StorageKind, StorageResource, StorageRoot,
} from "@/lib/api";
import {
  KIND_LABEL, KIND_SUMMARY, STORE_CAVEAT, backingLabel, backingSummary,
  hasDataPlane, reachTerms, resourceLocation,
} from "@/lib/storage";
import { Notice } from "@/components/notice";
import { Code } from "@/components/code";
import { RelativeTime } from "@/components/relative-time";
import { TableCard } from "@/components/table-card";
import { FieldError, FieldHelp } from "@/components/field-help";
import { DrivesSection, RepointDriveDialog } from "@/pages/admin/storage-drives";
import { StorageDefaultsSection } from "@/pages/admin/storage-defaults";
import { fieldErrorId } from "@/lib/utils";
import { primaryWhere, storageNameProblem } from "@/lib/storage-usage";

/**
 * Admin → Storage.
 *
 * An administrator names the durable locations and decides which apps reach
 * them. There is deliberately no self-service path: a volume is one access
 * point against a filesystem with a hard quota, and mapping one onto an app is
 * a grant of durable state that outlives every release the app will ever have.
 *
 * **This page is the inventory and nothing else.** It used to be the inventory
 * *and* three audience editors, reached through three count columns that each
 * opened a different dialog — one of which opened another dialog inside
 * itself. Eight columns of it overflowed the card below 1100px, clipping the
 * row's actions menu, which was the only route to Browse and Delete. So each
 * resource now has a page (`storage-detail.tsx`), and a row here answers one
 * question: what is this, where does it live, and who touches it.
 */
/** One sentence about this page, said the same way on every branch.
 *  Each of these files hand-wrote a second, shorter one for its error and
 *  loading paths, so the page described itself one way to whoever loaded it
 *  and another to whoever was refused. */
const PAGE_DESCRIPTION =
  "Durable storage you create and give to apps. Apps get a path, never a credential.";

export function AdminStoragePage() {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [toDelete, setToDelete] = useState<StorageResource | null>(null);
  const [repointing, setRepointing] = useState<StorageDrive | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["storage-resources"],
    queryFn: api.listStorageResources,
    retry: false,
  });

  const navigate = useNavigate();
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["storage-resources"] });
  const [releasing, setReleasing] = useState<string | null>(null);
  const releaseName = async (slug: string) => {
    setReleasing(slug);
    try {
      await api.releaseKeptStorageName(slug);
      toast.success(`“${slug}” is free to use again`);
      await refresh();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t release “${slug}”`);
    } finally {
      setReleasing(null);
    }
  };

  // What *this administrator* reaches, which is not the same question as what
  // they can provision. There is no admin bypass on a store: an administrator
  // who wants to browse one grants themselves, which is one audited call and
  // strictly better than a reach no audit row explains. So the Browse item is
  // present only when they already did.
  const { data: reach } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
  });
  const reachable = new Set((reach?.stores ?? []).map((s) => s.slug));

  // The route 404s on an install with no isolated backend and 403s for a
  // non-admin, and the page cannot tell those two apart from here. Both mean
  // "this is not yours to see", so one explanation covers them — and **only**
  // them. Every other failure used to get the same sentence, so a 500 or a
  // dropped connection told an administrator they were not an administrator,
  // which is the wrong thing to read at the wrong moment. `QueryError` leads
  // with the server's own words.
  if (error && (statusOf(error) === 403 || statusOf(error) === 404)) {
    return (
      <div>
        <PageHeader title="Storage" description={PAGE_DESCRIPTION} />
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
      <div>
        <PageHeader title="Storage" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The storage inventory could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  const resources = data?.resources ?? [];
  const kinds = data?.kinds ?? [];
  // The server's cap, not a copy of it. Zero until the list answers.
  const cap = data?.max_resources ?? 0;
  const atCap = cap > 0 && resources.length >= cap;
  const drives = data?.drives ?? [];
  const stranded = drives.filter((d) => !d.declared && d.resource_count > 0);
  const deleting = (data?.deleting ?? []).filter((d) => !d.kept);
  const kept = (data?.deleting ?? []).filter((d) => d.kept);

  return (
    <div>
      <PageHeader
        title="Storage"
        description={PAGE_DESCRIPTION}
        actions={
          <div className="flex items-center gap-3">
            {cap > 0 && (
              <span className="text-xs tabular-nums text-muted-foreground">
                {resources.length} of {cap}
              </span>
            )}
            <Button
              size="sm"
              onClick={() => setCreating(true)}
              disabled={kinds.length === 0 || atCap}
            >
              <Plus className="mr-1.5 h-4 w-4" />
              New resource
            </Button>
          </div>
        }
      />

      <LicenseLock feature="app_storage" />

      <div className="space-y-4">
        {atCap && (
          <Notice size="md" title={`This install is at its limit of ${cap} storage resources`}>
            Delete one to make room, or attach folders of a partitioned resource instead —
            folders don’t count.
          </Notice>
        )}
        {stranded.length > 0 && (
          <Notice
            size="md"
            tone="attention"
            title={`Resources are on a drive this install no longer declares`}
          >
            {stranded.map((d) => `${d.label} (${d.resource_count})`).join(", ")}. Apps on them
            won’t start until they’re moved to the drive that now holds their files.{" "}
            <a href="#drives" className="text-primary hover:underline">See Drives</a>
          </Notice>
        )}
        {deleting.length > 0 && (
          <Notice size="md" tone="info" title="Being deleted">
            {deleting.map((d, i) => (
              <span key={d.slug}>
                {i > 0 && ", "}
                <Code>{d.slug}</Code> (started <RelativeTime iso={d.started_at} />)
              </span>
            ))}
            . The cluster is removing their files; each name is free to use again once it has
            finished — usually minutes, at most an hour while a finished job still holds one.
          </Notice>
        )}
        {kept.length > 0 && (
          <Notice size="md" tone="attention" title="Names held by kept files">
            {kept.map((d, i) => (
              <span key={d.slug}>
                {i > 0 && ", "}
                <Code>{d.slug}</Code>{" "}
                <Button
                  size="sm"
                  variant="link"
                  className="h-auto p-0"
                  disabled={releasing === d.slug}
                  onClick={() => void releaseName(d.slug)}
                >
                  {releasing === d.slug ? "Releasing…" : "Release name"}
                </Button>
              </span>
            ))}
            . These volumes were deleted and their files kept at <Code>/lpv-&lt;name&gt;</Code>. A
            new volume of the same name would be handed them, so the name is held. Remove the
            directory first, or release the name only if the next resource may have its files.
          </Notice>
        )}
        {kinds.length === 0 && !isLoading && (
          <Notice size="md">
            Nothing can be created yet. Whoever runs Toft has to configure a storage class
            or a host mount first.
          </Notice>
        )}

        {isLoading ? (
          <TableCard className="space-y-3 p-4">
            {Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className="flex items-center gap-3">
                <Skeleton className="h-4 w-40" />
                <Skeleton className="h-4 w-20" />
                <Skeleton className="ml-auto h-4 w-16" />
              </div>
            ))}
          </TableCard>
        ) : resources.length === 0 ? (
          // With nothing creatable, the Notice above is the instruction; a
          // "Create a…" under it, beside a disabled New resource, contradicts it.
          <EmptyState
            icon={<HardDrive className="h-6 w-6" />}
            title="No storage yet"
            description={
              kinds.length === 0
                ? undefined
                : "Create a volume for app state, or an object store for datasets and reports."
            }
          />
        ) : (
          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Resource</TableHead>
                  {/* Kind and Backing were two columns answering overlapping
                      halves of one question, and the Kind badge answered its
                      half wrongly: a volume backed by Amazon S3 Files is a
                      volume that people can browse. One cell, and the address
                      under it is the part that never lies. */}
                  <TableHead className="hidden @3xl:table-cell">Location</TableHead>
                  <TableHead className="w-[260px]">Reach</TableHead>
                  <TableHead className="w-[50px]"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {resources.map((res) => (
                  // The whole row goes to the resource, because the row *is*
                  // the resource — everything in it is a summary of what that
                  // page holds. The name stays a real link: a row handler is a
                  // convenience for a pointer, and it is the link that a
                  // keyboard reaches, that a middle click opens in a tab, and
                  // that says where it goes before you commit to it.
                  <TableRow
                    key={res.id}
                    className="cursor-pointer"
                    onClick={() => navigate(`/admin/storage/${res.slug}`)}
                  >
                    <TableCell>
                      <Link
                        to={`/admin/storage/${res.slug}`}
                        className="font-mono font-medium text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                      >
                        {res.name}
                      </Link>
                      {res.partitioned && (
                        <div className="text-xs text-muted-foreground">Partitioned</div>
                      )}
                      {res.description && (
                        <div className="mt-0.5 text-xs text-muted-foreground">{res.description}</div>
                      )}
                      {/* Location is a column at `@3xl`. Below it, the kind is
                          here — a table may hide a column, but not the only
                          route to what it holds, and what a resource *is* is
                          not a fact a phone should have to do without. The
                          tier is the column's own and a container one; on the
                          viewport it printed the location twice for the whole
                          band between 1024px and the card's `@3xl`. */}
                      <div className="mt-0.5 text-xs text-muted-foreground @3xl:hidden">
                        {resourceLocation(res)}
                      </div>
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                      <div>{resourceLocation(res)}</div>
                      <span className="font-mono text-xs">
                        {primaryWhere(res, data?.drives ?? [], data?.stores_mounted ?? true)}
                      </span>
                    </TableCell>
                    {/* One question, one grammar, one destination. The three
                        audiences were three link columns reading `4`,
                        `6 people, 2 groups`, `Nobody` and `—` side by side. */}
                    <TableCell className="text-sm text-muted-foreground">
                      <span className="flex flex-wrap items-center gap-x-1.5">
                        {reachTerms(res).map((term, i) => (
                          <span key={i} className="flex items-center gap-x-1.5">
                            {i > 0 && <span aria-hidden="true">·</span>}
                            {term.kind === "count" ? (
                              <span>
                                <span
                                  className={
                                    term.count > 0
                                      ? "font-medium tabular-nums text-foreground"
                                      : "tabular-nums"
                                  }
                                >
                                  {term.count}
                                </span>{" "}
                                {term.noun}
                              </span>
                            ) : (
                              <span>{term.text}</span>
                            )}
                          </span>
                        ))}
                      </span>
                    </TableCell>
                    {/* The one part of the row that is not the row's
                        destination: without this, opening the menu — and
                        picking anything in it — also navigated away from the
                        list you opened it on. It sits on the cell rather than
                        on the trigger because the menu's popup is portalled
                        out of the table and into the body: a click on an item
                        never touches this `<td>` in the DOM, but it still
                        bubbles to it through the React tree, which is what
                        made Delete open its dialog and then leave for the
                        detail page. */}
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger
                          render={
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7"
                              aria-label={`Actions for ${res.name}`}
                            >
                              <MoreHorizontal className="h-4 w-4" />
                            </Button>
                          }
                        />
                        <DropdownMenuContent align="end">
                          {/* No "Manage…" item: the row is already the way to
                              the resource, and a menu entry for the thing
                              clicking anywhere else does is the third route to
                              one page. What is left here is what the row is
                              *not*. */}
                          {/* Only where there are objects behind it, and only
                              where this administrator already granted
                              themselves. */}
                          {hasDataPlane(res) && reachable.has(res.slug) && (
                            <DropdownMenuItem onClick={() => navigate(`/admin/storage/${res.slug}/files`)}>
                              Browse files
                            </DropdownMenuItem>
                          )}
                          <DropdownMenuItem variant="destructive" onClick={() => setToDelete(res)}>
                            Delete…
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableCard>
        )}

        <StorageDefaultsSection onChanged={refresh} />

        <DrivesSection drives={drives} onChanged={refresh} onRepoint={setRepointing} />
      </div>

      {/* Mounted only while it is open, and keyed on the kinds this install
          actually offers. It seeds Kind from `kinds[0]`, and mounted from the
          start that was the empty array the query had not answered yet — so a
          store-only install opened this dialog on "Volume" and let it be
          submitted. */}
      {creating && (
        <CreateResourceDialog
          key={kinds.join(",")}
          kinds={kinds}
          backings={data?.backings ?? {}}
          roots={declaredRoots(data?.drives ?? [])}
          classes={(data?.drives ?? []).filter((d) => d.kind === "class" && d.declared)}
          storesMounted={data?.stores_mounted ?? true}
          partitionedRoots={data?.partitioned_roots ?? false}
          onOpenChange={setCreating}
          onCreated={refresh}
        />
      )}
      {repointing && (
        <RepointDriveDialog
          key={repointing.key}
          drive={repointing}
          targets={(data?.drives ?? []).filter((d) => d.declared && d.kind === repointing.kind)}
          onOpenChange={() => setRepointing(null)}
          onDone={refresh}
        />
      )}
      <DeleteStorageResourceDialog
        resource={toDelete}
        drives={data?.drives ?? []}
        onOpenChange={() => setToDelete(null)}
        onDeleted={refresh}
      />
    </div>
  );
}

/** A class drive as the select prints it: its label, the class when different. */
function classTitle(c: StorageDrive): string {
  const base = c.label !== c.name ? `${c.label} (${c.name})` : c.name;
  return c.default ? `${base} — default` : base;
}

/** The declared host roots, as the create form offers them, with their labels. */
function declaredRoots(drives: StorageDrive[]): StorageRoot[] {
  return drives
    .filter((d) => d.kind === "root" && d.declared)
    .map((d) => ({ name: d.name, path: d.path ?? "", readOnly: d.read_only, label: d.label }));
}

/** A root as a select prints it: its label, its name when different, read-only. */
function rootTitle(root: StorageRoot | undefined): string {
  if (!root) return "";
  const base = root.label && root.label !== root.name ? `${root.label} (${root.name})` : root.name;
  return root.readOnly ? `${base} — read-only` : base;
}

function CreateResourceDialog({
  kinds, backings, roots, classes, storesMounted, partitionedRoots, onOpenChange, onCreated,
}: {
  /** What this install offers. Never empty by the time this is mounted — the
   * caller mounts it only while it is open and keys it on this. */
  kinds: StorageKind[];
  /** What can be underneath each kind here. A backing with no class or root
   * configured is absent from this, so the form only ever offers what will
   * provision — the same rule `kinds` follows one level up. */
  backings: Partial<Record<StorageKind, StorageBacking[]>>;
  /** The operator's host mounts, empty on an install with a cluster. */
  roots: StorageRoot[];
  /** A cluster's declared classes — its drives. Empty on a host install. */
  classes: StorageDrive[];
  /** False with no cluster, where a store is reached over signed URLs alone. */
  storesMounted: boolean;
  /** Whether a partitioned resource may go under a host mount here. */
  partitionedRoots: boolean;
  onOpenChange: (v: boolean) => void;
  onCreated: () => void;
}) {
  const [kind, setKind] = useState<StorageKind>(kinds[0] ?? "volume");
  const offered = backings[kind] ?? [];
  const [backing, setBacking] = useState<StorageBacking | "">("");
  // The chosen backing, or the first this kind offers. Derived rather than
  // synced by an effect: changing the kind changes what is on offer, and a
  // stale choice from the other kind would submit a pair the server refuses.
  const chosen: StorageBacking | undefined =
    backing && offered.includes(backing) ? backing : offered[0];
  const [name, setName] = useState("");
  // One mount is not a question, so it is chosen and stated — the rule the
  // backing already follows below.
  const [root, setRoot] = useState(roots.length === 1 ? roots[0].name : "");
  const [bucket, setBucket] = useState("");
  const [prefix, setPrefix] = useState("");
  const [description, setDescription] = useState("");
  const [partitioned, setPartitioned] = useState(false);
  const [classChoice, setClassChoice] = useState("");
  const [saving, setSaving] = useState(false);

  // The bucket is asked for by *backing*, never by kind: an S3 Files volume is
  // a bucket mounted as a filesystem, and naming it is what gives the resource
  // a data plane at all.
  const needsBucket = chosen === "mountpoint" || chosen === "s3files" || chosen === "blob";
  const needsRoot = chosen === "host";
  // A cluster's drives for the chosen backing. One is stated; several are a
  // choice, starting on the default — the class a volume naming none gets.
  const classesHere = kind === "volume" && !needsRoot
    ? classes.filter((c) => !c.backing || c.backing === chosen)
    : [];
  const defaultClass = classesHere.find((c) => c.default)?.name ?? "";
  const chosenClass = classesHere.some((c) => c.name === classChoice)
    ? classChoice
    : defaultClass || (classesHere.length === 1 ? classesHere[0].name : "");
  const chosenRoot = roots.find((r) => r.name === root);
  // Absent rather than disabled where the create would refuse it: under a host
  // mount, partitioning needs every app on an account of its own. On a
  // cluster the folder rules hold and Kubernetes does the confining.
  const offersPartitioned = needsRoot ? partitionedRoots : true;

  const nameProblem = storageNameProblem(name.trim());
  const ready =
    kinds.includes(kind) &&
    !!chosen &&
    !!name.trim() &&
    !nameProblem &&
    (!needsBucket || (!!bucket.trim() && !!prefix.trim())) &&
    (!needsRoot || !!root) &&
    (classesHere.length <= 1 || !!chosenClass);

  function reset() {
    setName("");
    setRoot(roots.length === 1 ? roots[0].name : "");
    setBucket("");
    setPrefix("");
    setDescription("");
    setPartitioned(false);
    setClassChoice("");
  }

  async function submit() {
    if (!ready) return;
    setSaving(true);
    try {
      await api.createStorageResource({
        name: name.trim(),
        kind,
        backing: chosen,
        storage_root: needsRoot ? root : undefined,
        storage_class: classesHere.length > 1 ? chosenClass : undefined,
        bucket: needsBucket ? bucket.trim() : undefined,
        prefix: needsBucket ? prefix.trim() : undefined,
        description: description.trim(),
        partitioned: offersPartitioned && partitioned,
      });
      toast.success(`Created “${name.trim()}”`);
      reset();
      onOpenChange(false);
      onCreated();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t create “${name.trim()}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent scrollable>
        {/* `contents`, so the form adds Enter-to-submit without becoming a box
            between the scrollable popup and its header, body and footer. */}
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving && ready) void submit();
          }}
        >
        <DialogHeader>
          <DialogTitle>New storage resource</DialogTitle>
          <DialogDescription>
            Uses a filesystem or bucket that already exists. Toft never creates one.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="resource-kind">Kind</Label>
              <Select value={kind} onValueChange={(v) => setKind(v as StorageKind)}>
                <SelectTrigger id="resource-kind" className="w-full">
                  {/* base-ui's Value renders the raw value, not the chosen
                      item's text — format it here. */}
                  <SelectValue>{(v) => KIND_LABEL[v as StorageKind]}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {kinds.map((k) => (
                    <SelectItem key={k} value={k}>
                      {KIND_LABEL[k]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>{KIND_SUMMARY[kind]}</FieldHelp>
            </div>

            {/* The backing, where there is a choice. One offering is not a
                question, so it is stated rather than asked — an install with a
                single filesystem should not have to pick it every time. */}
            {chosen && offered.length > 1 && (
              <div className="space-y-1.5">
                <Label htmlFor="resource-backing">Backing</Label>
                <Select value={chosen} onValueChange={(v) => setBacking(v as StorageBacking)}>
                  <SelectTrigger id="resource-backing" className="w-full">
                    <SelectValue>{(v) => backingLabel(v as StorageBacking, storesMounted)}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {offered.map((b) => (
                      <SelectItem key={b} value={b}>
                        {backingLabel(b, storesMounted)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <FieldHelp>{backingSummary(chosen, storesMounted)}</FieldHelp>
              </div>
            )}
            {chosen && offered.length === 1 && (
              <FieldHelp>
                {backingLabel(chosen, storesMounted)}. {backingSummary(chosen, storesMounted)}
              </FieldHelp>
            )}

            {/* The name is what apps and the CLI type, so it is shown being
                used while it is typed — the one way to make "this is an
                identifier" land before somebody writes "Sales datasets". */}
            <div className="space-y-1.5">
              <Label htmlFor="storage-name">Name</Label>
              <Input
                id="storage-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={kind === "volume" ? "shared-cache" : "sales-data"}
                className="font-mono"
                autoComplete="off"
                spellCheck={false}
                aria-invalid={!!nameProblem}
                aria-describedby={nameProblem ? fieldErrorId("storage-name") : "storage-name-help"}
              />
              {nameProblem ? (
                <FieldError field="storage-name">{nameProblem}</FieldError>
              ) : (
                <FieldHelp id="storage-name-help">
                  Apps use it as{" "}
                  <code className="font-mono">
                    toft.storage("{name.trim() || (kind === "volume" ? "shared-cache" : "sales-data")}")
                  </code>
                  , people as <code className="font-mono">toft store ls {name.trim() || "…"}</code>.
                  Lowercase letters, digits and hyphens. Can’t be changed later.
                </FieldHelp>
              )}
            </div>

            {classesHere.length > 1 && (
              <div className="space-y-1.5">
                <Label htmlFor="storage-class">Drive</Label>
                <Select value={chosenClass} onValueChange={(v) => setClassChoice(v ?? "")}>
                  <SelectTrigger id="storage-class" className="w-full">
                    <SelectValue>
                      {(v) => {
                        const c = classesHere.find((x) => x.name === v);
                        return c ? classTitle(c) : "Choose a drive…";
                      }}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {classesHere.map((c) => (
                      <SelectItem key={c.name} value={c.name}>
                        {classTitle(c)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <FieldHelp>
                  Created from the <code className="font-mono">{chosenClass || "…"}</code> storage
                  class. Can’t be changed later.
                </FieldHelp>
              </div>
            )}

            {needsRoot && roots.length === 1 && chosenRoot && (
              <FieldHelp>
                Creates a directory under <code className="font-mono">{chosenRoot.path}</code>, on{" "}
                {rootTitle(chosenRoot)}, the one drive on this install
                {chosenRoot.readOnly ? ". It is read-only, so apps can only read" : ""}.
              </FieldHelp>
            )}

            {needsRoot && roots.length > 1 && (
              <div className="space-y-1.5">
                <Label htmlFor="storage-root">Drive</Label>
                <Select value={root} onValueChange={(v) => setRoot(v ?? "")}>
                  <SelectTrigger id="storage-root" className="w-full">
                    <SelectValue>
                      {(v) => (v ? rootTitle(roots.find((r) => r.name === v)) : "Choose a drive…")}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {roots.map((r) => (
                      <SelectItem key={r.name} value={r.name}>
                        {rootTitle(r)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <FieldHelp>
                  {chosenRoot ? (
                    <>
                      Creates a directory under{" "}
                      <code className="font-mono">{chosenRoot.path}</code>.
                      {chosenRoot.readOnly
                        ? " This mount is read-only, so apps can only read."
                        : ""}
                    </>
                  ) : (
                    "Toft creates a directory under the mount you choose."
                  )}
                </FieldHelp>
              </div>
            )}

            {needsBucket && (
              <>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    {/* Azure's word for a bucket. The column, the API field and
                        the audit log all keep saying `bucket` — renaming what
                        crosses four surfaces to gain a synonym is churn with a
                        migration attached — so the accommodation is exactly this
                        label, where an administrator is typing the thing. */}
                    <Label htmlFor="storage-bucket">
                      {chosen === "blob" ? "Container" : "Bucket"}
                    </Label>
                    <Input
                      id="storage-bucket"
                      value={bucket}
                      onChange={(e) => setBucket(e.target.value)}
                      placeholder="acme-data"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="storage-prefix">Prefix</Label>
                    <Input
                      id="storage-prefix"
                      value={prefix}
                      onChange={(e) => setPrefix(e.target.value)}
                      placeholder="sales/reports"
                    />
                  </div>
                </div>
                <FieldHelp>
                  Apps see only what’s under the prefix. A prefix is required — a whole bucket
                  can’t be used.
                </FieldHelp>
                {/* The one thing on this form Toft cannot check for you, said
                    where the mistake would be made rather than discovered. */}
                {chosen === "s3files" && (
                  <Notice tone="attention">
                    Name the bucket this file system is built on. Toft can’t check it, and a
                    wrong bucket lists the wrong files here.
                  </Notice>
                )}
                {/* Said where the choice is made, because it is a property of
                    the driver and not a switch anybody can throw. The two levels
                    it *can* be mounted at are named, so this reads as a shape
                    rather than as a missing feature. */}
                {chosen === "blob" && (
                  <Notice tone="attention">
                    Apps can get Read only or Read & write here, not Can add.
                  </Notice>
                )}
                {kind === "store" && (
                  <FieldHelp>{STORE_CAVEAT}</FieldHelp>
                )}
              </>
            )}

            {offersPartitioned && (
              <div className="space-y-1.5">
                <div className="flex items-center gap-2">
                  <Checkbox
                    id="storage-partitioned"
                    checked={partitioned}
                    onCheckedChange={(on) => setPartitioned(on === true)}
                  />
                  <Label htmlFor="storage-partitioned">Partitioned</Label>
                </div>
                <FieldHelp>
                  Apps get folders, never the whole resource, and only apps on a folder can open it.
                  Can’t be changed later.
                </FieldHelp>
              </div>
            )}

            {/* A textarea rather than a one-line input, and the tallest field on
                the form. This is where an administrator writes what a volume is
                *for* — which app family, what is safe to delete, who to ask —
                and a single line taught people to write four words and stop. It
                is the only note the next person on the rota gets. */}
            <div className="space-y-1.5">
              <Label htmlFor="storage-description">Description (optional)</Label>
              <Textarea
                id="storage-description"
                rows={4}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="What it’s for, and what to know before deleting it"
              />
            </div>
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || !ready}>
            {saving ? "Creating…" : "Create"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

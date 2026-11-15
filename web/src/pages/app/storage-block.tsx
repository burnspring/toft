import { useState } from "react";
import { Code } from "@/components/code";

import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

import { Badge } from "@/components/ui/badge";

import { Notice } from "@/components/notice";

import { Section } from "@/components/section";
import { FieldHelp } from "@/components/field-help";
import { Disclosure } from "@/components/disclosure";
import { StorageUsage } from "@/components/storage-usage";

import { Loader2, Plus, Unlink } from "lucide-react";

import {
  KIND_LABEL,
  accessBadgeClass,
  accessLabel,
  accessSummary,
  appStorageControls,
  levelsUpTo,
  mappingKind,
  mappingRefusal,
} from "@/lib/storage";
import { StorageFolderField } from "@/components/storage-folder-field";
import { cn } from "@/lib/utils";

import { api } from "@/lib/api";

import type { AttachableResource, StorageAccess, StorageMapping, App } from "@/lib/api";

/**
 * An app's durable storage, on its Overview.
 *
 * Its own module because it belongs to neither half of the split: the block is
 * rendered by Overview, and the dialog behind it is the one place in the app
 * pages that writes a mapping. Leaving it in the settings module would have made
 * Overview import Settings — the largest tab, and one nobody opening an app to
 * read it should be made to load.
 */

/**
 * What durable storage this app has, and — for a delegate — the way to add one.
 *
 * Read-only for most readers, including most owners: an administrator
 * provisions storage, because a volume is a real cost and a durable grant that
 * outlives every release. What changed is that an administrator can now
 * *delegate* attaching a named resource, so somebody who manages one and can
 * edit this app gets Attach and Detach here, bounded by the ceiling that
 * administrator set. Everyone else sees exactly what they saw before.
 *
 * The section disappears entirely when the install offers no storage (the
 * endpoint 404s) and when there is neither a mount to show nor anything this
 * reader could attach. An empty "Storage" heading on every app would be a
 * permanent advertisement for a feature most apps never need.
 *
 * **On Overview, not in Settings.** These are facts first — nobody without a
 * delegation can change one — and the fact somebody wants ("where is my app's
 * data, and may it write there?") was unreachable behind a settings filter.
 */
export function StorageBlock({ appId, app, canEdit }: { appId: string; app: App; canEdit: boolean }) {
  const queryClient = useQueryClient();
  const [attaching, setAttaching] = useState(false);
  const [detaching, setDetaching] = useState<string | null>(null);

  // Asked only where the install offers storage: on one that does not, both
  // reads are a 404 on every app page -- the probe the admin nav stopped
  // making for the same reason. Not-answered-yet renders nothing, as before.
  const { data: caps } = useQuery({ queryKey: ["capabilities"], queryFn: api.getCapabilities });
  const storageOffered = caps?.install?.storage_available === true;
  const { data, isLoading } = useQuery({
    queryKey: ["app-storage", appId],
    queryFn: () => api.getAppStorage(appId),
    enabled: storageOffered,
    retry: false,
  });

  // **A mapping still grants nobody.** An app owner does not get a browser over
  // their app's store because the app mounts it — a mapping is the app's and a
  // grant is a person's. So the name becomes a link only where this reader
  // already holds a grant, and stays plain text otherwise. No "request access"
  // affordance and no explanation of who could grant it: this page has no way
  // to know the answer, and guessing at one would send somebody to the wrong
  // person.
  const { data: reach } = useQuery({
    queryKey: ["stores"],
    queryFn: api.listStores,
    retry: false,
  });
  // Keyed on the slug, which is the identity on both sides. A name is a label
  // an administrator can change and two resources can share.
  const reachable = new Set((reach?.stores ?? []).map((s) => s.slug));

  // What this reader may attach anywhere. Asked only of somebody who could act
  // on the answer: a viewer has no Attach control, so there is nothing for the
  // list to enable. An administrator with no delegation of their own sees
  // nothing here either — their door is Admin → Storage, which reaches every
  // resource rather than the ones somebody delegated to them.
  const { data: mine } = useQuery({
    queryKey: ["my-attachable-storage"],
    queryFn: api.listMyAttachableStorage,
    enabled: canEdit && storageOffered,
    retry: false,
  });

  function refresh() {
    queryClient.invalidateQueries({ queryKey: ["app-storage", appId] });
  }

  async function detach(m: StorageMapping) {
    setDetaching(m.id);
    try {
      await api.detachAppStorage(appId, m.resource_id, m.folder ?? "");
      toast.success(`Detached “${m.resource_name}”`, {
        description: "The app stops seeing it at its next restart or deploy.",
      });
      refresh();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t detach “${m.resource_name}”`);
    } finally {
      setDetaching(null);
    }
  }

  if (isLoading || !data) return null;

  // One object the section renders from, rather than four comparisons spread
  // through the JSX. `appStorageControls` is where the rules live and where
  // they are tested.
  const controls = appStorageControls({
    canEdit,
    managed: mine?.resources ?? [],
    mountedResourceIds: data.mappings.map((m) => m.resource_id),
    executionMode: app.execution_mode,
    sharedModeStorage: mine?.shared_mode,
  });
  if (data.mappings.length === 0 && !controls.canAttach) return null;

  return (
    <Section
      id="storage"
      className="divide-y divide-border/60"
      title="Storage"
      description="Storage attached to this app. It outlives every deployment."
      actions={
        /* Absent, not disabled: a greyed-out Attach is a promise that this
           reader could have it, and the way to have it is a delegation only an
           administrator can make. */
        controls.canAttach ? (
          <Button size="sm" variant="outline" onClick={() => setAttaching(true)}>
            <Plus className="mr-1.5 h-4 w-4" />
            Attach storage
          </Button>
        ) : undefined
      }
    >
      <div className="space-y-3 px-4 py-4">
        {data.mappings.length === 0 && (
          <p className="text-sm text-muted-foreground">
            Nothing is mounted yet. You can attach{" "}
            {controls.attachable.length === 1
              ? "one resource"
              : `${controls.attachable.length} resources`}.
          </p>
        )}
        {data.mappings.map((m) => {
          // One reading of the kind for all three renderings below. They used to
          // default in opposite directions — the badge to a store, the summary to
          // a volume — so a mapping that arrived without one described an object
          // store in a filesystem's words.
          const kind = mappingKind(m.resource_kind);
          return (
          <div key={m.id} className="flex flex-wrap items-center gap-2 text-sm">
            <Code className="px-1.5 text-xs">{m.path}</Code>
            {m.resource_slug && reachable.has(m.resource_slug) ? (
              <Link to={`/storage/${m.resource_slug}`} className="text-primary hover:underline">
                {m.resource_name}
              </Link>
            ) : (
              <span className="text-muted-foreground">{m.resource_name}</span>
            )}
            <Badge variant="outline" className="text-2xs">
              {KIND_LABEL[kind]}
            </Badge>
            {m.folder && (
              // Which directory of the resource is behind this path. Two rows
              // naming one resource are two folders of it, and without this
              // they read as the same mount listed twice.
              <Code className="px-1.5 text-2xs text-muted-foreground">{m.folder}/</Code>
            )}
            <Badge variant="outline" className={cn("text-2xs", accessBadgeClass(m.access))}>
              {accessLabel(m.access)}
            </Badge>
            {m.source === "manifest" && (
              // Declared in the app's own toft.toml, so it changes there and
              // the next deploy writes it — no Detach beside it.
              <Badge variant="outline" className="text-2xs">
                From toft.toml
              </Badge>
            )}
            {m.source !== "manifest" && controls.detachable.has(m.resource_id) && (
              <Button
                variant="ghost"
                size="icon"
                className="h-6 w-6"
                disabled={detaching === m.id}
                aria-label={`Detach ${m.resource_name}`}
                onClick={() => detach(m)}
              >
                {detaching === m.id ? (
                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                ) : (
                  <Unlink className="h-3.5 w-3.5" />
                )}
              </Button>
            )}
            <span className="w-full text-xs text-muted-foreground">
              {accessSummary(kind, m.access)}
              {m.source === "manifest" &&
                " Declared in toft.toml: edit the [[storage]] entry and deploy to change or remove it."}
            </span>
            {/* This app's own code, for this attachment: its name and, where
                the app holds two folders of one resource, the folder that
                tells them apart. Only the SDK — the terminal is a person's
                grant, not something attaching gives. */}
            {m.resource_slug && (
              <Disclosure label="Code" size="xs" className="w-full">
                <StorageUsage
                  name={m.resource_slug}
                  kind={kind}
                  folder={m.folder}
                  cli={false}
                  className="mt-2"
                />
              </Disclosure>
            )}
          </div>
          );
        })}

        <p className="text-xs text-muted-foreground">
          Your code finds each resource by its name, so a path can change
          without breaking the app.
        </p>
        {data.pending && data.note && (
          <FieldHelp tone="attention">{data.note}</FieldHelp>
        )}
      </div>

      <AttachStorageDialog
        open={attaching}
        appId={appId}
        app={app}
        resources={controls.attachable}
        warnsSharedMode={controls.warnsSharedMode}
        onOpenChange={setAttaching}
        onAttached={refresh}
      />
    </Section>
  );
}

/**
 * Attaching a resource somebody delegated to you.
 *
 * The three things it has to say, and each of them is otherwise learned the
 * hard way: which resources you may attach at all, what the ceiling on this one
 * is, and — if this app runs shared — that a deploy will be refused until it is
 * promoted.
 *
 * That last one is a warning and **not a refusal**. The deploy-time refusal
 * stays the enforcement point, because an administrator legitimately attaches
 * storage and *then* promotes the app, and refusing here would break that
 * order. Read from the app's own mode rather than from a flag on the response,
 * so it cannot say one thing while the app page says another.
 */
function AttachStorageDialog({
  open, appId, app, resources, warnsSharedMode, onOpenChange, onAttached,
}: {
  open: boolean;
  appId: string;
  app: App;
  resources: AttachableResource[];
  warnsSharedMode: boolean;
  onOpenChange: (v: boolean) => void;
  onAttached: () => void;
}) {
  const [resourceId, setResourceId] = useState("");
  const [path, setPath] = useState("");
  const [access, setAccess] = useState<StorageAccess>("read");
  const [folder, setFolder] = useState("");
  const [saving, setSaving] = useState(false);
  const [refusal, setRefusal] = useState("");

  const selected = resources.find((r) => r.id === resourceId);
  const levels = selected ? levelsUpTo(selected.kind, selected.max_access) : [];

  function choose(id: string) {
    const res = resources.find((r) => r.id === id);
    setResourceId(id);
    // A path the reader can accept as-is, and a level at the bottom of the
    // ladder rather than at their ceiling — the ceiling is what they *may*
    // have, not what this app needs.
    setPath(res ? `/data/${res.slug}` : "");
    setAccess("read");
    setFolder("");
    setRefusal("");
  }

  async function submit() {
    if (!selected || !path.trim()) return;
    setSaving(true);
    try {
      const result = await api.attachAppStorage(appId, selected.id, {
        path: path.trim(),
        access,
        folder: folder.trim(),
      });
      toast.success(`Attached “${selected.name}”`, { description: result.note });
      setRefusal("");
      onOpenChange(false);
      onAttached();
    } catch (err) {
      if (mappingRefusal(err)) setRefusal((err as Error).message);
      else toast.error(err instanceof Error ? err.message : `Couldn’t attach “${selected.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Attach storage to {app.name}</DialogTitle>
          <DialogDescription>
            The app sees it as an ordinary directory.
          </DialogDescription>
        </DialogHeader>

        {/* `contents`, so the form is no box of its own and the body and footer
            stay the scrollable column's children — Enter submits. */}
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
        {/* Bounded, because picking a resource grows this from one field to
            four with a paragraph under each, plus a notice on a shared-mode
            install. Unbounded it overflowed the viewport, and `DialogContent`
            is `position: fixed` — so neither it nor the page scrolls and
            Attach is unreachable by mouse, keyboard and touch alike. */}
        <DialogBody className="space-y-4">
          {warnsSharedMode && (
            <Notice tone="attention" size="sm" title="This app runs in shared mode">
              This install has no shared-mode storage mount. You can attach now, but the next deploy
              will be refused until an administrator adds a mount or the app moves to isolated mode.
            </Notice>
          )}

          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="attach-resource">Resource</Label>
              <Select value={resourceId} onValueChange={(v) => choose(v ?? "")}>
                <SelectTrigger id="attach-resource" className="w-full">
                  {/* base-ui's Value renders the raw value — here, an id. */}
                  <SelectValue>
                    {(v) => resources.find((r) => r.id === v)?.name ?? "Choose a resource…"}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {resources.map((r) => (
                    <SelectItem key={r.id} value={r.id}>
                      {r.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {selected && (
                <FieldHelp>
                  {selected.kind === "volume" ? "Volume" : "Object store"} · you may attach it at{" "}
                  {accessLabel(selected.max_access)} or below.
                </FieldHelp>
              )}
            </div>

            {selected && (
              <>
                <div className="space-y-1.5">
                  <Label htmlFor="attach-path">Path in the app</Label>
                  <Input
                    id="attach-path"
                    value={path}
                    onChange={(e) => setPath(e.target.value)}
                    className="font-mono text-sm"
                  />
                  <FieldHelp>
                    An absolute path, outside the app’s own code and build directories.
                  </FieldHelp>
                </div>

                <StorageFolderField
                  id="attach-folder"
                  partitioned={selected.partitioned}
                  value={folder}
                  onChange={(v) => {
                    setFolder(v);
                    setRefusal("");
                  }}
                  refusal={refusal}
                />

                <div className="space-y-1.5">
                  <Label htmlFor="attach-access">Access</Label>
                  <Select value={access} onValueChange={(v) => setAccess(v as StorageAccess)}>
                    <SelectTrigger id="attach-access" className="w-full">
                      <SelectValue>{(v) => accessLabel(v as StorageAccess)}</SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      {levels.map((level) => (
                        <SelectItem key={level} value={level}>
                          {accessLabel(level)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FieldHelp>
                    {accessSummary(selected.kind, access)}
                  </FieldHelp>
                </div>
              </>
            )}
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={saving || !selected || !path.trim() || (selected.partitioned && !folder.trim())}
          >
            {saving ? "Attaching…" : "Attach"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

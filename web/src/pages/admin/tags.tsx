import { useState, useMemo } from "react";
import { FieldLabel, Label } from "@/components/ui/label";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, MoreHorizontal, Plus, Tag as TagIcon } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { FilterBar } from "@/components/filter-bar";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { tagColorClass, tagDotClass, TAG_COLORS } from "@/lib/tags";
import { api } from "@/lib/api";
import type { Tag, TagColor } from "@/lib/api";
import { cn } from "@/lib/utils";
import { TableCard } from "@/components/table-card";
import { useUrlParam } from "@/hooks/use-list-state";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

const kindFilters = [
  { label: "All", value: "all" },
  { label: "Custom", value: "custom" },
  { label: "System", value: "system" },
];

/** One sentence about this page, said the same way on every branch.
 *  Each of these files hand-wrote a second, shorter one for its error and
 *  loading paths, so the page described itself one way to whoever loaded it
 *  and another to whoever was refused. */
const PAGE_DESCRIPTION =
  "App owners set a value for each on their apps.";

export function AdminTagsPage() {
  const queryClient = useQueryClient();
  const [search, setSearch] = useUrlParam("q");
  const [kindFilter, setKindFilter] = useUrlParam("kind", "all");
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Tag | null>(null);
  const [toDelete, setToDelete] = useState<Tag | null>(null);

  const { data: tags = [], isLoading, error, refetch } = useQuery<Tag[]>({
    queryKey: ["tags"],
    queryFn: api.listTags,
    retry: false,
  });

  const refresh = () => queryClient.invalidateQueries({ queryKey: ["tags"] });

  // Split so the tab strip's counts come from the rows the search has already
  // narrowed to — the same shape the apps list uses, and the reason "Custom (0)"
  // above three rows would be a number arguing with the table under it.
  const searched = useMemo(
    () => tags.filter((t) => t.name.toLowerCase().includes(search.toLowerCase())),
    [tags, search],
  );
  const filtered = useMemo(
    () => (kindFilter === "all" ? searched : searched.filter((t) => t.kind === kindFilter)),
    [searched, kindFilter],
  );

  async function recolor(tag: Tag, color: TagColor) {
    try {
      await api.updateTag(tag.id, { color });
      refresh();
      toast.success(`Recolored “${tag.name}”`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t recolor “${tag.name}”`);
    }
  }

  if (error) {
    return (
      <div>
        <PageHeader title="Tags" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The tag vocabulary could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Tags"
        description={PAGE_DESCRIPTION}
        actions={
          <Button size="sm" onClick={() => setCreating(true)}>
            <Plus className="mr-1.5 h-4 w-4" />
            New tag
          </Button>
        }
      />

      <div className="space-y-4">
        <div className="flex flex-wrap items-center gap-4">
          <SearchInput
            placeholder="Search tags…"
            value={search}
            onChange={setSearch}
            className="w-full sm:max-w-sm"
          />
          <FilterBar
            label="Filter tags by kind"
            options={kindFilters.map((f) => ({
              ...f,
              label: `${f.label} (${f.value === "all" ? searched.length : searched.filter((t) => t.kind === f.value).length})`,
            }))}
            value={kindFilter}
            onChange={setKindFilter}
          />
        </div>

        {isLoading ? (
          <TableCard className="space-y-3 p-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <div key={i} className="flex items-center gap-3">
                <Skeleton className="h-4 w-32" />
                <Skeleton className="h-4 w-16" />
                <Skeleton className="ml-auto h-4 w-20" />
              </div>
            ))}
          </TableCard>
        ) : tags.length === 0 ? (
          <EmptyState
            icon={<TagIcon className="h-6 w-6" />}
            title="No tags yet"
            description="Create a tag such as Department, then app owners can set its value."
          />
        ) : filtered.length === 0 ? (
          <EmptyState
            title="No tags match"
            action={
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  setSearch("");
                  setKindFilter("all");
                }}
              >
                Clear
              </Button>
            }
          />
        ) : (
          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {/* 640px of fixed columns before the tag itself had been
                      measured. What a tag *is* — its name, whether it is the
                      platform's, and what it is on — stays; the rest yield. */}
                  <TableHead className="w-full">Tag</TableHead>
                  <TableHead className="w-[100px]">Kind</TableHead>
                  <TableHead className="hidden w-[170px] @xl:table-cell">Color</TableHead>
                  <TableHead className="hidden w-[100px] @md:table-cell">Values</TableHead>
                  <TableHead className="w-[90px]">Apps</TableHead>
                  <TableHead className="hidden w-[130px] @3xl:table-cell">Created</TableHead>
                  <TableHead className="w-[50px]"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((tag) => {
                  const isSystem = tag.kind === "system";
                  return (
                    <TableRow key={tag.id}>
                      <TableCell className="max-w-0">
                        <div className="truncate font-medium">{tag.name}</div>
                        {tag.description && (
                          <div className="truncate text-xs text-muted-foreground">{tag.description}</div>
                        )}
                      </TableCell>
                      <TableCell>
                        {isSystem ? (
                          <Badge variant="secondary" className="text-2xs">System</Badge>
                        ) : (
                          <span className="text-xs text-muted-foreground">Custom</span>
                        )}
                      </TableCell>
                      <TableCell className="hidden @xl:table-cell">
                        {isSystem ? (
                          <span className={cn("inline-block h-3 w-3 rounded-full border", tagColorClass(tag.color))} />
                        ) : (
                          <ColorPicker value={tag.color} onChange={(c) => recolor(tag, c)} />
                        )}
                      </TableCell>
                      <TableCell className="hidden @md:table-cell">
                        {tag.value_count > 0 ? (
                          <Link to={`/tags/${tag.slug}`} className="text-sm text-primary hover:underline">
                            {tag.value_count}
                          </Link>
                        ) : (
                          <span className="text-sm text-muted-foreground">0</span>
                        )}
                      </TableCell>
                      {/* Openable, like Values: a count a reader cannot open is a
                          fact stated to somebody who cannot act on it. The tag's
                          own page lists the apps that carry it. */}
                      <TableCell>
                        {tag.app_count > 0 ? (
                          <Link to={`/tags/${tag.slug}`} className="text-sm text-primary hover:underline">
                            {tag.app_count}
                          </Link>
                        ) : (
                          <span className="text-sm text-muted-foreground">0</span>
                        )}
                      </TableCell>
                      <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
                        {isSystem ? "—" : <RelativeTime iso={tag.created_at} />}
                      </TableCell>
                      <TableCell>
                        {/* System tags have no editable identity: their name and
                            values come from the platform, not the database. */}
                        {!isSystem && (
                          <DropdownMenu>
                            <DropdownMenuTrigger
                              render={
                                <Button
                                  variant="ghost"
                                  size="icon"
                                  className="h-7 w-7"
                                  aria-label={`Actions for ${tag.name}`}
                                >
                                  <MoreHorizontal className="h-4 w-4" />
                                </Button>
                              }
                            />
                            <DropdownMenuContent align="end">
                              <DropdownMenuItem onClick={() => setEditing(tag)}>Edit…</DropdownMenuItem>
                              <DropdownMenuItem variant="destructive" onClick={() => setToDelete(tag)}>
                                Delete…
                              </DropdownMenuItem>
                            </DropdownMenuContent>
                          </DropdownMenu>
                        )}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </TableCard>
        )}
      </div>

      <CreateTagDialog open={creating} onOpenChange={setCreating} onCreated={refresh} />
      {/* Mounted only while a tag is being edited, and keyed on it. Seeding
          the field from a prop instead — which is what this did — meant
          reopening the *same* tag after abandoning an edit showed the
          abandoned draft rather than the name the tag actually has. */}
      {editing && (
        <EditTagDialog
          key={editing.id}
          tag={editing}
          onOpenChange={() => setEditing(null)}
          onSaved={refresh}
        />
      )}
      <DeleteTagDialog tag={toDelete} onOpenChange={() => setToDelete(null)} onDeleted={refresh} />
    </div>
  );
}

/** Recoloring is instant and reversible, so it needs no dialog — just a swatch row. */
function ColorPicker({ value, onChange }: { value: TagColor; onChange: (c: TagColor) => void }) {
  return (
    <div role="group" aria-label="Tag color" className="flex flex-wrap gap-0.5">
      {/* A 24px target holding the swatch (WCAG 2.5.8): the swatch alone was
          14px in the chip's pale tint; the solid dot tells them apart. Chosen is a ring and a check, so colour is not the
          only carrier; no hover scale — motion is spent on the deploy. */}
      {TAG_COLORS.map((c) => (
        <button
          key={c}
          type="button"
          onClick={() => onChange(c)}
          aria-label={c}
          aria-pressed={c === value}
          className="flex size-6 items-center justify-center rounded-full outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring"
        >
          <span
            className={cn(
              "flex size-4 items-center justify-center rounded-full text-card",
              tagDotClass(c),
              c === value && "ring-2 ring-ring ring-offset-1",
            )}
          >
            {c === value && <Check aria-hidden className="size-3" strokeWidth={3} />}
          </span>
        </button>
      ))}
    </div>
  );
}

function CreateTagDialog({
  open, onOpenChange, onCreated,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [color, setColor] = useState<TagColor>("indigo");
  const [saving, setSaving] = useState(false);

  function reset() {
    setName("");
    setDescription("");
    setColor("indigo");
  }

  async function submit() {
    // `saving` too: a second Enter while the first request is in flight
    // created the tag twice.
    if (!name.trim() || saving) return;
    setSaving(true);
    try {
      await api.createTag({ name: name.trim(), color, description: description.trim() });
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
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <DialogHeader>
            <DialogTitle>New tag</DialogTitle>
            <DialogDescription>
              Name the label, not the value: “Department”, not “Engineering”.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="new-tag-name">Name</Label>
              <Input
                id="new-tag-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Department"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="new-tag-description">Description (optional)</Label>
              <Input
                id="new-tag-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="Shown to app owners"
              />
            </div>
            <div className="space-y-1.5">
              <FieldLabel className="text-sm font-medium">Color</FieldLabel>
              <ColorPicker value={color} onChange={setColor} />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || !name.trim()}>
              {saving ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * A tag's name and description — the two things about it that are words.
 *
 * It was *Rename…*, with the name alone, so a description set on create
 * (shown to app owners) could never be changed afterwards. It sends only what
 * changed, because `updateTag` takes a partial.
 */
function EditTagDialog({
  tag, onOpenChange, onSaved,
}: {
  tag: Tag;
  onOpenChange: (v: boolean) => void;
  onSaved: () => void;
}) {
  // Seeded once, at mount, because the caller keys this on the tag and mounts
  // it only while one is open — which is the convention here, and the reason
  // this needs neither an effect nor a sync-from-props.
  const [name, setName] = useState(tag.name);
  const [description, setDescription] = useState(tag.description ?? "");
  const [saving, setSaving] = useState(false);

  const nameChanged = name.trim() !== tag.name;
  const descriptionChanged = description.trim() !== (tag.description ?? "");

  async function submit() {
    if (!name.trim() || saving || (!nameChanged && !descriptionChanged)) return;
    setSaving(true);
    try {
      await api.updateTag(tag.id, {
        ...(nameChanged ? { name: name.trim() } : {}),
        ...(descriptionChanged ? { description: description.trim() } : {}),
      });
      toast.success("Saved");
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t save “${tag.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <DialogHeader>
            <DialogTitle>Edit {tag.name}</DialogTitle>
            <DialogDescription>
              The link stays at <code className="text-xs">/tags/{tag.slug}</code>.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="edit-tag-name">Name</Label>
              <Input
                id="edit-tag-name"
                value={name}
                autoFocus
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="edit-tag-description">Description (optional)</Label>
              <Input
                id="edit-tag-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="Shown to app owners"
              />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={saving || !name.trim() || (!nameChanged && !descriptionChanged)}
            >
              {saving ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Deleting an unused tag is uneventful; deleting one in use strips it from every
 * app, so that case is confirmed. Not type-the-name — that friction is reserved
 * for app deletion and ownership transfer.
 */
function DeleteTagDialog({
  tag, onOpenChange, onDeleted,
}: {
  tag: Tag | null;
  onOpenChange: (v: boolean) => void;
  onDeleted: () => void;
}) {
  const [saving, setSaving] = useState(false);

  async function submit() {
    if (!tag) return;
    setSaving(true);
    try {
      // The server 409s a tag in use unless forced; the dialog is that consent.
      await api.deleteTag(tag.id, tag.app_count > 0);
      toast.success(`Deleted “${tag.name}”`);
      onOpenChange(false);
      onDeleted();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t delete “${tag.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={!!tag} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete {tag?.name}?</DialogTitle>
          <DialogDescription>
            {tag && tag.app_count > 0
              ? `It is set on ${tag.app_count} app${tag.app_count !== 1 ? "s" : ""}. Deleting removes it from all of them.`
              : "This tag isn’t set on any app."}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={submit} disabled={saving}>
            {saving ? "Deleting…" : "Delete"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

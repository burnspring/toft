import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, Plus, X } from "lucide-react";
import { toast } from "sonner";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList, CommandSeparator,
} from "@/components/ui/command";
import { Badge } from "@/components/ui/badge";
import { tagColorClass } from "@/lib/tags";
import { api } from "@/lib/api";
import { QueryError } from "@/components/query-error";
import type { AppTag, Tag, TagValue } from "@/lib/api";
import { cn } from "@/lib/utils";
import { FIELD_CONTROL, FIELD_ROW } from "@/components/field-row";
import { FieldHelp } from "@/components/field-help";

/**
 * Per-app tag editor: one row per tag in the vocabulary, with the value this
 * app carries for it.
 *
 * Values are free text, so the combobox is *creatable* — it suggests values
 * already in use and lets you type a new one. The suggestions are what keep the
 * vocabulary from drifting into Engineering / engineering / Eng; the server
 * additionally snaps a new spelling to any existing one with the same slug.
 */
export function TagEditor({
  appId,
  appTags,
  canEdit,
}: {
  appId: string;
  appTags: AppTag[];
  canEdit: boolean;
}) {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<string | null>(null);

  const { data: vocabulary = [], isLoading, error, refetch } = useQuery<Tag[]>({
    queryKey: ["tags"],
    queryFn: api.listTags,
    retry: false,
  });

  const valueFor = (slug: string) => appTags.find((t) => t.tag_slug === slug)?.value ?? "";

  async function setValue(tag: Tag, value: string) {
    if (value === valueFor(tag.slug)) return;
    setPending(tag.slug);
    try {
      await api.setAppTags(appId, { [tag.slug]: value });
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["tags"] });
      queryClient.invalidateQueries({ queryKey: ["tag-values", tag.slug] });
      toast.success(value ? `${tag.name}: ${value}` : `Cleared ${tag.name}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t update ${tag.name}`);
    } finally {
      setPending(null);
    }
  }

  if (isLoading) {
    return (
      <div className="space-y-2">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="h-7 w-full" />
        ))}
      </div>
    );
  }
  // Before the empty check: "No tags are defined yet" is a statement about the
  // install, and a failed read is not entitled to make it.
  if (error) {
    return (
      <QueryError
        title="The tag vocabulary could not be read"
        error={error}
        fallback="Tags are defined by an administrator under Admin → Tags."
        onRetry={() => void refetch()}
      />
    );
  }
  // A system tag's value is the platform's, and the one there is — Framework —
  // is already the chip in the page header. Listing it here put a read-only
  // row under "Changes save immediately" on every install with no tags of its
  // own, a promise about a control that was not there.
  const editable = vocabulary.filter((t) => t.kind !== "system");
  if (editable.length === 0) {
    return (
      <div className="space-y-1">
        <p className="text-sm font-medium text-foreground">Tags</p>
        <FieldHelp>No tags are defined yet. An administrator can create them under Admin → Tags.</FieldHelp>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      <div className="space-y-1">
        <p className="text-sm font-medium text-foreground">Tags</p>
        <FieldHelp>{canEdit ? "Changes save immediately." : "Set by the app’s editors."}</FieldHelp>
      </div>
      {editable.map((tag) => {
        const value = valueFor(tag.slug);
        // The settings row shape, so a tag's value starts at the same x as
        // the app name above it rather than at the end of a 10rem label.
        return (
          <div key={tag.id} className={FIELD_ROW}>
            <div className="min-w-0 space-y-1">
              <div className="text-sm font-medium">{tag.name}</div>
              {tag.description && (
                <div className="text-xs text-muted-foreground">{tag.description}</div>
              )}
            </div>

            <div className={FIELD_CONTROL}>
            {canEdit ? (
              <ValueCombobox
                tag={tag}
                value={value}
                busy={pending === tag.slug}
                onPick={(v) => setValue(tag, v)}
              />
            ) : value ? (
              <Badge variant="outline" className={cn("border text-2xs font-normal", tagColorClass(tag.color))}>
                {value}
              </Badge>
            ) : (
              <span className="text-sm text-muted-foreground">—</span>
            )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function ValueCombobox({
  tag,
  value,
  busy,
  onPick,
}: {
  tag: Tag;
  value: string;
  busy: boolean;
  onPick: (value: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");

  const { data: suggestions = [], isFetching } = useQuery<TagValue[]>({
    queryKey: ["tag-values", tag.slug, query],
    queryFn: () => api.listTagValues(tag.slug, query),
    enabled: open,
  });

  const typed = query.trim();
  const exists = suggestions.some((s) => s.value.toLowerCase() === typed.toLowerCase());

  function pick(v: string) {
    setOpen(false);
    setQuery("");
    onPick(v);
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button variant="outline" size="sm" className="h-7 gap-1.5 text-xs font-normal" disabled={busy}>
            {value ? (
              <>
                <span className={cn("h-2 w-2 rounded-full border", tagColorClass(tag.color))} />
                {value}
              </>
            ) : (
              <span className="text-muted-foreground">Set value…</span>
            )}
            <ChevronDown className="h-3 w-3 opacity-50" />
          </Button>
        }
      />
      {/* PopoverContent ships w-72 p-2.5, which fights cmdk's own layout. */}
      <PopoverContent align="start" className="w-72 p-0">
        <Command shouldFilter={false}>
          <CommandInput
            placeholder={`Search or type a ${tag.name.toLowerCase()}…`}
            value={query}
            onValueChange={setQuery}
          />
          <CommandList>
            {suggestions.length === 0 && !typed && !isFetching && (
              <CommandEmpty>No values in use yet — type one.</CommandEmpty>
            )}
            {suggestions.length > 0 && (
              <CommandGroup heading="In use">
                {suggestions.map((s) => (
                  <CommandItem key={s.slug} value={s.slug} onSelect={() => pick(s.value)}>
                    <span className="flex-1">{s.value}</span>
                    <span className="text-xs text-muted-foreground">{s.app_count}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {typed && !exists && (
              <CommandGroup>
                <CommandItem value={`__new__${typed}`} onSelect={() => pick(typed)}>
                  <Plus className="mr-1.5 h-3.5 w-3.5" />
                  Use “{typed}”
                </CommandItem>
              </CommandGroup>
            )}
            {value && (
              <>
                <CommandSeparator />
                <CommandGroup>
                  <CommandItem value="__clear__" onSelect={() => pick("")}>
                    <X className="mr-1.5 h-3.5 w-3.5" />
                    Clear {tag.name}
                  </CommandItem>
                </CommandGroup>
              </>
            )}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

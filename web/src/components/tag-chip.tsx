import { Link } from "react-router-dom";
import { X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { tagColorClass, tagHref } from "@/lib/tags";
import type { AppTag } from "@/lib/api";

interface TagChipProps {
  tag: AppTag;
  /** Links to the browse page. Off inside editors, where clicking means "remove". */
  link?: boolean;
  onRemove?: () => void;
  className?: string;
}

/**
 * Renders one key/value pair as `Key: Value`. Showing the key matters — a bare
 * "Engineering" doesn't say whether it is a department, a team, or an owner.
 */
export function TagChip({ tag, link = true, onRemove, className }: TagChipProps) {
  const body = (
    <>
      <span className="opacity-60">{tag.tag_name}:</span>
      <span className="font-medium">{tag.value}</span>
      {onRemove && (
        <button
          type="button"
          onClick={(e) => {
            e.preventDefault();
            e.stopPropagation();
            onRemove();
          }}
          className="-mr-0.5 ml-0.5 rounded-full p-0.5 opacity-60 transition-opacity hover:opacity-100"
          aria-label={`Remove ${tag.tag_name}`}
        >
          <X className="h-2.5 w-2.5" />
        </button>
      )}
    </>
  );

  const classes = cn(
    "gap-1 border text-2xs font-normal",
    tagColorClass(tag.color),
    className,
  );

  if (link && !onRemove) {
    return (
      <Badge variant="outline" className={cn(classes, "hover:brightness-95")} render={<Link to={tagHref(tag)} />}>
        {body}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className={classes}>
      {body}
    </Badge>
  );
}

/**
 * A row of chips, capped so a heavily tagged app can't blow out a table cell.
 */
export function TagChips({
  tags,
  max = 3,
  link = true,
  className,
}: {
  tags?: AppTag[];
  max?: number;
  /** Off inside a clickable row, where a nested link would fight the row's own navigation. */
  link?: boolean;
  className?: string;
}) {
  if (!tags || tags.length === 0) return null;
  const shown = tags.slice(0, max);
  const hidden = tags.length - shown.length;

  return (
    <div className={cn("flex flex-wrap items-center gap-1", className)}>
      {shown.map((t) => (
        <TagChip key={t.tag_id} tag={t} link={link} />
      ))}
      {hidden > 0 && (
        <span className="text-2xs text-muted-foreground" title={tags.slice(max).map((t) => `${t.tag_name}: ${t.value}`).join(", ")}>
          +{hidden}
        </span>
      )}
    </div>
  );
}

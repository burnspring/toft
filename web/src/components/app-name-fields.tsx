import { FieldHelp } from "@/components/field-help";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/** What a slug field accepts as it is typed: the server's own alphabet. */
function sanitizeSlug(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9-]/g, "-");
}

/**
 * An app's name and slug, as both doors that create one ask for them.
 *
 * The new-app wizard and the Gallery's deploy dialog put the same two fields in
 * front of the same server rule and worded them two ways — "derived from app
 * name" and "chosen from the name", "Leave blank to derive it" and "If the name
 * is taken" (it is the slug that collides). One component, so they cannot
 * drift again.
 */
export function AppNameFields({
  idPrefix,
  name,
  onNameChange,
  slug,
  onSlugChange,
  namePlaceholder = "Sales dashboard",
  slugPlaceholder = "sales-dashboard",
  autoFocusName = false,
}: {
  idPrefix: string;
  name: string;
  onNameChange: (name: string) => void;
  slug: string;
  onSlugChange: (slug: string) => void;
  namePlaceholder?: string;
  /** The shape of a slug — an example, never an instruction. */
  slugPlaceholder?: string;
  autoFocusName?: boolean;
}) {
  const nameId = `${idPrefix}-name`;
  const slugId = `${idPrefix}-slug`;
  return (
    <>
      <div className="space-y-1.5">
        <Label htmlFor={nameId}>Name</Label>
        <Input
          id={nameId}
          placeholder={namePlaceholder}
          value={name}
          onChange={(e) => onNameChange(e.target.value)}
          autoFocus={autoFocusName}
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor={slugId}>Slug (optional)</Label>
        <Input
          id={slugId}
          className="font-mono"
          placeholder={slugPlaceholder}
          value={slug}
          onChange={(e) => onSlugChange(sanitizeSlug(e.target.value))}
          aria-describedby={`${slugId}-help`}
        />
        <FieldHelp id={`${slugId}-help`}>
          The last part of this app’s address. Leave blank to use the name; a number is added if
          it’s taken.
        </FieldHelp>
      </div>
    </>
  );
}

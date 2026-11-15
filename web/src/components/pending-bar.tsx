import { Button } from "@/components/ui/button";

/**
 * What is typed but not yet saved.
 *
 * It exists because the alternative — a Save button per field, each disabled
 * until its own field changes — makes saving three related values three round
 * trips, and makes a half-typed change indistinguishable from a saved one on
 * the way to another page. Admin → Settings has had one since ADR §50 D6; the
 * app Settings tab had **five** save buttons and no page-wide indicator at
 * all, so somebody who had typed in three places could not answer *am I
 * saved?* from anything on the screen.
 *
 * Broken fields are counted separately. "2 changes, 1 needs fixing" says both
 * that the save is blocked and that most of the work is fine, which one
 * disabled button cannot.
 *
 * It does not know what it is saving, which is the point: the two surfaces
 * that mount it hold entirely different fields.
 */
export function PendingBar({
  count,
  broken,
  saving,
  onDiscard,
  onSave,
}: {
  count: number;
  broken: number;
  saving: boolean;
  onDiscard: () => void;
  onSave: () => void;
}) {
  if (count === 0) return null;
  return (
    <div className="fixed bottom-6 left-1/2 z-50 -translate-x-1/2 px-4">
      <div className="flex items-center gap-4 rounded-full border border-border/60 bg-card py-2 pl-5 pr-2 shadow-lg">
        <p className="text-sm" aria-live="polite">
          <span className="font-medium">
            {count} unsaved change{count === 1 ? "" : "s"}
          </span>
          {broken > 0 && (
            <span className="text-destructive">
              {" "}
              — {broken} need{broken === 1 ? "s" : ""} fixing
            </span>
          )}
        </p>
        <div className="flex items-center gap-1">
          <Button variant="ghost" size="sm" onClick={onDiscard} disabled={saving}>
            Discard
          </Button>
          <Button size="sm" className="rounded-full" onClick={onSave} disabled={saving || broken > 0}>
            {saving ? "Saving…" : "Save changes"}
          </Button>
        </div>
      </div>
    </div>
  );
}

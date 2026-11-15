import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { Section } from "@/components/section";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { FieldLabel, Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { api } from "@/lib/api";
import type { Announcement, AnnouncementLevel } from "@/lib/api";
import { useAuth } from "@/hooks/use-auth";
import { toneSurface, toneText } from "@/lib/tone";
import { FieldError } from "@/components/field-help";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { cn, fieldErrorId } from "@/lib/utils";

const MAX_LEN = 500;

export function AnnouncementEditor() {
  const { user } = useAuth();
  const queryClient = useQueryClient();
  const [current, setCurrent] = useState<Announcement | null>(null);
  const [message, setMessage] = useState("");
  const [level, setLevel] = useState<AnnouncementLevel>("info");
  const [saving, setSaving] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  /** When the last save landed, as an instant rather than a formatted string —
   * the one timestamp on the page was being formatted by hand, in a locale
   * nothing else here uses, and it stopped being true the moment it was
   * written. */
  const [savedAt, setSavedAt] = useState<string | null>(null);

  /**
   * The banner as it stands, read through the query cache like everything else.
   *
   * It was a `useEffect` with `.catch(() => {})`: a failed read left the form
   * empty and silent, which is indistinguishable from *there is no banner* —
   * so an administrator who could not read the current announcement was shown
   * **Publish banner** over a blank box, and pressing it would have replaced
   * whatever is actually live. Error before empty.
   */
  const { data: fetched, error: readError, refetch } = useQuery({
    queryKey: ["announcement"],
    queryFn: api.getAnnouncement,
    retry: false,
    enabled: user?.system_role === "system_admin",
  });

  // Seeded once. The banner and this editor share `["announcement"]`, so a
  // refetch — a window focus, another tab's save — would otherwise overwrite
  // whatever is half-typed in the box.
  // Adjusted during render, not in an effect: the first answer seeds the form,
  // and every answer after it only moves `current`.
  const [seenFetched, setSeenFetched] = useState<Announcement | undefined>(undefined);
  if (fetched && fetched !== seenFetched) {
    setSeenFetched(fetched);
    setCurrent(fetched);
    if (seenFetched === undefined && fetched.message) {
      setMessage(fetched.message);
      setLevel(fetched.level ?? "info");
    }
  }

  if (user?.system_role !== "system_admin") return null;

  const isActive = !!current?.message;
  const tooLong = message.length > MAX_LEN;

  const save = async () => {
    setSaving(true);
    try {
      const saved = await api.setAnnouncement({ message: message.trim(), level });
      setCurrent(saved);
      // The banner is on every route and reads the same fact by key. Without
      // this an administrator published an announcement and saw nothing until
      // they reloaded — with no way to tell a banner that had not arrived from
      // one that had not been saved.
      queryClient.invalidateQueries({ queryKey: ["announcement"] });
      setSavedAt(new Date().toISOString());
      // What is now true, and for whom: this is the one save whose reach is
      // every signed-in person.
      toast.success(isActive ? "Banner updated" : "Banner is live for everyone");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t publish the banner");
    } finally {
      setSaving(false);
    }
  };

  const clear = async () => {
    setSaving(true);
    try {
      await api.clearAnnouncement();
      setCurrent(null);
      queryClient.invalidateQueries({ queryKey: ["announcement"] });
      setMessage("");
      setLevel("info");
      setSavedAt(new Date().toISOString());
      setConfirmClear(false);
      toast.success("Banner cleared");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t clear the banner");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Section
      title="Announcement banner"
      description="Shown to everyone at the top of every page. Critical banners can’t be dismissed."
      actions={
        isActive ? (
          <Badge variant="outline" className={toneSurface.ok}>Active</Badge>
        ) : undefined
      }
    >
      <div className="space-y-4 p-4">
        {readError && (
          <QueryError
            title="The current banner could not be read"
            error={readError}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        )}
        <div className="space-y-2">
          <Label htmlFor="announcement-message">Message</Label>
          {/* Over the limit is flagged, never cut: a paste lost its tail. */}
          <Textarea
            id="announcement-message"
            value={message}
            aria-invalid={tooLong}
            aria-describedby={tooLong ? fieldErrorId("announcement-message") : undefined}
            onChange={(e) => setMessage(e.target.value)}
            rows={3}
            placeholder="e.g. Scheduled maintenance Saturday, Apr 2, 10 p.m.–midnight UTC."
          />
          <div
            className={cn(
              "flex justify-end text-xs",
              tooLong ? toneText.danger : "text-muted-foreground",
            )}
          >
            {message.length}/{MAX_LEN}
          </div>
          <FieldError field="announcement-message">
            {tooLong ? `Shorten this to ${MAX_LEN} characters.` : null}
          </FieldError>
        </div>

        <div className="space-y-2">
          <FieldLabel>Level</FieldLabel>
          {/* Not `ChoiceButtons`, which saves the moment one is chosen — this
              is a draft that goes out with the message beside it. What it
              borrows is the rendering: the product's own `Button`, and
              `aria-pressed` rather than a fill doing the whole job. */}
          <div role="group" aria-label="Announcement level" className="flex gap-2">
            {(["info", "warning", "critical"] as AnnouncementLevel[]).map((lv) => (
              <Button
                key={lv}
                type="button"
                size="sm"
                variant={level === lv ? "default" : "outline"}
                aria-pressed={level === lv}
                className="h-7 text-xs capitalize"
                onClick={() => setLevel(lv)}
              >
                {lv}
              </Button>
            ))}
          </div>
        </div>

        {/* The API answers `updated_at` and `updated_by` and both were being
            dropped, so an administrator arriving at this page was told nothing
            about a banner somebody else had published — only about their own
            save, and only in the session that made it. The local instant still
            wins after a save, because it is the newer of the two. */}
        {(savedAt || current?.updated_at) && (
          <p className="text-xs text-muted-foreground">
            Updated <RelativeTime iso={savedAt ?? current!.updated_at!} />
            {!savedAt && current?.updated_by ? ` by ${current.updated_by}` : ""}.
          </p>
        )}

        <div className="flex items-center gap-2 pt-1">
          <Button onClick={save} disabled={saving || !message.trim() || tooLong} size="sm">
            {saving
              ? isActive
                ? "Updating…"
                : "Publishing…"
              : isActive
                ? "Update banner"
                : "Publish banner"}
          </Button>
          {isActive && (
            <Button
              onClick={() => setConfirmClear(true)}
              disabled={saving}
              size="sm"
              variant="outline"
            >
              Clear…
            </Button>
          )}
        </div>
      </div>

      {/* Consequential and recoverable only by retyping it: clearing changes
          what every signed-in person sees, and the message is gone from the
          server the moment it lands. A plain confirm that states what survives. */}
      <Dialog open={confirmClear} onOpenChange={setConfirmClear}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Clear the announcement banner?</DialogTitle>
            <DialogDescription>
              Everyone stops seeing it on their next page. The message isn’t kept.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={saving} onClick={() => setConfirmClear(false)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={clear} disabled={saving}>
              {saving ? "Clearing…" : "Clear banner"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Section>
  );
}

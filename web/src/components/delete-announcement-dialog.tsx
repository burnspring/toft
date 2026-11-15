import { useState } from "react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import type { AnnouncementEmail } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Notice } from "@/components/notice";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { ConfirmPhrase } from "@/components/confirm-phrase";

/**
 * Deleting an announcement — the draft nobody got, and the record of one
 * everybody got.
 *
 * **They are two different acts and this is one dialog, because the difference
 * is what it has to say out loud.** A draft has reached nobody, so it is a
 * plain confirm; a sent row is the install's own history and the per-person
 * list of who was told, so it is type-to-confirm on the subject — irreversible
 * and unbounded, which is the tier an app deletion is at.
 *
 * The notice on the sent case is the honest half of that: what does not go is
 * the audit line, which keeps the subject, the audience, the date and all five
 * counts. Somebody deleting a record deserves to know that the statement it was
 * making survives it, and somebody reading the audit log a year later deserves
 * to find out that it once existed.
 */
export function DeleteAnnouncementDialog({
  announcement,
  open,
  onOpenChange,
  onDeleted,
}: {
  announcement: AnnouncementEmail;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDeleted: () => void;
}) {
  const [confirmText, setConfirmText] = useState("");
  const [deleting, setDeleting] = useState(false);

  const isDraft = announcement.status === "draft";
  const ready = isDraft || confirmText.trim() === announcement.subject.trim();

  const remove = async () => {
    setDeleting(true);
    try {
      await api.deleteAnnouncementEmail(announcement.id);
      onOpenChange(false);
      onDeleted();
      toast.success(isDraft ? "Draft deleted" : "Announcement deleted");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t delete this announcement");
    } finally {
      setDeleting(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setConfirmText("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{isDraft ? "Delete this draft?" : "Delete this announcement?"}</DialogTitle>
          <DialogDescription>
            {isDraft ? (
              <>“{announcement.subject}” has not been sent to anybody, and nothing else refers to it.</>
            ) : (
              <>
                This removes “{announcement.subject}” and the per-person list of who it
                reached. Type{" "}
                <ConfirmPhrase text={announcement.subject} /> to confirm.
              </>
            )}
          </DialogDescription>
        </DialogHeader>

        {!isDraft && (
          <>
            <Notice tone="attention" title="Activity keeps a line">
              Admin → Activity keeps what was deleted: the subject, the audience, when
              it went out, who sent it and all five counts. The message and the list
              of people do not come back.
            </Notice>
            <Input
              value={confirmText}
              onChange={(e) => setConfirmText(e.target.value)}
              placeholder={announcement.subject}
              className="font-mono"
              aria-label="Type the subject to confirm"
              autoFocus
            />
          </>
        )}

        <DialogFooter>
          <Button variant="outline" disabled={deleting} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={!ready || deleting} onClick={remove}>
            {deleting ? "Deleting…" : isDraft ? "Delete draft" : "Delete permanently"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

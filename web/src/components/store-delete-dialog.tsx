import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogBody,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { formatBytes } from "@/lib/bytes";
import { formatDateTime } from "@/lib/time";
import type { StoreObjectSummary } from "@/lib/api";
import { MAX_DELETE_PER_ACTION, deleteObject } from "@/lib/store-actions";
import { ConfirmPhrase } from "@/components/confirm-phrase";

/**
 * Deleting objects, which an object store cannot undo.
 *
 * Irreversible, so type-to-confirm — the policy's third tier, and the same
 * shape `delete-app-dialog.tsx` uses. What is typed differs with the size of
 * the act: one object asks for the **object's name**, because that is the thing
 * being destroyed and typing it is reading it; a selection asks for the
 * **store's slug**, because typing forty names is not a confirmation, it is an
 * obstacle course somebody clicks past.
 *
 * **The rows come in, not the names.** Every object is named by its full key
 * and carries its own size, so the total below is the total of the thing being
 * destroyed even when it is a folder this dialog's caller never listed — a
 * folder delete that confirmed "0 B" was asking about the wrong objects.
 *
 * **Partial failure is reported as itself.** Each object is its own signed
 * DELETE, so nine can succeed and one can be refused; one toast claiming the
 * whole selection went would be a lie about data.
 */
export function DeleteObjectsDialog({
  storeSlug,
  storeName,
  objects,
  open,
  onOpenChange,
  onDeleted,
}: {
  storeSlug: string;
  storeName: string;
  /** The objects to destroy, each named by its **full key** — what the presign
   * endpoints resolve against — and carrying its own size. */
  objects: StoreObjectSummary[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDeleted: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  /** How many of the loop below have been spent. One signed DELETE per object
   * means a hundred is a hundred round trips, and a button reading "Deleting…"
   * for a minute is indistinguishable from one that has hung. */
  const [done, setDone] = useState(0);

  const names = objects.map((o) => o.name);
  const single = objects.length === 1 ? objects[0] : null;
  // The leaf, not the key: a person confirms by reading back the name of the
  // file, and asking them to retype three folders in front of it is the kind of
  // friction that gets pasted rather than read.
  const expected = single ? (single.name.split("/").pop() ?? single.name) : storeSlug;
  const totalBytes = objects.reduce((n, o) => n + o.size, 0);
  const overCap = objects.length > MAX_DELETE_PER_ACTION;

  async function handleDelete() {
    setBusy(true);
    setDone(0);
    let deleted = 0;
    const refused: string[] = [];
    for (const name of names) {
      try {
        await deleteObject(storeSlug, name);
        deleted++;
      } catch (err) {
        refused.push(err instanceof Error ? err.message : name);
      }
      setDone((n) => n + 1);
    }
    setBusy(false);

    if (refused.length === 0) {
      toast.success(deleted === 1 ? `Deleted “${names[0]}”` : `Deleted ${deleted} files`);
    } else if (deleted === 0) {
      toast.error(`Nothing was deleted. ${refused[0]}`);
    } else {
      toast.error(`${deleted} deleted, ${refused.length} refused. ${refused[0]}`);
    }
    onOpenChange(false);
    onDeleted();
  }

  return (
    <Dialog
      open={open}
      // Held open while the loop runs: closing mid-delete hid the progress
      // while the deletes went on, and the result toast arrived with no
      // context.
      onOpenChange={(o) => {
        if (busy) return;
        if (!o) setTyped("");
        onOpenChange(o);
      }}
    >
      <DialogContent scrollable>
        <DialogHeader>
          <DialogTitle>
            {single ? "Delete this file?" : `Delete ${objects.length} files?`}
          </DialogTitle>
          <DialogDescription>
            {single ? (
              <>
                <span className="font-mono text-foreground">{single.name}</span> —{" "}
                {formatBytes(single.size)}, last modified {formatDateTime(single.last_modified)}.
                Deleted files can’t be recovered. Type{" "}
                <ConfirmPhrase text={expected} /> to confirm.
              </>
            ) : (
              <>
                {formatBytes(totalBytes)} in {storeName}. Deleted files can’t be recovered. Type{" "}
                <ConfirmPhrase text={storeSlug} /> to confirm.
              </>
            )}
          </DialogDescription>
        </DialogHeader>

        {!single && (
          // The list is the part that scrolls, so the count above it and the
          // confirmation below it are both always on screen. `DialogBody` is
          // that pattern, named — see `components/ui/dialog.tsx`.
          <DialogBody className="rounded-md border border-border/60 bg-muted/30 p-2 font-mono text-xs">
            <ul>
            {names.map((name) => (
              <li key={name} className="truncate py-0.5 text-foreground">
                {name}
              </li>
            ))}
            </ul>
          </DialogBody>
        )}

        {overCap && (
          <p className="text-xs text-tone-danger-fg">
            One action deletes at most {MAX_DELETE_PER_ACTION} files. Select fewer.
          </p>
        )}

        {/* A placeholder is not a label: it disappears the moment somebody
            types, which on a type-to-confirm field is exactly when they need
            to check what they were asked for. */}
        <Input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          placeholder={expected}
          aria-label={`Type ${expected} to confirm`}
          className="font-mono"
          autoFocus
        />

        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={typed !== expected || busy || overCap}
            onClick={() => void handleDelete()}
          >
            {busy
              ? single
                ? "Deleting…"
                : `Deleting ${done} of ${objects.length}…`
              : single
                ? "Delete permanently"
                : `Delete ${objects.length} files`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { ConfirmPhrase } from "@/components/confirm-phrase";

interface DeleteAppDialogProps {
  app: { id: string; name: string; slug: string };
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDeleted?: () => void;
}

/** Type-to-confirm deletion dialog — the only path to api.deleteApp. */
export function DeleteAppDialog({ app, open, onOpenChange, onDeleted }: DeleteAppDialogProps) {
  const queryClient = useQueryClient();
  const [confirmText, setConfirmText] = useState("");
  const [deleting, setDeleting] = useState(false);

  async function handleDelete() {
    setDeleting(true);
    try {
      await api.deleteApp(app.id);
      // Every key this act changes, not only the list. The header's
      // knowledge-base link resolves an app by slug and went on pointing at
      // this one until a reload; the estate and reach reports counted it; and
      // its own detail query would have answered from cache.
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["app", app.id] });
      queryClient.invalidateQueries({ queryKey: ["knowledge-base"] });
      queryClient.invalidateQueries({ queryKey: ["documentation"] });
      queryClient.invalidateQueries({ queryKey: ["estate-targets"] });
      toast.success(`Deleted “${app.name}”`);
      onOpenChange(false);
      onDeleted?.();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t delete “${app.name}”`);
    } finally {
      setDeleting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) setConfirmText(""); onOpenChange(o); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete {app.name}?</DialogTitle>
          <DialogDescription>
            This permanently removes the app, its deployments, logs, schedules and saved data. Type{" "}
            <ConfirmPhrase text={app.slug} /> to confirm.
          </DialogDescription>
        </DialogHeader>
        {/* Named. A placeholder is not a label — it disappears on the first
            keystroke and is announced as a hint or not at all — and this is
            the one type-to-confirm field in the product that reaches
            `api.deleteApp`. */}
        <Input
          value={confirmText}
          onChange={(e) => setConfirmText(e.target.value)}
          aria-label={`Type ${app.slug} to confirm deleting this app`}
          className="font-mono"
          autoFocus
        />
        <DialogFooter>
          <Button
            variant="outline"
            disabled={deleting}
            onClick={() => { setConfirmText(""); onOpenChange(false); }}
          >
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={confirmText !== app.slug || deleting}
            onClick={handleDelete}
          >
            {deleting ? "Deleting…" : "Delete permanently"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

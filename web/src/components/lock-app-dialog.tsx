import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { FieldHelp } from "@/components/field-help";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { api } from "@/lib/api";

/** Matches the server's messageLimit. A client bound that mirrors the server's
 * exactly, per the settings convention. */
export const LOCK_MESSAGE_MAX = 600;

/**
 * Locking an app, wherever the decision is taken.
 *
 * One dialog rather than one per surface. The lock is seven enforcement points
 * and a message a refused visitor reads, so a second copy of this — with its
 * own wording about what survives, its own idea of whether the message is
 * required, its own set of invalidated queries — would be two products
 * disagreeing about the same button. The Sharing tab takes this decision on one
 * app; Admin → Usage takes it on a row of the quiet list; they are the same
 * decision.
 *
 * **Consequential and recoverable, so a plain confirmation** that states what
 * survives — never type-to-confirm, which is for what cannot be undone. The
 * message field lives in the dialog because writing it is part of the decision,
 * not a setting to adjust afterwards.
 */
export function LockAppDialog({
  appId,
  appName,
  open,
  onOpenChange,
  defaultMessage = "",
}: {
  appId: string;
  appName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Seeds the field where the caller knows something useful to say — the quiet
   *  list knows how long the app has been quiet, and a message the reader of
   *  the lock can act on beats an empty box. Still editable, and still
   *  optional. */
  defaultMessage?: string;
}) {
  const queryClient = useQueryClient();
  const [message, setMessage] = useState(defaultMessage);
  const [busy, setBusy] = useState(false);

  async function lock() {
    setBusy(true);
    try {
      await api.lockApp(appId, message);
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      queryClient.invalidateQueries({ queryKey: ["quiet-apps"] });
      queryClient.invalidateQueries({ queryKey: ["admin-dependencies"] });
      // Names the app and the consequence, like its own unlock on the same
      // page — "App locked" beside "App unlocked — it is stopped until
      // somebody starts it" was two toasts about one act in two registers,
      // and the terser one is the one describing the destructive direction.
      toast.success(`“${appName}” is locked`, {
        description: "It is stopped, and deploys and jobs are refused until it is unlocked.",
      });
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t lock “${appName}”`);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) onOpenChange(false); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Lock {appName}?</DialogTitle>
          <DialogDescription>
            The app stops and everyone who opens it sees your message instead. Deploys and jobs are
            refused while it is locked. Its versions, data, grants and settings
            are all kept, and unlocking puts them back.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-1.5">
          <Label htmlFor="lock-message" className="text-sm font-medium">
            What visitors are told
          </Label>
          <Textarea
            id="lock-message"
            rows={3}
            value={message}
            placeholder="Decommissioned in March — the reports moved to Finance Hub."
            onChange={(e) => setMessage(e.target.value.slice(0, LOCK_MESSAGE_MAX))}
          />
          <div className="flex items-start justify-between gap-4">
            <FieldHelp>
              Optional. Left empty, visitors see this install’s standard message for a locked app,
              which you can edit under Admin → Messages.
            </FieldHelp>
            <span className="shrink-0 text-xs text-muted-foreground">
              {message.length}/{LOCK_MESSAGE_MAX}
            </span>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={busy} onClick={lock}>
            {busy ? "Locking…" : "Lock app"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

import { useState } from "react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";

/**
 * Stopping somebody else's app.
 *
 * The confirmation table has two rows for one verb: stopping *your own* app
 * takes none, because it is reversible and the button beside it is the undo;
 * stopping somebody else's is a plain confirm, "because it is somebody else's
 * users who notice".
 *
 * Whose users notice is a property of the **app**, not of the page you happened
 * to open — and for a long time it was the page. An administrator could take
 * any app on the install down from the apps list with one click of a ⋯ item
 * that looked exactly like the one on their own row, and got a dialog for the
 * identical act one nav group away, on Processes. So there is one dialog, and
 * `stopNeedsConfirm` — in `lib/stop-tier.ts`, because a file exporting both a
 * component and a plain function cannot hot-reload — is the one test of which
 * tier applies.
 *
 * `onStopped` is a prop rather than a fixed invalidation set: Processes has
 * three keys to refresh and the app page has two, and folding them together
 * would leave one of the two surfaces stale.
 */
export function StopAppDialog({
  appId,
  appName,
  open,
  onOpenChange,
  onStopped,
}: {
  appId: string;
  appName: string;
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onStopped: () => void;
}) {
  const [stopping, setStopping] = useState(false);

  async function submit() {
    setStopping(true);
    try {
      await api.stop(appId);
      toast.success(`Stopped “${appName}”`);
      onOpenChange(false);
      onStopped();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t stop “${appName}”`);
    } finally {
      setStopping(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Stop {appName}?</DialogTitle>
          <DialogDescription>
            Anyone using it now gets an error until it is started again. Nothing is deleted: the
            deployment, its data, and its settings stay, and the owner or an administrator can
            start it from the app’s page.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={stopping} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={submit} disabled={stopping}>
            {stopping ? "Stopping…" : "Stop app"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

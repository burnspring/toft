import { useState } from "react";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Upload } from "lucide-react";
import { ArchiveDropzone } from "@/components/archive-dropzone";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { removedSummary } from "@/lib/removed";

/**
 * A new version of an uploaded app.
 *
 * This replaced a bare hidden `<input type="file">` behind the header button —
 * no size check, no guidance, and no way to see what had been picked before it
 * was already uploading. A dialog rather than an inline zone because the
 * guidance is the point (ADR §60) and a header button has nowhere to put it,
 * and because picking a 200 MB file by accident should be undoable before the
 * bytes move.
 */
export function UploadVersionDialog({
  appId,
  open,
  onOpenChange,
}: {
  appId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [file, setFile] = useState<File | null>(null);
  const [uploading, setUploading] = useState(false);
  useUnloadGuard(uploading, "A new version is still uploading.");

  async function handleUpload() {
    if (!file) return;
    setUploading(true);
    try {
      const deployment = await api.uploadAppVersion(appId, file);
      const removed = removedSummary(deployment.removed);
      toast.success(
        removed
          ? `New version uploaded — deployment started. ${removed}`
          : "New version uploaded — deployment started"
      );
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["deployments", appId] });
      onOpenChange(false);
      setFile(null);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t upload new version");
    } finally {
      setUploading(false);
    }
  }

  return (
    <Dialog
      open={open}
      // **Not while the bytes are moving.** Cancel is disabled during the
      // upload and so is the dropzone, but Escape and the close cross were
      // not — and dismissing this dialog does not cancel the request, so a
      // reflex press left a hundred-megabyte upload running behind a closed
      // dialog with nothing on screen saying so, and a deploy arriving some
      // minutes later that nobody was expecting.
      onOpenChange={(next) => {
        if (uploading) return;
        onOpenChange(next);
        if (!next) setFile(null);
      }}
    >
      <DialogContent className="sm:max-w-lg" showCloseButton={!uploading}>
        <DialogHeader>
          <DialogTitle>Upload a new version</DialogTitle>
          <DialogDescription>
            The archive replaces this app’s source and deploys straight away. The app keeps
            serving its current deployment until the new one is healthy.
          </DialogDescription>
        </DialogHeader>

        <ArchiveDropzone file={file} onFile={setFile} disabled={uploading} />

        {uploading && (
          <p className="text-xs text-muted-foreground">
            Uploading. Keep this page open until it finishes.
          </p>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={uploading}>
            Cancel
          </Button>
          <Button onClick={handleUpload} disabled={!file || uploading}>
            {uploading ? (
              "Uploading…"
            ) : (
              <>
                <Upload className="mr-1.5 h-3.5 w-3.5" />
                Upload and deploy
              </>
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

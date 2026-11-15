import { useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { Input } from "@/components/ui/input";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { api } from "@/lib/api";
import type { StorageDeleteResult, StorageDrive, StorageResource } from "@/lib/api";
import { audienceSummary, deleteRemovesData } from "@/lib/storage";
import { ConfirmPhrase } from "@/components/confirm-phrase";

/**
 * What a delete does to the data is the one thing a reader needs to be sure of
 * before clicking, so the dialog leads with it — and it is the **drive's
 * verdict** (ADR §12a), never a guess from the backing. Where the files go —
 * a writable host root, or a cluster class whose driver removes them — the
 * dialog is type-to-confirm on the slug: irreversible and unbounded, the tier a
 * store delete already sits at. Where they stay, it says so.
 *
 * Its own file because two places delete a resource — the list's row menu and
 * the resource's own page — and a second copy of this dialog is a second
 * chance to describe what survives differently from the first.
 */
export function DeleteStorageResourceDialog({
  resource, drives, onOpenChange, onDeleted,
}: {
  resource: StorageResource | null;
  drives: StorageDrive[];
  onOpenChange: () => void;
  onDeleted: () => void;
}) {
  const [saving, setSaving] = useState(false);
  const [typed, setTyped] = useState("");
  const { pathname } = useLocation();
  const inUse = (resource?.mapping_count ?? 0) > 0;
  const deletesFiles = !!resource && deleteRemovesData(resource, drives);
  const onHost = !!resource?.storage_root;
  // Grants do not block the delete the way mappings do — an app losing a mount
  // mid-flight breaks it, and a person losing a grant is told by the next URL
  // they ask for. But they are the half of "who reaches this" that no app list
  // shows, so the one moment they have to be said out loud is this one.
  const audience = (resource?.grant_users ?? 0) + (resource?.grant_groups ?? 0);

  async function submit() {
    if (!resource) return;
    setSaving(true);
    try {
      const result = await api.deleteStorageResource(resource.id);
      const { title, description } = deleteToast(resource.name, result);
      toast.success(title, { description });
      setTyped("");
      onOpenChange();
      onDeleted();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t delete “${resource.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={!!resource} onOpenChange={() => { setTyped(""); onOpenChange(); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete {resource?.name}?</DialogTitle>
          <DialogDescription>
            {inUse
              ? `${resource?.mapping_count} app${resource?.mapping_count === 1 ? " is" : "s are"} still attached to this. Detach ${resource?.mapping_count === 1 ? "it" : "them"} first.`
              : deletesFiles
                ? onHost
                  ? "This deletes its folder on the host and every file in it. It can’t be undone."
                  : "This deletes every file in it, in every folder, from the cluster’s file system. It can’t be undone, and its name stays reserved until the cluster has finished."
                : resource?.kind === "volume"
                  ? "Toft stops tracking this and apps lose it. Its files stay on the file system — this drive doesn’t remove them."
                  : "Toft stops tracking this resource and apps lose it. The data stays where it is — nothing stored is deleted."}
            {!inUse && audience > 0 && (
              <>
                {" "}
                {audienceSummary(resource?.grant_users ?? 0, resource?.grant_groups ?? 0)}
                {" can reach it from outside today, and will stop being able to."}
              </>
            )}
            {!inUse && deletesFiles && (
              <>
                {" "}Type <ConfirmPhrase text={resource?.slug ?? ""} /> to confirm.
              </>
            )}
          </DialogDescription>
        </DialogHeader>
        {!inUse && deletesFiles && (
          <Input
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            aria-label={`Type ${resource?.slug} to confirm deleting its files`}
            className="font-mono"
            autoFocus
          />
        )}
        {/* In use, the server would refuse, so there is no destructive button
            to press — only the way to the apps that hold it, unless this is
            already that page. */}
        <DialogFooter>
          <Button variant="outline" onClick={onOpenChange} disabled={saving}>
            Cancel
          </Button>
          {inUse ? (
            resource && pathname !== `/admin/storage/${resource.slug}` && (
              <Link
                to={`/admin/storage/${resource.slug}`}
                className={buttonVariants()}
                onClick={onOpenChange}
              >
                Open {resource.name}
              </Link>
            )
          ) : (
            <Button
              variant="destructive"
              onClick={submit}
              disabled={saving || (deletesFiles && typed !== resource?.slug)}
            >
              {saving ? "Deleting…" : "Delete"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** What a finished delete says, from what the server reports it did. */
function deleteToast(name: string, result: StorageDeleteResult | undefined): { title: string; description: string } {
  const left = result?.left_behind ?? [];
  const kept = left.length
    ? `Kept: ${left[0].what} — ${left[0].reason}${left.length > 1 ? ` (and ${left.length - 1} more)` : ""}`
    : "";
  if (result?.state === "deleting") {
    return {
      title: `Deleting “${name}”`,
      description: kept || "Its files go once the cluster lets go — usually minutes, at most an hour. The name stays reserved until then.",
    };
  }
  if (kept) return { title: `Deleted “${name}”`, description: kept };
  return {
    title: `Deleted “${name}”`,
    description: result?.removes_data
      ? "Its folder and every file in it are gone."
      : "The data is still there; only Toft’s record of it is gone.",
  };
}

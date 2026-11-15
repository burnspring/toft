import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { DEPLOY_IN_PROGRESS } from "@/pages/app/state";

import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

import { StatusBadge } from "@/components/status-badge";
import { StatusDot } from "@/components/status-dot";

import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { TableCard } from "@/components/table-card";
import { Skeleton } from "@/components/ui/skeleton";

import { formatDuration, formatTimestamp } from "@/lib/time";
import { RelativeTime } from "@/components/relative-time";
import { formatBytes, formatOptionalBytes } from "@/lib/bytes";

import {
  CheckCircle2,
  GitCommit,
  GitBranch,
  MoreHorizontal,
  RotateCcw,
  Trash2,
  Download,
  Upload,
  Loader2,
  ScrollText,
} from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

import { api } from "@/lib/api";

import type { Deployment } from "@/lib/api";

import { rowActivation } from "@/lib/row-activation";

// ── Deployments Tab ──────────────────────────────────────────

export function DeploymentsTab({
  appId,
  deployments,
  loading,
  error,
  onRetry,
  canEdit,
}: {
  appId: string;
  deployments: Deployment[];
  /** The parent's read, so the empty branch is only reached once the answer
   *  has landed. Without it an owner with fifty builds was told, for one
   *  frame, to press a button in a different component. */
  loading: boolean;
  /** And the same read's failure, for the same reason one step further: an
   *  owner whose build history could not be read was told "No deployments
   *  yet. Use Redeploy in the header" — an app with fifty builds, described
   *  as an app with none, at the moment somebody is looking for one to roll
   *  back to. Error comes before empty. */
  error: unknown;
  onRetry?: () => void;
  canEdit: boolean;
}) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [rollingBackId, setRollingBackId] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<Deployment | null>(null);
  const [bulkDeleting, setBulkDeleting] = useState(false);

  const deployInFlight = deployments.some((d) =>
    DEPLOY_IN_PROGRESS.has(d.status),
  );

  // Which builds are deletable, and what deleting them would return, comes from
  // the server: after a rollback the running release and the row that records
  // it are different rows, and guessing at that here would offer to delete the
  // code the app is executing.
  // A column is a statement about each row, shown when some row has
  // something to say: an app that has only ever been uploaded has no branch.
  const anyBranch = deployments.some((d) => !isUpload(d));
  const deletable = deployments.filter((d) => !d.protected_reason);
  const reclaimable = deletable.reduce(
    (sum, d) => sum + (d.disk_bytes ?? 0) + (d.artifact_bytes ?? 0),
    0,
  );

  async function handleRollback(d: Deployment) {
    setRollingBackId(d.id);
    try {
      await api.rollbackDeployment(appId, d.id);
      toast.success("Making this deployment current");
      queryClient.invalidateQueries({ queryKey: ["app", appId] });
      queryClient.invalidateQueries({ queryKey: ["deployments", appId] });
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : "Couldn’t make this deployment current",
      );
    } finally {
      setRollingBackId(null);
    }
  }

  function reclaimed(freed: number) {
    return freed > 0 ? ` — ${formatBytes(freed)} reclaimed` : "";
  }

  function afterDelete() {
    queryClient.invalidateQueries({ queryKey: ["app", appId] });
    queryClient.invalidateQueries({ queryKey: ["deployments", appId] });
  }

  async function handleDelete(d: Deployment) {
    try {
      const res = await api.deleteDeployment(appId, d.id);
      toast.success(
        (d.commit_sha ? `Deleted deployment ${d.commit_sha.slice(0, 7)}` : "Deleted the deployment") +
          reclaimed(res.freed_disk_bytes + res.freed_artifact_bytes),
      );
      afterDelete();
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : "Couldn’t delete the deployment",
      );
      throw err;
    }
  }

  /**
   * Delete every build but the one being served, with no dialog in the way.
   *
   * The line beside the button is what a confirmation would have said — how
   * many builds and how much disk — so it is read before the click rather than
   * after it, and the toast names what actually went. Nothing here is rethrown:
   * there is no dialog left to keep open, and an unhandled rejection is the
   * only thing a throw would produce.
   */
  async function handleBulkDelete() {
    setBulkDeleting(true);
    try {
      const res = await api.deleteNonCurrentDeployments(appId);
      toast.success(
        `Deleted ${res.deleted} deployment${res.deleted === 1 ? "" : "s"}` +
          reclaimed(res.freed_disk_bytes + res.freed_artifact_bytes),
      );
      afterDelete();
    } catch (err) {
      toast.error(
        err instanceof Error ? err.message : "Couldn’t delete old deployments",
      );
    } finally {
      setBulkDeleting(false);
    }
  }

  /**
   * Open one build's log — which is a **page**, not a dialog.
   *
   * A build log is the thing people send to someone else: four hundred lines of
   * renv traceback naming a shared object, read by somebody who is not looking
   * at this screen. While it lived in a modal the only way to say *look at this
   * build* was to describe the clicks. Now it has an address (ADR §87), and this
   * tab keeps what it is actually for — the artifacts: status, commit, size,
   * rollback, ZIP, delete. None of those is a log.
   */
  function viewBuildLog(d: Deployment) {
    navigate(`/apps/${appId}/logs?source=build&build=${d.id}`);
  }

  return (
    <div className="space-y-3 pt-4">
      {canEdit && deletable.length > 0 && (
        <TableCard flat className="flex items-center justify-between gap-4 px-4 py-2.5">
          <p className="text-xs text-muted-foreground">
            {deletable.length} old deployment{deletable.length === 1 ? "" : "s"} held
            on disk
            {reclaimable > 0 && <> · {formatBytes(reclaimable)} reclaimable</>}
          </p>
          <Button
            variant="outline"
            size="sm"
            disabled={bulkDeleting}
            onClick={handleBulkDelete}
          >
            <Trash2 className="h-3 w-3 mr-1.5" />
            {bulkDeleting ? "Deleting…" : "Delete old deployments"}
          </Button>
        </TableCard>
      )}

      {loading ? (
        <TableCard className="space-y-2 p-4">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </TableCard>
      ) : error ? (
        <QueryError
          title="This app’s deployments could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see its deployments."
          onRetry={onRetry}
        />
      ) : deployments.length === 0 ? (
        <EmptyState
          icon={<GitCommit className="h-6 w-6" />}
          title="No deployments yet"
          description="Redeploy, or upload a new version, to deploy this app."
        />
      ) : (
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                {/* Status, commit and the actions stay: which build, whether
                    it worked, and what can be done about it. Branch, duration
                    and size yield, and the commit line carries the branch and
                    when it started at the widths that drop them. */}
                <TableHead>Status</TableHead>
                <TableHead className="w-full">Commit</TableHead>
                {anyBranch && <TableHead className="hidden @3xl:table-cell">Branch</TableHead>}
                <TableHead className="hidden @xl:table-cell">Started</TableHead>
                <TableHead className="hidden @4xl:table-cell">Duration</TableHead>
                <TableHead className="hidden @4xl:table-cell">Size</TableHead>
                <TableHead className="text-right"><span className="sr-only">Actions</span></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {deployments.map((d) => (
                <TableRow
                  key={d.id}
                  {...rowActivation(
                    `Open the build log for ${d.commit_sha?.slice(0, 7) || d.id.slice(0, 8)}`,
                    () => viewBuildLog(d),
                  )}
                >
                  <TableCell>
                    {/* One pill per row — the outcome — and Current as a
                        marked word beside it. Two pills stacked in a column
                        this narrow read as two outcomes. */}
                    <div className="flex items-center gap-2 whitespace-nowrap">
                      {/* The ordinary outcome is a dot and a word; the
                          filled pill is kept for the rows worth a look —
                          failed, building, cancelled. A column of green
                          pills said nothing, and a red one among them had
                          to be found rather than seen. */}
                      {d.status === "success" ? (
                        <StatusDot status={d.status} showLabel className="text-xs text-muted-foreground" />
                      ) : (
                        <StatusBadge status={d.status} />
                      )}
                      {d.current && (
                        <span className="inline-flex items-center gap-1 text-xs font-medium text-tone-ok-fg">
                          <CheckCircle2 className="h-3.5 w-3.5" />
                          {/* The word yields below the card's `@md` tier, where
                              the cell cannot hold a pill and a word side by
                              side; the tick stays and is still named. */}
                          <span className="sr-only @md:not-sr-only">Current</span>
                        </span>
                      )}
                      {/* A promote makes the source row current in place, so
                          Serving only differs from Current on rows written
                          before that change — say which one is on disk. */}
                      {d.serving && !d.current && (
                        <StatusBadge status="active" label="Serving" />
                      )}
                      {d.triggered_by === "auto" && (
                        <StatusBadge status="auto" />
                      )}
                      {/* A notebook's scheduled re-render. Its own badge and
                          not "auto": auto means a push landed, and this build
                          has no commit of its own behind it. */}
                      {d.triggered_by === "render" && (
                        <StatusBadge status="render" />
                      )}
                      {/* Only in-flight and failed promotes keep a row of
                          their own; a successful one marks its source build
                          Current instead. */}
                      {d.triggered_by === "rollback" && (
                        <StatusBadge status="rollback" />
                      )}
                    </div>
                  </TableCell>
                  <TableCell>
                    {/* A build with no commit is not a build with a missing
                        one: an uploaded archive has no repository to have come
                        from. Say the fact rather than drawing a placeholder in
                        the shape of an absent SHA. */}
                    {d.commit_sha ? (
                      <span className="inline-flex items-center gap-1 font-mono text-xs">
                        <GitCommit className="h-3 w-3 text-muted-foreground" />
                        {d.commit_sha.slice(0, 7)}
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                        <Upload className="h-3 w-3" />
                        Uploaded
                      </span>
                    )}
                    {/* Each restatement on its own column's container tier.
                        Read on the viewport, Branch was printed twice from
                        1024px up to the card's `@3xl`, and Started from 768px
                        to `@xl`. */}
                    <span className="mt-0.5 flex flex-wrap gap-x-2 text-2xs text-muted-foreground @3xl:hidden">
                      {!isUpload(d) && <span className="@3xl:hidden">{d.branch}</span>}
                      <span className="@xl:hidden">
                        <RelativeTime iso={d.started_at} />
                      </span>
                    </span>
                  </TableCell>
                  {anyBranch && (
                    <TableCell className="hidden @3xl:table-cell">
                      {/* An upload's branch is the pseudo-branch `upload`, which
                          beside a branch icon claimed a branch of that name. */}
                      {!isUpload(d) ? (
                        <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                          <GitBranch className="h-3 w-3" />
                          {d.branch}
                        </span>
                      ) : null}
                      {/* Nothing for an upload: the Commit cell beside it already
                          says Uploaded, and "Not from Git" was that fact twice. */}
                    </TableCell>
                  )}
                  <TableCell className="hidden text-xs text-muted-foreground @xl:table-cell">
                    <RelativeTime iso={d.started_at} />
                  </TableCell>
                  <TableCell className="hidden text-xs text-muted-foreground @4xl:table-cell">
                    {formatDuration(d.started_at, d.finished_at)}
                  </TableCell>
                  <TableCell className="hidden text-xs @4xl:table-cell">
                    {/* A rollback stores nothing of its own, so both figures
                        are blank on those rows rather than repeating the size
                        of the build it serves. */}
                    <div className="text-muted-foreground">
                      {formatOptionalBytes(d.disk_bytes)}
                    </div>
                    {d.artifact_bytes != null && (
                      <div className="text-2xs text-muted-foreground">
                        {formatBytes(d.artifact_bytes)} packed
                      </div>
                    )}
                  </TableCell>
                  {/* One menu, not four controls of four different shapes.
                      The cell held a text button, a second text button that
                      appeared on some rows, an icon-and-text link, and a bare
                      red trash — and because they flowed from the left, "ZIP"
                      landed at a different x on every row. `View log` stays out
                      as the button, because it is what somebody opening this
                      table came for; the rest are per-row acts and belong
                      behind the ⋯ that every other row in the product uses. */}
                  <TableCell>
                    <div className="flex items-center justify-end gap-1">
                      <Button
                        variant="outline"
                        size="xs"
                        onClick={(e) => {
                          e.stopPropagation();
                          viewBuildLog(d);
                        }}
                      >
                        <ScrollText className="mr-1 h-3 w-3" />
                        View log
                      </Button>
                      <DropdownMenu>
                        <DropdownMenuTrigger
                          render={
                            <Button
                              variant="ghost"
                              size="icon-xs"
                              aria-label={`Actions for the deployment started ${formatTimestamp(d.started_at)}`}
                              onClick={(e) => e.stopPropagation()}
                            >
                              <MoreHorizontal className="h-4 w-4" />
                            </Button>
                          }
                        />
                        <DropdownMenuContent align="end">
                          {d.status === "success" &&
                            !d.current &&
                            !d.serving && (
                              <DropdownMenuItem
                                disabled={
                                  deployInFlight || rollingBackId !== null
                                }
                                onClick={(e) => {
                                  e.stopPropagation();
                                  handleRollback(d);
                                }}
                              >
                                {rollingBackId === d.id ? (
                                  <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />
                                ) : (
                                  <RotateCcw className="mr-2 h-3.5 w-3.5" />
                                )}
                                Make current
                              </DropdownMenuItem>
                            )}
                          {d.status === "success" && canEdit && (
                            // Source of this version as deployed (sans node_modules,
                            // venvs, and other regenerable artifacts). Pruned
                            // releases 410 — surfaced by the browser, not a toast.
                            <DropdownMenuItem
                              render={
                                <a
                                  href={`/api/v1/apps/${appId}/deployments/${d.id}/archive`}
                                  // base-ui's `render` passes the item's own
                                  // children through to this anchor, so it does
                                  // have a label at runtime — the rule cannot
                                  // see that, and an anchor with no accessible
                                  // name is worth failing closed on anyway.
                                  aria-label="Download source ZIP"
                                  onClick={(e) => e.stopPropagation()}
                                />
                              }
                            >
                              <Download className="mr-2 h-3.5 w-3.5" /> Download
                              source ZIP
                            </DropdownMenuItem>
                          )}
                          {d.status === "success" && canEdit && (
                            // The release's SBOM (CycloneDX), from the inventory
                            // the build recorded. It says what it cannot vouch
                            // for rather than leaving it out.
                            <DropdownMenuItem
                              render={
                                <a
                                  href={`/api/v1/apps/${appId}/deployments/${d.id}/sbom`}
                                  aria-label="Download SBOM"
                                  onClick={(e) => e.stopPropagation()}
                                />
                              }
                            >
                              <Download className="mr-2 h-3.5 w-3.5" /> Download SBOM
                            </DropdownMenuItem>
                          )}
                          {canEdit && !d.protected_reason && (
                            <>
                              <DropdownMenuSeparator />
                              {/* `variant`, not a class on the label: the
                                  variant colours the icon and the focus state
                                  too, and the class coloured neither. */}
                              <DropdownMenuItem
                                variant="destructive"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  setPendingDelete(d);
                                }}
                              >
                                <Trash2 className="mr-2 h-3.5 w-3.5" /> Delete
                                this deployment…
                              </DropdownMenuItem>
                            </>
                          )}
                          {d.protected_reason && !d.current && (
                            <DropdownMenuItem disabled>
                              {d.protected_reason}
                            </DropdownMenuItem>
                          )}
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}

      <DeleteBuildDialog
        deployment={pendingDelete}
        onOpenChange={(open) => {
          if (!open) setPendingDelete(null);
        }}
        onConfirm={handleDelete}
      />
    </div>
  );
}

/**
 * Confirms deleting one build. A named single row does not need type-to-confirm
 * — the dialog says which commit and how much, which is the information the
 * decision actually turns on — but it does need to say that the build can never
 * be made current again, because that is the part that is not obvious from
 * "delete".
 */
function DeleteBuildDialog({
  deployment,
  onOpenChange,
  onConfirm,
}: {
  deployment: Deployment | null;
  onOpenChange: (open: boolean) => void;
  onConfirm: (d: Deployment) => Promise<void>;
}) {
  const [deleting, setDeleting] = useState(false);
  const freed =
    (deployment?.disk_bytes ?? 0) + (deployment?.artifact_bytes ?? 0);

  async function confirm() {
    if (!deployment) return;
    setDeleting(true);
    try {
      await onConfirm(deployment);
      onOpenChange(false);
    } catch {
      // The handler has already reported it; leave the dialog open to retry.
    } finally {
      setDeleting(false);
    }
  }

  return (
    <Dialog open={deployment !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {deployment?.commit_sha ? (
              <>
                Delete deployment{" "}
                <span className="font-mono text-sm">
                  {deployment.commit_sha.slice(0, 7)}
                </span>
                ?
              </>
            ) : (
              "Delete this deployment?"
            )}
          </DialogTitle>
          <DialogDescription>
            This deletes the deployment and its build log. It can’t be made
            current again.
            {freed > 0 && <> It is holding {formatBytes(freed)}.</>}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button
            variant="outline"
            disabled={deleting}
            onClick={() => onOpenChange(false)}
          >
            Cancel
          </Button>
          <Button variant="destructive" disabled={deleting} onClick={confirm}>
            {deleting ? "Deleting…" : "Delete deployment"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** An uploaded archive: no commit, and the pseudo-branch `upload`. */
function isUpload(d: Deployment): boolean {
  return !d.commit_sha && d.branch === "upload";
}

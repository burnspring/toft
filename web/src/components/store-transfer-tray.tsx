import { useEffect, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { formatBytes } from "@/lib/bytes";
import { storeUploads } from "@/lib/store-actions";
import { type Transfer } from "@/lib/store-transfer";

/** The leaf, for a row: the full name is the object's, and a person watching a
 * folder upload wants the file rather than the path they chose. */
function leafOf(name: string): string {
  const at = name.lastIndexOf("/");
  return at === -1 ? name : name.slice(at + 1);
}

function phaseLabel(t: Transfer): string {
  switch (t.phase) {
    case "queued":
      return "Waiting";
    case "checking":
      // Named rather than shown as a bar at zero: hashing a large file takes
      // minutes, and a bar that does not move is a page that looks broken.
      return "Checking";
    case "uploading":
      return t.rate > 0 ? `${formatBytes(t.rate)}/s` : "Uploading";
    case "done":
      return "Uploaded";
    case "failed":
      return "Failed";
    case "cancelled":
      return "Cancelled";
  }
}

/**
 * Where a transfer is described, and the only place it is.
 *
 * The table shows nothing until the object exists, because a row for a file
 * that is still being read is a row for something that is not there — and if
 * the upload fails, it never was.
 */
export function StoreTransferTray() {
  const transfers = useSyncExternalStore(storeUploads.subscribe, storeUploads.getSnapshot);
  const queryClient = useQueryClient();

  // Every completed transfer toasts, and every upload's success invalidates the
  // listing of the folder it landed in — that folder and no other, because a
  // person browsing elsewhere did not ask for their page to move.
  useEffect(() => {
    for (const t of transfers) {
      if (t.phase !== "done" && t.phase !== "failed" && t.phase !== "cancelled") continue;
      // The queue owns this, not a ref: Layout remounts on `/` ↔ anything.
      if (!storeUploads.announce(t.id)) continue;

      if (t.phase === "done") {
        toast.success(`Uploaded “${leafOf(t.name)}”`);
        queryClient.invalidateQueries({ queryKey: ["store-objects", t.storeSlug, t.path] });
      } else if (t.phase === "failed") {
        toast.error(`Couldn’t upload “${leafOf(t.name)}”`, { description: t.error });
      }
    }
  }, [transfers, queryClient]);

  // Armed only while something is actually running. A tab that always asks
  // "leave site?" trains people to click through the one time it mattered.
  const running = transfers.some(
    (t) => t.phase === "queued" || t.phase === "checking" || t.phase === "uploading",
  );
  useEffect(() => {
    if (!running) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [running]);

  if (transfers.length === 0) return null;

  return (
    <div
      // **Bottom left, and the toaster keeps the right.** Both were in the
      // same corner, so an upload's own success toasts landed on top of the
      // tray reporting it — the persistent record covered by the transient
      // notice about the same act, at exactly the moment somebody is watching
      // a four-gigabyte upload. The tray is the thing that stays, so it is the
      // thing that moves: a toast is gone in four seconds and a corner it has
      // to share is a corner it obscures.
      className="fixed bottom-4 left-4 z-50 w-[min(24rem,calc(100vw-2rem))] rounded-lg border border-border/60 bg-card shadow-lg"
      aria-label="Uploads"
    >
      <div className="flex items-center justify-between border-b border-border/60 px-3 py-2">
        <h2 className="text-sm font-semibold text-foreground">
          Uploads
          {running && <span className="ml-2 font-normal text-muted-foreground">in progress</span>}
        </h2>
        <Button variant="ghost" size="sm" onClick={() => storeUploads.dismissSettled()}>
          Clear finished
        </Button>
      </div>

      <ul className="max-h-64 divide-y divide-border/60 overflow-y-auto">
        {transfers.map((t) => {
          const pct = t.size > 0 ? Math.min(100, Math.round((t.transferred / t.size) * 100)) : 100;
          const active = t.phase === "checking" || t.phase === "uploading" || t.phase === "queued";
          return (
            <li key={t.id} className="px-3 py-2">
              <div className="flex items-center gap-2">
                <span className="min-w-0 flex-1 truncate text-sm text-foreground" title={t.name}>
                  {leafOf(t.name)}
                </span>
                <span className="shrink-0 text-2xs text-muted-foreground">{phaseLabel(t)}</span>
                {active && (
                  <button
                    type="button"
                    onClick={() => storeUploads.cancel(t.id)}
                    aria-label={`Cancel upload of ${leafOf(t.name)}`}
                    className="shrink-0 rounded p-0.5 text-muted-foreground hover:text-foreground"
                  >
                    <X className="h-3.5 w-3.5" />
                  </button>
                )}
              </div>

              {active && (
                <div
                  className="mt-1.5 h-1 w-full overflow-hidden rounded-full bg-muted"
                  role="progressbar"
                  aria-valuenow={pct}
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-label={`${leafOf(t.name)} ${phaseLabel(t)}`}
                >
                  <div
                    className={
                      t.phase === "uploading" ? "h-full bg-primary" : "h-full bg-muted-foreground/40"
                    }
                    style={{ width: `${pct}%` }}
                  />
                </div>
              )}

              {t.phase === "failed" && (t.error || storeUploads.canRetry(t.id)) && (
                <div className="mt-1 flex items-start gap-2">
                  {t.error && <p className="min-w-0 flex-1 text-2xs text-tone-danger-fg">{t.error}</p>}
                  {/* Not after a 412: something else wrote that name first,
                      and sending the same file again meets the same answer. */}
                  {t.failure !== "exists" && storeUploads.canRetry(t.id) && (
                    <Button
                      variant="outline"
                      size="xs"
                      className="ml-auto shrink-0"
                      onClick={() => storeUploads.retry(t.id)}
                      aria-label={`Retry upload of ${leafOf(t.name)}`}
                    >
                      Retry
                    </Button>
                  )}
                </div>
              )}
            </li>
          );
        })}
      </ul>

      {running && (
        <p className="border-t border-border/60 px-3 py-2 text-2xs text-muted-foreground">
          Moving between pages is fine. Closing or reloading this tab cancels what is still running.
        </p>
      )}
    </div>
  );
}

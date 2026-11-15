import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api } from "@/lib/api";
import type { App } from "@/lib/api";
import { RESTART_IN_FLIGHT } from "@/pages/app/state";

/**
 * Starting, restarting and stopping an app — from wherever somebody reaches for
 * them.
 *
 * These lived inside the Settings tab, under a red **Danger zone** heading, at
 * the bottom of a page nearly three thousand pixels tall. Two things were wrong
 * with that and they pull in opposite directions.
 *
 * The first is the product's own risk table: stopping and restarting *your own*
 * app takes no confirmation, because it is reversible and the control beside it
 * is the undo. Filing them under "Danger zone" beside Transfer ownership and
 * Delete says the opposite, and a warning that cries wolf about a restart is a
 * warning nobody reads before deleting.
 *
 * The second is that the app *list* has offered Redeploy, Restart and Stop on
 * every row's ⋯ menu the whole time. So the estate view gave you the controls
 * and the app's own page hid them — the wrong way round, and the reason
 * Overview had no lifecycle control at all.
 *
 * One hook so the header and anything else reach the same mutations, with the
 * same words and the same invalidations.
 */
export function useAppLifecycle(appId: string, app: App | undefined) {
  const queryClient = useQueryClient();
  /** Whether this is a start rather than a bounce, which decides every verb. */
  const wasDown = app?.status === "stopped" || app?.status === "sleeping";

  function settled() {
    queryClient.invalidateQueries({ queryKey: ["app", appId] });
    queryClient.invalidateQueries({ queryKey: ["apps"] });
  }

  /**
   * Keyed on the app, and the key is load-bearing: the header's status pill
   * watches `["restart", appId]` with `useIsMutating` to report a start that is
   * still happening, and so does the un-applied-changes notice on Overview.
   */
  const restart = useMutation({
    mutationKey: ["restart", appId],
    mutationFn: () => api.restart(appId),
    onSuccess: (res) => {
      settled();
      // 202: the start outlived the server's hold and is still running. Saying
      // "App restarted" here would be a claim nobody checked — an isolated
      // start is a pod schedule and an image pull, and the status pill is what
      // reports the outcome when it arrives.
      if (res.status === "restarting") {
        toast.info(res.detail ?? RESTART_IN_FLIGHT, { duration: 8000 });
        return;
      }
      toast.success(wasDown ? "App started" : "App restarted");
    },
    onError: (err: Error) => toast.error(err.message || "Couldn’t restart app"),
  });

  const stop = useMutation({
    mutationKey: ["stop", appId],
    mutationFn: () => api.stop(appId),
    onSuccess: () => {
      settled();
      toast.success("App stopped");
    },
    onError: (err: Error) => toast.error(err.message || "Couldn’t stop app"),
  });

  return {
    wasDown,
    restart: () => void restart.mutateAsync().catch(() => {}),
    stop: () => void stop.mutateAsync().catch(() => {}),
    restarting: restart.isPending,
    stopping: stop.isPending,
    /**
     * A locked app is the one app whose own people may not start it — which is
     * the whole reason the lock is a column and not the `stopped` status. The
     * reason has to be readable text somewhere, never a `title` on the disabled
     * control: a disabled element is not focusable and swallows mouse events,
     * so that explanation is unreachable by every route.
     */
    locked: !!app?.locked_at,
  };
}

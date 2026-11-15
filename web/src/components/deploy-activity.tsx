import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { AppIcon } from "@/components/app-icon";
import { api } from "@/lib/api";
import type { AppWithMetrics } from "@/lib/api";
import { DEPLOY_IN_PROGRESS } from "@/pages/app/state";

/** The app *statuses* that mean a deploy is under way. */
const IN_FLIGHT = new Set(["building", "deploying", "pending"]);

/**
 * Whether this app has a deploy in flight: its own status says so, or its
 * unfinished build does — `deploying` carries the build's status, from the same
 * set the app page reads, while `status` keeps describing the workload.
 */
function deployInFlight(a: AppWithMetrics): boolean {
  return IN_FLIGHT.has(a.status) || (!!a.deploying && DEPLOY_IN_PROGRESS.has(a.deploying));
}

/**
 * Deploys happening right now, anywhere on the install.
 *
 * The gap this closes: start a deploy, go back to the apps list, and the only
 * trace was a badge in one row of a table you might have filtered away. Watch
 * it from the app's own page and you were pinned there. Deploying is the one
 * thing in this product that takes minutes and that people wait on, and it was
 * the one thing with no presence outside the page that started it.
 *
 * **Absent when there is nothing happening.** Not a greyed-out icon — no
 * button at all, which is how every optional thing in this product behaves.
 * A permanent control that says "0" is a control that has to be read before it
 * can be ignored.
 *
 * It costs nothing extra: the same `["apps"]` query the dashboard, the layout
 * and the command palette already share, at the polling interval that query
 * already sets — which speeds itself up while any deploy is in flight, so this
 * is live for the same reason the list is.
 */
export function DeployActivity() {
  const { data: apps = [] } = useQuery<AppWithMetrics[]>({
    queryKey: ["apps"],
    queryFn: () => api.listApps(),
    refetchInterval: (query) =>
      (query.state.data ?? []).some(deployInFlight) ? 3000 : 15000,
  });

  const deploying = apps.filter(deployInFlight);
  if (deploying.length === 0) return null;

  return (
    <Popover>
      <PopoverTrigger
        className="flex items-center gap-1.5 rounded-md border border-primary/25 bg-primary/5 px-2 py-1.5 text-xs font-medium text-primary transition-colors hover:bg-primary/10"
        aria-label={`${deploying.length} deploy${deploying.length === 1 ? "" : "s"} in progress`}
      >
        <Loader2 className="h-3.5 w-3.5 motion-safe:animate-spin" />
        <span>{deploying.length}</span>
        <span className="hidden sm:inline">deploying</span>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-72 p-2">
        <p className="px-2 pt-1 pb-2 text-xs font-medium text-muted-foreground">
          Deploying now
        </p>
        <ul className="space-y-0.5">
          {deploying.map((app) => (
            <li key={app.id}>
              {/* The Deployments tab for whoever may open it, the app itself
                  for a viewer — who can see that it is deploying and cannot
                  read the build. */}
              <Link
                to={app.can_edit ? `/apps/${app.id}/deployments` : `/apps/${app.id}`}
                className="flex items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted"
              >
                <AppIcon name={app.name} seed={app.slug} size="sm" />
                {/* No elapsed counter: the list carries no deploy start time,
                    and `updated_at` moves on unrelated writes, so the figure it
                    gave could read "3s" four minutes into a build. */}
                <span className="min-w-0 flex-1 truncate text-sm font-medium">{app.name}</span>
              </Link>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}

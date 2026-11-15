import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronUp, ExternalLink, RotateCcw, ScrollText } from "lucide-react";

import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { Disclosure } from "@/components/disclosure";
import { DeployTimeline, PhaseBar } from "@/components/deploy-timeline";
import { LogPane } from "@/components/log-pane";
import { useBuildLog } from "@/hooks/use-build-log";
import { useDeployProgress } from "@/hooks/use-deploy-progress";
import { deployTimeline, formatStepMs, type DeployExpectation } from "@/lib/deploy-steps";
import { appUrl } from "@/lib/app-url";
import { formatDuration, formatElapsed } from "@/lib/time";
import { cn } from "@/lib/utils";
import { DEPLOY_IN_PROGRESS } from "@/pages/app/state";
import { buildLogFileUrl, type Deployment } from "@/lib/api";
import { stripLogNoise } from "@/lib/ansi";
import { firstErrorLine, lineHash } from "@/lib/log-view";

/**
 * A deploy, while it is happening, on the page the person is already on.
 *
 * This was a banner holding three phase rows and up to nine step rows, a
 * spinner on whichever one was running, and not one byte the build printed. To
 * reach those you went to another tab, found the row, clicked it, and read the
 * output in a modal over a socket that closed when the modal did.
 *
 * The phases were not wrong; they were in the live position. A person waiting on
 * their own code is asking two things at once — *is it still moving*, and *what
 * is it saying* — and only the second has an answer that cannot be faked. A
 * spinner on **Building** says the same thing at second five and at minute six.
 * The build's own output is the only progress indicator in the system that
 * cannot lie, because it *is* the work. So the log is the body, the phases are
 * one bar, and the twelve steps are a disclosure that opens on the one occasion
 * the step that stopped is the answer rather than the detail (ADR §87).
 *
 * Three rules about the log itself, and they are the design rather than its
 * polish:
 *
 * - **Every reader starts at line one.** A build's log lives in the builder's
 *   memory while it runs and in its row once it ends, and the socket's first
 *   message is whichever holds it — so a page that connects at minute four
 *   still shows minute one, and there is no "joined part-way" to explain.
 * - **The terminal frame changes the heading, not the contents.** Nothing
 *   refetches when the build ends: a person watching a build fail is reading the
 *   last twenty lines at the moment it fails, and replacing them with a fetch
 *   takes the evidence away at the instant it becomes evidence. The stored log
 *   is offered, never substituted.
 * - **One socket per build per page.** `useBuildLog` is shared with the Logs
 *   tab's Build source, so the two panes cannot disagree about where they
 *   joined.
 */
export function DeployConsole({
  appId,
  appSlug,
  deployment,
  onRedeploy,
  redeploying,
  expected,
}: {
  appId: string;
  appSlug: string;
  /** The build being watched. Absent for the moment before its row lands. */
  deployment?: Deployment;
  onRedeploy?: () => void;
  redeploying?: boolean;
  /** What the app's last successful deploy took — `expectedDeploy`. */
  expected?: DeployExpectation | null;
}) {
  const queryClient = useQueryClient();
  const [, tick] = useState(0);
  /**
   * Whether the pane is hidden — **derived, with the reader's choice on top**.
   *
   * Success collapses it, because the handoff line is the whole of what is left
   * to say; a failure keeps every line on screen, which is the point of it being
   * there. That is a function of the outcome rather than a thing to remember, so
   * it is computed, and `override` holds the one case where somebody disagreed.
   */
  const [override, setOverride] = useState<boolean | null>(null);

  /**
   * The bar's granularity, and the only per-deployment subscription this page
   * makes. The list poll underneath is unchanged and stays the list's source.
   */
  const frame = useDeployProgress(appId, deployment?.id, () => {
    queryClient.invalidateQueries({ queryKey: ["deployments", appId] });
    queryClient.invalidateQueries({ queryKey: ["app", appId] });
  });

  /**
   * The row, moved forward by the stream.
   *
   * A row that has already settled wins outright: the poll's terminal answer
   * carries the timings and the finish time, and a frame can only be older than
   * that. While the build runs the frame is the newer of the two.
   */
  const live = useMemo<Deployment | undefined>(() => {
    if (!deployment) return undefined;
    if (!DEPLOY_IN_PROGRESS.has(deployment.status)) return deployment;
    if (!frame || frame.deployment_id !== deployment.id) return deployment;
    return {
      ...deployment,
      status: frame.status,
      current_step: frame.step,
      step_started_at: frame.at,
    };
  }, [deployment, frame]);

  // No row yet is still deploying: the console is only on screen because the
  // page already believes a deploy is happening, and "Deployed" for the round
  // trip before the row lands would be a claim about an outcome nobody has.
  const running = !live || DEPLOY_IN_PROGRESS.has(live.status);
  const failed = live?.status === "failed";
  const settled = !!live && !running;
  const collapsed = override ?? live?.status === "success";

  const log = useBuildLog(appId, deployment?.id);

  // One interval, for the one counter that is live, and only while it is.
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, [running]);

  const lines = useMemo(() => log.lines.map((line) => ({ line })), [log.lines]);
  const failedStep = failed && live ? deployTimeline(live)?.find((r) => r.state === "failed") : undefined;
  // On a failure the reader opens at the first line that says so — the Logs
  // tab reads the fragment, unfolds that step and scrolls to it.
  const errorAt = failed ? firstErrorLine(log.lines.map(stripLogNoise)) : -1;
  const buildLogHref =
    (deployment
      ? `/apps/${appId}/logs?source=build&build=${deployment.id}`
      : `/apps/${appId}/logs?source=build`) + (errorAt >= 0 ? lineHash(errorAt) : "");

  return (
    <div
      className={cn(
        "space-y-3 rounded-lg border px-4 py-3",
        running && "border-tone-info-border bg-tone-info-surface",
        failed && "border-tone-danger-border bg-tone-danger-surface",
        settled && !failed && "border-tone-ok-border bg-tone-ok-surface",
      )}
    >
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0">
          <p className="text-sm font-medium text-foreground">
            {running
              ? "Deploying"
              : failed
                ? "This deploy failed"
                : "Deployed"}
            {live?.branch ? (
              <span className="font-normal text-muted-foreground"> — {live.branch}</span>
            ) : null}
          </p>
          <p className="text-xs text-muted-foreground">
            {running ? (
              live?.started_at ? (
                <>
                  Deploying for {formatElapsed(live.started_at)}
                  {expected ? (
                    <>
                      {" "}— the last one took {formatStepMs(expected.totalMs)}
                      {expected.slowest ? <>, most of it {expected.slowest.label.toLowerCase()}</> : null}
                    </>
                  ) : null}
                  . The app keeps serving meanwhile.
                </>
              ) : (
                "The app keeps serving meanwhile."
              )
            ) : failed ? (
              // What the *console* is for is where it died and what it printed.
              // Why that matters — the app is still up on the previous release,
              // and your new code is not live — is the banner below, which is
              // the one that meets somebody who was not watching.
              <>
                {failedStep ? (
                  <>
                    It got as far as{" "}
                    <strong className="font-medium">{failedStep.label}</strong>.
                    The output below is where it stopped.
                  </>
                ) : (
                  "The output below is where it stopped."
                )}
              </>
            ) : (
              <>
                Live in {live ? formatDuration(live.started_at, live.finished_at) : "—"}
                {live?.commit_sha ? (
                  <> · <span className="font-mono">{live.commit_sha.slice(0, 7)}</span></>
                ) : null}
              </>
            )}
          </p>
        </div>

        {/* Nothing here navigates on its own. The console's job at the end is to
            say which log matters now, not to move somebody who may be reading. */}
        <div className="flex shrink-0 flex-wrap items-center gap-1">
          {settled && !failed && (
            <>
              {/* A real anchor, like the identical control in the header: as a
                  button it could not be middle-clicked into a background tab,
                  ⌘-clicked, or right-clicked for its address. */}
              <a
                href={appUrl(appSlug)}
                target="_blank"
                rel="noopener noreferrer"
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                <ExternalLink className="mr-1.5 h-3.5 w-3.5" />
                Open app
              </a>
              <Link
                to={`/apps/${appId}/logs`}
                className={buttonVariants({ variant: "ghost", size: "sm" })}
              >
                <ScrollText className="mr-1.5 h-3.5 w-3.5" />
                Run log
              </Link>
            </>
          )}
          {failed && (
            <>
              <Link
                to={buildLogHref}
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                <ScrollText className="mr-1.5 h-3.5 w-3.5" />
                {errorAt >= 0 ? "Open full log at the error" : "Open full log"}
              </Link>
              {onRedeploy && (
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={redeploying}
                  onClick={onRedeploy}
                >
                  <RotateCcw className="mr-1.5 h-3.5 w-3.5" />
                  {redeploying ? "Redeploying…" : "Redeploy"}
                </Button>
              )}
            </>
          )}
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={!collapsed}
            onClick={() => setOverride(!collapsed)}
          >
            {collapsed ? (
              <ChevronDown className="mr-1.5 h-3.5 w-3.5" />
            ) : (
              <ChevronUp className="mr-1.5 h-3.5 w-3.5" />
            )}
            {collapsed ? "Show output" : "Hide output"}
          </Button>
        </div>
      </div>

      {live && <PhaseBar deployment={live} />}

      {/* The twelve steps, and the durations, which are what somebody asks
          afterwards. Keyed on the outcome so a build that fails while this is
          on screen opens it: the step that stopped is the answer at exactly
          that moment, and the detail at every other. */}
      {live && deployTimeline(live) && (
        <Disclosure key={failed ? "failed" : "running"} label="Steps" size="xs" defaultOpen={failed}>
          <DeployTimeline deployment={live} className="mt-2 max-w-md" />
        </Disclosure>
      )}

      {!collapsed && (
        <LogPane
          lines={lines}
          label="Build output"
          subject="build log"
          paneClassName="max-h-64"
          stream={running ? log.state : "none"}
          onReconnect={log.reconnect}
          emptyMessage={
            running
              ? "Waiting for the first line of this build…"
              : "This page received no output for this build."
          }
          downloadName={(narrowed) =>
            `${appSlug}-build-${(deployment?.commit_sha || deployment?.id || "").slice(0, 7)}${narrowed ? "-filtered" : ""}.log`
          }
          downloadHref={deployment ? buildLogFileUrl(appId, deployment.id) : undefined}
          // The way to the reader is a control beside search, not a link in
          // the footnote: this pane is a window onto the end of the log, and
          // the whole of it — full height, line links, its steps — is one
          // click away. On a failure the header already carries it.
          controls={
            failed ? undefined : (
              <Link
                to={buildLogHref}
                className={buttonVariants({ variant: "outline", size: "xs" })}
              >
                <ScrollText className="mr-1 h-3 w-3" />
                Open full log
              </Link>
            )
          }
          footnote={
            running ? <>From the first line of this build.</> : <>The whole build.</>
          }
        />
      )}
    </div>
  );
}

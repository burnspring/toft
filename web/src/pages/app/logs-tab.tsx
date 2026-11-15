import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { GitCommit } from "lucide-react";

import { LogPane, type LogPaneLine } from "@/components/log-pane";
import { EmptyState } from "@/components/empty-state";
import { FilterChips } from "@/components/filter-bar";
import { QueryError } from "@/components/query-error";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { useBuildLog } from "@/hooks/use-build-log";
import { api, buildLogFileUrl, buildLogRawUrl, runLogFileUrl, runLogRawUrl, wsUrl } from "@/lib/api";
import {
  defaultLogSource,
  pickBuild,
  readLogSource,
  type LogSource,
} from "@/lib/log-source";
import { formatTimestamp } from "@/lib/time";
import { DEPLOY_IN_PROGRESS } from "@/pages/app/state";
import type { Deployment, LogAbsenceReason, WorkloadLogLine } from "@/lib/api";

// ── Logs Tab ──────────────────────────────────────────────────

/**
 * An app has two logs, and until now nothing in the product said which was
 * which.
 *
 * The run log is *this release's* output and goes with the workload. The build
 * log is the deploy that made the release and is kept with the build. They
 * lived on different tabs — one of them behind a modal with no address of its
 * own — and nothing on either surface named the other, so "which log do I want"
 * was a thing every person worked out once, alone, usually while something was
 * broken.
 *
 * A switch rather than a second tab, for the same reason the App/Platform
 * filter below is not one: this is a **time** seam, not a subject one, and the
 * tab bar is where subjects go. And the product chooses the side rather than
 * asking — build while a deploy is in flight or the last one failed, run
 * otherwise (ADR §87). That default is the whole *when do I use what* question,
 * answered from evidence the page already holds instead of by somebody who has
 * to learn the difference first.
 */
export function LogsTab({
  appId,
  appSlug,
  deployments,
  deploymentsLoading,
}: {
  appId: string;
  appSlug: string;
  deployments: Deployment[];
  deploymentsLoading: boolean;
}) {
  const [params, setParams] = useSearchParams();

  const building = deployments.some((d) => DEPLOY_IN_PROGRESS.has(d.status));
  const serving =
    deployments.find((d) => d.serving) ?? deployments.find((d) => d.current);
  const latest = deployments[0];
  // The app that looks healthy and is running last week's code: a failed deploy
  // leaves the previous release serving, so nothing else on the page is red.
  const lastFailed =
    latest?.status === "failed" && (!serving || serving.id !== latest.id);

  const source = readLogSource(
    params.get("source"),
    defaultLogSource({ building, lastFailed }),
  );
  const build = pickBuild(deployments, params.get("build"));

  /**
   * Both choices are in the address, which is what makes a build log a thing
   * you can send to somebody. Replaced rather than pushed: flipping between two
   * views of the same app's output is not five steps of history, and the link
   * that arrives from the Deployments tab is a real navigation made there.
   */
  function choose(next: { source?: LogSource; build?: string }) {
    const updated = new URLSearchParams(params);
    if (next.source) updated.set("source", next.source);
    if (next.build) updated.set("build", next.build);
    setParams(updated, { replace: true });
  }

  return (
    <div className="space-y-3 pt-4">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        {/* A scope, not a filter — so the segmented control the apps list's
            Mine/All uses, not chips. */}
        <div
          className="inline-flex shrink-0 items-center gap-0.5 rounded-md border border-border/60 bg-muted/50 p-0.5"
          role="group"
          aria-label="Which log"
        >
          {(
            [
              { value: "run", label: "Run log" },
              { value: "build", label: "Build log" },
            ] as const
          ).map((s) => (
            <button
              key={s.value}
              type="button"
              onClick={() => choose({ source: s.value })}
              aria-pressed={source === s.value}
              className={`rounded px-2.5 py-1 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring ${
                source === s.value
                  ? "bg-card text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {s.label}
            </button>
          ))}
        </div>
      </div>

      {source === "run" ? (
        <RunLog appId={appId} appSlug={appSlug} />
      ) : (
        <BuildLog
          appId={appId}
          appSlug={appSlug}
          build={build}
          deployments={deployments}
          loading={deploymentsLoading}
          onPick={(id) => choose({ source: "build", build: id })}
        />
      )}
    </div>
  );
}

// ── The run log ───────────────────────────────────────────────

/**
 * Why the pane is empty, said as the situation it actually is.
 *
 * One sentence used to cover all of these — "No log output yet." — and one of
 * the things it covered was an app that is running perfectly and has printed
 * nothing. A log view that reports "no output" for a workload that does not
 * exist is a view that teaches people to distrust it.
 */
const LOG_ABSENCE_COPY: Record<LogAbsenceReason, string> = {
  never_deployed: "This app has never been deployed.",
  stopped: "This app is stopped. Its log resumes when it starts again.",
  sleeping: "This app is asleep. The next request wakes it.",
  locked: "An administrator has locked this app. Its earlier output isn’t kept.",
  launching: "This app is starting and hasn’t printed anything yet.",
  no_workload: "Nothing is running for this deployment.",
};

/**
 * The source filter, over a voice rather than over a backend.
 *
 * It used to render in isolated mode only, and correctly: shared mode had no
 * second speaker, so the filter was furniture there. Now that the platform
 * keeps its own account of every app in both modes, both have two voices and
 * the axis is the same one in each — which is why the split stayed a filter
 * rather than becoming a second tab. A tab per source would have put the
 * runtime seam in the tab bar, and the seam is not what a reader is asking
 * about.
 */
const LOG_SOURCE_FILTERS: { value: "all" | "app" | "platform"; label: string }[] = [
  { value: "all", label: "All" },
  { value: "app", label: "App" },
  { value: "platform", label: "Platform" },
];

/**
 * Which voice a line is in. Platform outranks stderr: a runner line on stderr
 * is still the platform speaking, and colouring it as the app's error would put
 * Toft's own diagnostics in the app's voice.
 */
function logLineTint(l: WorkloadLogLine): LogPaneLine["tint"] {
  if (l.source === "platform") return "platform";
  return l.stream === "stderr" ? "stderr" : undefined;
}

/**
 * How many streamed lines the pane keeps.
 *
 * A log stream has no end, and a tab left open on a chatty app grew an array
 * and a DOM node per line until the browser gave up — which happens hours in,
 * to somebody who left the tab open precisely because they were waiting for
 * something. The oldest go, because the reason the pane is open is the newest,
 * and the pane says so rather than quietly losing them.
 */
/** The endpoint's own ceiling, and the most this pane will ask for. */
const MAX_LOG_TAIL = 5000;

/**
 * The Logs tab is the reader, so its pane takes the screen: what is left of the
 * viewport below the app's header, the tab strip and the pane's own toolbar,
 * and never less than a useful window on a short one. The 600px it had was the
 * console's idea of a log, on the one surface whose subject is the whole thing
 * (ADR §87).
 */
const READER_HEIGHT = "max-h-[calc(100dvh-18rem)] min-h-64";

const MAX_STREAMED_LOG_LINES = 2000;

function RunLog({ appId, appSlug }: { appId: string; appSlug: string }) {
  const [sourceFilter, setSourceFilter] = useState<"all" | "app" | "platform">("all");
  const [streamedLines, setStreamedLines] = useState<WorkloadLogLine[]>([]);
  /** Whether the pane is trimming — the sentence below is drawn from it. */
  const [dropped, setDropped] = useState(false);
  /**
   * The socket, as the pane reports it.
   *
   * A stream that closes — the workload stopped, the backend went away, a proxy
   * timed out — used to close silently, and the pane went on looking exactly
   * like a live one that had nothing to say. Somebody watching for a line that
   * would never arrive had no way to tell the difference, which is the worst
   * thing a log view can do.
   */
  const [stream, setStream] = useState<"connecting" | "live" | "closed">("connecting");
  /** Bumped by Reconnect, which is the effect's other dependency. */
  const [reconnects, setReconnects] = useState(0);

  // Fetch initial logs. The error is read rather than discarded: this query
  // answered 500 for every stopped isolated app and the tab rendered it as
  // "No log output yet.", which is how the disagreement between the two
  // backends stayed invisible for as long as both existed.
  /**
   * How much history to ask for.
   *
   * It was 200 and unreachable — the endpoint has always served up to 5,000,
   * and there was no control and no sentence saying the tail had been cut. An
   * app that logs a line a second fills 200 in three minutes, which is less
   * than the time between noticing a problem and opening this tab.
   */
  const [tail, setTail] = useState(200);

  const {
    data: logsData,
    isLoading: logsLoading,
    isFetching: logsFetching,
    error: logsError,
    refetch: refetchTail,
  } = useQuery({
    queryKey: ["logs", appId, tail],
    queryFn: () => api.getLogs(appId, tail),
    // A longer tail keeps the shorter one on screen until it lands, rather
    // than the history vanishing for the round trip.
    placeholderData: keepPreviousData,
  });
  // A full tail is the signal there is more behind it. The server answers a
  // count, not "and there was more", so this is the honest half: it says the
  // *oldest* line on screen is where the request stopped, never that history
  // exists beyond it.
  const tailFull = (logsData?.lines?.length ?? 0) >= tail;

  // WebSocket for live streaming
  useEffect(() => {
    const ws = new WebSocket(wsUrl(`/apps/${appId}/logs/stream`));
    ws.onopen = () => setStream("live");
    ws.onmessage = (event) => {
      // One JSON object per message. A frame that will not parse is dropped
      // rather than rendered: this socket carries the backend's own shape, and
      // printing a malformed one into the pane would put transport noise where
      // the app's output goes.
      try {
        const line = JSON.parse(String(event.data)) as WorkloadLogLine;
        setStreamedLines((prev) => {
          const next = [...prev, line];
          if (next.length <= MAX_STREAMED_LOG_LINES) return next;
          setDropped(true);
          return next.slice(-MAX_STREAMED_LOG_LINES);
        });
      } catch {
        // Nothing to show for a frame we cannot read.
      }
    };
    ws.onerror = () => setStream("closed");
    ws.onclose = () => setStream("closed");
    return () => {
      // Detached first: closing on unmount is not the socket going away under
      // somebody, and reporting it as one would flash "not live" on the way out.
      ws.onclose = null;
      ws.onerror = null;
      ws.close();
    };
  }, [appId, reconnects]);

  const all = useMemo(
    () => [...(logsData?.lines ?? []), ...streamedLines],
    [logsData, streamedLines],
  );
  const sourceCounts = useMemo(() => {
    const c = { all: all.length, app: 0, platform: 0 };
    for (const l of all) {
      if (l.source === "app") c.app++;
      else if (l.source === "platform") c.platform++;
    }
    return c;
  }, [all]);
  const lines = useMemo<LogPaneLine[]>(
    () =>
      all
        .filter((l) => sourceFilter === "all" || l.source === sourceFilter)
        .map((l) => ({ line: l.line, at: l.at, tint: logLineTint(l) })),
    [all, sourceFilter],
  );

  if (logsError) {
    return (
      <QueryError
        title="This app’s log could not be read"
        error={logsError}
        fallback="Only the app’s owner and editors can see its log."
        onRetry={() => void refetchTail()}
      />
    );
  }

  return (
    <LogPane
      lines={lines}
      label="Run log"
      subject="log"
      paneClassName={READER_HEIGHT}
      rawHref={runLogRawUrl(appId)}
      stream={stream}
      narrowed={sourceFilter !== "all"}
      onReconnect={() => {
        // Said here rather than in the effect: the effect's job is the socket,
        // and a state reset inside it is a render the pane does not need on
        // every mount.
        setStream("connecting");
        // **And re-read the tail.** Resuming the socket picks up from *now*,
        // so whatever the app printed while the connection was down was gone
        // with no gap on screen to show for it — a log that silently drops
        // lines is worse than one that says it stopped. The streamed buffer is
        // cleared first because the re-read returns those lines too, and the
        // pane concatenates the two.
        setStreamedLines([]);
        setDropped(false);
        void refetchTail();
        setReconnects((n) => n + 1);
      }}
      emptyMessage={
        // The build log beside this one already distinguishes the read that
        // has not answered from the read that answered with nothing. This did
        // not, so the whole first round trip — the state a person meets on
        // every arrival — asserted that the app had printed nothing.
        logsLoading
          ? "Loading the log…"
          : (logsData?.reason && LOG_ABSENCE_COPY[logsData.reason]) ||
            "No log output yet."
      }
      downloadName={(narrowed) => `${appSlug}-logs${narrowed ? "-filtered" : ""}.txt`}
      downloadHref={runLogFileUrl(appId)}
      controls={
        <div className="flex items-center gap-3">
          <FilterChips
            label="Filter log by source"
            value={sourceFilter}
            onChange={(v) => setSourceFilter(v as typeof sourceFilter)}
            options={LOG_SOURCE_FILTERS.map((f) => ({
              ...f,
              count: sourceCounts[f.value],
            }))}
          />
          {/* Asking for more history, which had no control at all. Absent
              once the tail comes back short — there is nothing further back
              to ask for — and once it is at the endpoint's own ceiling. */}
          {tailFull && tail < MAX_LOG_TAIL && (
            <Button
              variant="outline"
              size="xs"
              disabled={logsFetching}
              onClick={() => {
                // The longer tail returns the lines streamed since the tab
                // opened too, and the pane concatenates the two — the same
                // reason Reconnect clears the buffer.
                setStreamedLines([]);
                setDropped(false);
                setTail((n) => Math.min(n * 5, MAX_LOG_TAIL));
              }}
            >
              {logsFetching ? "Loading…" : "Older lines"}
            </Button>
          )}
        </div>
      }
      footnote={
        <>
          {dropped
            ? `Showing the latest ${MAX_STREAMED_LOG_LINES.toLocaleString()} streamed lines. Older ones were dropped.`
            : "New lines appear as the app prints them."}{" "}
          {/* Said, rather than left to be inferred from where the scroll
              stops. The tail is a `LIMIT`, and a log that begins mid-sentence
              with nothing explaining why reads as the log itself being
              broken. */}
          {tailFull &&
            `Starts at the last ${tail.toLocaleString()} lines. `}
          {/* The two voices age out on different clocks, so scrolling back far
              enough thins one out while the other continues — which reads as a
              gap in the record unless it is said. The figure itself is an
              administrator's and is not repeated here: it belongs to the
              install, and an owner who cannot change it is owed the shape of
              the rule rather than its current value. */}
          Platform lines are kept longer than the app’s own output.
        </>
      }
    />
  );
}

// ── The build log ─────────────────────────────────────────────

/**
 * One build's output, at an address.
 *
 * A build log is the thing people send to someone else — a failing R restore is
 * four hundred lines of renv traceback naming a shared object, and the person
 * who can read it is not the person looking at the screen. Until this had a URL
 * the only way to say *look at this build* was to describe the clicks.
 *
 * A build in flight is read from the shared socket the console is already on,
 * never a second one; a build that is over is read from its row, which is where
 * the log is written — once, at the end.
 */
function BuildLog({
  appId,
  appSlug,
  build,
  deployments,
  loading,
  onPick,
}: {
  appId: string;
  appSlug: string;
  build?: Deployment;
  deployments: Deployment[];
  loading: boolean;
  onPick: (id: string) => void;
}) {
  const inFlight = !!build && DEPLOY_IN_PROGRESS.has(build.status);
  const streamed = useBuildLog(appId, inFlight ? build?.id : undefined);

  const {
    data: stored,
    error: storedError,
    isLoading: storedLoading,
    refetch: refetchStored,
  } = useQuery({
    queryKey: ["deployment-log", appId, build?.id],
    queryFn: () => api.getDeploymentLogs(appId, build!.id),
    enabled: !!build && !inFlight,
  });

  /**
   * The stored log when there is one, and what this page saw until then.
   *
   * A running build has nothing stored — the builder accumulates its log in
   * memory and writes the row once, at the end — so the socket is the only
   * copy. The moment the row is written it is the *complete* copy, and this is
   * the surface whose subject is the complete one.
   */
  const lines = useMemo<LogPaneLine[]>(() => {
    const text = stored?.build_log;
    if (!text) return streamed.lines.map((line, i) => ({ line, at: streamed.at[i] }));
    // Times pair with lines from the top; a line past the end has none (ADR §155).
    const t = stored?.line_times;
    return text.replace(/\n$/, "").split("\n").map((line, i) => ({
      line,
      at:
        t && i < t.offsets_ms.length
          ? new Date(t.start_ms + t.offsets_ms[i]).toISOString()
          : undefined,
    }));
  }, [stored, streamed.lines, streamed.at]);

  if (loading) return <Skeleton className="h-64 w-full" />;

  if (!build) {
    return (
      <EmptyState
        icon={<GitCommit className="h-6 w-6" />}
        title="No deployments yet"
        description="Redeploy, or upload a new version, to deploy this app."
      />
    );
  }

  if (storedError) {
    return (
      <QueryError
        title="This build’s log could not be read"
        error={storedError}
        fallback="This deployment may have been deleted, along with its build log."
        onRetry={() => void refetchStored()}
      />
    );
  }

  const short = build.commit_sha?.slice(0, 7) || build.id.slice(0, 8);

  return (
    <LogPane
      lines={lines}
      label="Build log"
      subject="build log"
      reader
      failed={build.status === "failed"}
      paneClassName={READER_HEIGHT}
      rawHref={buildLogRawUrl(appId, build.id)}
      stream={inFlight ? streamed.state : "none"}
      onReconnect={streamed.reconnect}
      emptyMessage={
        inFlight
          ? "Waiting for the first line of this build…"
          : storedLoading
            ? "Loading the build log…"
            : "This build recorded no log."
      }
      downloadName={(narrowed) =>
        `${appSlug}-build-${short}${narrowed ? "-filtered" : ""}.log`
      }
      downloadHref={buildLogFileUrl(appId, build.id)}
      controls={
        <Select
          value={build.id}
          onValueChange={(v) => onPick(String(v))}
        >
          {/* As wide as its label, not a fixed 15rem that cut it mid-word. */}
          <SelectTrigger size="sm" className="max-w-full min-w-[15rem]" aria-label="Which build">
            <SelectValue>
              {(v) => {
                const d = deployments.find((x) => x.id === v);
                return d ? buildLabel(d) : "This build";
              }}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {deployments.map((d) => (
              <SelectItem key={d.id} value={d.id}>
                {buildLabel(d)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      }
      footnote={
        inFlight ? (
          <>From the first line; it is saved when the build finishes.</>
        ) : (
          <>This log is deleted with its build.</>
        )
      }
    />
  );
}

/** One build, named the way somebody picking between them would name it. */
function buildLabel(d: Deployment): string {
  const short = d.commit_sha ? d.commit_sha.slice(0, 7) : "uploaded";
  const state = DEPLOY_IN_PROGRESS.has(d.status)
    ? "building"
    : d.status === "failed"
      ? "failed"
      : d.serving || d.current
        ? "serving"
        : "";
  return `${short} · ${formatTimestamp(d.started_at)}${state ? ` · ${state}` : ""}`;
}

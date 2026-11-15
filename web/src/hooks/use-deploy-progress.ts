import { useEffect, useRef, useState } from "react";

import { sseUrl, type DeployProgress } from "@/lib/api";

/**
 * One build's phase and step, as the server reports them.
 *
 * §40 built this stream for a command-line client and said the SPA would not
 * consume it, because the page already holds the deployment list and polls it.
 * That was right about the *list* and wrong about the console: a bar whose whole
 * job is to say "still moving" moving on a three-second poll is a bar that
 * spends most of its life lying by a second and a half. The list stays the
 * list's source; this is the one per-deployment question the page has, and it
 * costs the same connection the build log already opens (ADR §87).
 *
 * Every frame is a snapshot, so a watcher that connects late or misses one is
 * correct again on the next. The terminal frame is the end signal and this
 * closes on it — an `EventSource` reconnects on its own forever otherwise, and
 * the server closes the stream as soon as the deploy is over.
 */
export function useDeployProgress(
  appId: string | undefined,
  deployID: string | undefined,
  onTerminal?: () => void,
): DeployProgress | undefined {
  const [frame, setFrame] = useState<DeployProgress>();
  // Held rather than depended on: callers pass an inline arrow — a new identity
  // every render — and depending on it would tear down and re-open the stream on
  // every parent render. Updated in an effect rather than in render; its only
  // caller is a frame arriving over the network, which is long after the first
  // effect has run.
  const onTerminalRef = useRef(onTerminal);
  useEffect(() => {
    onTerminalRef.current = onTerminal;
  });

  useEffect(() => {
    if (!appId || !deployID) return;

    const es = new EventSource(
      sseUrl(`/apps/${appId}/deployments/${deployID}/progress`),
      { withCredentials: true },
    );
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      es.close();
      onTerminalRef.current?.();
    };
    es.addEventListener("progress", (event) => {
      // A frame that will not parse is dropped rather than drawn: this stream
      // carries the server's own shape, and half a frame is not a position.
      try {
        const next = JSON.parse(String((event as MessageEvent).data)) as DeployProgress;
        setFrame(next);
        if (next.status === "success" || next.status === "failed") finish();
      } catch {
        // Nothing to draw for a frame we cannot read.
      }
    });
    // A stream that drops is not a deploy that stopped, and nothing here is
    // load-bearing: the deployment list is still polling underneath. So an
    // error is left to `EventSource`'s own retry rather than reported.
    return () => {
      done = true;
      es.close();
    };
  }, [appId, deployID]);

  // The frame is keyed by the build it describes rather than cleared when the
  // subject changes: a snapshot of the previous deploy drawn under this one's
  // heading is the one wrong answer this stream can give, and discarding it here
  // is a comparison rather than a second render.
  return frame?.deployment_id === deployID ? frame : undefined;
}

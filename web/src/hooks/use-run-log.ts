import { useCallback, useEffect, useRef, useState } from "react";

/** One line of a job run’s log, as the log endpoint returns it. */
export interface RunLogLine {
  seq: number;
  at: string;
  stream: string;
  line: string;
}

/**
 * How many lines a run's pane holds, and why they are the *last* that many.
 *
 * The server answers a page at a time and never the whole log, so a pane has to
 * choose which end it keeps. It keeps the end: a run somebody has opened is
 * either still going, where the only interesting line is the newest one, or it
 * has just failed, where the traceback is at the bottom. Keeping the head
 * instead is what the old panes did by accident — they asked for the first page
 * once and never asked again — and on a chatty job that meant a live pane that
 * stopped moving at line 1,000 while the run went on printing.
 *
 * What is dropped is *said*, in the pane's footnote. A log silently missing its
 * beginning is worse than one that admits to it.
 */
export const RUN_LOG_CAP = 10000;

/** How often a run still in flight is asked what else it has printed. */
const POLL_MS = 3000;

export interface RunLog {
  lines: RunLogLine[];
  /** True until the first read of this run has come back, success or not. */
  loading: boolean;
  error: Error | null;
  /** Lines that fell off the front because the run printed more than the cap. */
  dropped: number;
  /** The run's status as the log endpoint last reported it, or null. */
  status: string | null;
  /** Start again from the top — for the retry on a failed read. */
  reload: () => void;
}

/**
 * A run's log, read forward and accumulated, for as long as it is being made.
 *
 * The log endpoint pages with `after`: they answer the lines *after* a
 * sequence number, capped by the server. So reading a log is a loop and not a
 * request — ask, keep what came back, ask again from the last sequence, and
 * stop when a page comes back empty. That last condition is deliberately not
 * "a page came back short": the server's page size is the server's business,
 * and a pane that knew the number would be wrong the day it changed.
 *
 * While the run is in flight the loop resumes on a timer from where it stopped,
 * so a poll carries the new lines rather than the whole log again.
 *
 * `fetchPage` is held in a ref and never depended on: callers pass an inline
 * arrow, and depending on it would restart the read on every parent render —
 * which on this surface means every ten seconds, when the run list refreshes.
 */
export function useRunLog(
  /** The run being read. Changing it starts over. */
  runID: string,
  fetchPage: (after: number) => Promise<{ lines: RunLogLine[]; status: string }>,
  /** Whether the run may still print anything. */
  live: boolean,
): RunLog {
  const fetchRef = useRef(fetchPage);
  useEffect(() => {
    fetchRef.current = fetchPage;
  });

  const [lines, setLines] = useState<RunLogLine[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const [dropped, setDropped] = useState(0);
  const [status, setStatus] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  // The accumulated log and the high-water mark live in refs, because they are
  // what the *next* request is built from rather than what the last render drew.
  const bufRef = useRef<RunLogLine[]>([]);
  const afterRef = useRef(0);
  const droppedRef = useRef(0);

  // Starting over is keyed on the run and the attempt and not on `live`: a run
  // that just finished has a last page to collect, and resetting there would
  // throw away everything already on screen to fetch it again. The rendered
  // state resets during render, so no frame draws the old run's lines under the
  // new one; the refs reset in the effect below, before it builds a request.
  const readKey = `${runID}\n${attempt}`;
  const [renderedKey, setRenderedKey] = useState(readKey);
  if (renderedKey !== readKey) {
    setRenderedKey(readKey);
    setLines([]);
    setDropped(0);
    setStatus(null);
    setError(null);
    setLoading(true);
  }
  const readingKeyRef = useRef(readKey);

  useEffect(() => {
    if (readingKeyRef.current !== readKey) {
      readingKeyRef.current = readKey;
      bufRef.current = [];
      afterRef.current = 0;
      droppedRef.current = 0;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function drain() {
      for (;;) {
        const page = await fetchRef.current(afterRef.current);
        if (cancelled) return;
        setStatus(page.status);
        if (page.lines.length === 0) return;

        const top = page.lines[page.lines.length - 1].seq;
        const merged = bufRef.current.concat(page.lines);
        let cut = 0;
        if (merged.length > RUN_LOG_CAP) {
          cut = merged.length - RUN_LOG_CAP;
          merged.splice(0, cut);
          droppedRef.current += cut;
        }
        bufRef.current = merged;
        setLines(merged);
        if (cut > 0) setDropped(droppedRef.current);

        // A server that answered without advancing the mark would loop here
        // forever, so the mark advancing is the condition for asking again.
        if (top <= afterRef.current) return;
        afterRef.current = top;
      }
    }

    async function tick() {
      try {
        await drain();
        if (!cancelled) setError(null);
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof Error ? err : new Error("Couldn’t read this run’s log"));
      }
      if (cancelled) return;
      setLoading(false);
      // A failed poll on a live run is retried rather than abandoned: the run is
      // still printing, and one refused read is not a reason to stop watching.
      if (live) timer = setTimeout(() => void tick(), POLL_MS);
    }

    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [readKey, live]);

  const reload = useCallback(() => setAttempt((n) => n + 1), []);

  return { lines, loading, error, dropped, status, reload };
}

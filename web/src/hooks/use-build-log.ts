import { useCallback, useSyncExternalStore } from "react";

import { wsUrl } from "@/lib/api";

/**
 * One build's log, followed once per page however many things are reading it.
 *
 * Two surfaces watch the same build now — the deploy console above the tabs and
 * the Logs tab's Build source — and two sockets on one build is not a
 * correctness problem, because the server broadcasts to every subscriber. It is
 * a problem because they would be two panes that **joined at different points**,
 * showing different amounts of the same build, with nothing on either saying
 * why. A build's log is written to its row once, at the end, so where a reader
 * joined is the only thing that decides what they can see; two answers to that
 * on one page is the confusion this whole design exists to prevent (ADR §87).
 * The server now hands every reader the build from its first line — the
 * builder keeps a live copy — but one socket per build is still one buffer and
 * one live state, whichever pane is reading.
 *
 * So the socket lives here, keyed by build, reference-counted, and the buffer
 * outlives the readers: walking from the console to the Logs tab keeps every
 * line the console had, which is the same rule from the other direction.
 */

/**
 * The last thing the server writes, and it is **framing rather than a line**.
 *
 * `builder.go` broadcasts it, the stream handler closes on it, and the stored
 * log does not contain it — so a pane that printed it showed a line that
 * vanished the next time the same log was opened. Only a message that is
 * exactly this is framing; a build that prints the word inside its own output
 * is printing output.
 */
export const BUILD_LOG_SENTINEL = "DONE";

/** How many builds' buffers are kept once nothing is reading them. */
const RETAINED_BUFFERS = 3;

export interface BuildLogSnapshot {
  /** Every line received on this page, the sentinel excluded. */
  lines: string[];
  /** When each of `lines` was written, index for index, as ISO strings —
   *  undefined for a line with no recorded time (ADR §155). */
  at: (string | undefined)[];
  /** The socket, as the pane reports it. */
  state: "connecting" | "live" | "closed";
  /** Whether the build said it was over on this connection. */
  ended: boolean;
}

interface Entry {
  url: string;
  refs: number;
  touched: number;
  socket: WebSocket | null;
  snapshot: BuildLogSnapshot;
  listeners: Set<() => void>;
}

const streams = new Map<string, Entry>();

const EMPTY: BuildLogSnapshot = {
  lines: [],
  at: [],
  state: "closed",
  ended: false,
};

function publish(entry: Entry, next: Partial<BuildLogSnapshot>) {
  entry.snapshot = { ...entry.snapshot, ...next };
  for (const listener of entry.listeners) listener();
}

function open(entry: Entry) {
  if (entry.socket || entry.snapshot.ended) return;
  const ws = new WebSocket(entry.url);
  entry.socket = ws;
  publish(entry, { state: "connecting" });
  ws.onopen = () => publish(entry, { state: "live" });
  // Every connection's first message is the whole log so far. The buffer
  // outlives its readers on purpose, so a reader coming back to a build
  // reopens the socket on a retained entry — and appending that first message
  // to what the earlier socket delivered printed every line twice (code review
  // 2026-11-11, O-1). The first message replaces; the rest append.
  let first = true;
  ws.onmessage = (event) => {
    const data = String(event.data);
    if (data === BUILD_LOG_SENTINEL) {
      publish(entry, { ended: true });
      return;
    }
    // The server sends whatever it has stored as one message and then a line
    // at a time, so a message is not a line.
    const { lines, at } = parseStamped(data);
    const fresh = first;
    first = false;
    publish(entry, {
      lines: fresh ? lines : [...entry.snapshot.lines, ...lines],
      at: fresh ? at : [...entry.snapshot.at, ...at],
    });
  };
  ws.onerror = () => publish(entry, { state: "closed" });
  ws.onclose = () => {
    if (entry.socket === ws) entry.socket = null;
    publish(entry, { state: "closed" });
  };
}

/**
 * One message of the `?times=1` stream: each line is "<unix ms>\t<line>", or
 * "-\t<line>" where the build recorded no time. A line without the tab is taken
 * whole and unmarked — a server from before the stream carried times.
 */
export function parseStamped(data: string): { lines: string[]; at: (string | undefined)[] } {
  const lines: string[] = [];
  const at: (string | undefined)[] = [];
  for (const raw of data.replace(/\n$/, "").split("\n")) {
    const tab = raw.indexOf("\t");
    const head = tab > 0 ? raw.slice(0, tab) : "";
    if (head === "-" || /^\d+$/.test(head)) {
      lines.push(raw.slice(tab + 1));
      at.push(head === "-" ? undefined : new Date(Number(head)).toISOString());
    } else {
      lines.push(raw);
      at.push(undefined);
    }
  }
  return { lines, at };
}

function close(entry: Entry) {
  const ws = entry.socket;
  if (!ws) return;
  entry.socket = null;
  // Detached first: closing because nobody is reading is not the socket going
  // away under somebody, and reporting it as one would flash "not live" on the
  // way out.
  ws.onclose = null;
  ws.onerror = null;
  ws.onmessage = null;
  ws.close();
}

/** Drop the buffers of builds nothing is reading, oldest first. */
function prune() {
  const idle = [...streams.entries()]
    .filter(([, e]) => e.refs === 0)
    .sort((a, b) => b[1].touched - a[1].touched);
  for (const [key, entry] of idle.slice(RETAINED_BUFFERS)) {
    close(entry);
    streams.delete(key);
  }
}

function acquire(key: string, url: string): Entry {
  let entry = streams.get(key);
  if (!entry) {
    entry = {
      url,
      refs: 0,
      touched: Date.now(),
      socket: null,
      listeners: new Set(),
      snapshot: { ...EMPTY, state: "connecting" },
    };
    streams.set(key, entry);
    prune();
  }
  entry.refs += 1;
  entry.touched = Date.now();
  open(entry);
  return entry;
}

function release(key: string) {
  const entry = streams.get(key);
  if (!entry) return;
  entry.refs -= 1;
  entry.touched = Date.now();
  // Deferred, because React mounts an effect twice in development: closing on
  // the first cleanup would drop a socket that is about to be wanted again, and
  // the lines a running build printed in between are unrecoverable.
  setTimeout(() => {
    if (entry.refs <= 0) close(entry);
  }, 0);
}

/**
 * Follow one build's log.
 *
 * The socket's first message is everything the build has printed so far —
 * the builder's live copy while it runs, the row once it has ended — and each
 * message after it is new output, so a page that connects at minute four still
 * shows minute one.
 */
export function useBuildLog(
  appId: string | undefined,
  deployID: string | undefined,
): BuildLogSnapshot & { reconnect: () => void } {
  const key = appId && deployID ? `${appId}/${deployID}` : "";
  const url = key ? wsUrl(`/apps/${appId}/deployments/${deployID}/stream?times=1`) : "";

  /**
   * Subscribing is what opens the socket, rather than an effect beside it.
   *
   * `useSyncExternalStore` calls this on mount and cleans it up on unmount, so
   * the reference count and the reader count are the same number by
   * construction. An effect would run *after* the first `getSnapshot`, leaving
   * the store empty at the moment React asked it what to draw.
   */
  const subscribe = useCallback(
    (listener: () => void) => {
      if (!key) return () => {};
      const entry = acquire(key, url);
      entry.listeners.add(listener);
      return () => {
        entry.listeners.delete(listener);
        release(key);
      };
    },
    [key, url],
  );

  const snapshot = useSyncExternalStore(
    subscribe,
    () => streams.get(key)?.snapshot ?? EMPTY,
    () => EMPTY,
  );

  const reconnect = useCallback(() => {
    const entry = streams.get(key);
    if (!entry) return;
    // Started clean: the new socket's first message is the whole log so far,
    // including everything the dropped one had delivered.
    entry.snapshot = { ...entry.snapshot, lines: [], at: [], ended: false };
    open(entry);
  }, [key]);

  return { ...snapshot, reconnect };
}

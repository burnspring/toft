import { digestBlob, type WorkerReply, type WorkerRequest } from "./store-hash";

/**
 * The digest pass, off the main thread.
 *
 * Deliberately thin: everything worth testing is in `digestBlob`, which runs
 * here and in a test with no worker at all. What this file owns is the message
 * shape and the promise that a failure arrives as a message rather than as
 * silence — a worker that throws and says nothing leaves a transfer in
 * "Checking…" with nothing to cancel.
 *
 * **No `/// <reference lib="webworker" />`.** That directive swaps the DOM lib
 * out for the whole compilation, so one worker file turns `window` into an
 * unknown name in every other file in the SPA. The two members this needs are
 * named here instead.
 */
const ctx = self as unknown as {
  onmessage: ((e: MessageEvent<WorkerRequest>) => void) | null;
  postMessage: (message: WorkerReply) => void;
};

ctx.onmessage = async (e) => {
  const reply = (msg: WorkerReply) => ctx.postMessage(msg);
  try {
    const digests = await digestBlob(e.data.blob, (bytesRead) =>
      reply({ type: "progress", bytesRead }),
    );
    reply({ type: "done", digests });
  } catch (err) {
    reply({ type: "error", message: err instanceof Error ? err.message : String(err) });
  }
};

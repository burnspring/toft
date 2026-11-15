import { createMD5, createSHA256 } from "hash-wasm";

/**
 * The two digests the platform signs, computed in the browser.
 *
 * `POST /store/{slug}/objects` signs `Content-MD5` and `x-amz-checksum-sha256`
 * into the URL, so the bytes that go up have to match a number computed before
 * the URL exists. That is the whole reason this file is here and the reason it
 * runs *first*: a URL lives one hour, and hashing four gigabytes takes minutes,
 * so minting before hashing spends part of the hour on the digest pass and, on
 * a slow link, hands the object store a signature that expired mid-body.
 *
 * **Why a dependency rather than WebCrypto.** `crypto.subtle.digest` is
 * one-shot — it takes the whole buffer — which for a file bigger than memory is
 * not an implementation detail but a wall, and it has no MD5 at all. The
 * alternative was hand-owned MD5 and SHA-256 in this tree, which is a very
 * different kind of code to own for the sake of one fewer package.
 *
 * **Both digests, one pass.** The file is read once in slices and fed to both
 * hashers, because reading a multi-gigabyte file off a disk twice is the
 * expensive half.
 */

/** 8 MiB: big enough that the per-slice overhead disappears, small enough that
 * peak memory is a slice and never the file. */
export const SLICE_BYTES = 8 * 1024 * 1024;

export interface StoreDigests {
  /** Hex. The server takes hex or base64 and normalizes; hex is what a person
   * can compare against `sha256sum` when they want to know why an upload was
   * refused. */
  sha256: string;
  md5: string;
}

/**
 * Hash a blob, reporting bytes read as it goes.
 *
 * Exported separately from the worker so it can be tested without one: this is
 * the function known vectors are checked against, and the worker is a thin
 * wrapper that runs exactly this off the main thread.
 */
export async function digestBlob(
  blob: Blob,
  onProgress?: (bytesRead: number) => void,
  signal?: AbortSignal,
): Promise<StoreDigests> {
  const [md5, sha256] = await Promise.all([createMD5(), createSHA256()]);
  md5.init();
  sha256.init();

  let read = 0;
  // A zero-byte file has no slices, so the loop below never runs and both
  // hashers return the digest of nothing — which is correct, and is the only
  // case where the digests are computed without reading anything.
  while (read < blob.size) {
    if (signal?.aborted) throw new DOMException("aborted", "AbortError");
    const slice = blob.slice(read, Math.min(read + SLICE_BYTES, blob.size));
    const bytes = new Uint8Array(await slice.arrayBuffer());
    md5.update(bytes);
    sha256.update(bytes);
    read += bytes.byteLength;
    onProgress?.(read);
  }

  return { md5: md5.digest("hex"), sha256: sha256.digest("hex") };
}

/**
 * Hash a file off the main thread.
 *
 * A four-gigabyte file takes minutes of solid CPU, and on the main thread that
 * is a frozen tab — the page would stop answering scrolls and clicks while the
 * tray showed a bar at zero. The worker is the difference between "Checking…"
 * and "this page is broken".
 *
 * Falls back to the main thread where `Worker` is absent, which is every test
 * environment and no browser this product supports. The fallback is honest
 * rather than fast: the digests are the same, the tab is not.
 */
export function hashFile(
  file: Blob,
  onProgress?: (bytesRead: number) => void,
  signal?: AbortSignal,
): Promise<StoreDigests> {
  if (typeof Worker === "undefined") return digestBlob(file, onProgress, signal);

  return new Promise((resolve, reject) => {
    // An already-aborted signal never fires `abort` again.
    if (signal?.aborted) {
      reject(new DOMException("aborted", "AbortError"));
      return;
    }
    const worker = new Worker(new URL("./store-hash.worker.ts", import.meta.url), {
      type: "module",
    });
    const done = () => {
      worker.terminate();
      signal?.removeEventListener("abort", onAbort);
    };
    const onAbort = () => {
      done();
      reject(new DOMException("aborted", "AbortError"));
    };
    signal?.addEventListener("abort", onAbort);

    worker.onmessage = (e: MessageEvent<WorkerReply>) => {
      const msg = e.data;
      if (msg.type === "progress") {
        onProgress?.(msg.bytesRead);
        return;
      }
      done();
      if (msg.type === "done") resolve(msg.digests);
      else reject(new Error(msg.message));
    };
    // A worker that dies mid-file has to reject, or the transfer sits in
    // "Checking…" forever with nothing to cancel.
    worker.onerror = (e) => {
      done();
      reject(new Error(e.message || "the digest worker stopped"));
    };
    worker.postMessage({ blob: file } satisfies WorkerRequest);
  });
}

export interface WorkerRequest {
  blob: Blob;
}

export type WorkerReply =
  | { type: "progress"; bytesRead: number }
  | { type: "done"; digests: StoreDigests }
  | { type: "error"; message: string };

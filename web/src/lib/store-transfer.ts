import { type StoreDigests, hashFile } from "./store-hash";

/**
 * The only place in this SPA that talks to an origin other than its own.
 *
 * Everything else posts to Toft. A store upload goes straight to the
 * object store, because the platform signs and never proxies — which is the
 * design's whole virtue and the source of every failure mode below. Keeping it
 * behind typed functions here rather than a bare `fetch` in a component is the
 * convention that already holds, and it matters more here than usual: the three
 * ways this fails look identical from a component and belong to three different
 * teams.
 */

/** What went wrong, in the terms of §7 of the store browser's design. */
export type TransferFailure =
  | "cors"
  | "refused"
  | "exists"
  | "expired"
  | "digest"
  | "cancelled"
  | "unknown";

export class StoreTransferError extends Error {
  readonly kind: TransferFailure;
  readonly status?: number;

  constructor(kind: TransferFailure, message: string, status?: number) {
    super(message);
    this.name = "StoreTransferError";
    this.kind = kind;
    this.status = status;
  }
}

/**
 * A failure told apart, so that "upload failed" does not send somebody to the
 * wrong team.
 *
 * The reasoning `cli/store.go` already applies to a hostname: a caller who
 * cannot tell an expired signature from a missing CORS rule from a revoked
 * grant will guess, and every guess here is somebody else's afternoon.
 */
export function describeTransferFailure(kind: TransferFailure): string {
  switch (kind) {
    case "cors":
      return "The object store did not accept a request from this site. An administrator needs to allow this origin on the bucket.";
    case "refused":
      return "This transfer was refused by the object store. Your access may have changed — reload the page.";
    case "exists":
      return "Something else wrote this object first.";
    case "expired":
      return "This transfer took longer than an hour, which is how long a signed URL lives. toft store put is steadier for a file this size.";
    case "digest":
      return "The file changed while it was being read. Nothing was written.";
    case "cancelled":
      return "Cancelled.";
    case "unknown":
      return "The object store refused this transfer and gave no reason this page recognizes.";
  }
}

/**
 * Which of the five a response is.
 *
 * Pure, and separated from the request for exactly that reason: this is the
 * part worth testing, and it is the part that is wrong in a way nobody notices
 * until somebody is standing in front of the wrong error message.
 *
 * **Status 0 is the CORS case and it has no status by definition.** A preflight
 * the bucket never answers surfaces in every browser as a network error with
 * nothing on it — no status, no body, no headers — which is indistinguishable
 * from the machine being offline and is why this is a sentence about the bucket
 * rather than about the file.
 */
export function classifyTransferFailure(status: number, body: string): TransferFailure {
  if (status === 0) return "cors";
  if (status === 412) return "exists";
  if (status === 400 && /BadDigest|Content-MD5|checksum/i.test(body)) return "digest";
  if (status === 403) {
    // S3 says "Request has expired" for a signature past its TTL and
    // "AccessDenied" for a permission it never had. The first is a transfer
    // that took too long; the second is a grant that changed underneath it.
    if (/expired|ExpiredToken/i.test(body)) return "expired";
    return "refused";
  }
  return "unknown";
}

/**
 * Headers a browser sets for itself and refuses to be told.
 *
 * The platform returns every header SigV4 signed, which is right for `toft store
 * put` and includes two the fetch spec forbids a page from setting. Sending
 * them is not the point — *matching* them is, and the browser matches them by
 * construction: `Host` is the URL's own host and `Content-Length` is the body's
 * own length. Skipping them keeps the signature valid and keeps two red console
 * errors off every upload, which is worth more than it sounds: a page that logs
 * errors on the happy path is a page nobody reads the log of.
 */
const BROWSER_OWNED_HEADERS = new Set(["host", "content-length"]);

/** A URL the platform minted, and the headers it signed into it. */
export interface SignedTarget {
  url: string;
  method: string;
  headers: Record<string, string>;
}

export interface SendOptions {
  onProgress?: (sent: number, total: number) => void;
  signal?: AbortSignal;
}

/**
 * Send a body to a signed URL, reporting progress.
 *
 * **XMLHttpRequest, not fetch**, and it is not nostalgia: `fetch` has no upload
 * progress event, so a two-gigabyte PUT would be a page that looks frozen for
 * several minutes. The body is the `File` itself rather than a buffer, so the
 * browser streams it off disk and peak memory stays a slice.
 */
export function sendSigned(
  target: SignedTarget,
  body: Blob | null,
  opts: SendOptions = {},
): Promise<void> {
  return new Promise((resolve, reject) => {
    // An already-aborted signal never fires `abort` again.
    if (opts.signal?.aborted) {
      reject(new StoreTransferError("cancelled", describeTransferFailure("cancelled")));
      return;
    }
    const xhr = new XMLHttpRequest();
    xhr.open(target.method, target.url, true);
    for (const [name, value] of Object.entries(target.headers)) {
      // Every header here was signed into the URL. Dropping one that carries a
      // *condition* does not relax it, it invalidates the signature — which is
      // the point. The two the browser owns are the exception, and they are an
      // exception because the browser sends the same values anyway.
      if (BROWSER_OWNED_HEADERS.has(name.toLowerCase())) continue;
      xhr.setRequestHeader(name, value);
    }

    const abort = () => xhr.abort();
    opts.signal?.addEventListener("abort", abort);
    const done = () => opts.signal?.removeEventListener("abort", abort);

    if (body && opts.onProgress) {
      xhr.upload.onprogress = (e) => opts.onProgress?.(e.loaded, e.total || body.size);
    }
    xhr.onload = () => {
      done();
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve();
        return;
      }
      const kind = classifyTransferFailure(xhr.status, xhr.responseText || "");
      reject(new StoreTransferError(kind, describeTransferFailure(kind), xhr.status));
    };
    xhr.onerror = () => {
      done();
      // No status, no body: the browser refuses to tell a page anything about a
      // cross-origin response it was not allowed to read.
      reject(new StoreTransferError("cors", describeTransferFailure("cors"), 0));
    };
    xhr.onabort = () => {
      done();
      reject(new StoreTransferError("cancelled", describeTransferFailure("cancelled")));
    };

    xhr.send(body);
  });
}

// --- The queue behind the tray ---

export type TransferPhase =
  | "queued"
  | "checking"
  | "uploading"
  | "done"
  | "failed"
  | "cancelled";

export interface Transfer {
  id: string;
  /** The object's name within the store — what the table will show once it
   * exists, and what a folder upload's relative path becomes. */
  name: string;
  storeSlug: string;
  /** The folder it is being written into, so the tray can invalidate the right
   * listing and only that one. */
  path: string;
  size: number;
  phase: TransferPhase;
  /** Bytes hashed while checking, bytes sent while uploading. One number,
   * because the tray shows one bar and the phase says which pass it is. */
  transferred: number;
  /** Bytes per second, uploading only — a rate for the hashing pass would be
   * measuring the disk and calling it the network. */
  rate: number;
  error?: string;
  failure?: TransferFailure;
}

/**
 * Three at a time.
 *
 * Not one: a queue of small files against a round-trip per file is dominated by
 * latency, and three is where that stops being the limit. Not unbounded: a
 * hundred parallel PUTs from one tab is a self-inflicted denial of the object
 * store, and the person who typed it would experience it as the platform being
 * slow.
 */
export const MAX_CONCURRENT_UPLOADS = 3;

export interface UploadRequest {
  storeSlug: string;
  path: string;
  name: string;
  file: File;
}

export interface QueueHooks {
  /** POST /store/{slug}/objects — the mint, which happens *after* the digests. */
  mint: (req: UploadRequest, digests: StoreDigests) => Promise<SignedTarget>;
  /** Overridable so the queue can be tested without a worker or a network. */
  hash?: (file: Blob, onProgress: (read: number) => void, signal: AbortSignal) => Promise<StoreDigests>;
  send?: (target: SignedTarget, body: Blob, opts: SendOptions) => Promise<void>;
  /** Every terminal state, for the toast and the query invalidation. */
  onSettled?: (transfer: Transfer) => void;
}

/**
 * The transfer queue: headless, so the tray is a rendering of it.
 *
 * It survives navigation within the SPA on purpose — a person who starts a
 * four-gigabyte upload and then goes to look at an app has not asked to cancel
 * it — which is why it is a module-level object subscribed to by the tray and
 * not state inside a page component. Closing the tab does cancel, because
 * nothing else can be true, and `beforeunload` is armed only while something is
 * actually running.
 */
export class TransferQueue {
  private transfers: Transfer[] = [];
  /** Settled ids already toasted. See {@link announce}. */
  private announced = new Set<string>();
  private listeners = new Set<() => void>();
  private controllers = new Map<string, AbortController>();
  /** Ids waiting to start, in order, and the request each one is. Ids rather
   * than requests, so a row and the work it stands for cannot come apart —
   * two files with the same name in one drop is the case that finds it. */
  private pending: string[] = [];
  private requests = new Map<string, UploadRequest>();
  private running = 0;
  private seq = 0;

  private hooks: QueueHooks;

  constructor(hooks: QueueHooks) {
    this.hooks = hooks;
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  /** A stable snapshot, because useSyncExternalStore compares by identity and
   * a fresh array every read is an infinite render. */
  getSnapshot = () => this.transfers;

  enqueue(requests: UploadRequest[]) {
    for (const req of requests) {
      const id = `t${++this.seq}`;
      this.transfers = [
        ...this.transfers,
        {
          id,
          name: req.name,
          storeSlug: req.storeSlug,
          path: req.path,
          size: req.file.size,
          phase: "queued",
          transferred: 0,
          rate: 0,
        },
      ];
      this.requests.set(id, req);
      this.pending.push(id);
    }
    this.emit();
    this.pump();
  }

  cancel(id: string) {
    this.controllers.get(id)?.abort();
    const t = this.byId(id);
    // A transfer still in the queue has no controller to abort, so cancelling
    // it is removing it from the queue — otherwise it would start later, after
    // the person who watched the row go away believed it gone.
    if (t && t.phase === "queued") {
      this.pending = this.pending.filter((pendingID) => pendingID !== id);
      this.requests.delete(id);
      this.patch(id, { phase: "cancelled" });
      this.settle(id);
    }
  }

  /**
   * Has this settled transfer been announced yet? True exactly once per id.
   *
   * It lives on the queue rather than in the tray because the tray is inside
   * `Layout`, which is mounted from two positions in the route tree and so
   * remounts on every `/` ↔ anything navigation. A component-local `useRef`
   * reset on each of those, and three uploads finished ten minutes ago
   * re-toasted every time somebody clicked Apps. Success feedback that repeats
   * for work on a page the person has left teaches them to ignore toasts,
   * which is the one mechanism every mutation in this product relies on.
   *
   * It neither emits nor touches `transfers`, so the `useSyncExternalStore`
   * snapshot is unchanged by it.
   */
  announce(id: string): boolean {
    if (this.announced.has(id)) return false;
    this.announced.add(id);
    return true;
  }

  /**
   * Run a failed upload again, from the same file.
   *
   * The request is kept past a failure for exactly this — without it the
   * person has to find the file on disk again to retry a network blip. It is
   * forgotten when the row is dismissed, or when the retry settles.
   */
  retry(id: string) {
    const t = this.byId(id);
    if (!t || t.phase !== "failed" || !this.requests.has(id)) return;
    this.announced.delete(id);
    this.patch(id, { phase: "queued", error: undefined, failure: undefined, transferred: 0, rate: 0 });
    this.pending.push(id);
    this.pump();
  }

  /** Can this row be run again? */
  canRetry(id: string): boolean {
    return this.byId(id)?.phase === "failed" && this.requests.has(id);
  }

  /** Clear the terminal rows. The running ones stay, because a tray that could
   * be emptied mid-transfer would be a cancel button wearing a different word. */
  dismissSettled() {
    for (const t of this.transfers) {
      if (t.phase === "done" || t.phase === "failed" || t.phase === "cancelled") {
        // Forget what has left the tray, so the set cannot grow for the life
        // of the tab — and a failed row's request, kept for its Retry.
        this.announced.delete(t.id);
        this.requests.delete(t.id);
      }
    }
    this.transfers = this.transfers.filter(
      (t) => t.phase !== "done" && t.phase !== "failed" && t.phase !== "cancelled",
    );
    this.emit();
  }

  get active(): number {
    return this.transfers.filter((t) => t.phase === "checking" || t.phase === "uploading" || t.phase === "queued")
      .length;
  }

  private byId(id: string) {
    return this.transfers.find((t) => t.id === id);
  }

  private emit() {
    for (const fn of this.listeners) fn();
  }

  private patch(id: string, patch: Partial<Transfer>) {
    this.transfers = this.transfers.map((t) => (t.id === id ? { ...t, ...patch } : t));
    this.emit();
  }

  private settle(id: string) {
    const t = this.byId(id);
    if (t) this.hooks.onSettled?.(t);
  }

  private pump() {
    while (this.running < MAX_CONCURRENT_UPLOADS && this.pending.length > 0) {
      const id = this.pending.shift()!;
      const req = this.requests.get(id);
      if (!req) continue;
      this.running++;
      void this.run(id, req).finally(() => {
        this.running--;
        this.pump();
      });
    }
  }

  private async run(id: string, req: UploadRequest) {
    const control = new AbortController();
    this.controllers.set(id, control);
    const hash = this.hooks.hash ?? hashFile;
    const send = this.hooks.send ?? sendSigned;

    try {
      // **Hash first, mint second.** A URL lives one hour from the moment it is
      // signed; hashing a multi-gigabyte file takes minutes. Minting first
      // spends part of that hour on the digest pass and, on a slow link, hands
      // the object store a signature that expires mid-body.
      this.patch(id, { phase: "checking", transferred: 0 });
      const digests = await hash(req.file, (read) => this.patch(id, { transferred: read }), control.signal);
      // A cancel between phases fires no event anybody is listening to: the
      // hash may already be past its last slice, and the mint takes no signal.
      // Asked here, or the PUT runs anyway and replaces the object the person
      // decided not to replace.
      control.signal.throwIfAborted();

      const target = await this.hooks.mint(req, digests);
      control.signal.throwIfAborted();

      const startedAt = Date.now();
      this.patch(id, { phase: "uploading", transferred: 0 });
      await send(target, req.file, {
        signal: control.signal,
        onProgress: (sent) => {
          const elapsed = (Date.now() - startedAt) / 1000;
          this.patch(id, { transferred: sent, rate: elapsed > 0 ? sent / elapsed : 0 });
        },
      });

      this.patch(id, { phase: "done", transferred: req.file.size });
    } catch (err) {
      const cancelled =
        (err instanceof StoreTransferError && err.kind === "cancelled") ||
        (err instanceof DOMException && err.name === "AbortError");
      if (cancelled) {
        this.patch(id, { phase: "cancelled", error: undefined });
      } else if (err instanceof StoreTransferError) {
        this.patch(id, { phase: "failed", error: err.message, failure: err.kind });
      } else {
        // A refusal from Toft itself — the mint — which is a different
        // origin's failure and already carries the platform's own words.
        this.patch(id, {
          phase: "failed",
          error: err instanceof Error ? err.message : String(err),
          failure: "unknown",
        });
      }
    } finally {
      this.controllers.delete(id);
      // A failure keeps its request, so the row can be retried.
      if (this.byId(id)?.phase !== "failed") this.requests.delete(id);
      this.settle(id);
    }
  }
}

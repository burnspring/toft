import { describe, expect, it, vi } from "vitest";

import {
  MAX_CONCURRENT_UPLOADS,
  type QueueHooks,
  type SignedTarget,
  StoreTransferError,
  type Transfer,
  TransferQueue,
  type UploadRequest,
  classifyTransferFailure,
  describeTransferFailure,
  sendSigned,
} from "./store-transfer";

const TARGET: SignedTarget = { url: "https://s3.example/x", method: "PUT", headers: {} };

function fakeFile(name: string, size = 4): File {
  return new File([new Uint8Array(size)], name);
}

function req(name: string, path = ""): UploadRequest {
  return { storeSlug: "reports", path, name, file: fakeFile(name) };
}

/** A queue with every outside edge replaced: no worker, no network, no clock
 * to wait on. What is under test is the state machine, and the state machine is
 * the part a person watches. */
function queue(over: Partial<QueueHooks> = {}) {
  const settled: Transfer[] = [];
  const q = new TransferQueue({
    mint: async () => TARGET,
    hash: async () => ({ sha256: "a".repeat(64), md5: "b".repeat(32) }),
    send: async () => {},
    onSettled: (t) => settled.push(t),
    ...over,
  });
  return { q, settled };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

describe("classifyTransferFailure", () => {
  it("tells the five apart", () => {
    expect(classifyTransferFailure(0, "")).toBe("cors");
    expect(classifyTransferFailure(412, "<Error><Code>PreconditionFailed</Code></Error>")).toBe(
      "exists",
    );
    expect(classifyTransferFailure(400, "<Error><Code>BadDigest</Code></Error>")).toBe("digest");
    expect(classifyTransferFailure(403, "<Error><Code>AccessDenied</Code></Error>")).toBe("refused");
    expect(classifyTransferFailure(403, "<Message>Request has expired</Message>")).toBe("expired");
    expect(classifyTransferFailure(500, "")).toBe("unknown");
  });

  it("says something specific for every one of them", () => {
    // A kind with no sentence would surface as "undefined" in the tray, which
    // is the failure this feature exists to stop.
    for (const kind of [
      "cors",
      "refused",
      "exists",
      "expired",
      "digest",
      "cancelled",
      "unknown",
    ] as const) {
      expect(describeTransferFailure(kind).length).toBeGreaterThan(8);
    }
    // The CORS sentence is about the bucket and the origin, because it is the
    // one failure that is nobody's file and nobody's grant.
    expect(describeTransferFailure("cors")).toMatch(/origin/i);
    expect(describeTransferFailure("expired")).toMatch(/toft store put/);
  });
});

describe("TransferQueue", () => {
  it("hashes before it mints", async () => {
    const order: string[] = [];
    const { q } = queue({
      hash: async () => {
        order.push("hash");
        return { sha256: "a".repeat(64), md5: "b".repeat(32) };
      },
      mint: async () => {
        order.push("mint");
        return TARGET;
      },
      send: async () => {
        order.push("send");
      },
    });

    q.enqueue([req("a.csv")]);
    await settle();

    // The whole reason the digest pass is first: a URL lives one hour, and a
    // mint that happened before a four-gigabyte hash spends part of that hour
    // on the disk.
    expect(order).toEqual(["hash", "mint", "send"]);
    expect(q.getSnapshot()[0].phase).toBe("done");
  });

  it("runs three at a time and no more", async () => {
    let running = 0;
    let peak = 0;
    const release: Array<() => void> = [];
    const { q } = queue({
      send: () =>
        new Promise<void>((resolve) => {
          running++;
          peak = Math.max(peak, running);
          release.push(() => {
            running--;
            resolve();
          });
        }),
    });

    q.enqueue(["a", "b", "c", "d", "e"].map((n) => req(`${n}.csv`)));
    await settle();

    expect(peak).toBe(MAX_CONCURRENT_UPLOADS);
    expect(q.getSnapshot().filter((t) => t.phase === "queued")).toHaveLength(2);

    // Drain: each release frees a slot, which starts one of the two waiting —
    // which is the property under test, expressed as the queue emptying rather
    // than as a count.
    while (release.length > 0) {
      release.shift()!();
      await settle();
    }
    expect(q.getSnapshot().every((t) => t.phase === "done")).toBe(true);
    expect(peak).toBe(MAX_CONCURRENT_UPLOADS);
  });

  it("reports a 412 as something else having written first", async () => {
    const { q, settled } = queue({
      send: async () => {
        throw new StoreTransferError("exists", describeTransferFailure("exists"), 412);
      },
    });

    q.enqueue([req("nightly.parquet")]);
    await settle();

    const [t] = q.getSnapshot();
    expect(t.phase).toBe("failed");
    expect(t.failure).toBe("exists");
    expect(t.error).toMatch(/wrote this object first/);
    // Failed is a terminal state and settles like any other, because the tray
    // row has to stay until it is dismissed and the toast has to fire.
    expect(settled).toHaveLength(1);
  });

  it("retries a failed upload from the same file, and forgets it once dismissed", async () => {
    let attempts = 0;
    const { q, settled } = queue({
      send: async () => {
        attempts++;
        if (attempts === 1) throw new StoreTransferError("expired", describeTransferFailure("expired"), 403);
      },
    });
    q.enqueue([req("nightly.parquet")]);
    await settle();
    const [failed] = q.getSnapshot();
    expect(failed.phase).toBe("failed");
    expect(q.canRetry(failed.id)).toBe(true);

    q.retry(failed.id);
    await settle();
    expect(q.getSnapshot()[0].phase).toBe("done");
    expect(q.getSnapshot()[0].error).toBeUndefined();
    expect(settled.map((t) => t.phase)).toEqual(["failed", "done"]);
    // Done, and the request is gone with it: nothing left to run again.
    expect(q.canRetry(failed.id)).toBe(false);
  });

  it("reports an expired mint in the words that name the remedy", async () => {
    const { q } = queue({
      send: async () => {
        throw new StoreTransferError("expired", describeTransferFailure("expired"), 403);
      },
    });
    q.enqueue([req("huge.bin")]);
    await settle();
    expect(q.getSnapshot()[0].error).toMatch(/toft store put/);
  });

  it("cancels a running transfer through its signal", async () => {
    const { q } = queue({
      send: (_target, _body, opts) =>
        new Promise<void>((_resolve, reject) => {
          opts.signal?.addEventListener("abort", () =>
            reject(new StoreTransferError("cancelled", "Cancelled.")),
          );
        }),
    });

    q.enqueue([req("slow.bin")]);
    await settle();
    expect(q.getSnapshot()[0].phase).toBe("uploading");

    q.cancel(q.getSnapshot()[0].id);
    await settle();

    const [t] = q.getSnapshot();
    expect(t.phase).toBe("cancelled");
    // Cancelling is not failing: a person who cancelled knows why, and an error
    // message under their own decision reads as something having gone wrong.
    expect(t.error).toBeUndefined();
  });

  // A cancel that lands after the hash's last slice, or during the mint, fires
  // no event either phase hears — so the PUT has to be refused by asking.
  for (const phase of ["hash", "mint"] as const) {
    it(`does not send a transfer cancelled during the ${phase}`, async () => {
      let sent = false;
      // The hash starts inside `enqueue`, so the row is found when it runs.
      const cancel = () => q.cancel(q.getSnapshot()[0].id);
      const { q } = queue({
        [phase]: async () => {
          cancel();
          return phase === "hash" ? { sha256: "a".repeat(64), md5: "b".repeat(32) } : TARGET;
        },
        send: async () => {
          sent = true;
        },
      });

      q.enqueue([req("replace.csv")]);
      await settle();

      expect(sent).toBe(false);
      expect(q.getSnapshot()[0].phase).toBe("cancelled");
    });
  }

  it("sendSigned refuses a signal that is already aborted", async () => {
    const control = new AbortController();
    control.abort();
    await expect(sendSigned(TARGET, null, { signal: control.signal })).rejects.toMatchObject({
      kind: "cancelled",
    });
  });

  it("cancels a queued transfer without ever starting it", async () => {
    const started: string[] = [];
    const release: Array<() => void> = [];
    const { q } = queue({
      send: (_t, body) =>
        new Promise<void>((resolve) => {
          started.push((body as File).name);
          release.push(resolve);
        }),
    });

    q.enqueue(["a", "b", "c", "d"].map((n) => req(`${n}.csv`)));
    await settle();

    const waiting = q.getSnapshot().find((t) => t.phase === "queued")!;
    q.cancel(waiting.id);
    for (const r of [...release]) r();
    await settle();
    await settle();

    expect(q.getSnapshot().find((t) => t.id === waiting.id)!.phase).toBe("cancelled");
    expect(started).not.toContain(waiting.name);
  });

  it("keeps two files of the same name apart", async () => {
    // A folder drop can carry two rows whose *name* matches — the same leaf in
    // two directories — and a queue that matched work to rows by name would
    // start one twice and strand the other.
    const { q } = queue();
    q.enqueue([req("data.csv", "a/"), req("data.csv", "b/")]);
    await settle();

    const rows = q.getSnapshot();
    expect(rows).toHaveLength(2);
    expect(rows.every((t) => t.phase === "done")).toBe(true);
    expect(new Set(rows.map((t) => t.id)).size).toBe(2);
  });

  it("dismisses what has settled and leaves what has not", async () => {
    const release: Array<() => void> = [];
    const { q } = queue({
      send: () => new Promise<void>((resolve) => release.push(resolve)),
    });

    q.enqueue([req("a.csv"), req("b.csv")]);
    await settle();
    release.shift()!();
    await settle();

    q.dismissSettled();
    const left = q.getSnapshot();
    expect(left).toHaveLength(1);
    expect(left[0].phase).toBe("uploading");
  });

  it("tells its subscribers about every step", async () => {
    const seen = vi.fn();
    const { q } = queue();
    q.subscribe(seen);
    q.enqueue([req("a.csv")]);
    await settle();
    // queued → checking → uploading → done, at least.
    expect(seen.mock.calls.length).toBeGreaterThanOrEqual(4);
  });
});

describe("sendSigned", () => {
  /** The two headers a page may not set, and the ones it must. */
  it("sends the signed conditions and leaves the browser's own headers alone", async () => {
    const set: Array<[string, string]> = [];
    class FakeXHR {
      upload = {} as { onprogress?: (e: ProgressEvent) => void };
      status = 200;
      responseText = "";
      onload?: () => void;
      onerror?: () => void;
      onabort?: () => void;
      open() {}
      setRequestHeader(name: string, value: string) {
        set.push([name, value]);
      }
      send() {
        this.onload?.();
      }
      abort() {}
    }
    const original = globalThis.XMLHttpRequest;
    // @ts-expect-error — a stand-in for the one browser API this file uses.
    globalThis.XMLHttpRequest = FakeXHR;
    try {
      await sendSigned(
        {
          url: "https://s3.example/x",
          method: "PUT",
          headers: {
            "Content-MD5": "abc==",
            "x-amz-checksum-sha256": "def==",
            "If-None-Match": "*",
            // Both of these come back from the platform and both are refused by
            // every browser: sending them is a console error, and not sending
            // them costs nothing because the browser's own values match.
            Host: "s3.example",
            "Content-Length": "12",
          },
        },
        new Blob(["x"]),
      );
    } finally {
      globalThis.XMLHttpRequest = original;
    }

    const names = set.map(([n]) => n.toLowerCase());
    expect(names).toContain("content-md5");
    expect(names).toContain("x-amz-checksum-sha256");
    expect(names).toContain("if-none-match");
    expect(names).not.toContain("host");
    expect(names).not.toContain("content-length");
  });
});

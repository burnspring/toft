/**
 * Whether this tab can still reach the install.
 *
 * The query cache is what made an outage invisible: a tab somebody had already
 * opened kept its last good copy, the background refetch failed, and the page
 * went on rendering the cached rows as if nothing had happened — so the only
 * way to learn the server was down was to reload. Every request goes through
 * `request()` in `api.ts`, which reports here, and the Layout's
 * `ConnectionBanner` is what reads it.
 *
 * Two kinds of answer mean "nobody answered": `fetch` itself throwing (the
 * server is down, or the network is), and a gateway status that is not this
 * server's JSON — Caddy or a load balancer saying the upstream is gone. The
 * platform does write 502 itself, for an object store or a git host that did
 * not answer, and those carry a JSON body: the install answered, and the page
 * renders its sentence as it always did.
 *
 * A module singleton rather than React state, because the reporter is a plain
 * function with no component above it.
 */

type Listener = () => void;

let unreachable = false;
const listeners = new Set<Listener>();

function set(next: boolean) {
  if (next === unreachable) return;
  unreachable = next;
  for (const l of listeners) l();
}

export function isUnreachable(): boolean {
  return unreachable;
}

export function subscribeConnection(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

const GATEWAY = new Set([502, 503, 504]);

/** Whether a response came from something in front of the install rather
 *  than from the install. */
export function isGatewayFailure(status: number, contentType: string | null): boolean {
  return GATEWAY.has(status) && !(contentType ?? "").includes("application/json");
}

/** A request got an answer. */
export function noteResponse(res: Pick<Response, "status" | "headers">) {
  set(isGatewayFailure(res.status, res.headers.get("content-type")));
}

/** `fetch` threw. An abort is the page cancelling, not the server going. */
export function noteFetchError(err: unknown) {
  if (err instanceof Error && err.name === "AbortError") return;
  set(true);
}

/** Asks once, for the banner's poll. `/config` is public, so it answers a
 *  session that has meanwhile expired too — which `request()` then handles on
 *  the page's own next read. */
export async function probeConnection(): Promise<boolean> {
  try {
    const res = await fetch("/api/v1/config", { cache: "no-store", credentials: "include" });
    noteResponse(res);
  } catch (err) {
    noteFetchError(err);
  }
  return !unreachable;
}

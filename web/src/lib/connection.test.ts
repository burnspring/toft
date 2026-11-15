import { describe, expect, it } from "vitest";

import { isGatewayFailure, isUnreachable, noteFetchError, noteResponse } from "./connection";

/* ------------------------------------------------------------------ *
 * Whether the install answered
 *
 * A tab kept rendering its cached copy through an outage, so the only
 * way to notice the server had gone was a reload. The half worth a test
 * is the line between "nobody answered" and "the install answered with
 * a failure": the platform writes 502 itself, with a JSON body, when an
 * object store or a git host does not answer — and that must not put up
 * a banner saying the install is unreachable.
 * ------------------------------------------------------------------ */

const res = (status: number, type: string | null) => ({
  status,
  headers: new Headers(type ? { "content-type": type } : {}),
});

describe("the connection", () => {
  it("calls a gateway status with no JSON a proxy's", () => {
    expect(isGatewayFailure(502, null)).toBe(true);
    expect(isGatewayFailure(504, "text/html")).toBe(true);
    expect(isGatewayFailure(503, "text/plain; charset=utf-8")).toBe(true);
  });

  it("calls the install's own 502 an answer", () => {
    expect(isGatewayFailure(502, "application/json")).toBe(false);
    expect(isGatewayFailure(500, null)).toBe(false);
    expect(isGatewayFailure(404, "text/html")).toBe(false);
  });

  it("goes down on a failed fetch and up on the next answer", () => {
    noteFetchError(new TypeError("Failed to fetch"));
    expect(isUnreachable()).toBe(true);
    noteResponse(res(403, "application/json"));
    expect(isUnreachable()).toBe(false);
    noteResponse(res(502, "text/html"));
    expect(isUnreachable()).toBe(true);
    noteResponse(res(200, "application/json"));
    expect(isUnreachable()).toBe(false);
  });

  it("does not take an abort for an outage", () => {
    const abort = new Error("aborted");
    abort.name = "AbortError";
    noteFetchError(abort);
    expect(isUnreachable()).toBe(false);
  });
});

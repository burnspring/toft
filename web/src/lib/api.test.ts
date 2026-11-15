import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";

// The calls that build their own request — the two uploads and the viewers CSV
// — bypassed `request`'s status handling, so a session that ended during an
// upload was shown as an upload failure instead of the login page.
describe("the calls that do not send JSON", () => {
  let location: { pathname: string; search: string; href: string };

  beforeEach(() => {
    location = { pathname: "/apps/new", search: "", href: "" };
    vi.stubGlobal("window", { location });
    vi.stubGlobal("sessionStorage", { getItem: () => "1", setItem() {}, removeItem() {} });
  });
  afterEach(() => vi.unstubAllGlobals());

  const respond = (status: number, body: unknown) =>
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      })),
    );

  const calls: [string, () => Promise<unknown>][] = [
    ["uploadApp", () => api.uploadApp(new File(["x"], "a.zip"), {})],
    ["uploadAppVersion", () => api.uploadAppVersion("app1", new File(["x"], "a.zip"))],
    ["downloadAppUsageViewers", () => api.downloadAppUsageViewers("app1", "2026-10-01", "2026-10-28")],
  ];

  for (const [name, call] of calls) {
    it(`${name} bounces an expired session to the login page`, async () => {
      respond(401, { error: "unauthorized" });
      await expect(call()).rejects.toBeInstanceOf(ApiError);
      expect(location.href).toBe("/login?redirect=%2Fapps%2Fnew&expired=1");
    });

    it(`${name} bounces a locked account to the login page`, async () => {
      respond(403, { error: "locked", reason: "locked" });
      await expect(call()).rejects.toMatchObject({ status: 403 });
      expect(location.href).toBe("/login?denied=locked");
    });
  }
});

// A wrong password on the local sign-in and password routes is the caller's
// answer, not a session that ended: no bounce, and the server's own sentence
// (code review 2026-11-11, O-2).
describe("a 401 from the local credential routes", () => {
  let location: { pathname: string; search: string; href: string };

  beforeEach(() => {
    location = { pathname: "/me", search: "", href: "" };
    vi.stubGlobal("window", { location });
    vi.stubGlobal("sessionStorage", { getItem: () => "1", setItem() {}, removeItem() {} });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("is thrown with the server's sentence and does not bounce", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "incorrect username or password" }), {
        status: 401,
        headers: { "Content-Type": "application/json" },
      })),
    );
    await expect(api.changePassword("admin", "wrong", "newer-and-longer")).rejects.toMatchObject({
      status: 401,
      message: "incorrect username or password",
    });
    expect(location.href).toBe("");
  });
});

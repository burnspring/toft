import { describe, expect, it } from "vitest";
import { connectRedirectTarget } from "./connect-redirect";

const id = "91fe8754-da19-4bdd-88d8-3ff64fe28e46";

describe("connectRedirectTarget", () => {
  it("maps rsconnect's two destinations onto the app's tabs", () => {
    expect(connectRedirectTarget(`#/apps/${id}/access`)).toBe(`/apps/${id}/access`);
    expect(connectRedirectTarget(`#/apps/${id}/logs`)).toBe(`/apps/${id}/logs`);
    expect(connectRedirectTarget(`#/apps/${id}`)).toBe(`/apps/${id}`);
  });
  it("sends anything else to the app list", () => {
    expect(connectRedirectTarget("")).toBe("/");
    expect(connectRedirectTarget("#/apps/not-an-id/access")).toBe("/");
    expect(connectRedirectTarget(`#/apps/${id}/settings`)).toBe("/");
  });
});

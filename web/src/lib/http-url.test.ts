import { describe, expect, it } from "vitest";
import { isHttpUrl } from "./http-url";

describe("isHttpUrl", () => {
  it("accepts http and https", () => {
    expect(isHttpUrl("https://osv.dev/vulnerability/GHSA-xxxx")).toBe(true);
    expect(isHttpUrl("http://mirror.internal/adv/1")).toBe(true);
  });

  it("refuses every other scheme and anything unparseable", () => {
    for (const u of [undefined, null, "", "javascript:alert(1)", " JavaScript:alert(1)", "data:text/html,x", "/relative", "not a url"]) {
      expect(isHttpUrl(u)).toBe(false);
    }
  });
});

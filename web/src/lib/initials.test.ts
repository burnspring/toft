import { describe, expect, it } from "vitest";
import { initials } from "./initials";

describe("initials", () => {
  it("tells apart logins that share a prefix", () => {
    expect(initials(undefined, "admin")).toBe("AD");
    expect(initials(undefined, "admin-one")).toBe("AO");
    expect(initials(undefined, "admin-two")).toBe("AT");
  });
  it("prefers the name's words over the login", () => {
    expect(initials("Local administrator", "admin")).toBe("LA");
    expect(initials("Ada Lovelace", "ada")).toBe("AL");
  });
  it("splits an email-shaped login", () => {
    expect(initials("", "kim.lee@example.com")).toBe("KL");
  });
  it("falls back to two letters of a single word, and never to nothing", () => {
    expect(initials("", "kim")).toBe("KI");
    expect(initials("", "")).toBe("?");
  });
});

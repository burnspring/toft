import { describe, expect, it } from "vitest";
import { signInCopy } from "./sign-in-client";

describe("signInCopy", () => {
  it("names the act each client's reader just performed", () => {
    expect(signInCopy("toft").act).toContain("toft login");
    expect(signInCopy("rsconnect").act).toContain("RStudio");
  });
  it("falls back to toft for anything else", () => {
    expect(signInCopy(undefined)).toEqual(signInCopy("toft"));
    expect(signInCopy("something-new")).toEqual(signInCopy("toft"));
  });
});

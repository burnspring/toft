import { describe, expect, it } from "vitest";
import { redirectURIPreview } from "./redirect-uri";

describe("redirectURIPreview", () => {
  it("takes the server's base, not the browser's origin", () => {
    const listed = [
      { id: "local", redirect_uri: "" },
      { id: "okta", redirect_uri: "https://toft.example.com/api/v1/auth/okta/callback" },
    ];
    expect(redirectURIPreview(listed, "entra", "http://localhost:8080")).toBe(
      "https://toft.example.com/api/v1/auth/entra/callback",
    );
  });

  it("falls back to the origin when no provider names one", () => {
    expect(redirectURIPreview([{ id: "local", redirect_uri: "" }], "", "http://localhost:8080")).toBe(
      "http://localhost:8080/api/v1/auth/<id>/callback",
    );
  });
});

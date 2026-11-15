import { describe, expect, it } from "vitest";
import { installNameError, INSTALL_NAME_LIMIT } from "./settings-fields";

// The server's NormalizeInstallName cases, restated: the client refuses what
// the server refuses, so the sentence lands under the field and not in a toast.
describe("installNameError", () => {
  it.each([
    "",
    "Staging Apps Platform",
    "Acme Inc. Analytics",
    "Platform v2.0",
    "Søren’s Apps",
    "a".repeat(INSTALL_NAME_LIMIT),
  ])("accepts %j", (v) => expect(installNameError(v)).toBeNull());

  it.each([
    "a".repeat(INSTALL_NAME_LIMIT + 1),
    "Staging\nApps",
    "<b>Staging</b>",
    "https://evil.example",
    "evil.example",
    "Apps at apps.example.com",
    "www.acme",
  ])("refuses %j", (v) => expect(installNameError(v)).not.toBeNull());
});

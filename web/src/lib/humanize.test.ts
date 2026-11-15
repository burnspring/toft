import { describe, expect, it } from "vitest";
import { humanize } from "./humanize";

describe("humanize", () => {
  it.each([
    ["current_role_since", "Current role since"],
    ["repo_url", "Repo URL"],
    ["id", "ID"],
    ["repo_subdir", "Repo subdir"],
    ["api_key_id", "API key ID"],
    ["oidc_issuer", "OIDC issuer"],
    ["commit_sha", "Commit SHA"],
    ["tls", "TLS"],
    ["Already_Capitalised", "Already capitalised"],
  ])("%s → %s", (key, want) => {
    expect(humanize(key)).toBe(want);
  });
});

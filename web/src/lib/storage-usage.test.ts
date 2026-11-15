import { describe, expect, it } from "vitest";

import {
  USAGE_LANGUAGES, cliExample, primaryWhere, resourceWhere, sdkExample, storageNameProblem,
} from "@/lib/storage-usage";

// The server's own table (TestStorageNamesFollowTheNamingSchema), so the field
// and the create cannot disagree about a name.
describe("storageNameProblem", () => {
  it.each([
    ["sales-data", null],
    ["s3-reports-2026", null],
    ["", null],
  ])("accepts %s", (name, want) => {
    expect(storageNameProblem(name)).toBe(want);
  });

  it.each(["Sales data", "sales_data", "2026-data", "ab", "sales--data", "sales-", "n".repeat(41)])(
    "refuses %s",
    (name) => {
      expect(storageNameProblem(name)).not.toBeNull();
    },
  );
});

describe("cliExample", () => {
  it("writes the resource's own name and folder into every line", () => {
    const text = cliExample({ name: "sales-data", folder: "2026/", access: "full" });
    for (const line of text.split("\n")) {
      expect(line).toContain("sales-data");
    }
    expect(text).toContain("toft store ls sales-data/2026");
    expect(text).toContain("toft store get sales-data 2026/report.csv");
    expect(text).toContain("toft store put --as 2026/report.csv sales-data ./report.csv");
    expect(text).toContain("toft store rm sales-data/2026/report.csv");
  });

  it("lists only what the reader's level allows", () => {
    const read = cliExample({ name: "sales-data", access: "read" });
    expect(read).not.toContain("put");
    expect(read).not.toContain("rm");
    const add = cliExample({ name: "sales-data", access: "write" });
    expect(add).toContain("put");
    expect(add).not.toContain("rm");
  });

  it("names the level a command needs when the reader's is unknown", () => {
    const text = cliExample({ name: "sales-data" });
    expect(text).toMatch(/put .*# needs Can add/);
    expect(text).toMatch(/rm .*# needs Read & write/);
  });
});

describe("sdkExample", () => {
  it.each(USAGE_LANGUAGES)("%s names the resource, for both kinds", (language) => {
    for (const kind of ["volume", "store"] as const) {
      expect(sdkExample({ name: "sales-data", kind, language })).toContain(`"sales-data"`);
    }
  });

  it("carries the folder in each language's own spelling", () => {
    const at = (language: (typeof USAGE_LANGUAGES)[number]) =>
      sdkExample({ name: "drop", folder: "backup", kind: "store", language });
    expect(at("python")).toContain(`toft.store("drop", folder="backup")`);
    expect(at("node")).toContain(`toft.store("drop", { folder: "backup" })`);
    expect(at("r")).toContain(`store("drop", "backup")`);
    expect(at("go")).toContain(`toft.StoreIn("drop", "backup")`);
  });

  it("appends to the volume's file in every language, as the others do (BB-102)", () => {
    const go = sdkExample({ name: "cache", kind: "volume", language: "go" });
    expect(go).toContain("os.O_APPEND");
    expect(go).not.toContain("os.WriteFile");
  });

  it("uses the volume accessor for a volume", () => {
    expect(sdkExample({ name: "cache", kind: "volume", language: "python" })).toContain(
      `toft.storage("cache")`,
    );
    expect(sdkExample({ name: "cache", kind: "volume", language: "go" })).toContain(
      `toft.Storage("cache")`,
    );
  });
});

describe("resourceWhere", () => {
  const base = { slug: "sales-data", claim_name: "lpv-sales-data" };

  const oms = {
    key: "root:oms", kind: "root" as const, name: "oms", label: "Order management",
    path: "/mnt/oms/", declared: true, resource_count: 1,
  };

  it("gives a host-root resource its drive's label and real directory", () => {
    const facts = resourceWhere({ ...base, backing: "host", storage_root: "oms" }, [oms]);
    expect(facts).toContainEqual({ label: "Drive", value: "Order management (oms)" });
    expect(facts).toContainEqual({ label: "Directory", value: "/mnt/oms/sales-data" });
    expect(facts.map((f) => f.label)).not.toContain("Claim");
  });

  it("says when a resource's drive is no longer declared", () => {
    const facts = resourceWhere({ ...base, backing: "host", storage_root: "efs" }, [
      { key: "root:efs", kind: "root", name: "efs", label: "efs", declared: false, resource_count: 1 },
    ]);
    expect(facts).toContainEqual({ label: "Drive", value: "efs, no longer declared" });
    expect(facts.find((f) => f.label === "Directory")?.value).toContain("no longer declares");
  });

  it("puts a cluster volume naming no class on its backing's default drive", () => {
    const facts = resourceWhere({ ...base, backing: "efs" }, [
      { key: "class:efs-toft", kind: "class", name: "efs-toft", label: "OMS share",
        backing: "efs", default: true, declared: true, resource_count: 1 },
    ]);
    expect(facts).toContainEqual({ label: "Drive", value: "OMS share (efs-toft)" });
    expect(facts).toContainEqual({ label: "Storage class", value: "efs-toft" });
  });

  it("gives a bucket its bucket, prefix and URI", () => {
    const facts = resourceWhere(
      { ...base, backing: "mountpoint", bucket: "acme", prefix: "sales" },
      [],
    );
    expect(facts).toContainEqual({ label: "Bucket", value: "acme" });
    expect(facts).toContainEqual({ label: "Prefix", value: "sales" });
    expect(facts).toContainEqual({ label: "URI", value: "s3://acme/sales/" });
    expect(primaryWhere({ ...base, backing: "blob", bucket: "acme", prefix: "sales" }, []))
      .toBe("az://acme/sales/");
  });

  it("gives a cluster volume its class and claim", () => {
    const facts = resourceWhere({ ...base, backing: "efs" }, []);
    expect(facts).toContainEqual({ label: "Storage class", value: "The install’s default" });
    expect(facts).toContainEqual({ label: "Claim", value: "lpv-sales-data" });
  });

  it("gives a store on a host install no claim, because nothing binds one", () => {
    const facts = resourceWhere({ ...base, backing: "mountpoint", bucket: "a", prefix: "p" }, [], false);
    expect(facts.map((f) => f.label)).not.toContain("Claim");
  });
});

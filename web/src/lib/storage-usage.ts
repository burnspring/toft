import type { StorageAccess, StorageDrive, StorageKind, StorageResource } from "@/lib/api";
import { storeAffordances } from "@/lib/storage";

/**
 * How to use one storage resource, written with its own name.
 *
 * Generic examples were the trap: a person granted `sales-data` read
 * `toft store ls reports` in the manual and had to work out that the word to
 * substitute was the small mono line under a label. Every string here is built
 * from the resource's real name and the folder being looked at, so what is on
 * the screen can be pasted as it is.
 *
 * Pure, so `storage-usage.test.ts` can hold every line to the SDKs' real
 * signatures (`sdk/python/toft/resources.py`, `sdk/node/app.js`,
 * `sdk/go/storage.go` and `store.go`, `sdk/r/R/storage.R`) and to `toft store`.
 */

/** The storage name rules, as the server states them (`ValidateStorageName`). */
export const STORAGE_NAME_MIN = 3;
export const STORAGE_NAME_MAX = 40;
const STORAGE_NAME_PATTERN = /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/;

/**
 * What is wrong with a proposed name, or null. A rendering of the server's rule
 * rather than a second opinion: the create refuses in its own words either way,
 * and this is what lets the field say so before the click.
 */
export function storageNameProblem(name: string): string | null {
  if (name === "") return null;
  if (name.length > STORAGE_NAME_MAX) return `At most ${STORAGE_NAME_MAX} characters.`;
  if (/[A-Z]/.test(name)) return "Lowercase only.";
  if (/[^a-z0-9-]/.test(name)) return "Letters, digits and hyphens only — no spaces.";
  if (!/^[a-z]/.test(name)) return "Start with a letter.";
  if (/--/.test(name)) return "One hyphen at a time.";
  if (name.endsWith("-")) return "Don’t end with a hyphen.";
  if (name.length < STORAGE_NAME_MIN) return `At least ${STORAGE_NAME_MIN} characters.`;
  return STORAGE_NAME_PATTERN.test(name) ? null : "Lowercase letters, digits and hyphens.";
}

export type UsageLanguage = "python" | "node" | "r" | "go";

export const USAGE_LANGUAGE_LABEL: Record<UsageLanguage, string> = {
  python: "Python",
  node: "Node",
  r: "R",
  go: "Go",
};

export const USAGE_LANGUAGES = Object.keys(USAGE_LANGUAGE_LABEL) as UsageLanguage[];

/** A usage panel's tabs: the terminal, then each SDK. */
export type UsageTab = "cli" | UsageLanguage;

// The last tab anybody chose, for the page's life. Module state rather than a
// context: it is a default for the next panel to open on, never shared truth.
let lastUsageTab: UsageTab | null = null;

export function rememberUsageTab(tab: UsageTab): void {
  lastUsageTab = tab;
}

/** The remembered tab, when this panel offers it. */
export function rememberedUsageTab(offered: UsageTab[]): UsageTab | null {
  return lastUsageTab && offered.includes(lastUsageTab) ? lastUsageTab : null;
}

function trimFolder(folder?: string): string {
  return (folder ?? "").replace(/^\/+|\/+$/g, "");
}

/**
 * The terminal half: `toft store`, for a person holding a grant.
 *
 * `access` is the level *here* when the caller knows it — the browser and the
 * account page do — and then only commands that level allows are listed. Without
 * it (an administrator's view of the resource) every command is listed with the
 * level it needs beside it.
 */
export function cliExample({
  name, folder, access,
}: {
  name: string;
  folder?: string;
  access?: StorageAccess;
}): string {
  const f = trimFolder(folder);
  const at = f ? `${name}/${f}` : name;
  const key = (file: string) => (f ? `${f}/${file}` : file);
  const can = access ? storeAffordances(access) : null;
  const needs = (level: string) => (can ? "" : `   # needs ${level}`);

  const lines = [
    `toft store ls ${at}`,
    `toft store get ${name} ${key("report.csv")}`,
    `toft store cp -r toft://${at}/ ./${f ? f.split("/").pop() : name}`,
  ];
  if (!can || can.canUpload) {
    lines.push(`toft store put --as ${key("report.csv")} ${name} ./report.csv${needs("Can add")}`);
    lines.push(`toft store cp -r ./out toft://${at}/${needs("Can add")}`);
  }
  if (!can || can.canDelete) {
    lines.push(`toft store rm ${at}/report.csv${needs("Read & write")}`);
  }
  return lines.join("\n");
}

/**
 * The code half, for an app this resource is attached to.
 *
 * A volume is an ordinary directory, so its example is the language's own file
 * call on the path the SDK resolves. A store is reached through the SDK's
 * transfer calls, which work whether or not it is mounted — the one form that
 * is right on a cluster and on a host install alike.
 */
export function sdkExample({
  name, folder, kind, language,
}: {
  name: string;
  folder?: string;
  kind: StorageKind;
  language: UsageLanguage;
}): string {
  const f = trimFolder(folder);
  const q = JSON.stringify(name);
  const fq = JSON.stringify(f);

  if (kind === "volume") {
    switch (language) {
      case "python":
        return [
          "import toft",
          "",
          `folder = toft.storage(${q}${f ? `, folder=${fq}` : ""})`,
          `with open(folder.path / "notes.txt", "a") as out:   # an ordinary directory`,
          `    out.write("hello\\n")`,
        ].join("\n");
      case "node":
        return [
          `import toft from "toft-sdk";`,
          `import { appendFile } from "node:fs/promises";`,
          `import { join } from "node:path";`,
          "",
          `const folder = toft.storage(${q}${f ? `, { folder: ${fq} }` : ""});`,
          `await appendFile(join(folder.path, "notes.txt"), "hello\\n");`,
        ].join("\n");
      case "r":
        return [
          "library(toft)",
          "",
          `folder <- storage(${q}${f ? `, ${fq}` : ""})`,
          `write("hello", file.path(folder$path, "notes.txt"), append = TRUE)`,
        ].join("\n");
      case "go":
        return [
          `folder, err := toft.${f ? `StorageIn(${q}, ${fq})` : `Storage(${q})`}`,
          "if err != nil {",
          "\treturn err",
          "}",
          // Appends, as the other three languages do — os.WriteFile truncated
          // the file on every call (BB-102).
          `f, err := os.OpenFile(filepath.Join(folder.Path, "notes.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)`,
          "if err != nil {",
          "\treturn err",
          "}",
          "defer f.Close()",
          `_, err = f.WriteString("hello\\n")`,
        ].join("\n");
    }
  }

  switch (language) {
    case "python":
      return [
        "import toft",
        "",
        `store = toft.store(${q}${f ? `, folder=${fq}` : ""})`,
        `store.upload("report.csv")              # a local file in; needs Can add`,
        `data = store.download("report.csv")     # bytes`,
        `page = store.list()                     # .objects, .prefixes`,
      ].join("\n");
    case "node":
      return [
        `import toft from "toft-sdk";`,
        "",
        `const store = toft.store(${q}${f ? `, { folder: ${fq} }` : ""});`,
        `await store.upload("report.csv");          // a local file in; needs Can add`,
        `const data = await store.download("report.csv");`,
        `const page = await store.list();`,
      ].join("\n");
    case "r":
      return [
        "library(toft)",
        "",
        `s <- store(${q}${f ? `, ${fq}` : ""})`,
        `store_upload(s, "report.csv")           # a local file in; needs Can add`,
        `data <- store_download(s, "report.csv")`,
        `page <- store_list(s)`,
      ].join("\n");
    case "go":
      return [
        `s, err := toft.${f ? `StoreIn(${q}, ${fq})` : `Store(${q})`}`,
        "if err != nil {",
        "\treturn err",
        "}",
        `_, err = s.Upload(ctx, "report.csv", "")   // a local file in; needs Can add`,
        `data, err := s.Download(ctx, "report.csv")`,
        `page, err := s.List(ctx, "", "")`,
      ].join("\n");
  }
}

/**
 * What is underneath a resource, as facts an administrator can copy into
 * another tool — never one string doing three jobs.
 *
 * "Address" was one field holding a bucket URI on one backing, a root's *name*
 * joined to the resource's on another (`efs/sales-data`, which is no path on
 * any machine) and a claim name on a third. Each backing now says what it has.
 */
export interface WhereFact {
  label: string;
  value: string;
}

export function resourceWhere(
  res: Pick<StorageResource, "slug" | "backing" | "bucket" | "prefix" | "storage_root" | "storage_class" | "claim_name">,
  drives: StorageDrive[],
  /** False on an install with no cluster, where a store has no binding at all. */
  storesMounted = true,
): WhereFact[] {
  const out: WhereFact[] = [];
  if (res.storage_root) {
    const drive = drives.find((d) => d.key === `root:${res.storage_root}`);
    out.push({ label: "Drive", value: driveTitle(drive, res.storage_root) });
    out.push({
      label: "Directory",
      value: drive?.declared && drive.path
        ? `${drive.path.replace(/\/+$/, "")}/${res.slug}`
        : `${res.slug} on ${res.storage_root}, which this install no longer declares`,
    });
  }
  if (res.bucket) {
    const blob = res.backing === "blob";
    out.push({ label: blob ? "Container" : "Bucket", value: res.bucket });
    out.push({ label: "Prefix", value: res.prefix ?? "" });
    out.push({
      label: "URI",
      value: `${blob ? "az" : "s3"}://${res.bucket}/${res.prefix ? `${res.prefix}/` : ""}`,
    });
  }
  if (!res.storage_root && res.backing !== "mountpoint" && res.backing !== "blob") {
    const drive = classDriveFor(res, drives);
    if (drive) out.push({ label: "Drive", value: driveTitle(drive, drive.name) });
    out.push({ label: "Storage class", value: res.storage_class || drive?.name || "The install’s default" });
  }
  const store = res.backing === "mountpoint" || res.backing === "blob";
  if (!res.storage_root && (!store || storesMounted) && res.claim_name) {
    out.push({ label: "Claim", value: res.claim_name });
  }
  return out;
}

/** The class drive a cluster volume lives on: its own class, or its backing's default. */
export function classDriveFor(
  res: Pick<StorageResource, "backing" | "storage_class">,
  drives: StorageDrive[],
): StorageDrive | undefined {
  if (res.storage_class) return drives.find((d) => d.key === `class:${res.storage_class}`);
  return drives.find(
    (d) => d.kind === "class" && d.default && (!d.backing || d.backing === res.backing),
  );
}

/**
 * A drive as one line: the label an administrator gave it, with the operator's
 * name beside it when they differ, and what is unusual about it.
 */
export function driveTitle(drive: StorageDrive | undefined, name: string): string {
  if (!drive) return `${name} (no longer declared)`;
  const base = drive.label === drive.name ? drive.name : `${drive.label} (${drive.name})`;
  if (!drive.declared) return `${base}, no longer declared`;
  return drive.read_only ? `${base}, read-only` : base;
}

/** The one fact a list row has room for: the URI, else the directory, else the claim. */
export function primaryWhere(
  res: Parameters<typeof resourceWhere>[0],
  drives: StorageDrive[],
  storesMounted = true,
): string {
  const facts = resourceWhere(res, drives, storesMounted);
  for (const label of ["URI", "Directory", "Claim"]) {
    const hit = facts.find((f) => f.label === label);
    if (hit) return hit.value;
  }
  return "";
}

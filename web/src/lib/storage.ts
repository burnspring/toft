import { toneSurface, type Tone } from "@/lib/tone";
import { statusOf } from "@/lib/api";
import type {
  AttachableResource, ExecutionMode, StorageAccess, StorageBacking, StorageKind,
} from "@/lib/api";

/**
 * The vocabulary of durable storage, in one place.
 *
 * These strings carry most of the feature's teaching. An admin choosing between
 * a volume and a store, or between "write" and "full", is making a decision
 * with real consequences for an app's data, and the page has to say what they
 * are — the mount will enforce them either way, loudly, at the app's first
 * wrong call.
 */

export const KIND_LABEL: Record<StorageKind, string> = {
  volume: "Volume",
  store: "Object store",
};

/**
 * What is underneath a resource, in the words an administrator chooses it by.
 *
 * The kind says what the app experiences; the backing says what the operator
 * pointed it at. They were the same answer until a volume could be something
 * other than EFS, and the reason they are two now is the one thing this table
 * has to teach: whether there are objects behind it, which is what decides
 * whether anyone can reach it without mounting it.
 */
export const BACKING_LABEL: Record<StorageBacking, string> = {
  efs: "EFS",
  s3files: "Amazon S3 Files",
  host: "Host mount",
  azurefiles: "Azure Files",
  mountpoint: "S3, via Mountpoint",
  blob: "Azure Blob, via blobfuse2",
};

export const BACKING_SUMMARY: Record<StorageBacking, string> = {
  efs: "An EFS access point. Apps it’s attached to can use it; it can’t be browsed.",
  s3files:
    "A bucket mounted as a filesystem. People you grant can browse and upload too.",
  host: "A directory on a file system mounted on this machine. Attached apps use it; people you grant access can browse it too.",
  // The NFS condition is copy rather than a fourth backing, because the two
  // protocols are the same code path here: Toft cannot see through a
  // mount, so it cannot tell which one the operator chose and must not guess.
  // The condition is real — SMB does not give a single-file embedded database
  // the locks it needs — so it is said where the choice is made.
  azurefiles:
    "An Azure Files share. Use NFS 4.1 if an app keeps SQLite or DuckDB here; SMB can corrupt them.",
  mountpoint: "An object-store prefix. Files are read and written whole.",
  blob:
    "An Azure Blob container prefix. Files are read and written whole; apps get Read only or Read & write, nothing between.",
};

/**
 * A store's backing on an install that never mounts one. The value stays
 * `mountpoint` — it is what a cluster would mount it with — but the word is
 * wrong on a form where nothing will.
 */
export function backingLabel(backing: StorageBacking, storesMounted = true): string {
  if (!storesMounted && backing === "mountpoint") return "S3";
  if (!storesMounted && backing === "blob") return "Azure Blob";
  return BACKING_LABEL[backing];
}

export function backingSummary(backing: StorageBacking, storesMounted = true): string {
  if (!storesMounted && (backing === "mountpoint" || backing === "blob")) {
    return "An object-store prefix. People browse it; attached apps reach it through the SDK.";
  }
  return BACKING_SUMMARY[backing];
}

/**
 * Whether anyone can reach this without mounting it.
 *
 * **The bucket is the test, not the kind — or a host root.** A volume backed
 * by S3 Files is a filesystem *and* a bucket, so it takes grants and has a
 * Files page; so does a directory under a host root, whose bytes the platform
 * carries itself. One backed by EFS on a cluster has neither. The server
 * asks exactly this question, in exactly this way, in three places — so a page
 * that asked about the kind instead would be the place the two disagree, and
 * what somebody meets when they do is a control that refuses.
 */
export function hasDataPlane(res: {
  bucket?: string;
  backing?: StorageBacking;
  storage_root?: string;
}): boolean {
  return Boolean(res.bucket) || (res.backing === "host" && Boolean(res.storage_root));
}

export const KIND_SUMMARY: Record<StorageKind, string> = {
  volume:
    "A real filesystem. Use it for state an app appends to, renames, or locks — SQLite, caches, working files.",
  store:
    "An object-store prefix. Use it for blobs read and written whole — datasets, model weights, reports. No rename, no append.",
};

/** The one-line warning worth repeating wherever a store is chosen. */
export const STORE_CAVEAT =
  "Files are read and written whole: no rename, append or locking. Keep SQLite on a volume.";

/**
 * The ladder in words: three steps, one label each, the same on every kind
 * and every path.
 *
 * **One meaning per token.** `write` is *Can add* — create-only — wherever it
 * is offered, and `full` is *Read & write*. There used to be a fourth step
 * between them, *Can replace*, which withheld deleting a name but not
 * destroying its contents (anything could be replaced with zero bytes), so it
 * read as a safety boundary and was not one. It was merged into `full`.
 *
 * **Each is a noun phrase, never a bare verb.** The top step was once labelled
 * `Delete`, in `danger` red, in a chip beside a mount path — the exact shape of
 * a destructive button. What a step says is what it *allows*, not what pressing
 * it would do. *Read & write* rather than *Can delete*: with nothing between
 * them, what the top step adds over *Can add* is writing in general, and it is
 * what a volume has always called it.
 */
const ACCESS_LABEL: Record<StorageAccess, string> = {
  read: "Read only",
  write: "Can add",
  full: "Read & write",
};

/** A step's name. One table, so a volume and a store cannot disagree. */
export function accessLabel(access: StorageAccess): string {
  return ACCESS_LABEL[access];
}

/** The steps, in ladder order — the ladder's own enumeration for a test. */
export const ACCESS_RUNGS = Object.keys(ACCESS_LABEL) as StorageAccess[];

/**
 * A mapping's kind, from the field a mapping carries it in.
 *
 * `resource_kind` is denormalised onto a mapping for display and is optional on
 * the wire, so every reader had to decide what an absent one meant — and they
 * decided differently, one defaulting to a volume and one to a store. One
 * function, defaulting to **store**, because a store's vocabulary never
 * promises more than it can do while a volume's does.
 */
export function mappingKind(resourceKind?: string): StorageKind {
  return resourceKind === "volume" ? "volume" : "store";
}

/**
 * What each step actually permits. A volume mapping offers no `write`: a
 * filesystem cannot refuse a rename to a process that can write, so a
 * create-only volume mount would be advisory.
 */
export function accessSummary(kind: StorageKind, access: StorageAccess): string {
  if (kind === "volume") {
    return access === "read"
      ? "Mounted read-only. The app cannot modify anything."
      : "Full read and write, as an ordinary filesystem.";
  }
  switch (access) {
    case "read":
      return "Files can be listed and read. Nothing can be written.";
    case "write":
      return "New files only. Existing ones can’t be replaced or removed — the safe default for a dataset an app adds to.";
    case "full":
      return "New files, replacement, and deletion.";
  }
}

/** The levels a kind offers to a mapping or a delegate's ceiling, in ladder order. */
export function accessLevels(kind: StorageKind): StorageAccess[] {
  return kind === "volume" ? ["read", "full"] : ["read", "write", "full"];
}

/**
 * The levels a grant offers, on either kind. A grant is reached over a signed
 * URL or the host data plane, which can refuse an existing name exactly — so
 * a person can be *Can add* on a volume no app can be attached to at that step.
 */
export function grantLevels(): StorageAccess[] {
  return ["read", "write", "full"];
}

/**
 * The levels this backing can actually be *mounted* at — what an attach may
 * choose, which is not always what the ladder has.
 *
 * A narrowing of `accessLevels` and never a widening, differing in exactly one
 * place: an Azure Blob store. blobfuse2 has no allow-overwrite and no
 * allow-delete — it mounts read-only or read-write, with nothing between — so
 * `write` cannot be expressed as a mount there at all. The server
 * refuses them at attach, and this is what stops that refusal being the way
 * somebody finds out.
 *
 * A rendering of the server's answer rather than a second opinion about it,
 * exactly as `levelsUpTo` is for a ceiling. Grants and delegation ceilings keep
 * all three steps: a grant is reached over a signed URL, where the permission
 * *can* be expressed.
 */
export function mountableAccessLevels(
  kind: StorageKind, backing: StorageBacking,
): StorageAccess[] {
  if (kind === "store" && backing === "blob") return ["read", "full"];
  return accessLevels(kind);
}

/**
 * Why a step is missing from an attach, or null when none is.
 *
 * A sentence rather than a disabled item: a disabled option in a select cannot
 * be focused and swallows the pointer, so an explanation attached to one is
 * unreachable by every route. It names the driver, because "this level is not
 * available" invites the reader to look for the switch that would make it
 * available, and there is not one.
 */
export function unmountableLevelsNote(
  kind: StorageKind, backing: StorageBacking,
): string | null {
  if (kind !== "store" || backing !== "blob") return null;
  return "blobfuse2 attaches this storage read-only or read-write, with nothing in between, so Can add isn’t available when attaching it. It still works over a signed URL — grant a person access, or use toft store put.";
}

/**
 * The rungs at or below a ceiling — what a delegate may actually choose.
 *
 * A rendering of the ceiling rather than a second opinion about it: the server
 * refuses anything above it either way, and this is what stops the refusal
 * being the way somebody finds out.
 */
export function levelsUpTo(kind: StorageKind, ceiling: StorageAccess): StorageAccess[] {
  const ladder = accessLevels(kind);
  const top = ladder.indexOf(ceiling);
  return top < 0 ? ladder : ladder.slice(0, top + 1);
}

/**
 * The ladder as a tone: read-only is the calm case and each rung above it can
 * destroy more of somebody's data.
 *
 * `full` is `danger` because it is the one that deletes, and the chip is the
 * only place that fact is visible in a table of mounts. The rungs used to
 * be zinc/sky/amber/rose — a fourth palette nothing else in the product used —
 * which made the ladder look like decoration rather than a warning that gets
 * steeper.
 */
export function accessTone(access: StorageAccess): Tone {
  switch (access) {
    case "read":
      return "neutral";
    case "write":
      return "info";
    case "full":
      return "danger";
  }
}

/** The chip's classes for a rung. */
export function accessBadgeClass(access: StorageAccess): string {
  return toneSurface[accessTone(access)];
}

/**
 * A store's audience, in words.
 *
 * The same phrasing an app's Restricted audience is written in, because "who
 * can reach this" is one question and two phrasings of it read as two facts.
 *
 * **Subjects, not resolved people.** A group of forty counts as one group here,
 * because this number sits beside a dialog that lists rows and a count that
 * disagreed with the list it opens is worse than no count. What the number is
 * for is noticing that a store no app mounts is reachable by six people
 * anyway — a fact the Apps column cannot carry and, until it was printed here,
 * nothing on the page did.
 */
export function audienceSummary(users: number, groups: number): string {
  const parts = [
    users > 0 ? `${users} ${users === 1 ? "person" : "people"}` : null,
    groups > 0 ? `${groups} ${groups === 1 ? "group" : "groups"}` : null,
  ].filter(Boolean);
  return parts.length > 0 ? parts.join(", ") : "Nobody";
}

/**
 * What a rung permits **from outside the platform**, over a signed URL.
 *
 * The same three words, said for a person rather than an app, because the two
 * audiences reach a store differently and only one of them has a filesystem.
 * The ladder is the same ladder — the URL is never more generous than the mount
 * — and saying so twice in the product's own words is how an administrator
 * granting one can predict the other.
 */
export function grantSummary(access: StorageAccess): string {
  switch (access) {
    case "read":
      return "Download and list. Nothing can be uploaded.";
    case "write":
      return "Upload new files. An existing name is refused rather than replaced — the safe default for a nightly that keeps its history.";
    case "full":
      return "Upload, replace, and delete.";
  }
}

/**
 * A grant's folder, as the wire spells it: no leading or trailing slash, and
 * the empty string for the whole store.
 *
 * A grant is identified by its **scope** as much as by its subject — a folder
 * grant and a root grant are separate rows, granted, audited and revoked
 * separately — so every writer of one has to say which folder it lands on.
 * Here rather than in the page because a default folder is what let a row
 * control write at the wrong one: the argument is now explicit at every call
 * site and this is the only place the shape is decided.
 */
export function normalizeFolder(folder: string | null | undefined): string {
  return (folder ?? "").trim().replace(/^\/+|\/+$/g, "");
}

/**
 * What a rung lets the store browser *show*, as one object.
 *
 * The page renders from this and never from a comparison of its own. That is
 * the point: `presignFor` on the server is the ladder, every refusal still
 * comes from there, and this is a rendering of the same three rungs rather than
 * a second copy of them. A page that decided for itself which button to draw
 * would be the place the two quietly disagree — and the disagreement a person
 * meets is a control that does nothing.
 *
 * **Absent, not disabled.** A reader with `read` has no upload button at all,
 * because a greyed-out one is a promise that they could have it, and the way
 * to have it is a grant only an administrator can make.
 */
export interface StoreAffordances {
  /** Download, and the detail panel. Every rung has it. */
  canDownload: boolean;
  /** The upload button, the dropzone, and the New folder button — a folder is
   * only real once something is written into it. */
  canUpload: boolean;
  /** Whether an existing name may be replaced. At `write` a collision this
   * page can see is refused before the mint — the server's `If-None-Match: *`
   * is still the authority, and would otherwise refuse it as a 412 after the
   * whole file had been hashed and pushed. Above it, a collision is a
   * confirmation, because an object store keeps no previous version. */
  canReplace: boolean;
  /** Delete, single and multi-select, and therefore the checkbox column. */
  canDelete: boolean;
}

export function storeAffordances(access: StorageAccess): StoreAffordances {
  return {
    canDownload: true,
    canUpload: access !== "read",
    canReplace: access === "full",
    canDelete: access === "full",
  };
}

/**
 * What "manage" promises, which turns on the data plane, not the kind.
 *
 * On a resource with a data plane — a store, a volume under a host root, an S3
 * Files volume — a delegation also confers that data plane at its ceiling: the
 * holder browses, downloads and uploads exactly as a grant at that level would
 * let them, because there is no second row and no second answer. Without one
 * (an EFS volume) it confers nothing but the ability to attach: there is no
 * browser, no uploads and nothing to sign, so the word has to say so rather
 * than promise a page that does not exist.
 */
export function manageSummary(res: { kind: StorageKind; hasDataPlane: boolean }): string {
  const noun = res.kind === "store" ? "store" : "volume";
  return res.hasDataPlane
    ? `They can attach this ${noun} to an app they can already edit, up to the level you set — and reach its files themselves, at that same level, from the storage browser and the CLI.`
    : `They can attach this ${noun} to an app they can already edit, up to the level you set. That is all: there is nothing behind it to sign for, so the only way to read it is from an app it’s attached to.`;
}

/** What a delegation at this rung lets somebody attach, in one line. */
export function ceilingSummary(kind: StorageKind, access: StorageAccess): string {
  return `They can attach it at ${accessLabel(access)} or below. ${accessSummary(kind, access)}`;
}

/**
 * What an app's Storage section offers this reader, as one object.
 *
 * The same shape and the same reason as `storeAffordances`: the page renders
 * from this rather than deciding for itself, because every refusal still comes
 * from the server and a page that formed a second opinion would be the place
 * the two quietly disagree. What a reader meets when they do is a control that
 * does nothing.
 *
 * **Absent, not disabled.** A reader with no delegation gets no Attach button
 * at all — a greyed-out one is a promise that they could have it, and the way
 * to have it is a delegation only an administrator can make.
 */
export interface AppStorageControls {
  /** The resources this reader may attach here: ones they manage, minus what
   * this app already mounts. */
  attachable: AttachableResource[];
  /** Whether the Attach control appears at all. */
  canAttach: boolean;
  /** The resource ids this reader may detach — exactly the ones they manage,
   * whoever attached them. Somebody who may attach and re-attach in the next
   * request gains nothing from being refused here. */
  detachable: Set<string>;
  /**
   * Whether the attach dialog warns that this app runs shared.
   *
   * Two facts, and it needs both. The app's own mode, read from the app and
   * never from a flag on the storage response, so it cannot say one thing while
   * the app page says another — and whether this install offers shared-mode
   * storage at all, because what a deploy refuses is storage with nowhere to
   * put it rather than storage in shared mode. Warning on the mode alone would
   * be warning about a refusal that is not going to happen.
   *
   * It is a warning and not a refusal: the deploy-time refusal stays the
   * enforcement point, because an administrator legitimately attaches storage
   * and *then* promotes the app, and refusing here would break that order.
   */
  warnsSharedMode: boolean;
}

export function appStorageControls(opts: {
  canEdit: boolean;
  managed: AttachableResource[];
  mountedResourceIds: string[];
  executionMode: ExecutionMode;
  /** Whether this install can attach storage to a shared workload at all. */
  sharedModeStorage?: boolean;
}): AppStorageControls {
  const managed = opts.canEdit ? opts.managed : [];
  const mounted = new Set(opts.mountedResourceIds);
  const attachable = managed.filter((r) => !mounted.has(r.id));
  return {
    attachable,
    canAttach: attachable.length > 0,
    detachable: new Set(managed.map((r) => r.id)),
    warnsSharedMode: opts.executionMode !== "isolated" && !opts.sharedModeStorage,
  };
}

/**
 * The folder a store-browser URL is showing, **slash-terminated or empty**.
 *
 * The browser builds every key by concatenation — an object is `path + name`, a
 * child folder is `path + folder`, an upload lands at `path + relative` — while
 * the router cannot carry the trailing slash, because a route ending in one is
 * a different route. So the two representations meet here and nowhere else. Get
 * this wrong and nothing errors: the page signs a URL over a key one separator
 * short of the object, which downloads nothing and deletes something else.
 */
export function storePathFromWildcard(wildcard: string): string {
  const raw = wildcard.replace(/^\/+|\/+$/g, "");
  return raw === "" ? "" : raw + "/";
}

/**
 * A full key as a row shows it: what is left once the folder being shown has
 * been taken off the front. A flat listing keeps its slashes, because there the
 * remainder *is* the distinguishing part of the name.
 */
export function storeLeafOf(key: string, path: string): string {
  return key.startsWith(path) ? key.slice(path.length) : key;
}

/** What is underneath a resource, in one line: what the app experiences, then
 * what the operator pointed it at. */
export function resourceLocation(res: { kind: StorageKind; backing: StorageBacking }): string {
  return `${KIND_LABEL[res.kind]} · ${BACKING_LABEL[res.backing]}`;
}

/**
 * One term of "who touches this".
 *
 * A count, or — for the one slot that can be *impossible* rather than empty —
 * a note. That distinction is the whole reason this is a type rather than
 * three numbers: a resource with no bucket behind it takes no grants ever, and
 * printing `0` there says "nobody yet" about a thing that will never have
 * anybody. It used to be an em dash with the explanation in a `title`, which
 * no keyboard and no screen reader could reach.
 */
export type ReachTerm =
  | { kind: "count"; count: number; noun: string }
  | { kind: "note"; text: string };

/**
 * Who reaches a resource, as the list shows it: three terms, one grammar, in
 * order of consequence.
 *
 * These were three columns — Apps, People, Delegates — answering one question
 * in four grammars (`4`, `6 people, 2 groups`, `Nobody`, `—`), each a link to
 * a different dialog. Three near-identical link columns is what made the table
 * unreadable and what pushed it off the right edge below 1100px, taking the
 * row's actions menu with it.
 *
 * **Subjects, not resolved people**, as everywhere else: a group of forty is
 * one, because this number sits over a page that lists rows and a count that
 * disagreed with the list it opens is worse than no count.
 */
export function reachTerms(res: {
  mapping_count: number;
  grant_users: number;
  grant_groups: number;
  manager_users: number;
  manager_groups: number;
  bucket?: string;
  backing?: StorageBacking;
  storage_root?: string;
}): ReachTerm[] {
  const plural = (n: number, one: string, many: string) => (n === 1 ? one : many);
  const apps = res.mapping_count;
  const people = res.grant_users + res.grant_groups;
  const delegates = res.manager_users + res.manager_groups;
  return [
    { kind: "count", count: apps, noun: plural(apps, "app", "apps") },
    hasDataPlane(res)
      ? { kind: "count", count: people, noun: plural(people, "person", "people") }
      : { kind: "note", text: "apps only" },
    { kind: "count", count: delegates, noun: plural(delegates, "delegate", "delegates") },
  ];
}

/**
 * Whether a mapping write was refused for *what* it maps — a partitioned
 * resource's folder rules, or an app at its cap — which the attach dialogs say
 * under the folder rather than in a toast. Both are the server's sentences;
 * this only decides where they go.
 */
export function mappingRefusal(err: unknown): boolean {
  const status = statusOf(err);
  return status === 422 || status === 409;
}

export interface StorageFolder {
  folder: string;
  apps: { id: string; name: string }[];
}

/**
 * A partitioned resource's folders, each with the apps on it, from the
 * resource's own mappings. Sorted by folder, so `teams/a` and `teams/b` sit
 * together; an app mapping one folder at two paths is listed once.
 */
export function storageFolders(
  mappings: { folder?: string; app_id: string; app_name?: string }[],
): StorageFolder[] {
  const byFolder = new Map<string, Map<string, string>>();
  for (const m of mappings) {
    const folder = m.folder ?? "";
    const apps = byFolder.get(folder) ?? new Map<string, string>();
    apps.set(m.app_id, m.app_name || m.app_id);
    byFolder.set(folder, apps);
  }
  return [...byFolder.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([folder, apps]) => ({
      folder,
      apps: [...apps.entries()]
        .map(([id, name]) => ({ id, name }))
        .sort((a, b) => a.name.localeCompare(b.name)),
    }));
}

/**
 * Whether deleting this resource removes its files, from the drive it lives
 * on — the server's verdict, never the backing's name (ADR §12a). A store is a
 * prefix in a bucket the operator lent and is never emptied. A volume naming no
 * class is on its backing's default class.
 */
export function deleteRemovesData(
  res: { kind: StorageKind; backing: StorageBacking; storage_root?: string; storage_class?: string },
  drives: { key: string; kind: "root" | "class"; default?: boolean; backing?: StorageBacking; delete_removes_data?: boolean }[],
): boolean {
  if (res.kind === "store") return false;
  if (res.storage_root) return drives.find((d) => d.key === `root:${res.storage_root}`)?.delete_removes_data ?? false;
  if (res.storage_class) return drives.find((d) => d.key === `class:${res.storage_class}`)?.delete_removes_data ?? false;
  const fallback = drives.find((d) => d.kind === "class" && d.default && (!d.backing || d.backing === res.backing));
  return fallback?.delete_removes_data ?? false;
}

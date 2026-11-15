import { describe, expect, it } from "vitest";

import {
  ACCESS_RUNGS,
  accessLabel,
  accessLevels,
  appStorageControls,
  deleteRemovesData,
  grantLevels,
  grantSummary,
  hasDataPlane,
  levelsUpTo,
  mountableAccessLevels,
  normalizeFolder,
  reachTerms,
  resourceLocation,
  storeAffordances,
  storeLeafOf,
  storePathFromWildcard,
  storageFolders,
  unmountableLevelsNote,
  BACKING_LABEL,
  backingLabel,
  backingSummary,
} from "./storage";
import type { AttachableResource } from "./api";

/**
 * The affordance table of the store browser's design, asserted rung by rung.
 *
 * It reads the levels from `ACCESS_RUNGS` rather than listing them, so a fifth
 * rung added to the ladder fails here — loudly, once — instead of rendering as
 * a store whose every control is quietly absent because nothing in the page
 * knew the word.
 */
const RUNGS = ACCESS_RUNGS;

describe("accessLabel", () => {
  it("names every step", () => {
    for (const access of RUNGS) {
      expect(accessLabel(access)).toBeTruthy();
    }
  });

  /** One label per token, so a volume and a store say the same thing. */
  it("is one table", () => {
    expect(accessLabel("read")).toBe("Read only");
    expect(accessLabel("write")).toBe("Can add");
    expect(accessLabel("full")).toBe("Read & write");
  });
});

describe("accessLevels and grantLevels", () => {
  it("offers a volume mapping no create-only step", () => {
    // A filesystem cannot refuse a rename to a process that can write.
    expect(accessLevels("volume")).toEqual(["read", "full"]);
    expect(accessLevels("store")).toEqual(["read", "write", "full"]);
  });

  it("offers a grant every step, on either kind", () => {
    // The host data plane and a presigned PUT refuse an existing name exactly.
    expect(grantLevels()).toEqual(RUNGS);
  });
});

describe("storeAffordances", () => {
  it("covers every rung on the ladder", () => {
    expect(RUNGS).toEqual(["read", "write", "full"]);
    for (const access of RUNGS) {
      expect(storeAffordances(access)).toBeDefined();
      expect(grantSummary(access).length).toBeGreaterThan(8);
    }
  });

  it("is the ladder and not a second opinion about it", () => {
    expect(storeAffordances("read")).toEqual({
      canDownload: true,
      canUpload: false,
      canReplace: false,
      canDelete: false,
    });
    expect(storeAffordances("write")).toEqual({
      canDownload: true,
      canUpload: true,
      canReplace: false,
      canDelete: false,
    });
    expect(storeAffordances("full")).toEqual({
      canDownload: true,
      canUpload: true,
      canReplace: true,
      canDelete: true,
    });
  });

  it("never widens as it goes down the ladder", () => {
    // The monotonic property, which is the one a new rung could break without
    // any single line looking wrong: each rung permits everything the one below
    // it does. The URL is never more generous than the mount, and the page is
    // never more generous than the URL.
    const rank = RUNGS.map(storeAffordances);
    for (let i = 1; i < rank.length; i++) {
      for (const key of ["canDownload", "canUpload", "canReplace", "canDelete"] as const) {
        if (rank[i - 1][key]) {
          expect(rank[i][key], `${RUNGS[i]} lost ${key} that ${RUNGS[i - 1]} had`).toBe(true);
        }
      }
    }
  });

  it("replaces only where it may also delete", () => {
    // `write` is create-only: a collision is refused before the mint, never
    // offered as a replace the server's If-None-Match would then refuse.
    expect(storeAffordances("write").canReplace).toBe(false);
    expect(storeAffordances("full").canReplace).toBe(true);
  });
});

/**
 * A delegate's controls on an app's Storage section.
 *
 * The two properties the design rests on: nothing appears for somebody with no
 * delegation, and the shared-mode warning comes from the app's own mode rather
 * than from a flag the storage response could disagree with.
 */
describe("appStorageControls", () => {
  const resource = (id: string): AttachableResource => ({
    id,
    slug: id,
    name: id,
    kind: "store",
    description: "",
    partitioned: false,
    max_access: "write",
    authority: "user:u1",
  });

  it("offers nothing to a caller with no delegation", () => {
    const c = appStorageControls({
      canEdit: true, managed: [], mountedResourceIds: ["r1"], executionMode: "isolated",
    });
    expect(c.canAttach).toBe(false);
    expect(c.attachable).toEqual([]);
    // And no Detach either: a mount somebody else made is a fact on this page,
    // not a control.
    expect(c.detachable.has("r1")).toBe(false);
  });

  it("offers nothing to a reader who cannot edit the app", () => {
    // Both halves are required. Managing the resource is not enough — the app
    // half is the same authorization that governs every other change to it.
    const c = appStorageControls({
      canEdit: false, managed: [resource("r1")], mountedResourceIds: [], executionMode: "isolated",
    });
    expect(c.canAttach).toBe(false);
    expect(c.detachable.size).toBe(0);
  });

  it("does not offer to attach what the app already mounts", () => {
    const c = appStorageControls({
      canEdit: true,
      managed: [resource("r1"), resource("r2")],
      mountedResourceIds: ["r1"],
      executionMode: "isolated",
    });
    expect(c.attachable.map((r) => r.id)).toEqual(["r2"]);
    // Detaching is the other direction: the mounted one is exactly the one
    // there is something to do with.
    expect(c.detachable.has("r1")).toBe(true);
  });

  it("warns about shared mode from the app's mode, and only warns", () => {
    const shared = appStorageControls({
      canEdit: true, managed: [resource("r1")], mountedResourceIds: [], executionMode: "shared",
    });
    expect(shared.warnsSharedMode).toBe(true);
    // A warning, not a refusal: an administrator legitimately attaches storage
    // and then promotes the app, and refusing here would break that order.
    expect(shared.canAttach).toBe(true);

    const isolated = appStorageControls({
      canEdit: true, managed: [resource("r1")], mountedResourceIds: [], executionMode: "isolated",
    });
    expect(isolated.warnsSharedMode).toBe(false);

    // And it does not warn where there is nothing to warn about: what a deploy
    // refuses is storage with nowhere to put it, so an install that declares a
    // host mount attaches this to a shared app and starts it.
    const withRoots = appStorageControls({
      canEdit: true, managed: [resource("r1")], mountedResourceIds: [],
      executionMode: "shared", sharedModeStorage: true,
    });
    expect(withRoots.warnsSharedMode).toBe(false);
    expect(withRoots.canAttach).toBe(true);
  });
});

/** A delegate never sees a rung above their ceiling in the picker. */
describe("levelsUpTo", () => {
  it("stops at the ceiling", () => {
    expect(levelsUpTo("store", "write")).toEqual(["read", "write"]);
    expect(levelsUpTo("store", "full")).toEqual(["read", "write", "full"]);
    expect(levelsUpTo("volume", "full")).toEqual(["read", "full"]);
    expect(levelsUpTo("volume", "read")).toEqual(["read"]);
  });

  it("falls back to the whole ladder on a level the kind does not have", () => {
    // `write` is not on a volume's ladder. The server refuses such a
    // delegation and a trigger refuses it underneath, so this is the
    // unreachable case — and it must not silently render an empty picker with
    // no explanation.
    expect(levelsUpTo("volume", "write")).toEqual(["read", "full"]);
  });
});

describe("storePathFromWildcard", () => {
  it("is empty at the store root", () => {
    expect(storePathFromWildcard("")).toBe("");
    expect(storePathFromWildcard("/")).toBe("");
  });

  it("terminates every folder with a slash, whatever the URL carried", () => {
    // The router never gives the trailing slash and may give a leading one;
    // both have to land on the same string, because that string is what every
    // key on the page is built by concatenating onto.
    for (const wildcard of ["reports", "/reports", "reports/", "/reports/"]) {
      expect(storePathFromWildcard(wildcard)).toBe("reports/");
    }
    expect(storePathFromWildcard("reports/2026")).toBe("reports/2026/");
  });

  it("makes path + name a whole key", () => {
    const path = storePathFromWildcard("/reports/2026");
    expect(path + "q1.csv").toBe("reports/2026/q1.csv");
  });
});

describe("storeLeafOf", () => {
  it("takes the shown folder off the front", () => {
    expect(storeLeafOf("reports/2026/q1.csv", "reports/2026/")).toBe("q1.csv");
    expect(storeLeafOf("q1.csv", "")).toBe("q1.csv");
  });

  it("keeps the slashes a flat listing depends on", () => {
    expect(storeLeafOf("reports/2026/q1.csv", "reports/")).toBe("2026/q1.csv");
  });

  it("leaves a key that is not under the folder alone", () => {
    expect(storeLeafOf("elsewhere/q1.csv", "reports/")).toBe("elsewhere/q1.csv");
  });
});

describe("resourceLocation", () => {
  it("says what the app experiences, then what the operator pointed it at", () => {
    expect(resourceLocation({ kind: "store", backing: "mountpoint" }))
      .toBe("Object store · S3, via Mountpoint");
    expect(resourceLocation({ kind: "volume", backing: "efs" })).toBe("Volume · EFS");
    expect(resourceLocation({ kind: "store", backing: "blob" }))
      .toBe("Object store · Azure Blob, via blobfuse2");
    expect(resourceLocation({ kind: "volume", backing: "azurefiles" }))
      .toBe("Volume · Azure Files");
  });
});

/**
 * The two rungs an Azure Blob store cannot be mounted at.
 *
 * This is a rendering of the server's refusal, so what it has to keep is the
 * direction: a select offering a level the attach refuses is how somebody finds
 * out by being refused, and a select offering *more* than the backing can mount
 * is the same bug pointing the other way. Every other backing keeps its ladder
 * whole, which is what makes this one narrowing rather than a new rule.
 */
describe("mountableAccessLevels", () => {
  it("drops the two rungs blobfuse2 has no flag for", () => {
    expect(mountableAccessLevels("store", "blob")).toEqual(["read", "full"]);
  });

  it("leaves every other backing's ladder alone", () => {
    expect(mountableAccessLevels("store", "mountpoint")).toEqual(accessLevels("store"));
    expect(mountableAccessLevels("volume", "efs")).toEqual(accessLevels("volume"));
    expect(mountableAccessLevels("volume", "azurefiles")).toEqual(accessLevels("volume"));
    expect(mountableAccessLevels("volume", "host")).toEqual(accessLevels("volume"));
  });

  it("never offers a level the ladder does not have", () => {
    for (const backing of ["mountpoint", "blob"] as const) {
      for (const level of mountableAccessLevels("store", backing)) {
        expect(accessLevels("store")).toContain(level);
      }
    }
  });
});

describe("unmountableLevelsNote", () => {
  it("explains the gap where there is one, naming the driver", () => {
    const note = unmountableLevelsNote("store", "blob");
    expect(note).toContain("blobfuse2");
    expect(note).toContain("signed URL");
  });

  it("says nothing where the ladder is whole", () => {
    expect(unmountableLevelsNote("store", "mountpoint")).toBeNull();
    expect(unmountableLevelsNote("volume", "azurefiles")).toBeNull();
  });
});

describe("reachTerms", () => {
  const base = {
    mapping_count: 0, grant_users: 0, grant_groups: 0, manager_users: 0, manager_groups: 0,
  };

  it("counts subjects, so a group of forty is one", () => {
    const terms = reachTerms({ ...base, bucket: "acme", mapping_count: 3, grant_users: 6, grant_groups: 2 });
    expect(terms).toEqual([
      { kind: "count", count: 3, noun: "apps" },
      { kind: "count", count: 8, noun: "people" },
      { kind: "count", count: 0, noun: "delegates" },
    ]);
  });

  it("agrees with itself in the singular", () => {
    const terms = reachTerms({ ...base, bucket: "acme", mapping_count: 1, grant_users: 1, manager_groups: 1 });
    expect(terms.map((t) => (t.kind === "count" ? t.noun : t.text)))
      .toEqual(["app", "person", "delegate"]);
  });

  it("says apps-only rather than zero where a grant is impossible", () => {
    // Zero would read as "nobody yet" about a resource nobody can ever be
    // given: an EFS access point has no objects to sign for.
    const terms = reachTerms({ ...base, mapping_count: 4, manager_users: 2 });
    expect(terms[1]).toEqual({ kind: "note", text: "apps only" });
  });

  it("counts delegates on a volume, which very much has them", () => {
    const terms = reachTerms({ ...base, manager_users: 2, manager_groups: 1 });
    expect(terms[2]).toEqual({ kind: "count", count: 3, noun: "delegates" });
  });
});

describe("normalizeFolder", () => {
  // A grant is identified by its scope as much as by its subject, so the shape
  // of that scope is decided in one place. The row control on Admin → Storage
  // used to read the *add form's* folder through a closure: changing somebody's
  // level on `finance/` while the form sat empty wrote a root grant — Write on
  // the whole store — and toasted success.
  it("is the empty string for the whole store", () => {
    expect(normalizeFolder("")).toBe("");
    expect(normalizeFolder(null)).toBe("");
    expect(normalizeFolder(undefined)).toBe("");
    expect(normalizeFolder("   ")).toBe("");
    expect(normalizeFolder("/")).toBe("");
  });

  it("carries no leading or trailing slash", () => {
    expect(normalizeFolder("finance")).toBe("finance");
    expect(normalizeFolder("/finance/")).toBe("finance");
    expect(normalizeFolder("//finance//")).toBe("finance");
    expect(normalizeFolder(" finance/2026/ ")).toBe("finance/2026");
  });

  it("does not fold a folder into the root", () => {
    // The failure this guards: any normalization that answered "" for a real
    // folder would widen the grant to the whole store.
    expect(normalizeFolder("finance/2026")).not.toBe("");
  });
});

// A store on an install that never mounts one keeps its backing value, and the
// form stops naming the driver it will not be mounted with.
describe("backingLabel on an install that never mounts a store", () => {
  it("names the object store, not the mount driver", () => {
    expect(backingLabel("mountpoint", false)).toBe("S3");
    expect(backingLabel("blob", false)).toBe("Azure Blob");
    expect(backingLabel("mountpoint")).toBe(BACKING_LABEL.mountpoint);
    expect(backingLabel("efs", false)).toBe(BACKING_LABEL.efs);
    expect(backingSummary("mountpoint", false)).toMatch(/SDK/);
  });
});

/**
 * A partitioned resource's page lists its folders with the apps on each. Two
 * apps on one folder are one row — that is what sharing a folder is — and an
 * app holding one folder at two paths is named once.
 */
describe("storageFolders", () => {
  it("groups the mappings by folder, sorted, each app once", () => {
    expect(
      storageFolders([
        { folder: "teams/b", app_id: "2", app_name: "Billing" },
        { folder: "teams/a", app_id: "1", app_name: "Reports" },
        { folder: "teams/a", app_id: "2", app_name: "Billing" },
        { folder: "teams/a", app_id: "1", app_name: "Reports" },
      ]),
    ).toEqual([
      { folder: "teams/a", apps: [{ id: "2", name: "Billing" }, { id: "1", name: "Reports" }] },
      { folder: "teams/b", apps: [{ id: "2", name: "Billing" }] },
    ]);
  });
});

/**
 * A directory under a host root has a data plane — the platform carries its
 * bytes — and one on a cluster's EFS still does not.
 */
describe("hasDataPlane", () => {
  it("is a bucket, or a host root", () => {
    expect(hasDataPlane({ bucket: "acme" })).toBe(true);
    expect(hasDataPlane({ backing: "host", storage_root: "data" })).toBe(true);
    expect(hasDataPlane({ backing: "host" })).toBe(false);
    expect(hasDataPlane({ backing: "efs" })).toBe(false);
  });
});

describe("deleteRemovesData", () => {
  const drives = [
    { key: "root:data", kind: "root" as const, delete_removes_data: true },
    { key: "root:frozen", kind: "root" as const, delete_removes_data: false },
    { key: "class:efs-toft", kind: "class" as const, default: true, backing: "efs" as const, delete_removes_data: true },
    { key: "class:ord-efs", kind: "class" as const, backing: "efs" as const, delete_removes_data: false },
  ];

  it("reads the drive a resource is on, never its backing", () => {
    expect(deleteRemovesData({ kind: "volume", backing: "host", storage_root: "data" }, drives)).toBe(true);
    expect(deleteRemovesData({ kind: "volume", backing: "host", storage_root: "frozen" }, drives)).toBe(false);
    expect(deleteRemovesData({ kind: "volume", backing: "efs", storage_class: "ord-efs" }, drives)).toBe(false);
  });

  it("puts a volume naming no class on its backing's default", () => {
    expect(deleteRemovesData({ kind: "volume", backing: "efs" }, drives)).toBe(true);
    expect(deleteRemovesData({ kind: "volume", backing: "azurefiles" }, drives)).toBe(false);
  });

  it("never empties a store, and says no about a drive it cannot find", () => {
    expect(deleteRemovesData({ kind: "store", backing: "mountpoint" }, drives)).toBe(false);
    expect(deleteRemovesData({ kind: "volume", backing: "host", storage_root: "gone" }, drives)).toBe(false);
  });
});

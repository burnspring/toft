import { api } from "@/lib/api";
import { TransferQueue, sendSigned } from "@/lib/store-transfer";

/**
 * The store's verbs, bound to this install's API.
 *
 * `store-transfer.ts` is deliberately API-free — its queue takes the mint as a
 * hook so the state machine can be tested with no network and no worker. This
 * is where that hook is filled in, and it is a module rather than a component
 * so that the queue outlives every route.
 */

/**
 * The upload queue, one per tab, above every route.
 *
 * Module-level rather than page state because a person who starts a four-
 * gigabyte upload and then goes to look at an app has not asked to cancel it.
 * Closing or reloading the tab does cancel — nothing else can be true — and the
 * tray says so while something is running.
 */
export const storeUploads = new TransferQueue({
  mint: async (req, digests) => {
    const signed = await api.signStoreUpload(req.storeSlug, {
      name: req.name,
      size: req.file.size,
      sha256: digests.sha256,
      md5: digests.md5,
    });
    return { url: signed.url, method: signed.method, headers: signed.headers };
  },
});

/**
 * One action deletes at most this many objects.
 *
 * Each one is a mint, a cross-origin DELETE and an `audit_log` row, so a
 * selection of ten thousand is a request to build a bulk route rather than to
 * loop harder in a tab.
 */
export const MAX_DELETE_PER_ACTION = 100;

/**
 * Spend one signed DELETE.
 *
 * The platform mints it and the browser spends it, which is the same shape as
 * every other verb here: the fourth rung of the ladder is expressed once, as a
 * condition on a URL, and nothing about deletion goes through the control
 * plane.
 */
export async function deleteObject(slug: string, name: string): Promise<void> {
  const signed = await api.signStoreDelete(slug, name);
  await sendSigned({ url: signed.url, method: signed.method, headers: signed.headers }, null);
}

/**
 * Hand a presigned GET to the browser as a download.
 *
 * A temporary `<a download>` rather than `window.location`: a navigation that
 * failed would replace the SPA with the object store's own error document, and
 * the person would be looking at XML where their app used to be. The
 * disposition is asked for at the mint, so the file is saved under the object's
 * own name rather than the last path segment of a signed URL.
 */
export async function downloadObject(slug: string, name: string): Promise<string> {
  const signed = await api.signStoreDownload(slug, name, true);
  const leaf = name.split("/").pop() ?? name;
  const a = document.createElement("a");
  a.href = signed.url;
  a.rel = "noopener";
  a.download = leaf;
  document.body.appendChild(a);
  a.click();
  a.remove();
  return leaf;
}

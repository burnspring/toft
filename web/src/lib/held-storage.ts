import { ApiError, refusalCodeOf } from "@/lib/api";
import type { HeldStorageMapping } from "@/lib/api";

/** The mappings a user or group delete was refused over, out of the 409's
 *  body — or null when the refusal was something else. */
export function heldStorageMappings(error: unknown): HeldStorageMapping[] | null {
  if (refusalCodeOf(error) !== "holds_storage_mappings" || !(error instanceof ApiError)) return null;
  const list = error.body["mappings"];
  return Array.isArray(list) ? (list as HeldStorageMapping[]) : [];
}

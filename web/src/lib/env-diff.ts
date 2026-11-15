import type { AppEnvVar, UpdateAppEnvRequest } from "@/lib/api";

/** One row of the Settings tab's environment form, as the diff reads it. */
export interface EnvFormRow {
  key: string;
  value: string;
  sensitive: boolean;
  /** The stored key this row was seeded from; absent for a row the person added. */
  seededKey?: string;
}

/**
 * The Settings form as a merge body for `PATCH /apps/{id}/env`: only what
 * changed, because a key the body does not name is left alone.
 *
 * - a stored key with no row → `unset`;
 * - a new key, an edited value or a flipped flag → `set`;
 * - a stored sensitive row left blank → not mentioned, which is how the form
 *   says "keep" now that "" is a real value (it clears).
 *
 * A sensitive row made visible with no value typed is sent without a value,
 * which the install refuses: a stored secret is never revealed by its flag.
 * Returns null when nothing changed.
 */
export function envChanges(stored: AppEnvVar[], rows: EnvFormRow[]): UpdateAppEnvRequest | null {
  const byKey = new Map(stored.map((v) => [v.key, v]));
  const set: UpdateAppEnvRequest["set"] = {};
  const kept = new Set<string>();
  for (const row of rows) {
    const key = row.key.trim();
    if (!key) continue;
    kept.add(key);
    const was = byKey.get(key);
    if (!was) {
      set[key] = { value: row.value, sensitive: row.sensitive };
      continue;
    }
    if (was.sensitive) {
      if (row.value !== "") set[key] = { value: row.value, sensitive: row.sensitive };
      else if (!row.sensitive) set[key] = { sensitive: false };
      continue;
    }
    if (row.value !== (was.value ?? "") || row.sensitive) {
      set[key] = { value: row.value, sensitive: row.sensitive };
    }
  }
  const unset = stored.map((v) => v.key).filter((k) => !kept.has(k));
  if (Object.keys(set).length === 0 && unset.length === 0) return null;
  return { set, unset };
}

/**
 * The server variables a dirty form diffs against, grown by the keys the
 * server gained since it was seeded.
 *
 * The form diffs against what it was seeded from, never the live state: a row
 * the person did not touch then equals its snapshot and is not mentioned,
 * whatever another writer did to that key meanwhile — `PATCH /apps/{id}/env`
 * is a merge so two writers of different keys cannot lose each other's edit.
 * A key the seed already holds keeps the seed's entry; a key it lacks is
 * added, so the form can show it.
 */
export function envSnapshotGrown(seed: AppEnvVar[], server: AppEnvVar[]): AppEnvVar[] {
  const have = new Set(seed.map((v) => v.key));
  return [...seed, ...server.filter((v) => !have.has(v.key))];
}

/**
 * Rows renamed from a stored hidden variable with no value typed, by their new
 * name.
 *
 * The form never holds a hidden value, so such a rename would send an empty
 * value under the new name and unset the old one: the secret is gone. A save
 * is refused until the value is typed again. A visible row locked in the form
 * is not one of these — the form holds its value — so the test is the stored
 * variable's flag, not the row's.
 */
export function hiddenRenames(stored: AppEnvVar[], rows: EnvFormRow[]): string[] {
  const byKey = new Map(stored.map((v) => [v.key, v]));
  const out: string[] = [];
  for (const row of rows) {
    if (row.seededKey === undefined) continue;
    const was = byKey.get(row.seededKey);
    if (!was || !was.sensitive || was.unset) continue;
    const key = row.key.trim();
    if (key === row.seededKey || row.value !== "") continue;
    out.push(key);
  }
  return out;
}

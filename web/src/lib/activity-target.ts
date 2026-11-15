/**
 * What an Activity row's Target cell says, and where it goes.
 *
 * An app is linked by id (GET /apps/{id} resolves ids, not slugs) — unless it
 * has since been deleted, when the link is a door onto a 404. The server flags
 * those rows (`target_gone`) and names them by the slug their deletion
 * recorded; when even that is missing the label is the bare id, which names
 * nothing a reader can place, so it reads "Deleted app" — the counterpart of
 * the "Deleted account" this product already writes.
 */
export interface ActivityTargetRow {
  target_type: string;
  target_id: string;
  target_label: string;
  target_gone?: boolean;
}

export function activityTarget(row: ActivityTargetRow): { text: string; href?: string; gone: boolean } {
  const label = row.target_label || row.target_id;
  if (row.target_type === "app" && row.target_id) {
    if (row.target_gone) {
      return { text: label === row.target_id ? "Deleted app" : label, gone: true };
    }
    return { text: label, href: `/apps/${row.target_id}`, gone: false };
  }
  return { text: label, gone: false };
}

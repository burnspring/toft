import type { AppWithMetrics, User } from "@/lib/api";
import { systemRoleLabel } from "@/lib/utils";
import { appShareUrl } from "@/lib/app-url";

/**
 * Build and download an .xlsx from row objects; keys become the header row in
 * key order. The library loads on demand so it stays out of the main bundle.
 */
export async function exportRowsToXlsx(
  rows: Record<string, string | number>[],
  sheetName: string,
  filename: string,
) {
  const XLSX = await import("xlsx");
  const sheet = XLSX.utils.json_to_sheet(rows);
  const book = XLSX.utils.book_new();
  XLSX.utils.book_append_sheet(book, sheet, sheetName);
  XLSX.writeFile(book, filename);
}

/** Export the user directory as the People page currently filters it. */
export function usersToXlsx(users: User[]) {
  const stamp = new Date().toISOString().slice(0, 10);
  return exportRowsToXlsx(
    users.map((u) => ({
      Name: u.name || u.login,
      Login: u.login,
      Email: u.email,
      Role: systemRoleLabel(u.system_role),
      Status: u.locked_at ? "Locked" : "Active",
      Joined: u.created_at.slice(0, 10),
      "Last sign-in": u.last_login_at ? u.last_login_at.slice(0, 10) : "Never",
      // Both, and in this order, because the pair is the point: an account
      // whose last sign-in is a year ago and whose last use was yesterday is
      // a live automation, and a review that saw only the first column would
      // recommend removing it. This sheet is what leaves the building at
      // audit time, so it carries the fact that makes the other one readable.
      "Last used": u.last_active_at ? u.last_active_at.slice(0, 10) : "",
      "Last API key use": u.last_key_use_at ? u.last_key_use_at.slice(0, 10) : "Never",
    })),
    "Users",
    `toft-users-${stamp}.xlsx`,
  );
}

/**
 * Export the estate as the Apps page currently filters it.
 *
 * The columns are the ones somebody takes off this platform to work on
 * elsewhere: what it is, whether it is running, who answers for it, and where
 * it is. Owner is here whatever the viewport did with the column — the table
 * drops it on a narrow screen and a spreadsheet has no breakpoints.
 *
 * `ownerLabel` is the caller's, because an owner is a **principal**: a user or
 * a group, and resolving a group's name means a list this file has no business
 * fetching. The page already holds it for the Owner column, so it passes what
 * it rendered rather than this making a second, differently-worded answer.
 *
 * Framework is the app's own word and is blank for an app that has never
 * built. That blank is the honest answer and worth keeping: a bulk import
 * whose builds all failed is a column of blanks, which is the fact the sheet
 * exists to carry.
 */
export function appsToXlsx(
  apps: AppWithMetrics[],
  ownerLabel: (app: AppWithMetrics) => string,
) {
  const stamp = new Date().toISOString().slice(0, 10);
  return exportRowsToXlsx(
    apps.map((a) => ({
      Name: a.name,
      Slug: a.slug,
      // Empty for a row held at the `list` rung, which carries no status.
      Status: a.locked_at ? "Locked" : (a.status ?? ""),
      Framework: a.framework ?? "",
      Mode: a.execution_mode ?? "",
      Visibility: a.visibility ?? "",
      Owner: ownerLabel(a),
      URL: appShareUrl(a),
      // The serving release's date, like the column this export mirrors.
      // `updated_at` moves on every status transition, so the spreadsheet
      // somebody takes to a meeting said an app deployed on the day it last
      // crashed.
      "Last deployed": a.last_deployed_at ? a.last_deployed_at.slice(0, 10) : "",
    })),
    "Apps",
    `toft-apps-${stamp}.xlsx`,
  );
}

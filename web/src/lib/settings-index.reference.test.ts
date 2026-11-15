import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import { SETTINGS_INDEX, settingPageLabel, type SettingEntry } from "./settings-index";

/* ------------------------------------------------------------------ *
 * The admin guide's settings reference
 *
 * The page lists every install-wide setting by hand, grouped under the
 * admin page that renders it, with a Range column the index does not hold
 * — so it is checked, not generated (open-gaps PRD D12). Its ids, the
 * heading each sits under and the counts in its prose must be the index's.
 * The page is in the toft-docs checkout; scripts/check-reference-pages.sh
 * points TOFT_DOCS_DIR at it, and without it the real-page test skips.
 * ------------------------------------------------------------------ */

const ROW = /^\| `([a-z0-9_]+)` \|/;
const COUNT = /All (\d+) install-wide settings|There are \*\*(\d+)\*\*/g;

function headingFor(entry: SettingEntry): string {
  return `Admin → ${settingPageLabel(entry.route)}`;
}

/** Every way the page differs from the index, each with what to write. */
function settingsPageProblems(
  page: string,
  index: readonly SettingEntry[],
): string[] {
  const listed = new Map<string, string>();
  const ranged = new Set<string>();
  let heading = "";
  for (const line of page.split("\n")) {
    if (line.startsWith("## ")) heading = line.slice(3).trim();
    else if (line.startsWith("| Setting |") && line.includes("| Range |")) ranged.add(heading);
    else if (heading.startsWith("Admin → ")) {
      const m = ROW.exec(line);
      if (m) listed.set(m[1], heading);
    }
  }

  const problems: string[] = [];
  const missing = new Map<string, string[]>();
  for (const entry of index) {
    const want = headingFor(entry);
    const at = listed.get(entry.id);
    if (at === undefined) {
      const row = `| \`${entry.id}\` | ${entry.label} | ${entry.summary} |${ranged.has(want) ? " — |" : ""}`;
      missing.set(want, [...(missing.get(want) ?? []), row]);
    } else if (at !== want) {
      problems.push(`\`${entry.id}\` is under "${at}"; it is rendered on "${want}"`);
    }
  }
  for (const [heading, rows] of missing) {
    problems.push(`missing under "## ${heading}":\n${rows.join("\n")}`);
  }
  const ids = new Set(index.map((e) => e.id));
  for (const id of listed.keys()) {
    if (!ids.has(id)) problems.push(`\`${id}\` is on the page and not in the index; delete its row`);
  }
  for (const m of page.matchAll(COUNT)) {
    const n = Number(m[1] ?? m[2]);
    if (n !== index.length) problems.push(`"${m[0]}": the index has ${index.length}`);
  }
  return problems;
}

describe("the settings reference page", () => {
  const dir = process.env.TOFT_DOCS_DIR;

  it.skipIf(!dir)("lists exactly the settings index", () => {
    const page = readFileSync(
      join(dir ?? "", "src/content/docs/admin/reference/settings.md"),
      "utf8",
    );
    expect(settingsPageProblems(page, SETTINGS_INDEX)).toEqual([]);
  });

  const entry = (id: string, route: string): SettingEntry =>
    ({ ...SETTINGS_INDEX[0], id, label: `Label ${id}`, summary: `Does ${id}.`, route }) as SettingEntry;
  const index = [entry("log_level", "/admin/settings"), entry("api_keys_enabled", "/admin/api-keys")];
  const settings = headingFor(index[0]);
  const keys = headingFor(index[1]);

  it("prints a missing row, under its heading, and a stale count", () => {
    const page = [
      "description: All 1 install-wide settings.",
      "There are **1**.",
      `## ${settings}`,
      "| Setting | Label | What it does | Range |",
      "| `log_level` | Level | Logs. | — |",
      `## ${keys}`,
      "| Setting | Label | What it does |",
    ].join("\n");
    const got = settingsPageProblems(page, index).join("\n");
    expect(got).toContain(`missing under "## ${keys}":\n| \`api_keys_enabled\` | Label api_keys_enabled | Does api_keys_enabled. |`);
    expect(got).toContain(`"All 1 install-wide settings": the index has 2`);
    expect(got).toContain(`"There are **1**": the index has 2`);
  });

  it("catches a setting under the wrong page", () => {
    const page = [
      "There are **2**.",
      `## ${settings}`,
      "| `log_level` | Level | Logs. | — |",
      "| `api_keys_enabled` | Keys | Keys. | — |",
    ].join("\n");
    expect(settingsPageProblems(page, index)).toEqual([
      `\`api_keys_enabled\` is under "${settings}"; it is rendered on "${keys}"`,
    ]);
  });
});

import type { SystemCheck, SystemCheckReport } from "@/lib/api";
import { SETTINGS_INDEX, settingHref } from "@/lib/settings-index";
import { formatTimestamp } from "@/lib/time";

/**
 * The checks page's logic, out of the page.
 *
 * Three things live here rather than in `checks.tsx`: the severity sort, how a
 * row's destination is resolved, and what the report looks like as plain text.
 * All three are decisions with edges — an unknown setting key, a row that is a
 * fact rather than a finding, forty rows in a paste — and this is where `web/`'s
 * tests are, beside the settings-index guard rather than as a page test.
 */

/** Failures first, then warnings: an operator opens this page because
 *  something is wrong, and the answer should not be below the fold. */
const severity: Record<SystemCheck["status"], number> = {
  fail: 0,
  warn: 1,
  pass: 2,
  skip: 3,
};

/** The rows in reading order. Stable within a severity, so the backend's list
 *  order — which is the order a person reviewing the registry sees — survives. */
export function sortChecks(checks: readonly SystemCheck[]): SystemCheck[] {
  return [...checks].sort((a, b) => severity[a.status] - severity[b.status]);
}

/**
 * Where this row is fixed, or null.
 *
 * Two fields on the wire and one answer here. `href` is already an admin route
 * and is used as it stands; `setting` is a settings *key*, resolved through the
 * same guarded index every other link to a setting goes through (ADR §50) — so
 * an anchor rename moves this link too, and the backend never has to know which
 * tab a setting is currently on.
 *
 * An unknown key resolves to null rather than to a broken link. The backend and
 * the SPA ship together, but they are two artifacts and a settings key removed
 * on one side must not produce a destination that 404s on the other.
 */
export function checkDestination(check: SystemCheck): string | null {
  if (check.href) return check.href;
  if (!check.setting) return null;
  const entry = SETTINGS_INDEX.find((e) => e.id === check.setting);
  return entry ? settingHref(entry) : null;
}

const RESULT_LABEL: Record<SystemCheck["status"], string> = {
  pass: "PASS",
  warn: "WARN",
  fail: "FAIL",
  skip: "SKIP",
};

/**
 * The report as plain text, for pasting into a ticket or a chat.
 *
 * This is what an operator does next, and it is the reason the no-history
 * decision survives a page of forty rows: the report leaves the install in
 * their hands rather than into a table with a retention policy behind it.
 *
 * Plain text and no HTML — the destination is a ticket field or a chat box, and
 * both of those do something unwanted with markup. The header line carries what
 * a reader of the paste needs and does not have: which install, and when.
 */
export function reportAsText(report: SystemCheckReport): string {
  const header =
    `Toft system checks — ${report.hostname} — ${formatTimestamp(report.ran_at)}\n` +
    `${report.passed} passed, ${report.warned} warning${report.warned === 1 ? "" : "s"}, ` +
    `${report.failed} failed, ${report.skipped} not applicable`;

  const rows = sortChecks(report.checks).map(
    (c) => `[${RESULT_LABEL[c.status]}] ${c.category} — ${c.name}: ${c.detail}`
  );

  return [header, "", ...rows].join("\n");
}

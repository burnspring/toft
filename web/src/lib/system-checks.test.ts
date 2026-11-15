import { describe, expect, it } from "vitest";
import { checkDestination, reportAsText, sortChecks } from "@/lib/system-checks";
import type { SystemCheck, SystemCheckReport } from "@/lib/api";

const check = (over: Partial<SystemCheck>): SystemCheck => ({
  id: "x",
  name: "X",
  category: "Platform",
  status: "pass",
  detail: "fine",
  ...over,
});

describe("sortChecks", () => {
  it("puts failures first and warnings next", () => {
    const sorted = sortChecks([
      check({ id: "a", status: "skip" }),
      check({ id: "b", status: "pass" }),
      check({ id: "c", status: "fail" }),
      check({ id: "d", status: "warn" }),
    ]);
    expect(sorted.map((c) => c.id)).toEqual(["c", "d", "b", "a"]);
  });

  it("holds its order at forty rows, and does not mutate its input", () => {
    const input = Array.from({ length: 40 }, (_, i) =>
      check({ id: `c${i}`, status: i % 4 === 0 ? "fail" : "pass" })
    );
    const sorted = sortChecks(input);
    expect(sorted).toHaveLength(40);
    expect(sorted.slice(0, 10).every((c) => c.status === "fail")).toBe(true);
    // Stable within a severity: the backend's list order is the order a person
    // reviewing the registry sees, and it should survive the sort.
    expect(sorted.slice(10).map((c) => c.id)).toEqual(
      input.filter((c) => c.status === "pass").map((c) => c.id)
    );
    expect(input[0].id).toBe("c0");
  });
});

describe("checkDestination", () => {
  it("uses an admin route as it stands", () => {
    expect(checkDestination(check({ href: "/admin/sources" }))).toBe("/admin/sources");
  });

  it("resolves a settings key through the guarded index", () => {
    const href = checkDestination(check({ setting: "security_txt_expires" }));
    expect(href).toMatch(/^\/admin\/[a-z-]+#.+/);
  });

  // The backend and the SPA are two artifacts. A key removed on one side must
  // not become a link that 404s on the other.
  it("resolves an unknown setting to nothing rather than to a broken link", () => {
    expect(checkDestination(check({ setting: "no_such_setting" }))).toBeNull();
  });

  it("is null for a row with neither", () => {
    expect(checkDestination(check({}))).toBeNull();
  });
});

describe("reportAsText", () => {
  const report: SystemCheckReport = {
    ran_at: "2026-08-30T12:00:00Z",
    hostname: "toft-1",
    duration_ms: 240,
    status: "warn",
    passed: 1,
    warned: 1,
    failed: 0,
    skipped: 1,
    checks: [
      check({ id: "a", name: "Alpha", status: "pass", detail: "fine" }),
      check({ id: "b", name: "Beta", status: "warn", detail: "needs a look" }),
      check({ id: "c", name: "Gamma", status: "skip", detail: "not configured" }),
    ],
  };

  it("carries a header naming the install and the time", () => {
    const text = reportAsText(report);
    expect(text.split("\n")[0]).toContain("toft-1");
    expect(text).toContain("1 passed, 1 warning, 0 failed, 1 not applicable");
  });

  it("contains every row, warnings first", () => {
    const lines = reportAsText(report).split("\n").filter((l) => l.startsWith("["));
    expect(lines).toHaveLength(3);
    expect(lines[0]).toContain("Beta");
    expect(lines.join("\n")).toContain("Gamma");
  });

  it("is plain text with no markup", () => {
    expect(reportAsText(report)).not.toMatch(/<[a-z/]/i);
  });
});

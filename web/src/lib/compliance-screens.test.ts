import { readFileSync, readdirSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { adminGroups } from "./admin-nav";
import { SETTINGS_SECTIONS } from "./settings-index";

/**
 * Every screen the compliance evidence registry names is a page this SPA has.
 *
 * The registry is Go-side YAML and the label is what an assessor reads — "where
 * a human reads the same fact in the UI". Nothing noticed when the admin pages
 * were regrouped and the settings moved onto the pages they govern: most labels
 * named a nav group or a settings section that no longer existed, and eight of
 * the paths were not routes at all.
 *
 * It reads the YAML rather than a fixture, for the reason audit-actions.test.ts
 * reads the Go source.
 */

const ROOT = fileURLToPath(new URL("../../..", import.meta.url));
const EVIDENCE = readFileSync(join(ROOT, "internal/compliance/data/evidence.yaml"), "utf8");

type Screen = { item: string; path: string; anchor?: string; label: string };

function screens(): Screen[] {
  const out: Screen[] = [];
  let item = "";
  for (const line of EVIDENCE.split("\n")) {
    const id = line.match(/^\s+- id:\s*(\S+)/);
    if (id) item = id[1];
    const m = line.match(/^\s+screen:\s*\{(.*)\}\s*$/);
    if (!m) continue;
    const field = (k: string) =>
      m[1].match(new RegExp(`\\b${k}:\\s*(?:"([^"]*)"|([^,}]+))`))?.slice(1).find(Boolean)?.trim();
    out.push({ item, path: field("path")!, anchor: field("anchor"), label: field("label")! });
  }
  return out;
}

/** The SPA's routes, from App.tsx: top-level paths and the /admin children. */
function routes(): string[] {
  const src = readFileSync(join(ROOT, "web/src/App.tsx"), "utf8");
  const paths = [...src.matchAll(/path="([^"]+)"/g)].map((m) => m[1]);
  return paths.map((p) => (p.startsWith("/") ? p : `/admin/${p}`));
}

function matchesRoute(path: string, route: string): boolean {
  const pattern = route.replace(/:[a-zA-Z]+/g, "[^/]+").replace(/\*$/, ".*");
  return new RegExp(`^${pattern}$`).test(path);
}

function webSources(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) webSources(path, out);
    else if (name.endsWith(".tsx") && !name.endsWith(".test.tsx")) out.push(path);
  }
  return out;
}

describe("the compliance evidence registry's screens", () => {
  const all = screens();

  it("are read", () => {
    expect(all.length).toBeGreaterThan(25);
  });

  it("name a route the SPA serves", () => {
    const known = routes();
    const missing = all
      .map((s) => ({ ...s, bare: s.path.split("?")[0].replace(/\{[a-z]+\}/g, "x") }))
      .filter((s) => !known.some((r) => matchesRoute(s.bare, r)))
      .map((s) => `${s.item}: ${s.path}`);
    expect(missing).toEqual([]);
  });

  it("label an admin page by the nav's own group and name", () => {
    const where = new Map<string, string>();
    for (const group of adminGroups) {
      for (const page of group.items) where.set(page.href, `Admin → ${group.label} → ${page.label}`);
    }
    const wrong = all
      .filter((s) => s.label.startsWith("Admin →"))
      .filter((s) => !s.label.startsWith(where.get(s.path) ?? "\0"))
      .map((s) => `${s.item}: "${s.label}" for ${s.path}, the nav says "${where.get(s.path)}"`);
    expect(wrong).toEqual([]);
  });

  it("anchor to a section that exists", () => {
    // Settings renders its sections from a table, so their ids are not literals.
    const ids = new Set<string>(SETTINGS_SECTIONS.map((s) => s.id));
    for (const file of webSources(join(ROOT, "web/src"))) {
      for (const m of readFileSync(file, "utf8").matchAll(/\bid="([a-z-]+)"/g)) ids.add(m[1]);
    }
    const missing = all
      .filter((s) => s.anchor && !ids.has(s.anchor))
      .map((s) => `${s.item}: #${s.anchor}`);
    expect(missing).toEqual([]);
  });
});

import { describe, expect, it } from "vitest";
import { ownerFacetKey, ownerFacetOptions, versionFacetOptions } from "@/lib/app-facets";
import { runtimeSeriesKey } from "@/lib/runtime-series";
import type { AppRuntime, AppWithMetrics } from "@/lib/api";

const python = (series: string): AppRuntime =>
  ({ language: "python", label: "Python", series, state: "resolved" });

/** Only the two fields these facets read. */
const app = (runtime?: AppRuntime) => ({ runtime }) as unknown as AppWithMetrics;

describe("versionFacetOptions", () => {
  it("derives its vocabulary from the apps already loaded", () => {
    const options = versionFacetOptions([app(python("3.13")), app(python("3.12"))]);
    expect(options.map((o) => o.key)).toEqual(["python:3.13", "python:3.12"]);
  });

  it("offers one option per series, however many apps are on it", () => {
    const options = versionFacetOptions([app(python("3.13")), app(python("3.13"))]);
    expect(options).toHaveLength(1);
  });

  // A facet renders nothing below two options, so an install with one release
  // line gets no filter — which is correct, and is the rule the Mode *column*
  // deliberately does not share.
  it("offers nothing at all when no app carries a runtime", () => {
    expect(versionFacetOptions([app(), app()])).toEqual([]);
  });

  it("sorts newest first inside a language and puts the wordless states last", () => {
    const options = versionFacetOptions([
      app({ language: "go", label: "Go", series: "", state: "unresolved" }),
      app(python("3.9")),
      app(python("3.13")),
      app({ language: "python", label: "Python", series: "", state: "unresolved" }),
    ]);
    expect(options.map((o) => o.key)).toEqual([
      "go:unresolved",
      "python:3.13",
      "python:3.9",
      "python:unresolved",
    ]);
  });

  it("keys options the way the census links to them", () => {
    const [option] = versionFacetOptions([app(python("3.13"))]);
    expect(option.key).toBe(runtimeSeriesKey(python("3.13")));
    expect(option.label).toBe("3.13");
    expect(option.group).toBe("Python");
  });

  // "Runs no interpreter" and "nobody recorded which interpreter" are different
  // facts, and a filter that merged them would answer neither.
  it("keeps not-applicable and unresolved apart", () => {
    const options = versionFacetOptions([
      app({ language: "static", label: "Static", series: "", state: "not_applicable" }),
      app({ language: "go", label: "Go", series: "", state: "unresolved" }),
    ]);
    expect(options.map((o) => o.key).sort()).toEqual(["go:unresolved", "static:not_applicable"]);
  });
});

describe("ownerFacetOptions", () => {
  const person = (id: string, login: string, name = "") =>
    ({ owner_id: id, owner: { id, login, name } }) as unknown as AppWithMetrics;
  const group = (id: string, ownerID = "u-admin") =>
    ({ owner_id: ownerID, owner_group_id: id }) as unknown as AppWithMetrics;

  it("keys a group-owned app by its group, never by the person who made it", () => {
    expect(ownerFacetKey(group("g1", "u1"))).toBe("group:g1");
    expect(ownerFacetKey(person("u1", "ada"))).toBe("user:u1");
  });

  it("puts people before groups, one option each, sorted by label", () => {
    const options = ownerFacetOptions(
      [group("g2"), person("u2", "zed"), person("u1", "ada", "Ada Lovelace"), group("g1"), person("u2", "zed")],
      [{ id: "g1", name: "Analytics" }, { id: "g2", name: "Finance" }],
    );
    expect(options.map((o) => [o.key, o.label, o.group])).toEqual([
      ["user:u1", "Ada Lovelace", "People"],
      ["user:u2", "zed", "People"],
      ["group:g1", "Analytics", "Groups"],
      ["group:g2", "Finance", "Groups"],
    ]);
    expect(options[0].search).toBe("ada");
  });

  it("keeps a group whose name this account cannot read", () => {
    expect(ownerFacetOptions([group("g9")], []).map((o) => o.label)).toEqual(["Group"]);
  });
});

import { describe, it, expect } from "vitest";
import { APP_TABS, LEGACY_APP_TABS, appTabsFor, type AppTab } from "./app-tabs";

const owner = { can_edit: true, can_manage_grants: true, can_change_visibility: true };
const editor = { can_edit: true, can_manage_grants: false, can_change_visibility: false };
const viewer = { can_edit: false, can_manage_grants: false, can_change_visibility: false };

const sorted = (tabs: Set<AppTab>) => [...tabs].sort();

describe("appTabsFor", () => {
  it("gives a viewer — and the reader of a public app — exactly Overview and Usage", () => {
    // The whole point of the rule. Every other tab's endpoints answer 403 or
    // 404 to somebody who can only read the app, so offering the tab is a
    // control that exists to fail. Asserted with the dependency verdict
    // present, because a verdict is never the reason a viewer gains a tab.
    expect(sorted(appTabsFor(viewer, true))).toEqual(["overview", "usage"]);
  });

  it("gives an editor everything but Access", () => {
    expect(sorted(appTabsFor(editor, true))).toEqual([
      "audit",
      "automation",
      "dependencies",
      "deployments",
      "integrations",
      "logs",
      "overview",
      "reach",
      "settings",
      "usage",
    ]);
  });

  it("gives an owner every tab there is", () => {
    expect(sorted(appTabsFor(owner, true))).toEqual([...APP_TABS].sort());
  });

  it("withholds Dependencies when this install produced no verdict", () => {
    expect(appTabsFor(owner, false).has("dependencies")).toBe(false);
    expect(appTabsFor(owner, true).has("dependencies")).toBe(true);
  });

  it("gives Access to somebody who may change visibility but not grants", () => {
    const publisher = { can_edit: false, can_manage_grants: false, can_change_visibility: true };
    expect(appTabsFor(publisher, false).has("access")).toBe(true);
  });

  it("answers the two everybody has while permissions are still in flight", () => {
    // Not the empty set: the strip would paint a round trip late and then jump.
    // Not more than two: a tab that appears and is then taken away is worse.
    expect(sorted(appTabsFor(undefined, true))).toEqual(["overview", "usage"]);
  });

  it("maps every legacy tab name onto a tab that exists", () => {
    for (const [from, to] of Object.entries(LEGACY_APP_TABS)) {
      expect(APP_TABS).toContain(to);
      expect(APP_TABS).not.toContain(from);
    }
  });
});

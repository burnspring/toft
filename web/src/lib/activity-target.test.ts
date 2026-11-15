import { describe, expect, it } from "vitest";
import { activityTarget } from "./activity-target";

const app = { target_type: "app", target_id: "c63f98f2-4268-441c-9a00-000000000000" };

describe("activityTarget", () => {
  it("links a live app by id and names it by slug", () => {
    expect(activityTarget({ ...app, target_label: "payroll" })).toEqual({
      text: "payroll",
      href: `/apps/${app.target_id}`,
      gone: false,
    });
  });
  it("never links a deleted app, and keeps the slug its deletion recorded", () => {
    expect(activityTarget({ ...app, target_label: "payroll", target_gone: true })).toEqual({
      text: "payroll",
      gone: true,
    });
  });
  it("says Deleted app rather than printing a bare id", () => {
    expect(activityTarget({ ...app, target_label: app.target_id, target_gone: true }).text).toBe("Deleted app");
  });
  it("leaves other targets unlinked", () => {
    expect(activityTarget({ target_type: "user", target_id: "u1", target_label: "kim" })).toEqual({
      text: "kim",
      gone: false,
    });
  });
});

import { describe, expect, it } from "vitest";
import { appKind } from "./frameworks";

// The same table as builder.AppKind (internal/builder/kind_test.go).
describe("appKind", () => {
  it.each([
    ["flask", "serving"], ["nextjs", "serving"], ["shiny", "serving"], ["marimo", "serving"],
    ["plumber", "serving"], [undefined, "serving"],
    ["static", "rendered"], ["notebook", "rendered"], ["rmarkdown", "rendered"], ["quarto", "rendered"],
    ["batch-python", "batch"], ["batch-r", "batch"],
  ])("%s is %s", (fw, kind) => {
    expect(appKind(fw)).toBe(kind);
  });
});

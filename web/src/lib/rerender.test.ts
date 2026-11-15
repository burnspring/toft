import { describe, expect, it } from "vitest";
import { isRerenderable } from "./rerender";

describe("isRerenderable", () => {
  it("is the three document frameworks", () => {
    for (const fw of ["notebook", "rmarkdown", "quarto"]) expect(isRerenderable(fw)).toBe(true);
  });
  it("is nothing that runs per viewer or as a server", () => {
    for (const fw of ["marimo", "shiny", "streamlit", "static", "", undefined, null])
      expect(isRerenderable(fw)).toBe(false);
  });
});

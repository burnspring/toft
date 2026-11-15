import { describe, expect, it } from "vitest";
import { ArrowRightLeft } from "lucide-react";
import { GALLERY_ICONS } from "./gallery-icons";

describe("GALLERY_ICONS", () => {
  it("finds a known name", () => {
    expect(GALLERY_ICONS["arrow-right-left"]).toBe(ArrowRightLeft);
  });

  it("finds nothing for an unknown name, including an Object member", () => {
    for (const name of ["nope", "constructor", "__proto__", "toString", "hasOwnProperty"]) {
      expect(GALLERY_ICONS[name]).toBeUndefined();
    }
  });
});

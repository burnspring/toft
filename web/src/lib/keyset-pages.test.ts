import { describe, expect, it } from "vitest";
import { chainedPages } from "./keyset-pages";

describe("chainedPages", () => {
  const pages = [null, { before_id: "a" }, { before_id: "b" }];

  it("keeps the pages while each tail is the next page's cursor", () => {
    expect(chainedPages(pages, 0, "a")).toBe(pages);
    expect(chainedPages(pages, 2, undefined)).toBe(pages);
  });

  it("drops the pages after one whose tail moved", () => {
    expect(chainedPages(pages, 0, "z")).toEqual([null]);
    expect(chainedPages(pages, 1, "q")).toEqual([null, { before_id: "a" }]);
  });
});

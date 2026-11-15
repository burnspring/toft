import { isValidElement, type ReactElement, type ReactNode } from "react";
import { describe, expect, it } from "vitest";

import { renderLogLine, stripLogNoise } from "./ansi";

/** The coloured runs a rendered line holds, as [text, colour] pairs. */
function runs(node: ReactNode): [string, string | undefined][] {
  const out: [string, string | undefined][] = [];
  const walk = (n: ReactNode, color?: string) => {
    if (typeof n === "string") {
      if (n !== "\n") out.push([n, color]);
    } else if (Array.isArray(n)) {
      n.forEach((c) => walk(c, color));
    } else if (isValidElement(n)) {
      const el = n as ReactElement<{ style?: { color?: string }; children?: ReactNode }>;
      walk(el.props.children, el.props.style?.color ?? color);
    }
  };
  walk(node);
  return out;
}

describe("a log line's colours", () => {
  it("survives the pass that removes every other escape", () => {
    const got = runs(renderLogLine("\x1b[32madded\x1b[0m package", 0));
    expect(got).toEqual([
      ["added", "#4ade80"],
      [" package", undefined],
    ]);
  });

  it("still removes cursor and erase sequences", () => {
    expect(runs(renderLogLine("\x1b[2K\x1b[1Gdone", 0))).toEqual([["done", undefined]]);
  });

  it("is removed entirely from plain text", () => {
    expect(stripLogNoise("\x1b[1;31mERR\x1b[0m\x1b[2K x")).toBe("ERR x");
  });
});
